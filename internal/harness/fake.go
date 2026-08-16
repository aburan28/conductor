package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// FakeDriver is a deterministic in-process harness.
//
// DESIGN.md §36.2 requires a fake harness before any model provider, and §32.3 requires
// integration tests driven by one. It exists so the expensive invariants — claim exclusivity,
// fencing, reclamation, scope drift, handoff — can be exercised end to end at full speed,
// with no API key, no network, and no vendor CLI installed.
//
// It does real work: it writes a file into the worktree, so changed-path detection, merge-risk
// edges, and evidence collection are exercised against actual git state rather than mocked.
type FakeDriver struct {
	// Behavior selects the scripted outcome, set per run through the FAKE_HARNESS_BEHAVIOR
	// environment entry in RunSpec.Env: "succeed" (default), "fail", "hang", "drift".
	Behavior string
}

func NewFakeDriver() *FakeDriver { return &FakeDriver{Behavior: "succeed"} }

func (f *FakeDriver) Name() string { return "fake" }

// FakeHarness is the harness name the fake driver registers under.
const FakeHarness = "fake"

// FakeProfiles returns synthetic model profiles for the fake harness.
//
// The router resolves an alias to a concrete model profile before dispatch, so a harness with
// no profiles is undispatchable — correct for a real vendor, wrong for the fake, which needs
// no model at all. These are supplied in memory rather than written to the catalog: a dry run
// should leave nothing behind for an operator to clean up.
//
// Capabilities are deliberately generous so the fake satisfies any alias's capability floor;
// its whole purpose is to let coordination be tested without a model in the loop.
func FakeProfiles(orgID domain.ID, aliases []string) []domain.ModelProfile {
	if len(aliases) == 0 {
		aliases = []string{"worker.fast", "worker.general", "planner.frontier",
			"verifier.cheap", "reviewer.strong"}
	}
	capabilities := []string{
		"code_edit", "tests", "multi_file", "read_diff", "test_analysis",
		"architecture", "long_context", "strong_tool_use", "code_review",
	}
	out := make([]domain.ModelProfile, 0, len(aliases))
	for _, alias := range aliases {
		out = append(out, domain.ModelProfile{
			OrganizationID:  orgID,
			Alias:           alias,
			Harness:         FakeHarness,
			Provider:        "builtin",
			Model:           "fake/deterministic",
			ReasoningEffort: domain.EffortLow,
			Capabilities:    capabilities,
			Tier:            domain.TierT4,
			Billing:         "capacity",
			Enabled:         true,
			CatalogVersion:  "builtin-fake",
		})
	}
	return out
}

func (f *FakeDriver) Capabilities(context.Context) Capabilities {
	return Capabilities{
		Kind: "fake", Available: true, Version: "builtin",
		SupportsMCP: true, SupportsJSON: true, SupportsEffort: true,
		Detail: "deterministic in-process harness for tests and dry runs",
	}
}

func (f *FakeDriver) Start(ctx context.Context, spec RunSpec) (Handle, error) {
	behavior := f.Behavior
	for _, kv := range spec.Env {
		if name, value, ok := strings.Cut(kv, "="); ok && name == "FAKE_HARNESS_BEHAVIOR" {
			behavior = value
		}
	}
	if behavior == "" {
		behavior = "succeed"
	}

	h := &fakeHandle{
		events:   make(chan Event, 16),
		done:     make(chan struct{}),
		cancelCh: make(chan struct{}),
		started:  time.Now(),
		behavior: behavior,
		spec:     spec,
	}
	go h.run(ctx)
	return h, nil
}

type fakeHandle struct {
	events   chan Event
	done     chan struct{}
	cancelCh chan struct{}
	started  time.Time
	behavior string
	spec     RunSpec

	mu         sync.Mutex
	result     Result
	cancelled  bool
	closeOnce  sync.Once
	cancelOnce sync.Once
}

func (h *fakeHandle) Events() <-chan Event { return h.events }

func (h *fakeHandle) run(ctx context.Context) {
	defer close(h.events)
	defer h.closeOnce.Do(func() { close(h.done) })

	emit := func(ev Event) {
		ev.At = time.Now()
		select {
		case h.events <- ev:
		default:
		}
	}

	emit(Event{Kind: EventStarted})
	emit(Event{Kind: EventTurn, TokensIn: 1200, TokensOut: 300})
	emit(Event{Kind: EventToolUse, Tool: "Edit"})

	// Write something real so git observes a change and the rest of the pipeline — changed
	// paths, scope drift, merge risk, evidence — runs against actual state.
	touched := "CONDUCTOR_FAKE.md"
	if h.behavior == "drift" {
		// Deliberately edit outside any plausible declared scope, to exercise the
		// scope-expansion pause path.
		touched = filepath.Join("internal", "unexpected", "drift.md")
	}
	if h.spec.Cwd != "" {
		full := filepath.Join(h.spec.Cwd, touched)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err == nil {
			content := fmt.Sprintf("fake harness run for %s on branch %s at %s\n",
				h.spec.TaskRef, h.spec.Branch, h.started.UTC().Format(time.RFC3339))
			_ = os.WriteFile(full, []byte(content), 0o644)
		}
	}

	if h.behavior == "hang" {
		// Emit nothing further: this is what stall detection is supposed to notice.
		select {
		case <-ctx.Done():
		case <-h.cancelCh:
		case <-time.After(10 * time.Minute):
		}
	}

	h.mu.Lock()
	cancelled := h.cancelled
	h.mu.Unlock()

	res := Result{
		Turns:     1,
		TokensIn:  1200,
		TokensOut: 300,
		CostUSD:   0,
		Duration:  time.Since(h.started),
	}
	switch {
	case cancelled || ctx.Err() != nil:
		res.ExitCode, res.FailureClass = 130, "cancelled"
	case h.behavior == "fail":
		res.ExitCode, res.FailureClass = 1, "nonzero_exit"
		emit(Event{Kind: EventError, Note: "scripted failure"})
	default:
		res.Succeeded = true
		emit(Event{Kind: EventFinished, Turns: 1, TokensIn: 1200, TokensOut: 300})
	}

	h.mu.Lock()
	h.result = res
	h.mu.Unlock()
}

func (h *fakeHandle) Wait(ctx context.Context) (Result, error) {
	select {
	case <-h.done:
	case <-ctx.Done():
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.result, ctx.Err()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.result, nil
}

func (h *fakeHandle) Cancel(context.Context) error {
	h.mu.Lock()
	h.cancelled = true
	h.mu.Unlock()
	h.cancelOnce.Do(func() { close(h.cancelCh) })
	return nil
}
