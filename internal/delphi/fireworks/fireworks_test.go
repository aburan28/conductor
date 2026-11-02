package fireworks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aburan28/conductor/internal/delphi"
)

type fakeFireworks struct {
	mu       sync.Mutex
	datasets map[string]string         // id -> uploaded JSONL
	jobs     map[string]map[string]any // job id -> create body
	polls    map[string]int
	auth     []string
	outputs  func(job string) map[string]string // job -> file name -> JSONL
}

func newFake(t *testing.T) (*fakeFireworks, *Provider) {
	f := &fakeFireworks{datasets: map[string]string{}, jobs: map[string]map[string]any{}, polls: map[string]int{}}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		if strings.HasPrefix(p, "/files/") { // signed URLs carry no auth
			if r.Header.Get("Authorization") != "" {
				http.Error(w, "signed URL got an Authorization header", 400)
				return
			}
			parts := strings.SplitN(strings.TrimPrefix(p, "/files/"), "/", 2)
			fmt.Fprint(w, f.outputs(parts[0])[parts[1]])
			return
		}
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		const acct = "/v1/accounts/acme/"
		switch {
		case r.Method == "POST" && p == acct+"datasets":
			var body struct {
				DatasetID string `json:"datasetId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.datasets[body.DatasetID] = ""
			fmt.Fprint(w, `{}`)
		case r.Method == "POST" && strings.HasSuffix(p, ":upload"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, acct+"datasets/"), ":upload")
			file, _, err := r.FormFile("file")
			if err != nil {
				http.Error(w, `{"error":"no file"}`, 400)
				return
			}
			b, _ := io.ReadAll(file)
			f.datasets[id] = string(b)
			fmt.Fprint(w, `{}`)
		case r.Method == "POST" && p == acct+"batchInferenceJobs":
			id := r.URL.Query().Get("batchInferenceJobId")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.jobs[id] = body
			fmt.Fprintf(w, `{"name":"accounts/acme/batchInferenceJobs/%s","state":"JOB_STATE_VALIDATING"}`, id)
		case r.Method == "GET" && strings.HasPrefix(p, acct+"batchInferenceJobs/"):
			id := strings.TrimPrefix(p, acct+"batchInferenceJobs/")
			body, ok := f.jobs[id]
			if !ok {
				http.Error(w, `{"error":"not found"}`, 404)
				return
			}
			f.polls[id]++
			state := "JOB_STATE_RUNNING"
			if f.polls[id] > 1 {
				state = "JOB_STATE_COMPLETED"
			}
			fmt.Fprintf(w, `{"name":"accounts/acme/batchInferenceJobs/%s","state":%q,"outputDatasetId":%q}`, id, state, body["outputDatasetId"])
		case r.Method == "GET" && strings.HasSuffix(p, ":getDownloadEndpoint"):
			out := strings.TrimSuffix(strings.TrimPrefix(p, acct+"datasets/"), ":getDownloadEndpoint")
			job := strings.TrimSuffix(out, "-out")
			urls := map[string]string{}
			for name := range f.outputs(job) {
				urls[name] = srv.URL + "/files/" + job + "/" + name
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"filenameToSignedUrls": urls})
		default:
			http.Error(w, `{"error":"no route"}`, 404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FIREWORKS_BASE_URL", srv.URL)
	t.Setenv("FIREWORKS_API_KEY", "fw-key")
	t.Setenv("FIREWORKS_ACCOUNT_ID", "acme")
	t.Setenv("DELPHI_FIREWORKS_BATCH_PRICES", `{"accounts/fireworks/models/a":[0.1,0.2]}`)
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return f, p
}

func rq(id, model string) delphi.Request {
	return delphi.Request{ID: id, Provider: Name, Model: model, System: "be brief", MaxTokens: 64,
		Messages: []delphi.Message{{Role: "user", Content: "q " + id}}}
}

func TestSubmitPollAndReadEveryOutputShape(t *testing.T) {
	f, p := newFake(t)
	f.outputs = func(job string) map[string]string {
		if strings.HasSuffix(job, "-m1") {
			return map[string]string{
				"results.jsonl": `{"custom_id":"a","response":{"body":{"model":"m-a","choices":[{"message":{"content":"answer a"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}}}` + "\n" +
					`{"custom_id":"len","choices":[{"message":{"content":[{"type":"text","text":"cut"}]},"finish_reason":"length"}]}` + "\n",
				"errors.jsonl": `{"custom_id":"bad","error":{"message":"boom"}}` + "\n",
			}
		}
		return map[string]string{"results.jsonl": `{"custom_id":"b","response":{"choices":[{"message":{"content":""},"finish_reason":"content_filter"}]}}` + "\n"}
	}
	reqs := []delphi.Request{rq("a", "accounts/fireworks/models/a"), rq("len", "accounts/fireworks/models/a"),
		rq("bad", "accounts/fireworks/models/a"), rq("b", "accounts/fireworks/models/b")}
	label := "delphi-20261102T120000Z-abcd"
	id, err := p.Submit(context.Background(), reqs, label)
	want := "accounts/acme/batchInferenceJobs/delphi-20261102t120000z-abcd-m1|accounts/acme/batchInferenceJobs/delphi-20261102t120000z-abcd-m2"
	if err != nil || id != want {
		t.Fatalf("submit: %q %v", id, err)
	}
	f.mu.Lock()
	in := f.datasets["delphi-20261102t120000z-abcd-m1-in"]
	job := f.jobs["delphi-20261102t120000z-abcd-m1"]
	f.mu.Unlock()
	if !strings.Contains(in, `"custom_id":"a"`) || !strings.Contains(in, `"role":"system"`) || strings.Contains(in, `"model"`) || strings.Count(in, "\n") != 3 {
		t.Errorf("input dataset: %s", in)
	}
	if job["model"] != "accounts/fireworks/models/a" || job["inputDatasetId"] != "accounts/acme/datasets/delphi-20261102t120000z-abcd-m1-in" {
		t.Errorf("job: %v", job)
	}
	if st, _ := p.Poll(context.Background(), id); st.Ended {
		t.Fatal("ended on the first poll (running)")
	}
	if st, err := p.Poll(context.Background(), id); err != nil || !st.Ended || st.Failure != "" {
		t.Fatalf("second poll: %+v %v", st, err)
	}
	got := map[string]delphi.Result{}
	if err := p.Results(context.Background(), id, func(r delphi.Result) error { got[r.ID] = r; return nil }); err != nil {
		t.Fatal(err)
	}
	if r := got["a"]; r.Status != delphi.Succeeded || r.Text != "answer a" || r.Usage.CacheReadTokens != 3 || r.Model != "m-a" {
		t.Errorf("a: %+v", r)
	}
	if r := got["len"]; r.Text != "cut" || !r.Truncated {
		t.Errorf("len: %+v", r)
	}
	if r := got["bad"]; r.Status != delphi.Errored || r.Error.Message != "boom" || !r.Retryable() {
		t.Errorf("bad: %+v", r)
	}
	if r := got["b"]; r.Status != delphi.Refused {
		t.Errorf("b: %+v", r)
	}
	for _, a := range f.auth {
		if a != "Bearer fw-key" {
			t.Fatalf("auth header %q", a)
		}
	}
	// The flight is found again from its label alone.
	if found, ok, err := p.Find(context.Background(), label); err != nil || !ok || found != want {
		t.Errorf("find: %q %v %v", found, ok, err)
	}
	if _, ok, err := p.Find(context.Background(), "delphi-never"); err != nil || ok {
		t.Errorf("find unknown: %v %v", ok, err)
	}
	if err := p.Cancel(context.Background(), id); err != delphi.ErrNotSupported {
		t.Errorf("cancel: %v", err)
	}
	if in, out, ok := p.Price("accounts/fireworks/models/a"); !ok || in != 0.1 || out != 0.2 {
		t.Errorf("price: %v %v %v", in, out, ok)
	}
}

func TestValidateWantsAFullModelName(t *testing.T) {
	if got := Offline().Validate(rq("a", "deepseek-v4")); len(got) != 1 || !strings.Contains(got[0], "accounts/") {
		t.Errorf("validate: %v", got)
	}
}
