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

// Package gitrepo finds out what a remote Git repository currently contains,
// without cloning it and without a git binary.
package gitrepo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxAdvertisementBytes caps how much of a server's answer is read. The answer
// lists every branch and tag, so a huge repository could otherwise be slow.
const maxAdvertisementBytes = 16 << 20

// HTTP reads a repository's refs over Git's smart HTTP protocol, the same
// request "git ls-remote" makes. It works for public repositories.
type HTTP struct {
	// Client is used for the request. A client with a 20 second timeout is
	// used when it is nil.
	Client *http.Client
}

// Head returns the commit the given branch of the repository points to.
func (h *HTTP) Head(ctx context.Context, repoURL, branch string) (string, error) {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	endpoint := strings.TrimSuffix(repoURL, "/") + "/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("building the request: %w", err)
	}
	// Some servers answer differently to clients they do not recognise as Git.
	req.Header.Set("User-Agent", "git/2.45.0 (platformator)")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the server answered %s (is the repository public and the URL right?)", resp.Status)
	}

	return headFromAdvertisement(io.LimitReader(resp.Body, maxAdvertisementBytes), branch)
}

// headFromAdvertisement finds a branch in the server's list of refs. The list
// is a sequence of "pkt-lines": four hex digits with the length of the line
// including those four digits, then the data. "0000" is a separator. Ref lines
// look like "<sha> refs/heads/main", and the first one also carries the
// server's capabilities after a NUL byte.
func headFromAdvertisement(r io.Reader, branch string) (string, error) {
	reader := bufio.NewReader(r)
	want := "refs/heads/" + branch

	for {
		length := make([]byte, 4)
		if _, err := io.ReadFull(reader, length); err != nil {
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("branch %q not found", branch)
			}
			return "", fmt.Errorf("reading the answer: %w", err)
		}
		n, err := strconv.ParseUint(string(length), 16, 16)
		if err != nil {
			return "", fmt.Errorf("unexpected answer from the server (not a Git repository URL?)")
		}
		if n == 0 {
			continue // separator
		}
		if n < 4 {
			return "", fmt.Errorf("unexpected answer from the server (bad line length %d)", n)
		}
		data := make([]byte, n-4)
		if _, err := io.ReadFull(reader, data); err != nil {
			return "", fmt.Errorf("reading the answer: %w", err)
		}

		line := strings.TrimSuffix(string(data), "\n")
		if strings.HasPrefix(line, "#") {
			continue // "# service=git-upload-pack"
		}
		line, _, _ = strings.Cut(line, "\x00")
		sha, name, ok := strings.Cut(line, " ")
		if ok && name == want {
			return sha, nil
		}
	}
}
