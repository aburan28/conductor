package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
	"github.com/adamburan/conductor/internal/selector"
)

// Capability aggregation and assignment (DESIGN.md §7.3, §7.7, §13).
//
// Presence answers "who is here". This answers "what can the people who are here actually
// do", and then acts on the answer: work that needs a frontier model at xhigh effort can be
// offered to a session that has one, instead of being started by whoever happened to ask
// first and discovering the ceiling three failed attempts later.

// DefaultOfferTTL bounds how long an unanswered offer holds a task.
const DefaultOfferTTL = 15 * time.Minute

// CapabilityInventory is the project-wide roll-up plus the per-session detail behind it.
type CapabilityInventory struct {
	Project   string                          `json:"project"`
	Inventory selector.Inventory              `json:"inventory"`
	Sessions  []privacy.SessionCapabilityView `json:"sessions"`
	Runners   []selector.Fallback             `json:"runners,omitempty"`
	// Servable answers a requirement the caller asked about, when they asked about one.
	Servable *ServabilityAnswer `json:"servable,omitempty"`
}

// ServabilityAnswer is "can anything here do this, and if not, what is missing".
type ServabilityAnswer struct {
	Requirement domain.CapabilityRequirement `json:"requirement"`
	Servable    bool                         `json:"servable"`
	Reasons     []string                     `json:"reasons,omitempty"`
}

// Capabilities aggregates what every live session in a project can do.
func (s *Service) Capabilities(ctx context.Context, c Caller, projectID domain.ID, req domain.CapabilityRequirement) (CapabilityInventory, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return CapabilityInventory{}, err
	}

	cands, sessions, err := s.candidates(ctx, projectID)
	if err != nil {
		return CapabilityInventory{}, err
	}

	principals, err := s.principalsFor(ctx, sessions)
	if err != nil {
		return CapabilityInventory{}, err
	}

	policy := privacy.AttemptPolicy{
		PublishModelIdentity:   project.Config.PublishModelIdentity,
		PublishHarnessIdentity: project.Config.PublishHarnessIdentity,
	}

	queued := map[domain.ID]int{}
	for _, cand := range cands {
		queued[cand.SessionID] = cand.Queued
	}

	views := make([]privacy.SessionCapabilityView, 0, len(sessions))
	for _, sess := range sessions {
		view := privacy.ProjectSessionCapability(c.Viewer(), sess, principals[sess.PrincipalID], policy)
		view.QueuedOffers = queued[sess.ID]
		if sess.ActiveTaskID != "" {
			if task, err := s.Store.GetTask(ctx, sess.ActiveTaskID); err == nil {
				view.ActiveTaskRef = task.Ref
			} else if !errors.Is(err, domain.ErrNotFound) {
				return CapabilityInventory{}, err
			}
		}
		views = append(views, view)
	}

	inv := selector.Aggregate(cands)
	runners, err := s.runnerFallbacks(ctx, projectID)
	if err != nil {
		return CapabilityInventory{}, err
	}
	inv.Runners = runners

	out := CapabilityInventory{
		Project:   project.Slug,
		Inventory: inv,
		Sessions:  views,
		Runners:   runners,
	}
	if !req.Empty() {
		servable, reasons := selector.Serves(cands, req)
		out.Servable = &ServabilityAnswer{Requirement: req, Servable: servable, Reasons: reasons}
	}
	return out, nil
}

// AssignResult is the outcome of placing work.
type AssignResult struct {
	Assignment domain.Assignment  `json:"assignment"`
	Choice     selector.Choice    `json:"choice"`
	Inventory  selector.Inventory `json:"inventory"`
}

// AssignParams is a request to place a task with a capable session.
type AssignParams struct {
	TaskID      domain.ID
	Requirement domain.CapabilityRequirement
	// SessionID names a specific session. It is still checked against the requirement:
	// naming a target is a preference, not a way around a floor.
	SessionID domain.ID
	HandoffID domain.ID
	TTL       time.Duration
}

