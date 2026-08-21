// Package coord is the coordination service: the logic that sits between the store and the
// two northbound interfaces (REST and MCP).
//
// Both interfaces call the same methods here. That is deliberate — DESIGN.md §7.2 requires
// the MCP gateway to be a thin translation layer with no independent state or scheduling
// logic, so anything an agent can do through MCP is exactly what a human can do through the
// CLI, with the same checks in the same order.
package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
)

// Service carries no mutable state; it is safe to share across requests.
type Service struct {
	Store *db.Store
}

func New(store *db.Store) *Service { return &Service{Store: store} }

// Caller identifies who is asking, resolved from a bearer token before any project data is
// read.
type Caller struct {
	Principal domain.Principal
	Role      domain.Role
	SessionID domain.ID
}

// Viewer builds the privacy viewer for this caller.
func (c Caller) Viewer() privacy.Viewer {
	return privacy.Viewer{PrincipalID: c.Principal.ID, Role: c.Role}
}

// Authorize resolves a caller's role in a project and checks it meets a minimum.
//
// Every project-scoped entry point begins here. Membership is verified in Go rather than
// implied by the URL, because DESIGN.md §25.6 forbids any path that reaches a row by id
// alone.
func (s *Service) Authorize(ctx context.Context, principal domain.Principal, projectID domain.ID, need domain.Role) (Caller, error) {
	role, err := s.Store.RoleIn(ctx, projectID, principal.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Do not distinguish "no such project" from "not a member".
			return Caller{}, domain.ErrNotFound
		}
		return Caller{}, err
	}
	if !role.Can(need) {
		return Caller{}, fmt.Errorf("%w: role %s cannot act as %s", domain.ErrNotPermitted, role, need)
	}
	return Caller{Principal: principal, Role: role}, nil
}

// ---------------------------------------------------------------------------
// Intent checking
// ---------------------------------------------------------------------------

// IntentRequest is the sanitized envelope of DESIGN.md §12.1.
//
// There is no prompt field, and there never will be. The caller decides what to publish as
// PublicSummary; a client that wants to share nothing sets Visibility to private and still
// participates in duplicate detection through the fingerprint alone.
type IntentRequest struct {
	ProjectID   domain.ID
	SessionID   domain.ID
	ExternalRef string
	Summary     string
	Visibility  domain.Visibility
	Scopes      []domain.ScopeRequest
	// Fingerprint, when supplied, is trusted as a client-computed value (§12.2 allows
	// client-side canonicalization). When empty the server derives one from Summary and
	// Scopes under the tenant key.
	Fingerprint string
	Verb        string
	Resource    string
	// ExcludeTask omits a task from duplicate and conflict checks — used when an existing
	// task is re-checking its own scope expansion.
	ExcludeTask domain.ID
}

// IntentDecision is the engine's answer: an action, never a bare score (DESIGN.md §6.6).
type IntentDecision struct {
	Outcome    domain.Outcome          `json:"outcome"`
	Duplicates []db.DuplicateCandidate `json:"duplicates,omitempty"`
	Conflicts  []db.ScopeConflict      `json:"conflicts,omitempty"`
	Reason     string                  `json:"reason,omitempty"`
	// Advice is human-readable guidance matching the outcome, so a CLI or agent can present
	// something actionable without reimplementing the policy.
	Advice string `json:"advice,omitempty"`
}

