package runner

import (
	"context"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// Backend is how the runner reaches the control plane.
//
// Two implementations exist: one that calls the service in-process (single host, DESIGN.md
// §28.1) and one that speaks HTTP (§28.2 — a developer laptop or CI box connects outbound and
// never holds a database credential). The interface is deliberately shaped around what the
// runner *does*, not around store calls, so neither implementation can drift into making
// decisions the control plane should own.
type Backend interface {
	// Snapshot returns the project's policy, model catalog, spend, and concurrency in one
	// round trip.
	Snapshot(ctx context.Context) (coord.RunnerSnapshot, error)

	// ClaimNext takes the highest-priority ready task, or returns domain.ErrNotFound when
	// the queue is empty.
	ClaimNext(ctx context.Context, req ClaimRequest) (db.ClaimResult, error)

	// SetRoute records the resolved model and workspace for an attempt.
	SetRoute(ctx context.Context, attemptID domain.ID, route coord.RouteRecord) error

	// SetAttemptState advances the attempt lifecycle.
	SetAttemptState(ctx context.Context, attemptID domain.ID, state domain.AttemptState, branch, worktree string, markTaskRunning bool) error

	// Brief renders the agent-facing instruction and task card.
	Brief(ctx context.Context, attemptID domain.ID) (coord.AttemptBrief, error)

	// ReportProgress heartbeats the lease and records observed progress.
	ReportProgress(ctx context.Context, fence domain.Fence, report coord.ProgressReport) error

	// ExpandScope requests additional territory mid-attempt.
	ExpandScope(ctx context.Context, fence domain.Fence, scopes []domain.ScopeRequest, source domain.ReservationSource) (coord.ExpandScopeResult, error)

	// Finish submits evidence and transitions the task.
	Finish(ctx context.Context, req coord.FinishRequest) (coord.FinishResult, error)

	// Release hands a task back without completing it.
	Release(ctx context.Context, params ReleaseRequest) error

	// RegisterRunner advertises this machine's capabilities and returns its id.
	RegisterRunner(ctx context.Context, r domain.Runner) (domain.Runner, error)

	// HeartbeatRunner keeps the advertised capacity fresh.
	HeartbeatRunner(ctx context.Context, runnerID domain.ID, inFlight int) error
}

// ClaimRequest asks for the next dispatchable task.
type ClaimRequest struct {
	Harness          string
	RunnerID         domain.ID
	Role             domain.AgentRole
	WorkflowSHA      string
	ProjectConfigSHA string
	RouterPolicyVer  string
	LeaseTTL         domain.Duration
	MaxCandidates    int
}

// ReleaseRequest hands a task back.
type ReleaseRequest struct {
	Fence          domain.Fence
	Reason         string
	NextTaskStatus domain.TaskStatus
	AttemptState   domain.AttemptState
	FailureClass   string
	FailureSummary string
}
