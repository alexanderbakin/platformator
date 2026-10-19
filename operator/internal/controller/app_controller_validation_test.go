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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

// nameRuleMessage is the start of the CRD's message for the name rule.
const nameRuleMessage = "name must be a DNS label"

// envFromRuleMessage is the CRD's message for the envFrom rule.
const envFromRuleMessage = "each envFrom entry must set exactly one of configMapRef and secretRef"

// validApp builds an App that passes every validation rule of the CRD.
func validApp(name string) *platformatorv1alpha1.App {
	return &platformatorv1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: platformatorv1alpha1.AppSpec{
			Image:       testImage,
			Port:        8080,
			MinReplicas: new(int32(1)),
			MaxReplicas: new(int32(2)),
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

var _ = Describe("App validation", func() {
	ctx := context.Background()

	// expectRejected creates the App and expects the API server to refuse it
	// with an Invalid error that mentions the given text.
	expectRejected := func(app *platformatorv1alpha1.App, mention string) {
		err := k8sClient.Create(ctx, app)
		if err == nil {
			// Do not leak an App that should not exist into other specs.
			_ = k8sClient.Delete(ctx, app)
		}
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err.Error()).To(ContainSubstring(mention))
	}

	It("accepts a valid App", func() {
		app := validApp("valid-app")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		Expect(k8sClient.Delete(ctx, app)).To(Succeed())
	})

	It("rejects an image with whitespace", func() {
		app := validApp("bad-image")
		app.Spec.Image = "nginx latest"
		expectRejected(app, "image")
	})

	It("rejects zero replicas, which a HorizontalPodAutoscaler cannot handle", func() {
		app := validApp("zero-min")
		app.Spec.MinReplicas = new(int32(0))
		expectRejected(app, "minReplicas")

		app = validApp("zero-max")
		app.Spec.MaxReplicas = new(int32(0))
		expectRejected(app, "maxReplicas")
	})

	It("rejects maxReplicas below minReplicas", func() {
		app := validApp("max-below-min")
		app.Spec.MinReplicas = new(int32(3))
		app.Spec.MaxReplicas = new(int32(2))
		expectRejected(app, "maxReplicas must be greater than or equal to minReplicas")
	})

	It("rejects a target CPU utilization of zero", func() {
		app := validApp("zero-cpu-target")
		app.Spec.TargetCPUUtilizationPercentage = new(int32(0))
		expectRejected(app, "targetCPUUtilizationPercentage")
	})

	It("rejects a port outside 1-65535", func() {
		app := validApp("bad-port")
		app.Spec.Port = 70000
		expectRejected(app, "port")
	})

	It("rejects names that cannot be a Service name or a hostname", func() {
		// A dot is a valid object name, but a Service name must be a DNS label.
		expectRejected(validApp("my.app"), nameRuleMessage)
		// Service names must start with a letter.
		expectRejected(validApp("1app"), nameRuleMessage)
		// 64 characters: valid for an object, too long for a DNS label.
		expectRejected(validApp(strings.Repeat("a", 64)), nameRuleMessage)
	})

	It("accepts envFrom entries that reference a Secret or a ConfigMap", func() {
		app := validApp("env-from-ok")
		app.Spec.EnvFrom = []corev1.EnvFromSource{
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}}},
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: testConfigMap}}},
		}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		Expect(k8sClient.Delete(ctx, app)).To(Succeed())
	})

	It("rejects an envFrom entry that references nothing, or both a Secret and a ConfigMap", func() {
		app := validApp("env-from-empty")
		app.Spec.EnvFrom = []corev1.EnvFromSource{{Prefix: "DB_"}}
		expectRejected(app, envFromRuleMessage)

		app = validApp("env-from-both")
		app.Spec.EnvFrom = []corev1.EnvFromSource{{
			SecretRef:    &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}},
			ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: testConfigMap}},
		}}
		expectRejected(app, envFromRuleMessage)
	})
})
