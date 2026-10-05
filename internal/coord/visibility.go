package coord

import (
	"context"
	"errors"
	"fmt"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
)

// Read-path projections for the records that do not go through a privacy.TaskView: domain
// events, handoff bundles, and a task's validation and decision history. Each follows the
// rules internal/privacy applies to tasks (DESIGN.md §12.3): the owner sees their own work in
// full, a private task shows others its territory and nothing about its intent, team_summary
// adds the summary-level fields, and team_artifacts adds commits, paths, and check results.
//
// Like the projections in internal/privacy, these build from allowlists. A new event key or a
// new bundle field is invisible to other principals until someone decides it may be shown.

// effectiveVisibility is the narrower of two visibilities.
func effectiveVisibility(a, b domain.Visibility) domain.Visibility {
	if a.AtLeast(b) {
		return b
	}
	return a
}

// Event payload keys by the visibility a viewer needs to see them. Keys that are in none of
// these sets (lease_id, and anything added later) never reach another principal.
var (
	// eventKeysTerritory is coordination state: what is happening and where, never why.
	eventKeysTerritory = map[string]bool{
		"task_id": true, "task_ref": true, "attempt_id": true, "attempt_number": true,
		"session_id": true, "principal": true, "principal_id": true, "runner_id": true,
		"ticket_id": true, "assignment_id": true, "conflict_id": true,
		"status": true, "from": true, "to": true, "state": true, "phase": true,
		"percent_hint": true, "resource": true, "resources": true, "mode": true, "scopes": true,
		"outcome": true, "severity": true, "suggestion": true, "kind": true,
		"position": true, "queue_depth": true, "lane": true, "granted": true, "expired": true,
		"visibility": true, "count": true, "priority": true, "expires_at": true,
		"fencing_epoch": true, "dropped_keys": true,
	}
	// eventKeysSummary is what team_summary adds: the summary-level description of the work
	// and its execution identity, as privacy.ProjectTask publishes for the task itself.
	eventKeysSummary = map[string]bool{
		"title": true, "summary": true, "blocker": true, "reason": true, "failure_class": true,
		"harness": true, "model_alias": true, "resolved_model": true, "reasoning_effort": true,
		"role": true, "tier": true, "provider": true, "labels": true, "branch": true,
		"error": true, "similarity": true, "workflow_sha": true, "tokens": true,
		// Which files an attempt touched and where it ran describe the work, so they follow
		// the task's summary visibility — the same rule privacy.ProjectAttempt applies.
		"changed_paths": true, "worktree": true,
	}
	// eventKeysArtifacts is what team_artifacts adds: commits, files, and check results.
	eventKeysArtifacts = map[string]bool{
		"commit_sha": true, "base_sha": true,
		"exit_code": true, "command_id": true, "duration_ms": true,
	}
	// eventKeysSponsor is an attempt's spend, which privacy.ProjectAttempt shows only to its
	// sponsor. Project-level budget events carry no task and are not affected.
	eventKeysSponsor = map[string]bool{
		"tokens_in": true, "tokens_out": true, "cost_usd": true, "turns": true,
	}
)

// ProjectEvents applies the caller's visibility to a page of one project's events.
//
// An event about a task is shown at the narrower of the visibility it was written with and
// the task's visibility now, so a private task's claim, progress, and evidence events do not
// outrun the task's own redaction just because their writer labelled them team-visible.
func (s *Service) ProjectEvents(ctx context.Context, c Caller, projectID domain.ID, events []domain.Event) ([]domain.Event, error) {
	var ids []domain.ID
	var refs []string
	for _, e := range events {
		if e.AggregateType == "task" {
			ids = append(ids, e.AggregateID)
		}
		if v, ok := e.Payload["task_id"].(string); ok && v != "" {
			ids = append(ids, v)
		}
		if v, ok := e.Payload["task_ref"].(string); ok && v != "" {
			refs = append(refs, v)
		}
	}
	tasks, err := s.Store.TaskAccessFor(ctx, projectID, ids, refs)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Event, 0, len(events))
	for _, e := range events {
		out = append(out, projectEvent(c.Viewer(), e, eventTask(e, tasks)))
	}
	return out, nil
}

