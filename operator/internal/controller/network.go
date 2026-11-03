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
	"net/netip"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

const (
	// defaultIngressNamespace is where k3s runs Traefik.
	defaultIngressNamespace = "kube-system"

	// ingressControllerLabelKey and ingressControllerLabelValue pick the
	// ingress controller's pods inside that namespace (k3s's Traefik).
	ingressControllerLabelKey   = "app.kubernetes.io/name"
	ingressControllerLabelValue = "traefik"

	// buildPolicyName is the NetworkPolicy of the build pods in a namespace. It is
	// shared by the Apps of the namespace, so no App owns it.
	buildPolicyName = "platformator-builds"

	// egressAllowedAnnotation is set by the platform admin on a tenant's
	// namespace. It lists, separated by commas, the private ranges the Apps of
	// the namespace may ask for in spec.network.allowEgressTo. Tenants cannot
	// edit a Namespace, so this is the ceiling: a range outside it is ignored.
	egressAllowedAnnotation = "platformator.alexanderbakin.com/egress-allowed"

	// namespaceNameLabel is set by Kubernetes on every namespace.
	namespaceNameLabel = "kubernetes.io/metadata.name"

	// dnsNamespace and dnsLabel pick the cluster DNS pods (CoreDNS on k3s).
	dnsNamespace = "kube-system"
	dnsLabelKey  = "k8s-app"
	dnsLabelVal  = "kube-dns"

	// linkLocalCIDR holds the cloud metadata address, 169.254.169.254.
	linkLocalCIDR = "169.254.0.0/16"
)

// privateCIDRs are the private IPv4 ranges. An App cannot reach them by default,
// which covers the node, the Kubernetes API, the VPC and every cluster address
// on a default k3s.
var privateCIDRs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

// linkLocal is the range of the cloud metadata address. No App reaches it.
var linkLocal = netip.MustParsePrefix(linkLocalCIDR)

// reconcileNetworkPolicy makes the App's pods reachable only from the ingress
// controller, and only on the App's port. With spec.network.allowSameNamespace,
// the other pods of the App's namespace may connect too.
//
// Egress is limited as well: the App may resolve names, reach the pods of its
// own namespace and reach the public internet, but not other namespaces, the
// Kubernetes API, the node or the cloud metadata address. The ranges in
// spec.network.allowEgressTo are added to that, for example a database in the
// VPC.
func (r *AppReconciler) reconcileNetworkPolicy(ctx context.Context, app *platformatorv1alpha1.App, allowedEgress []string) error {
	ingressNamespace := r.IngressNamespace
	if ingressNamespace == "" {
		ingressNamespace = defaultIngressNamespace
	}
	peers := []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{namespaceNameLabel: ingressNamespace},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{ingressControllerLabelKey: ingressControllerLabelValue},
		},
	}}
	if app.Spec.Network != nil && app.Spec.Network.AllowSameNamespace {
		// An empty pod selector, with no namespace selector, means every pod in
		// the policy's own namespace.
		peers = append(peers, networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}})
	}
	protocol := corev1.ProtocolTCP
	port := intstr.FromInt32(app.Spec.Port)

	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labelsFor(app)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  peers,
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &port}},
			}},
			Egress: egressRules(allowedEgress),
		}
		return controllerutil.SetControllerReference(app, policy, r.Scheme)
	})
	return err
}

// dnsEgressRule lets pods resolve names through the cluster DNS.
func dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dnsPort := intstr.FromInt32(53)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{namespaceNameLabel: dnsNamespace},
			},
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{dnsLabelKey: dnsLabelVal},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &dnsPort},
			{Protocol: &tcp, Port: &dnsPort},
		},
	}
}

// internetEgressRule lets pods reach the public internet: every address except
// the cloud metadata range and the private ranges, which hold the node, the
// Kubernetes API, the VPC and the rest of the cluster.
func internetEgressRule() networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			IPBlock: &networkingv1.IPBlock{
				CIDR:   "0.0.0.0/0",
				Except: append([]string{linkLocalCIDR}, privateCIDRs...),
			},
		}},
	}
}

// egressRules lists what an App's pods may connect to: cluster DNS, the pods of
// their own namespace (each of which still applies its own ingress policy), the
// internet and the given private ranges, which the namespace's ceiling has
// already approved.
func egressRules(allowed []string) []networkingv1.NetworkPolicyEgressRule {
	rules := []networkingv1.NetworkPolicyEgressRule{
		dnsEgressRule(),
		{
			// No namespace selector: the pods of the policy's own namespace.
			To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
		},
		internetEgressRule(),
	}
	if peers := allowedPeers(allowed); len(peers) > 0 {
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: peers})
	}
	return rules
}

// allowedPeers turns the ranges of spec.network.allowEgressTo into policy
// peers. The metadata range is never allowed: a range inside it is dropped, and
// a wider range keeps it as an exception. A value that is not an IPv4 range is
// dropped too (the CRD already refuses it).
func allowedPeers(cidrs []string) []networkingv1.NetworkPolicyPeer {
	var peers []networkingv1.NetworkPolicyPeer
	for _, value := range cidrs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		prefix = prefix.Masked()
		block := &networkingv1.IPBlock{CIDR: prefix.String()}
		if prefix.Overlaps(linkLocal) {
			if prefix.Bits() >= linkLocal.Bits() {
				continue
			}
			block.Except = []string{linkLocalCIDR}
		}
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: block})
	}
	return peers
}

