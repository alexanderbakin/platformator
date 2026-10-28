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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

const (
	testSSHURL     = "git@github.com:owner/private.git"
	testDeployKey  = "deploy-key"
	testPrivateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nnot a real key\n-----END OPENSSH PRIVATE KEY-----\n"
	testKnownHosts = "github.com ssh-ed25519 AAAA\n"
	testCloneName  = "clone"
)

// privateApp is an App that builds a private repository over SSH.
func privateApp(name string) *platformatorv1alpha1.App {
	app := sourceApp(name)
	app.Spec.Source.Git.URL = testSSHURL
	app.Spec.Source.Git.DeployKeySecret = testDeployKey
	return app
}

func TestBuildJobForAPrivateRepository(t *testing.T) {
	app := privateApp("private-job")
	app.Spec.Source.Git.Path = "services/api"
	r := &AppReconciler{Registry: testRegistry, RegistrySecret: testRegistrySecret, BuilderImage: testBuilderImage}
	job := r.buildJob(app, testCommitOne, imageFor(testRegistry, app, testCommitOne), buildJobName(app, testCommitOne))
	pod := job.Spec.Template.Spec

	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != testCloneName {
		t.Fatalf("want one clone init container, got %+v", pod.InitContainers)
	}
	clone := pod.InitContainers[0]
	env := map[string]string{}
	for _, e := range clone.Env {
		env[e.Name] = e.Value
	}
	if env["REPOSITORY"] != testSSHURL || env["COMMIT"] != testCommitOne {
		t.Errorf("the clone needs the repository and the commit, got %v", env)
	}
	if !strings.Contains(env["GIT_SSH_COMMAND"], "StrictHostKeyChecking=yes") {
		t.Errorf("host keys must be checked, got %q", env["GIT_SSH_COMMAND"])
	}
	if !strings.Contains(env["GIT_SSH_COMMAND"], "UserKnownHostsFile=/ssh/known_hosts") {
		t.Errorf("host keys must come from the Secret, got %q", env["GIT_SSH_COMMAND"])
	}
	if clone.Resources.Requests == nil || clone.Resources.Limits == nil {
		t.Error("a ResourceQuota refuses containers without requests and limits")
	}

	args := strings.Join(pod.Containers[0].Args, " ")
	for _, want := range []string{"context=/workspace/services/api", "dockerfile=/workspace/services/api"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if strings.Contains(args, testSSHURL) {
		t.Errorf("BuildKit must not fetch the repository itself, args %q", args)
	}

	if pod.SecurityContext == nil || pod.SecurityContext.FSGroup == nil {
		t.Error("fsGroup is needed to read the key from the Secret volume")
	}
	var found bool
	for _, v := range pod.Volumes {
		if v.Secret != nil && v.Secret.SecretName == testDeployKey {
			found = true
			if v.Secret.DefaultMode == nil || *v.Secret.DefaultMode&0o007 != 0 {
				t.Errorf("the key must not be readable by everyone, mode %v", v.Secret.DefaultMode)
			}
		}
	}
	if !found {
		t.Error("the deploy key Secret is not mounted")
	}
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.Name == "deploy-key" {
			t.Error("the build container must not see the deploy key")
		}
	}
}

func TestBuildJobForAPublicRepositoryHasNoClone(t *testing.T) {
	app := sourceApp("public-job")
	r := &AppReconciler{Registry: testRegistry, RegistrySecret: testRegistrySecret, BuilderImage: testBuilderImage}
	job := r.buildJob(app, testCommitOne, imageFor(testRegistry, app, testCommitOne), buildJobName(app, testCommitOne))
	if len(job.Spec.Template.Spec.InitContainers) != 0 {
		t.Errorf("a public repository is fetched by BuildKit, got %+v", job.Spec.Template.Spec.InitContainers)
	}
}

var _ = Describe("App source with a private repository", func() {
	ctx := context.Background()

	newReconciler := func(resolver HeadResolver) *AppReconciler {
		return &AppReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain,
			Registry: testRegistry, RegistrySecret: testRegistrySecret,
			BuilderImage: testBuilderImage, Resolver: resolver,
			BuildTimeout: DefaultBuildTimeout,
		}
	}

	It("needs a deploy key for an SSH address, and only then", func() {
		keyless := privateApp("ssh-no-key")
		keyless.Spec.Source.Git.DeployKeySecret = ""
		err := k8sClient.Create(ctx, keyless)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("set deployKeySecret for an SSH url"))

		pointless := sourceApp("https-with-key")
		pointless.Spec.Source.Git.DeployKeySecret = testDeployKey
		err = k8sClient.Create(ctx, pointless)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("set deployKeySecret for an SSH url"))

		urlOnly := privateApp("ssh-scheme")
		urlOnly.Spec.Source.Git.URL = "ssh://git@github.com/owner/private.git"
		Expect(k8sClient.Create(ctx, urlOnly)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, urlOnly)).To(Succeed()) })
	})

	It("waits for the deploy key Secret and says how to create it", func() {
		ensureRegistrySecret(ctx)
		app := privateApp("private-nokey")
		app.Spec.Source.Git.DeployKeySecret = "missing-deploy-key"
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })

		name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
		resolver := &fakeResolver{commit: testCommitOne}
		result, err := newReconciler(resolver).Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0), "it must look again, the Secret may be created")

		err = k8sClient.Get(ctx, types.NamespacedName{Name: "private-nokey-build-1111111", Namespace: app.Namespace}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no Job without the key")

		Expect(k8sClient.Get(ctx, name, app)).To(Succeed())
		Expect(app.Status.Build.State).To(Equal(buildNoKey))
		Expect(app.Status.Build.Message).To(ContainSubstring("missing-deploy-key"))
		built := meta.FindStatusCondition(app.Status.Conditions, conditionBuilt)
		Expect(built).NotTo(BeNil())
		Expect(built.Status).To(Equal(metav1.ConditionFalse))
		Expect(built.Reason).To(Equal(reasonDeployKeyMissing))
	})

	It("refuses a Secret that lacks a key", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "half-key", Namespace: testNamespace},
			Data:       map[string][]byte{corev1.SSHAuthPrivateKey: []byte(testPrivateKey)},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, secret)).To(Succeed()) })
		ensureRegistrySecret(ctx)
		app := privateApp("private-halfkey")
		app.Spec.Source.Git.DeployKeySecret = "half-key"
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })

		name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
		_, err := newReconciler(&fakeResolver{commit: testCommitOne}).Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, name, app)).To(Succeed())
		Expect(app.Status.Build.State).To(Equal(buildNoKey))
		Expect(app.Status.Build.Message).To(ContainSubstring(knownHostsKey))
	})

	It("looks up the branch with the key and builds from a clone", func() {
		ensureRegistrySecret(ctx)
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testDeployKey, Namespace: testNamespace},
			Data: map[string][]byte{
				corev1.SSHAuthPrivateKey: []byte(testPrivateKey),
				knownHostsKey:            []byte(testKnownHosts),
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, secret)).To(Succeed()) })
		app := privateApp("private-build")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })

		name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
		resolver := &fakeResolver{commit: testCommitOne}
		_, err := newReconciler(resolver).Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())

		Expect(resolver.auth).NotTo(BeNil())
		Expect(string(resolver.auth.PrivateKey)).To(Equal(testPrivateKey))
		Expect(string(resolver.auth.KnownHosts)).To(Equal(testKnownHosts))

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "private-build-build-1111111", Namespace: app.Namespace}, job)).To(Succeed())
		Expect(job.Spec.Template.Spec.InitContainers).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.Containers[0].Args).To(ContainElement("context=/workspace"))
	})
})
