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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

const testSecret = "db"

func secretRef(name string) corev1.EnvFromSource {
	return corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
	}}
}

func configMapRef(name string) corev1.EnvFromSource {
	return corev1.EnvFromSource{ConfigMapRef: &corev1.ConfigMapEnvSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
	}}
}

func TestConfigRefs(t *testing.T) {
	app := validApp("refs")
	if refs := configRefs(app); len(refs) != 0 {
		t.Fatalf("an App without references has none, got %v", refs)
	}

	app.Spec.EnvFrom = []corev1.EnvFromSource{secretRef(testSecret), configMapRef(testConfigMap), secretRef(testSecret)}
	app.Spec.Env = []corev1.EnvVar{
		{Name: "A", Value: "plain"},
		{Name: "B", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "token"}, Key: "k",
		}}},
		{Name: "C", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: testConfigMap}, Key: "k",
		}}},
	}
	want := []configRef{
		{kindConfigMap, testConfigMap},
		{kindSecret, testSecret},
		{kindSecret, "token"},
	}
	got := configRefs(app)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestSecurityDefaults(t *testing.T) {
	pod, container := securityFor(validApp("sec"))
	if pod.SeccompProfile == nil || pod.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("the runtime default seccomp profile is always on")
	}
	if container.AllowPrivilegeEscalation == nil || *container.AllowPrivilegeEscalation {
		t.Error("privilege escalation is always off")
	}
	if pod.RunAsNonRoot != nil || container.ReadOnlyRootFilesystem != nil || container.Capabilities != nil {
		t.Error("the options that can break images are off by default")
	}
}

func TestSecurityOptions(t *testing.T) {
	app := validApp("sec")
	app.Spec.Security = &platformatorv1alpha1.Security{
		RunAsNonRoot: true, ReadOnlyRootFilesystem: true, DropCapabilities: true,
	}
	pod, container := securityFor(app)
	if pod.RunAsNonRoot == nil || !*pod.RunAsNonRoot {
		t.Error("runAsNonRoot should be set")
	}
	if container.ReadOnlyRootFilesystem == nil || !*container.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem should be set")
	}
	caps := container.Capabilities
	if caps == nil || len(caps.Drop) != 1 || caps.Drop[0] != "ALL" ||
		len(caps.Add) != 1 || caps.Add[0] != "NET_BIND_SERVICE" {
		t.Errorf("capabilities: got %+v, want drop ALL and add NET_BIND_SERVICE", caps)
	}
}

var _ = Describe("App config reload", func() {
	ctx := context.Background()

	It("rolls the pods when a referenced Secret or ConfigMap changes, and only then", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "reload-secret", Namespace: testNamespace},
			StringData: map[string]string{"PASSWORD": "one"},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, secret)).To(Succeed()) })

		app := validApp("reload-app")
		app.Spec.EnvFrom = []corev1.EnvFromSource{secretRef(secret.Name)}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })

		name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
		reconciler := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain}
		revision := func() string {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, name, dep)).To(Succeed())
			return dep.Spec.Template.Annotations[configRevisionAnnotation]
		}

		first := revision()
		Expect(first).NotTo(BeEmpty())
		Expect(revision()).To(Equal(first), "nothing changed, so the revision must not")

		secret.StringData = map[string]string{"PASSWORD": "two"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		second := revision()
		Expect(second).NotTo(Equal(first), "a changed Secret must change the revision")
		Expect(revision()).To(Equal(second))
	})

	It("changes the revision when a missing Secret appears", func() {
		app := validApp("reload-missing")
		app.Spec.EnvFrom = []corev1.EnvFromSource{secretRef("not-yet")}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })

		reconciler := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain}
		before, err := reconciler.configRevision(ctx, app)
		Expect(err).NotTo(HaveOccurred())

		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "not-yet", Namespace: testNamespace}}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, secret)).To(Succeed()) })

		after, err := reconciler.configRevision(ctx, app)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).NotTo(Equal(before))
	})

	It("adds no annotation to an App that uses no Secret or ConfigMap", func() {
		app := validApp("reload-none")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })

		name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
		reconciler := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, name, dep)).To(Succeed())
		Expect(dep.Spec.Template.Annotations).NotTo(HaveKey(configRevisionAnnotation))
	})

	It("maps a changed Secret to the Apps that use it, in its namespace only", func() {
		using := validApp("map-using")
		using.Spec.EnvFrom = []corev1.EnvFromSource{secretRef("mapped")}
		other := validApp("map-other")
		other.Spec.EnvFrom = []corev1.EnvFromSource{secretRef("something-else")}
		for _, a := range []*platformatorv1alpha1.App{using, other} {
			Expect(k8sClient.Create(ctx, a)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, a)).To(Succeed()) })
		}

		reconciler := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain}
		changed := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "mapped", Namespace: testNamespace}}
		requests := reconciler.appsUsing(kindSecret)(ctx, changed)
		Expect(requests).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Name: using.Name, Namespace: testNamespace},
		}))

		// A ConfigMap with the same name is a different object.
		Expect(reconciler.appsUsing(kindConfigMap)(ctx, changed)).To(BeEmpty())
	})
})
