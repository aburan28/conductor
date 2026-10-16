package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Publishing is verified by construction: the exact argument vectors, the backend URL
// derivation, and the post-run serve status check are pure; the tailscale daemon is not
// stubbed here.

func TestLocateTailscaleCLIPathFirst(t *testing.T) {
	got, err := locateTailscaleCLI(
		func(string) (string, error) { return "/opt/homebrew/bin/tailscale", nil },
		func(string) (os.FileInfo, error) { return nil, errors.New("unused") })
	if err != nil || got != "/opt/homebrew/bin/tailscale" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestLocateTailscaleCLIMacAppBundle(t *testing.T) {
	got, err := locateTailscaleCLI(
		func(string) (string, error) { return "", errors.New("not found") },
		func(p string) (os.FileInfo, error) {
			if p == "/Applications/Tailscale.app/Contents/MacOS/Tailscale" {
				return fakeFileInfo{}, nil
			}
			return nil, errors.New("not found")
		})
	if err != nil || !strings.HasSuffix(got, "Tailscale.app/Contents/MacOS/Tailscale") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestLocateTailscaleCLIMissing(t *testing.T) {
	_, err := locateTailscaleCLI(
		func(string) (string, error) { return "", errors.New("not found") },
		func(string) (os.FileInfo, error) { return nil, errors.New("not found") })
	if err == nil {
		t.Fatal("expected an error when tailscale is nowhere")
	}
}

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "Tailscale" }
func (fakeFileInfo) Size() int64        { return 1 }
func (fakeFileInfo) Mode() os.FileMode  { return 0o755 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

func TestPublishBackend(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"loopback tls", "https://127.0.0.1:8443", "https+insecure://127.0.0.1:8443"},
		{"loopback http", "http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"localhost alias", "https://localhost:8443", "https+insecure://127.0.0.1:8443"},
		{"no port", "https://127.0.0.1", defaultPublishBackend},
		{"garbage", "://", defaultPublishBackend},
		{"remote endpoint falls back", "https://conductor.team", defaultPublishBackend},
		{"empty", "", defaultPublishBackend},
	}
	for _, tc := range cases {
		if got := publishBackend(tc.endpoint); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPublishCommandsUseFullURLBackends(t *testing.T) {
	backend := "https+insecure://127.0.0.1:8443"
	cmds := publishCommands(false, backend)
	if len(cmds) != 1 {
		t.Fatalf("serve only: got %d commands", len(cmds))
	}
	if got := strings.Join(cmds[0], " "); got != "serve --bg --https=443 "+backend {
		t.Errorf("serve command: %q", got)
	}
	cmds = publishCommands(true, backend)
	if len(cmds) != 2 {
		t.Fatalf("serve+funnel: got %d commands", len(cmds))
	}
	if got := strings.Join(cmds[1], " "); got != "funnel --bg "+backend {
		t.Errorf("funnel command: %q", got)
	}
	// The footgun: a bare-port funnel target would re-point the public route at plain
	// http, so every backend argument must be a URL.
	for _, args := range cmds {
		last := args[len(args)-1]
		if !strings.Contains(last, "://") {
			t.Errorf("backend %q is not a URL form", last)
		}
	}
}

func TestServeRootMatches(t *testing.T) {
	const hostPort = "mac.tailnet.ts.net:443"
	const backend = "https+insecure://127.0.0.1:8443"
	serve := `{"Web":{"mac.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"https+insecure://127.0.0.1:8443/"}}}}}`
	if rootOK, funnel := serveRootMatches([]byte(serve), hostPort, backend); !rootOK || funnel {
		t.Fatalf("rootOK=%v funnel=%v, want true false", rootOK, funnel)
	}
	withFunnel := `{"Web":{"mac.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"https+insecure://127.0.0.1:8443"}}}},"AllowFunnel":{"mac.tailnet.ts.net:443":true}}`
	if rootOK, funnel := serveRootMatches([]byte(withFunnel), hostPort, backend); !rootOK || !funnel {
		t.Fatalf("rootOK=%v funnel=%v, want true true", rootOK, funnel)
	}
	// A clobbered root route (e.g. by a re-run funnel command) must be detected.
	clobbered := `{"Web":{"mac.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8443"}}}}}`
	if rootOK, _ := serveRootMatches([]byte(clobbered), hostPort, backend); rootOK {
		t.Fatal("a plain-http root route should not match the TLS backend")
	}
	if rootOK, _ := serveRootMatches([]byte(`{}`), hostPort, backend); rootOK {
		t.Fatal("an empty serve status should not match")
	}
	if rootOK, _ := serveRootMatches([]byte(`not json`), hostPort, backend); rootOK {
		t.Fatal("unparsable serve status should not match")
	}
}
