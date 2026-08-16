package coord

import (
	"context"
	"errors"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
)

// Presence answers "who is working on what, right now" (DESIGN.md §8.5).
//
// This is the feature the whole privacy design exists to make safe. A teammate sees that
// Alice is on T-42 holding `internal/router/`, that a Codex worker is modifying migrations
// under Adam's sponsorship, and that Rachel is reviewing T-38. They do not see a single word
// anyone typed. There is no field in the response that could carry one.
func (s *Service) Presence(ctx context.Context, c Caller, projectID domain.ID) ([]domain.PresenceEntry, error) {
	sessions, err := s.Store.LiveSessions(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return []domain.PresenceEntry{}, nil
	}

	principalIDs := make([]domain.ID, 0, len(sessions))
	for _, sess := range sessions {
		principalIDs = append(principalIDs, sess.PrincipalID)
	}
	principals, err := s.Store.PrincipalsByID(ctx, principalIDs)
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

	out := make([]domain.PresenceEntry, 0, len(sessions))
	for _, sess := range sessions {
		principal := principals[sess.PrincipalID]

		var task *domain.Task
		var owner privacy.Owner
		var scopes []string
		var sponsor string

		if sess.ActiveTaskID != "" {
			t, err := s.Store.GetTask(ctx, sess.ActiveTaskID)
			if err == nil {
				task = &t
				scopes = scopesByTask[t.ID]
				if p, err := s.Store.GetPrincipal(ctx, t.CreatedBy); err == nil {
					owner = privacy.Owner{PrincipalID: p.ID, Handle: p.Handle}
				}
				// An autonomous worker acts on behalf of a human. Showing the sponsor is
				// what makes "codex-17 is modifying db/migrations" accountable rather than
				// anonymous (DESIGN.md §24.1).
				if attempt, err := s.Store.ActiveAttempt(ctx, t.ID); err == nil {
					if attempt.SponsorPrincipal != "" && attempt.SponsorPrincipal != principal.ID {
						if sp, err := s.Store.GetPrincipal(ctx, attempt.SponsorPrincipal); err == nil {
							sponsor = sp.Handle
						}
					}
				} else if !errors.Is(err, domain.ErrNotFound) {
					return nil, err
				}
			} else if !errors.Is(err, domain.ErrNotFound) {
				return nil, err
			}
		}

		out = append(out, privacy.ProjectPresence(
			c.Viewer(), sess, principal, task, owner, scopes, sponsor))
	}
	return out, nil
}

// StatusSummary is the compact project view behind coord_project_status (DESIGN.md §18.8).
//
// It is deliberately small. Returning every historical task to a model's context is both
// expensive and useless; what an agent needs is what is in flight and what is contested.
type StatusSummary struct {
	Project     string                 `json:"project"`
	Counts      map[string]int         `json:"counts"`
	Active      []privacy.TaskView     `json:"active"`
	Ready       []privacy.TaskView     `json:"ready"`
	Conflicts   []privacy.ConflictView `json:"conflicts"`
	Presence    []domain.PresenceEntry `json:"presence"`
	WorkflowSHA string                 `json:"workflow_sha,omitempty"`
}

// StatusLimit bounds how many tasks the compact status returns per bucket.
const StatusLimit = 25

// ProjectStatus builds the compact status view.
func (s *Service) ProjectStatus(ctx context.Context, c Caller, projectID domain.ID) (StatusSummary, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return StatusSummary{}, err
	}

	all, err := s.Store.ListTasks(ctx, projectID, db.ListTasksFilter{OpenOnly: true})
	if err != nil {
		return StatusSummary{}, err
	}

	counts := map[string]int{}
	var activeTasks, readyTasks []domain.Task
	for _, t := range all {
		counts[string(t.Status)]++
		switch {
		case t.Status.IsActive() && len(activeTasks) < StatusLimit:
			activeTasks = append(activeTasks, t)
		case t.Status == domain.TaskReady && len(readyTasks) < StatusLimit:
			readyTasks = append(readyTasks, t)
		}
	}

	project_ := project // capture for closure clarity
	views := func(tasks []domain.Task) ([]privacy.TaskView, error) {
		out := make([]privacy.TaskView, 0, len(tasks))
		for _, t := range tasks {
			v, err := s.taskView(ctx, c, t)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}

	active, err := views(activeTasks)
	if err != nil {
		return StatusSummary{}, err
	}
	ready, err := views(readyTasks)
	if err != nil {
		return StatusSummary{}, err
	}
	conflicts, err := s.ConflictViews(ctx, c, projectID, true)
	if err != nil {
		return StatusSummary{}, err
	}
	presence, err := s.Presence(ctx, c, projectID)
	if err != nil {
		return StatusSummary{}, err
	}

	return StatusSummary{
		Project:     project_.Slug,
		Counts:      counts,
		Active:      active,
		Ready:       ready,
		Conflicts:   conflicts,
		Presence:    presence,
		WorkflowSHA: project_.WorkflowSHA,
	}, nil
}
