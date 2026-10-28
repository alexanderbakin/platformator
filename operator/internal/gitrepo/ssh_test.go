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
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const (
	sshRepoPath = "owner/repo.git"
	gitUser     = "git"
	gitHost     = "github.com"
)

func TestParseSSHURL(t *testing.T) {
	cases := map[string]sshAddr{
		"git@github.com:owner/repo.git":              {user: gitUser, host: gitHost, port: "22", path: sshRepoPath},
		"github.com:owner/repo.git":                  {user: gitUser, host: gitHost, port: "22", path: sshRepoPath},
		"ssh://git@github.com/owner/repo.git":        {user: gitUser, host: gitHost, port: "22", path: sshRepoPath},
		"ssh://deploy@git.example.com:2222/team/app": {user: "deploy", host: "git.example.com", port: "2222", path: "team/app"},
		"ssh://git.example.com/team/app.git":         {user: gitUser, host: "git.example.com", port: "22", path: "team/app.git"},
	}
	for raw, want := range cases {
		got, err := parseSSHURL(raw)
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v, want %+v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"https://github.com/owner/repo", "github.com", "git@github.com:", "ssh:///repo.git"} {
		if _, err := parseSSHURL(raw); err == nil {
			t.Errorf("%s: want an error", raw)
		}
	}
}

func TestKnownHostsName(t *testing.T) {
	if got := knownHostsName(gitHost, "22"); got != gitHost {
		t.Errorf("got %q", got)
	}
	if got := knownHostsName("git.example.com", "2222"); got != "[git.example.com]:2222" {
		t.Errorf("got %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("owner/repo.git"); got != `'owner/repo.git'` {
		t.Errorf("got %q", got)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("a quote must not end the quoting, got %q", got)
	}
}

// newKey makes a key pair for a test server or client.
func newKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	return signer, pem.EncodeToMemory(block)
}

// sshAnswer is what a Git server sends first over SSH: the refs, with no HTTP
// headers around them.
func sshAnswer() string {
	return pktLine(mainSHA+" HEAD\x00multi_ack symref=HEAD:refs/heads/main\n") +
		pktLine(mainSHA+" refs/heads/main\n") +
		pktLine(featureSHA+" refs/heads/feature/x\n") +
		"0000"
}

// startSSHServer runs a server that accepts one client key and answers every
// command with the given text. The commands it receives are sent on the channel.
func startSSHServer(t *testing.T, host ssh.Signer, client ssh.PublicKey, answer string) (string, <-chan string) {
	t.Helper()
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), client.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	config.AddHostKey(host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	commands := make(chan string, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveSSH(conn, config, answer, commands)
		}
	}()
	return listener.Addr().String(), commands
}

func serveSSH(conn net.Conn, config *ssh.ServerConfig, answer string, commands chan<- string) {
	defer func() { _ = conn.Close() }()
	_, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		channel, reqs, err := newChannel.Accept()
		if err != nil {
			return
		}
		for req := range reqs {
			if req.Type != "exec" {
				_ = req.Reply(false, nil)
				continue
			}
			var payload struct{ Command string }
			_ = ssh.Unmarshal(req.Payload, &payload)
			_ = req.Reply(true, nil)
			commands <- payload.Command
			_, _ = channel.Write([]byte(answer))
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			_ = channel.Close()
		}
	}
}

// knownHostsFor is the known_hosts line of a test server.
func knownHostsFor(addr string, key ssh.PublicKey) []byte {
	_, port, _ := net.SplitHostPort(addr)
	return []byte("[127.0.0.1]:" + port + " " + string(ssh.MarshalAuthorizedKey(key)))
}

