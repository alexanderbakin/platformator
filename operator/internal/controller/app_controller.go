/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

// AppReconciler reconciles a App object
type AppReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Domain string

	// Recorder publishes Kubernetes Events about each App, visible in
	// `kubectl describe app`. It may be nil (tests), then no events are sent.
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=platformator.alexanderbakin.com,resources=apps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platformator.alexanderbakin.com,resources=apps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platformator.alexanderbakin.com,resources=apps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// The named return values (result, err) let the deferred status-update
// closure below see whatever error (if any) this function is about to
// return, without every early "return ..." having to remember to also
// update the App's status itself.
func (r *AppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {

	log := logf.FromContext(ctx)
	log.Info("reconciling App", "name", req.Name)

	var app platformatorv1alpha1.App
	if err = r.Get(ctx, req.NamespacedName, &app); err != nil {
		if apierrors.IsNotFound(err) {
			// App was deleted - owner references handle cleanup of the
			// Deployment/Service/Ingress, nothing left for us to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// dep is filled in by reconcileDeployment and read by the deferred
	// closure below to decide whether the App is actually Ready.
	var dep *appsv1.Deployment

	// certCond is the CertificateReady condition. It stays nil until the
	// Ingress has been reconciled, so a pass that fails earlier leaves the
	// condition from the last successful pass untouched.
	var certCond *metav1.Condition

	// This runs right before Reconcile actually returns, whichever of the
	// return statements below fires. It records two status conditions and
	// persists them via the status subresource:
	//   - Reconciled: did every API call in this pass succeed?
	//   - Ready: is the Deployment fully rolled out and Available (pods serving)?
	defer func() {
		// A conflict means someone else (typically the Deployment controller
		// updating status) changed an object between our read and our write.
		// That is not a real failure: keep the conditions from the last
		// successful pass instead of flapping them to ReconcileError, and
		// retry shortly with fresh copies of everything.
		if err != nil && apierrors.IsConflict(err) {
			log.V(1).Info("conflict while reconciling, requeueing", "error", err.Error())
			result = ctrl.Result{RequeueAfter: time.Second}
			err = nil
			return
		}

		reconciled := metav1.Condition{
			Type:               "Reconciled",
			Status:             metav1.ConditionTrue,
			Reason:             "ReconcileSucceeded",
			Message:            "Deployment, Service, Ingress and HorizontalPodAutoscaler are up to date",
			ObservedGeneration: app.Generation,
		}
		ready := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "DeploymentAvailable",
			Message:            "Deployment is fully rolled out and available",
			ObservedGeneration: app.Generation,
		}

		avail := deploymentCondition(dep, appsv1.DeploymentAvailable)
		progressing := deploymentCondition(dep, appsv1.DeploymentProgressing)
		rolledOut, rolloutMessage := rolloutComplete(dep)
		switch {
		case err != nil:
			reconciled.Status = metav1.ConditionFalse
			reconciled.Reason = reasonReconcileError
			reconciled.Message = err.Error()
			ready.Status = metav1.ConditionFalse
			ready.Reason = reasonReconcileError
			ready.Message = "Reconcile failed, see the Reconciled condition"
		case avail == nil:
			ready.Status = metav1.ConditionFalse
			ready.Reason = "DeploymentUnavailable"
			ready.Message = "Deployment has not reported an Available condition yet"
		case avail.Status != corev1.ConditionTrue:
			ready.Status = metav1.ConditionFalse
			ready.Reason = "DeploymentUnavailable"
			ready.Message = avail.Message
		case progressing != nil && progressing.Status == corev1.ConditionFalse &&
			progressing.Reason == progressDeadlineExceeded:
			// The rollout stalled: old pods may keep Available=True, but the
			// new ones never became available within the progress deadline.
			ready.Status = metav1.ConditionFalse
			ready.Reason = progressDeadlineExceeded
			ready.Message = progressing.Message
		case !rolledOut:
			ready.Status = metav1.ConditionFalse
			ready.Reason = "RolloutInProgress"
			ready.Message = rolloutMessage
		case certCond != nil && certCond.Status == metav1.ConditionFalse:
			// The pods are fine, but the App's HTTPS address would still show a
			// certificate error. Unknown (no cert-manager) does not block.
			ready.Status = metav1.ConditionFalse
			ready.Reason = reasonCertificateNotReady
			ready.Message = "TLS certificate is not issued yet: " + certCond.Message
		}

		previous := conditionsByType(app.Status.Conditions)

		meta.SetStatusCondition(&app.Status.Conditions, reconciled)
		meta.SetStatusCondition(&app.Status.Conditions, ready)
		if certCond != nil {
			meta.SetStatusCondition(&app.Status.Conditions, *certCond)
			// Certificate changes are not watched (the CRD may not even be
			// installed), so poll until cert-manager reports it issued.
			if err == nil && certCond.Status == metav1.ConditionFalse && result.RequeueAfter == 0 {
				result = ctrl.Result{RequeueAfter: certificatePollInterval}
			}
		}

		if statusErr := r.Status().Update(ctx, &app); statusErr != nil {
			if apierrors.IsConflict(statusErr) {
				// Our copy of the App is stale: retry with a fresh one so the
				// status update is not lost.
				if err == nil {
					result = ctrl.Result{RequeueAfter: time.Second}
				}
			} else {
				log.Error(statusErr, "failed to update App status")
			}
		} else {
			// Events are sent only after the new status is stored, so a lost
			// update (conflict) does not announce the same change twice.
			r.emitTransitionEvents(&app, previous, reconciled, ready, certCond)
		}
	}()

	if dep, err = r.reconcileDeployment(ctx, &app); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling deployment: %w", err)
	}

	if err = r.reconcileService(ctx, &app); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
	}

	if err = r.reconcileIngress(ctx, &app); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling ingress: %w", err)
	}
	app.Status.URL = "https://" + r.hostFor(&app)
	tls := r.certificateCondition(ctx, &app)
	certCond = &tls

	if err = r.reconcileHPA(ctx, &app); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling HPA: %w", err)
	}

	log.Info("reconciled App")
	return ctrl.Result{}, nil
}