// eventTask finds the task an event concerns, if any.
func eventTask(e domain.Event, tasks map[string]db.TaskAccess) *db.TaskAccess {
	keys := []string{}
	if e.AggregateType == "task" {
		keys = append(keys, e.AggregateID)
	}
	if v, ok := e.Payload["task_id"].(string); ok {
		keys = append(keys, v)
	}
	if v, ok := e.Payload["task_ref"].(string); ok {
		keys = append(keys, "ref:"+v)
	}
	for _, k := range keys {
		if t, ok := tasks[k]; ok {
			return &t
		}
	}
	return nil
}

// projectEvent narrows one event for a viewer.
func projectEvent(v privacy.Viewer, e domain.Event, task *db.TaskAccess) domain.Event {
	vis := e.Visibility
	if task != nil {
		vis = effectiveVisibility(vis, task.Visibility)
		if v.PrincipalID != "" && v.PrincipalID == task.CreatedBy {
			return e
		}
	}
	if v.PrincipalID != "" && v.PrincipalID == e.ActorPrincipal {
		return e
	}

	payload := make(map[string]any, len(e.Payload))
	redacted := false
	for k, val := range e.Payload {
		allowed := eventKeysTerritory[k] ||
			(vis.AtLeast(domain.VisibilityTeamSummary) && eventKeysSummary[k]) ||
			(vis.AtLeast(domain.VisibilityTeamArtifacts) && eventKeysArtifacts[k]) ||
			(task == nil && eventKeysSponsor[k])
		if allowed {
			payload[k] = val
		} else {
			redacted = true
		}
	}
	if redacted {
		payload["redacted"] = true
	}
	e.Payload = payload
	e.Visibility = vis
	return e
}

// HandoffView is what the caller may learn about a task's latest handoff.
//
// A handoff bundle restates the task's objective and acceptance criteria and adds the outgoing
// session's account of the work, so it is the task's content in another shape and follows the
// task's visibility. The owner and the principal who wrote the handoff see it whole.
func (s *Service) HandoffView(ctx context.Context, c Caller, task domain.Task) (domain.Handoff, error) {
	h, err := s.Store.LatestHandoff(ctx, task.ID)
	if err != nil {
		return domain.Handoff{}, err
	}
	if c.Principal.ID == task.CreatedBy || c.Principal.ID == h.CreatedBy {
		return h, nil
	}
	vis := effectiveVisibility(h.Visibility, task.Visibility)
	b := h.Bundle
	out := domain.Handoff{
		ID: h.ID, TaskID: h.TaskID, FromAttemptID: h.FromAttemptID, ToAttemptID: h.ToAttemptID,
		Visibility: vis, CreatedBy: h.CreatedBy, CreatedAt: h.CreatedAt,
		Bundle: domain.HandoffBundle{
			TaskID: b.TaskID, TaskRef: b.TaskRef, FromAttemptID: b.FromAttemptID,
			CreatedAt: b.CreatedAt, Scopes: b.Scopes, Visibility: vis,
		},
	}
	if vis.AtLeast(domain.VisibilityTeamSummary) {
		out.ToHarness, out.ToRole = h.ToHarness, h.ToRole
		out.Bundle.Objective = b.Objective
		out.Bundle.AcceptanceCriteria = b.AcceptanceCriteria
		out.Bundle.CompletedWork = b.CompletedWork
		out.Bundle.Decisions = b.Decisions
		out.Bundle.Assumptions = b.Assumptions
		out.Bundle.OpenQuestions = b.OpenQuestions
		out.Bundle.Blockers = b.Blockers
		out.Bundle.RecommendedNextAction = b.RecommendedNextAction
		out.Bundle.RecommendedRole = b.RecommendedRole
		out.Bundle.Branch = b.Branch
		out.Bundle.BaseSHA = b.BaseSHA
	}
	if vis.AtLeast(domain.VisibilityTeamArtifacts) {
		out.Bundle.CommitSHA = b.CommitSHA
		out.Bundle.PatchArtifactID = b.PatchArtifactID
		out.Bundle.Validation = b.Validation
	}
	return out, nil
}