// CheckIntent evaluates duplicates and scope conflicts without changing anything.
//
// This is the highest-value call in the system. An agent that runs it before editing cannot
// cause a collision, and it costs one round trip.
func (s *Service) CheckIntent(ctx context.Context, c Caller, req IntentRequest) (IntentDecision, error) {
	project, err := s.Store.GetProject(ctx, req.ProjectID)
	if err != nil {
		return IntentDecision{}, err
	}

	fingerprint, signature, err := s.fingerprint(ctx, req)
	if err != nil {
		return IntentDecision{}, err
	}

	threshold := project.Config.DuplicateThreshold
	if threshold <= 0 {
		threshold = 0.50
	}
	duplicates, err := s.Store.FindDuplicates(ctx, req.ProjectID,
		fingerprint, signature, req.ExternalRef, threshold, req.ExcludeTask)
	if err != nil {
		return IntentDecision{}, err
	}
	// A private task's title must not leak through the duplicate list.
	for i := range duplicates {
		if duplicates[i].Visibility == domain.VisibilityPrivate && duplicates[i].OwnerID != c.Principal.ID {
			duplicates[i].Title = ""
		}
	}

	conflicts, err := s.Store.CheckScopes(ctx, db.CheckScopesParams{
		ProjectID:   req.ProjectID,
		ExcludeTask: req.ExcludeTask,
		Viewer:      c.Principal.ID,
		Requests:    req.Scopes,
		Policy:      config.ScopePolicyFrom(project.Config),
	})
	if err != nil {
		return IntentDecision{}, err
	}

	return decide(duplicates, conflicts, project.Config), nil
}

// decide combines duplicate and conflict evidence into a single action.
//
// Precedence matters: an exact duplicate outranks a scope conflict, because "this work
// already exists" is more useful than "this file is busy" — joining the existing task
// resolves both at once.
func decide(duplicates []db.DuplicateCandidate, conflicts []db.ScopeConflict, cfg domain.ProjectConfig) IntentDecision {
	d := IntentDecision{Outcome: domain.OutcomeAllow, Duplicates: duplicates, Conflicts: conflicts}

	for _, dup := range duplicates {
		if dup.Exact {
			if cfg.DuplicatePolicy == "allow" {
				d.Outcome = domain.OutcomeAllowWithWarning
				d.Reason = "identical intent already tracked as " + dup.TaskRef
				d.Advice = "This work is already tracked as " + dup.TaskRef +
					" (owner " + dup.Owner + "). Attach to it rather than starting a second copy."
				return d
			}
			d.Outcome = domain.OutcomeBlockDuplicate
			d.Reason = "identical intent already tracked as " + dup.TaskRef
			d.Advice = dup.TaskRef + " (owner " + dup.Owner +
				") is already this exact work. Attach to it, review it, or file a dependent task."
			return d
		}
	}

	if len(duplicates) > 0 {
		d.Outcome = domain.OutcomeSuggestJoin
		d.Reason = "similar work already in flight"
		d.Advice = duplicates[0].TaskRef + " (owner " + duplicates[0].Owner +
			") looks like the same work. Join it, or narrow your scope so the two do not overlap."
	}

	if db.Blocking(conflicts) {
		d.Outcome = domain.OutcomeBlockConflict
		blocker := conflicts[0]
		for _, c := range conflicts {
			if c.Outcome.Blocks() {
				blocker = c
				break
			}
		}
		d.Reason = "scope conflict on " + blocker.ResourceKey
		d.Advice = blocker.HolderOwner + " holds " + blocker.ResourceKey +
			" for " + blocker.HolderTaskRef + " (" + string(blocker.HolderMode) +
			"). Wait for it, split your scope, or join their task."
		return d
	}

	if len(conflicts) > 0 && d.Outcome == domain.OutcomeAllow {
		d.Outcome = domain.OutcomeAllowWithWarning
		d.Reason = "advisory overlap with " + conflicts[0].HolderTaskRef
		d.Advice = "Overlaps " + conflicts[0].HolderTaskRef + " (" +
			string(conflicts[0].HolderMode) + " by " + conflicts[0].HolderOwner +
			"). Proceed, but expect to coordinate on merge."
	}
	return d
}

