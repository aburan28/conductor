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
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
