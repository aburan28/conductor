package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRunThroughFireworksBatch runs a request through the engine against a stand-in
// Fireworks API: dataset, upload, job, poll, signed download.
func TestRunThroughFireworksBatch(t *testing.T) {
	servers(t)
	var mu sync.Mutex
	var customID, outDataset string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/datasets") && r.Method == "POST":
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(p, ":upload"):
			file, _, _ := r.FormFile("file")
			sc := bufio.NewScanner(file)
			sc.Scan()
			var row map[string]any
			_ = json.Unmarshal(sc.Bytes(), &row)
			customID = row["custom_id"].(string)
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(p, "/batchInferenceJobs") && r.Method == "POST":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			outDataset = body["outputDatasetId"].(string)
			fmt.Fprintf(w, `{"name":"accounts/acme/batchInferenceJobs/%s"}`, r.URL.Query().Get("batchInferenceJobId"))
		case strings.Contains(p, "/batchInferenceJobs/") && r.Method == "GET":
			fmt.Fprintf(w, `{"state":"JOB_STATE_COMPLETED","outputDatasetId":%q}`, outDataset)
		case strings.HasSuffix(p, ":getDownloadEndpoint"):
			fmt.Fprintf(w, `{"filenameToSignedUrls":{"results.jsonl":%q}}`, srv.URL+"/signed/results.jsonl")
		case p == "/signed/results.jsonl":
			fmt.Fprintf(w, `{"custom_id":%q,"response":{"body":{"choices":[{"message":{"content":"fireworks answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000000,"completion_tokens":1000000}}}}`+"\n", customID)
		default:
			http.Error(w, `{"error":"no route"}`, 404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FIREWORKS_BASE_URL", srv.URL)
	t.Setenv("FIREWORKS_API_KEY", "fw")
	t.Setenv("FIREWORKS_ACCOUNT_ID", "acme")
	t.Setenv("DELPHI_FIREWORKS_BATCH_PRICES", `{"accounts/fireworks/models/glm-5p3":[0.3,1.1]}`)

	out := filepath.Join(t.TempDir(), "run")
	code, so, se := cli(t, "", "run", "-provider", "fireworks-batch", "-model", "accounts/fireworks/models/glm-5p3",
		"-prompt", "hi", "-out", out, "-every", "1ms")
	if code != 0 {
		t.Fatalf("run: %d\n%s\n%s", code, so, se)
	}
	rows := lines(t, filepath.Join(out, "results.jsonl"))
	if len(rows) != 1 || rows[0]["status"] != "succeeded" || rows[0]["text"] != "fireworks answer" {
		t.Fatalf("results: %v", rows)
	}
	if !strings.Contains(fmt.Sprint(rows[0]["batch_id"]), "accounts/acme/batchInferenceJobs/delphi-") {
		t.Errorf("batch id: %v", rows[0]["batch_id"])
	}
}
