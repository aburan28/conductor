package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/privacy"
)

// Objective tasks: delphi's objective tree mirrored as tasks (docs/DELPHI.md). A node's task
// carries ObjectiveLabel; delphi appends the tree's decisions to its timeline and settles it
// once the node is accepted, failed or abandoned. Nothing here carries a prompt, an answer
// or a reviewer's feedback: those stay with delphi (DESIGN.md §12.4).

// ObjectiveLabel marks a task that delphi's objective runner drives.
const ObjectiveLabel = "delphi-objective"

// objectiveEvents are the timeline events delphi may append to an objective task.
var objectiveEvents = map[string]bool{
	"objective.created": true, "objective.routed": true, "objective.delegated": true,
	"objective.answered": true, "objective.decomposed": true, "objective.checked": true,
	"objective.reviewer": true, "objective.synthesized": true, "objective.granted": true,
	"objective.settled": true,
}

type timelineBody struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

// objectiveTask loads the path's task and checks the caller may drive it: it must be an
// objective task, and the caller its creator or a maintainer.
func (s *Server) objectiveTask(r *http.Request, p domain.Principal) (domain.Task, error) {
	task, caller, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		return task, err
	}
	isObjective := false
	for _, l := range task.Labels {
		if l == ObjectiveLabel {
			isObjective = true
		}
	}
	if !isObjective {
		return task, fmt.Errorf("%w: task %s is not an objective task (label %s)", domain.ErrInvalidArgument, task.Ref, ObjectiveLabel)
	}
	if task.CreatedBy != caller.Principal.ID && !caller.Role.Can(domain.RoleMaintainer) {
		return task, fmt.Errorf("%w: only the objective's creator or a maintainer may record its timeline", domain.ErrNotPermitted)
	}
	return task, nil
}

func (s *Server) appendTaskTimeline(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, err := s.objectiveTask(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body timelineBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if !objectiveEvents[body.Type] {
		s.fail(w, r, fmt.Errorf("%w: event type %q is not an objective event", domain.ErrInvalidArgument, body.Type))
		return
	}
	payload, dropped := privacy.SanitizeEventPayload(body.Payload)
	if len(dropped) > 0 {
		s.fail(w, r, fmt.Errorf("%w: payload keys not allowed on a timeline: %s", domain.ErrInvalidArgument, strings.Join(dropped, ", ")))
		return
	}
	if err := s.store.AppendEvent(r.Context(), task.OrganizationID, task.ProjectID, p.ID, "task", task.ID,
		body.Type, domain.VisibilityTeamSummary, payload); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, map[string]any{"task": task.Ref, "type": body.Type})
}

func (s *Server) taskTimeline(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	events, err := s.store.TaskTimeline(r.Context(), task.ProjectID, task.ID, r.URL.Query().Get("subtree") == "true", intParam(r, "limit", 500))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Every event leaves through the caller's visibility, exactly like the project feed: a
	// private task in the subtree shows only what its owner would let others see.
	if events, err = s.svc.ProjectEvents(r.Context(), caller, task.ProjectID, events); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"task": task.Ref, "events": events})
}

type settleBody struct {
	Status domain.TaskStatus `json:"status"`
	Reason string            `json:"reason"`
}

func (s *Server) settleObjectiveTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, err := s.objectiveTask(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body settleBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.store.SettleObjectiveTask(r.Context(), task.ID, body.Status, privacy.ClampSummary(body.Reason))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"task": out.Ref, "status": out.Status})
}