// fingerprint returns the intent fingerprint and MinHash signature, computing them under the
// tenant key unless the client supplied its own fingerprint.
func (s *Service) fingerprint(ctx context.Context, req IntentRequest) (string, []int64, error) {
	key, err := s.Store.DedupeKeyForProject(ctx, req.ProjectID)
	if err != nil {
		return "", nil, err
	}
	scopes := make([]string, 0, len(req.Scopes))
	for _, sc := range req.Scopes {
		scopes = append(scopes, sc.Resource)
	}
	env := privacy.Envelope{Summary: req.Summary, Scopes: scopes, ExternalRef: req.ExternalRef}

	fp := req.Fingerprint
	if fp == "" {
		fp = env.Fingerprint(key)
	}
	return fp, env.Signature(key), nil
}

// ---------------------------------------------------------------------------
// Starting work
// ---------------------------------------------------------------------------

// StartWorkRequest implements coord_start_work (DESIGN.md §18.1).
type StartWorkRequest struct {
	IntentRequest
	Title string
	// AttachTo names an existing task to claim instead of creating a new one.
	AttachTo domain.ID
	// Force proceeds despite a duplicate suggestion, creating a deliberate alternative.
	// Speculative work is legitimate; it just has to be declared rather than accidental.
	Force              bool
	Harness            string
	HarnessVersion     string
	ModelAlias         string
	ResolvedModel      string
	ReasoningEffort    domain.Effort
	Role               domain.AgentRole
	Branch             string
	BaseSHA            string
	WorktreePath       string
	RunnerID           domain.ID
	AcceptanceCriteria []domain.AcceptanceCriterion
	Priority           int
}

// StartWorkResult is the coord_start_work response.
type StartWorkResult struct {
	Outcome      domain.Outcome          `json:"outcome"`
	Task         *privacy.TaskView       `json:"task,omitempty"`
	TaskID       domain.ID               `json:"task_id,omitempty"`
	TaskRef      string                  `json:"task_ref,omitempty"`
	AttemptID    domain.ID               `json:"attempt_id,omitempty"`
	LeaseID      domain.ID               `json:"lease_id,omitempty"`
	FencingEpoch int64                   `json:"fencing_epoch,omitempty"`
	TaskCardURI  string                  `json:"task_card_uri,omitempty"`
	ExpiresAt    *time.Time              `json:"lease_expires_at,omitempty"`
	Duplicates   []db.DuplicateCandidate `json:"duplicates,omitempty"`
	Conflicts    []db.ScopeConflict      `json:"conflicts,omitempty"`
	Advice       string                  `json:"advice,omitempty"`
}

