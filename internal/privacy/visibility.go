package privacy

import (
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// This file is the single boundary through which one principal observes another's work.
//
// The projections below construct a new struct from an explicit allowlist of fields. They
// never take a full domain object and delete parts of it. That distinction matters: a
// "remove the secret fields" pass fails open when someone adds a field and forgets to update
// it, whereas an allowlist fails closed — a newly added field is invisible until somebody
// deliberately adds it here.

// Viewer is the principal on whose behalf a projection is being made.
type Viewer struct {
	PrincipalID domain.ID
	Role        domain.Role
}

// Owner identifies the principal a piece of work belongs to.
type Owner struct {
	PrincipalID domain.ID
	Handle      string
}

// IsSelf reports whether the viewer owns the work being projected.
func (v Viewer) IsSelf(o Owner) bool {
	return o.PrincipalID != "" && v.PrincipalID == o.PrincipalID
}

// TaskView is what a principal is allowed to learn about a task.
//
// Note what is absent at every level: there is no prompt, transcript, reasoning, model
// output, or tool payload field. Not "empty for most viewers" — absent from the type.
type TaskView struct {
	ID            domain.ID         `json:"id"`
	Ref           string            `json:"ref"`
	ProjectID     domain.ID         `json:"project_id"`
	Status        domain.TaskStatus `json:"status"`
	Visibility    domain.Visibility `json:"visibility"`
	Owner         string            `json:"owner,omitempty"`
	Priority      int               `json:"priority"`
	RiskLevel     domain.RiskLevel  `json:"risk_level"`
	Scopes        []string          `json:"scopes"`
	Branch        string            `json:"branch,omitempty"`
	FencingEpoch  int64             `json:"fencing_epoch"`
	AttemptsCount int               `json:"attempts_count"`
	UpdatedAt     time.Time         `json:"updated_at"`
	CreatedAt     time.Time         `json:"created_at"`

	// team_summary and above.
	Title              string                       `json:"title,omitempty"`
	Objective          string                       `json:"objective,omitempty"`
	ExternalRef        string                       `json:"external_ref,omitempty"`
	AcceptanceCriteria []domain.AcceptanceCriterion `json:"acceptance_criteria,omitempty"`
	DependsOn          []string                     `json:"depends_on,omitempty"`
	ModelAlias         string                       `json:"model_alias,omitempty"`
	Harness            string                       `json:"harness,omitempty"`

	// team_artifacts and above.
	CommitSHA  string                    `json:"commit_sha,omitempty"`
	Artifacts  []domain.Artifact         `json:"artifacts,omitempty"`
	Validation []domain.ValidationResult `json:"validation,omitempty"`

	// Redacted marks a view that was narrowed because the viewer is not the owner. Clients
	// show "private work in progress" rather than pretending the task has no title.
	Redacted bool `json:"redacted,omitempty"`
}

// TaskProjection carries the extra context a projection needs beyond the task row itself.
type TaskProjection struct {
	Owner      Owner
	Scopes     []string
	Branch     string
	CommitSHA  string
	DependsOn  []string
	Artifacts  []domain.Artifact
	Validation []domain.ValidationResult
	Harness    string
}

// ProjectTask applies DESIGN.md §12.3 to a single task.
//
// A `private` task still exposes its owner, status, and reserved scopes to everyone. That is
// the whole trick: coordination needs territory, not intent. A teammate learns that
// `internal/billing/` is spoken for and by whom — enough to avoid the collision, and nothing
// about why.
func ProjectTask(v Viewer, t domain.Task, p TaskProjection) TaskView {
	self := v.IsSelf(p.Owner)

	view := TaskView{
		ID:            t.ID,
		Ref:           t.Ref,
		ProjectID:     t.ProjectID,
		Status:        t.Status,
		Visibility:    t.Visibility,
		Owner:         p.Owner.Handle,
		Priority:      t.Priority,
		RiskLevel:     t.RiskLevel,
		Scopes:        p.Scopes,
		Branch:        p.Branch,
		FencingEpoch:  t.FencingEpoch,
		AttemptsCount: t.AttemptsCount,
		UpdatedAt:     t.UpdatedAt,
		CreatedAt:     t.CreatedAt,
	}
	// A task commonly holds the same resource twice — once as a `planned` reservation from
	// when it was filed, once as `declared` when it was claimed. That is legitimate (a task
	// never conflicts with itself) but reads as a duplicate to anyone looking at it.
	view.Scopes = dedupe(p.Scopes)

	// The owner always sees their own work in full, regardless of visibility level.
	if !self && t.Visibility == domain.VisibilityPrivate {
		view.Redacted = true
		return view
	}

	if self || t.Visibility.AtLeast(domain.VisibilityTeamSummary) {
		view.Title = t.Title
		view.Objective = t.Objective
		view.ExternalRef = t.ExternalRef
		view.AcceptanceCriteria = t.AcceptanceCriteria
		view.DependsOn = p.DependsOn
		view.ModelAlias = t.ModelAlias
		view.Harness = p.Harness
	}

	if self || t.Visibility.AtLeast(domain.VisibilityTeamArtifacts) {
		view.CommitSHA = p.CommitSHA
		view.Artifacts = p.Artifacts
		view.Validation = p.Validation
	}

	return view
}

// ProjectPresence builds the team presence entry of DESIGN.md §8.5.
//
// The task title is included only when the task's visibility permits it; a private task
// shows as its ref alone. Everything else here is session metadata, which is coordination
// state by definition.
func ProjectPresence(
	v Viewer,
	s domain.Session,
	principal domain.Principal,
	task *domain.Task,
	taskOwner Owner,
	scopes []string,
	sponsor string,
) domain.PresenceEntry {
	entry := domain.PresenceEntry{
		SessionID:     s.ID,
		Principal:     principal.Handle,
		PrincipalID:   principal.ID,
		Kind:          principal.Kind,
		Harness:       s.Harness,
		State:         s.State,
		Branch:        s.Branch,
		Scopes:        scopes,
		SponsoredBy:   sponsor,
		StartedAt:     s.StartedAt,
		LastHeartbeat: s.HeartbeatAt,
	}
	entry.Scopes = dedupe(scopes)

	if task != nil {
		entry.TaskRef = task.Ref
		entry.TaskStatus = task.Status
		if v.IsSelf(taskOwner) || task.Visibility.AtLeast(domain.VisibilityTeamSummary) {
			entry.TaskTitle = task.Title
		}
	}
	return entry
}

// AttemptView is what a principal may learn about one execution of a task.
//
// The split here matters. Execution *identity* — which harness, which model alias, which
// reasoning effort, which workflow hash — is coordination state: it explains why a result
// looks the way it does, and DESIGN.md §20.4 requires it be recorded and explicable. Token
// counts and cost are the sponsor's business, so they are owner-only.
//
// As everywhere else, there is no field that can carry what the model said.
type AttemptView struct {
	ID            domain.ID           `json:"id"`
	AttemptNumber int                 `json:"attempt_number"`
	State         domain.AttemptState `json:"state"`
	Role          domain.AgentRole    `json:"role"`
	Harness       string              `json:"harness,omitempty"`
	ModelAlias    string              `json:"model_alias,omitempty"`
	ResolvedModel string              `json:"resolved_model,omitempty"`
	Effort        domain.Effort       `json:"reasoning_effort,omitempty"`
	Branch        string              `json:"branch,omitempty"`
	WorktreePath  string              `json:"worktree_path,omitempty"`
	BaseSHA       string              `json:"base_commit_sha,omitempty"`
	CommitSHA     string              `json:"commit_sha,omitempty"`
	ChangedPaths  []string            `json:"changed_paths,omitempty"`

	// Provenance: which rules were in force when this ran (DESIGN.md §20.4).
	WorkflowSHA      string `json:"workflow_sha,omitempty"`
	ProjectConfigSHA string `json:"project_config_sha,omitempty"`
	RouterPolicyVer  string `json:"router_policy_version,omitempty"`

	FailureClass string     `json:"failure_class,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	LastEventAt  time.Time  `json:"last_event_at"`

	// Sponsor-only.
	TokensIn  int64   `json:"tokens_in,omitempty"`
	TokensOut int64   `json:"tokens_out,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	Turns     int     `json:"turns,omitempty"`
}

// AttemptPolicy controls whether execution identity is published, per DESIGN.md §20.2
// (publishModelIdentity / publishHarnessIdentity).
type AttemptPolicy struct {
	PublishModelIdentity   bool
	PublishHarnessIdentity bool
}

// ProjectAttempt applies visibility rules to one attempt.
func ProjectAttempt(v Viewer, a domain.Attempt, sponsor Owner, policy AttemptPolicy) AttemptView {
	view := AttemptView{
		ID:               a.ID,
		AttemptNumber:    a.AttemptNumber,
		State:            a.State,
		Role:             a.Role,
		Branch:           a.Branch,
		WorktreePath:     a.WorktreePath,
		BaseSHA:          a.BaseCommitSHA,
		CommitSHA:        a.CommitSHA,
		ChangedPaths:     a.ChangedPaths,
		WorkflowSHA:      a.WorkflowSHA,
		ProjectConfigSHA: a.ProjectConfigSHA,
		RouterPolicyVer:  a.RouterPolicyVer,
		FailureClass:     a.FailureClass,
		StartedAt:        a.StartedAt,
		EndedAt:          a.EndedAt,
		LastEventAt:      a.LastEventAt,
	}

	self := v.IsSelf(sponsor)
	if self || policy.PublishHarnessIdentity {
		view.Harness = a.Harness
	}
	if self || policy.PublishModelIdentity {
		view.ModelAlias = a.ModelAlias
		view.ResolvedModel = a.ResolvedModel
		view.Effort = a.ReasoningEffort
	}
	if self {
		view.TokensIn, view.TokensOut = a.TokensIn, a.TokensOut
		view.CostUSD, view.Turns = a.CostUSD, a.Turns
	}
	return view
}

// SessionCapabilityView is what one principal may learn about what another's live session can
// do (DESIGN.md §7.3, §20.2).
//
// Tier, reasoning effort, and capability tags are always published. They have to be: they are
// what makes "hand this to something that can think harder" answerable at all, and they say
// nothing about what anyone is working on. The concrete model name is execution identity and
// follows the same publishModelIdentity switch as an attempt's — a team that does not want to
// broadcast which vendor each person is paying for still gets working capability routing.
type SessionCapabilityView struct {
	SessionID     domain.ID           `json:"session_id"`
	Principal     string              `json:"principal"`
	Harness       string              `json:"harness"`
	State         domain.SessionState `json:"state"`
	Available     bool                `json:"available"`
	Model         string              `json:"model,omitempty"`
	Alias         string              `json:"alias,omitempty"`
	Tier          domain.Tier         `json:"tier,omitempty"`
	Effort        domain.Effort       `json:"reasoning_effort,omitempty"`
	MaxEffort     domain.Effort       `json:"max_reasoning_effort,omitempty"`
	Capabilities  []string            `json:"capabilities,omitempty"`
	ContextWindow int                 `json:"context_window,omitempty"`
	Roles         []string            `json:"roles,omitempty"`
	Resolved      bool                `json:"resolved"`
	ActiveTaskRef string              `json:"active_task_ref,omitempty"`
	QueuedOffers  int                 `json:"queued_offers,omitempty"`
	LastHeartbeat time.Time           `json:"last_heartbeat"`
	MachineID     string              `json:"machine_id,omitempty"`
}

// ProjectSessionCapability applies visibility rules to one session's capability record.
func ProjectSessionCapability(
	v Viewer,
	s domain.Session,
	principal domain.Principal,
	policy AttemptPolicy,
) SessionCapabilityView {
	caps := s.Capabilities
	view := SessionCapabilityView{
		SessionID: s.ID,
		Principal: principal.Handle,
		// Harness is already part of presence, so withholding it here would be theatre.
		Harness:       s.Harness,
		State:         s.State,
		Available:     s.State.Accepting(),
		Tier:          caps.Tier,
		Effort:        caps.Effort,
		MaxEffort:     caps.Ceiling(),
		Capabilities:  caps.Capabilities,
		ContextWindow: caps.ContextWindow,
		Roles:         caps.Roles,
		Resolved:      caps.Resolved,
		LastHeartbeat: s.HeartbeatAt,
	}

	self := v.IsSelf(Owner{PrincipalID: principal.ID, Handle: principal.Handle})
	if self || policy.PublishHarnessIdentity {
		view.MachineID = s.MachineID
	}
	if self || policy.PublishModelIdentity {
		view.Model = caps.Model
		view.Alias = caps.Alias
	}
	return view
}

// SessionView is what one principal may learn about a session when a project's whole
// session history is exported (`conductor sessions save all`).
//
// It is the union of what presence and the capability inventory already publish, plus the
// lifecycle timestamps that make a historical record worth keeping. Machine identity — the
// machine id, worktree path, and harness version — follows publishHarnessIdentity and the
// concrete model name follows publishModelIdentity, exactly as they do for a live session;
// a principal always sees those fields on their own sessions. The active task appears as a
// ref only. As with every view in this file, there is no field that can carry what anyone
// typed.
type SessionView struct {
	ID             domain.ID               `json:"id"`
	ProjectID      domain.ID               `json:"project_id"`
	Principal      string                  `json:"principal"`
	PrincipalID    domain.ID               `json:"principal_id"`
	Kind           domain.PrincipalKind    `json:"kind"`
	RunnerID       domain.ID               `json:"runner_id,omitempty"`
	Harness        string                  `json:"harness"`
	HarnessVersion string                  `json:"harness_version,omitempty"`
	MachineID      string                  `json:"machine_id,omitempty"`
	WorktreePath   string                  `json:"worktree_path,omitempty"`
	Branch         string                  `json:"branch,omitempty"`
	BaseSHA        string                  `json:"base_sha,omitempty"`
	Visibility     domain.Visibility       `json:"visibility"`
	State          domain.SessionState     `json:"state"`
	ActiveTaskRef  string                  `json:"active_task_ref,omitempty"`
	Capabilities   SessionCapabilitiesView `json:"capabilities"`
	StartedAt      time.Time               `json:"started_at"`
	LastHeartbeat  time.Time               `json:"last_heartbeat"`
	ExpiresAt      time.Time               `json:"expires_at"`
	ClosedAt       *time.Time              `json:"closed_at,omitempty"`

	// Redacted marks a view whose machine or model identity was withheld because the viewer
	// is not the session's principal and the project does not publish it.
	Redacted bool `json:"redacted,omitempty"`
}

// SessionCapabilitiesView is the capability record inside a SessionView. It carries the same
// fields as SessionCapabilityView, minus the liveness ones that belong to the session itself.
type SessionCapabilitiesView struct {
	Model         string        `json:"model,omitempty"`
	Alias         string        `json:"alias,omitempty"`
	Tier          domain.Tier   `json:"tier,omitempty"`
	Effort        domain.Effort `json:"reasoning_effort,omitempty"`
	MaxEffort     domain.Effort `json:"max_reasoning_effort,omitempty"`
	Capabilities  []string      `json:"capabilities,omitempty"`
	ContextWindow int           `json:"context_window,omitempty"`
	Roles         []string      `json:"roles,omitempty"`
	Resolved      bool          `json:"resolved"`
}

// ProjectSession applies visibility rules to one session for export.
func ProjectSession(
	v Viewer,
	s domain.Session,
	principal domain.Principal,
	activeTaskRef string,
	policy AttemptPolicy,
) SessionView {
	caps := s.Capabilities
	view := SessionView{
		ID:            s.ID,
		ProjectID:     s.ProjectID,
		Principal:     principal.Handle,
		PrincipalID:   principal.ID,
		Kind:          principal.Kind,
		RunnerID:      s.RunnerID,
		Harness:       s.Harness,
		Branch:        s.Branch,
		BaseSHA:       s.BaseSHA,
		Visibility:    s.Visibility,
		State:         s.State,
		ActiveTaskRef: activeTaskRef,
		Capabilities: SessionCapabilitiesView{
			Tier:          caps.Tier,
			Effort:        caps.Effort,
			MaxEffort:     caps.Ceiling(),
			Capabilities:  caps.Capabilities,
			ContextWindow: caps.ContextWindow,
			Roles:         caps.Roles,
			Resolved:      caps.Resolved,
		},
		StartedAt:     s.StartedAt,
		LastHeartbeat: s.HeartbeatAt,
		ExpiresAt:     s.ExpiresAt,
		ClosedAt:      s.ClosedAt,
	}

	self := v.IsSelf(Owner{PrincipalID: principal.ID, Handle: principal.Handle})
	if self || policy.PublishHarnessIdentity {
		view.HarnessVersion = s.HarnessVersion
		view.MachineID = s.MachineID
		view.WorktreePath = s.WorktreePath
	} else if s.HarnessVersion != "" || s.MachineID != "" || s.WorktreePath != "" {
		view.Redacted = true
	}
	if self || policy.PublishModelIdentity {
		view.Capabilities.Model = caps.Model
		view.Capabilities.Alias = caps.Alias
	} else if caps.Model != "" || caps.Alias != "" {
		view.Redacted = true
	}
	return view
}

// ConflictView is what the other side of a conflict is told.
type ConflictView struct {
	ID         domain.ID           `json:"id"`
	Kind       domain.ConflictKind `json:"kind"`
	Severity   domain.Severity     `json:"severity"`
	Suggestion domain.Outcome      `json:"suggestion"`
	State      string              `json:"state"`
	Weight     float64             `json:"weight"`
	Resources  []string            `json:"resources,omitempty"`
	Reason     string              `json:"reason,omitempty"`
	Other      ConflictParty       `json:"other"`
	Mine       ConflictParty       `json:"mine"`
	DetectedAt time.Time           `json:"detected_at"`
}

// ConflictParty names one side of a conflict in coordination terms only.
type ConflictParty struct {
	TaskID  domain.ID `json:"task_id"`
	TaskRef string    `json:"task_ref"`
	Title   string    `json:"title,omitempty"`
	Owner   string    `json:"owner,omitempty"`
}

// ProjectConflict orients a conflict edge from the viewer's perspective and redacts the
// counterparty's title when their task is private.
//
// The similarity score is deliberately omitted from the view. Telling someone "your intent
// is 0.83 similar to Bob's private task" is a side channel: repeated probing against a
// numeric score can reconstruct what Bob is working on. The suggestion (join / wait / split)
// carries the actionable part without the oracle.
func ProjectConflict(
	v Viewer,
	e domain.ConflictEdge,
	mine, other domain.Task,
	mineOwner, otherOwner Owner,
) ConflictView {
	view := ConflictView{
		ID:         e.ID,
		Kind:       e.Kind,
		Severity:   e.Severity,
		Suggestion: e.Suggestion,
		State:      e.State,
		Weight:     e.Weight,
		Resources:  e.Detail.Resources,
		Reason:     e.Detail.Reason,
		DetectedAt: e.DetectedAt,
		Mine: ConflictParty{
			TaskID:  mine.ID,
			TaskRef: mine.Ref,
			Title:   mine.Title,
			Owner:   mineOwner.Handle,
		},
		Other: ConflictParty{
			TaskID:  other.ID,
			TaskRef: other.Ref,
			Owner:   otherOwner.Handle,
		},
	}
	if v.IsSelf(otherOwner) || other.Visibility.AtLeast(domain.VisibilityTeamSummary) {
		view.Other.Title = other.Title
	}
	return view
}

// EventPayloadAllowlist is the set of payload keys permitted on a domain event that leaves
// the control plane. Anything not listed is dropped by SanitizeEventPayload.
//
// This is an allowlist rather than a denylist for the same reason the projections are: a new
// event type that carries an unexpected key should be silently narrowed, not silently
// leaked.
var EventPayloadAllowlist = map[string]bool{
	"task_id": true, "task_ref": true, "attempt_id": true, "lease_id": true,
	"session_id": true, "principal": true, "principal_id": true, "runner_id": true,
	"status": true, "from": true, "to": true, "state": true, "harness": true,
	"model_alias": true, "resolved_model": true, "reasoning_effort": true, "role": true,
	"branch": true, "commit_sha": true, "base_sha": true, "worktree": true,
	"resource": true, "resources": true, "mode": true, "scopes": true,
	"outcome": true, "severity": true, "suggestion": true, "kind": true,
	"conflict_id": true, "reason": true, "failure_class": true, "phase": true,
	"percent_hint": true, "summary": true, "blocker": true, "changed_paths": true,
	"exit_code": true, "command_id": true, "duration_ms": true,
	"tokens_in": true, "tokens_out": true, "tokens": true, "cost_usd": true, "turns": true,
	"fencing_epoch": true, "attempt_number": true, "expires_at": true,
	"visibility": true, "count": true, "error": true, "title": true,
	"workflow_sha": true, "similarity": true, "tier": true,
}

// SanitizeEventPayload drops any key not on the allowlist. Returns the sanitized map and the
// keys that were removed, so the caller can log a warning about a mis-declared event without
// logging the values themselves.
func SanitizeEventPayload(payload map[string]any) (map[string]any, []string) {
	if payload == nil {
		return map[string]any{}, nil
	}
	out := make(map[string]any, len(payload))
	var dropped []string
	for k, v := range payload {
		if EventPayloadAllowlist[k] {
			out[k] = v
		} else {
			dropped = append(dropped, k)
		}
	}
	return out, dropped
}

// MaxSummaryLength bounds caller-supplied progress text. A summary field is the one place a
// careless agent could paste a transcript, so it is truncated rather than trusted
// (DESIGN.md §18.4).
const MaxSummaryLength = 500

// dedupe removes repeated entries while preserving order, and always returns a non-nil slice
// so the field serializes as [] rather than null.
func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ClampSummary truncates an agent-supplied summary to a length that cannot carry a
// meaningful amount of conversation.
func ClampSummary(s string) string {
	r := []rune(s)
	if len(r) <= MaxSummaryLength {
		return s
	}
	return string(r[:MaxSummaryLength]) + "…"
}
