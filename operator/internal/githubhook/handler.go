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

// Package githubhook receives GitHub push webhooks and asks for an immediate
// look at the Apps that build from the pushed branch. Polling still runs, so a
// lost delivery only costs time.
package githubhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

const (
	// Path is where GitHub is told to send events.
	Path = "/hooks/github"

	// maxBodyBytes caps a delivery. A push event is a few kilobytes, because
	// GitHub lists at most a handful of commits in it.
	maxBodyBytes = 1 << 20

	signatureHeader = "X-Hub-Signature-256"
	eventHeader     = "X-GitHub-Event"
	branchPrefix    = "refs/heads/"
	defaultBranch   = "main"
)

var log = logf.Log.WithName("github-webhook")

// Handler answers GitHub deliveries. A delivery is accepted only if it is signed
// with the shared secret, and a push to a branch that an App builds sends that
// App on Events.
type Handler struct {
	// Secret is the shared secret configured on the GitHub webhook.
	Secret []byte
	// Reader lists the Apps.
	Reader client.Reader
	// Events receives the Apps to look at again.
	Events chan<- event.GenericEvent
}

// pushEvent holds the parts of GitHub's push payload that matter here.
type pushEvent struct {
	Ref        string `json:"ref"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		CloneURL string `json:"clone_url"`
		SSHURL   string `json:"ssh_url"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
}

// ServeHTTP checks the signature first, and reads nothing from the payload
// before it is valid.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "cannot read the body, is it too large?", http.StatusRequestEntityTooLarge)
		return
	}
	if !validSignature(h.Secret, body, r.Header.Get(signatureHeader)) {
		log.Info("Rejected a delivery with a missing or wrong signature", "remote", r.RemoteAddr)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	switch r.Header.Get(eventHeader) {
	case "ping":
		_, _ = fmt.Fprintln(w, "pong")
	case "push":
		h.push(w, r, body)
	default:
		// Other events are not subscribed to; acknowledge so GitHub shows no failure.
		w.WriteHeader(http.StatusAccepted)
	}
}

func (h *Handler) push(w http.ResponseWriter, r *http.Request, body []byte) {
	var push pushEvent
	if err := json.Unmarshal(body, &push); err != nil {
		http.Error(w, "unreadable payload", http.StatusBadRequest)
		return
	}
	branch, isBranch := strings.CutPrefix(push.Ref, branchPrefix)
	if !isBranch || push.Deleted {
		// A tag, or a branch that was removed: nothing to build.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var apps platformatorv1alpha1.AppList
	if err := h.Reader.List(r.Context(), &apps); err != nil {
		log.Error(err, "Cannot list Apps for a push event")
		http.Error(w, "cannot list Apps", http.StatusInternalServerError)
		return
	}
	repos := []string{
		repoKey(push.Repository.CloneURL), repoKey(push.Repository.SSHURL), repoKey(push.Repository.HTMLURL),
	}
	triggered := 0
	for i := range apps.Items {
		app := &apps.Items[i]
		if !builds(app, repos, branch) {
			continue
		}
		select {
		case h.Events <- event.GenericEvent{Object: app}:
			triggered++
		case <-r.Context().Done():
			http.Error(w, "cancelled", http.StatusServiceUnavailable)
			return
		}
	}
	log.Info("Handled a push event", "repository", repos[0], "branch", branch, "apps", triggered)
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, "{\"triggered\":%d}\n", triggered)
}

// builds says whether an App builds the given branch of one of the repositories.
func builds(app *platformatorv1alpha1.App, repos []string, branch string) bool {
	if app.Spec.Source == nil {
		return false
	}
	appBranch := app.Spec.Source.Git.Branch
	if appBranch == "" {
		appBranch = defaultBranch
	}
	if appBranch != branch {
		return false
	}
	want := repoKey(app.Spec.Source.Git.URL)
	if want == "" {
		return false
	}
	return slices.Contains(repos, want)
}

// validSignature checks GitHub's HMAC-SHA256 of the body. An empty secret never
// validates, so a missing configuration cannot let everything in.
func validSignature(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	hexSum, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// repoKey reduces the address of a repository, in any of the forms Git uses, to
// "host/owner/name" in lower case, so that the https and ssh addresses of the
// same repository compare equal. It returns "" for an address it cannot read.
func repoKey(raw string) string {
	s := strings.TrimSpace(raw)
	var host, path string
	if _, rest, ok := strings.Cut(s, "://"); ok {
		host, path, _ = strings.Cut(rest, "/")
	} else {
		host, path, _ = strings.Cut(s, ":")
	}
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host + "/" + path)
}

// Server runs a Handler on an address. It is a controller-runtime Runnable, so
// the manager starts and stops it with the controllers.
type Server struct {
	Addr    string
	Handler http.Handler
}

// Start serves until ctx ends.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle(Path, s.Handler)
	server := &http.Server{
		Addr:              s.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	log.Info("Receiving GitHub webhooks", "address", s.Addr, "path", Path)

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	case err := <-failed:
		return err
	}
}