// StartWork declares intent, checks for duplicates and conflicts, then creates or attaches to
// a task and acquires a lease when permitted.
//
// The order is the point: check first, mutate second. A blocked caller learns who holds what
// without having created a phantom task that someone else now has to clean up.
func (s *Service) StartWork(ctx context.Context, c Caller, req StartWorkRequest) (StartWorkResult, error) {
	project, err := s.Store.GetProject(ctx, req.ProjectID)
	if err != nil {
		return StartWorkResult{}, err
	}

	decision, err := s.CheckIntent(ctx, c, req.IntentRequest)
	if err != nil {
		return StartWorkResult{}, err
	}

	// Record the intent regardless of outcome. A blocked attempt is exactly the signal a
	// teammate needs to see on the presence view.
	fingerprint, signature, err := s.fingerprint(ctx, req.IntentRequest)
	if err != nil {
		return StartWorkResult{}, err
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = project.Config.DefaultVisibility
	}
	if _, err := s.Store.RecordIntent(ctx, domain.Intent{
		ProjectID:     req.ProjectID,
		SessionID:     req.SessionID,
		PrincipalID:   c.Principal.ID,
		ExternalRef:   req.ExternalRef,
		Visibility:    visibility,
		PublicSummary: req.Summary,
		Verb:          req.Verb,
		Resource:      req.Resource,
		Fingerprint:   fingerprint,
		MinHash:       signature,
		Scopes:        req.Scopes,
		Outcome:       decision.Outcome,
	}, 15*time.Minute); err != nil {
		return StartWorkResult{}, err
	}

	blocked := decision.Outcome.Blocks() ||
		(decision.Outcome == domain.OutcomeSuggestJoin && !req.Force && req.AttachTo == "")
	if blocked {
		return StartWorkResult{
			Outcome:    decision.Outcome,
			Duplicates: decision.Duplicates,
			Conflicts:  decision.Conflicts,
			Advice:     decision.Advice,
		}, nil
	}

	// Create or attach.
	taskID := req.AttachTo
	if taskID == "" {
		title := req.Title
		if title == "" {
			title = req.Summary
		}
		task, err := s.Store.CreateTask(ctx, db.CreateTaskParams{
			ProjectID:          req.ProjectID,
			CreatedBy:          c.Principal.ID,
			ExternalRef:        req.ExternalRef,
			Title:              privacy.ClampSummary(title),
			Objective:          privacy.ClampSummary(req.Summary),
			AcceptanceCriteria: req.AcceptanceCriteria,
			Status:             domain.TaskReady,
			Visibility:         visibility,
			Priority:           req.Priority,
			ModelAlias:         req.ModelAlias,
			HarnessPref:        req.Harness,
			MaxAttempts:        project.Config.MaxAttempts,
			Fingerprint:        fingerprint,
			MinHash:            signature,
			BaseSHA:            req.BaseSHA,
			WorkflowSHA:        project.WorkflowSHA,
		})
		if err != nil {
			return StartWorkResult{}, err
		}
		taskID = task.ID
	}

	claim, err := s.Store.Claim(ctx, db.ClaimParams{
		TaskID:            taskID,
		ProjectID:         req.ProjectID,
		HolderPrincipal:   c.Principal.ID,
		SponsorPrincipal:  c.Principal.ID,
		SessionID:         req.SessionID,
		RunnerID:          req.RunnerID,
		Role:              req.Role,
		Harness:           req.Harness,
		ModelAlias:        req.ModelAlias,
		ResolvedModel:     req.ResolvedModel,
		ReasoningEffort:   req.ReasoningEffort,
		Branch:            req.Branch,
		WorktreePath:      req.WorktreePath,
		BaseCommitSHA:     req.BaseSHA,
		WorkflowSHA:       project.WorkflowSHA,
		ProjectConfigSHA:  project.ConfigSHA,
		LeaseTTL:          project.Config.LeaseTTL.OrDefault(90 * time.Second),
		Scopes:            req.Scopes,
		ScopePolicy:       config.ScopePolicyFrom(project.Config),
		AllowWarnings:     true,
		MemberTokenBudget: project.Config.Budget.MemberTokens,
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrAlreadyClaimed) {
			return StartWorkResult{
				Outcome:   domain.OutcomeBlockConflict,
				Conflicts: decision.Conflicts,
				Advice:    "The task or its scopes were taken between the check and the claim. Re-check and retry.",
			}, nil
		}
		return StartWorkResult{}, err
	}

	// Claiming the work settles any offer of it. The session that was offered the task and
	// took it fulfils its own offer; anyone else claiming first cancels it, so an offer never
	// outlives the opening it was made for.
	if req.SessionID != "" {
		if err := s.Store.FulfilledByAttempt(ctx, claim.Task.ID, req.SessionID); err != nil {
			return StartWorkResult{}, err
		}
	}
	if _, err := s.Store.CancelAssignmentsForTask(ctx, claim.Task.ID, "task was claimed"); err != nil {
		return StartWorkResult{}, err
	}

	view, err := s.taskView(ctx, c, claim.Task)
	if err != nil {
		return StartWorkResult{}, err
	}

	return StartWorkResult{
		Outcome:      domain.OutcomeAllow,
		Task:         &view,
		TaskID:       claim.Task.ID,
		TaskRef:      claim.Task.Ref,
		AttemptID:    claim.Attempt.ID,
		LeaseID:      claim.Lease.ID,
		FencingEpoch: claim.Fence.FencingEpoch,
		TaskCardURI:  "coord://tasks/" + claim.Task.ID + "/card",
		ExpiresAt:    &claim.Lease.ExpiresAt,
		Conflicts:    claim.Warnings,
		Advice:       decision.Advice,
	}, nil
}

