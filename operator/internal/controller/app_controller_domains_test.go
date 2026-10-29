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
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

const (
	shopDomain   = "shop.example.org"
	sharedDomain = "shared.example.org"
)

func TestUnderPlatformDomain(t *testing.T) {
	r := &AppReconciler{Domain: "Paas.Example.com"}
	cases := map[string]bool{
		"paas.example.com":          true,
		"app.paas.example.com":      true,
		"a.b.paas.example.com":      true,
		"example.com":               false,
		"notpaas.example.com":       false,
		"paas.example.com.evil.org": false,
		shopDomain:                  false,
	}
	for domain, want := range cases {
		if got := r.underPlatformDomain(domain); got != want {
			t.Errorf("underPlatformDomain(%q) = %v, want %v", domain, got, want)
		}
	}
	if (&AppReconciler{}).underPlatformDomain("anything.dev") {
		t.Error("without a platform domain, no name is under it")
	}
}

func TestDomainSecretName(t *testing.T) {
	app := &platformatorv1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	first := domainSecretName(app, "a.dev")
	if first != domainSecretName(app, "a.dev") {
		t.Error("the name must not change between passes")
	}
	if first == domainSecretName(app, "b.dev") {
		t.Error("two domains must not share a Secret")
	}
	if first == tlsSecretName(app) {
		t.Error("a domain must not share the Secret of the App's own address")
	}
	if !strings.HasPrefix(first, "shop-tls-") || len(first) != len("shop-tls-")+8 {
		t.Errorf("unexpected name %q", first)
	}
}

func TestDomainOwner(t *testing.T) {
	oldest := metav1.Now()
	middle := metav1.NewTime(oldest.Add(time.Minute))
	newest := metav1.NewTime(oldest.Add(2 * time.Minute))
	app := func(name string, created metav1.Time, domains ...string) platformatorv1alpha1.App {
		return platformatorv1alpha1.App{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", CreationTimestamp: created},
			Spec:       platformatorv1alpha1.AppSpec{Domains: domains},
		}
	}
	apps := &platformatorv1alpha1.AppList{Items: []platformatorv1alpha1.App{
		app("middle", middle, sharedDomain),
		app("oldest", oldest, sharedDomain, "only-oldest.example.org"),
		app("newest", newest, sharedDomain),
	}}

	if got := domainOwner(apps, &apps.Items[2], sharedDomain); got == nil || got.Name != "oldest" {
		t.Errorf("the newest App must lose to the oldest, got %v", got)
	}
	if got := domainOwner(apps, &apps.Items[0], sharedDomain); got == nil || got.Name != "oldest" {
		t.Errorf("the middle App must lose to the oldest, got %v", got)
	}
	if got := domainOwner(apps, &apps.Items[1], sharedDomain); got != nil {
		t.Errorf("the oldest App keeps the name, got %v", got.Name)
	}
	if got := domainOwner(apps, &apps.Items[2], "free.example.org"); got != nil {
		t.Errorf("a name nobody else lists is free, got %v", got.Name)
	}
}

