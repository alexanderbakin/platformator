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
)

// reconcileNetworkPolicy makes the App's pods reachable only from the ingress
// controller, and only on the App's port. Egress is left open: Apps need the
// internet and their databases. With spec.network.allowSameNamespace, the other
// pods of the App's namespace may connect too.
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
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  peers,
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &port}},
			}},
		}
		return controllerutil.SetControllerReference(app, policy, r.Scheme)
	})
	return err
}
