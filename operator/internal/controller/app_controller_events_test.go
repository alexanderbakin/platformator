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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("App events", func() {
	ctx := context.Background()

	It("sends an event when the status changes, and stays quiet when it does not", func() {
		app := validApp("events-app")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, app)).To(Succeed())
		})

		recorder := events.NewFakeRecorder(10)
		reconciler := &AppReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Domain:   testDomain,
			Recorder: recorder,
		}
		request := reconcile.Request{NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}}

		// First pass: Reconciled and Ready both appear for the first time.
		_, err := reconciler.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(recorder.Events).To(Receive(ContainSubstring("ReconcileSucceeded")))
		Expect(recorder.Events).To(Receive(ContainSubstring("DeploymentUnavailable")))

		// Second pass: nothing changed, so there is nothing to announce.
		_, err = reconciler.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(recorder.Events).To(BeEmpty())
	})
})