func TestSSHHeadFindsTheBranch(t *testing.T) {
	host, _ := newKey(t)
	client, clientPEM := newKey(t)
	addr, commands := startSSHServer(t, host, client.PublicKey(), sshAnswer())
	auth := &SSHAuth{PrivateKey: clientPEM, KnownHosts: knownHostsFor(addr, host.PublicKey())}
	_, port, _ := net.SplitHostPort(addr)
	repo := "ssh://git@127.0.0.1:" + port + "/" + sshRepoPath

	for branch, want := range map[string]string{mainBranch: mainSHA, "feature/x": featureSHA} {
		got, err := (&SSH{}).Head(context.Background(), repo, branch, auth)
		if err != nil {
			t.Fatalf("branch %s: %v", branch, err)
		}
		if got != want {
			t.Errorf("branch %s: got %s, want %s", branch, got, want)
		}
	}
	if got := <-commands; got != "git-upload-pack '"+sshRepoPath+"'" {
		t.Errorf("the server ran %q", got)
	}
}

func TestSSHHeadReportsAMissingBranch(t *testing.T) {
	host, _ := newKey(t)
	client, clientPEM := newKey(t)
	addr, _ := startSSHServer(t, host, client.PublicKey(), sshAnswer())
	auth := &SSHAuth{PrivateKey: clientPEM, KnownHosts: knownHostsFor(addr, host.PublicKey())}
	_, port, _ := net.SplitHostPort(addr)

	_, err := (&SSH{}).Head(context.Background(), "ssh://git@127.0.0.1:"+port+"/"+sshRepoPath, "nope", auth)
	if err == nil || !strings.Contains(err.Error(), `branch "nope" not found`) {
		t.Fatalf("got %v", err)
	}
}

func TestSSHHeadRefusesAServerWithAnotherKey(t *testing.T) {
	host, _ := newKey(t)
	impostor, _ := newKey(t)
	client, clientPEM := newKey(t)
	addr, commands := startSSHServer(t, host, client.PublicKey(), sshAnswer())
	// known_hosts holds a different key than the server presents.
	auth := &SSHAuth{PrivateKey: clientPEM, KnownHosts: knownHostsFor(addr, impostor.PublicKey())}
	_, port, _ := net.SplitHostPort(addr)

	_, err := (&SSH{}).Head(context.Background(), "ssh://git@127.0.0.1:"+port+"/"+sshRepoPath, mainBranch, auth)
	if err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("want a host key error, got %v", err)
	}
	if len(commands) != 0 {
		t.Error("no command may run on a server that is not trusted")
	}
}

func TestSSHHeadRefusesAHostMissingFromKnownHosts(t *testing.T) {
	host, _ := newKey(t)
	client, clientPEM := newKey(t)
	addr, _ := startSSHServer(t, host, client.PublicKey(), sshAnswer())
	_, port, _ := net.SplitHostPort(addr)
	auth := &SSHAuth{PrivateKey: clientPEM, KnownHosts: []byte("github.com " + string(ssh.MarshalAuthorizedKey(host.PublicKey())))}

	_, err := (&SSH{}).Head(context.Background(), "ssh://git@127.0.0.1:"+port+"/"+sshRepoPath, mainBranch, auth)
	if err == nil || !strings.Contains(err.Error(), "known_hosts has no key") {
		t.Fatalf("got %v", err)
	}
}

func TestSSHHeadRefusesAnUnknownClientKey(t *testing.T) {
	host, _ := newKey(t)
	allowed, _ := newKey(t)
	_, strangerPEM := newKey(t)
	addr, _ := startSSHServer(t, host, allowed.PublicKey(), sshAnswer())
	_, port, _ := net.SplitHostPort(addr)
	auth := &SSHAuth{PrivateKey: strangerPEM, KnownHosts: knownHostsFor(addr, host.PublicKey())}

	if _, err := (&SSH{}).Head(context.Background(), "ssh://git@127.0.0.1:"+port+"/"+sshRepoPath, mainBranch, auth); err == nil {
		t.Fatal("a key the server does not know must be refused")
	}
}

func TestResolverUsesHTTPWithoutAKey(t *testing.T) {
	server := serve(t, 200, advertisement())

	got, err := (&Resolver{}).Head(context.Background(), server.URL+repoPath, mainBranch, nil)
	if err != nil || got != mainSHA {
		t.Fatalf("got %q, %v", got, err)
	}
}
