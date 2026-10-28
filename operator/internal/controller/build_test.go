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
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
	"github.com/alexanderbakin/platformator/operator/internal/gitrepo"
)

const (
	testRepoURL   = "https://github.com/owner/repo"
	testCommitOne = "1111111111111111111111111111111111111111"
	testCommitTwo = "2222222222222222222222222222222222222222"
	testRegistry  = "ghcr.io/owner"

	testRegistrySecret = "registry-credentials"
	testBuilderImage   = "moby/buildkit:rootless"
)

// ensureRegistrySecret creates the Secret the build Jobs need, unless an earlier
// spec already did. Specs share one namespace and nothing deletes Secrets there.
func ensureRegistrySecret(ctx context.Context) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testRegistrySecret, Namespace: testNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")},
	}
	if err := k8sClient.Create(ctx, secret); !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

// fakeResolver answers with a fixed commit, so tests need no Git server.
type fakeResolver struct {
	commit string
	err    error
	// auth is the deploy key of the last lookup, nil for a public repository.
	auth *gitrepo.SSHAuth
}

func (f *fakeResolver) Head(_ context.Context, _, _ string, auth *gitrepo.SSHAuth) (string, error) {
	f.auth = auth
	return f.commit, f.err
}

func sourceApp(name string) *platformatorv1alpha1.App {
	app := validApp(name)
	app.Spec.Image = ""
	app.Spec.Source = &platformatorv1alpha1.Source{Git: platformatorv1alpha1.GitSource{URL: testRepoURL}}
	return app
}

func TestGitContext(t *testing.T) {
	src := &platformatorv1alpha1.GitSource{URL: testRepoURL}
	if got, want := gitContext(src, testCommitOne), testRepoURL+".git#"+testCommitOne; got != want {
		t.Errorf("root: got %q, want %q", got, want)
	}
	src = &platformatorv1alpha1.GitSource{URL: testRepoURL + ".git/", Path: "/services/api/"}
	if got, want := gitContext(src, testCommitOne), testRepoURL+".git#"+testCommitOne+":services/api"; got != want {
		t.Errorf("subdir: got %q, want %q", got, want)
	}
}

func TestBuildJobNameFitsALabel(t *testing.T) {
	app := validApp(strings.Repeat("a", 63))
	if name := buildJobName(app, testCommitOne); len(name) > 63 {
		t.Errorf("job name %q is %d characters, the limit is 63", name, len(name))
	}
	if got, want := buildJobName(validApp("hello"), testCommitOne), "hello-build-1111111"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestImageFor(t *testing.T) {
	app := validApp("hello")
	if got, want := imageFor(testRegistry+"/", app, testCommitOne), testRegistry+"/default-hello:111111111111"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestJobFinished(t *testing.T) {
	job := func(t batchv1.JobConditionType) *batchv1.Job {
		return &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: t, Status: corev1.ConditionTrue},
		}}}
	}
	if done, _ := jobFinished(&batchv1.Job{}); done {
		t.Error("a Job without conditions is not finished")
	}
	if done, ok := jobFinished(job(batchv1.JobComplete)); !done || !ok {
		t.Error("a Complete Job is finished and worked")
	}
	if done, ok := jobFinished(job(batchv1.JobFailed)); !done || ok {
		t.Error("a Failed Job is finished and did not work")
	}
}

func TestBuildCondition(t *testing.T) {
	app := sourceApp("hello")
	if buildCondition(app) != nil {
		t.Error("no build status, no condition")
	}
	cases := map[string]metav1.ConditionStatus{
		buildRunning:   metav1.ConditionUnknown,
		buildSucceeded: metav1.ConditionTrue,
		buildFailed:    metav1.ConditionFalse,
	}
	for state, want := range cases {
		app.Status.Build = &platformatorv1alpha1.BuildStatus{State: state, Commit: testCommitOne}
		if got := buildCondition(app); got == nil || got.Status != want {
			t.Errorf("state %s: got %v, want status %s", state, got, want)
		}
	}
	if buildCondition(validApp("plain")) != nil {
		t.Error("an App with an image has no Built condition")
	}
}

