package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoRetries429ThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":"slow down","code":"rate_limited"}`, http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "yes"})
	}))
	defer srv.Close()

	var out map[string]string
	err := New(srv.URL, "tok").Get(context.Background(), "/v1/x", &out)
	if err != nil {
		t.Fatalf("after retries: %v", err)
	}
	if out["ok"] != "yes" {
		t.Fatalf("out = %v", out)
	}
	if n := hits.Load(); n != 3 {
		t.Fatalf("hits = %d, want 3", n)
	}
}

func TestDoDoesNotRetry404(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":"not found","code":"not_found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	err := New(srv.URL, "tok").Get(context.Background(), "/v1/x", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want 404 APIError", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want 1 (no retry on 404)", n)
	}
}

func TestDoHonoursRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	start := time.Now()
	err := New(srv.URL, "").Get(context.Background(), "/v1/x", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("elapsed %v, expected to wait for Retry-After", elapsed)
	}
}

func TestAPIErrorTransient(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   bool
	}{
		{429, "rate_limited", true},
		{503, "", true},
		{502, "", true},
		{504, "", true},
		{408, "", true},
		{404, "not_found", false},
		{401, "unauthenticated", false},
		{409, "scope_conflict", false},
		{500, "internal", true},
		{400, "bad_request", false},
	}
	for _, c := range cases {
		e := &APIError{Status: c.status, Code: c.code}
		if got := e.Transient(); got != c.want {
			t.Errorf("%d/%s Transient() = %v, want %v", c.status, c.code, got, c.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("2"); !ok || d != 2*time.Second {
		t.Errorf("seconds: got %v %v", d, ok)
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Error("empty should fail")
	}
	if _, ok := parseRetryAfter("nope"); ok {
		t.Error("garbage should fail")
	}
}
