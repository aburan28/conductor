package delphi_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/delphi"
)

// fakeProvider is an in-memory batch API.
type fakeProvider struct {
	mu        sync.Mutex
	name      string
	submits   [][]delphi.Request
	labels    map[string]string // label -> batch id, for Find
	findable  bool
	ended     map[string]bool
	failure   map[string]string
	answer    func(delphi.Request, int) (delphi.Result, bool) // (result, present)
	submitErr error
	limits    delphi.Limits
}

func newFake(name string) *fakeProvider {
	return &fakeProvider{name: name, labels: map[string]string{}, ended: map[string]bool{}, failure: map[string]string{},
		limits: delphi.Limits{MaxRequests: 1000, MaxBytes: 1 << 20},
		answer: func(r delphi.Request, _ int) (delphi.Result, bool) {
			return delphi.Result{ID: r.ID, Status: delphi.Succeeded, Text: "answer to " + r.ID, Usage: &delphi.Usage{InputTokens: 10, OutputTokens: 20}}, true
		}}
}

func (p *fakeProvider) Name() string                          { return p.name }
func (p *fakeProvider) DefaultModel() string                  { return "m1" }
func (p *fakeProvider) Limits() delphi.Limits                 { return p.limits }
func (p *fakeProvider) Validate(delphi.Request) []string      { return nil }
func (p *fakeProvider) Price(string) (float64, float64, bool) { return 1, 2, true }
func (p *fakeProvider) Submit(_ context.Context, reqs []delphi.Request, label string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.submitErr != nil {
		return "", p.submitErr
	}
	p.submits = append(p.submits, reqs)
	id := fmt.Sprintf("b%d", len(p.submits))
	p.labels[label] = id
	return id, nil
}
func (p *fakeProvider) Poll(_ context.Context, id string) (delphi.BatchStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return delphi.BatchStatus{Ended: p.ended[id], State: map[bool]string{true: "ended", false: "running"}[p.ended[id]], Failure: p.failure[id]}, nil
}
func (p *fakeProvider) Results(_ context.Context, id string, emit func(delphi.Result) error) error {
	var n int
	fmt.Sscanf(id, "b%d", &n)
	for _, r := range p.submits[n-1] {
		if res, ok := p.answer(r, r.Attempt); ok {
			if err := emit(res); err != nil {
				return err
			}
		}
	}
	return nil
}
func (p *fakeProvider) Cancel(context.Context, string) error { return nil }
func (p *fakeProvider) Find(_ context.Context, label string) (string, bool, error) {
	if !p.findable {
		return "", false, delphi.ErrNotSupported
	}
	id, ok := p.labels[label]
	return id, ok, nil
}
func (p *fakeProvider) endAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.submits {
		p.ended[fmt.Sprintf("b%d", i+1)] = true
	}
}

type statusErr struct{ code int }

func (e statusErr) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

func setup(t *testing.T, provs ...*fakeProvider) (*delphi.Engine, *delphi.FileStore, *time.Time) {
	t.Helper()
	st, err := delphi.OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := delphi.Registry{}
	for _, p := range provs {
		reg[p.name] = p
	}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	e := &delphi.Engine{Store: st, Providers: reg, MaxAttempts: 3, Now: func() time.Time { return now },
		StatusOf: func(err error) int {
			var se statusErr
			if errors.As(err, &se) {
				return se.code
			}
			return 0
		}}
	return e, st, &now
}

func reqs(provider string, ids ...string) []delphi.Request {
	var out []delphi.Request
	for _, id := range ids {
		out = append(out, delphi.Request{ID: id, Provider: provider, Model: "m1", MaxTokens: 100,
			Messages: []delphi.Message{{Role: "user", Content: "q " + id}}, Meta: map[string]string{"task": "T-" + id}})
	}
	return out
}

