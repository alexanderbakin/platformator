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

package githubhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

var secret = []byte("shared-secret")

// fakeReader lists a fixed set of Apps.
type fakeReader struct{ apps []platformatorv1alpha1.App }

func (fakeReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("not used")
}

func (f fakeReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	list.(*platformatorv1alpha1.AppList).Items = f.apps
	return nil
}

func app(name, url, branch string) platformatorv1alpha1.App {
	return platformatorv1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: platformatorv1alpha1.AppSpec{Source: &platformatorv1alpha1.Source{
			Git: platformatorv1alpha1.GitSource{URL: url, Branch: branch},
		}},
	}
}

func sign(body string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

const pushToMain = `{"ref":"refs/heads/main","repository":{` +
	`"clone_url":"https://github.com/Owner/Repo.git","ssh_url":"git@github.com:Owner/Repo.git",` +
	`"html_url":"https://github.com/Owner/Repo"}}`

// deliver sends a request to a handler that knows the given Apps and returns the
// response and the Apps that were sent for a look.
func deliver(t *testing.T, apps []platformatorv1alpha1.App, kind, body, signature string) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	events := make(chan event.GenericEvent, 8)
	handler := &Handler{Secret: secret, Reader: fakeReader{apps: apps}, Events: events}
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(body))
	req.Header.Set(eventHeader, kind)
	if signature != "" {
		req.Header.Set(signatureHeader, signature)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	close(events)
	var names []string
	for e := range events {
		names = append(names, e.Object.GetName())
	}
	return rec, names
}

func TestPushTriggersTheAppsThatBuildThatBranch(t *testing.T) {
	apps := []platformatorv1alpha1.App{
		app("by-https", "https://github.com/owner/repo", "main"),
		app("by-ssh", "git@github.com:owner/repo.git", ""),
		app("other-branch", "https://github.com/owner/repo", "dev"),
		app("other-repo", "https://github.com/owner/else", "main"),
		{ObjectMeta: metav1.ObjectMeta{Name: "from-image"}},
	}
	rec, names := deliver(t, apps, "push", pushToMain, sign(pushToMain))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(names) != 2 || names[0] != "by-https" || names[1] != "by-ssh" {
		t.Errorf("got %v, want by-https and by-ssh", names)
	}
	if !strings.Contains(rec.Body.String(), `"triggered":2`) {
		t.Errorf("body %q", rec.Body)
	}
}

func TestPushThatBuildsNothingIsIgnored(t *testing.T) {
	apps := []platformatorv1alpha1.App{app("a", "https://github.com/owner/repo", "main")}
	for name, body := range map[string]string{
		"a tag":            `{"ref":"refs/tags/v1","repository":{"clone_url":"https://github.com/owner/repo.git"}}`,
		"a deleted branch": `{"ref":"refs/heads/main","deleted":true,"repository":{"clone_url":"https://github.com/owner/repo.git"}}`,
		"another branch":   `{"ref":"refs/heads/feature","repository":{"clone_url":"https://github.com/owner/repo.git"}}`,
	} {
		rec, names := deliver(t, apps, "push", body, sign(body))
		if len(names) != 0 {
			t.Errorf("%s: triggered %v", name, names)
		}
		if rec.Code >= http.StatusBadRequest {
			t.Errorf("%s: status %d, GitHub would show a failed delivery", name, rec.Code)
		}
	}
}

func TestDeliveryWithoutAValidSignatureIsRefused(t *testing.T) {
	apps := []platformatorv1alpha1.App{app("a", "https://github.com/owner/repo", "main")}
	forged := "sha256=" + strings.Repeat("0", 64)
	for name, signature := range map[string]string{"missing": "", "wrong": forged, "not hex": "sha256=zz", "no prefix": "abc"} {
		rec, names := deliver(t, apps, "push", pushToMain, signature)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
		if len(names) != 0 {
			t.Errorf("%s: triggered %v", name, names)
		}
	}
}

func TestSignatureOfAnotherBodyIsRefused(t *testing.T) {
	rec, names := deliver(t, nil, "push", pushToMain, sign(`{"ref":"refs/heads/main"}`))
	if rec.Code != http.StatusUnauthorized || len(names) != 0 {
		t.Errorf("status %d, triggered %v", rec.Code, names)
	}
}

func TestNoSecretMeansNothingValidates(t *testing.T) {
	if validSignature(nil, []byte("x"), "sha256="+hex.EncodeToString(hmac.New(sha256.New, nil).Sum(nil))) {
		t.Error("an empty secret must never validate")
	}
}

func TestPingAndOtherEvents(t *testing.T) {
	body := `{"zen":"Keep it logically awesome."}`
	if rec, _ := deliver(t, nil, "ping", body, sign(body)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Errorf("ping: status %d, body %q", rec.Code, rec.Body)
	}
	if rec, _ := deliver(t, nil, "issues", body, sign(body)); rec.Code != http.StatusAccepted {
		t.Errorf("other event: status %d", rec.Code)
	}
}

func TestOnlyPostIsAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Handler{Secret: secret}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d", rec.Code)
	}
}

func TestRepoKey(t *testing.T) {
	want := "github.com/owner/repo"
	for _, raw := range []string{
		"https://github.com/owner/repo",
		"https://github.com/Owner/Repo.git",
		"https://github.com/owner/repo/",
		"git@github.com:owner/repo.git",
		"ssh://git@github.com/owner/repo.git",
		" https://github.com/owner/repo ",
	} {
		if got := repoKey(raw); got != want {
			t.Errorf("%q: got %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{"", "github.com", "https://github.com", "git@github.com:"} {
		if got := repoKey(raw); got != "" {
			t.Errorf("%q: got %q, want nothing", raw, got)
		}
	}
}
