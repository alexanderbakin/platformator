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
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

// HeadResolver finds the latest commit of a branch of a Git repository.
type HeadResolver interface {
	Head(ctx context.Context, repoURL, branch string) (string, error)
}

// Build states, as stored in status.build.state.
const (
	buildRunning   = "Running"
	buildSucceeded = "Succeeded"
	buildFailed    = "Failed"
)

// Names of the Built condition and its reasons.
const (
	conditionBuilt         = "Built"
	reasonBuildRunning     = "BuildRunning"
	reasonBuildSucceeded   = "BuildSucceeded"
	reasonBuildFailed      = "BuildFailed"
	reasonBranchUnreadable = "BranchUnreadable"
	reasonImageNotBuilt    = "ImageNotBuilt"

	// buildPollInterval is how often the branch is checked for a new commit.
	buildPollInterval = time.Minute

	// buildJobTTL is how long a finished build Job (and its logs) is kept.
	buildJobTTL = int32(24 * 60 * 60)

	// shortCommitLength is the number of commit characters in an image tag.
	shortCommitLength = 12

	// buildkitName names the build container and its state volume.
	buildkitName = "buildkit"

	// dockerConfigDir is where the registry Secret is mounted for BuildKit.
	dockerConfigDir = "/home/user/.docker"
)

// buildOutcome is what a pass learned about the App's builds.
type buildOutcome struct {
	// image is the image the Deployment should run. Empty means "nothing to
	// run yet": the first build has not finished.
	image string
	// requeue says when to look at the branch again. Zero means never.
	requeue time.Duration
}

// reconcileBuild keeps the image of an App with spec.source up to date: it
// looks up the branch head, starts a BuildKit Job for a commit that has none,
// and reports the image of the newest build that worked. An App with
// spec.image just runs that image.
//
// The image of the last good build keeps running while a newer commit builds or
// fails to build, so a broken commit never takes the App down.
func (r *AppReconciler) reconcileBuild(ctx context.Context, app *platformatorv1alpha1.App) (buildOutcome, error) {
	src := app.Spec.Source
	if src == nil {
		app.Status.Build = nil
		return buildOutcome{image: app.Spec.Image}, nil
	}
	if r.Registry == "" {
		return buildOutcome{}, fmt.Errorf("spec.source needs the operator to know a registry: set --registry (REGISTRY)")
	}
	if app.Status.Build == nil {
		app.Status.Build = &platformatorv1alpha1.BuildStatus{}
	}
	status := app.Status.Build
	out := buildOutcome{image: status.Image, requeue: buildPollInterval}

	if r.Resolver == nil {
		return buildOutcome{}, fmt.Errorf("no Git resolver configured")
	}
	commit, err := r.Resolver.Head(ctx, src.Git.URL, src.Git.Branch)
	if err != nil {
		// A flaky or unreachable Git host must not fail the App; keep what runs.
		status.Message = "cannot read the branch: " + err.Error()
		return out, nil
	}
	if status.Message != "" && status.State != buildFailed {
		status.Message = ""
	}

	image := imageFor(r.Registry, app, commit)
	jobName := buildJobName(app, commit)

	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: jobName}, &job)
	switch {
	case err == nil:
		// Fall through to read the Job's state below.
	case !apierrors.IsNotFound(err):
		return out, err
	case status.Commit == commit && (status.State == buildSucceeded || status.State == buildFailed):
		// The Job was cleaned up after finishing; do not build the commit again.
		return out, nil
	default:
		job = *r.buildJob(app, commit, image, jobName)
		if err := controllerutil.SetControllerReference(app, &job, r.Scheme); err != nil {
			return out, err
		}
		if err := r.Create(ctx, &job); err != nil && !apierrors.IsAlreadyExists(err) {
			return out, fmt.Errorf("creating build job: %w", err)
		}
	}

	status.Commit = commit
	status.JobName = jobName
	switch done, ok := jobFinished(&job); {
	case done && ok:
		status.State = buildSucceeded
		status.Message = ""
		status.Image = image
		out.image = image
	case done:
		status.State = buildFailed
		status.Message = jobFailureMessage(&job)
	default:
		status.State = buildRunning
		status.Message = ""
	}
	return out, nil
}