func labelsFor(app *platformatorv1alpha1.App) map[string]string {
	return map[string]string{"app": app.Name}
}

func (r *AppReconciler) reconcileDeployment(ctx context.Context, app *platformatorv1alpha1.App) (*appsv1.Deployment, error) {

	replicas := int32(1)
	if app.Spec.MinReplicas != nil {
		replicas = *app.Spec.MinReplicas
	}

	readiness, liveness := probesFor(app)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		if dep.CreationTimestamp.IsZero() {
			dep.Spec.Replicas = &replicas
		}
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labelsFor(app)}
		dep.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labelsFor(app)},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:      app.Name,
					Image:     app.Spec.Image,
					Ports:     []corev1.ContainerPort{{ContainerPort: app.Spec.Port}},
					Resources: app.Spec.Resources,
					Env:       app.Spec.Env,

					ReadinessProbe: readiness,
					LivenessProbe:  liveness,
				}},
			},
		}
		return controllerutil.SetControllerReference(app, dep, r.Scheme)
	})
	return dep, err
}

// probesFor returns the readiness and liveness probes of an App.
//
// Readiness is always on. Without a probe Kubernetes treats a pod as ready as
// soon as its container starts, even if nothing listens on the port, and the
// App would report Ready=True. The default is a TCP connect to the App's port,
// which works for any server. With spec.healthCheck.path, readiness becomes an
// HTTP request on that path.
//
// Liveness is off unless spec.healthCheck.livenessPath is set, and it never
// reuses the readiness path. A readiness path may check dependencies such as a
// database, which is fine because a failing readiness probe only removes the
// pod from the Service. A failing liveness probe restarts the container, so
// sharing the path would restart every pod at once during a database outage.
// There is no default liveness probe: a wrong guess would restart healthy apps.
func probesFor(app *platformatorv1alpha1.App) (readiness, liveness *corev1.Probe) {
	port := intstr.FromInt32(app.Spec.Port)

	readiness = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: port}},
		PeriodSeconds:    5,
		TimeoutSeconds:   1,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
	if app.Spec.HealthCheck == nil {
		return readiness, nil
	}

	httpGet := func(path string) corev1.ProbeHandler {
		return corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path:   path,
			Port:   port,
			Scheme: corev1.URISchemeHTTP,
		}}
	}
	readiness.ProbeHandler = httpGet(app.Spec.HealthCheck.Path)
	if app.Spec.HealthCheck.LivenessPath == "" {
		return readiness, nil
	}
	liveness = &corev1.Probe{
		ProbeHandler:        httpGet(app.Spec.HealthCheck.LivenessPath),
		InitialDelaySeconds: 10,
		PeriodSeconds:       10,
		TimeoutSeconds:      1,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}
	return readiness, liveness
}

// progressDeadlineExceeded is the Reason the Deployment controller puts on the
// Progressing condition when a rollout has not made progress in time.
const progressDeadlineExceeded = "ProgressDeadlineExceeded"

// deploymentCondition returns the Deployment's condition of the given type, or
// nil if the Deployment is nil or has not reported one yet.
// meta.FindStatusCondition doesn't work here: it takes []metav1.Condition,
// while Deployments use their own appsv1.DeploymentCondition type.
func deploymentCondition(dep *appsv1.Deployment, condType appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	if dep == nil {
		return nil
	}
	for i := range dep.Status.Conditions {
		if dep.Status.Conditions[i].Type == condType {
			return &dep.Status.Conditions[i]
		}
	}
	return nil
}

