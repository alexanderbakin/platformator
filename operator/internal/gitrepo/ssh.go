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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshTimeout bounds a whole branch lookup over SSH: connecting, the handshake
// and reading the list of refs.
const sshTimeout = 20 * time.Second

// errNotSSHAddress is returned for a repository address that SSH cannot use.
var errNotSSHAddress = errors.New("not an SSH address: use git@host:owner/repo.git or ssh://git@host/owner/repo.git")

// SSHAuth is what it takes to reach a private repository over SSH.
type SSHAuth struct {
	// PrivateKey is a private key without a passphrase, in PEM or OpenSSH format.
	PrivateKey []byte
	// KnownHosts holds the keys the Git server may present, one host per line in
	// the known_hosts format. A server with any other key is refused.
	KnownHosts []byte
}

// Resolver finds the head of a branch: over HTTPS for a public repository, and
// over SSH, with a deploy key, for a private one.
type Resolver struct {
	HTTP HTTP
	SSH  SSH
}

// Head returns the commit the branch points to. A nil auth means a public
// repository over HTTPS.
func (r *Resolver) Head(ctx context.Context, repoURL, branch string, auth *SSHAuth) (string, error) {
	if auth == nil {
		return r.HTTP.Head(ctx, repoURL, branch)
	}
	return r.SSH.Head(ctx, repoURL, branch, auth)
}

// SSH reads a repository's refs by running git-upload-pack on the server, the
// same command "git ls-remote" runs over SSH.
type SSH struct{}

// Head returns the commit the given branch of the repository points to.
func (*SSH) Head(ctx context.Context, repoURL, branch string, auth *SSHAuth) (string, error) {
	addr, err := parseSSHURL(repoURL)
	if err != nil {
		return "", err
	}
	signer, err := ssh.ParsePrivateKey(auth.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("reading the private key (it must not have a passphrase): %w", err)
	}
	known, err := parseKnownHosts(auth.KnownHosts)
	if err != nil {
		return "", err
	}
	hostKeys := known[knownHostsName(addr.host, addr.port)]
	if len(hostKeys) == 0 {
		return "", fmt.Errorf("known_hosts has no key for %s", knownHostsName(addr.host, addr.port))
	}

	config := &ssh.ClientConfig{
		User:              addr.user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   pinnedHostKeys(addr.host, hostKeys),
		HostKeyAlgorithms: algorithmsFor(hostKeys),
		Timeout:           sshTimeout,
	}

	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	hostPort := net.JoinHostPort(addr.host, addr.port)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return "", err
	}
	// Closing the connection is what stops a handshake or a read that hangs.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	sshConn, channels, requests, err := ssh.NewClientConn(conn, hostPort, config)
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("ssh: %w", err)
	}
	client := ssh.NewClient(sshConn, channels, requests)
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh: %w", err)
	}
	defer func() { _ = session.Close() }()
	var stderr bytes.Buffer
	session.Stderr = &stderr
	stdout, err := session.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("ssh: %w", err)
	}
	if err := session.Start("git-upload-pack " + shellQuote(addr.path)); err != nil {
		return "", fmt.Errorf("ssh: %w", err)
	}

	head, err := headFromAdvertisement(io.LimitReader(stdout, maxAdvertisementBytes), branch)
	if err != nil {
		// Let the command end, so that what the server said about a missing
		// repository or a refused key is complete.
		_ = session.Wait()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w (the server said: %s)", err, msg)
		}
	}
	return head, err
}

// sshAddr is a repository address split into the parts SSH needs.
type sshAddr struct {
	user, host, port, path string
}

// parseSSHURL understands both address styles of Git over SSH:
// "git@github.com:owner/repo.git" and "ssh://git@github.com/owner/repo.git".
func parseSSHURL(raw string) (sshAddr, error) {
	addr := sshAddr{user: "git", port: "22"}
	if strings.HasPrefix(raw, "ssh://") {
		u, err := url.Parse(raw)
		if err != nil {
			return addr, fmt.Errorf("parsing the repository address: %w", err)
		}
		if u.User != nil && u.User.Username() != "" {
			addr.user = u.User.Username()
		}
		addr.host = u.Hostname()
		if port := u.Port(); port != "" {
			addr.port = port
		}
		addr.path = strings.TrimPrefix(u.Path, "/")
	} else {
		userHost, path, ok := strings.Cut(raw, ":")
		if !ok || strings.Contains(raw, "://") {
			// No colon, or another scheme such as https://, which only looks like
			// "host:path" to a parser that stops at the first colon.
			return addr, errNotSSHAddress
		}
		if user, host, hasUser := strings.Cut(userHost, "@"); hasUser {
			addr.user, addr.host = user, host
		} else {
			addr.host = userHost
		}
		addr.path = path
	}
	if addr.host == "" || addr.path == "" {
		return addr, errNotSSHAddress
	}
	return addr, nil
}

// knownHostsName is how a host appears in a known_hosts file: the bare name on
// the default port, "[name]:port" otherwise.
func knownHostsName(host, port string) string {
	if port == "22" {
		return host
	}
	return "[" + host + "]:" + port
}

// parseKnownHosts reads known_hosts text into the keys of each host name.
// Hashed host names ("|1|...", from ssh-keyscan -H) are not supported.
func parseKnownHosts(data []byte) (map[string][]ssh.PublicKey, error) {
	keys := map[string][]ssh.PublicKey{}
	for len(bytes.TrimSpace(data)) > 0 {
		_, hosts, key, _, rest, err := ssh.ParseKnownHosts(data)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading known_hosts: %w", err)
		}
		for _, host := range hosts {
			keys[host] = append(keys[host], key)
		}
		data = rest
	}
	return keys, nil
}

// pinnedHostKeys accepts a server only if it presents one of the given keys.
func pinnedHostKeys(host string, allowed []ssh.PublicKey) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		for _, want := range allowed {
			if bytes.Equal(want.Marshal(), key.Marshal()) {
				return nil
			}
		}
		return fmt.Errorf("the key of %s (%s) is not the one in known_hosts", host, ssh.FingerprintSHA256(key))
	}
}

// algorithmsFor lists the signature algorithms of the pinned keys, so that the
// server offers a key that can be compared instead of one that cannot.
func algorithmsFor(keys []ssh.PublicKey) []string {
	var algorithms []string
	for _, key := range keys {
		if key.Type() == ssh.KeyAlgoRSA {
			algorithms = append(algorithms, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
			continue
		}
		algorithms = append(algorithms, key.Type())
	}
	return algorithms
}

// shellQuote wraps a string for the remote shell that runs the command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