func tick(t *testing.T, e *delphi.Engine, force bool) delphi.TickReport {
	t.Helper()
	rep, err := e.Tick(context.Background(), force)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestFlushPolicyAndRoundTrip(t *testing.T) {
	p := newFake("p")
	e, st, now := setup(t, p)
	e.FlushEvery, e.FlushAt = 10*time.Minute, 3
	var hooked []string
	e.OnResult = func(r delphi.Result) { hooked = append(hooked, r.ID) }
	st.Enqueue(reqs("p", "a", "b"), *now)
	if rep := tick(t, e, false); rep.Submitted != 0 {
		t.Fatal("flushed before the policy allowed")
	}
	*now = now.Add(11 * time.Minute) // age trigger
	if rep := tick(t, e, false); rep.Submitted != 2 || len(p.submits) != 1 {
		t.Fatalf("age trigger: %+v", rep)
	}
	st.Enqueue(reqs("p", "c", "d", "e"), *now) // count trigger
	if rep := tick(t, e, false); rep.Submitted != 3 {
		t.Fatalf("count trigger: %+v", rep)
	}
	tick(t, e, false) // batches still running: nothing collected
	p.endAll()
	if rep := tick(t, e, false); rep.Finished != 5 || rep.Collected != 2 {
		t.Fatalf("collect: %+v", rep)
	}
	r, ok, _ := st.Result("c")
	if !ok || r.Text != "answer to c" || r.Meta["task"] != "T-c" || r.BatchID != "b2" || r.Attempt != 1 || r.Provider != "p" {
		t.Fatalf("result: %+v", r)
	}
	if len(hooked) != 5 {
		t.Fatalf("hook calls: %v", hooked)
	}
	if fl, _ := st.Flights(); len(fl) != 0 {
		t.Fatalf("flights left: %v", fl)
	}
}

func TestRetriesUpToMaxAttempts(t *testing.T) {
	p := newFake("p")
	p.answer = func(r delphi.Request, attempt int) (delphi.Result, bool) {
		switch r.ID {
		case "flaky": // expires once, then succeeds
			if attempt == 0 {
				return delphi.Result{ID: r.ID, Status: delphi.Expired}, true
			}
			return delphi.Result{ID: r.ID, Status: delphi.Succeeded, Text: "ok"}, true
		case "lost":
			return delphi.Result{}, false // never returned: missing
		case "bad":
			return delphi.Result{ID: r.ID, Status: delphi.Errored, Error: &delphi.Error{Type: "invalid_request_error"}}, true
		case "refused":
			return delphi.Result{ID: r.ID, Status: delphi.Refused}, true
		}
		return delphi.Result{ID: r.ID, Status: delphi.Succeeded}, true
	}
	e, st, now := setup(t, p)
	st.Enqueue(reqs("p", "flaky", "lost", "bad", "refused"), *now)
	for i := 0; i < 5; i++ {
		tick(t, e, true)
		p.endAll()
	}
	want := map[string]struct {
		s       delphi.Status
		attempt int
	}{"flaky": {delphi.Succeeded, 2}, "lost": {delphi.Missing, 3}, "bad": {delphi.Errored, 1}, "refused": {delphi.Refused, 1}}
	for id, w := range want {
		r, ok, _ := st.Result(id)
		if !ok || r.Status != w.s || r.Attempt != w.attempt {
			t.Errorf("%s: %+v (ok=%v), want %v attempt %d", id, r, ok, w.s, w.attempt)
		}
	}
	if len(p.submits) != 3 {
		t.Errorf("submits: %d, want 3 (initial, retry of flaky+lost, retry of lost)", len(p.submits))
	}
}

func TestWholeBatchFailureIsFinal(t *testing.T) {
	p := newFake("p")
	e, st, now := setup(t, p)
	st.Enqueue(reqs("p", "a"), *now)
	tick(t, e, true)
	p.failure["b1"] = "batch rejected: line 1: bad model"
	p.endAll()
	tick(t, e, true)
	r, _, _ := st.Result("a")
	if r.Status != delphi.Errored || r.Error.Type != "batch_failed" || r.Retryable() || len(p.submits) != 1 {
		t.Fatalf("%+v submits=%d", r, len(p.submits))
	}
}

func TestSubmitErrors(t *testing.T) {
	p := newFake("p")
	e, st, now := setup(t, p)
	st.Enqueue(reqs("p", "a"), *now)

	p.submitErr = statusErr{529} // provider overloaded: stays queued
	tick(t, e, true)
	if q, _ := st.Pending(); len(q) != 1 {
		t.Fatalf("5xx: queue %v", q)
	}
	p.submitErr = statusErr{400} // rejected outright: finished as an error
	tick(t, e, true)
	if r, ok, _ := st.Result("a"); !ok || r.Status != delphi.Errored || r.Error.Type != "submit_http_400" {
		t.Fatalf("4xx: %+v", r)
	}

	st.Enqueue(reqs("p", "b"), *now)
	p.submitErr = errors.New("connection reset after the request was sent") // outcome unknown
	rep := tick(t, e, true)
	if len(rep.Stuck) != 1 {
		t.Fatalf("ambiguous: %+v", rep)
	}
	p.submitErr = nil
	for i := 0; i < 3; i++ { // never resubmitted while the outcome is unknown
		tick(t, e, true)
	}
	if len(p.submits) != 0 {
		t.Fatalf("resubmitted a request whose first submit had an unknown outcome")
	}
	if err := delphi.Reconcile(st, rep.Stuck[0], "", true); err != nil {
		t.Fatal(err)
	}
	tick(t, e, true)
	if len(p.submits) != 1 {
		t.Fatalf("requeue after reconcile: %d submits", len(p.submits))
	}
}

func TestFindRecoversUnknownSubmit(t *testing.T) {
	p := newFake("p")
	p.findable = true
	e, st, now := setup(t, p)
	st.Enqueue(reqs("p", "a"), *now)
	// Simulate a crash after the provider accepted the batch but before the
	// flight recorded its id.
	pending, _ := st.Pending()
	f := delphi.Flight{Label: "L1", Provider: "p", State: delphi.FlightSubmitting, SubmittedAt: *now, Requests: []delphi.Request{pending[0].Request}}
	st.BeginFlight(f)
	p.Submit(context.Background(), f.Requests, "L1")
	p.endAll()
	tick(t, e, false)
	if r, ok, _ := st.Result("a"); !ok || r.Status != delphi.Succeeded || len(p.submits) != 1 {
		t.Fatalf("recovery: %+v submits=%d", r, len(p.submits))
	}
}

func TestCrashInsideBeginFlightNeverDoubleSubmits(t *testing.T) {
	p := newFake("p")
	e, st, now := setup(t, p)
	st.Enqueue(reqs("p", "a"), *now)
	// Flight written, queue file not yet removed.
	f := delphi.Flight{Label: "L1", Provider: "p", State: delphi.FlightRunning, BatchID: "b1", SubmittedAt: *now, Requests: reqs("p", "a")}
	p.submits = append(p.submits, f.Requests)
	st.UpdateFlight(f)
	if q, _ := st.Pending(); len(q) != 0 {
		t.Fatalf("a request in a flight still shows as pending: %v", q)
	}
	tick(t, e, true)
	if len(p.submits) != 1 {
		t.Fatalf("submitted twice")
	}
}

func TestEnqueueIdempotence(t *testing.T) {
	_, st, now := setup(t)
	if n, err := st.Enqueue(reqs("p", "a", "b"), *now); n != 2 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := st.Enqueue(reqs("p", "a", "b"), *now); n != 0 || err != nil {
		t.Fatalf("identical re-enqueue: %d %v", n, err)
	}
	other := reqs("p", "a")
	other[0].Messages[0].Content = "a different question"
	if _, err := st.Enqueue(other, *now); err == nil || !strings.Contains(err.Error(), "different request") {
		t.Fatalf("conflicting id accepted: %v", err)
	}
}

func TestChunkingAndProviderIsolation(t *testing.T) {
	p, q := newFake("p"), newFake("q")
	p.limits.MaxRequests = 2
	e, st, now := setup(t, p, q)
	st.Enqueue(append(reqs("p", "a", "b", "c"), reqs("q", "x")...), *now)
	st.Enqueue([]delphi.Request{{ID: "z", Provider: "nokey", Model: "m", MaxTokens: 1, Messages: []delphi.Message{{Role: "user", Content: "?"}}}}, *now)
	tick(t, e, true)
	if len(p.submits) != 2 || len(q.submits) != 1 {
		t.Fatalf("p %d batches, q %d", len(p.submits), len(q.submits))
	}
	if pend, _ := st.Pending(); len(pend) != 1 || pend[0].ID != "z" {
		t.Fatalf("unconfigured provider's request should wait: %v", pend)
	}
}

func TestLock(t *testing.T) {
	_, st, _ := setup(t)
	unlock, err := st.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Lock(); err == nil {
		t.Fatal("second loop got the lock")
	}
	unlock()
	if u, err := st.Lock(); err != nil {
		t.Fatal(err)
	} else {
		u()
	}
}
