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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

var _ = Describe("App Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		app := &platformatorv1alpha1.App{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind App")
			err := k8sClient.Get(ctx, typeNamespacedName, app)
			if err != nil && errors.IsNotFound(err) {
				resource := &platformatorv1alpha1.App{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: platformatorv1alpha1.AppSpec{
						Image:       "nginx:latest",
						Port:        80,
						MinReplicas: new(int32(1)),
						MaxReplicas: new(int32(1)),
						Env: []corev1.EnvVar{
							{Name: "KEY", Value: "value"},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("50M"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("100M"),
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &platformatorv1alpha1.App{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance App")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &AppReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Domain: "example.com",
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the App reports Reconciled and Ready conditions")
			// Reconcile wrote to its own local copy of the App, not the
			// "app" variable from BeforeEach - re-fetch to see what actually
			// landed in the API server (and its status subresource).
			updated := &platformatorv1alpha1.App{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())

			reconciledCondition := meta.FindStatusCondition(updated.Status.Conditions, "Reconciled")
			Expect(reconciledCondition).NotTo(BeNil())
			Expect(reconciledCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(reconciledCondition.Reason).To(Equal("ReconcileSucceeded"))

			// envtest has no Deployment controller, so the Deployment never
			// reports Available on its own: Ready must be False for now.
			readyCondition := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("DeploymentUnavailable"))

			// fakeStatus plays the role of the Deployment controller (absent in
			// envtest): it rewrites the Deployment's status, reconciles, and
			// returns the App's resulting Ready condition.
			fakeStatus := func(mutate func(dep *appsv1.Deployment)) *metav1.Condition {
				dep := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, dep)).To(Succeed())
				mutate(dep)
				Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				Expect(err).NotTo(HaveOccurred())

				Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
				cond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
				Expect(cond).NotTo(BeNil())
				return cond
			}

			// healthy marks the Deployment as fully rolled out and available.
			healthy := func(dep *appsv1.Deployment) {
				dep.Status.ObservedGeneration = dep.Generation
				dep.Status.Replicas = 1
				dep.Status.UpdatedReplicas = 1
				dep.Status.AvailableReplicas = 1
				dep.Status.ReadyReplicas = 1
				dep.Status.Conditions = []appsv1.DeploymentCondition{{
					Type:    appsv1.DeploymentAvailable,
					Status:  corev1.ConditionTrue,
					Reason:  "MinimumReplicasAvailable",
					Message: "Deployment has minimum availability.",
				}}
			}

			By("Faking a fully rolled out Deployment")
			readyCondition = fakeStatus(healthy)
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCondition.Reason).To(Equal("DeploymentAvailable"))

			By("Faking a stuck rollout: the old pod keeps Available=True, the new pod is never available")
			readyCondition = fakeStatus(func(dep *appsv1.Deployment) {
				healthy(dep)
				dep.Status.Replicas = 2
			})
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("RolloutInProgress"))

			By("Faking a Deployment controller that has not observed the latest spec yet")
			readyCondition = fakeStatus(func(dep *appsv1.Deployment) {
				healthy(dep)
				dep.Status.ObservedGeneration = dep.Generation - 1
			})
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("RolloutInProgress"))

			By("Faking a rollout that exceeded its progress deadline")
			readyCondition = fakeStatus(func(dep *appsv1.Deployment) {
				healthy(dep)
				dep.Status.Replicas = 2
				dep.Status.Conditions = append(dep.Status.Conditions, appsv1.DeploymentCondition{
					Type:    appsv1.DeploymentProgressing,
					Status:  corev1.ConditionFalse,
					Reason:  "ProgressDeadlineExceeded",
					Message: "ReplicaSet has timed out progressing.",
				})
			})
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("ProgressDeadlineExceeded"))

			By("Verifying the Deployment's container got the App's Env and Resources")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, dep)).To(Succeed())

			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := dep.Spec.Template.Spec.Containers[0]

			Expect(container.Env).To(Equal(updated.Spec.Env))
			Expect(container.Resources).To(Equal(updated.Spec.Resources))
		})
	})
})
