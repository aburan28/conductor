package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// localModel is an OpenAI-compatible endpoint you run, answering with the model it served.
func localModel(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		models = append(models, body["model"].(string))
		mu.Unlock()
		io.WriteString(w, `{"model":"`+body["model"].(string)+`","choices":[{"message":{"content":"free answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &models
}

func TestFreeModeAnswersAPaidRequestWithItsFreeSubstitute(t *testing.T) {
	fa, _ := servers(t)
	local, served := localModel(t)
	home := os.Getenv("DELPHI_HOME")
	os.WriteFile(filepath.Join(home, "endpoints.json"), []byte(`{"vllm":{"base_url":"`+local.URL+`/v1"}}`), 0o600)
	os.WriteFile(filepath.Join(home, "free.json"), []byte(`{"substitutes":{"anthropic:*":"vllm:qwen3.8-27b"}}`), 0o600)
	t.Setenv("DELPHI_FREE", "local")

	out := filepath.Join(t.TempDir(), "run")
	code, stdout, stderr := cli(t, "", "run", "-provider", "anthropic", "-model", "claude-sonnet-5-5", "-effort", "high",
		"-prompt", "hello", "-out", out, "-every", "1ms")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "free mode (local)") {
		t.Errorf("free mode was not announced: %s", stderr)
	}
	rows := lines(t, filepath.Join(out, "results.jsonl"))
	if len(rows) != 1 || rows[0]["provider"] != "vllm" || rows[0]["text"] != "free answer" ||
		rows[0]["meta"].(map[string]any)["free_substitute_for"] != "anthropic:claude-sonnet-5-5@high" {
		t.Fatalf("result = %v", rows)
	}
	if len(*served) != 1 || (*served)[0] != "qwen3.8-27b" {
		t.Errorf("local endpoint served %v", *served)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.bodies) != 0 {
		t.Errorf("free mode sent %d request(s) to the paid API", len(fa.bodies))
	}
	var summary map[string]any
	b, _ := os.ReadFile(filepath.Join(out, "summary.json"))
	json.Unmarshal(b, &summary)
	if models := summary["models"].([]any); models[0].(map[string]any)["estimated_cost_usd"] != 0.0 {
		t.Errorf("free run summary: %v", summary["models"])
	}
}

func TestFreeModeRefusesAPaidRequestWithNoSubstitute(t *testing.T) {
	fa, _ := servers(t)
	t.Setenv("DELPHI_FREE", "local")
	code, _, stderr := cli(t, "", "run", "-provider", "anthropic", "-model", "claude-sonnet-5-5",
		"-prompt", "hello", "-out", filepath.Join(t.TempDir(), "run"), "-every", "1ms")
	if code == 0 || !strings.Contains(stderr, "free mode") || !strings.Contains(stderr, "paid") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.bodies) != 0 {
		t.Errorf("%d request(s) reached the paid API", len(fa.bodies))
	}
}

func TestFreeModeRejectsAnUnknownMode(t *testing.T) {
	servers(t)
	t.Setenv("DELPHI_FREE", "yes")
	if code, _, stderr := cli(t, "", "providers"); code == 0 || !strings.Contains(stderr, "DELPHI_FREE") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}
