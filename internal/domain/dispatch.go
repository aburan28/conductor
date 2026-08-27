package domain

import (
	"fmt"
	"strings"
	"time"
)

// Dispatch policy (.conductor/dispatch.yaml).
//
// The router of DESIGN.md §13 decides what a task *needs* — a tier, an effort, a capability
// floor. The dispatch policy is where a team says which concrete models it wants that need
// met by, in what order, and under which conditions: "docs go to the local Qwen, everything
// else to Sonnet, escalate to Opus after two failures". It lives in the repository like every
// other policy file, is stored on the project row at bootstrap so a runner anywhere applies
// the same rules, and every decision it produces carries the rule and candidate that won.
//
// These types are plain data. Evaluation — parsing `when` expressions, matching candidates,
// explaining the choice — lives in internal/policy.

// DispatchPolicy is the whole file.
type DispatchPolicy struct {
	Version  int                     `json:"version" yaml:"version"`
	Lanes    map[string]DispatchLane `json:"lanes" yaml:"lanes"`
	Rules    []DispatchRule          `json:"rules,omitempty" yaml:"rules"`
	Defaults DispatchDefaults        `json:"defaults" yaml:"defaults"`
	Limits   []DispatchLimit         `json:"limits,omitempty" yaml:"limits"`
}

// DispatchLane is one named execution path: a role and an ordered ladder of candidate
// models. The first eligible candidate wins; escalation walks down the ladder.
type DispatchLane struct {
	Role       AgentRole           `json:"role" yaml:"role"`
	Candidates []DispatchCandidate `json:"candidates" yaml:"candidates"`
	// Review "required" means every attempt on this lane must be independently reviewed
	// before merge, regardless of tier.
	Review string `json:"review,omitempty" yaml:"review"`
}

// DispatchCandidate is one concrete model on one harness.
type DispatchCandidate struct {
	// Model is the harness's own model id: `claude-sonnet-5`, `gpt-5-codex`,
	// `ollama/qwen3:27b` (OpenCode addresses models as provider/model).
	Model    string `json:"model,omitempty" yaml:"model"`
	Harness  string `json:"harness,omitempty" yaml:"harness"`
	Provider string `json:"provider,omitempty" yaml:"provider"`
	// Alias names a models.yaml alias instead of a concrete model; the catalog resolves it.
	Alias  string `json:"alias,omitempty" yaml:"alias"`
	Effort Effort `json:"effort,omitempty" yaml:"effort"`
	// When restricts this candidate to tasks matching the expression. Empty means always.
	When string `json:"when,omitempty" yaml:"when"`
	// MaxConcurrent caps how many attempts may run on this candidate at once, project-wide.
	// A local model with one GPU is the obvious case.
	MaxConcurrent int `json:"max_concurrent,omitempty" yaml:"max_concurrent"`
	// Tags are free-form labels rules can prefer: "local", "cheap", "frontier".
	Tags []string `json:"tags,omitempty" yaml:"tags"`
}

// Name renders the candidate as harness/model for rationale and display.
func (c DispatchCandidate) Name() string {
	model := c.Model
	if model == "" {
		model = "alias:" + c.Alias
	}
	if c.Harness == "" {
		return model
	}
	return c.Harness + ":" + model
}