// Assign offers a task to the session best able to serve it.
//
// Selection is a floor check first and a ranking second, so a requirement of "T4 at xhigh"
// cannot be satisfied by an idle T2 session no matter how convenient it would be. When
// nothing qualifies the error names the gap — the highest tier and effort actually present —
// because "no capacity" alone gives a coordinator nothing to act on.
func (s *Service) Assign(ctx context.Context, c Caller, p AssignParams) (AssignResult, error) {
	task, err := s.Store.GetTask(ctx, p.TaskID)
	if err != nil {
		return AssignResult{}, err
	}

	cands, _, err := s.candidates(ctx, task.ProjectID)
	if err != nil {
		return AssignResult{}, err
	}

	if p.SessionID != "" {
		cands = filterCandidates(cands, p.SessionID)
		if len(cands) == 0 {
			return AssignResult{}, fmt.Errorf("%w: session %s is not live in this project",
				domain.ErrNotFound, p.SessionID)
		}
	}

	choice, err := selector.Select(cands, p.Requirement)
	if err != nil {
		return AssignResult{Choice: choice, Inventory: selector.Aggregate(cands)}, err
	}

	assignment, err := s.Store.CreateAssignment(ctx, db.CreateAssignmentParams{
		TaskID:      task.ID,
		ProjectID:   task.ProjectID,
		SessionID:   choice.SessionID,
		HandoffID:   p.HandoffID,
		Requirement: p.Requirement,
		Rationale:   joinRationale(choice.Rationale),
		CreatedBy:   c.Principal.ID,
		TTL:         p.TTL,
	})
	if err != nil {
		return AssignResult{}, err
	}

	// The event carries the requirement and the chosen session, never why the coordinator
	// wanted it. Visibility follows the task, so a private task's assignment is not a hole
	// in its own privacy.
	if err := s.Store.AppendEvent(ctx, task.OrganizationID, task.ProjectID, c.Principal.ID,
		"task", task.ID, "task.assigned", task.Visibility, map[string]any{
			"assignment_id": assignment.ID,
			"session_id":    choice.SessionID,
			"requirement":   p.Requirement.Describe(),
			"harness":       choice.Harness,
			"tier":          string(choice.Tier),
			"effort":        string(choice.Effort),
		}); err != nil {
		return AssignResult{}, err
	}
	s.Store.Audit(ctx, task.OrganizationID, task.ProjectID, c.Principal.ID,
		"task.assign", "task", task.ID, map[string]any{
			"session_id": choice.SessionID, "requirement": p.Requirement.Describe(),
		})

	return AssignResult{Assignment: assignment, Choice: choice, Inventory: selector.Aggregate(cands)}, nil
}

// DelegateParams packages "hand this work to something more capable than me" as one call.
type DelegateParams struct {
	Fence       domain.Fence
	ToHarness   string
	ToRole      string
	Bundle      domain.HandoffBundle
	Requirement domain.CapabilityRequirement
	TTL         time.Duration
}

// DelegateResult reports what was packaged and where it went.
type DelegateResult struct {
	Handoff    domain.Handoff      `json:"handoff"`
	Assignment *domain.Assignment  `json:"assignment,omitempty"`
	Choice     *selector.Choice    `json:"choice,omitempty"`
	Inventory  selector.Inventory  `json:"inventory"`
	Unplaced   string              `json:"unplaced,omitempty"`
	Runners    []selector.Fallback `json:"runners,omitempty"`
}

// Delegate hands the current work to a session that clears a capability floor.
//
// This is the coordinator's escalation path: package what is known, release the lease, then
// offer the task to a session that can actually do the next part. The two halves are one call
// because doing only the first is what produces work that sits in a handoff nobody reads.
//
// Failing to place the work is not an error. The handoff is still written and the task is
// still released — losing a completed handoff because no frontier session happened to be
// online would be strictly worse than leaving the task ready with an explanation attached.
func (s *Service) Delegate(ctx context.Context, c Caller, p DelegateParams) (DelegateResult, error) {
	handoff, err := s.Handoff(ctx, c, p.Fence, p.ToHarness, p.ToRole, p.Bundle)
	if err != nil {
		return DelegateResult{}, err
	}

	req := p.Requirement
	if req.ExcludeSession == "" {
		req.ExcludeSession = c.SessionID
	}
	if req.Harness == "" {
		req.Harness = p.ToHarness
	}

	assigned, err := s.Assign(ctx, c, AssignParams{
		TaskID:      handoff.TaskID,
		Requirement: req,
		HandoffID:   handoff.ID,
		TTL:         p.TTL,
	})
	if err != nil {
		if errors.Is(err, domain.ErrCapacity) || errors.Is(err, domain.ErrAlreadyClaimed) {
			var runners []selector.Fallback
			if task, terr := s.Store.GetTask(ctx, handoff.TaskID); terr == nil {
				runners, _ = s.runnerFallbacks(ctx, task.ProjectID)
			}
			return DelegateResult{
				Handoff:   handoff,
				Inventory: assigned.Inventory,
				Choice:    &assigned.Choice,
				Unplaced:  err.Error(),
				Runners:   runners,
			}, nil
		}
		return DelegateResult{Handoff: handoff}, err
	}

	return DelegateResult{
		Handoff:    handoff,
		Assignment: &assigned.Assignment,
		Choice:     &assigned.Choice,
		Inventory:  assigned.Inventory,
	}, nil
}

// Inbox lists the offers waiting for a session.
//
// A session may only read its own inbox. Assignments name tasks, and a task's existence is
// itself disclosure under §12.3, so this is authorization, not tidiness.
func (s *Service) Inbox(ctx context.Context, c Caller, sessionID domain.ID, all bool) ([]domain.Assignment, error) {
	session, err := s.Store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session.PrincipalID != c.Principal.ID && !c.Role.Can(domain.RoleMaintainer) {
		return nil, domain.ErrNotFound
	}
	return s.Store.SessionInbox(ctx, sessionID, all)
}