// ValidationView returns a task's check results when the caller may see them: the owner
// always, everyone else from team_artifacts up — the same rule taskView applies before
// putting validation into a TaskView. Otherwise the list is empty, not an error, so a
// teammate's dashboard renders "no results shared" rather than failing.
func (s *Service) ValidationView(ctx context.Context, c Caller, task domain.Task) ([]domain.ValidationResult, error) {
	if c.Principal.ID != task.CreatedBy && !task.Visibility.AtLeast(domain.VisibilityTeamArtifacts) {
		return []domain.ValidationResult{}, nil
	}
	results, err := s.Store.ListValidation(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if results == nil {
		results = []domain.ValidationResult{}
	}
	return results, nil
}

// DecisionsView returns a task's policy decisions. The decision itself (routed, blocked,
// escalated) is coordination state; the rationale is free-form facts about the task, so a
// private task's rationale is withheld from everyone but its owner.
func (s *Service) DecisionsView(ctx context.Context, c Caller, task domain.Task, limit int) ([]domain.PolicyDecision, error) {
	decisions, err := s.Store.ListDecisions(ctx, task.ID, limit)
	if err != nil {
		return nil, err
	}
	if decisions == nil {
		return []domain.PolicyDecision{}, nil
	}
	if c.Principal.ID == task.CreatedBy || task.Visibility.AtLeast(domain.VisibilityTeamSummary) {
		return decisions, nil
	}
	for i := range decisions {
		decisions[i].Rationale = map[string]any{"redacted": true}
	}
	return decisions, nil
}

// AuthorizeBrief checks the caller may read an attempt's brief. The brief is the agent's
// instruction — the task's full content, its handoff, and its live fence — so it belongs to
// whoever is executing the attempt: the attempt's sponsor or executor, or the holder of the
// task's live lease. The task's owner may read it too, and so may a maintainer for a task
// that is not private.
func (s *Service) AuthorizeBrief(ctx context.Context, c Caller, attempt domain.Attempt) error {
	if c.Principal.ID == attempt.SponsorPrincipal || c.Principal.ID == attempt.ExecutorPrincipal {
		return nil
	}
	if lease, err := s.Store.ActiveLeaseForTask(ctx, attempt.TaskID); err == nil {
		if c.HoldsLease(lease) {
			return nil
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	task, err := s.Store.GetTask(ctx, attempt.TaskID)
	if err != nil {
		return err
	}
	if c.Principal.ID == task.CreatedBy {
		return nil
	}
	if task.Visibility != domain.VisibilityPrivate && c.Role.Can(domain.RoleMaintainer) {
		return nil
	}
	return fmt.Errorf("%w: an attempt's brief is for the session executing it", domain.ErrNotPermitted)
}

// AuthorizeAttemptControl checks the caller may drive an attempt's lifecycle — record its
// route, advance its state. That is the executing session's job: the attempt's sponsor or
// executor, or the holder of the task's live lease. A maintainer may as well, to clean up
// after a runner that died. Anyone else in the project could otherwise fail or re-route a
// teammate's running attempt.
func (s *Service) AuthorizeAttemptControl(ctx context.Context, c Caller, attempt domain.Attempt) error {
	if c.Principal.ID == attempt.SponsorPrincipal || c.Principal.ID == attempt.ExecutorPrincipal ||
		c.Role.Can(domain.RoleMaintainer) {
		return nil
	}
	lease, err := s.Store.ActiveLeaseForTask(ctx, attempt.TaskID)
	if err == nil && c.HoldsLease(lease) && lease.AttemptID == attempt.ID {
		return nil
	}
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	return fmt.Errorf("%w: this attempt belongs to another principal", domain.ErrNotPermitted)
}
