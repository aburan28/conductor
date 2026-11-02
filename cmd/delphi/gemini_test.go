package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRunThroughGeminiBatchMode runs a request through the engine against a stand-in Batch
// Mode API: submit, poll until done, collect the inline answer, price it.
func TestRunThroughGeminiBatchMode(t *testing.T) {
	servers(t)
	var mu sync.Mutex
	polls := 0
	keys := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, ":batchGenerateContent"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			reqs := body["batch"].(map[string]any)["inputConfig"].(map[string]any)["requests"].(map[string]any)["requests"].([]any)
			keys["batches/g1"] = reqs[0].(map[string]any)["metadata"].(map[string]any)["key"].(string)
			fmt.Fprint(w, `{"name":"batches/g1"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/batches/g1"):
			polls++
			if polls == 1 {
				fmt.Fprint(w, `{"name":"batches/g1","metadata":{"state":"BATCH_STATE_RUNNING"}}`)
				return
			}
			fmt.Fprintf(w, `{"name":"batches/g1","done":true,"metadata":{"state":"BATCH_STATE_SUCCEEDED"},"response":{"inlinedResponses":{"inlinedResponses":[{"metadata":{"key":%q},"response":{"candidates":[{"content":{"parts":[{"text":"gemini answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1000000,"candidatesTokenCount":1000000}}}]}}}`, keys["batches/g1"])
		default:
			http.Error(w, `{"error":{"message":"no route"}}`, 404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GEMINI_BASE_URL", srv.URL+"/v1beta")
	t.Setenv("GEMINI_API_KEY", "g")
	t.Setenv("DELPHI_GEMINI_PRICES", `{"gemini-flash":[0.5,2]}`)

	out := filepath.Join(t.TempDir(), "run")
	code, so, se := cli(t, "", "run", "-provider", "gemini", "-model", "gemini-flash", "-prompt", "hi", "-out", out, "-every", "1ms")
	if code != 0 {
		t.Fatalf("run: %d\n%s\n%s", code, so, se)
	}
	rows := lines(t, filepath.Join(out, "results.jsonl"))
	if len(rows) != 1 || rows[0]["status"] != "succeeded" || rows[0]["text"] != "gemini answer" || rows[0]["batch_id"] != "batches/g1" {
		t.Fatalf("results: %v", rows)
	}
	var sum struct {
		Models []struct {
			CostUSD *float64 `json:"estimated_cost_usd"`
		} `json:"models"`
	}
	b, _ := os.ReadFile(filepath.Join(out, "summary.json"))
	if err := json.Unmarshal(b, &sum); err != nil || len(sum.Models) != 1 || sum.Models[0].CostUSD == nil || *sum.Models[0].CostUSD != 2.5 {
		t.Fatalf("cost ($0.50 in + $2.00 out per million, one million each way): %s", b)
	}
}
