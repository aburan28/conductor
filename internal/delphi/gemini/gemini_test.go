package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aburan28/conductor/internal/delphi"
)

// fakeGemini is the Batch Mode surface: create, get, cancel, list. Batch n alternates
// between the two documented places a finished batch puts its inline responses.
type fakeGemini struct {
	mu        sync.Mutex
	batches   map[string]map[string]any // name -> {model, display, requests}
	order     []string
	answer    func(key string) string // inlined response JSON
	failCount int                     // fail this many creates after the first
	cancelled []string
	key       string
}

func newFake(t *testing.T) (*fakeGemini, *Provider) {
	f := &fakeGemini{batches: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.key = r.Header.Get("x-goog-api-key")
		path := strings.TrimPrefix(r.URL.Path, "/v1beta/")
		switch {
		case r.Method == "POST" && strings.HasSuffix(path, ":batchGenerateContent"):
			if len(f.batches) > 0 && f.failCount > 0 {
				f.failCount--
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"error":{"code":429,"message":"quota"}}`)
				return
			}
			var body struct {
				Batch struct {
					DisplayName string `json:"displayName"`
					InputConfig struct {
						Requests struct {
							Requests []map[string]any `json:"requests"`
						} `json:"requests"`
					} `json:"inputConfig"`
				} `json:"batch"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			name := fmt.Sprintf("batches/b%d", len(f.batches)+1)
			f.batches[name] = map[string]any{"model": strings.TrimSuffix(strings.TrimPrefix(path, "models/"), ":batchGenerateContent"),
				"display": body.Batch.DisplayName, "requests": body.Batch.InputConfig.Requests.Requests}
			f.order = append(f.order, name)
			fmt.Fprintf(w, `{"name":%q,"metadata":{"state":"BATCH_STATE_PENDING","displayName":%q}}`, name, body.Batch.DisplayName)
		case r.Method == "POST" && strings.HasSuffix(path, ":cancel"):
			f.cancelled = append(f.cancelled, strings.TrimSuffix(path, ":cancel"))
			fmt.Fprint(w, `{}`)
		case r.Method == "GET" && path == "batches":
			var ops []string
			for _, n := range f.order {
				ops = append(ops, fmt.Sprintf(`{"name":%q,"metadata":{"displayName":%q}}`, n, f.batches[n]["display"]))
			}
			fmt.Fprintf(w, `{"operations":[%s]}`, strings.Join(ops, ","))
		case r.Method == "GET" && f.batches[path] != nil:
			b := f.batches[path]
			var items []string
			for _, req := range b["requests"].([]map[string]any) {
				key := req["metadata"].(map[string]any)["key"].(string)
				items = append(items, f.answer(key))
			}
			inl := fmt.Sprintf(`{"inlinedResponses":[%s]}`, strings.Join(items, ","))
			if path == "batches/b1" { // the guide's shape
				fmt.Fprintf(w, `{"name":%q,"done":true,"metadata":{"state":"BATCH_STATE_SUCCEEDED"},"response":{"inlinedResponses":%s}}`, path, inl)
			} else { // the reference's shape
				fmt.Fprintf(w, `{"name":%q,"done":true,"metadata":{"state":"JOB_STATE_SUCCEEDED","output":{"inlinedResponses":%s}}}`, path, inl)
			}
		default:
			http.Error(w, `{"error":{"code":404,"message":"no route"}}`, 404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GEMINI_BASE_URL", srv.URL+"/v1beta")
	return f, NewWithKey("g-key")
}

func ok(key, text, finish string) string {
	return fmt.Sprintf(`{"metadata":{"key":%q},"response":{"candidates":[{"content":{"parts":[{"text":"thinking...","thought":true},{"text":%q}]},"finishReason":%q}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"thoughtsTokenCount":6,"cachedContentTokenCount":2},"modelVersion":"gemini-test-001"}}`, key, text, finish)
}

func req(id, model string) delphi.Request {
	return delphi.Request{ID: id, Provider: "gemini", Model: model, System: "be brief", MaxTokens: 100, Effort: "low",
		Messages: []delphi.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}, {Role: "user", Content: "q " + id}}}
}

func TestSubmitSplitsByModelAndResultsMatchByKey(t *testing.T) {
	f, p := newFake(t)
	f.answer = func(key string) string {
		switch key {
		case "trunc":
			return ok(key, "partial", "MAX_TOKENS")
		case "safety":
			return fmt.Sprintf(`{"metadata":{"key":%q},"response":{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}}`, key)
		case "blocked":
			return fmt.Sprintf(`{"metadata":{"key":%q},"response":{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}}`, key)
		case "busy":
			return fmt.Sprintf(`{"metadata":{"key":%q},"error":{"code":8,"message":"resource exhausted"}}`, key)
		}
		return ok(key, "answer to "+key, "STOP")
	}
	reqs := []delphi.Request{req("a", "gemini-flash"), req("trunc", "gemini-flash"), req("safety", "gemini-pro"),
		req("blocked", "gemini-pro"), req("busy", "gemini-pro")}
	id, err := p.Submit(context.Background(), reqs, "L1")
	if err != nil || id != "batches/b1|batches/b2" {
		t.Fatalf("submit: %q %v", id, err)
	}
	f.mu.Lock()
	b1 := f.batches["batches/b1"]
	body, _ := json.Marshal(b1["requests"])
	f.mu.Unlock()
	if b1["model"] != "gemini-flash" || b1["display"] != "L1#1" || f.key != "g-key" {
		t.Fatalf("batch 1: %v key %q", b1, f.key)
	}
	for _, want := range []string{`"role":"model"`, `"systemInstruction"`, `"thinkingLevel":"low"`, `"maxOutputTokens":100`, `"key":"a"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("request body lacks %s: %s", want, body)
		}
	}
	st, err := p.Poll(context.Background(), id)
	if err != nil || !st.Ended || st.Failure != "" {
		t.Fatalf("poll: %+v %v", st, err)
	}
	got := map[string]delphi.Result{}
	if err := p.Results(context.Background(), id, func(r delphi.Result) error { got[r.ID] = r; return nil }); err != nil {
		t.Fatal(err)
	}
	if r := got["a"]; r.Status != delphi.Succeeded || r.Text != "answer to a" || r.Usage.InputTokens != 10 || r.Usage.OutputTokens != 10 || r.Usage.CacheReadTokens != 2 || r.Model != "gemini-test-001" {
		t.Errorf("a: %+v %+v", r, r.Usage)
	}
	if r := got["trunc"]; !r.Truncated || r.Status != delphi.Succeeded {
		t.Errorf("trunc: %+v", r)
	}
	if r := got["safety"]; r.Status != delphi.Refused || r.Refusal.Category != "safety" {
		t.Errorf("safety: %+v", r)
	}
	if r := got["blocked"]; r.Status != delphi.Refused || r.Refusal.Category != "prohibited_content" {
		t.Errorf("blocked: %+v", r)
	}
	if r := got["busy"]; r.Status != delphi.Errored || !r.Retryable() {
		t.Errorf("busy: %+v", r)
	}
	// Find recovers the whole flight from its label; Cancel reaches every batch.
	if found, ok, err := p.Find(context.Background(), "L1"); err != nil || !ok || found != id {
		t.Errorf("find: %q %v %v", found, ok, err)
	}
	if err := p.Cancel(context.Background(), id); err != nil || len(f.cancelled) != 2 {
		t.Errorf("cancel: %v %v", err, f.cancelled)
	}
}

func TestAPartialSubmitDoesNotReadAsNothingCreated(t *testing.T) {
	f, p := newFake(t)
	f.failCount = 1
	id, err := p.Submit(context.Background(), []delphi.Request{req("a", "m1"), req("b", "m2")}, "L2")
	if err == nil || id != "batches/b1" || StatusOf(err) != 0 {
		t.Fatalf("partial submit: id %q err %v status %d", id, err, StatusOf(err))
	}
	// A rejection of the very first batch is definite.
	f2, p2 := newFake(t)
	f2.failCount = 1
	f2.batches["batches/seed"] = map[string]any{"display": "x", "requests": []map[string]any{}}
	_, err = p2.Submit(context.Background(), []delphi.Request{req("a", "m1")}, "L3")
	if StatusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("first-batch rejection: %v (status %d)", err, StatusOf(err))
	}
}

func TestValidateRefusesWhatGeminiCannotTake(t *testing.T) {
	p := Offline()
	r := req("a", "m")
	r.Effort = "xhigh"
	r.Messages = append(r.Messages, delphi.Message{Role: "system", Content: "x"})
	r.Params = map[string]any{"contents": []any{}}
	if got := strings.Join(p.Validate(r), "; "); !strings.Contains(got, "thinkingLevel") || !strings.Contains(got, "params.contents") || !strings.Contains(got, `role "system"`) {
		t.Errorf("validate: %s", got)
	}
}
