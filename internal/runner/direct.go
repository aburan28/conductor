package runner

import (
	"context"
	"time"

	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// DirectBackend runs against the store in-process.
//
// This is the single-host shape of DESIGN.md §28.1, where the runner shares a machine with
// the control plane so it can reach locally-authenticated harness sessions. It skips HTTP
// entirely, which is worth roughly a millisecond per call and, more usefully, means the
// single-host setup has no network to misconfigure.
type DirectBackend struct {
	store     *db.Store
	svc       *coord.Service
	projectID domain.ID
	caller    coord.Caller
}

func NewDirectBackend(store *db.Store, svc *coord.Service, projectID domain.ID, principal domain.Principal) *DirectBackend {
	return &DirectBackend{
		store: store, svc: svc, projectID: projectID,
		caller: coord.Caller{Principal: principal, Role: domain.RoleContributor},
	}
}

func (b *DirectBackend) Snapshot(ctx context.Context) (coord.RunnerSnapshot, error) {
	return b.svc.Snapshot(ctx, b.caller, b.projectID)
}

func (b *DirectBackend) ClaimNext(ctx context.Context, req ClaimRequest) (db.ClaimResult, error) {
	project, err := b.store.GetProject(ctx, b.projectID)
	if err != nil {
		return db.ClaimResult{}, err
	}
	return b.store.ClaimNext(ctx, db.ClaimNextParams{
		ClaimParams: db.ClaimParams{
			ProjectID:        b.projectID,
			HolderPrincipal:  b.caller.Principal.ID,
			SponsorPrincipal: b.caller.Principal.ID,
			RunnerID:         req.RunnerID,
			Role:             req.Role,
			Harness:          req.Harness,
			WorkflowSHA:      req.WorkflowSHA,
			ProjectConfigSHA: req.ProjectConfigSHA,
			RouterPolicyVer:  req.RouterPolicyVer,
			LeaseTTL:         req.LeaseTTL.OrDefault(90 * time.Second),
			ScopePolicy:      config.ScopePolicyFrom(project.Config),
			AllowWarnings:    true,
		},
		MaxCandidates: req.MaxCandidates,
	})
}

func (b *DirectBackend) SetRoute(ctx context.Context, attemptID domain.ID, route coord.RouteRecord) error {
	return b.svc.SetRoute(ctx, b.caller, attemptID, route)
}

func (b *DirectBackend) SetAttemptState(ctx context.Context, attemptID domain.ID, state domain.AttemptState, branch, worktree string, markTaskRunning bool) error {
	if err := b.svc.SetAttemptState(ctx, b.caller, attemptID, state, branch, worktree); err != nil {
		return err
	}
	if !markTaskRunning {
		return nil
	}
	attempt, err := b.store.GetAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	return b.svc.MarkTaskRunning(ctx, attempt.TaskID)
}

func (b *DirectBackend) Brief(ctx context.Context, attemptID domain.ID) (coord.AttemptBrief, error) {
	return b.svc.Brief(ctx, b.caller, attemptID)
}

func (b *DirectBackend) ReportProgress(ctx context.Context, fence domain.Fence, report coord.ProgressReport) error {
	return b.svc.ReportProgress(ctx, b.caller, fence, b.projectID, report)
}

func (b *DirectBackend) ExpandScope(ctx context.Context, fence domain.Fence, scopes []domain.ScopeRequest, source domain.ReservationSource) (coord.ExpandScopeResult, error) {
	return b.svc.ExpandScope(ctx, b.caller, fence, b.projectID, scopes, source)
}

func (b *DirectBackend) Finish(ctx context.Context, req coord.FinishRequest) (coord.FinishResult, error) {
	req.ProjectID = b.projectID
	return b.svc.FinishWork(ctx, b.caller, req)
}

func (b *DirectBackend) Release(ctx context.Context, params ReleaseRequest) error {
	_, err := b.store.Release(ctx, db.ReleaseParams{
		Fence:          params.Fence,
		Reason:         params.Reason,
		NextTaskStatus: params.NextTaskStatus,
		AttemptState:   params.AttemptState,
		FailureClass:   params.FailureClass,
		FailureSummary: params.FailureSummary,
	})
	return err
}

func (b *DirectBackend) RegisterRunner(ctx context.Context, r domain.Runner) (domain.Runner, error) {
	return b.store.RegisterRunner(ctx, r)
}

func (b *DirectBackend) HeartbeatRunner(ctx context.Context, runnerID domain.ID, inFlight int) error {
	return b.store.HeartbeatRunner(ctx, runnerID, inFlight)
}
