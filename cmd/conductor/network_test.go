package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNetworkVerdict(t *testing.T) {
	cases := []struct {
		name                     string
		loopback, tls, reachable bool
		want                     string
		wantAdvice               bool
	}{
		{"loopback beats everything", true, true, true, "local_only", true},
		{"unreachable non-loopback", false, true, false, "unreachable", true},
		{"reachable plaintext", false, false, true, "reachable_plaintext", true},
		{"reachable tls", false, true, true, "reachable_tls", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, summary, advice := networkVerdict(c.loopback, c.tls, c.reachable)
			if v != c.want {
				t.Errorf("verdict = %q, want %q", v, c.want)
			}
			if summary == "" {
				t.Error("summary is empty")
			}
			if c.wantAdvice != (len(advice) > 0) {
				t.Errorf("advice presence = %v, want %v", len(advice) > 0, c.wantAdvice)
			}
		})
	}
}

// The live probe hits the unauthenticated /v1/health and reports reachability; against an
// httptest server (127.0.0.1) the verdict is local_only, but Reachable must be true.
func TestDiagnoseNetworkingProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	d := diagnoseNetworking(context.Background(), srv.URL)
	if !d.Reachable {
		t.Fatalf("expected the health probe to succeed against %s: %s", srv.URL, d.Error)
	}
	if !d.Loopback || d.Verdict != "local_only" {
		t.Errorf("httptest is 127.0.0.1 so it should read local_only, got loopback=%v verdict=%q", d.Loopback, d.Verdict)
	}

	// An unconfigured endpoint is a clean "unknown", not a crash.
	if u := diagnoseNetworking(context.Background(), ""); u.Verdict != "unknown" {
		t.Errorf("empty endpoint verdict = %q, want unknown", u.Verdict)
	}
	// An unreachable non-loopback endpoint reports unreachable.
	down := diagnoseNetworking(context.Background(), "http://198.51.100.7:8080") // TEST-NET-2, unroutable
	if down.Loopback {
		t.Fatal("TEST-NET address should not be loopback")
	}
	if down.Reachable {
		t.Fatal("TEST-NET address should not be reachable")
	}
	if down.Verdict != "unreachable" {
		t.Errorf("verdict = %q, want unreachable", down.Verdict)
	}
}