// HasTag reports whether the candidate carries a tag.
func (c DispatchCandidate) HasTag(tag string) bool {
	for _, t := range c.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// DispatchRule chooses a lane and adjusts the choice for tasks matching `when`. Rules are
// evaluated in order; a later matching rule overrides an earlier one field by field, and
// `stop: true` ends evaluation.
type DispatchRule struct {
	ID   string `json:"id" yaml:"id"`
	When string `json:"when" yaml:"when"`
	Lane string `json:"lane,omitempty" yaml:"lane"`
	// Prefer moves matching candidates to the front of the ladder without excluding the
	// rest. Require is a floor: candidates that do not meet it are ineligible.
	Prefer  *DispatchSelector      `json:"prefer,omitempty" yaml:"prefer"`
	Require *CapabilityRequirement `json:"require,omitempty" yaml:"require"`
	Review  string                 `json:"review,omitempty" yaml:"review"`
	// Effort overrides the winning candidate's effort.
	Effort Effort `json:"effort,omitempty" yaml:"effort"`
	Stop   bool   `json:"stop,omitempty" yaml:"stop"`
}

// DispatchSelector names candidates by model, harness, or tag. Empty fields match anything.
type DispatchSelector struct {
	Model   string `json:"model,omitempty" yaml:"model"`
	Harness string `json:"harness,omitempty" yaml:"harness"`
	Tag     string `json:"tag,omitempty" yaml:"tag"`
}

// Matches reports whether the selector selects the candidate.
func (s DispatchSelector) Matches(c DispatchCandidate) bool {
	if s.Model != "" && !strings.EqualFold(s.Model, c.Model) && !strings.EqualFold(s.Model, c.Alias) {
		return false
	}
	if s.Harness != "" && !strings.EqualFold(s.Harness, c.Harness) {
		return false
	}
	if s.Tag != "" && !c.HasTag(s.Tag) {
		return false
	}
	return true
}

// Empty reports whether the selector constrains nothing.
func (s DispatchSelector) Empty() bool { return s.Model == "" && s.Harness == "" && s.Tag == "" }

func (s DispatchSelector) String() string {
	var parts []string
	if s.Harness != "" {
		parts = append(parts, "harness="+s.Harness)
	}
	if s.Model != "" {
		parts = append(parts, "model="+s.Model)
	}
	if s.Tag != "" {
		parts = append(parts, "tag="+s.Tag)
	}
	if len(parts) == 0 {
		return "anything"
	}
	return strings.Join(parts, " ")
}

// DispatchDefaults applies when no rule says otherwise.
type DispatchDefaults struct {
	Lane string `json:"lane,omitempty" yaml:"lane"`
	// OnFailure: escalate (walk down the ladder on each failure), retry (same candidate),
	// stop (fail the task after the first failure).
	OnFailure      string `json:"on_failure,omitempty" yaml:"on_failure"`
	MaxEscalations int    `json:"max_escalations,omitempty" yaml:"max_escalations"`
}

// DispatchLimit caps concurrent attempts across every candidate the selector matches.
type DispatchLimit struct {
	Match         DispatchSelector `json:"match" yaml:"match"`
	MaxConcurrent int              `json:"max_concurrent" yaml:"max_concurrent"`
}

// Empty reports whether the policy declares nothing at all.
func (p *DispatchPolicy) Empty() bool {
	return p == nil || len(p.Lanes) == 0
}

// DispatchDecision is what the policy chose for one attempt, and why. It is recorded as a
// policy decision on the attempt, so "why did this run on Qwen" is always answerable.
type DispatchDecision struct {
	Lane     string    `json:"lane"`
	Role     AgentRole `json:"role"`
	Harness  string    `json:"harness"`
	Model    string    `json:"model"`
	Provider string    `json:"provider,omitempty"`
	Alias    string    `json:"alias,omitempty"`
	Effort   Effort    `json:"reasoning_effort,omitempty"`
	Tier     Tier      `json:"tier,omitempty"`
	// Review is "required" when a rule or lane demands independent review.
	Review string `json:"review,omitempty"`
	// Rules lists the ids of the rules that matched, in order.
	Rules []string `json:"rules,omitempty"`
	// Candidates records every candidate considered on the lane and why it was or was not
	// eligible, so a rejected choice is as explainable as the winning one.
	Candidates []DispatchCandidateVerdict `json:"candidates,omitempty"`
	Rationale  []string                   `json:"rationale"`
	// Escalations is how many ladder steps failure history walked past.
	Escalations int `json:"escalations,omitempty"`
}

// DispatchCandidateVerdict is one row of the explanation.
type DispatchCandidateVerdict struct {
	Lane     string `json:"lane"`
	Model    string `json:"model"`
	Harness  string `json:"harness"`
	Effort   Effort `json:"reasoning_effort,omitempty"`
	Tier     Tier   `json:"tier,omitempty"`
	Eligible bool   `json:"eligible"`
	Chosen   bool   `json:"chosen,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// RouterRule is one hard routing rule of DESIGN.md §13.5, as stored on the project. The
// `when` expression is evaluated by internal/policy; a model recommendation can never route
// around one.
type RouterRule struct {
	ID                       string `json:"id" yaml:"id"`
	When                     string `json:"when" yaml:"when"`
	RequireTier              Tier   `json:"require_tier,omitempty" yaml:"require_tier"`
	PreferTier               Tier   `json:"prefer_tier,omitempty" yaml:"prefer_tier"`
	RequireIndependentReview bool   `json:"require_independent_review,omitempty" yaml:"require_independent_review"`
	HumanMergeApproval       bool   `json:"human_merge_approval,omitempty" yaml:"human_merge_approval"`
	RequireScope             string `json:"require_scope,omitempty" yaml:"require_scope"`
	MaxParallelWriters       int    `json:"max_parallel_writers,omitempty" yaml:"max_parallel_writers"`
	RequireRollbackPlan      bool   `json:"require_rollback_plan,omitempty" yaml:"require_rollback_plan"`
	EscalateOneTier          bool   `json:"escalate_one_tier,omitempty" yaml:"escalate_one_tier"`
	RequireReplan            bool   `json:"require_replan,omitempty" yaml:"require_replan"`
}

// FeaturePathPolicy declares which resource patterns mark a task as sensitive. It comes from
// policies.yaml, so a project decides what "security sensitive" means in its own tree.
type FeaturePathPolicy struct {
	SecuritySensitivePaths     []string `json:"security_sensitive_paths,omitempty" yaml:"security_sensitive_paths"`
	CryptographySensitivePaths []string `json:"cryptography_sensitive_paths,omitempty" yaml:"cryptography_sensitive_paths"`
	SchemaOrMigrationPaths     []string `json:"schema_or_migration_paths,omitempty" yaml:"schema_or_migration_paths"`
	InfraPaths                 []string `json:"infra_paths,omitempty" yaml:"infra_paths"`
}

// Empty reports whether the project declared no feature paths of its own.
func (f FeaturePathPolicy) Empty() bool {
	return len(f.SecuritySensitivePaths) == 0 && len(f.CryptographySensitivePaths) == 0 &&
		len(f.SchemaOrMigrationPaths) == 0 && len(f.InfraPaths) == 0
}

// ---------------------------------------------------------------------------
// Admission queue
// ---------------------------------------------------------------------------

// TicketKind says what an admission ticket admits.
type TicketKind string

const (
	// TicketSession admits one interactive session (a `conductor wrap`).
	TicketSession TicketKind = "session"
	// TicketAttempt admits one runner attempt on a task.
	TicketAttempt TicketKind = "attempt"
)

// TicketState is the lifecycle of an admission ticket.
type TicketState string

const (
	TicketQueued    TicketState = "queued"
	TicketGranted   TicketState = "granted"
	TicketReleased  TicketState = "released"
	TicketExpired   TicketState = "expired"
	TicketCancelled TicketState = "cancelled"
)

var AllTicketStates = []TicketState{TicketQueued, TicketGranted, TicketReleased, TicketExpired, TicketCancelled}

// Open reports whether the ticket still occupies or waits for a slot.
func (s TicketState) Open() bool { return s == TicketQueued || s == TicketGranted }

// AdmissionTicket is one place in the admission queue (DESIGN.md §7.7 concurrency).
//
// When a project caps how many sessions or attempts may be active at once, work that arrives
// past the cap does not fail: it takes a ticket and waits. The scheduler grants tickets in
// order as slots free up, and a granted ticket must be heartbeated or it expires and its slot
// is handed on. Tickets carry identity, kind, and a model name — never what the work is about.
type AdmissionTicket struct {
	ID          ID          `json:"id"`
	ProjectID   ID          `json:"project_id"`
	PrincipalID ID          `json:"principal_id"`
	Principal   string      `json:"principal,omitempty"`
	SessionID   ID          `json:"session_id,omitempty"`
	TaskID      ID          `json:"task_id,omitempty"`
	TaskRef     string      `json:"task_ref,omitempty"`
	Kind        TicketKind  `json:"kind"`
	Harness     string      `json:"harness,omitempty"`
	Model       string      `json:"model,omitempty"`
	Priority    int         `json:"priority"`
	State       TicketState `json:"state"`
	// Position is 1-based among queued tickets, populated on read; 0 once granted.
	Position    int        `json:"position,omitempty"`
	Note        string     `json:"note,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	GrantedAt   *time.Time `json:"granted_at,omitempty"`
	HeartbeatAt time.Time  `json:"heartbeat_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ReleasedAt  *time.Time `json:"released_at,omitempty"`
}

// Describe renders a ticket for a terminal line.
func (t AdmissionTicket) Describe() string {
	what := string(t.Kind)
	if t.TaskRef != "" {
		what += " " + t.TaskRef
	}
	if t.Model != "" {
		what += " on " + t.Model
	}
	return fmt.Sprintf("%s (%s)", what, t.State)
}

// QueuePolicy is the project's admission limits. Zero means unlimited.
type QueuePolicy struct {
	MaxActiveSessions       int `json:"max_active_sessions,omitempty"`
	MaxSessionsPerPrincipal int `json:"max_sessions_per_principal,omitempty"`
	MaxConcurrentAttempts   int `json:"max_concurrent_attempts,omitempty"`
	// TicketTTLSeconds is how long a granted ticket lives without a heartbeat, and how long
	// a queued one waits before it is dropped as abandoned.
	TicketTTLSeconds int `json:"ticket_ttl_seconds,omitempty"`
}

// Enabled reports whether any session cap is set, which is what makes wrap take a ticket.
func (q QueuePolicy) Enabled() bool {
	return q.MaxActiveSessions > 0 || q.MaxSessionsPerPrincipal > 0
}

// TicketTTL returns the configured TTL or a default.
func (q QueuePolicy) TicketTTL() time.Duration {
	if q.TicketTTLSeconds > 0 {
		return time.Duration(q.TicketTTLSeconds) * time.Second
	}
	return 90 * time.Second
}
