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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

var _ = Describe("App Controller conflict handling", func() {
	It("requeues quietly on an update conflict and keeps the existing conditions", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "conflict-app", Namespace: "default"}

		app := &platformatorv1alpha1.App{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec:       platformatorv1alpha1.AppSpec{Image: "traefik/whoami:latest", Port: 80},
			Status: platformatorv1alpha1.AppStatus{Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "DeploymentAvailable",
				LastTransitionTime: metav1.Now(),
			}}},
		}

		// A client whose Deployment creation always fails with a conflict,
		// like a lost race against another writer.
		c := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithObjects(app).
			WithStatusSubresource(app).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*appsv1.Deployment); ok {
						return apierrors.NewConflict(
							schema.GroupResource{Group: "apps", Resource: "deployments"},
							obj.GetName(), errors.New("simulated conflict"))
					}
					return c.Create(ctx, obj, opts...)
				},
			}).
			Build()

		reconciler := &AppReconciler{Client: c, Scheme: c.Scheme(), Domain: "example.com"}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})

		By("returning no error and asking to be retried shortly")
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Second))

		By("leaving the conditions from the last successful pass untouched")
		updated := &platformatorv1alpha1.App{}
		Expect(c.Get(ctx, key, updated)).To(Succeed())
		Expect(meta.FindStatusCondition(updated.Status.Conditions, "Reconciled")).To(BeNil())
		ready := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal("DeploymentAvailable"))
	})
})
