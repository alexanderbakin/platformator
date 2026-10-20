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

package gitrepo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	mainSHA    = "1111111111111111111111111111111111111111"
	featureSHA = "2222222222222222222222222222222222222222"

	repoPath   = "/owner/repo"
	mainBranch = "main"
)

// pktLine encodes one line the way Git's HTTP protocol does.
func pktLine(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

// advertisement builds a server answer listing a main and a feature branch.
func advertisement() string {
	return pktLine("# service=git-upload-pack\n") + "0000" +
		pktLine(mainSHA+" HEAD\x00multi_ack side-band-64k symref=HEAD:refs/heads/main\n") +
		pktLine(mainSHA+" refs/heads/main\n") +
		pktLine(featureSHA+" refs/heads/feature/x\n") +
		pktLine(featureSHA+" refs/tags/v1\n") +
		"0000"
}

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != repoPath+"/info/refs" || r.URL.Query().Get("service") != "git-upload-pack" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestHeadFindsTheBranch(t *testing.T) {
	server := serve(t, http.StatusOK, advertisement())
	repo := &HTTP{}

	for branch, want := range map[string]string{mainBranch: mainSHA, "feature/x": featureSHA} {
		got, err := repo.Head(context.Background(), server.URL+repoPath, branch)
		if err != nil {
			t.Fatalf("branch %s: %v", branch, err)
		}
		if got != want {
			t.Errorf("branch %s: got %s, want %s", branch, got, want)
		}
	}
}

func TestHeadAcceptsATrailingSlash(t *testing.T) {
	server := serve(t, http.StatusOK, advertisement())

	got, err := (&HTTP{}).Head(context.Background(), server.URL+repoPath+"/", mainBranch)
	if err != nil || got != mainSHA {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestHeadReportsAMissingBranch(t *testing.T) {
	server := serve(t, http.StatusOK, advertisement())

	_, err := (&HTTP{}).Head(context.Background(), server.URL+repoPath, "nope")
	if err == nil || !strings.Contains(err.Error(), `branch "nope" not found`) {
		t.Fatalf("got %v", err)
	}
}

func TestHeadDoesNotMatchTagsOrPrefixes(t *testing.T) {
	server := serve(t, http.StatusOK, advertisement())

	// "v1" is a tag, and "feature" is only a prefix of "feature/x".
	for _, branch := range []string{"v1", "feature"} {
		if _, err := (&HTTP{}).Head(context.Background(), server.URL+repoPath, branch); err == nil {
			t.Errorf("branch %s: expected an error", branch)
		}
	}
}

func TestHeadReportsAServerError(t *testing.T) {
	server := serve(t, http.StatusNotFound, "not found")

	_, err := (&HTTP{}).Head(context.Background(), server.URL+repoPath, mainBranch)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v", err)
	}
}

func TestHeadRejectsAnAnswerThatIsNotGit(t *testing.T) {
	server := serve(t, http.StatusOK, "<html>hello</html>")

	_, err := (&HTTP{}).Head(context.Background(), server.URL+repoPath, mainBranch)
	if err == nil || !strings.Contains(err.Error(), "unexpected answer") {
		t.Fatalf("got %v", err)
	}
}
