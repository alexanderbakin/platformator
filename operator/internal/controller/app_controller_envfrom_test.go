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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("App envFrom", func() {
	ctx := context.Background()

	It("passes envFrom through to the container", func() {
		app := validApp("env-from-app")
		app.Spec.EnvFrom = []corev1.EnvFromSource{
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}}},
			{
				Prefix:       "CFG_",
				ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: testConfigMap}},
			},
		}
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
		Expect(dep.Spec.Template.Spec.Containers[0].EnvFrom).To(Equal(app.Spec.EnvFrom))
	})
})
