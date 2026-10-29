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

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

const (
	reasonHostTaken = "HostTaken"

	// hostRetryInterval is how often an App that lost its hostname checks
	// whether the other App is gone.
	hostRetryInterval = 30 * time.Second
)

// hostConflict returns the App that owns the hostname of app, or nil if app
// owns it. An App is served on <name>.<domain>, so two Apps with the same name in
// different namespaces would fight over one host. The older App keeps it, and
// when creation times are equal the namespace name decides, so the answer is
// the same on every pass and for both Apps.
func (r *AppReconciler) hostConflict(
	ctx context.Context, app *platformatorv1alpha1.App,
) (*platformatorv1alpha1.App, error) {
	var apps platformatorv1alpha1.AppList
	if err := r.List(ctx, &apps); err != nil {
		return nil, err
	}
	for i := range apps.Items {
		other := &apps.Items[i]
		if other.Name == app.Name && other.Namespace != app.Namespace && ownsHostBefore(other, app) {
			return other, nil
		}
	}
	return nil, nil
}

// ownsHostBefore reports whether a claimed a shared name (a hostname or a custom
// domain) before b. It is a strict order: two different Apps are never equal,
// so exactly one of them wins.
func ownsHostBefore(a, b *platformatorv1alpha1.App) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	return a.Name < b.Name
}
