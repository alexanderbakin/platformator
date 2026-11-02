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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
func (r *AppReconciler) reconcileNetworkPolicy(ctx context.Context, app *platformatorv1alpha1.App) error {
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
			Egress: egressRules(app),
		}
		return controllerutil.SetControllerReference(app, policy, r.Scheme)
	})
	return err
}

// egressRules lists what an App's pods may connect to: cluster DNS, the pods of
// their own namespace (each of which still applies its own ingress policy), the
// internet and the ranges in spec.network.allowEgressTo. The internet rule
// excludes the cloud metadata range and every private range.
func egressRules(app *platformatorv1alpha1.App) []networkingv1.NetworkPolicyEgressRule {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dnsPort := intstr.FromInt32(53)

	rules := []networkingv1.NetworkPolicyEgressRule{
		{
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
		},
		{
			// No namespace selector: the pods of the policy's own namespace.
			To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
		},
		{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{
					CIDR:   "0.0.0.0/0",
					Except: append([]string{linkLocalCIDR}, privateCIDRs...),
				},
			}},
		},
	}

	if app.Spec.Network != nil {
		if peers := allowedPeers(app.Spec.Network.AllowEgressTo); len(peers) > 0 {
			rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: peers})
		}
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
