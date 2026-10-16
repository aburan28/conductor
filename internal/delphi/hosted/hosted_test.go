package hosted

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/delphi"
	"github.com/aburan28/conductor/internal/domain"
)

// fakeProvider records what it is sent and answers from a script.
type fakeProvider struct {
	mu        sync.Mutex
	submitted []delphi.Request
	labels    map[string]string
	ended     bool
	failure   string
	submitErr error
	findable  bool
	result    func(delphi.Request) (delphi.Result, bool)
}

func (p *fakeProvider) Name() string         { return "anthropic" }
func (p *fakeProvider) DefaultModel() string { return "claude-mythos-5-1" }
func (p *fakeProvider) Limits() delphi.Limits {
	return delphi.Limits{MaxRequests: 10, MaxBytes: 1 << 20}
}
func (p *fakeProvider) Validate(delphi.Request) []string      { return nil }
func (p *fakeProvider) Price(string) (float64, float64, bool) { return 5, 25, true }
func (p *fakeProvider) Cancel(context.Context, string) error  { return nil }
func (p *fakeProvider) Poll(context.Context, string) (delphi.BatchStatus, error) {
	return delphi.BatchStatus{Ended: p.ended, Failure: p.failure}, nil
}
func (p *fakeProvider) Submit(_ context.Context, reqs []delphi.Request, label string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.submitErr != nil {
		return "", p.submitErr
	}
	p.submitted = append(p.submitted, reqs...)
	id := fmt.Sprintf("batch_%d", len(p.submitted))
	if p.labels == nil {
		p.labels = map[string]string{}
	}
	p.labels[label] = id
	return id, nil
}
func (p *fakeProvider) Results(_ context.Context, id string, emit func(delphi.Result) error) error {
	var n int
	fmt.Sscanf(id, "batch_%d", &n)
	if r, ok := p.result(p.submitted[n-1]); ok {
		return emit(r)
	}
	return nil
}
func (p *fakeProvider) Find(_ context.Context, label string) (string, bool, error) {
	if !p.findable {
		return "", false, delphi.ErrNotSupported
	}
	id, ok := p.labels[label]
	return id, ok, nil
}

type fixture struct {
	ctx     context.Context
	store   *db.Store
	project domain.Project
	alice   domain.Principal
	task    domain.Task
	prov    *fakeProvider
	svc     *Service
	opened  []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	store, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	org, err := store.CreateOrganization(ctx, "delphi-org-"+suffix, "delphi")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, db.CreateProjectParams{OrganizationID: org.ID, Slug: "app-" + suffix,
		DisplayName: "app", RepoPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	alice, err := store.CreatePrincipal(ctx, org.ID, domain.PrincipalHuman, "alice-"+suffix, "Alice", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, db.CreateTaskParams{ProjectID: project.ID, CreatedBy: alice.ID, Title: "needs a second opinion"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{ctx: ctx, store: store, project: project, alice: alice, task: task}
	f.prov = &fakeProvider{result: func(r delphi.Request) (delphi.Result, bool) {
		return delphi.Result{ID: r.ID, Status: delphi.Succeeded, Text: "answer to " + r.ID,
			Usage: &delphi.Usage{InputTokens: 1000, OutputTokens: 2000}}, true
	}}
	f.svc = &Service{Store: store,
		Key: func(_ context.Context, p domain.Project, name string) (string, error) {
			f.opened = append(f.opened, name)
			if p.ID != project.ID {
				return "", errors.New("wrong project")
			}
			return "sk-test", nil
		},
		NewProvider: func(kind, key string) (delphi.Provider, error) {
			if key != "sk-test" {
				return nil, errors.New("wrong key")
			}
			return f.prov, nil
		},
		Now: func() time.Time { return time.Now().Add(2 * time.Minute) },
	}
	return f
}

func TestSubmitTickAnswerRoundTrip(t *testing.T) {
	f := newFixture(t)
	d, created, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Ref: "q1", Text: "Is this sound?",
		Effort: "high", TaskID: f.task.ID, Labels: map[string]string{"run": "7"}})
	if err != nil || !created || d.State != domain.DelphiRunning || d.BatchID != "batch_1" {
		t.Fatalf("submit: %v %v %+v", err, created, d)
	}
	if f.opened[0] != "ANTHROPIC_API_KEY" || len(f.prov.submitted) != 1 || f.prov.submitted[0].Effort != "high" ||
		f.prov.submitted[0].Model != "claude-mythos-5-1" {
		t.Fatalf("provider call: opened %v, sent %+v", f.opened, f.prov.submitted)
	}
	// A retry of the same request is a no-op; a different body under the ref is refused.
	if _, created, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Ref: "q1", Text: "Is this sound?",
		Effort: "high", TaskID: f.task.ID, Labels: map[string]string{"run": "7"}}); err != nil || created {
		t.Fatalf("idempotent resubmit: %v %v", err, created)
	}
	if _, _, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Ref: "q1", Text: "something else"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("conflicting resubmit: %v", err)
	}
	if len(f.prov.submitted) != 1 {
		t.Fatalf("resubmits reached the provider: %d", len(f.prov.submitted))
	}

	if err := f.svc.Tick(f.ctx); err != nil { // batch still running
		t.Fatal(err)
	}
	if _, err := f.svc.Answer(f.ctx, f.project, "q1"); !errors.Is(err, domain.ErrIllegalTransition) {
		t.Fatalf("answer before the batch ended: %v", err)
	}
	f.prov.ended = true
	if err := f.svc.Tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := f.store.GetDelphiDelegation(f.ctx, f.project.ID, "q1")
	if got.State != domain.DelphiFinished || got.Status != "succeeded" || got.OutputTokens != 2000 || got.CostUSD != 0.055 {
		t.Fatalf("finished row: %+v", got)
	}
	a, err := f.svc.Answer(f.ctx, f.project, "q1")
	if err != nil || a.Text != "answer to q1" {
		t.Fatalf("answer: %v %+v", err, a)
	}

	// Spend lands in the usage ledger as a delegation, priced at the batch rate.
	rows, err := f.store.QueryUsage(f.ctx, db.UsageQuery{ProjectID: f.project.ID, Since: time.Now().Add(-24 * time.Hour),
		Until: time.Now().Add(24 * time.Hour), By: []string{"source"}})
	if err != nil || len(rows) != 1 || rows[0].Source != "delegation" {
		t.Fatalf("usage: %v %+v", err, rows)
	}
	// The linked task's timeline says it finished, and carries no answer text.
	events, err := f.store.ListEvents(f.ctx, f.project.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var ev *domain.Event
	for i := range events {
		if events[i].Type == "delphi.finished" {
			ev = &events[i]
		}
	}
	if ev == nil || ev.AggregateID != f.task.ID || ev.Payload["status"] != "succeeded" {
		t.Fatalf("task event: %+v", ev)
	}
	for _, v := range ev.Payload {
		if s, ok := v.(string); ok && strings.Contains(s, "answer to") {
			t.Fatalf("the event carries answer text: %v", ev.Payload)
		}
	}
}

