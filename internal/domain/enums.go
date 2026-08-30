// Package domain holds Conductor's core types and the rules that govern them.
//
// Everything in this package is deterministic and dependency-free: no database, no network,
// no model. That is deliberate. DESIGN.md §4.1 requires that state transitions, leases, and
// authorization are owned by code that can be exhaustively tested, never by a model.
package domain

import "fmt"

// TaskStatus enumerates DESIGN.md §9.1.
type TaskStatus string

const (
	TaskProposed          TaskStatus = "proposed"
	TaskReady             TaskStatus = "ready"
	TaskClaimed           TaskStatus = "claimed"
	TaskRunning           TaskStatus = "running"
	TaskBlockedDependency TaskStatus = "blocked_dependency"
	TaskBlockedConflict   TaskStatus = "blocked_conflict"
	TaskBlockedInput      TaskStatus = "blocked_input"
	TaskVerifying         TaskStatus = "verifying"
	TaskReviewRequired    TaskStatus = "review_required"
	TaskMerging           TaskStatus = "merging"
	TaskDone              TaskStatus = "done"
	TaskFailed            TaskStatus = "failed"
	TaskCancelled         TaskStatus = "cancelled"
	TaskSuperseded        TaskStatus = "superseded"
)

// AllTaskStatuses is the authoritative list; it must match the CHECK constraint on
// tasks.status. TestTaskStatusesMatchSchema enforces that.
var AllTaskStatuses = []TaskStatus{
	TaskProposed, TaskReady, TaskClaimed, TaskRunning,
	TaskBlockedDependency, TaskBlockedConflict, TaskBlockedInput,
	TaskVerifying, TaskReviewRequired, TaskMerging,
	TaskDone, TaskFailed, TaskCancelled, TaskSuperseded,
}

// IsBlocked reports whether the status is one of the three blocked variants.
func (s TaskStatus) IsBlocked() bool {
	return s == TaskBlockedDependency || s == TaskBlockedConflict || s == TaskBlockedInput
}

// IsTerminal reports whether no further transition is possible except operator retry.
func (s TaskStatus) IsTerminal() bool {
	return s == TaskDone || s == TaskCancelled || s == TaskSuperseded
}

// IsActive reports whether the task currently occupies a worker slot.
func (s TaskStatus) IsActive() bool {
	return s == TaskClaimed || s == TaskRunning || s == TaskVerifying ||
		s == TaskReviewRequired || s == TaskMerging
}

// IsOpen reports whether the task still represents outstanding work. Used by conflict
// detection to decide which tasks participate in the graph.
func (s TaskStatus) IsOpen() bool {
	return !s.IsTerminal() && s != TaskFailed
}

// AttemptState enumerates DESIGN.md §9.2.
type AttemptState string

const (
	AttemptQueued             AttemptState = "queued"
	AttemptPreparingWorkspace AttemptState = "preparing_workspace"
	AttemptStartingHarness    AttemptState = "starting_harness"
	AttemptRunning            AttemptState = "running"
	AttemptWaitingApproval    AttemptState = "waiting_for_approval"
	AttemptWaitingInput       AttemptState = "waiting_for_input"
	AttemptPausedConflict     AttemptState = "paused_conflict"
	AttemptSucceeded          AttemptState = "succeeded"
	AttemptFailedRetryable    AttemptState = "failed_retryable"
	AttemptFailedTerminal     AttemptState = "failed_terminal"
	AttemptCancelled          AttemptState = "cancelled"
	AttemptStale              AttemptState = "stale"
)

var AllAttemptStates = []AttemptState{
	AttemptQueued, AttemptPreparingWorkspace, AttemptStartingHarness, AttemptRunning,
	AttemptWaitingApproval, AttemptWaitingInput, AttemptPausedConflict,
	AttemptSucceeded, AttemptFailedRetryable, AttemptFailedTerminal,
	AttemptCancelled, AttemptStale,
}

// IsLive reports whether the attempt still holds its worker slot and lease.
func (s AttemptState) IsLive() bool {
	switch s {
	case AttemptQueued, AttemptPreparingWorkspace, AttemptStartingHarness, AttemptRunning,
		AttemptWaitingApproval, AttemptWaitingInput, AttemptPausedConflict:
		return true
	}
	return false
}

// IsTerminal is the complement of IsLive.
func (s AttemptState) IsTerminal() bool { return !s.IsLive() }

// SessionState enumerates the presence states of DESIGN.md §7.3.
type SessionState string

