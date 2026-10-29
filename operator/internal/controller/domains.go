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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

// The DomainsReady condition covers the custom domains of an App (spec.domains).
// It is separate from Ready on purpose: a name whose DNS does not point at the
// cluster yet must not make an App that serves fine on its own address look
// broken.
const (
	conditionDomainsReady  = "DomainsReady"
	reasonDomainsIssued    = "DomainsIssued"
	reasonDomainNotAllowed = "DomainNotAllowed"
	reasonDomainTaken      = "DomainTaken"

	eventActionDomains = "Domains"

	// domainPollInterval is how often an App with a domain that is not ready is
	// checked again: the certificate may be issued, or the older App that holds
	// the name may be gone. cert-manager retries the challenge by itself.
	domainPollInterval = 30 * time.Second
)

// skippedDomain is a custom domain an App does not serve, and why.
type skippedDomain struct {
	reason  string
	message string
}

// domainPlan says which of an App's custom domains it serves and which it
// does not.
type domainPlan struct {
	serve   []string
	skipped []skippedDomain
}

// planDomains splits spec.domains into the names this App serves and the ones
// it must not. A name under the platform domain belongs to the Apps' own
// addresses (and to the webhook's), so it is refused. A name another App
// already uses stays with that App: the older one keeps it, like a hostname.
func (r *AppReconciler) planDomains(
	ctx context.Context, app *platformatorv1alpha1.App,
) (domainPlan, error) {
	var plan domainPlan
	if len(app.Spec.Domains) == 0 {
		return plan, nil
	}
	var apps platformatorv1alpha1.AppList
	if err := r.List(ctx, &apps); err != nil {
		return plan, fmt.Errorf("listing Apps to check the domains: %w", err)
	}
	for _, domain := range app.Spec.Domains {
		owner := domainOwner(&apps, app, domain)
		switch {
		case r.underPlatformDomain(domain):
			plan.skipped = append(plan.skipped, skippedDomain{
				reason: reasonDomainNotAllowed,
				message: fmt.Sprintf("%s is under the platform domain %s, which is for the Apps' own addresses",
					domain, r.Domain),
			})
		case owner != nil:
			plan.skipped = append(plan.skipped, skippedDomain{
				reason:  reasonDomainTaken,
				message: fmt.Sprintf("%s is already used by App %s/%s", domain, owner.Namespace, owner.Name),
			})
		default:
			plan.serve = append(plan.serve, domain)
		}
	}
	return plan, nil
}

// underPlatformDomain reports whether domain is the platform domain or a name
// below it.
func (r *AppReconciler) underPlatformDomain(domain string) bool {
	platform := strings.ToLower(r.Domain)
	return platform != "" && (domain == platform || strings.HasSuffix(domain, "."+platform))
}

// domainOwner returns the oldest other App that is older than app and lists
// domain, or nil if app may have it. Every pass and every App reach the same
// answer, because ownsHostBefore is a strict order.
func domainOwner(
	apps *platformatorv1alpha1.AppList, app *platformatorv1alpha1.App, domain string,
) *platformatorv1alpha1.App {
	var owner *platformatorv1alpha1.App
	for i := range apps.Items {
		other := &apps.Items[i]
		if other.Namespace == app.Namespace && other.Name == app.Name {
			continue
		}
		if !slices.Contains(other.Spec.Domains, domain) || !ownsHostBefore(other, app) {
			continue
		}
		if owner == nil || ownsHostBefore(other, owner) {
			owner = other
		}
	}
	return owner
}

// domainSecretName is the Secret that holds the certificate of one custom
// domain. Each domain has its own, so a name whose DNS is wrong cannot hold up
// the certificate of the App's own address or of another domain. A hash, not
// the list position, keeps the name when the list is reordered, which would
// otherwise issue every certificate again.
func domainSecretName(app *platformatorv1alpha1.App, domain string) string {
	sum := sha256.Sum256([]byte(domain))
	return fmt.Sprintf("%s-tls-%s", app.Name, hex.EncodeToString(sum[:4]))
}

// domainsCondition works out DomainsReady, or returns nil for an App without
// custom domains. Names that cannot be served come first, because they need a
// change to the App; then certificates that are not issued yet.
func (r *AppReconciler) domainsCondition(
	ctx context.Context, app *platformatorv1alpha1.App, plan domainPlan,
) *metav1.Condition {
	if len(app.Spec.Domains) == 0 {
		return nil
	}
	cond := &metav1.Condition{Type: conditionDomainsReady, ObservedGeneration: app.Generation}

	if len(plan.skipped) > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonDomainTaken
		messages := make([]string, 0, len(plan.skipped))
		for _, skipped := range plan.skipped {
			if skipped.reason == reasonDomainNotAllowed {
				cond.Reason = reasonDomainNotAllowed
			}
			messages = append(messages, skipped.message)
		}
		cond.Message = strings.Join(messages, "; ")
		return cond
	}

	var pending []string
	var unknown *metav1.Condition
	for _, domain := range plan.serve {
		cert := r.certificateStatus(ctx, app.Namespace, domainSecretName(app, domain), app.Generation)
		switch cert.Status {
		case metav1.ConditionTrue:
		case metav1.ConditionFalse:
			pending = append(pending, domain+": "+cert.Message)
		default:
			if unknown == nil {
				cert.Message = domain + ": " + cert.Message
				unknown = &cert
			}
		}
	}

	switch {
	case len(pending) > 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonCertificatePending
		cond.Message = strings.Join(pending, "; ")
	case unknown != nil:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = unknown.Reason
		cond.Message = unknown.Message
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = reasonDomainsIssued
		cond.Message = "cert-manager issued a certificate for every domain"
	}
	return cond
}

// domainsRequeueAfter says when to look at the domains again: soon while one
// is not ready, slowly after that, to notice a renewal that fails.
func domainsRequeueAfter(cond *metav1.Condition) time.Duration {
	switch {
	case cond == nil:
		return 0
	case cond.Status == metav1.ConditionFalse, cond.Reason == reasonCertificateCheckFailed:
		return domainPollInterval
	default:
		return certificateRecheckInterval
	}
}

// emitDomainsEvent publishes an Event when DomainsReady changes status or
// reason.
func (r *AppReconciler) emitDomainsEvent(
	app *platformatorv1alpha1.App, previous map[string]metav1.Condition, cond *metav1.Condition,
) {
	if r.Recorder == nil || cond == nil {
		return
	}
	if prev, found := previous[cond.Type]; found && prev.Status == cond.Status && prev.Reason == cond.Reason {
		return
	}
	eventType := corev1.EventTypeNormal
	if cond.Reason == reasonDomainNotAllowed || cond.Reason == reasonDomainTaken {
		eventType = corev1.EventTypeWarning
	}
	r.Recorder.Eventf(app, nil, eventType, cond.Reason, eventActionDomains, "%s", cond.Message)
}
