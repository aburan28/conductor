package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/delphi"
)

// ---- fake Anthropic Message Batches API ----------------------------------

type fakeAnthropic struct {
	mu      sync.Mutex
	batches map[string][]map[string]any
	bodies  []map[string]any
	key     string
	outcome func(id string) string // JSON result object; "" omits the row
}

func (f *fakeAnthropic) batch(id, host, status string) string {
	return fmt.Sprintf(`{"id":%q,"type":"message_batch","processing_status":%q,"request_counts":{"processing":0,"succeeded":%d,"errored":0,"canceled":0,"expired":0},"created_at":"2026-10-15T00:00:00Z","expires_at":"2026-10-16T00:00:00Z","ended_at":null,"cancel_initiated_at":null,"archived_at":null,"results_url":"http://%s/v1/messages/batches/%s/results"}`,
		id, status, len(f.batches[id]), host, id)
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key = r.Header.Get("X-Api-Key")
	w.Header().Set("content-type", "application/json")
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // v1 messages batches [id [results|cancel]]
	switch {
	case r.Method == "POST" && len(p) == 3:
		var body struct {
			Requests []map[string]any `json:"requests"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		id := fmt.Sprintf("msgbatch_%d", len(f.batches)+1)
		f.batches[id] = body.Requests
		f.bodies = append(f.bodies, body.Requests...)
		fmt.Fprint(w, f.batch(id, r.Host, "in_progress"))
	case r.Method == "GET" && len(p) == 4:
		fmt.Fprint(w, f.batch(p[3], r.Host, "ended"))
	case r.Method == "GET" && len(p) == 5 && p[4] == "results":
		w.Header().Set("content-type", "application/binary")
		reqs := f.batches[p[3]]
		for i := len(reqs) - 1; i >= 0; i-- {
			id := reqs[i]["custom_id"].(string)
			if res := f.outcome(id); res != "" {
				fmt.Fprintf(w, `{"custom_id":%q,"result":%s}`+"\n", id, res)
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func claudeMsg(text, stop, extra string) string {
	return fmt.Sprintf(`{"type":"succeeded","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-mythos-5-1","content":[{"type":"thinking","thinking":"","signature":"s"},{"type":"text","text":%q}],"stop_reason":%q,"stop_sequence":null%s,"usage":{"input_tokens":1000,"output_tokens":2000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, text, stop, extra)
}

// ---- fake OpenAI Files + Batch API --------------------------------------

type fakeOpenAI struct {
	mu      sync.Mutex
	files   map[string][]byte
	batches map[string]map[string]any
	lines   []map[string]any // uploaded input lines
	auth    string
	outcome func(customID string, body map[string]any) (output, errorLine string)
}

func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	w.Header().Set("content-type", "application/json")
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // v1 files|batches ...
	switch {
	case r.Method == "POST" && len(p) == 2 && p[1] == "files":
		file, _, err := r.FormFile("file")
		if err != nil || r.FormValue("purpose") != "batch" {
			http.Error(w, `{"error":{"message":"bad upload","type":"invalid_request_error"}}`, 400)
			return
		}
		data, _ := io.ReadAll(file)
		id := fmt.Sprintf("file-in-%d", len(f.files)+1)
		f.files[id] = data
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			var l map[string]any
			json.Unmarshal(sc.Bytes(), &l)
			f.lines = append(f.lines, l)
		}
		fmt.Fprintf(w, `{"id":%q,"object":"file","bytes":%d,"created_at":0,"filename":"in.jsonl","purpose":"batch","status":"processed"}`, id, len(data))
	case r.Method == "POST" && len(p) == 2 && p[1] == "batches":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["endpoint"] != "/v1/responses" || body["completion_window"] != "24h" {
			http.Error(w, `{"error":{"message":"bad batch","type":"invalid_request_error"}}`, 400)
			return
		}
		id := fmt.Sprintf("batch_%d", len(f.batches)+1)
		body["id"], body["object"], body["status"], body["created_at"] = id, "batch", "in_progress", 0
		f.batches[id] = body
		b, _ := json.Marshal(body)
		w.Write(b)
	case r.Method == "GET" && len(p) == 3 && p[1] == "batches":
		b := f.batches[p[2]]
		var out, errs []byte
		sc := bufio.NewScanner(bytes.NewReader(f.files[b["input_file_id"].(string)]))
		for sc.Scan() {
			var l struct {
				CustomID string         `json:"custom_id"`
				Body     map[string]any `json:"body"`
			}
			json.Unmarshal(sc.Bytes(), &l)
			o, e := f.outcome(l.CustomID, l.Body)
			if o != "" {
				out = append(out, o+"\n"...)
			}
			if e != "" {
				errs = append(errs, e+"\n"...)
			}
		}
		f.files["file-out-"+p[2]], f.files["file-err-"+p[2]] = out, errs
		b["status"], b["output_file_id"], b["error_file_id"] = "completed", "file-out-"+p[2], "file-err-"+p[2]
		js, _ := json.Marshal(b)
		w.Write(js)
	case r.Method == "GET" && len(p) == 2 && p[1] == "batches":
		var data []map[string]any
		for _, b := range f.batches {
			data = append(data, b)
		}
		js, _ := json.Marshal(map[string]any{"object": "list", "data": data, "has_more": false})
		w.Write(js)
	case r.Method == "GET" && len(p) == 4 && p[1] == "files" && p[3] == "content":
		w.Header().Set("content-type", "application/octet-stream")
		w.Write(f.files[p[2]])
	default:
		http.NotFound(w, r)
	}
}

func oaOK(id, text, status string) string {
	extra := ""
	if status == "incomplete" {
		extra = `,"incomplete_details":{"reason":"max_output_tokens"}`
	}
	return fmt.Sprintf(`{"id":"batch_req_1","custom_id":%q,"response":{"status_code":200,"request_id":"req_x","body":{"id":"resp_1","object":"response","status":%q,"model":"gpt-test"%s,"output":[{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q,"annotations":[]}]}],"usage":{"input_tokens":100,"output_tokens":200,"input_tokens_details":{"cached_tokens":40},"output_tokens_details":{"reasoning_tokens":150}}}},"error":null}`, id, status, extra, text)
}

// ---- harness -------------------------------------------------------------

func servers(t *testing.T) (*fakeAnthropic, *fakeOpenAI) {
	t.Helper()
	fa := &fakeAnthropic{batches: map[string][]map[string]any{}, outcome: func(id string) string { return claudeMsg("claude says "+id, "end_turn", "") }}
	fo := &fakeOpenAI{files: map[string][]byte{}, batches: map[string]map[string]any{},
		outcome: func(id string, _ map[string]any) (string, string) { return oaOK(id, "gpt says "+id, "completed"), "" }}
	sa, so := httptest.NewServer(fa), httptest.NewServer(fo)
	t.Cleanup(sa.Close)
	t.Cleanup(so.Close)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("ANTHROPIC_BASE_URL", sa.URL)
	t.Setenv("OPENAI_API_KEY", "sk-oa-test")
	t.Setenv("OPENAI_BASE_URL", so.URL+"/v1/")
	t.Setenv("DELPHI_OPENAI_MODEL", "gpt-test")
	t.Setenv("DELPHI_OPENAI_PRICES", `{"gpt-test":[1,4]}`)
	t.Setenv("DELPHI_HOME", t.TempDir())
	return fa, fo
}

func cli(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &app{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb})
	return code, out.String(), errb.String()
}