// ---------------------------------------------------------------------------
// Scope expansion
// ---------------------------------------------------------------------------

// ExpandScopeResult reports whether additional territory was granted.
type ExpandScopeResult struct {
	Outcome   domain.Outcome            `json:"outcome"`
	Granted   []domain.ScopeReservation `json:"granted,omitempty"`
	Conflicts []db.ScopeConflict        `json:"conflicts,omitempty"`
	Advice    string                    `json:"advice,omitempty"`
}

// ExpandScope implements coord_expand_scope (DESIGN.md §18.3).
//
// Scope drift is the normal case, not an error: an agent discovers mid-task that it needs an
// adjacent file. The right behaviour is to grant it when free and pause when contested, which
// is what this does.
func (s *Service) ExpandScope(ctx context.Context, c Caller, fence domain.Fence, projectID domain.ID, requests []domain.ScopeRequest, source domain.ReservationSource) (ExpandScopeResult, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return ExpandScopeResult{}, err
	}
	if err := s.Store.AssertFence(ctx, fence); err != nil {
		return ExpandScopeResult{}, err
	}

	granted, conflicts, err := s.Store.AcquireScopes(ctx, db.AcquireScopesParams{
		ProjectID:     projectID,
		TaskID:        fence.TaskID,
		AttemptID:     fence.AttemptID,
		LeaseID:       fence.LeaseID,
		PrincipalID:   c.Principal.ID,
		Requests:      requests,
		Source:        source,
		Policy:        config.ScopePolicyFrom(project.Config),
		AllowWarnings: true,
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			// Pause the attempt rather than letting it keep editing contested ground.
			if _, uerr := s.Store.UpdateAttempt(ctx, fence.AttemptID, db.AttemptProgress{
				State: domain.AttemptPausedConflict,
			}); uerr != nil && !errors.Is(uerr, domain.ErrIllegalTransition) {
				return ExpandScopeResult{}, uerr
			}
			if _, uerr := s.Store.UpdateTaskStatus(ctx, fence.TaskID, domain.TaskBlockedConflict); uerr != nil &&
				!errors.Is(uerr, domain.ErrIllegalTransition) {
				return ExpandScopeResult{}, uerr
			}
			advice := "Scope expansion blocked."
			if len(conflicts) > 0 {
				advice = conflicts[0].HolderOwner + " holds " + conflicts[0].ResourceKey +
					" for " + conflicts[0].HolderTaskRef + ". Split, wait, or join."
			}
			return ExpandScopeResult{
				Outcome:   domain.OutcomeBlockConflict,
				Conflicts: conflicts,
				Advice:    advice,
			}, nil
		}
		return ExpandScopeResult{}, err
	}

	outcome := domain.OutcomeAllow
	if len(conflicts) > 0 {
		outcome = domain.OutcomeAllowWithWarning
	}
	return ExpandScopeResult{Outcome: outcome, Granted: granted, Conflicts: conflicts}, nil
}

// ---------------------------------------------------------------------------
// Progress and completion
// ---------------------------------------------------------------------------

// ProgressReport implements coord_report_progress (DESIGN.md §18.4). Every field is
// structural; Summary is clamped so it cannot become a transcript channel.
type ProgressReport struct {
	Phase        string
	Summary      string
	PercentHint  int
	Blocker      string
	ChangedPaths []string
	TokensIn     int64
	TokensOut    int64
	CostUSD      float64
	Turns        int
}

