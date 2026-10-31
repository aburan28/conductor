package compat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aburan28/conductor/internal/delphi"
	"github.com/aburan28/conductor/internal/delphi/compat"
)

// server answers /chat/completions by the request's model: each model name is one scenario.
func server(t *testing.T, seen *[]map[string]any) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "wrong path or key", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if seen != nil {
			mu.Lock()
			*seen = append(*seen, body)
			mu.Unlock()
		}
		switch body["model"] {
		case "ok":
			io.WriteString(w, `{"model":"ok-2026","choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":11,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":5}}}`)
		case "parts":
			io.WriteString(w, `{"choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]},"finish_reason":"stop"}]}`)
		case "long":
			io.WriteString(w, `{"choices":[{"message":{"content":"cut"},"finish_reason":"length"}]}`)
		case "refuse":
			io.WriteString(w, `{"choices":[{"message":{"content":null,"refusal":"I can't help with that."},"finish_reason":"stop"}]}`)
		case "busy":
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit"}}`)
		case "bad":
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"unknown model","type":"invalid_request_error"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func provider(t *testing.T, base string) *compat.Provider {
	t.Helper()
	t.Setenv("TEST_COMPAT_KEY", "k")
	p, err := compat.New(compat.Endpoint{Name: "fw", BaseURL: base + "/v1", KeyEnv: "TEST_COMPAT_KEY", Concurrency: 2}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSubmitAnswersLiveAndSpoolsEveryOutcome(t *testing.T) {
	var seen []map[string]any
	srv := server(t, &seen)
	p := provider(t, srv.URL)
	ctx := context.Background()
	reqs := []delphi.Request{}
	for _, m := range []string{"ok", "parts", "long", "refuse", "busy", "bad"} {
		reqs = append(reqs, delphi.Request{ID: m, Model: m, System: "sys", MaxTokens: 100,
			Messages: []delphi.Message{{Role: "user", Content: "hi"}}})
	}
	reqs[0].Effort = "high"
	if _, found, _ := p.Find(ctx, "L1"); found {
		t.Fatal("Find reported a flight that was never submitted")
	}
	id, err := p.Submit(ctx, reqs, "L1")
	if err != nil || id != "L1" {
		t.Fatalf("Submit = %q, %v", id, err)
	}
	if got, found, err := p.Find(ctx, "L1"); err != nil || !found || got != "L1" {
		t.Fatalf("Find after Submit = %q %v %v", got, found, err)
	}
	st, err := p.Poll(ctx, id)
	if err != nil || !st.Ended || st.Counts["succeeded"] != 3 || st.Counts["refused"] != 1 || st.Counts["errored"] != 2 {
		t.Fatalf("Poll = %+v, %v", st, err)
	}
	got := map[string]delphi.Result{}
	if err := p.Results(ctx, id, func(r delphi.Result) error { got[r.ID] = r; return nil }); err != nil {
		t.Fatal(err)
	}
	if r := got["ok"]; r.Text != "hello" || r.Model != "ok-2026" || r.Usage == nil ||
		*r.Usage != (delphi.Usage{InputTokens: 11, OutputTokens: 3, CacheReadTokens: 5}) {
		t.Errorf("ok = %+v usage %+v", r, r.Usage)
	}
	if r := got["parts"]; r.Status != delphi.Succeeded || r.Text != "ab" {
		t.Errorf("parts = %+v", r)
	}
	if r := got["long"]; !r.Truncated || r.Text != "cut" {
		t.Errorf("long = %+v", r)
	}
	if r := got["refuse"]; r.Status != delphi.Refused || r.Refusal == nil || r.Refusal.Explanation != "I can't help with that." {
		t.Errorf("refuse = %+v", r)
	}
	if r := got["busy"]; r.Status != delphi.Errored || !r.Retryable() || r.Error.Message != "rate limited" {
		t.Errorf("busy = %+v err %+v", r, r.Error)
	}
	if r := got["bad"]; r.Status != delphi.Errored || r.Retryable() || r.Error.Type != "http_400" {
		t.Errorf("bad = %+v err %+v", r, r.Error)
	}
	// The wire body: system first, effort as reasoning_effort, max_tokens carried.
	for _, b := range seen {
		msgs := b["messages"].([]any)
		if first := msgs[0].(map[string]any); first["role"] != "system" || first["content"] != "sys" || b["max_tokens"] != float64(100) {
			t.Errorf("body = %v", b)
		}
		if _, has := b["reasoning_effort"]; has != (b["model"] == "ok") {
			t.Errorf("reasoning_effort sent = %v for %v", has, b["model"])
		}
	}
}

func TestUnreachableEndpointIsARetryableResultNotASubmitError(t *testing.T) {
	srv := server(t, nil)
	base := srv.URL
	srv.Close()
	p := provider(t, base)
	if _, err := p.Submit(context.Background(), []delphi.Request{{ID: "x", Model: "ok", MaxTokens: 1}}, "L2"); err != nil {
		t.Fatal(err)
	}
	_ = p.Results(context.Background(), "L2", func(r delphi.Result) error {
		if r.Status != delphi.Errored || !r.Retryable() || r.Error.Type != "network" {
			t.Errorf("result = %+v", r)
		}
		return nil
	})
}

func TestEndpointsMergeFileOverPresets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "endpoints.json")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	eps, err := compat.Endpoints(path) // no file: presets only
	if err != nil || eps["fireworks"].KeyEnv != "FIREWORKS_API_KEY" || !eps["vllm"].Local || eps["vllm"].Listed {
		t.Fatalf("presets = %+v, %v", eps, err)
	}
	if o := eps["opencode"]; o.KeyEnv != "OPENCODE_API_KEY" || o.BaseURL != "https://opencode.ai/inference/v1" || o.Local {
		t.Errorf("opencode preset = %+v", o)
	}
	if _, ok := eps["openai"]; ok {
		t.Fatal("the native openai provider must not appear as a compatible endpoint")
	}
	write(`{"deepseek":{"base_url":"https://api.deepseek.com/v1","key_env":"DEEPSEEK_API_KEY","prices":{"deepseek-chat":[0.27,1.1]}},
		"vllm":{"base_url":"http://127.0.0.1:8000/v1","default_model":"glm-5.3"},
		"fireworks":{"concurrency":16}}`)
	eps, err = compat.Endpoints(path)
	if err != nil {
		t.Fatal(err)
	}
	if d := eps["deepseek"]; !d.Listed || d.Local || d.Prices["deepseek-chat"] != [2]float64{0.27, 1.1} {
		t.Errorf("deepseek = %+v", d)
	}
	if v := eps["vllm"]; !v.Listed || !v.Local || v.DefaultModel != "glm-5.3" {
		t.Errorf("vllm = %+v", v)
	}
	if f := eps["fireworks"]; f.Concurrency != 16 || !strings.Contains(f.BaseURL, "fireworks.ai") || f.KeyEnv != "FIREWORKS_API_KEY" {
		t.Errorf("fireworks kept its preset fields? %+v", f)
	}
	for _, bad := range []string{`{"openai":{"base_url":"https://x.example/v1"}}`, `{"nobase":{}}`} {
		write(bad)
		if _, err := compat.Endpoints(path); err == nil {
			t.Errorf("Endpoints accepted %s", bad)
		}
	}
}

func TestNewSaysWhatWouldMakeAnEndpointLive(t *testing.T) {
	t.Setenv("ABSENT_KEY", "")
	if _, err := compat.New(compat.Endpoint{Name: "fw", BaseURL: "https://x.example/v1", KeyEnv: "ABSENT_KEY"}, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "export ABSENT_KEY") {
		t.Errorf("missing key: %v", err)
	}
	if _, err := compat.New(compat.Endpoint{Name: "vllm", BaseURL: "http://127.0.0.1:8000/v1", Local: true}, t.TempDir()); err == nil {
		t.Error("an unlisted local endpoint went live")
	}
	if _, err := compat.New(compat.Endpoint{Name: "vllm", BaseURL: "http://127.0.0.1:8000/v1", Local: true, Listed: true}, t.TempDir()); err != nil {
		t.Errorf("listed local endpoint: %v", err)
	}
}

func TestFreeModelsAndTheirPrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "endpoints.json")
	if err := os.WriteFile(path, []byte(`{"vllm":{"base_url":"http://127.0.0.1:8000/v1"},
		"donor":{"base_url":"https://donor.example/v1","key_env":"DONOR_KEY","free":true},
		"mixed":{"base_url":"https://mixed.example/v1","key_env":"MIXED_KEY","free_models":["small"],"prices":{"big":[1,2]}},
		"colab":{"base_url":"https://gpu.example/v1","key_env":"COLAB_KEY","self_hosted":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	eps, err := compat.Endpoints(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"OPENROUTER_API_KEY", "DONOR_KEY", "MIXED_KEY", "COLAB_KEY"} {
		t.Setenv(k, "k")
	}
	dir := t.TempDir()
	get := func(name string) *compat.Provider {
		p, err := compat.New(eps[name], dir)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, c := range []struct {
		endpoint, model string
		free, local     bool
	}{
		{"vllm", "anything", true, true},                           // an endpoint you run
		{"openrouter", "poolside/laguna-xs-2.1:free", true, false}, // OpenRouter's ":free" ids
		{"openrouter", "poolside/laguna-xs-2.1", false, false},
		{"donor", "any", true, false}, // "free": true
		{"mixed", "small", true, false},
		{"mixed", "big", false, false},
		{"colab", "any", true, true}, // self_hosted: yours behind a key
	} {
		if free, local := get(c.endpoint).Free(c.model); free != c.free || local != c.local {
			t.Errorf("%s:%s free=%v local=%v, want %v %v", c.endpoint, c.model, free, local, c.free, c.local)
		}
	}
	if in, out, ok := get("mixed").Price("small"); !ok || in != 0 || out != 0 {
		t.Errorf("a free model is priced %v/%v (%v)", in, out, ok)
	}
	if in, _, ok := get("mixed").Price("big"); !ok || in != 1 {
		t.Errorf("a paid model lost its price: %v %v", in, ok)
	}
	if _, _, ok := get("openrouter").Price("poolside/laguna-xs-2.1"); ok {
		t.Error("a paid model with no configured price must stay unpriced, not free")
	}
}
