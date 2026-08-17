package coord

import (
	"context"
	"errors"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/taskcard"
)

// Service operations a runner needs.
//
// These exist so the runner has one code path whether it reaches the control plane in-process
// or over HTTP: the API handlers call exactly these methods, and the direct backend calls
// them too. Anything a runner could decide differently depending on how it connected would be
// a second source of truth, which is the thing the whole design is trying to avoid.

// RunnerSnapshot is everything a runner needs to decide whether and how to dispatch, in one
// round trip. Five separate calls per poll would make an HTTP runner unusable over a link
// with real latency.
type RunnerSnapshot struct {
	Project        domain.Project        `json:"project"`
	Profiles       []domain.ModelProfile `json:"model_profiles"`
	SpentUSD       float64               `json:"spent_usd"`
	ActiveAttempts int                   `json:"active_attempts"`
	WorkflowBody   string                `json:"workflow_body"`
}

// Snapshot builds the runner's view of a project.
func (s *Service) Snapshot(ctx context.Context, c Caller, projectID domain.ID) (RunnerSnapshot, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return RunnerSnapshot{}, err
	}
	profiles, err := s.Store.ListModelProfiles(ctx, project.OrganizationID)
	if err != nil {
		return RunnerSnapshot{}, err
	}
	spent, err := s.Store.SpendSince(ctx, project.ID, "30 days")
	if err != nil {
		return RunnerSnapshot{}, err
	}
	active, err := s.Store.CountActiveAttempts(ctx, project.ID, "")
	if err != nil {
		return RunnerSnapshot{}, err
	}

	snap := RunnerSnapshot{
		Project: project, Profiles: profiles, SpentUSD: spent, ActiveAttempts: active,
	}
	if wf, err := s.Store.GetWorkflow(ctx, project.ID, project.WorkflowSHA); err == nil {
		snap.WorkflowBody = wf.Content
	} else if !errors.Is(err, domain.ErrNotFound) {
		return RunnerSnapshot{}, err
	}
	return snap, nil
}

// AttemptBrief is the complete agent-facing input for one attempt.
//
// Rendering happens here rather than in the runner so that an HTTP runner and an in-process
// one produce byte-identical instructions, and so the privacy rules that govern a task card
// are applied in one place.
type AttemptBrief struct {
	Instruction string `json:"instruction"`
	Card        string `json:"card"`
	TaskRef     string `json:"task_ref"`
	Branch      string `json:"branch"`
	BaseSHA     string `json:"base_sha"`
}

// Brief assembles the task card, workflow rules, and any handoff bundle for an attempt.
func (s *Service) Brief(ctx context.Context, c Caller, attemptID domain.ID) (AttemptBrief, error) {
	attempt, err := s.Store.GetAttempt(ctx, attemptID)
	if err != nil {
		return AttemptBrief{}, err
	}
	task, err := s.Store.GetTask(ctx, attempt.TaskID)
	if err != nil {
		return AttemptBrief{}, err
	}
	project, err := s.Store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return AttemptBrief{}, err
	}
	owner, err := s.Store.GetPrincipal(ctx, task.CreatedBy)
	if err != nil {
		return AttemptBrief{}, err
	}
	reservations, err := s.Store.ReservationsForTask(ctx, task.ID)
	if err != nil {
		return AttemptBrief{}, err
	}

	var lease *domain.Lease
	if l, err := s.Store.ActiveLeaseForTask(ctx, task.ID); err == nil {
		lease = &l
	} else if !errors.Is(err, domain.ErrNotFound) {
		return AttemptBrief{}, err
	}

	card := taskcard.FromTask(task, project.Slug, owner.Handle, &attempt, lease,
		reservations, task.DependsOn, project.Config.RequiredChecks)

	var workflowBody string
	if wf, err := s.Store.GetWorkflow(ctx, project.ID, project.WorkflowSHA); err == nil {
		workflowBody = wf.Content
	} else if !errors.Is(err, domain.ErrNotFound) {
		return AttemptBrief{}, err
	}

	var handoff *domain.HandoffBundle
	if h, err := s.Store.LatestHandoff(ctx, task.ID); err == nil {
		handoff = &h.Bundle
	} else if !errors.Is(err, domain.ErrNotFound) {
		return AttemptBrief{}, err
	}

	instruction, err := taskcard.Instruction(card, workflowBody, handoff)
	if err != nil {
		return AttemptBrief{}, err
	}
	rendered, err := card.Render()
	if err != nil {
		return AttemptBrief{}, err
	}

	return AttemptBrief{
		Instruction: instruction,
		Card:        rendered,
		TaskRef:     task.Ref,
		Branch:      attempt.Branch,
		BaseSHA:     attempt.BaseCommitSHA,
	}, nil
}

// RouteRecord is a routing decision applied to an attempt, plus the rationale that produced
// it. They are recorded together so the policy-audit view can always explain a route.
type RouteRecord struct {
	ProfileID       domain.ID     `json:"profile_id,omitempty"`
	Alias           string        `json:"alias"`
	Model           string        `json:"model"`
	Harness         string        `json:"harness"`
	Effort          domain.Effort `json:"reasoning_effort"`
	RouterPolicyVer string        `json:"router_policy_version,omitempty"`
	ModelCatalogVer string        `json:"model_catalog_version,omitempty"`
	WorktreePath    string        `json:"worktree_path,omitempty"`
	Branch          string        `json:"branch,omitempty"`
	BaseCommitSHA   string        `json:"base_commit_sha,omitempty"`
	RunnerID        domain.ID     `json:"runner_id,omitempty"`
	Tier            string        `json:"tier,omitempty"`
	Rationale       []string      `json:"rationale,omitempty"`
}

// SetRoute records the resolved model for an attempt.
func (s *Service) SetRoute(ctx context.Context, c Caller, attemptID domain.ID, r RouteRecord) error {
	attempt, err := s.Store.SetAttemptRoute(ctx, attemptID, db.AttemptRoute{
		ProfileID: r.ProfileID, Alias: r.Alias, Model: r.Model, Harness: r.Harness,
		Effort: r.Effort, RouterPolicyVer: r.RouterPolicyVer, ModelCatalogVer: r.ModelCatalogVer,
		WorktreePath: r.WorktreePath, Branch: r.Branch, BaseCommitSHA: r.BaseCommitSHA,
		RunnerID: r.RunnerID,
	})
	if err != nil {
		return err
	}
	return s.Store.RecordDecision(ctx, domain.PolicyDecision{
		ProjectID: attempt.ProjectID, TaskID: attempt.TaskID, AttemptID: attempt.ID,
		Kind: "route", Decision: r.Alias + " -> " + r.Model,
		Rationale: map[string]any{
			"tier": r.Tier, "effort": string(r.Effort),
			"harness": r.Harness, "why": r.Rationale,
		},
	})
}

// SetAttemptState advances an attempt through its lifecycle states.
func (s *Service) SetAttemptState(ctx context.Context, c Caller, attemptID domain.ID, state domain.AttemptState, branch, worktree string) error {
	_, err := s.Store.UpdateAttempt(ctx, attemptID, db.AttemptProgress{
		State: state, Branch: branch, WorktreePath: worktree,
	})
	return err
}

// MarkTaskRunning moves a claimed task to running once its harness has started. Tolerates an
// illegal transition, because the task may already have moved on.
func (s *Service) MarkTaskRunning(ctx context.Context, taskID domain.ID) error {
	_, err := s.Store.UpdateTaskStatus(ctx, taskID, domain.TaskRunning)
	if err != nil && errors.Is(err, domain.ErrIllegalTransition) {
		return nil
	}
	return err
}
