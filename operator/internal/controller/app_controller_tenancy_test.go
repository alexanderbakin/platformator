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
	"net/netip"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

const (
	testOtherNamespace = "tenant-b"

	// testDatabaseCIDR is a database address in the private range of the tests.
	testDatabaseCIDR = "10.0.2.15/32"
)

// setEgressCeiling puts the platform admin's egress ceiling on a namespace for
// the length of a spec.
func setEgressCeiling(ctx context.Context, namespace, ceiling string) {
	ns := &corev1.Namespace{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns)).To(Succeed())
	ns.Annotations = map[string]string{egressAllowedAnnotation: ceiling}
	Expect(k8sClient.Update(ctx, ns)).To(Succeed())
	DeferCleanup(func() {
		fresh := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, fresh)).To(Succeed())
		delete(fresh.Annotations, egressAllowedAnnotation)
		Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
	})
}

func TestInsideAny(t *testing.T) {
	ceiling := []netip.Prefix{netip.MustParsePrefix("10.0.2.0/24"), netip.MustParsePrefix("192.168.7.0/28")}
	for value, want := range map[string]bool{
		testDatabaseCIDR:  true,
		"10.0.2.0/24":     true,
		"10.0.2.0/25":     true,
		"10.0.2.0/16":     false, // wider than the ceiling
		"10.0.0.98/32":    false, // another address
		"192.168.7.5/32":  true,
		"192.168.7.20/32": false,
		"0.0.0.0/0":       false,
	} {
		if got := insideAny(netip.MustParsePrefix(value), ceiling); got != want {
			t.Errorf("insideAny(%s) = %v, want %v", value, got, want)
		}
	}
	if insideAny(netip.MustParsePrefix(testDatabaseCIDR), nil) {
		t.Error("with no ceiling nothing is permitted")
	}
}

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
			Expect(policy.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
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

		It("limits egress to DNS, its own namespace and the public internet", func() {
			app := validApp("netpol-egress")
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })
			reconcileApp(app)

			policy := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, policy)).To(Succeed())
			Expect(policy.Spec.Egress).To(HaveLen(3))

			dns := policy.Spec.Egress[0]
			Expect(dns.To[0].PodSelector.MatchLabels).To(HaveKeyWithValue(dnsLabelKey, dnsLabelVal))
			Expect(dns.Ports).To(HaveLen(2))

			own := policy.Spec.Egress[1]
			Expect(own.To[0].NamespaceSelector).To(BeNil(), "no namespace selector means the policy's own namespace")
			Expect(own.To[0].PodSelector.MatchLabels).To(BeEmpty())

			internet := policy.Spec.Egress[2].To[0].IPBlock
			Expect(internet.CIDR).To(Equal("0.0.0.0/0"))
			Expect(internet.Except).To(ConsistOf(linkLocalCIDR, "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"))
		})

		It("adds only the listed ranges, and never the metadata range", func() {
			// Even an admin who permits everything cannot open the metadata range.
			setEgressCeiling(ctx, testNamespace, "0.0.0.0/0")
			app := validApp("netpol-allow-to")
			app.Spec.Network = &platformatorv1alpha1.Network{
				AllowEgressTo: []string{testDatabaseCIDR, "169.254.169.254/32", "0.0.0.0/0", "10.0.3.7/24"},
			}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })
			reconcileApp(app)

			policy := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, policy)).To(Succeed())
			Expect(policy.Spec.Egress).To(HaveLen(4))
			peers := policy.Spec.Egress[3].To
			Expect(peers).To(HaveLen(3), "the range inside the metadata range is dropped")
			Expect(peers[0].IPBlock.CIDR).To(Equal(testDatabaseCIDR))
			Expect(peers[0].IPBlock.Except).To(BeEmpty())
			Expect(peers[1].IPBlock.CIDR).To(Equal("0.0.0.0/0"))
			Expect(peers[1].IPBlock.Except).To(ConsistOf(linkLocalCIDR), "a wider range keeps the metadata range out")
			Expect(peers[2].IPBlock.CIDR).To(Equal("10.0.3.0/24"), "the range is masked")
		})

		It("ignores ranges outside the namespace's ceiling and says so in a condition", func() {
			setEgressCeiling(ctx, testNamespace, "10.0.2.0/24")
			app := validApp("netpol-ceiling")
			app.Spec.Network = &platformatorv1alpha1.Network{AllowEgressTo: []string{testDatabaseCIDR, "10.0.0.98/32"}}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })
			reconcileApp(app)

			policy := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, policy)).To(Succeed())
			Expect(policy.Spec.Egress).To(HaveLen(4))
			peers := policy.Spec.Egress[3].To
			Expect(peers).To(HaveLen(1), "the node's address is outside the ceiling")
			Expect(peers[0].IPBlock.CIDR).To(Equal(testDatabaseCIDR))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app)).To(Succeed())
			cond := meta.FindStatusCondition(app.Status.Conditions, conditionEgressAllowed)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(reasonEgressRefused))
			Expect(cond.Message).To(ContainSubstring("10.0.0.98/32"))
			Expect(cond.Message).NotTo(ContainSubstring(testDatabaseCIDR))
		})

		It("permits nothing private without a ceiling, and reports it", func() {
			app := validApp("netpol-no-ceiling")
			app.Spec.Network = &platformatorv1alpha1.Network{AllowEgressTo: []string{testDatabaseCIDR}}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })
			reconcileApp(app)

			policy := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, policy)).To(Succeed())
			Expect(policy.Spec.Egress).To(HaveLen(3), "no extra rule")

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app)).To(Succeed())
			cond := meta.FindStatusCondition(app.Status.Conditions, conditionEgressAllowed)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		})

		It("reports a permitted range, and has no condition when none is asked for", func() {
			setEgressCeiling(ctx, testNamespace, "10.0.2.0/24")
			app := validApp("netpol-permitted")
			app.Spec.Network = &platformatorv1alpha1.Network{AllowEgressTo: []string{testDatabaseCIDR}}
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })
			reconcileApp(app)

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app)).To(Succeed())
			cond := meta.FindStatusCondition(app.Status.Conditions, conditionEgressAllowed)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			plain := validApp("netpol-plain")
			Expect(k8sClient.Create(ctx, plain)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, plain)).To(Succeed()) })
			reconcileApp(plain)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: plain.Name, Namespace: plain.Namespace}, plain)).To(Succeed())
			Expect(meta.FindStatusCondition(plain.Status.Conditions, conditionEgressAllowed)).To(BeNil())
		})

		It("refuses a range that is not an IPv4 CIDR", func() {
			app := validApp("netpol-bad-cidr")
			app.Spec.Network = &platformatorv1alpha1.Network{AllowEgressTo: []string{"database.internal"}}
			Expect(k8sClient.Create(ctx, app)).NotTo(Succeed())
		})

		It("limits the build pods of a namespace to DNS and the internet", func() {
			app := sourceApp("netpol-build")
			reconciler := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain, Registry: testRegistry}
			Expect(reconciler.reconcileBuildNetworkPolicy(ctx, app)).To(Succeed())
			DeferCleanup(func() {
				policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: buildPolicyName, Namespace: app.Namespace}}
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, policy))).To(Succeed())
			})

			policy := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: buildPolicyName, Namespace: app.Namespace}, policy)).To(Succeed())
			Expect(policy.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue(buildLabel, buildLabelValue))
			Expect(policy.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
			Expect(policy.Spec.Ingress).To(BeEmpty(), "nothing may connect to a build pod")
			Expect(policy.Spec.Egress).To(HaveLen(2))
			Expect(policy.Spec.Egress[0].To[0].PodSelector.MatchLabels).To(HaveKeyWithValue(dnsLabelKey, dnsLabelVal))
			Expect(policy.Spec.Egress[1].To[0].IPBlock.Except).To(ContainElements(linkLocalCIDR, "10.0.0.0/8"))
			Expect(policy.OwnerReferences).To(BeEmpty(), "the Apps of the namespace share it")
		})

		It("makes no build policy for an App with an image, or when the registry is a local test one", func() {
			// Other specs build from Git and leave the shared policy behind.
			stale := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: buildPolicyName, Namespace: testNamespace}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, stale))).To(Succeed())

			reconciler := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain, Registry: testRegistry}
			Expect(reconciler.reconcileBuildNetworkPolicy(ctx, validApp("netpol-image"))).To(Succeed())

			reconciler.RegistryInsecure = true
			Expect(reconciler.reconcileBuildNetworkPolicy(ctx, sourceApp("netpol-insecure"))).To(Succeed())

			err := k8sClient.Get(ctx, types.NamespacedName{Name: buildPolicyName, Namespace: testNamespace}, &networkingv1.NetworkPolicy{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
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