const (
	SessionOnlineIdle      SessionState = "online_idle"
	SessionPlanning        SessionState = "planning"
	SessionWorking         SessionState = "working"
	SessionWaitingForInput SessionState = "waiting_for_input"
	SessionPaused          SessionState = "paused"
	SessionBlocked         SessionState = "blocked"
	SessionReviewing       SessionState = "reviewing"
	SessionOfflineGrace    SessionState = "offline_grace"
	SessionStale           SessionState = "stale"
	SessionClosed          SessionState = "closed"
)

var AllSessionStates = []SessionState{
	SessionOnlineIdle, SessionPlanning, SessionWorking, SessionWaitingForInput,
	SessionPaused, SessionBlocked, SessionReviewing, SessionOfflineGrace, SessionStale,
	SessionClosed,
}

// Accepting reports whether a session in this state can be offered new work. A session that
// is blocked or waiting for its human is present but not available, and offering it work
// would park a task behind a person who is not there.
func (s SessionState) Accepting() bool {
	switch s {
	case SessionOnlineIdle, SessionPlanning, SessionWorking, SessionReviewing:
		return true
	default:
		return false
	}
}

// SessionControl is a lifecycle instruction held for a session's sidecar: pause freezes the
// harness in place, resume wakes it. The dashboard records it; the sidecar picks it up on
// its next heartbeat, acts, and acknowledges, at which point it clears.
type SessionControl string

const (
	ControlPause  SessionControl = "pause"
	ControlResume SessionControl = "resume"
)

// AssignmentState is the lifecycle of an offer of work to a specific session (DESIGN.md §7.7).
//
// An assignment is an offer, not a command. Conductor cannot make a session do anything; it
// can only put work where a capable session will find it, and record what happened next.
type AssignmentState string

const (
	AssignmentOffered   AssignmentState = "offered"
	AssignmentAccepted  AssignmentState = "accepted"
	AssignmentDeclined  AssignmentState = "declined"
	AssignmentExpired   AssignmentState = "expired"
	AssignmentCancelled AssignmentState = "cancelled"
	AssignmentFulfilled AssignmentState = "fulfilled"
)

var AllAssignmentStates = []AssignmentState{
	AssignmentOffered, AssignmentAccepted, AssignmentDeclined,
	AssignmentExpired, AssignmentCancelled, AssignmentFulfilled,
}

// Open reports whether the assignment is still awaiting or holding a session's answer.
func (a AssignmentState) Open() bool {
	return a == AssignmentOffered || a == AssignmentAccepted
}

// Visibility enumerates DESIGN.md §12.3. Ordering matters: each level is a superset of the
// one before it.
type Visibility string

const (
	VisibilityPrivate       Visibility = "private"
	VisibilityTeamSummary   Visibility = "team_summary"
	VisibilityTeamArtifacts Visibility = "team_artifacts"
	VisibilitySharedDebug   Visibility = "shared_debug"
)

var AllVisibilities = []Visibility{
	VisibilityPrivate, VisibilityTeamSummary, VisibilityTeamArtifacts, VisibilitySharedDebug,
}

// rank orders visibility from most to least restrictive.
func (v Visibility) rank() int {
	switch v {
	case VisibilityPrivate:
		return 0
	case VisibilityTeamSummary:
		return 1
	case VisibilityTeamArtifacts:
		return 2
	case VisibilitySharedDebug:
		return 3
	}
	return 0 // unknown values are treated as maximally restrictive
}

// AtLeast reports whether v permits everything level permits.
func (v Visibility) AtLeast(level Visibility) bool { return v.rank() >= level.rank() }

// ResourceType enumerates the reservation resource keys of DESIGN.md §11.1.
type ResourceType string

const (
	ResourceRepo      ResourceType = "repo"
	ResourcePath      ResourceType = "path"
	ResourceDir       ResourceType = "dir"
	ResourceLockfile  ResourceType = "lockfile"
	ResourceMigration ResourceType = "migration"
	ResourceSchema    ResourceType = "schema"
	ResourceAPI       ResourceType = "api"
	ResourceSymbol    ResourceType = "symbol"
	ResourceTest      ResourceType = "test"
)

var AllResourceTypes = []ResourceType{
	ResourceRepo, ResourcePath, ResourceDir, ResourceLockfile,
	ResourceMigration, ResourceSchema, ResourceAPI, ResourceSymbol, ResourceTest,
}

// EnforcedInMVP marks the resource types the first release enforces (DESIGN.md §11.1).
// Others are recorded and surfaced, but do not block.
func (r ResourceType) EnforcedInMVP() bool {
	switch r {
	case ResourcePath, ResourceDir, ResourceLockfile, ResourceMigration, ResourceRepo:
		return true
	}
	return false
}

