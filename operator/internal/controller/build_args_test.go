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
	"strings"
	"testing"
)

func TestBuildJobArgsForTheRegistryMode(t *testing.T) {
	app := sourceApp("args-app")
	image := imageFor(testRegistry, app, testCommitOne)
	name := buildJobName(app, testCommitOne)
	argsFor := func(r *AppReconciler) string {
		job := r.buildJob(app, testCommitOne, image, name)
		return strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	}

	t.Run("a registry on the network gets a cache and verified TLS", func(t *testing.T) {
		args := argsFor(&AppReconciler{Registry: testRegistry})
		for _, want := range []string{"push=true", "--import-cache", "--export-cache"} {
			if !strings.Contains(args, want) {
				t.Errorf("args %q lack %q", args, want)
			}
		}
		if strings.Contains(args, "registry.insecure") {
			t.Errorf("args %q allow an insecure registry", args)
		}
	})

	t.Run("an insecure registry is pushed to over HTTP, without a cache", func(t *testing.T) {
		args := argsFor(&AppReconciler{Registry: testRegistry, RegistryInsecure: true})
		for _, want := range []string{"push=true", "registry.insecure=true"} {
			if !strings.Contains(args, want) {
				t.Errorf("args %q lack %q", args, want)
			}
		}
		for _, unwanted := range []string{"--import-cache", "--export-cache"} {
			if strings.Contains(args, unwanted) {
				t.Errorf("args %q contain %q", args, unwanted)
			}
		}
	})
}