var _ = Describe("App source", func() {
	ctx := context.Background()

	newReconciler := func(resolver HeadResolver) *AppReconciler {
		return &AppReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain,
			Registry: testRegistry, RegistrySecret: testRegistrySecret,
			BuilderImage: testBuilderImage, Resolver: resolver,
			BuildTimeout: DefaultBuildTimeout,
		}
	}

	It("is rejected together with an image, or without either", func() {
		both := sourceApp("both")
		both.Spec.Image = testImage
		err := k8sClient.Create(ctx, both)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("exactly one of image and source"))

		neither := sourceApp("neither")
		neither.Spec.Source = nil
		err = k8sClient.Create(ctx, neither)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("exactly one of image and source"))
	})

	It("starts a build for each new commit and runs nothing until one finishes", func() {
		ensureRegistrySecret(ctx)
		app := sourceApp("git-app")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })

		name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
		resolver := &fakeResolver{commit: testCommitOne}
		reconciler := newReconciler(resolver)

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "git-app-build-1111111", Namespace: app.Namespace}, job)).To(Succeed())
		args := job.Spec.Template.Spec.Containers[0].Args
		Expect(args).To(ContainElement("context=" + testRepoURL + ".git#" + testCommitOne))
		Expect(args).To(ContainElement("type=image,name=" + testRegistry + "/default-git-app:111111111111,push=true"))
		Expect(job.OwnerReferences).To(HaveLen(1))
		Expect(job.Spec.ActiveDeadlineSeconds).To(HaveValue(Equal(int64(DefaultBuildTimeout.Seconds()))))
		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Resources.Requests).To(HaveKey(corev1.ResourceCPU), "a ResourceQuota refuses pods without requests")
		Expect(container.Resources.Limits).To(HaveKey(corev1.ResourceMemory))
		Expect(args).To(ContainElement(ContainSubstring("--import-cache")))
		Expect(args).To(ContainElement("type=registry,ref=" + testRegistry + "/default-git-app:buildcache"))

		Expect(k8sClient.Get(ctx, name, app)).To(Succeed())
		Expect(app.Status.Build).NotTo(BeNil())
		Expect(app.Status.Build.State).To(Equal(buildRunning))
		Expect(app.Status.Build.Commit).To(Equal(testCommitOne))
		ready := meta.FindStatusCondition(app.Status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(reasonImageNotBuilt))
		built := meta.FindStatusCondition(app.Status.Conditions, conditionBuilt)
		Expect(built).NotTo(BeNil())
		Expect(built.Reason).To(Equal(reasonBuildRunning))

		// No image yet, so no Deployment.
		err = k8sClient.Get(ctx, name, &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		// The same commit again does not build twice; a new one does.
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		resolver.commit = testCommitTwo
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())

		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs)).To(Succeed())
		var names []string
		for _, j := range jobs.Items {
			if j.Namespace == app.Namespace && strings.HasPrefix(j.Name, "git-app-build-") {
				names = append(names, j.Name)
			}
		}
		Expect(names).To(ConsistOf("git-app-build-1111111", "git-app-build-2222222"))
	})

	It("keeps going when the branch cannot be read", func() {
		app := sourceApp("git-down")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })

		name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
		reconciler := newReconciler(&fakeResolver{err: context.DeadlineExceeded})
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		Expect(k8sClient.Get(ctx, name, app)).To(Succeed())
		Expect(app.Status.Build.Message).To(ContainSubstring("cannot read the branch"))
		reconciled := meta.FindStatusCondition(app.Status.Conditions, "Reconciled")
		Expect(reconciled.Status).To(Equal(metav1.ConditionTrue))
	})

	It("does not start a build while the registry Secret is missing", func() {
		app := sourceApp("git-nosecret")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })

		name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
		reconciler := newReconciler(&fakeResolver{commit: testCommitOne})
		reconciler.RegistrySecret = "no-such-secret"
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0), "it must look again, the Secret may be created")

		err = k8sClient.Get(ctx, types.NamespacedName{Name: "git-nosecret-build-1111111", Namespace: app.Namespace}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no Job without credentials")

		Expect(k8sClient.Get(ctx, name, app)).To(Succeed())
		Expect(app.Status.Build.State).To(Equal(buildBlocked))
		Expect(app.Status.Build.Message).To(ContainSubstring("no-such-secret"))
		built := meta.FindStatusCondition(app.Status.Conditions, conditionBuilt)
		Expect(built).NotTo(BeNil())
		Expect(built.Status).To(Equal(metav1.ConditionFalse))
		Expect(built.Reason).To(Equal(reasonSecretMissing))
		ready := meta.FindStatusCondition(app.Status.Conditions, "Ready")
		Expect(ready.Reason).To(Equal(reasonImageNotBuilt))
		Expect(ready.Message).To(ContainSubstring("no-such-secret"))
	})

	It("queues a build while too many are running", func() {
		ensureRegistrySecret(ctx)
		first, second := sourceApp("queue-first"), sourceApp("queue-second")
		for _, a := range []*platformatorv1alpha1.App{first, second} {
			Expect(k8sClient.Create(ctx, a)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, a)).To(Succeed()) })
		}

		reconciler := newReconciler(&fakeResolver{commit: testCommitOne})
		// Jobs from other specs never finish in envtest, so count what is there
		// and leave room for exactly one more.
		running, err := reconciler.runningBuilds(ctx)
		Expect(err).NotTo(HaveOccurred())
		reconciler.MaxBuilds = running + 1

		for _, a := range []*platformatorv1alpha1.App{first, second} {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: a.Name, Namespace: a.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())
		}

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "queue-first-build-1111111", Namespace: testNamespace}, &batchv1.Job{})).To(Succeed())
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "queue-second-build-1111111", Namespace: testNamespace}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the second build must wait")

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: second.Name, Namespace: testNamespace}, second)).To(Succeed())
		Expect(second.Status.Build.State).To(Equal(buildQueued))
		built := meta.FindStatusCondition(second.Status.Conditions, conditionBuilt)
		Expect(built.Reason).To(Equal(reasonBuildQueued))
	})

	It("reports a missing registry", func() {
		app := sourceApp("git-noreg")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })

		name := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
		reconciler := newReconciler(&fakeResolver{commit: testCommitOne})
		reconciler.Registry = ""
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("registry"))
	})
})