// ReportProgress records structured progress against a leased attempt.
func (s *Service) ReportProgress(ctx context.Context, c Caller, fence domain.Fence, projectID domain.ID, r ProgressReport) error {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if _, err := s.Store.HeartbeatLease(ctx, fence,
		project.Config.LeaseTTL.OrDefault(90*time.Second)); err != nil {
		return err
	}

	state := domain.AttemptRunning
	if r.Blocker != "" {
		state = domain.AttemptPausedConflict
	}
	if _, err := s.Store.UpdateAttempt(ctx, fence.AttemptID, db.AttemptProgress{
		State:        state,
		ChangedPaths: r.ChangedPaths,
		TokensIn:     r.TokensIn,
		TokensOut:    r.TokensOut,
		CostUSD:      r.CostUSD,
		Turns:        r.Turns,
	}); err != nil && !errors.Is(err, domain.ErrIllegalTransition) {
		return err
	}

	task, err := s.Store.GetTask(ctx, fence.TaskID)
	if err != nil {
		return err
	}
	if task.Status == domain.TaskClaimed {
		if _, err := s.Store.UpdateTaskStatus(ctx, task.ID, domain.TaskRunning); err != nil &&
			!errors.Is(err, domain.ErrIllegalTransition) {
			return err
		}
	}

	return s.Store.AppendEvent(ctx, task.OrganizationID, projectID, c.Principal.ID,
		"attempt", fence.AttemptID, "attempt.progress", task.Visibility, map[string]any{
			"task_ref": task.Ref, "phase": r.Phase,
			"summary": privacy.ClampSummary(r.Summary), "percent_hint": r.PercentHint,
			"blocker": r.Blocker, "changed_paths": r.ChangedPaths,
			"tokens_in": r.TokensIn, "tokens_out": r.TokensOut, "cost_usd": r.CostUSD,
		})
}

// FinishRequest implements coord_finish_work (DESIGN.md §18.6).
type FinishRequest struct {
	Fence          domain.Fence
	ProjectID      domain.ID
	Outcome        string // succeeded | failed | cancelled | blocked
	FailureClass   string
	FailureSummary string
	Evidence       *domain.EvidenceManifest
	RunnerID       domain.ID
}

// FinishResult reports the resulting task state and any missing verification.
type FinishResult struct {
	TaskStatus    domain.TaskStatus   `json:"task_status"`
	AttemptState  domain.AttemptState `json:"attempt_state"`
	MissingChecks []string            `json:"missing_checks,omitempty"`
	Advice        string              `json:"advice,omitempty"`
}

