package hosted

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/delphi"
	"github.com/aburan28/conductor/internal/delphi/compat"
	"github.com/aburan28/conductor/internal/domain"
)

// liveEndpoint is an OpenAI-compatible endpoint that answers only once release is closed.
func liveEndpoint(t *testing.T, release chan struct{}) (compat.Endpoint, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var calls atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, `{"model":"`+body["model"].(string)+`","choices":[{"message":{"content":"live answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	t.Cleanup(srv.Close)
	return compat.Endpoint{Name: "fireworks", BaseURL: srv.URL + "/v1", KeyEnv: "FIREWORKS_API_KEY", Listed: true}, &calls, &auth
}

func waitFinished(t *testing.T, f *fixture, ref string) domain.DelphiDelegation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d, err := f.store.GetDelphiDelegation(f.ctx, f.project.ID, ref)
		if err != nil {
			t.Fatal(err)
		}
		if d.State == domain.DelphiFinished {
			return d
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("delegation %s never finished", ref)
	return domain.DelphiDelegation{}
}

func TestLiveSubmitReturnsAtOnceAndHoldsTheAnswerOnlyInMemory(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	ep, calls, auth := liveEndpoint(t, release)
	f.svc.Live = &Live{Endpoints: map[string]compat.Endpoint{"fireworks": ep}}

	d, created, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Ref: "live-1", Provider: "fireworks",
		Model: "accounts/fireworks/models/glm-5p3", Text: "a prompt that must never be stored", TaskID: f.task.ID})
	if err != nil || !created {
		t.Fatalf("submit: %v %v", created, err)
	}
	// The API answered while the model is still working.
	if d.State != domain.DelphiRunning || !d.Live || d.Holder != f.svc.Live.Holder {
		t.Fatalf("submitted row: %+v", d)
	}
	if a, err := f.svc.Answer(f.ctx, f.project, "live-1"); err == nil {
		t.Fatalf("answer before the call finished: %+v", a)
	}
	close(release)
	fin := waitFinished(t, f, "live-1")
	if fin.Status != string(delphi.Succeeded) || fin.InputTokens != 7 || fin.OutputTokens != 3 || calls.Load() != 1 {
		t.Fatalf("finished row: %+v, calls %d", fin, calls.Load())
	}
	if auth.Load() != "Bearer sk-test" || f.opened[len(f.opened)-1] != "FIREWORKS_API_KEY" {
		t.Errorf("key: header %v, opened %v", auth.Load(), f.opened)
	}
	a, err := f.svc.Answer(f.ctx, f.project, "live-1")
	if err != nil || a.Text != "live answer" {
		t.Fatalf("answer: %+v %v", a, err)
	}
	// Another process (a restart, another replica) does not have it: nothing was stored.
	other := &Service{Store: f.store, Live: &Live{Endpoints: f.svc.Live.Endpoints}}
	a, err = other.Answer(f.ctx, f.project, "live-1")
	if err != nil || a.Text != "" || !strings.Contains(a.Unavailable, "never stored") {
		t.Fatalf("answer from another process: %+v %v", a, err)
	}
	b, _ := json.Marshal(fin)
	if strings.Contains(string(b), "never be stored") || strings.Contains(string(b), "live answer") {
		t.Errorf("the row carries the prompt or answer: %s", b)
	}
	// The task timeline got the usual summary.
	evs, err := f.store.ListEvents(f.ctx, f.project.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, e := range evs {
		if e.Type == "delphi.finished" && e.AggregateID == f.task.ID {
			seen = true
		}
	}
	if !seen {
		t.Error("no delphi.finished event on the task")
	}
}

func TestLiveCallCanBeCanceledAndALostHolderIsExpired(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	defer close(release)
	ep, _, _ := liveEndpoint(t, release)
	f.svc.Live = &Live{Endpoints: map[string]compat.Endpoint{"fireworks": ep}}

	if _, _, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Ref: "live-cancel", Provider: "fireworks", Model: "m", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Cancel(f.ctx, f.project, "live-cancel"); err != nil {
		t.Fatal(err)
	}
	if d := waitFinished(t, f, "live-cancel"); d.Status != string(delphi.Canceled) {
		t.Fatalf("canceled row: %+v", d)
	}

	// A holder that stops heartbeating loses its call; the loop says so instead of waiting.
	if _, _, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Ref: "live-lost", Provider: "fireworks", Model: "m", Text: "y"}); err != nil {
		t.Fatal(err)
	}
	lost, err := f.store.ExpireLiveDelphi(f.ctx, -time.Minute) // everything open counts as stale
	if err != nil {
		t.Fatal(err)
	}
	var d domain.DelphiDelegation
	for _, l := range lost {
		if l.Ref == "live-lost" {
			d = l
		}
	}
	if d.Status != string(delphi.Missing) || d.ErrorType != "lost" || !strings.Contains(d.ErrorDetail, "resubmit") {
		t.Fatalf("lost row: %+v (of %d)", d, len(lost))
	}
	// The batch loop never touches live rows.
	open, err := f.store.ListOpenDelphiDelegations(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range open {
		if o.Live {
			t.Errorf("live row %s in the batch work list", o.Ref)
		}
	}
}

func TestLiveRefusesAnEndpointTheOperatorDidNotConfigure(t *testing.T) {
	f := newFixture(t)
	f.svc.Live = &Live{Endpoints: map[string]compat.Endpoint{}}
	_, _, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Provider: "evil", Model: "m", Text: "x"})
	if err == nil {
		t.Fatal("an unconfigured endpoint was accepted")
	}
}