func TestSubmitFailures(t *testing.T) {
	f := newFixture(t)
	// Never connected: nothing was created, the ref is free again.
	f.prov.submitErr = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	if _, _, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Ref: "q", Text: "x"}); !errors.Is(err, domain.ErrCapacity) {
		t.Fatalf("dial failure: %v", err)
	}
	if _, err := f.store.GetDelphiDelegation(f.ctx, f.project.ID, "q"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("row left after a submit that never connected: %v", err)
	}
	// Outcome unknown: the row stays submitting and is never resent.
	f.prov.submitErr = errors.New("connection reset by peer")
	d, created, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Ref: "q", Text: "x"})
	if err != nil || !created || d.State != domain.DelphiSubmitting {
		t.Fatalf("ambiguous submit: %v %+v", err, d)
	}
	f.prov.submitErr = nil
	if err := f.svc.Tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.prov.submitted) != 0 {
		t.Fatal("an ambiguous submit was resent")
	}
	if got, _ := f.store.GetDelphiDelegation(f.ctx, f.project.ID, "q"); got.State != domain.DelphiSubmitting {
		t.Fatalf("anthropic-style provider cannot find it; it must wait for reconcile: %s", got.State)
	}
	if got, err := f.svc.Reconcile(f.ctx, f.project, "q", "", true); err != nil || got.State != domain.DelphiAbandoned {
		t.Fatalf("abandon: %v %+v", err, got)
	}
	// Invalid input never reaches the provider.
	if _, _, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Text: "x", Params: map[string]any{"temperature": 0}}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("validation: %v", err)
	}
	if _, _, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Text: "x", Labels: map[string]string{"k": strings.Repeat("v", 300)}}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("oversized label: %v", err)
	}
}

func TestFindRecoversAndBatchFailure(t *testing.T) {
	f := newFixture(t)
	f.prov.findable = true
	f.prov.submitErr = errors.New("timeout after the request was sent")
	d, _, _ := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Ref: "q", Text: "x"})
	// The batch was in fact created; the loop finds it by label.
	f.prov.submitErr = nil
	f.prov.Submit(f.ctx, []delphi.Request{{ID: "q"}}, d.Label)
	f.prov.ended, f.prov.failure = true, "batch rejected: bad model"
	if err := f.svc.Tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := f.store.GetDelphiDelegation(f.ctx, f.project.ID, "q")
	if got.State != domain.DelphiFinished || got.Status != "errored" || got.ErrorType != "batch_failed" || got.BatchID != "batch_1" {
		t.Fatalf("recovered + failed: %+v", got)
	}
}

func TestMissingKeyIsActionable(t *testing.T) {
	f := newFixture(t)
	f.svc.Key = func(context.Context, domain.Project, string) (string, error) { return "", domain.ErrNotFound }
	_, _, err := f.svc.Submit(f.ctx, f.project, f.alice.ID, SubmitRequest{Text: "x", Provider: "openai", Model: "gpt-x"})
	if !errors.Is(err, domain.ErrInvalidArgument) || !strings.Contains(err.Error(), "conductor secrets set OPENAI_API_KEY") {
		t.Fatalf("missing key: %v", err)
	}
}