// RespondToOffer records a session's accept or decline.
func (s *Service) RespondToOffer(ctx context.Context, c Caller, assignmentID domain.ID, accept bool, note string) (domain.Assignment, error) {
	assignment, err := s.Store.GetAssignment(ctx, assignmentID)
	if err != nil {
		return domain.Assignment{}, err
	}
	session, err := s.Store.GetSession(ctx, assignment.SessionID)
	if err != nil {
		return domain.Assignment{}, err
	}
	if session.PrincipalID != c.Principal.ID && !c.Role.Can(domain.RoleMaintainer) {
		return domain.Assignment{}, domain.ErrNotFound
	}

	state := domain.AssignmentDeclined
	if accept {
		state = domain.AssignmentAccepted
	}
	updated, err := s.Store.RespondToAssignment(ctx, assignmentID, state, note)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Assignment{}, fmt.Errorf(
				"%w: this offer is no longer open (it was answered, cancelled, or expired)",
				domain.ErrIllegalTransition)
		}
		return domain.Assignment{}, err
	}

	task, err := s.Store.GetTask(ctx, updated.TaskID)
	if err != nil {
		return domain.Assignment{}, err
	}
	eventType := "task.offer_declined"
	if accept {
		eventType = "task.offer_accepted"
	}
	if err := s.Store.AppendEvent(ctx, task.OrganizationID, task.ProjectID, c.Principal.ID,
		"task", task.ID, eventType, task.Visibility, map[string]any{
			"assignment_id": updated.ID, "session_id": updated.SessionID,
		}); err != nil {
		return domain.Assignment{}, err
	}
	return updated, nil
}

// ResolveCapabilities turns what a session declared into what the catalog says it is worth.
//
// Called on registration and on every capability update, so the tier a session is selected on
// is always the organization's opinion of that model rather than the session's own.
func (s *Service) ResolveCapabilities(ctx context.Context, orgID domain.ID, harness string, declared domain.SessionCapabilities) (domain.SessionCapabilities, error) {
	profiles, err := s.Store.ListModelProfiles(ctx, orgID)
	if err != nil {
		return domain.SessionCapabilities{}, err
	}
	return selector.Resolve(declared, harness, profiles), nil
}

// candidates builds the selector's view of a project: every live session, with how much work
// it is already holding.
func (s *Service) candidates(ctx context.Context, projectID domain.ID) ([]selector.Candidate, []domain.Session, error) {
	sessions, err := s.Store.LiveSessions(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	open, err := s.Store.OpenAssignments(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	queued := map[domain.ID]int{}
	for _, a := range open {
		queued[a.SessionID]++
	}

	principals, err := s.principalsFor(ctx, sessions)
	if err != nil {
		return nil, nil, err
	}

	cands := make([]selector.Candidate, 0, len(sessions))
	for _, sess := range sessions {
		cands = append(cands, selector.Candidate{
			SessionID:     sess.ID,
			Principal:     principals[sess.PrincipalID].Handle,
			PrincipalID:   sess.PrincipalID,
			Harness:       sess.Harness,
			State:         sess.State,
			Capabilities:  sess.Capabilities,
			Busy:          sess.ActiveTaskID != "",
			Queued:        queued[sess.ID],
			MachineID:     sess.MachineID,
			LastHeartbeat: sess.HeartbeatAt,
		})
	}
	return cands, sessions, nil
}

func (s *Service) principalsFor(ctx context.Context, sessions []domain.Session) (map[domain.ID]domain.Principal, error) {
	ids := make([]domain.ID, 0, len(sessions))
	for _, sess := range sessions {
		ids = append(ids, sess.PrincipalID)
	}
	if len(ids) == 0 {
		return map[domain.ID]domain.Principal{}, nil
	}
	return s.Store.PrincipalsByID(ctx, ids)
}

// runnerFallbacks reports the machines that could launch a fresh harness, so a coordinator
// told "nobody live can do this" also learns whether the work is dispatchable at all.
func (s *Service) runnerFallbacks(ctx context.Context, projectID domain.ID) ([]selector.Fallback, error) {
	runners, err := s.Store.AvailableRunners(ctx, projectID, "", 2*time.Minute)
	if err != nil {
		return nil, err
	}
	out := make([]selector.Fallback, 0, len(runners))
	for _, r := range runners {
		out = append(out, selector.Fallback{Name: r.Name, Models: r.Capabilities.Models})
	}
	return out, nil
}

func filterCandidates(in []selector.Candidate, id domain.ID) []selector.Candidate {
	for _, c := range in {
		if c.SessionID == id {
			return []selector.Candidate{c}
		}
	}
	return nil
}

func joinRationale(lines []string) string {
	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "; "
		}
		out += line
	}
	return out
}