// ReservationMode enumerates DESIGN.md §11.2.
type ReservationMode string

const (
	ModeReadShared         ReservationMode = "read_shared"
	ModeWriteExclusive     ReservationMode = "write_exclusive"
	ModeReviewShared       ReservationMode = "review_shared"
	ModeSpeculativeWrite   ReservationMode = "speculative_write"
	ModeProtectedExclusive ReservationMode = "protected_exclusive"
)

var AllReservationModes = []ReservationMode{
	ModeReadShared, ModeWriteExclusive, ModeReviewShared,
	ModeSpeculativeWrite, ModeProtectedExclusive,
}

// Writes reports whether the mode intends to modify the resource.
func (m ReservationMode) Writes() bool {
	return m == ModeWriteExclusive || m == ModeSpeculativeWrite || m == ModeProtectedExclusive
}

// ReservationSource records how a reservation came to exist. `observed` reservations are
// created by the runner's git watcher when an attempt touches a path it did not declare.
type ReservationSource string

const (
	SourceDeclared ReservationSource = "declared"
	SourcePlanned  ReservationSource = "planned"
	SourceObserved ReservationSource = "observed"
	SourcePolicy   ReservationSource = "policy"
)

// PrincipalKind enumerates DESIGN.md §24.1.
type PrincipalKind string

const (
	PrincipalHuman        PrincipalKind = "human"
	PrincipalSession      PrincipalKind = "interactive_session"
	PrincipalAgentAttempt PrincipalKind = "agent_attempt"
	PrincipalRunner       PrincipalKind = "runner_service"
	PrincipalControlPlane PrincipalKind = "control_plane_service"
	PrincipalReviewBot    PrincipalKind = "review_bot"
)

// Role enumerates the RBAC roles of DESIGN.md §24.2.
type Role string

const (
	RoleOrgAdmin     Role = "org_admin"
	RoleProjectAdmin Role = "project_admin"
	RoleMaintainer   Role = "maintainer"
	RoleContributor  Role = "contributor"
	RoleReviewer     Role = "reviewer"
	RoleObserver     Role = "observer"
	RoleRunner       Role = "runner"
)

// roleRank orders roles by privilege. Runner is deliberately outside the ladder: it is a
// capability, not a seniority level, and never implies human permissions.
func roleRank(r Role) int {
	switch r {
	case RoleObserver:
		return 1
	case RoleReviewer:
		return 2
	case RoleContributor:
		return 3
	case RoleMaintainer:
		return 4
	case RoleProjectAdmin:
		return 5
	case RoleOrgAdmin:
		return 6
	}
	return 0
}

// Can reports whether role r satisfies the requirement of role need.
func (r Role) Can(need Role) bool {
	if r == RoleRunner {
		// A runner may act on execution endpoints only; those declare RoleRunner directly.
		return need == RoleRunner || need == RoleObserver
	}
	if need == RoleRunner {
		return false
	}
	return roleRank(r) >= roleRank(need)
}

// AgentRole enumerates the routing roles of DESIGN.md §13.1.
type AgentRole string

const (
	RolePlanner      AgentRole = "planner"
	RoleImplementer  AgentRole = "implementer"
	RoleVerifier     AgentRole = "verifier"
	RoleCodeReviewer AgentRole = "reviewer"
	RoleResearcher   AgentRole = "researcher"
)

// Effort enumerates reasoning effort levels.
//
// `xhigh` sits between `high` and `max` because harnesses expose it as a distinct, more
// expensive setting rather than a synonym for either neighbour. Both `xhigh` and `max` are
// opt-in: automatic escalation stops at `high` (see Up), so a retry loop can never walk a
// task onto the two most expensive settings on its own.
type Effort string