// jobFinished reports whether a Job has ended and, if so, whether it worked.
func jobFinished(job *batchv1.Job) (done, succeeded bool) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, true
		case batchv1.JobFailed:
			return true, false
		}
	}
	return false, false
}

func jobFailureMessage(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return fmt.Sprintf("build job %s failed (%s): see `kubectl logs job/%s`", job.Name, c.Reason, job.Name)
		}
	}
	return "build job " + job.Name + " failed"
}

// buildCondition turns the build state into the Built condition. It is nil for
// an App without spec.source.
func buildCondition(app *platformatorv1alpha1.App) *metav1.Condition {
	if app.Spec.Source == nil || app.Status.Build == nil {
		return nil
	}
	b := app.Status.Build
	cond := metav1.Condition{Type: conditionBuilt, ObservedGeneration: app.Generation}
	switch b.State {
	case buildSucceeded:
		cond.Status = metav1.ConditionTrue
		cond.Reason = reasonBuildSucceeded
		cond.Message = "Built " + shortCommit(b.Commit)
	case buildFailed:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonBuildFailed
		cond.Message = b.Message
	case buildRunning:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = reasonBuildRunning
		cond.Message = "Building " + shortCommit(b.Commit)
	default:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = reasonBranchUnreadable
		cond.Message = b.Message
	}
	return &cond
}

func shortCommit(commit string) string {
	if len(commit) > shortCommitLength {
		return commit[:shortCommitLength]
	}
	return commit
}

// imageFor is the image a commit of the App is pushed to.
func imageFor(registry string, app *platformatorv1alpha1.App, commit string) string {
	return fmt.Sprintf("%s/%s-%s:%s", strings.TrimSuffix(registry, "/"), app.Namespace, app.Name, shortCommit(commit))
}

// buildJobName names the Job of a commit. App names can be 63 characters long,
// so the name is cut to leave room for the suffix inside the 63 of a label.
func buildJobName(app *platformatorv1alpha1.App, commit string) string {
	const maxName = 40
	name := app.Name
	if len(name) > maxName {
		name = strings.TrimRight(name[:maxName], "-.")
	}
	return fmt.Sprintf("%s-build-%s", name, commit[:7])
}

// gitContext is the BuildKit build context for a commit: the repository at
// that commit, optionally narrowed to a directory.
func gitContext(src *platformatorv1alpha1.GitSource, commit string) string {
	repo := strings.TrimSuffix(src.URL, "/")
	if !strings.HasSuffix(repo, ".git") {
		repo += ".git"
	}
	ctx := repo + "#" + commit
	if p := strings.Trim(src.Path, "/"); p != "" {
		ctx += ":" + p
	}
	return ctx
}

// buildJob is the rootless BuildKit Job that builds a commit and pushes it.
func (r *AppReconciler) buildJob(app *platformatorv1alpha1.App, commit, image, name string) *batchv1.Job {
	src := app.Spec.Source.Git
	dockerfile := src.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	labels := map[string]string{"app": app.Name, "platformator.alexanderbakin.com/build": "true"}
	zero := int32(0)
	ttl := buildJobTTL
	uid := int64(1000)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &zero,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    buildkitName,
						Image:   r.BuilderImage,
						Command: []string{"buildctl-daemonless.sh"},
						Args: []string{
							"build",
							"--frontend", "dockerfile.v0",
							"--opt", "context=" + gitContext(&src, commit),
							"--opt", "filename=" + dockerfile,
							"--output", "type=image,name=" + image + ",push=true",
						},
						Env: []corev1.EnvVar{
							{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
							{Name: "DOCKER_CONFIG", Value: dockerConfigDir},
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:       &uid,
							RunAsGroup:      &uid,
							SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
							AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: buildkitName, MountPath: "/home/user/.local/share/buildkit"},
							{Name: "registry", MountPath: dockerConfigDir, ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: buildkitName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "registry", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: r.RegistrySecret,
							Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
						}}},
					},
				},
			},
		},
	}
}
