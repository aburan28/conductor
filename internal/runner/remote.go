package runner

import (
	"context"
	"errors"
	"net/http"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// RemoteBackend reaches the control plane over HTTP.
//
// This is the deployment shape of DESIGN.md §28.2: a developer laptop or CI box connects
// outbound only, exposes no inbound port, and — the point of the exercise — holds no database
// credential. Without this, giving a coworker an autonomous runner meant giving them
// Postgres access, which would make the privacy model decorative: anyone who can read the
// database can read every row in it.
type RemoteBackend struct {
	api       *client.Client
	projectID string
}

func NewRemoteBackend(api *client.Client, projectRef string) *RemoteBackend {
	return &RemoteBackend{api: api, projectID: projectRef}
}

func (b *RemoteBackend) path(suffix string) string {
	return "/v1/projects/" + b.projectID + suffix
}

func (b *RemoteBackend) Snapshot(ctx context.Context) (coord.RunnerSnapshot, error) {
	var snap coord.RunnerSnapshot
	err := b.api.Get(ctx, b.path("/runner/snapshot"), &snap)
	return snap, remoteErr(err)
}

func (b *RemoteBackend) ClaimNext(ctx context.Context, req ClaimRequest) (db.ClaimResult, error) {
	var out db.ClaimResult
	err := b.api.Post(ctx, b.path("/claim-next"), map[string]any{
		"harness":   req.Harness,
		"runner_id": req.RunnerID,
		"role":      req.Role,
	}, &out)
	return out, remoteErr(err)
}

func (b *RemoteBackend) SetRoute(ctx context.Context, attemptID domain.ID, route coord.RouteRecord) error {
	return remoteErr(b.api.Post(ctx, "/v1/attempts/"+attemptID+"/route", route, nil))
}

func (b *RemoteBackend) SetAttemptState(ctx context.Context, attemptID domain.ID, state domain.AttemptState, branch, worktree string, markTaskRunning bool) error {
	return remoteErr(b.api.Post(ctx, "/v1/attempts/"+attemptID+"/state", map[string]any{
		"state": state, "branch": branch, "worktree_path": worktree,
		"mark_task_running": markTaskRunning,
	}, nil))
}

func (b *RemoteBackend) Brief(ctx context.Context, attemptID domain.ID) (coord.AttemptBrief, error) {
	var brief coord.AttemptBrief
	err := b.api.Get(ctx, "/v1/attempts/"+attemptID+"/brief", &brief)
	return brief, remoteErr(err)
}

func (b *RemoteBackend) ReportProgress(ctx context.Context, fence domain.Fence, report coord.ProgressReport) error {
	return remoteErr(b.api.Post(ctx, "/v1/attempts/"+fence.AttemptID+"/progress", map[string]any{
		"task_id": fence.TaskID, "lease_id": fence.LeaseID,
		"fencing_epoch": fence.FencingEpoch,
		"phase":         report.Phase, "summary": report.Summary,
		"changed_paths": report.ChangedPaths, "blocker": report.Blocker,
		"tokens_in": report.TokensIn, "tokens_out": report.TokensOut,
		"cost_usd": report.CostUSD, "turns": report.Turns,
	}, nil))
}

func (b *RemoteBackend) ExpandScope(ctx context.Context, fence domain.Fence, scopes []domain.ScopeRequest, source domain.ReservationSource) (coord.ExpandScopeResult, error) {
	var out coord.ExpandScopeResult
	err := b.api.Post(ctx, "/v1/tasks/"+fence.TaskID+"/scopes", map[string]any{
		"attempt_id": fence.AttemptID, "lease_id": fence.LeaseID,
		"fencing_epoch": fence.FencingEpoch,
		"scopes":        scopes, "source": source,
	}, &out)

	// A blocked expansion is an answer, not a transport failure: the body carries who holds
	// the contested resource, and the runner acts on it.
	var apiErr *client.APIError
	if err != nil && errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		if out.Outcome == "" {
			out.Outcome = domain.OutcomeBlockConflict
		}
		return out, nil
	}
	return out, remoteErr(err)
}

func (b *RemoteBackend) Finish(ctx context.Context, req coord.FinishRequest) (coord.FinishResult, error) {
	var out coord.FinishResult
	err := b.api.Post(ctx, "/v1/attempts/"+req.Fence.AttemptID+"/result", map[string]any{
		"task_id": req.Fence.TaskID, "lease_id": req.Fence.LeaseID,
		"fencing_epoch": req.Fence.FencingEpoch,
		"outcome":       req.Outcome, "failure_class": req.FailureClass,
		"failure_summary": req.FailureSummary,
		"evidence":        req.Evidence, "runner_id": req.RunnerID,
	}, &out)
	return out, remoteErr(err)
}

func (b *RemoteBackend) Release(ctx context.Context, params ReleaseRequest) error {
	return remoteErr(b.api.Post(ctx, "/v1/tasks/"+params.Fence.TaskID+"/release", map[string]any{
		"attempt_id": params.Fence.AttemptID, "lease_id": params.Fence.LeaseID,
		"fencing_epoch": params.Fence.FencingEpoch,
		"reason":        params.Reason, "next_status": params.NextTaskStatus,
		"attempt_state": params.AttemptState, "failure_class": params.FailureClass,
		"failure_summary": params.FailureSummary,
	}, nil))
}

func (b *RemoteBackend) RegisterRunner(ctx context.Context, r domain.Runner) (domain.Runner, error) {
	var out domain.Runner
	err := b.api.Post(ctx, "/v1/runners/register", map[string]any{
		"name": r.Name, "project_id": b.projectID,
		"capabilities": r.Capabilities, "max_concurrency": r.MaxConcurrency,
	}, &out)
	return out, remoteErr(err)
}

func (b *RemoteBackend) HeartbeatRunner(ctx context.Context, runnerID domain.ID, inFlight int) error {
	return remoteErr(b.api.Post(ctx, "/v1/runners/"+runnerID+"/heartbeat",
		map[string]any{"in_flight": inFlight}, nil))
}

// remoteErr maps HTTP failures back onto the domain sentinels the runner already branches on,
// so the same control flow works over either backend. Without this, "the queue is empty"
// would arrive as a 404 that the runner treats as a hard error and logs on every poll.
func remoteErr(err error) error {
	var apiErr *client.APIError
	if err == nil || !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "not_found":
		return domain.ErrNotFound
	case "already_claimed":
		return domain.ErrAlreadyClaimed
	case "scope_conflict":
		return domain.ErrConflict
	case "stale_fencing":
		return domain.ErrStaleFencing
	case "lease_not_held":
		return domain.ErrLeaseNotHeld
	case "illegal_transition":
		return domain.ErrIllegalTransition
	case "forbidden":
		return domain.ErrNotPermitted
	case "unauthenticated":
		return domain.ErrUnauthenticated
	case "no_capacity":
		return domain.ErrCapacity
	case "budget_exhausted":
		return domain.ErrBudgetExhausted
	}
	if apiErr.Status == http.StatusNotFound {
		return domain.ErrNotFound
	}
	return err
}