func lines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			var m map[string]any
			json.Unmarshal([]byte(l), &m)
			out = append(out, m)
		}
	}
	return out
}

func TestRunMixedProvidersEndToEnd(t *testing.T) {
	fa, fo := servers(t)
	in := filepath.Join(t.TempDir(), "jobs.jsonl")
	os.WriteFile(in, []byte(`{"id":"c1","prompt":"alpha","effort":"high","system":"be brief"}
{"id":"g1","provider":"openai","prompt":"beta","effort":"high","params":{"metadata":{"k":"v"}}}
{"id":"c2","provider":"anthropic","messages":[{"role":"user","content":"gamma"}],"meta":{"task":"T-9"}}
`), 0o644)
	out := filepath.Join(t.TempDir(), "run")
	code, stdout, stderr := cli(t, "", "run", "-in", in, "-out", out, "-every", "1ms")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	// Anthropic wire format.
	if fa.key != "sk-ant-test" || len(fa.bodies) != 2 {
		t.Fatalf("anthropic: key %q, %d requests", fa.key, len(fa.bodies))
	}
	p := fa.bodies[0]["params"].(map[string]any)
	if p["model"] != "claude-mythos-5-1" || p["output_config"].(map[string]any)["effort"] != "high" ||
		p["system"].([]any)[0].(map[string]any)["text"] != "be brief" ||
		p["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "alpha" {
		t.Fatalf("anthropic params: %v", p)
	}
	for _, k := range []string{"thinking", "temperature", "stream"} {
		if _, bad := p[k]; bad {
			t.Errorf("anthropic body carries %s", k)
		}
	}
	// OpenAI wire format: one JSONL line per request against /v1/responses.
	if fo.auth != "Bearer sk-oa-test" || len(fo.lines) != 1 {
		t.Fatalf("openai: auth %q, %d lines", fo.auth, len(fo.lines))
	}
	l := fo.lines[0]
	body := l["body"].(map[string]any)
	if l["custom_id"] != "g1" || l["method"] != "POST" || l["url"] != "/v1/responses" || body["model"] != "gpt-test" ||
		body["max_output_tokens"].(float64) != 32000 || body["reasoning"].(map[string]any)["effort"] != "high" ||
		body["input"].([]any)[0].(map[string]any)["content"] != "beta" || body["metadata"].(map[string]any)["k"] != "v" {
		t.Fatalf("openai line: %v", l)
	}
	for _, b := range fo.batches {
		if b["metadata"].(map[string]any)["delphi_label"] == nil {
			t.Fatal("openai batch lacks the delphi label")
		}
	}
	// Results in input order, one schema for both providers.
	rs := lines(t, filepath.Join(out, "results.jsonl"))
	got := []string{}
	for _, r := range rs {
		got = append(got, fmt.Sprintf("%s/%s/%s/%s", r["id"], r["provider"], r["status"], r["text"]))
	}
	want := "c1/anthropic/succeeded/claude says c1 g1/openai/succeeded/gpt says g1 c2/anthropic/succeeded/claude says c2"
	if strings.Join(got, " ") != want {
		t.Fatalf("results:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if rs[2]["meta"].(map[string]any)["task"] != "T-9" {
		t.Fatalf("meta lost: %v", rs[2])
	}
	if u := rs[1]["usage"].(map[string]any); u["cache_read_tokens"].(float64) != 40 {
		t.Fatalf("openai usage: %v", u)
	}
	var sum runSummary
	b, _ := os.ReadFile(filepath.Join(out, "summary.json"))
	json.Unmarshal(b, &sum)
	if len(sum.Models) != 2 || *sum.Models[0].CostUSD != 0.11 || *sum.Models[1].CostUSD != 0.0009 {
		t.Fatalf("summary models: %+v", sum.Models)
	}
	// Re-running resumes: nothing is submitted again.
	if code, _, se := cli(t, "", "run", "-in", in, "-out", out, "-every", "1ms"); code != 0 || len(fa.bodies) != 2 || len(fo.lines) != 1 {
		t.Fatalf("resume: code %d, anthropic %d, openai %d %s", code, len(fa.bodies), len(fo.lines), se)
	}
	// The same ids with different content are refused.
	os.WriteFile(in, []byte(`{"id":"c1","prompt":"changed"}`), 0o644)
	if code, _, se := cli(t, "", "run", "-in", in, "-out", out); code != exitUsage || !strings.Contains(se, "different request") {
		t.Fatalf("conflict: %d %s", code, se)
	}
}

func TestGoalLoopRevisesAndResumesWithoutDuplicateBatches(t *testing.T) {
	fa, _ := servers(t)
	fa.mu.Lock()
	fa.outcome = func(id string) string {
		switch id {
		case "goal-r1-plan", "goal-r2-plan":
			return claudeMsg("plan for "+id, "end_turn", "")
		case "goal-r1-draft", "goal-r2-draft":
			return claudeMsg("answer from "+id, "end_turn", "")
		case "goal-r1-review":
			return claudeMsg(`{"verdict":"revise","feedback":"add the missing check"}`, "end_turn", "")
		case "goal-r2-review":
			return claudeMsg(`{"verdict":"accept","feedback":"criteria addressed"}`, "end_turn", "")
		}
		return ""
	}
	fa.mu.Unlock()
	out := filepath.Join(t.TempDir(), "goal")
	if code, _, se := cli(t, "", "goal", "init", "-out", out, "-goal", "Assess an option", "-rounds", "2"); code != 0 {
		t.Fatalf("init: %d %s", code, se)
	}
	if code, _, se := cli(t, "", "goal", "run", "-out", out, "-every", "1ms"); code != 0 {
		t.Fatalf("run: %d %s", code, se)
	}
	s, err := loadGoal(out)
	if err != nil || s.Status != "self_reviewed" || s.Round != 2 || len(s.History) != 2 || s.History[0].Verdict != "revise" {
		t.Fatalf("state: %+v, %v", s, err)
	}
	b, err := os.ReadFile(filepath.Join(out, "answer.md"))
	if err != nil || string(b) != "answer from goal-r2-draft\n" {
		t.Fatalf("answer: %q %v", b, err)
	}
	if len(fa.bodies) != 6 {
		t.Fatalf("submitted %d requests, want six", len(fa.bodies))
	}
	if code, _, se := cli(t, "", "goal", "run", "-out", out, "-every", "1ms"); code != 0 || len(fa.bodies) != 6 {
		t.Fatalf("resume duplicated work: %d, %d requests, %s", code, len(fa.bodies), se)
	}
}

func TestGoalRoundLimitKeepsUnacceptedDraft(t *testing.T) {
	s := goalState{Stage: "review", Round: 1, MaxRounds: 1, Plan: "plan", Draft: "draft"}
	r := delphi.Result{Status: delphi.Succeeded, Text: `{"verdict":"revise","feedback":"still incomplete"}`}
	if err := nextGoal(&s, r); err != nil || s.Status != "needs_review" || s.Stage != "done" || len(s.History) != 1 {
		t.Fatalf("limit state: %+v, %v", s, err)
	}
}

func TestGoalStartRunsDetachedBinary(t *testing.T) {
	fa, _ := servers(t)
	fa.mu.Lock()
	fa.outcome = func(id string) string {
		if strings.HasSuffix(id, "-review") {
			return claudeMsg(`{"verdict":"accept","feedback":"checked"}`, "end_turn", "")
		}
		return claudeMsg("work from "+id, "end_turn", "")
	}
	fa.mu.Unlock()
	bin := filepath.Join(t.TempDir(), "delphi")
	build := exec.Command("go", "build", "-o", bin, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build detached CLI: %v\n%s", err, output)
	}
	out := filepath.Join(t.TempDir(), "goal")
	cmd := exec.Command(bin, "goal", "start", "-out", out, "-goal", "Produce an answer", "-rounds", "1", "-every", "10ms")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start detached CLI: %v\n%s", err, output)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s, err := loadGoal(out)
		if err == nil && s.Stage == "done" {
			fa.mu.Lock()
			count := len(fa.bodies)
			fa.mu.Unlock()
			if s.Status != "self_reviewed" || count != 3 {
				t.Fatalf("detached state: %+v, requests %d", s, count)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	_, _ = exec.Command(bin, "goal", "stop", "-out", out).CombinedOutput()
	log, _ := os.ReadFile(filepath.Join(out, "runner.log"))
	t.Fatalf("detached runner did not finish; log:\n%s", log)
}

func TestRunAnthropicWithWorkloadFederation(t *testing.T) {
	fa := &fakeAnthropic{batches: map[string][]map[string]any{}, outcome: func(id string) string {
		return claudeMsg("federated reply for "+id, "end_turn", "")
	}}
	var exchangeCalls, batchCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			exchangeCalls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode token exchange: %v", err)
			}
			for key, want := range map[string]string{
				"grant_type":         "urn:ietf:params:oauth:grant-type:jwt-bearer",
				"assertion":          "header.claims.signature",
				"federation_rule_id": "fdrl_test",
				"organization_id":    "org_test",
				"service_account_id": "svac_test",
				"workspace_id":       "default",
			} {
				if body[key] != want {
					t.Errorf("exchange %s = %v, want %s", key, body[key], want)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"federated-access","token_type":"Bearer","expires_in":600}`)
			return
		}
		if r.URL.Path == "/v1/messages/batches" {
			batchCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer federated-access" {
				t.Errorf("batch Authorization = %q", got)
			}
			if got := r.Header.Get("X-Api-Key"); got != "" {
				t.Errorf("batch unexpectedly used an API key")
			}
		}
		fa.ServeHTTP(w, r)
	}))
	defer srv.Close()
	for key, value := range map[string]string{
		"ANTHROPIC_API_KEY": "", "ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_PROFILE": "",
		"ANTHROPIC_BASE_URL": srv.URL, "ANTHROPIC_FEDERATION_RULE_ID": "fdrl_test",
		"ANTHROPIC_ORGANIZATION_ID": "org_test", "ANTHROPIC_SERVICE_ACCOUNT_ID": "svac_test",
		"ANTHROPIC_WORKSPACE_ID": "default", "OPENAI_API_KEY": "",
	} {
		t.Setenv(key, value)
	}
	assertion := filepath.Join(t.TempDir(), "assertion")
	if err := os.WriteFile(assertion, []byte("header.claims.signature"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN_FILE", assertion)
	t.Setenv("ANTHROPIC_IDENTITY_TOKEN", "")
	out := filepath.Join(t.TempDir(), "run")
	code, _, stderr := cli(t, "", "run", "-prompt", "Reply briefly", "-out", out, "-every", "1ms")
	if code != exitOK {
		t.Fatalf("delphi run exit %d: %s", code, stderr)
	}
	if exchangeCalls != 1 || batchCalls != 1 {
		t.Fatalf("exchange calls %d, batch submits %d", exchangeCalls, batchCalls)
	}
	results := lines(t, filepath.Join(out, "results.jsonl"))
	if len(results) != 1 || results[0]["status"] != "succeeded" {
		t.Fatalf("unexpected results: %v", results)
	}
}

func TestOutcomeClassification(t *testing.T) {
	fa, fo := servers(t)
	fa.outcome = func(id string) string {
		switch id {
		case "refused":
			return claudeMsg("", "refusal", `,"stop_details":{"type":"refusal","category":"cyber","explanation":"declined"}`)
		case "cut":
			return claudeMsg("partial", "max_tokens", "")
		case "bad":
			return `{"type":"errored","error":{"type":"error","error":{"type":"invalid_request_error","message":"nope"},"request_id":"req_1"}}`
		}
		return claudeMsg("ok", "end_turn", "")
	}
	fo.outcome = func(id string, _ map[string]any) (string, string) {
		switch id {
		case "o-cut":
			return oaOK(id, "half", "incomplete"), ""
		case "o-bad":
			return fmt.Sprintf(`{"custom_id":%q,"response":{"status_code":400,"request_id":"r","body":{"error":{"message":"bad param","type":"invalid_request_error"}}},"error":null}`, id), ""
		case "o-late":
			return "", fmt.Sprintf(`{"custom_id":%q,"response":null,"error":{"code":"batch_expired","message":"expired"}}`, id)
		case "o-refused":
			return fmt.Sprintf(`{"custom_id":%q,"response":{"status_code":200,"request_id":"r","body":{"status":"completed","model":"gpt-test","output":[{"type":"message","content":[{"type":"refusal","refusal":"I can't help with that."}]}],"usage":{"input_tokens":5,"output_tokens":5}}},"error":null}`, id), ""
		}
		return oaOK(id, "fine", "completed"), ""
	}
	var rows []string
	for _, id := range []string{"refused", "cut", "bad"} {
		rows = append(rows, fmt.Sprintf(`{"id":%q,"prompt":"p"}`, id))
	}
	for _, id := range []string{"o-cut", "o-bad", "o-late", "o-refused"} {
		rows = append(rows, fmt.Sprintf(`{"id":%q,"provider":"openai","prompt":"p"}`, id))
	}
	in := filepath.Join(t.TempDir(), "in.jsonl")
	os.WriteFile(in, []byte(strings.Join(rows, "\n")), 0o644)
	out := filepath.Join(t.TempDir(), "run")
	code, _, _ := cli(t, "", "run", "-in", in, "-out", out, "-every", "1ms", "-max-attempts", "1")
	if code != exitFailures {
		t.Fatalf("exit %d", code)
	}
	got := map[string]map[string]any{}
	for _, r := range lines(t, filepath.Join(out, "results.jsonl")) {
		got[r["id"].(string)] = r
	}
	check := func(id, status string, truncated bool) {
		t.Helper()
		r := got[id]
		if r["status"] != status || (r["truncated"] == true) != truncated {
			t.Errorf("%s: %v", id, r)
		}
	}
	check("refused", "refused", false)
	check("cut", "succeeded", true)
	check("bad", "errored", false)
	check("o-cut", "succeeded", true)
	check("o-bad", "errored", false)
	check("o-late", "expired", false)
	check("o-refused", "refused", false)
	if got["refused"]["refusal"].(map[string]any)["category"] != "cyber" {
		t.Errorf("anthropic refusal category: %v", got["refused"])
	}
	if e := got["o-bad"]["error"].(map[string]any); e["type"] != "invalid_request_error" || e["retryable"] != false {
		t.Errorf("openai 400: %v", e)
	}
	retry := lines(t, filepath.Join(out, "retryable.jsonl"))
	if len(retry) != 1 || retry[0]["id"] != "o-late" || retry[0]["provider"] != "openai" {
		t.Fatalf("retryable: %v", retry)
	}
}

func TestAskServeGetWithHook(t *testing.T) {
	servers(t)
	hookLog := filepath.Join(t.TempDir(), "hook.log")
	code, id, se := cli(t, "what is 2+2?", "ask", "-provider", "openai", "-meta", "task=T-1")
	if code != 0 {
		t.Fatalf("ask: %d %s", code, se)
	}
	id = strings.TrimSpace(id)
	if code, _, _ := cli(t, "", "get", id); code != exitFailures {
		t.Fatal("result before the loop ran")
	}
	hook := fmt.Sprintf(`echo "$DELPHI_ID $DELPHI_STATUS $(test -f "$DELPHI_RESULT" && echo file)" >> %s`, hookLog)
	for i := 0; i < 2; i++ { // pass 1 submits, pass 2 collects
		if code, so, se := cli(t, "", "serve", "-once", "-flush-every", "0", "-on-result", hook); code != 0 {
			t.Fatalf("serve: %d %s %s", code, so, se)
		}
	}
	code, text, _ := cli(t, "", "get", "-text", id)
	if code != 0 || strings.TrimSpace(text) != "gpt says "+id {
		t.Fatalf("get: %d %q", code, text)
	}
	if b, _ := os.ReadFile(hookLog); strings.TrimSpace(string(b)) != id+" succeeded file" {
		t.Fatalf("hook log: %q", b)
	}
}

func TestValidateAndProvidersNeedNoKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("DELPHI_OPENAI_MODEL", "")
	code, so, _ := cli(t, `{"id":"a","prompt":"x"}`+"\n"+`{"id":"b","provider":"openai","model":"gpt-x","prompt":"y"}`, "validate", "-in", "-")
	if code != 0 || !strings.Contains(so, "anthropic claude-mythos-5-1") || !strings.Contains(so, "openai gpt-x") {
		t.Fatalf("validate: %d %s", code, so)
	}
	cases := map[string]string{
		`{"id":"a","prompt":"x","params":{"temperature":0}}`:                                       "sampling",
		`{"id":"a","prompt":"x","params":{"tool_choice":{"type":"any"}}}`:                          "tool_choice",
		`{"id":"a","prompt":"x","params":{"thinking":{"type":"disabled"}}}`:                        "thinking",
		`{"id":"a","prompt":"x","effort":"turbo"}`:                                                 "effort",
		`{"id":"a","provider":"openai","prompt":"x"}`:                                              "no model",
		`{"id":"a","provider":"bedrock","prompt":"x"}`:                                             "not one of",
		`{"id":"a","prompt":"x","params":{"max_output_tokens":5}}`:                                 "max_tokens",
		`{"id":"a","messages":[{"role":"user","content":"q"},{"role":"assistant","content":"{"}]}`: "prefill",
		`{"id":"bad id","prompt":"x"}`:                                                             "id must match",
	}
	for row, needle := range cases {
		if code, _, se := cli(t, row, "validate", "-in", "-"); code != exitUsage || !strings.Contains(se, needle) {
			t.Errorf("%s: code %d, %s", row, code, se)
		}
	}
	// a non-strict Claude model may use sampling parameters
	if code, _, se := cli(t, `{"id":"a","model":"claude-haiku-4-5","prompt":"x","params":{"temperature":0}}`, "validate", "-in", "-"); code != 0 {
		t.Errorf("haiku temperature: %s", se)
	}
	code, so, _ = cli(t, "", "providers")
	if code != 0 || !strings.Contains(so, "export ANTHROPIC_API_KEY") || !strings.Contains(so, "DELPHI_OPENAI_MODEL") {
		t.Fatalf("providers: %s", so)
	}
	if code, _, se := cli(t, "", "serve", "-once"); code != exitUsage || !strings.Contains(se, "no provider credentials") {
		t.Fatalf("serve without keys: %d %s", code, se)
	}
}

func TestSubmitRejectionFinishesAsError(t *testing.T) {
	servers(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"In order to access this model, your organization or workspace must have data retention enabled."}}`)
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	out := filepath.Join(t.TempDir(), "run")
	code, _, se := cli(t, "", "run", "-prompt", "hi", "-out", out, "-every", "1ms")
	if code != exitFailures {
		t.Fatalf("exit %d %s", code, se)
	}
	r := lines(t, filepath.Join(out, "results.jsonl"))[0]
	if r["status"] != "errored" || !strings.Contains(r["error"].(map[string]any)["message"].(string), "data retention") {
		t.Fatalf("%v", r)
	}
}

func TestProfileAndFallbackOnRefusal(t *testing.T) {
	fa, fo := servers(t)
	fa.outcome = func(id string) string {
		return claudeMsg("", "refusal", `,"stop_details":{"type":"refusal","category":"cyber","explanation":"declined"}`)
	}
	profiles := filepath.Join(t.TempDir(), "profiles.json")
	t.Setenv("DELPHI_PROFILES", profiles)
	os.WriteFile(profiles, []byte(`{
	  "implementer": {"provider": "anthropic", "model": "claude-haiku-5-5", "effort": "max"},
	  "frontier": {"provider": "anthropic", "model": "claude-mythos-5-1", "effort": "high",
	               "fallback": ["openai:gpt-daybreak@high"]}}`), 0o644)

	code, so, _ := cli(t, "", "profiles")
	if code != 0 || !strings.Contains(so, "frontier       anthropic:claude-mythos-5-1 effort high -> openai:gpt-daybreak@high") ||
		!strings.Contains(so, "implementer    anthropic:claude-haiku-5-5 effort max") {
		t.Fatalf("profiles: %d %s", code, so)
	}
	if code, _, se := cli(t, "", "run", "-prompt", "x", "-out", t.TempDir(), "-profile", "ghost"); code != exitUsage || !strings.Contains(se, "ghost") {
		t.Fatalf("unknown profile: %d %s", code, se)
	}
	// Haiku 5.5 takes only default sampling.
	if code, _, se := cli(t, `{"id":"a","prompt":"x","params":{"temperature":0}}`, "validate", "-in", "-", "-profile", "implementer"); code != exitUsage || !strings.Contains(se, "sampling") {
		t.Fatalf("haiku 5.5 temperature: %d %s", code, se)
	}

	out := filepath.Join(t.TempDir(), "run")
	code, so, se := cli(t, "", "run", "-prompt", "probe the parser", "-out", out, "-every", "1ms", "-profile", "frontier")
	if code != 0 {
		t.Fatalf("run: %d %s %s", code, so, se)
	}
	res := lines(t, filepath.Join(out, "results.jsonl"))
	if len(res) != 1 || res[0]["status"] != "succeeded" || res[0]["provider"] != "openai" || res[0]["text"] != "gpt says prompt-1" {
		t.Fatalf("results: %v", res)
	}
	if ff, _ := res[0]["fallback_from"].([]any); len(ff) != 1 || ff[0] != "anthropic:claude-mythos-5-1 refused (cyber)" {
		t.Fatalf("fallback_from: %v", res[0]["fallback_from"])
	}
	sent := fa.bodies[0]["params"].(map[string]any)
	if sent["model"] != "claude-mythos-5-1" || sent["output_config"].(map[string]any)["effort"] != "high" {
		t.Fatalf("anthropic body: %v", sent)
	}
	body := fo.lines[0]["body"].(map[string]any)
	if body["model"] != "gpt-daybreak" || body["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatalf("openai body: %v", body)
	}

	// An explicit flag beats the profile, and -fallback replaces its chain.
	code, so, se = cli(t, `{"id":"a","prompt":"x"}`, "validate", "-in", "-", "-profile", "frontier", "-model", "claude-opus-5-5")
	if code != 0 || !strings.Contains(so, "anthropic claude-opus-5-5") {
		t.Fatalf("flag over profile: %d %s %s", code, so, se)
	}
	if code, _, se := cli(t, `{"id":"a","prompt":"x"}`, "validate", "-in", "-", "-fallback", "nosuch"); code != exitUsage || !strings.Contains(se, "nosuch") {
		t.Fatalf("bad fallback: %d %s", code, se)
	}
}