// reconcileBuildNetworkPolicy limits the build pods of the App's namespace. A
// build runs a Dockerfile, which is arbitrary code, so its pods may only resolve
// names and reach the internet (Git hosts, the registry, base images). They
// cannot reach the node, the Kubernetes API, other pods or the metadata
// address, and nothing can connect to them.
//
// The policy is skipped when builds push to a local HTTP registry
// (RegistryInsecure), which is a test setup inside the cluster that a private
// range rule would cut off.
func (r *AppReconciler) reconcileBuildNetworkPolicy(ctx context.Context, app *platformatorv1alpha1.App) error {
	if app.Spec.Source == nil || r.RegistryInsecure {
		return nil
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: buildPolicyName, Namespace: app.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{buildLabel: buildLabelValue}},
			// No ingress rules: nothing may connect to a build pod.
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{dnsEgressRule(), internetEgressRule()},
		}
		return nil
	})
	return err
}

// The EgressAllowed condition reports spec.network.allowEgressTo. It is absent
// when the App asks for nothing, True when every range is inside the ceiling of
// its namespace and False when some are refused.
const (
	conditionEgressAllowed = "EgressAllowed"
	reasonEgressPermitted  = "EgressPermitted"
	reasonEgressRefused    = "EgressRefused"

	eventActionEgress = "Egress"
)

// egressPlan splits the ranges an App asks for into those its namespace permits
// and those it refuses.
type egressPlan struct {
	allowed []string
	refused []string
}

// planEgress compares spec.network.allowEgressTo with the ceiling in the
// namespace's annotation. A range is permitted when it lies inside one of the
// ceiling's ranges. With no annotation, nothing private is permitted.
func (r *AppReconciler) planEgress(ctx context.Context, app *platformatorv1alpha1.App) (egressPlan, error) {
	var plan egressPlan
	if app.Spec.Network == nil || len(app.Spec.Network.AllowEgressTo) == 0 {
		return plan, nil
	}
	ceiling, err := r.egressCeiling(ctx, app.Namespace)
	if err != nil {
		return plan, err
	}
	for _, value := range app.Spec.Network.AllowEgressTo {
		prefix, parseErr := netip.ParsePrefix(value)
		if parseErr != nil || !prefix.Addr().Is4() {
			plan.refused = append(plan.refused, value)
			continue
		}
		prefix = prefix.Masked()
		if insideAny(prefix, ceiling) {
			plan.allowed = append(plan.allowed, value)
		} else {
			plan.refused = append(plan.refused, value)
		}
	}
	return plan, nil
}

// insideAny says whether prefix lies inside one of the ranges.
func insideAny(prefix netip.Prefix, ranges []netip.Prefix) bool {
	for _, r := range ranges {
		if r.Bits() <= prefix.Bits() && r.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

// egressCeiling reads the ranges in the namespace's annotation. Values that are
// not IPv4 ranges are ignored. The Namespace is read straight from the API, so
// the operator needs only "get" on namespaces, not a cluster-wide watch.
func (r *AppReconciler) egressCeiling(ctx context.Context, namespace string) ([]netip.Prefix, error) {
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var ns corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return nil, fmt.Errorf("reading namespace %q: %w", namespace, err)
	}
	var ceiling []netip.Prefix
	for part := range strings.SplitSeq(ns.Annotations[egressAllowedAnnotation], ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil || !prefix.Addr().Is4() {
			continue
		}
		ceiling = append(ceiling, prefix.Masked())
	}
	return ceiling, nil
}

// egressCondition is the EgressAllowed condition, or nil for an App that asks
// for no private ranges.
func egressCondition(app *platformatorv1alpha1.App, plan egressPlan) *metav1.Condition {
	if app.Spec.Network == nil || len(app.Spec.Network.AllowEgressTo) == 0 {
		return nil
	}
	cond := &metav1.Condition{Type: conditionEgressAllowed, ObservedGeneration: app.Generation}
	if len(plan.refused) == 0 {
		cond.Status = metav1.ConditionTrue
		cond.Reason = reasonEgressPermitted
		cond.Message = "every range in spec.network.allowEgressTo is inside the namespace's ceiling"
		return cond
	}
	cond.Status = metav1.ConditionFalse
	cond.Reason = reasonEgressRefused
	cond.Message = fmt.Sprintf(
		"%s not allowed for this namespace: its ceiling is the annotation %s, set by the platform admin on the Namespace",
		strings.Join(plan.refused, ", "), egressAllowedAnnotation)
	return cond
}

// emitEgressEvent publishes an Event when EgressAllowed changes status or reason.
func (r *AppReconciler) emitEgressEvent(
	app *platformatorv1alpha1.App, previous map[string]metav1.Condition, cond *metav1.Condition,
) {
	if r.Recorder == nil || cond == nil {
		return
	}
	if prev, found := previous[cond.Type]; found && prev.Status == cond.Status && prev.Reason == cond.Reason {
		return
	}
	eventType := corev1.EventTypeNormal
	if cond.Status == metav1.ConditionFalse {
		eventType = corev1.EventTypeWarning
	}
	r.Recorder.Eventf(app, nil, eventType, cond.Reason, eventActionEgress, "%s", cond.Message)
}
