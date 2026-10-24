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
	"errors"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

// stubReader lists a fixed set of Apps, so the collector is tested without a
// cluster.
type stubReader struct {
	apps []platformatorv1alpha1.App
	err  error
}

func (s stubReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return nil
}

func (s stubReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if s.err != nil {
		return s.err
	}
	list.(*platformatorv1alpha1.AppList).Items = s.apps
	return nil
}

func appWith(ready *metav1.Condition, build string) platformatorv1alpha1.App {
	app := platformatorv1alpha1.App{}
	if ready != nil {
		app.Status.Conditions = []metav1.Condition{*ready}
	}
	if build != "" {
		app.Spec.Source = &platformatorv1alpha1.Source{Git: platformatorv1alpha1.GitSource{URL: "https://example.com/repo"}}
		app.Status.Build = &platformatorv1alpha1.BuildStatus{State: build}
	}
	return app
}

func readyCondition(status metav1.ConditionStatus, reason string) *metav1.Condition {
	return &metav1.Condition{Type: conditionReady, Status: status, Reason: reason}
}

func testApps() []platformatorv1alpha1.App {
	return []platformatorv1alpha1.App{
		appWith(readyCondition(metav1.ConditionTrue, "DeploymentAvailable"), ""),
		appWith(readyCondition(metav1.ConditionFalse, reasonHostTaken), ""),
		appWith(readyCondition(metav1.ConditionFalse, reasonHostTaken), ""),
		appWith(nil, ""),
		appWith(readyCondition(metav1.ConditionTrue, "DeploymentAvailable"), buildSucceeded),
		appWith(readyCondition(metav1.ConditionTrue, "DeploymentAvailable"), buildFailed),
	}
}

func TestSummarize(t *testing.T) {
	g := gomega.NewWithT(t)

	ready, builds := summarize(testApps())

	g.Expect(ready).To(gomega.Equal(map[readyKey]int{
		{status: "True", reason: "DeploymentAvailable"}: 3,
		{status: "False", reason: reasonHostTaken}:      2,
		{status: "Unknown", reason: reasonPending}:      1,
	}))
	g.Expect(builds).To(gomega.Equal(map[string]int{buildSucceeded: 1, buildFailed: 1}))
}

// gather returns metric name -> label values joined by "/" -> value, for the
// gauges and counters of a registry.
func gather(t *testing.T, registry *prometheus.Registry) map[string]map[string]float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	out := map[string]map[string]float64{}
	for _, family := range families {
		values := map[string]float64{}
		for _, m := range family.GetMetric() {
			key := ""
			for i, label := range m.GetLabel() {
				if i > 0 {
					key += "/"
				}
				key += label.GetValue()
			}
			switch {
			case m.GetGauge() != nil:
				values[key] = m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				values[key] = m.GetCounter().GetValue()
			case m.GetHistogram() != nil:
				values[key] = float64(m.GetHistogram().GetSampleCount())
			}
		}
		out[family.GetName()] = values
	}
	return out
}

func TestAppsCollector(t *testing.T) {
	g := gomega.NewWithT(t)
	registry := prometheus.NewPedanticRegistry()
	g.Expect(registry.Register(&appsCollector{reader: stubReader{apps: testApps()}})).To(gomega.Succeed())

	got := gather(t, registry)

	g.Expect(got["platformator_apps"]).To(gomega.Equal(map[string]float64{
		"True/DeploymentAvailable": 3,
		"False/" + reasonHostTaken: 2,
		"Unknown/" + reasonPending: 1,
	}))
	g.Expect(got["platformator_app_builds"]).To(gomega.Equal(map[string]float64{
		buildSucceeded: 1,
		buildFailed:    1,
	}))
}

func TestAppsCollectorListError(t *testing.T) {
	g := gomega.NewWithT(t)
	registry := prometheus.NewPedanticRegistry()
	g.Expect(registry.Register(&appsCollector{reader: stubReader{err: errors.New("cache not ready")}})).
		To(gomega.Succeed())

	_, err := registry.Gather()

	g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("cache not ready")))
}

func TestRecordBuild(t *testing.T) {
	g := gomega.NewWithT(t)
	registry := prometheus.NewPedanticRegistry()
	g.Expect(registry.Register(buildsTotal)).To(gomega.Succeed())
	g.Expect(registry.Register(buildDuration)).To(gomega.Succeed())
	before := gather(t, registry)

	running := metav1.Condition{Type: conditionBuilt, Status: metav1.ConditionUnknown, Reason: reasonBuildRunning}
	succeeded := metav1.Condition{Type: conditionBuilt, Status: metav1.ConditionTrue, Reason: reasonBuildSucceeded}
	failed := metav1.Condition{Type: conditionBuilt, Status: metav1.ConditionFalse, Reason: reasonBuildFailed}
	was := func(c metav1.Condition) map[string]metav1.Condition { return map[string]metav1.Condition{c.Type: c} }

	recordBuild(was(running), &succeeded, 90*time.Second)   // counted
	recordBuild(was(succeeded), &succeeded, 90*time.Second) // seen again, not counted
	recordBuild(was(running), &failed, 0)                   // counted, no duration
	recordBuild(nil, &running, 0)                           // not finished, not counted
	recordBuild(was(running), nil, 0)                       // no Git source

	after := gather(t, registry)
	g.Expect(after["platformator_builds_total"][buildResultSucceeded] -
		before["platformator_builds_total"][buildResultSucceeded]).To(gomega.Equal(1.0))
	g.Expect(after["platformator_builds_total"][buildResultFailed] -
		before["platformator_builds_total"][buildResultFailed]).To(gomega.Equal(1.0))
	g.Expect(after["platformator_build_duration_seconds"][""] -
		before["platformator_build_duration_seconds"][""]).To(gomega.Equal(1.0))
}

func TestJobDuration(t *testing.T) {
	g := gomega.NewWithT(t)
	start := metav1.NewTime(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	end := metav1.NewTime(start.Add(75 * time.Second))

	g.Expect(jobDuration(&batchv1.Job{})).To(gomega.BeZero(), "not started")

	running := &batchv1.Job{Status: batchv1.JobStatus{StartTime: &start}}
	g.Expect(jobDuration(running)).To(gomega.BeZero(), "still running")

	succeeded := &batchv1.Job{Status: batchv1.JobStatus{StartTime: &start, CompletionTime: &end}}
	g.Expect(jobDuration(succeeded)).To(gomega.Equal(75 * time.Second))

	failed := &batchv1.Job{Status: batchv1.JobStatus{
		StartTime: &start,
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: end,
		}},
	}}
	g.Expect(jobDuration(failed)).To(gomega.Equal(75 * time.Second))
}