// FinishWork validates the fence and evidence, then transitions the task.
//
// Completion requires a commit plus validation evidence (DESIGN.md §11 of the principles, and
// §15.2). A worker that says it succeeded without evidence gets sent back rather than
// marked done, because the alternative is a ledger full of unverified claims.
func (s *Service) FinishWork(ctx context.Context, c Caller, req FinishRequest) (FinishResult, error) {
	project, err := s.Store.GetProject(ctx, req.ProjectID)
	if err != nil {
		return FinishResult{}, err
	}
	if err := s.Store.AssertFence(ctx, req.Fence); err != nil {
		return FinishResult{}, err
	}

	if req.Evidence != nil {
		ev := *req.Evidence
		ev.TaskID, ev.AttemptID = req.Fence.TaskID, req.Fence.AttemptID
		ev.LeaseID, ev.FencingEpoch = req.Fence.LeaseID, req.Fence.FencingEpoch
		if err := s.Store.SubmitEvidence(ctx, ev, req.RunnerID); err != nil {
			return FinishResult{}, err
		}
	}

	var attemptState domain.AttemptState
	var nextStatus domain.TaskStatus
	var missing []string

	switch req.Outcome {
	case "succeeded":
		ok, m, err := s.Store.RequiredChecksPassed(ctx, req.Fence.AttemptID, project.Config.RequiredChecks)
		if err != nil {
			return FinishResult{}, err
		}
		missing = m
		if !ok {
			// Not a failure — the work may be fine, but it is not verified. Hand it back
			// with a specific reason rather than accepting an unproven success.
			return FinishResult{
				TaskStatus:    domain.TaskRunning,
				AttemptState:  domain.AttemptRunning,
				MissingChecks: missing,
				Advice: "Required checks have no passing result yet: " +
					joinComma(missing) + ". Run them and resubmit evidence.",
			}, nil
		}
		attemptState, nextStatus = domain.AttemptSucceeded, domain.TaskVerifying
	case "failed":
		attemptState = domain.AttemptFailedRetryable
		nextStatus = ""
	case "cancelled":
		attemptState, nextStatus = domain.AttemptCancelled, domain.TaskCancelled
	case "blocked":
		attemptState, nextStatus = domain.AttemptPausedConflict, domain.TaskBlockedInput
	default:
		return FinishResult{}, fmt.Errorf("%w: outcome=%q", domain.ErrInvalidEnum, req.Outcome)
	}

	if req.Outcome == "blocked" {
		// A blocked task keeps its lease and territory; it is paused, not handed back.
		if _, err := s.Store.UpdateAttempt(ctx, req.Fence.AttemptID,
			db.AttemptProgress{State: attemptState}); err != nil &&
			!errors.Is(err, domain.ErrIllegalTransition) {
			return FinishResult{}, err
		}
		task, err := s.Store.UpdateTaskStatus(ctx, req.Fence.TaskID, nextStatus)
		if err != nil {
			return FinishResult{}, err
		}
		return FinishResult{TaskStatus: task.Status, AttemptState: attemptState}, nil
	}

	task, err := s.Store.Release(ctx, db.ReleaseParams{
		Fence:          req.Fence,
		Reason:         req.Outcome,
		NextTaskStatus: nextStatus,
		AttemptState:   attemptState,
		FailureClass:   req.FailureClass,
		FailureSummary: privacy.ClampSummary(req.FailureSummary),
	})
	if err != nil {
		return FinishResult{}, err
	}
	if task.Status.IsTerminal() {
		if err := s.Store.CloseConflictsForTask(ctx, task.ID); err != nil {
			return FinishResult{}, err
		}
	}
	return FinishResult{TaskStatus: task.Status, AttemptState: attemptState, MissingChecks: missing}, nil
}