// rolloutComplete reports whether the Deployment has finished rolling out its
// latest spec, using the same checks as "kubectl rollout status". Available=True
// alone is not enough: during a rolling update the old pods keep it True while
// the new pods can still be crashlooping.
func rolloutComplete(dep *appsv1.Deployment) (bool, string) {
	if dep == nil {
		return false, "Deployment not found"
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	status := dep.Status
	switch {
	case status.ObservedGeneration < dep.Generation:
		return false, "Waiting for the Deployment controller to observe the latest spec"
	case status.UpdatedReplicas < desired:
		return false, fmt.Sprintf("%d of %d replicas updated", status.UpdatedReplicas, desired)
	case status.Replicas > status.UpdatedReplicas:
		return false, fmt.Sprintf("%d old replicas pending termination", status.Replicas-status.UpdatedReplicas)
	case status.AvailableReplicas < status.UpdatedReplicas:
		return false, fmt.Sprintf("%d of %d updated replicas available", status.AvailableReplicas, status.UpdatedReplicas)
	}
	return true, ""
}

func (r *AppReconciler) reconcileService(ctx context.Context, app *platformatorv1alpha1.App) error {

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.Selector = labelsFor(app)
		svc.Spec.Ports = []corev1.ServicePort{{
			Port:       app.Spec.Port,
			TargetPort: intstr.FromInt32(app.Spec.Port),
		}}
		return controllerutil.SetControllerReference(app, svc, r.Scheme)
	})
	return err
}

func (r *AppReconciler) reconcileIngress(ctx context.Context, app *platformatorv1alpha1.App) error {

	clusterIssuerName := "letsencrypt"
	ingressClassName := "traefik"
	host := r.hostFor(app)
	pathType := networkingv1.PathTypePrefix

	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		if ing.Annotations == nil {
			ing.Annotations = map[string]string{}
		}
		ing.Annotations["cert-manager.io/cluster-issuer"] = clusterIssuerName

		ing.Spec.IngressClassName = &ingressClassName
		ing.Spec.Rules = []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: networkingv1.IngressBackend{
							Service: &networkingv1.IngressServiceBackend{
								Name: app.Name,
								Port: networkingv1.ServiceBackendPort{Number: app.Spec.Port},
							},
						},
					}},
				},
			},
		}}
		ing.Spec.TLS = []networkingv1.IngressTLS{{
			Hosts:      []string{host},
			SecretName: tlsSecretName(app),
		}}
		return controllerutil.SetControllerReference(app, ing, r.Scheme)
	})
	return err
}

func (r *AppReconciler) reconcileHPA(ctx context.Context, app *platformatorv1alpha1.App) error {

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {

		hpa.Spec = autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind:       "Deployment",
				Name:       app.Name,
				APIVersion: "apps/v1",
			},
			MinReplicas: app.Spec.MinReplicas,
			MaxReplicas: *app.Spec.MaxReplicas,
			Metrics: []autoscalingv2.MetricSpec{
				{
					Type: autoscalingv2.ResourceMetricSourceType,
					Resource: &autoscalingv2.ResourceMetricSource{
						Name: corev1.ResourceCPU,
						Target: autoscalingv2.MetricTarget{
							Type:               autoscalingv2.UtilizationMetricType,
							AverageUtilization: app.Spec.TargetCPUUtilizationPercentage,
						},
					},
				},
			},
		}
		return controllerutil.SetControllerReference(app, hpa, r.Scheme)
	})
	return err
}

// hostFor returns the hostname the App is served on.
func (r *AppReconciler) hostFor(app *platformatorv1alpha1.App) string {
	return fmt.Sprintf("%s.%s", app.Name, r.Domain)
}

// tlsSecretName is the Secret the Ingress keeps its certificate in. cert-manager's
// ingress-shim names the Certificate it creates after this Secret.
func tlsSecretName(app *platformatorv1alpha1.App) string {
	return app.Name + "-tls"
}

// Names of the CertificateReady condition and its reasons.
const (
	conditionCertificateReady     = "CertificateReady"
	reasonCertManagerNotInstalled = "CertManagerNotInstalled"
	reasonCertificatePending      = "CertificatePending"
	reasonCertificateIssued       = "CertificateIssued"
	reasonCertificateCheckFailed  = "CertificateCheckFailed"
	reasonCertificateNotReady     = "CertificateNotReady"

	// certManagerReadyType is the type of the Certificate condition that says
	// whether the certificate has been issued.
	certManagerReadyType = "Ready"

	// certificatePollInterval is how often an App whose certificate is not
	// issued yet is checked again.
	certificatePollInterval = 10 * time.Second
)

