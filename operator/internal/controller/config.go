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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

// configRevisionAnnotation is put on the pod template. Its value changes when a
// Secret or ConfigMap the App uses changes, which makes the Deployment roll
// out new pods that read the new values.
const configRevisionAnnotation = "platformator.alexanderbakin.com/config-revision"

// Kinds of objects an App can read configuration from.
const (
	kindSecret    = "Secret"
	kindConfigMap = "ConfigMap"
)

// absentRevision stands in for the version of a Secret or ConfigMap that does
// not exist, so creating it later changes the revision.
const absentRevision = "absent"

// configRef names a Secret or ConfigMap an App reads its environment from.
type configRef struct {
	kind string // kindSecret or kindConfigMap
	name string
}

// configRefs lists, without duplicates and in a stable order, the Secrets and
// ConfigMaps referenced by spec.env and spec.envFrom.
func configRefs(app *platformatorv1alpha1.App) []configRef {
	seen := map[configRef]bool{}
	add := func(kind, name string) {
		if name != "" {
			seen[configRef{kind: kind, name: name}] = true
		}
	}
	for _, e := range app.Spec.EnvFrom {
		if e.SecretRef != nil {
			add(kindSecret, e.SecretRef.Name)
		}
		if e.ConfigMapRef != nil {
			add(kindConfigMap, e.ConfigMapRef.Name)
		}
	}
	for _, e := range app.Spec.Env {
		if e.ValueFrom == nil {
			continue
		}
		if e.ValueFrom.SecretKeyRef != nil {
			add(kindSecret, e.ValueFrom.SecretKeyRef.Name)
		}
		if e.ValueFrom.ConfigMapKeyRef != nil {
			add(kindConfigMap, e.ValueFrom.ConfigMapKeyRef.Name)
		}
	}
	refs := make([]configRef, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a, b configRef) int {
		if c := strings.Compare(a.kind, b.kind); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	return refs
}

// configRevision is a short fingerprint of the resourceVersions of everything
// the App reads its environment from. It is empty when the App uses no Secret
// or ConfigMap, so such Apps never get the annotation.
//
// Only metadata is read, never the data: the operator can tell that a Secret
// changed without ever seeing what is in it. The price is that it needs
// get, list and watch on Secrets and ConfigMaps, since Kubernetes RBAC cannot
// grant metadata-only access.
func (r *AppReconciler) configRevision(ctx context.Context, app *platformatorv1alpha1.App) (string, error) {
	refs := configRefs(app)
	if len(refs) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, ref := range refs {
		obj := &metav1.PartialObjectMetadata{}
		obj.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(ref.kind))
		version := absentRevision
		err := r.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: ref.name}, obj)
		switch {
		case err == nil:
			version = obj.ResourceVersion
		case !apierrors.IsNotFound(err):
			return "", fmt.Errorf("reading %s %s: %w", ref.kind, ref.name, err)
		}
		_, _ = fmt.Fprintf(h, "%s/%s=%s\n", ref.kind, ref.name, version)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// appsUsing maps a changed Secret or ConfigMap to the Apps in its namespace
// that read from it, so they are reconciled and pick up the new revision.
func (r *AppReconciler) appsUsing(kind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var apps platformatorv1alpha1.AppList
		if err := r.List(ctx, &apps, client.InNamespace(obj.GetNamespace())); err != nil {
			logf.FromContext(ctx).Error(err, "listing Apps to find users of a changed config object")
			return nil
		}
		var requests []reconcile.Request
		for i := range apps.Items {
			for _, ref := range configRefs(&apps.Items[i]) {
				if ref.kind == kind && ref.name == obj.GetName() {
					requests = append(requests, reconcile.Request{
						NamespacedName: client.ObjectKeyFromObject(&apps.Items[i]),
					})
					break
				}
			}
		}
		return requests
	}
}