const (
	EffortNone   Effort = "none"
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

var AllEfforts = []Effort{
	EffortNone, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax,
}

var effortOrder = map[Effort]int{
	EffortNone: 0, EffortLow: 1, EffortMedium: 2, EffortHigh: 3, EffortXHigh: 4, EffortMax: 5,
}

// AtLeast reports whether e meets the floor. An unset effort meets only an unset floor.
func (e Effort) AtLeast(floor Effort) bool {
	if floor == "" {
		return true
	}
	if e == "" {
		return false
	}
	return effortOrder[e] >= effortOrder[floor]
}

// AtMost clamps e to ceiling.
func (e Effort) AtMost(ceiling Effort) Effort {
	if effortOrder[e] > effortOrder[ceiling] {
		return ceiling
	}
	return e
}

// Up returns the next effort level, saturating at high.
func (e Effort) Up() Effort {
	switch e {
	case EffortNone:
		return EffortLow
	case EffortLow:
		return EffortMedium
	case EffortMedium:
		return EffortHigh
	default:
		// `xhigh` and `max` are opt-in only; escalation stops at high. A caller that wants
		// one of them asks for it by name, which is what makes the spend deliberate.
		return EffortHigh
	}
}

// Down returns the previous effort level, saturating at none.
func (e Effort) Down() Effort {
	switch e {
	case EffortMax:
		return EffortXHigh
	case EffortXHigh:
		return EffortHigh
	case EffortHigh:
		return EffortMedium
	case EffortMedium:
		return EffortLow
	default:
		return EffortNone
	}
}

// Tier enumerates the routing tiers of DESIGN.md §13.4.
type Tier string

const (
	TierT0 Tier = "T0"
	TierT1 Tier = "T1"
	TierT2 Tier = "T2"
	TierT3 Tier = "T3"
	TierT4 Tier = "T4"
)

var tierOrder = map[Tier]int{TierT0: 0, TierT1: 1, TierT2: 2, TierT3: 3, TierT4: 4}

// AtLeast reports whether t is at least as capable as floor.
func (t Tier) AtLeast(floor Tier) bool { return tierOrder[t] >= tierOrder[floor] }

// Up returns the next tier, saturating at T4.
func (t Tier) Up() Tier {
	switch t {
	case TierT0:
		return TierT1
	case TierT1:
		return TierT2
	case TierT2:
		return TierT3
	default:
		return TierT4
	}
}

// Down returns the previous tier, saturating at T0.
func (t Tier) Down() Tier {
	switch t {
	case TierT4:
		return TierT3
	case TierT3:
		return TierT2
	case TierT2:
		return TierT1
	default:
		return TierT0
	}
}

// RiskLevel classifies a task for policy purposes.
type RiskLevel string

const (
	RiskUnknown  RiskLevel = "unknown"
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

// ConflictKind enumerates the edge types in the merge-risk graph (DESIGN.md §11.6).
type ConflictKind string

const (
	ConflictScopeOverlap      ConflictKind = "scope_overlap"
	ConflictDuplicateIntent   ConflictKind = "duplicate_intent"
	ConflictMergeRisk         ConflictKind = "merge_risk"
	ConflictDependencyCycle   ConflictKind = "dependency_cycle"
	ConflictProtectedResource ConflictKind = "protected_resource"
)

// Severity is shared by conflicts and review findings.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

var severityOrder = map[Severity]int{
	SeverityInfo: 0, SeverityLow: 1, SeverityMedium: 2, SeverityHigh: 3, SeverityCritical: 4,
}

// AtLeast reports whether s is at least as severe as floor.
func (s Severity) AtLeast(floor Severity) bool { return severityOrder[s] >= severityOrder[floor] }

// Outcome enumerates the actions the conflict engine produces (DESIGN.md §6.6). The engine
// returns an action, never a bare score.
type Outcome string

const (
	OutcomeAllow            Outcome = "allow"
	OutcomeAllowWithWarning Outcome = "allow_with_warning"
	OutcomeBlockDuplicate   Outcome = "block_duplicate"
	OutcomeBlockConflict    Outcome = "block_conflict"
	OutcomeSuggestJoin      Outcome = "suggest_join"
	OutcomeSuggestSplit     Outcome = "suggest_split"
	OutcomeSuggestWait      Outcome = "suggest_wait"
	OutcomeSuggestSupersede Outcome = "suggest_supersede"
)

// Blocks reports whether the outcome forbids the caller from proceeding.
func (o Outcome) Blocks() bool {
	return o == OutcomeBlockDuplicate || o == OutcomeBlockConflict
}

// EnforcementLevel enumerates DESIGN.md §11.5.
type EnforcementLevel string

const (
	EnforceAdvisory      EnforcementLevel = "advisory"
	EnforceCooperative   EnforcementLevel = "cooperative"
	EnforceStrictHarness EnforcementLevel = "strict_harness"
	EnforceStrictFS      EnforcementLevel = "strict_filesystem"
)

// Validate returns an error if the enum value is not one this build recognizes. Used at the
// API boundary so a bad value is a 400 rather than a database constraint violation.
func Validate[T ~string](value T, allowed []T, field string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return fmt.Errorf("%w: %s=%q", ErrInvalidEnum, field, string(value))
}