// certificateGVK is cert-manager's Certificate. It is read as unstructured so
// the operator needs neither cert-manager's Go module nor its CRDs to be
// installed: without them the condition is just Unknown.
var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// certificateCondition reports whether cert-manager has issued the App's TLS
// certificate. It never fails the reconcile: TLS status is informational, and
// issuing depends on things outside the cluster (DNS, the ACME server).
func (r *AppReconciler) certificateCondition(
	ctx context.Context, app *platformatorv1alpha1.App,
) metav1.Condition {
	cond := metav1.Condition{Type: conditionCertificateReady, ObservedGeneration: app.Generation}

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: tlsSecretName(app)}, cert)
	switch {
	case err == nil:
		return certificateReadiness(cert, app.Generation)
	case meta.IsNoMatchError(err):
		cond.Status = metav1.ConditionUnknown
		cond.Reason = reasonCertManagerNotInstalled
		cond.Message = "The cert-manager Certificate CRD is not installed, so TLS status is unknown"
	case apierrors.IsNotFound(err):
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonCertificatePending
		cond.Message = "cert-manager has not created the Certificate yet"
	default:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = reasonCertificateCheckFailed
		cond.Message = err.Error()
	}
	return cond
}

// certificateReadiness turns the Ready condition of a cert-manager Certificate
// into the App's CertificateReady condition.
func certificateReadiness(cert *unstructured.Unstructured, generation int64) metav1.Condition {
	cond := metav1.Condition{
		Type:               conditionCertificateReady,
		Status:             metav1.ConditionFalse,
		Reason:             reasonCertificatePending,
		Message:            "cert-manager has not reported a Ready condition yet",
		ObservedGeneration: generation,
	}

	conditions, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	for _, item := range conditions {
		c, ok := item.(map[string]any)
		if !ok || c["type"] != certManagerReadyType {
			continue
		}
		if c["status"] == "True" {
			cond.Status = metav1.ConditionTrue
			cond.Reason = reasonCertificateIssued
			cond.Message = "cert-manager issued the TLS certificate"
			return cond
		}
		// Keep cert-manager's own reason and message (Issuing, Pending, ...).
		if reason, _ := c["reason"].(string); reason != "" {
			cond.Reason = reason
		}
		if message, _ := c["message"].(string); message != "" {
			cond.Message = message
		}
		return cond
	}
	return cond
}

// conditionsByType returns a copy of the conditions keyed by type, so the
// values stay unchanged when meta.SetStatusCondition edits the slice in place.
func conditionsByType(conditions []metav1.Condition) map[string]metav1.Condition {
	byType := make(map[string]metav1.Condition, len(conditions))
	for _, c := range conditions {
		byType[c.Type] = c
	}
	return byType
}

// reasonReconcileError is the reason of Reconciled and Ready when a pass failed.
const reasonReconcileError = "ReconcileError"

// Event actions, as shown in the "Action" field of the Event.
const (
	eventActionReconcile = "Reconcile"
	eventActionRollout   = "Rollout"
	eventActionTLS       = "TLS"
)

// emitTransitionEvents publishes an Event when the Ready or Reconciled
// condition changed its status or reason, and when the certificate gets
// issued. Reconcile runs often, so events are tied to changes rather than to
// every pass, otherwise `kubectl describe` would drown in repeats.
func (r *AppReconciler) emitTransitionEvents(
	app *platformatorv1alpha1.App,
	previous map[string]metav1.Condition,
	reconciled, ready metav1.Condition,
	certCond *metav1.Condition,
) {
	if r.Recorder == nil {
		return
	}
	changed := func(prev metav1.Condition, found bool, now metav1.Condition) bool {
		return !found || prev.Status != now.Status || prev.Reason != now.Reason
	}

	prevReconciled, found := previous[reconciled.Type]
	if changed(prevReconciled, found, reconciled) {
		eventType := corev1.EventTypeNormal
		if reconciled.Status == metav1.ConditionFalse {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(app, nil, eventType, reconciled.Reason, eventActionReconcile, "%s", reconciled.Message)
	}

	prevReady, found := previous[ready.Type]
	if changed(prevReady, found, ready) {
		eventType := corev1.EventTypeNormal
		if ready.Reason == reasonReconcileError || ready.Reason == progressDeadlineExceeded {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(app, nil, eventType, ready.Reason, eventActionRollout, "%s", ready.Message)
	}

	if certCond != nil && certCond.Status == metav1.ConditionTrue {
		prevCert, found := previous[certCond.Type]
		if !found || prevCert.Status != metav1.ConditionTrue {
			r.Recorder.Eventf(app, nil, corev1.EventTypeNormal, certCond.Reason, eventActionTLS, "%s", certCond.Message)
		}
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformatorv1alpha1.App{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Named("app").
		Complete(r)
}