func joinComma(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// ---------------------------------------------------------------------------
// Handoff
// ---------------------------------------------------------------------------

// Handoff packages a task for continuation elsewhere (DESIGN.md §8.4, §18.7).
//
// The bundle is assembled from ledger state — decisions, scopes, validation, branch — not
// from the outgoing session's conversation. That is what lets Claude hand to Codex without
// either one seeing the other's transcript.
func (s *Service) Handoff(ctx context.Context, c Caller, fence domain.Fence, toHarness, toRole string, bundle domain.HandoffBundle) (domain.Handoff, error) {
	task, err := s.Store.GetTask(ctx, fence.TaskID)
	if err != nil {
		return domain.Handoff{}, err
	}
	reservations, err := s.Store.ReservationsForTask(ctx, task.ID)
	if err != nil {
		return domain.Handoff{}, err
	}
	validation, err := s.Store.ListValidation(ctx, task.ID)
	if err != nil {
		return domain.Handoff{}, err
	}

	bundle.TaskID = task.ID
	bundle.TaskRef = task.Ref
	bundle.FromAttemptID = fence.AttemptID
	bundle.CreatedAt = s.Store.Now()
	bundle.Objective = task.Objective
	bundle.AcceptanceCriteria = task.AcceptanceCriteria
	bundle.Scopes = reservations
	bundle.Validation = validation
	bundle.BaseSHA = task.BaseSHA
	if bundle.Visibility == "" {
		bundle.Visibility = task.Visibility
	}
	if attempt, err := s.Store.GetAttempt(ctx, fence.AttemptID); err == nil {
		bundle.Branch = attempt.Branch
		bundle.CommitSHA = attempt.CommitSHA
		if bundle.BaseSHA == "" {
			bundle.BaseSHA = attempt.BaseCommitSHA
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.Handoff{}, err
	}

	return s.Store.CreateHandoff(ctx, domain.Handoff{
		TaskID:        task.ID,
		FromAttemptID: fence.AttemptID,
		ToHarness:     toHarness,
		ToRole:        toRole,
		Visibility:    bundle.Visibility,
		Bundle:        bundle,
		CreatedBy:     c.Principal.ID,
	}, fence, true)
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// taskView builds the privacy-projected view of a task for this caller.
func (s *Service) taskView(ctx context.Context, c Caller, task domain.Task) (privacy.TaskView, error) {
	owner, err := s.Store.GetPrincipal(ctx, task.CreatedBy)
	if err != nil {
		return privacy.TaskView{}, err
	}
	reservations, err := s.Store.ReservationsForTask(ctx, task.ID)
	if err != nil {
		return privacy.TaskView{}, err
	}
	scopes := make([]string, 0, len(reservations))
	for _, r := range reservations {
		scopes = append(scopes, r.Resource())
	}

	proj := privacy.TaskProjection{
		Owner:     privacy.Owner{PrincipalID: owner.ID, Handle: owner.Handle},
		Scopes:    scopes,
		DependsOn: task.DependsOn,
	}
	// The latest attempt, not the active one: a finished task has no active attempt, and its
	// commit and branch are the whole point of looking at it.
	if attempt, err := s.Store.LatestAttempt(ctx, task.ID); err == nil {
		proj.Branch = attempt.Branch
		proj.CommitSHA = attempt.CommitSHA
		proj.Harness = attempt.Harness
	} else if !errors.Is(err, domain.ErrNotFound) {
		return privacy.TaskView{}, err
	}

	self := c.Principal.ID == owner.ID
	if self || task.Visibility.AtLeast(domain.VisibilityTeamArtifacts) {
		if proj.Artifacts, err = s.Store.ListArtifacts(ctx, task.ID); err != nil {
			return privacy.TaskView{}, err
		}
		if proj.Validation, err = s.Store.ListValidation(ctx, task.ID); err != nil {
			return privacy.TaskView{}, err
		}
	}
	return privacy.ProjectTask(c.Viewer(), task, proj), nil
}

// TaskView is the exported single-task projection.
func (s *Service) TaskView(ctx context.Context, c Caller, taskID domain.ID) (privacy.TaskView, error) {
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return privacy.TaskView{}, err
	}
	return s.taskView(ctx, c, task)
}

// ListTaskViews projects a filtered task list.
func (s *Service) ListTaskViews(ctx context.Context, c Caller, projectID domain.ID, f db.ListTasksFilter) ([]privacy.TaskView, error) {
	tasks, err := s.Store.ListTasks(ctx, projectID, f)
	if err != nil {
		return nil, err
	}
	owners := make([]domain.ID, 0, len(tasks))
	for _, t := range tasks {
		owners = append(owners, t.CreatedBy)
	}
	principals, err := s.Store.PrincipalsByID(ctx, owners)
	if err != nil {
		return nil, err
	}
	reservations, err := s.Store.ListReservations(ctx, projectID)
	if err != nil {
		return nil, err
	}
	scopesByTask := map[domain.ID][]string{}
	for _, r := range reservations {
		scopesByTask[r.TaskID] = append(scopesByTask[r.TaskID], r.Resource())
	}

	out := make([]privacy.TaskView, 0, len(tasks))
	for _, t := range tasks {
		owner := principals[t.CreatedBy]
		out = append(out, privacy.ProjectTask(c.Viewer(), t, privacy.TaskProjection{
			Owner:  privacy.Owner{PrincipalID: owner.ID, Handle: owner.Handle},
			Scopes: scopesByTask[t.ID],
		}))
	}
	return out, nil
}
