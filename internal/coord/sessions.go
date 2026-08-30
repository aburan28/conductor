package coord

import (
	"context"
	"errors"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
)

// SessionSnapshot is one session's durable record: the projected session, its capability
// record, and its inbox — what the dashboard's save and export persist in one file.
type SessionSnapshot struct {
	Session     privacy.SessionView           `json:"session"`
	Capability  privacy.SessionCapabilityView `json:"capability"`
	Assignments []domain.Assignment           `json:"assignments"`
}

// SessionSnapshot gathers one session the way Sessions projects the whole project: the
// caller sees exactly what an export would show them. The inbox is included only when the
// caller may read it — the session's own principal or a maintainer, the same floor the
// assignments endpoint enforces — because an assignment names a task, and a task's
// existence is itself disclosure under §12.3.
func (s *Service) SessionSnapshot(ctx context.Context, c Caller, sessionID domain.ID) (SessionSnapshot, error) {
	session, err := s.Store.GetSession(ctx, sessionID)
	if err != nil {
		return SessionSnapshot{}, err
	}
	project, err := s.Store.GetProject(ctx, session.ProjectID)
	if err != nil {
		return SessionSnapshot{}, err
	}
	principal, err := s.Store.GetPrincipal(ctx, session.PrincipalID)
	if err != nil {
		return SessionSnapshot{}, err
	}
	policy := privacy.AttemptPolicy{
		PublishModelIdentity:   project.Config.PublishModelIdentity,
		PublishHarnessIdentity: project.Config.PublishHarnessIdentity,
	}

	var ref string
	if session.ActiveTaskID != "" {
		task, err := s.Store.GetTask(ctx, session.ActiveTaskID)
		switch {
		case err == nil:
			ref = task.Ref
		case errors.Is(err, domain.ErrNotFound):
			// The task was deleted after the session pointed at it; the session is
			// still worth snapshotting.
		default:
			return SessionSnapshot{}, err
		}
	}

	out := SessionSnapshot{
		Session:     privacy.ProjectSession(c.Viewer(), session, principal, ref, policy),
		Capability:  privacy.ProjectSessionCapability(c.Viewer(), session, principal, policy),
		Assignments: []domain.Assignment{},
	}
	if session.PrincipalID == c.Principal.ID || c.Role.Can(domain.RoleMaintainer) {
		assignments, err := s.Store.SessionInbox(ctx, session.ID, true)
		if err != nil {
			return SessionSnapshot{}, err
		}
		if assignments != nil {
			out.Assignments = assignments
		}
	}
	return out, nil
}

// Sessions returns a project's complete session history, projected for the caller.
//
// This is the read behind `conductor sessions save all`. Presence is a live projection and
// forgets a session the moment it goes stale; an export has to include the sessions that
// closed cleanly and the ones that did not, because the stale ones are exactly the evidence
// a recovery needs (which worktree, which branch, which machine). The projection applies
// the same identity switches as the live views, so exporting is never a way around them.
func (s *Service) Sessions(ctx context.Context, c Caller, projectID domain.ID) ([]privacy.SessionView, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	sessions, err := s.Store.ListSessions(ctx, projectID)
	if err != nil {
		return nil, err
	}
	principals, err := s.principalsFor(ctx, sessions)
	if err != nil {
		return nil, err
	}
	policy := privacy.AttemptPolicy{
		PublishModelIdentity:   project.Config.PublishModelIdentity,
		PublishHarnessIdentity: project.Config.PublishHarnessIdentity,
	}

	// Many sessions share a task over a project's life; resolve each ref once.
	refs := map[domain.ID]string{}
	views := make([]privacy.SessionView, 0, len(sessions))
	for _, sess := range sessions {
		var ref string
		if sess.ActiveTaskID != "" {
			cached, ok := refs[sess.ActiveTaskID]
			if !ok {
				task, err := s.Store.GetTask(ctx, sess.ActiveTaskID)
				switch {
				case err == nil:
					cached = task.Ref
				case errors.Is(err, domain.ErrNotFound):
					// The task was deleted after the session pointed at it; the session is
					// still worth exporting.
				default:
					return nil, err
				}
				refs[sess.ActiveTaskID] = cached
			}
			ref = cached
		}
		views = append(views, privacy.ProjectSession(
			c.Viewer(), sess, principals[sess.PrincipalID], ref, policy))
	}
	return views, nil
}
