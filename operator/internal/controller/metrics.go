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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

// The operator exports two kinds of metrics next to the ones controller-runtime
// already provides (reconcile counts and durations, work queue depth):
//
//   - platformator_apps and platformator_app_builds describe the Apps as they are
//     now. They are computed at scrape time from the informer cache, so they
//     cannot go stale when an App is deleted or the operator restarts.
//   - platformator_builds_total and platformator_build_duration_seconds count
//     events, once per finished build.
const (
	// conditionReady is the type of the Ready condition.
	conditionReady = "Ready"

	// reasonPending labels an App that has no Ready condition yet.
	reasonPending = "Pending"

	// collectTimeout bounds the listing done on every scrape.
	collectTimeout = 5 * time.Second

	buildResultSucceeded = "succeeded"
	buildResultFailed    = "failed"
)

var (
	buildsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platformator_builds_total",
		Help: "Git builds that finished, by result (succeeded or failed).",
	}, []string{"result"})

	buildDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "platformator_build_duration_seconds",
		Help:    "How long finished Git builds took, from the Job's start to its end.",
		Buckets: []float64{15, 30, 60, 120, 240, 480, 900},
	})

	appsDesc = prometheus.NewDesc(
		"platformator_apps",
		"Number of Apps by the status and reason of their Ready condition.",
		[]string{"ready", "reason"}, nil,
	)

	appBuildsDesc = prometheus.NewDesc(
		"platformator_app_builds",
		"Number of Apps built from Git by the state of their latest build.",
		[]string{"state"}, nil,
	)
)

// RegisterMetrics adds the operator's metrics to the registry served on the
// metrics endpoint. reader is where Apps are listed from at scrape time, which
// should be the manager's cached client.
func RegisterMetrics(reader client.Reader) {
	ctrlmetrics.Registry.MustRegister(buildsTotal, buildDuration, &appsCollector{reader: reader})
}

// appsCollector reports the current Apps. It lists them on every scrape.
type appsCollector struct {
	reader client.Reader
}

// Describe sends the descriptions of the metrics the collector produces.
func (c *appsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- appsDesc
	ch <- appBuildsDesc
}

// Collect lists the Apps and sends one gauge per combination found.
func (c *appsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	var apps platformatorv1alpha1.AppList
	if err := c.reader.List(ctx, &apps); err != nil {
		ch <- prometheus.NewInvalidMetric(appsDesc, err)
		return
	}
	ready, builds := summarize(apps.Items)
	for key, count := range ready {
		ch <- prometheus.MustNewConstMetric(appsDesc, prometheus.GaugeValue, float64(count), key.status, key.reason)
	}
	for state, count := range builds {
		ch <- prometheus.MustNewConstMetric(appBuildsDesc, prometheus.GaugeValue, float64(count), state)
	}
}

// readyKey is the label set of platformator_apps.
type readyKey struct {
	status string
	reason string
}

// summarize counts Apps by Ready status and reason, and Apps with a Git source
// by the state of their latest build.
func summarize(apps []platformatorv1alpha1.App) (ready map[readyKey]int, builds map[string]int) {
	ready = map[readyKey]int{}
	builds = map[string]int{}
	for i := range apps {
		key := readyKey{status: string(metav1.ConditionUnknown), reason: reasonPending}
		if cond := meta.FindStatusCondition(apps[i].Status.Conditions, conditionReady); cond != nil {
			key = readyKey{status: string(cond.Status), reason: cond.Reason}
		}
		ready[key]++
		if apps[i].Spec.Source != nil && apps[i].Status.Build != nil && apps[i].Status.Build.State != "" {
			builds[apps[i].Status.Build.State]++
		}
	}
	return ready, builds
}

// recordBuild counts a build once, when the Built condition turns to
// succeeded or failed. Like the Events, it runs only after the new status is
// stored, and only on a change, so a pass that sees the same finished build
// again does not count it twice.
func recordBuild(previous map[string]metav1.Condition, built *metav1.Condition, took time.Duration) {
	if built == nil {
		return
	}
	if prev, found := previous[built.Type]; found && prev.Status == built.Status && prev.Reason == built.Reason {
		return
	}
	switch built.Reason {
	case reasonBuildSucceeded:
		buildsTotal.WithLabelValues(buildResultSucceeded).Inc()
	case reasonBuildFailed:
		buildsTotal.WithLabelValues(buildResultFailed).Inc()
	default:
		return
	}
	if took > 0 {
		buildDuration.Observe(took.Seconds())
	}
}

// jobDuration is how long a finished build Job ran. It is zero while the Job
// has not started or ended.
func jobDuration(job *batchv1.Job) time.Duration {
	if job.Status.StartTime == nil {
		return 0
	}
	var end time.Time
	if job.Status.CompletionTime != nil {
		end = job.Status.CompletionTime.Time
	} else {
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				end = c.LastTransitionTime.Time
			}
		}
	}
	if end.IsZero() || end.Before(job.Status.StartTime.Time) {
		return 0
	}
	return end.Sub(job.Status.StartTime.Time)
}
