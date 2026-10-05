package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// UnboundClaimWindow is how long a claim made with no session behind it stays alive waiting
// for one. `conductor task claim` in a plain shell, followed by `conductor wrap <tool>`, is
// the documented order; the person needs long enough to read the claim and start the tool,
// which one lease TTL (90 seconds by default) is not. The wrap sidecar adopts the claim on
// start and its heartbeat keeps it alive from then on; a claim nobody adopts lapses at the end
// of the window and its territory is released.
const UnboundClaimWindow = 10 * time.Minute

// ClaimLeaseTTL is the lease TTL for a new claim. A claim bound to a session or a runner is
// renewed by that party's heartbeat and gets the project's TTL. One bound to neither gets
// UnboundClaimWindow (or the project TTL, if that is longer) to be adopted.
func ClaimLeaseTTL(cfg domain.ProjectConfig, sessionID, runnerID domain.ID) time.Duration {
	ttl := cfg.LeaseTTL.OrDefault(90 * time.Second)
	if sessionID == "" && runnerID == "" && ttl < UnboundClaimWindow {
		return UnboundClaimWindow
	}
	return ttl
}

// sessionFor loads a session and checks it belongs to the caller. A session is a person's (or
// an agent's) own working context; nobody else may act through it.
func (s *Service) sessionFor(ctx context.Context, principal domain.Principal, sessionID domain.ID) (domain.Session, Caller, error) {
	session, err := s.Store.GetSession(ctx, sessionID)
	if err != nil {
		return domain.Session{}, Caller{}, err
	}
	if session.PrincipalID != principal.ID {
		return domain.Session{}, Caller{}, fmt.Errorf("%w: session belongs to another principal", domain.ErrNotPermitted)
	}
	caller, err := s.Authorize(ctx, principal, session.ProjectID, domain.RoleContributor)
	if err != nil {
		return domain.Session{}, Caller{}, err
	}
	caller.SessionID = session.ID
	return session, caller, nil
}

// AdoptClaims binds the caller's unbound claims in a worktree to their session, so the
// session's heartbeat keeps them alive (db.AdoptLeases has the rules).
func (s *Service) AdoptClaims(ctx context.Context, principal domain.Principal, sessionID domain.ID, worktree string) ([]db.AdoptedLease, error) {
	session, _, err := s.sessionFor(ctx, principal, sessionID)
	if err != nil {
		return nil, err
	}
	project, err := s.Store.GetProject(ctx, session.ProjectID)
	if err != nil {
		return nil, err
	}
	adopted, err := s.Store.AdoptLeases(ctx, db.AdoptLeasesParams{
		SessionID: session.ID, WorktreePath: worktree,
		TTL: project.Config.LeaseTTL.OrDefault(90 * time.Second),
	})
	if adopted == nil {
		adopted = []db.AdoptedLease{}
	}
	return adopted, err
}

// SessionScopeResult is ReserveForSession's answer.
type SessionScopeResult struct {
	ExpandScopeResult
	TaskID  domain.ID `json:"task_id,omitempty"`
	TaskRef string    `json:"task_ref,omitempty"`
	// NoClaim is set when the session holds no live claim, so there is nothing to reserve
	// under. The caller decides whether that is worth telling anyone.
	NoClaim bool `json:"no_claim,omitempty"`
}

// ReserveForSession reserves resources under the live claim a session carries, without the
// caller presenting a fence. The pre-edit hook uses it: it runs inside a wrapped session,
// knows the session, and should not have to juggle a lease id and epoch to record that the
// person just started editing a file their claim did not declare. The session is the
// authority — it must be the caller's own, and the lease must be bound to it — so this grants
// nothing a fenced coord_expand_scope by the same person could not.
func (s *Service) ReserveForSession(ctx context.Context, principal domain.Principal, sessionID domain.ID, requests []domain.ScopeRequest, source domain.ReservationSource) (SessionScopeResult, error) {
	session, caller, err := s.sessionFor(ctx, principal, sessionID)
	if err != nil {
		return SessionScopeResult{}, err
	}
	lease, err := s.Store.SessionLease(ctx, session.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return SessionScopeResult{NoClaim: true}, nil
		}
		return SessionScopeResult{}, err
	}
	task, err := s.Store.GetTask(ctx, lease.TaskID)
	if err != nil {
		return SessionScopeResult{}, err
	}
	if source == "" {
		source = domain.SourceObserved
	}
	fence := domain.Fence{TaskID: lease.TaskID, AttemptID: lease.AttemptID,
		LeaseID: lease.ID, FencingEpoch: lease.FencingEpoch}
	result, err := s.ExpandScope(ctx, caller, fence, session.ProjectID, requests, source)
	if err != nil {
		return SessionScopeResult{}, err
	}
	return SessionScopeResult{ExpandScopeResult: result, TaskID: task.ID, TaskRef: task.Ref}, nil
}

// PublishEvidence records evidence against a live attempt without finishing it.
//
// Only the lease's holder may publish into it: the fence proves the attempt is current, and
// the holder check proves the caller is the one working it. Commands are stored with no
// runner id, which is how an agent's own report stays distinguishable from a runner's
// observation; summaries are not accepted here at all.
func (s *Service) PublishEvidence(ctx context.Context, c Caller, fence domain.Fence, m domain.EvidenceManifest) error {
	lease, err := s.Store.GetLease(ctx, fence.LeaseID)
	if err != nil {
		return err
	}
	if lease.HolderPrincipal != c.Principal.ID {
		return fmt.Errorf("%w: only the lease holder may publish evidence for this attempt", domain.ErrNotPermitted)
	}
	m.TaskID, m.AttemptID = fence.TaskID, fence.AttemptID
	m.LeaseID, m.FencingEpoch = fence.LeaseID, fence.FencingEpoch
	m.RunnerID = ""
	for i := range m.Commands {
		m.Commands[i].RunnerID = ""
	}
	if err := s.Store.SubmitEvidence(ctx, m, ""); err != nil {
		return err
	}
	task, err := s.Store.GetTask(ctx, fence.TaskID)
	if err != nil {
		return err
	}
	return s.Store.AppendEvent(ctx, task.OrganizationID, task.ProjectID, c.Principal.ID,
		"attempt", fence.AttemptID, "attempt.evidence", task.Visibility, map[string]any{
			"task_ref": task.Ref, "commit_sha": m.CommitSHA, "count": len(m.Commands),
			"changed_paths": m.ChangedPaths,
		})
}
