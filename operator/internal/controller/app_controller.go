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

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
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
}

// +kubebuilder:rbac:groups=platformator.alexanderbakin.com,resources=apps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platformator.alexanderbakin.com,resources=apps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platformator.alexanderbakin.com,resources=apps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete

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

	// This runs right before Reconcile actually returns, whichever of the
	// return statements below fires. It records two status conditions and
	// persists them via the status subresource:
	//   - Reconciled: did every API call in this pass succeed?
	//   - Ready: is the Deployment fully rolled out and Available (pods serving)?
	defer func() {
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
			reconciled.Reason = "ReconcileError"
			reconciled.Message = err.Error()
			ready.Status = metav1.ConditionFalse
			ready.Reason = "ReconcileError"
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
		}

		meta.SetStatusCondition(&app.Status.Conditions, reconciled)
		meta.SetStatusCondition(&app.Status.Conditions, ready)

		if statusErr := r.Status().Update(ctx, &app); statusErr != nil {
			log.Error(statusErr, "failed to update App status")
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
				}},
			},
		}
		return controllerutil.SetControllerReference(app, dep, r.Scheme)
	})
	return dep, err
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
	host := fmt.Sprintf("%s.%s", app.Name, r.Domain)
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
			SecretName: app.Name + "-tls",
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
