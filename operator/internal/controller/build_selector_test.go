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
	"testing"

	"k8s.io/apimachinery/pkg/labels"
)

// A build pod that carries the App's selector labels is picked up by the App's
// Service, NetworkPolicy and HPA. A finished one has no CPU metrics but a CPU
// request, so the HPA stops scaling the App up. Found by a load test.
func TestBuildPodsDoNotMatchTheAppSelector(t *testing.T) {
	app := sourceApp("selector-app")
	r := &AppReconciler{Registry: testRegistry}
	job := r.buildJob(app, testCommitOne, imageFor(testRegistry, app, testCommitOne), buildJobName(app, testCommitOne))

	selector := labels.SelectorFromSet(labelsFor(app))
	if selector.Matches(labels.Set(job.Spec.Template.Labels)) {
		t.Errorf("build pod labels %v match the App selector %v", job.Spec.Template.Labels, labelsFor(app))
	}
	if selector.Matches(labels.Set(job.Labels)) {
		t.Errorf("build Job labels %v match the App selector %v", job.Labels, labelsFor(app))
	}
	if job.Spec.Template.Labels[buildOfLabel] != app.Name || job.Spec.Template.Labels[buildLabel] != "true" {
		t.Errorf("build pod labels %v lack the build labels", job.Spec.Template.Labels)
	}
}
