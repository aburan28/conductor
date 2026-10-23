package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthyRequiresExactlyOK(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent, http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %s, want GET", r.Method)
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			if got := healthy(newProbeClient(time.Second), server.URL); got != (status == http.StatusOK) {
				t.Errorf("healthy status %d = %v", status, got)
			}
		})
	}
}

func TestHealthyDoesNotFollowRedirect(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()
	if healthy(newProbeClient(time.Second), server.URL) {
		t.Error("redirect was accepted as healthy")
	}
	if followed.Load() {
		t.Error("probe followed the redirect")
	}
}

func TestHealthyTimesOutWaitingForHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	started := time.Now()
	if healthy(newProbeClient(50*time.Millisecond), server.URL) {
		t.Error("stalled response was accepted as healthy")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("probe did not enforce its deadline: %s", elapsed)
	}
}

func TestHealthyDoesNotWaitForBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Promise a huge body and send none: waiting for it would exhaust the timeout.
		w.Header().Set("Content-Length", "1099511627776")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	started := time.Now()
	if !healthy(newProbeClient(2*time.Second), server.URL) {
		t.Error("healthy headers were rejected because the body was incomplete")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("probe waited for the body: %s", elapsed)
	}
}

func TestProbeClientIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	client := newProbeClient(probeTimeout)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("probe transport can consult proxy environment settings")
	}
	if client.Timeout != 4*time.Second {
		t.Errorf("total timeout = %s, want 4s", client.Timeout)
	}
}

func TestHealthyRejectsConnectionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()
	if healthy(newProbeClient(time.Second), server.URL) {
		t.Error("unreachable process was accepted as healthy")
	}
}
