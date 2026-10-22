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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

const testOtherNamespace = "tenant-b"

func TestOwnsHostBefore(t *testing.T) {
	now := metav1.Now()
	later := metav1.NewTime(now.Add(time.Minute))
	app := func(namespace string, created metav1.Time) *platformatorv1alpha1.App {
		return &platformatorv1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, CreationTimestamp: created}}
	}
	if !ownsHostBefore(app("b", now), app("a", later)) {
		t.Error("the older App keeps the host, whatever the namespace names are")
	}
	if ownsHostBefore(app("a", later), app("b", now)) {
		t.Error("the newer App does not")
	}
	if !ownsHostBefore(app("a", now), app("b", now)) || ownsHostBefore(app("b", now), app("a", now)) {
		t.Error("with equal times the namespace name decides, the same way from both sides")
	}
}

var _ = Describe("App tenancy", func() {
	ctx := context.Background()

	reconcileApp := func(app *platformatorv1alpha1.App) reconcile.Result {
		reconciler := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace},
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	Describe("network policy", func() {
		It("lets only the ingress controller in, on the App's port", func() {
			app := validApp("netpol-app")
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })
			reconcileApp(app)

			policy := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, policy)).To(Succeed())
			Expect(policy.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))
			Expect(policy.Spec.PodSelector.MatchLabels).To(Equal(labelsFor(app)))
			Expect(policy.Spec.Ingress).To(HaveLen(1))
			rule := policy.Spec.Ingress[0]
			Expect(rule.From).To(HaveLen(1))
			Expect(rule.From[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(namespaceNameLabel, defaultIngressNamespace))
			Expect(rule.From[0].PodSelector.MatchLabels).To(HaveKeyWithValue(ingressControllerLabelKey, ingressControllerLabelValue))
			Expect(rule.Ports).To(HaveLen(1))
			Expect(rule.Ports[0].Port.IntValue()).To(Equal(int(app.Spec.Port)))
		})

		It("also lets the namespace's own pods in when asked to", func() {
			app := validApp("netpol-same-ns")
			app.Spec.Network = &platformatorv1alpha1.Network{AllowSameNamespace: true}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })
			reconcileApp(app)

			policy := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, policy)).To(Succeed())
			Expect(policy.Spec.Ingress[0].From).To(HaveLen(2))
			sameNamespace := policy.Spec.Ingress[0].From[1]
			Expect(sameNamespace.NamespaceSelector).To(BeNil(), "no namespace selector means the policy's own namespace")
			Expect(sameNamespace.PodSelector).NotTo(BeNil())
			Expect(sameNamespace.PodSelector.MatchLabels).To(BeEmpty())
		})
	})

	Describe("hostnames", func() {
		It("serves the older of two Apps with the same name and holds back the other", func() {
			other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testOtherNamespace}}
			if err := k8sClient.Create(ctx, other); !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			older := validApp("shared-name")
			Expect(k8sClient.Create(ctx, older)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, older)).To(Succeed()) })
			newer := validApp("shared-name")
			newer.Namespace = testOtherNamespace
			Expect(k8sClient.Create(ctx, newer)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, newer)).To(Succeed()) })

			result := reconcileApp(newer)
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "it must notice when the other App goes away")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: newer.Name, Namespace: newer.Namespace}, newer)).To(Succeed())
			ready := meta.FindStatusCondition(newer.Status.Conditions, "Ready")
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(reasonHostTaken))
			Expect(ready.Message).To(ContainSubstring(testNamespace + "/shared-name"))
			Expect(newer.Status.URL).To(BeEmpty())
			err := k8sClient.Get(ctx, types.NamespacedName{Name: newer.Name, Namespace: newer.Namespace}, &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "nothing is created for an App that cannot be served")

			reconcileApp(older)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: older.Name, Namespace: older.Namespace}, &appsv1.Deployment{})).To(Succeed())
		})
	})

	Describe("quota", func() {
		It("is not Ready when the Deployment cannot create pods, even if old pods run", func() {
			app := validApp("replica-failure")
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })
			reconcileApp(app)

			name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, name, dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas = 1
			dep.Status.UpdatedReplicas = 1
			dep.Status.AvailableReplicas = 1
			dep.Status.ReadyReplicas = 1
			dep.Status.Conditions = []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"},
				{
					Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
					Message: "pods is forbidden: exceeded quota: compute, requested: requests.cpu=50m",
				},
			}
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
			reconcileApp(app)

			Expect(k8sClient.Get(ctx, name, app)).To(Succeed())
			ready := meta.FindStatusCondition(app.Status.Conditions, "Ready")
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(reasonReplicaFailure))
			Expect(ready.Message).To(ContainSubstring("exceeded quota"))
		})
	})
})
