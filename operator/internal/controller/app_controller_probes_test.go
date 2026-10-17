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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

// Health check paths used by the probe tests.
const (
	readyPath = "/healthz"
	livePath  = "/livez"
)

var _ = Describe("App Controller probes", func() {
	ctx := context.Background()

	// newApp builds a valid App on port 8080 with the given health check.
	newApp := func(name string, healthCheck *platformatorv1alpha1.HealthCheck) *platformatorv1alpha1.App {
		return &platformatorv1alpha1.App{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: platformatorv1alpha1.AppSpec{
				Image:       testImage,
				Port:        8080,
				HealthCheck: healthCheck,
				MinReplicas: new(int32(1)),
				MaxReplicas: new(int32(1)),
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
	}

	// reconcileApp creates the App, reconciles it once and returns the
	// container of the resulting Deployment.
	reconcileApp := func(app *platformatorv1alpha1.App) corev1.Container {
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, app)).To(Succeed())
		})

		name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
		reconciler := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, name, dep)).To(Succeed())
		Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
		return dep.Spec.Template.Spec.Containers[0]
	}

	It("adds a TCP readiness probe on the App's port by default", func() {
		container := reconcileApp(newApp("probe-default", nil))

		Expect(container.ReadinessProbe).NotTo(BeNil())
		Expect(container.ReadinessProbe.TCPSocket).NotTo(BeNil())
		Expect(container.ReadinessProbe.TCPSocket.Port.IntValue()).To(Equal(8080))
		Expect(container.ReadinessProbe.HTTPGet).To(BeNil())
		Expect(container.LivenessProbe).To(BeNil())
	})

	It("uses an HTTP readiness probe and no liveness probe when only a path is set", func() {
		container := reconcileApp(newApp("probe-http", &platformatorv1alpha1.HealthCheck{Path: readyPath}))

		Expect(container.ReadinessProbe).NotTo(BeNil())
		Expect(container.ReadinessProbe.TCPSocket).To(BeNil())
		Expect(container.ReadinessProbe.HTTPGet).NotTo(BeNil())
		Expect(container.ReadinessProbe.HTTPGet.Path).To(Equal(readyPath))
		Expect(container.ReadinessProbe.HTTPGet.Port.IntValue()).To(Equal(8080))

		// Restarting on a failing dependency check would be harmful, so the
		// readiness path is never reused for liveness.
		Expect(container.LivenessProbe).To(BeNil())
	})

	It("adds a liveness probe on its own path when livenessPath is set", func() {
		container := reconcileApp(newApp("probe-liveness", &platformatorv1alpha1.HealthCheck{
			Path:         readyPath,
			LivenessPath: livePath,
		}))

		Expect(container.ReadinessProbe.HTTPGet).NotTo(BeNil())
		Expect(container.ReadinessProbe.HTTPGet.Path).To(Equal(readyPath))

		Expect(container.LivenessProbe).NotTo(BeNil())
		Expect(container.LivenessProbe.HTTPGet).NotTo(BeNil())
		Expect(container.LivenessProbe.HTTPGet.Path).To(Equal(livePath))
		Expect(container.LivenessProbe.HTTPGet.Port.IntValue()).To(Equal(8080))
		// A slow-starting app must not be restarted before it can answer.
		Expect(container.LivenessProbe.InitialDelaySeconds).To(BeNumerically(">", 0))
	})

	It("rejects a health check path without a leading slash", func() {
		err := k8sClient.Create(ctx, newApp("probe-invalid", &platformatorv1alpha1.HealthCheck{Path: "healthz"}))

		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
	})

	It("rejects a liveness path without a leading slash", func() {
		err := k8sClient.Create(ctx, newApp("probe-invalid-live", &platformatorv1alpha1.HealthCheck{
			Path:         readyPath,
			LivenessPath: "livez",
		}))

		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
	})
})