var _ = Describe("Custom domains", func() {
	ctx := context.Background()

	reconcileApp := func(app *platformatorv1alpha1.App) reconcile.Result {
		reconciler := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace},
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}
	createApp := func(name string, domains ...string) *platformatorv1alpha1.App {
		app := validApp(name)
		app.Spec.Domains = domains
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, app))).To(Succeed()) })
		return app
	}
	fetch := func(app *platformatorv1alpha1.App) {
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, app)).To(Succeed())
	}
	ingressOf := func(app *platformatorv1alpha1.App) *networkingv1.Ingress {
		ing := &networkingv1.Ingress{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, ing)).To(Succeed())
		return ing
	}
	hostsOf := func(ing *networkingv1.Ingress) []string {
		hosts := make([]string, 0, len(ing.Spec.Rules))
		for _, rule := range ing.Spec.Rules {
			hosts = append(hosts, rule.Host)
		}
		return hosts
	}

	It("serves the App on each domain, with a certificate of its own for each name", func() {
		app := createApp("domains-ok", shopDomain, "www."+shopDomain)
		reconcileApp(app)

		ing := ingressOf(app)
		Expect(hostsOf(ing)).To(Equal([]string{"domains-ok." + testDomain, shopDomain, "www." + shopDomain}))
		Expect(ing.Spec.TLS).To(HaveLen(3))
		secrets := map[string]bool{}
		for _, tls := range ing.Spec.TLS {
			Expect(tls.Hosts).To(HaveLen(1), "one name per certificate, so one bad name cannot hold up the others")
			secrets[tls.SecretName] = true
		}
		Expect(secrets).To(HaveLen(3))
		Expect(ing.Spec.TLS[0].SecretName).To(Equal(tlsSecretName(app)))
		Expect(ing.Spec.TLS[1].SecretName).To(Equal(domainSecretName(app, shopDomain)))

		By("keeping the App's own address as its URL")
		fetch(app)
		Expect(app.Status.URL).To(Equal("https://domains-ok." + testDomain))

		By("reporting the domains in their own condition, not in Ready")
		cond := meta.FindStatusCondition(app.Status.Conditions, conditionDomainsReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionUnknown), "there is no cert-manager in the test cluster")
		Expect(cond.Message).To(ContainSubstring(shopDomain))
	})

	It("stops serving a domain that was removed from the list", func() {
		app := createApp("domains-removed", shopDomain, "www."+shopDomain)
		reconcileApp(app)

		fetch(app)
		app.Spec.Domains = []string{shopDomain}
		Expect(k8sClient.Update(ctx, app)).To(Succeed())
		reconcileApp(app)

		ing := ingressOf(app)
		Expect(hostsOf(ing)).To(Equal([]string{"domains-removed." + testDomain, shopDomain}))
		Expect(ing.Spec.TLS).To(HaveLen(2))
	})

	It("has no DomainsReady condition without custom domains", func() {
		app := createApp("domains-none")
		reconcileApp(app)

		fetch(app)
		Expect(meta.FindStatusCondition(app.Status.Conditions, conditionDomainsReady)).To(BeNil())
		Expect(ingressOf(app).Spec.Rules).To(HaveLen(1))
	})

	It("refuses a name under the platform domain and still serves the others", func() {
		app := createApp("domains-platform", "stolen."+testDomain, shopDomain)
		reconcileApp(app)

		Expect(hostsOf(ingressOf(app))).To(Equal([]string{"domains-platform." + testDomain, shopDomain}))
		fetch(app)
		cond := meta.FindStatusCondition(app.Status.Conditions, conditionDomainsReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(reasonDomainNotAllowed))
		Expect(cond.Message).To(ContainSubstring("stolen." + testDomain))
	})

	It("gives a name to the older App and holds it back from the newer one", func() {
		first := createApp("domains-first", sharedDomain)
		second := createApp("domains-second", sharedDomain, "second-only.example.org")

		result := reconcileApp(second)
		Expect(result.RequeueAfter).To(BeNumerically(">", 0), "it must notice when the other App goes away")
		Expect(hostsOf(ingressOf(second))).To(Equal([]string{"domains-second." + testDomain, "second-only.example.org"}))
		fetch(second)
		cond := meta.FindStatusCondition(second.Status.Conditions, conditionDomainsReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(reasonDomainTaken))
		Expect(cond.Message).To(ContainSubstring(testNamespace + "/domains-first"))

		reconcileApp(first)
		Expect(hostsOf(ingressOf(first))).To(ContainElement(sharedDomain))

		By("handing the name over once the older App is gone")
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())
		reconcileApp(second)
		Expect(hostsOf(ingressOf(second))).To(ContainElement(sharedDomain))
	})
})

var _ = Describe("Custom domain validation", func() {
	ctx := context.Background()

	withDomains := func(name string, domains ...string) *platformatorv1alpha1.App {
		app := validApp(name)
		app.Spec.Domains = domains
		return app
	}
	expectRejected := func(app *platformatorv1alpha1.App) {
		err := k8sClient.Create(ctx, app)
		if err == nil {
			_ = k8sClient.Delete(ctx, app)
		}
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err.Error()).To(ContainSubstring("domains"))
	}

	It("accepts ordinary names, including internationalised ones", func() {
		app := withDomains("domains-valid", shopDomain, "a-b.example.co.uk", "xn--80aswg.xn--p1ai")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		Expect(k8sClient.Delete(ctx, app)).To(Succeed())
	})

	It("rejects names the HTTP-01 challenge or an Ingress could not use", func() {
		expectRejected(withDomains("domains-wildcard", "*."+shopDomain))
		expectRejected(withDomains("domains-upper", "Shop.example.org"))
		expectRejected(withDomains("domains-ip", "10.0.0.1"))
		expectRejected(withDomains("domains-bare", "localhost"))
		expectRejected(withDomains("domains-dot", shopDomain+"."))
		expectRejected(withDomains("domains-long-label", strings.Repeat("a", 64)+".example.org"))
	})

	It("rejects a name listed twice and more than ten names", func() {
		expectRejected(withDomains("domains-twice", shopDomain, shopDomain))

		many := make([]string, 0, 11)
		for i := range 11 {
			many = append(many, strings.Repeat("a", i+1)+".example.org")
		}
		expectRejected(withDomains("domains-many", many...))
	})
})
