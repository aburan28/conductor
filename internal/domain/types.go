package domain

import "time"

// IDs are UUIDs carried as strings. SQL casts them explicitly (`$1::uuid`, `id::text`) so no
// database-specific UUID type has to leak into the domain package.
type ID = string

type Organization struct {
	ID        ID        `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// DedupeKey is never serialized. It exists on the struct only so the intent engine can
	// receive it from the store; `json:"-"` keeps it out of every API response.
	DedupeKey []byte `json:"-"`
}

type Principal struct {
	ID             ID            `json:"id"`
	OrganizationID ID            `json:"organization_id"`
	Kind           PrincipalKind `json:"kind"`
	Handle         string        `json:"handle"`
	DisplayName    string        `json:"display_name"`
	Email          string        `json:"email,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
}

type Project struct {
	ID              ID            `json:"id"`
	OrganizationID  ID            `json:"organization_id"`
	Slug            string        `json:"slug"`
	DisplayName     string        `json:"display_name"`
	CanonicalRemote string        `json:"canonical_remote"`
	DefaultBranch   string        `json:"default_branch"`
	WorktreeRoot    string        `json:"worktree_root"`
	RepoPath        string        `json:"repo_path"`
	Config          ProjectConfig `json:"config"`
	ConfigSHA       string        `json:"config_sha"`
	WorkflowSHA     string        `json:"workflow_sha"`
	CreatedAt       time.Time     `json:"created_at"`
}

// ProjectConfig is the runtime view of .conductor/project.yaml plus policies.yaml. It is
// stored as jsonb so a project's policy travels with its row and an attempt can be explained
// by the config hash recorded against it (DESIGN.md §20.4).
type ProjectConfig struct {
	DefaultVisibility  Visibility       `json:"default_visibility"`
	ClaimMode          EnforcementLevel `json:"claim_mode"`
	LeaseTTL           Duration         `json:"lease_ttl"`
	HeartbeatInterval  Duration         `json:"heartbeat_interval"`
	OfflineGrace       Duration         `json:"offline_grace"`
	StartupTimeout     Duration         `json:"startup_timeout"`
	StalledTurnTimeout Duration         `json:"stalled_turn_timeout"`

	DuplicateThreshold float64 `json:"duplicate_threshold"`
	DuplicatePolicy    string  `json:"duplicate_policy"`
	WriteConflict      Outcome `json:"write_conflict_policy"`
	ReadWriteConflict  Outcome `json:"read_write_conflict_policy"`

	MaxConcurrentAttempts int `json:"max_concurrent_attempts"`
	MaxPerPrincipal       int `json:"max_per_principal"`
	MaxAttempts           int `json:"max_attempts"`

	ProtectedScopes []string `json:"protected_scopes"`
	RequiredChecks  []string `json:"required_checks"`

	Isolation      string `json:"isolation"`
	NetworkDefault string `json:"network_default"`

	// Publication switches from .conductor/project.yaml §20.2. Model and harness identity
	// are coordination state a project may choose to share; they default to on because
	// "which model produced this" is usually what a reviewer needs first.
	PublishModelIdentity   bool `json:"publish_model_identity"`
	PublishHarnessIdentity bool `json:"publish_harness_identity"`

	Budget BudgetPolicy `json:"budget"`
}

// DefaultProjectConfig returns the defaults from DESIGN.md §10.1 and §20.2. Every value is
// policy, not a constant, and can be overridden per project.
func DefaultProjectConfig() ProjectConfig {
	return ProjectConfig{
		DefaultVisibility:      VisibilityTeamSummary,
		ClaimMode:              EnforceCooperative,
		LeaseTTL:               Duration(90 * time.Second),
		HeartbeatInterval:      Duration(20 * time.Second),
		OfflineGrace:           Duration(45 * time.Second),
		StartupTimeout:         Duration(120 * time.Second),
		StalledTurnTimeout:     Duration(900 * time.Second),
		DuplicateThreshold:     0.60,
		DuplicatePolicy:        "block_exact_warn_similar",
		WriteConflict:          OutcomeBlockConflict,
		ReadWriteConflict:      OutcomeAllowWithWarning,
		MaxConcurrentAttempts:  4,
		MaxPerPrincipal:        2,
		MaxAttempts:            4,
		Isolation:              "git-worktree",
		NetworkDefault:         "deny",
		PublishModelIdentity:   true,
		PublishHarnessIdentity: true,
		Budget:                 DefaultBudgetPolicy(),
	}
}

type BudgetPolicy struct {
	MonthlyUSD  float64 `json:"monthly_usd"`
	DownshiftAt float64 `json:"downshift_at"`
	PauseAt     float64 `json:"pause_at"`
}

func DefaultBudgetPolicy() BudgetPolicy {
	return BudgetPolicy{MonthlyUSD: 0, DownshiftAt: 0.75, PauseAt: 0.95}
}

// Budget is the per-task/attempt envelope of DESIGN.md §13.8.
type Budget struct {
	Class               string   `json:"class,omitempty"`
	MaxInputTokens      int64    `json:"max_input_tokens,omitempty"`
	MaxOutputTokens     int64    `json:"max_output_tokens,omitempty"`
	MaxReasoningLevel   Effort   `json:"max_reasoning_level,omitempty"`
	MaxWallClock        Duration `json:"max_wall_clock,omitempty"`
	MaxAttempts         int      `json:"max_attempts,omitempty"`
	MaxEstimatedCostUSD *float64 `json:"max_estimated_cost_usd,omitempty"`
}

type Membership struct {
	ProjectID   ID        `json:"project_id"`
	PrincipalID ID        `json:"principal_id"`
	Role        Role      `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
}

type Runner struct {
	ID             ID                 `json:"id"`
	OrganizationID ID                 `json:"organization_id"`
	ProjectID      ID                 `json:"project_id,omitempty"`
	PrincipalID    ID                 `json:"principal_id,omitempty"`
	Name           string             `json:"name"`
	Capabilities   RunnerCapabilities `json:"capabilities"`
	MaxConcurrency int                `json:"max_concurrency"`
	InFlight       int                `json:"in_flight"`
	State          string             `json:"state"`
	RegisteredAt   time.Time          `json:"registered_at"`
	HeartbeatAt    time.Time          `json:"heartbeat_at"`
}

// RunnerCapabilities is what a runner advertises (DESIGN.md §7.10).
type RunnerCapabilities struct {
	Harnesses    []string `json:"harnesses"`
	Models       []string `json:"models"`
	Sandbox      string   `json:"sandbox"`
	Platform     string   `json:"platform"`
	RepoPath     string   `json:"repo_path"`
	WorktreeRoot string   `json:"worktree_root"`
}

type Session struct {
	ID             ID           `json:"id"`
	ProjectID      ID           `json:"project_id"`
	PrincipalID    ID           `json:"principal_id"`
	RunnerID       ID           `json:"runner_id,omitempty"`
	Harness        string       `json:"harness"`
	HarnessVersion string       `json:"harness_version,omitempty"`
	MachineID      string       `json:"machine_id,omitempty"`
	BaseSHA        string       `json:"base_sha,omitempty"`
	Branch         string       `json:"branch,omitempty"`
	WorktreePath   string       `json:"worktree_path,omitempty"`
	Visibility     Visibility   `json:"visibility"`
	ActiveTaskID   ID           `json:"active_task_id,omitempty"`
	State          SessionState `json:"state"`
	StartedAt      time.Time    `json:"started_at"`
	HeartbeatAt    time.Time    `json:"heartbeat_at"`
	ExpiresAt      time.Time    `json:"expires_at"`
	ClosedAt       *time.Time   `json:"closed_at,omitempty"`
}

type AcceptanceCriterion struct {
	Text     string `json:"text"`
	Status   string `json:"status,omitempty"`   // pending | pass | fail
	Evidence string `json:"evidence,omitempty"` // command_id of the validation that proves it
}

type Task struct {
	ID                 ID                    `json:"id"`
	OrganizationID     ID                    `json:"organization_id"`
	ProjectID          ID                    `json:"project_id"`
	Ref                string                `json:"ref"`
	ParentID           ID                    `json:"parent_id,omitempty"`
	ExternalRef        string                `json:"external_ref,omitempty"`
	Title              string                `json:"title"`
	Objective          string                `json:"objective,omitempty"`
	AcceptanceCriteria []AcceptanceCriterion `json:"acceptance_criteria"`
	Status             TaskStatus            `json:"status"`
	Visibility         Visibility            `json:"visibility"`
	Priority           int                   `json:"priority"`
	RiskLevel          RiskLevel             `json:"risk_level"`
	BaseSHA            string                `json:"base_sha,omitempty"`
	WorkflowSHA        string                `json:"workflow_sha,omitempty"`
	ActiveLeaseID      ID                    `json:"active_lease_id,omitempty"`
	FencingEpoch       int64                 `json:"fencing_epoch"`
	ModelAlias         string                `json:"model_alias,omitempty"`
	HarnessPref        string                `json:"harness_pref,omitempty"`
	Budget             Budget                `json:"budget"`
	Features           TaskFeatures          `json:"features"`
	AttemptsCount      int                   `json:"attempts_count"`
	MaxAttempts        int                   `json:"max_attempts"`
	SupersededBy       ID                    `json:"superseded_by,omitempty"`
	CreatedBy          ID                    `json:"created_by"`
	CreatedAt          time.Time             `json:"created_at"`
	UpdatedAt          time.Time             `json:"updated_at"`
	CompletedAt        *time.Time            `json:"completed_at,omitempty"`

	// Fingerprint and MinHash are coordination metadata, never rendered to another
	// principal. See internal/privacy for why they can be stored without exposing text.
	Fingerprint string  `json:"-"`
	MinHash     []int64 `json:"-"`

	// Populated by joins when requested.
	DependsOn    []ID               `json:"depends_on,omitempty"`
	Reservations []ScopeReservation `json:"reservations,omitempty"`
}

// TaskFeatures is the deterministic feature record the router consumes (DESIGN.md §13.3).
// Every field is derivable from scopes, paths, dependency structure, and attempt history —
// none of it requires reading a prompt.
type TaskFeatures struct {
	EstimatedFiles        int      `json:"estimated_files"`
	EstimatedLOC          int      `json:"estimated_loc"`
	Languages             []string `json:"languages,omitempty"`
	CrossModuleEdges      int      `json:"cross_module_edges"`
	PublicAPIChange       bool     `json:"public_api_change"`
	SchemaOrMigration     bool     `json:"schema_or_migration"`
	SecuritySensitive     bool     `json:"security_sensitive"`
	CryptographySensitive bool     `json:"cryptography_sensitive"`
	InfraOrDeployment     bool     `json:"infra_or_deployment"`
	UnknownCodeRatio      float64  `json:"unknown_code_ratio"`
	TestCoverageSignal    float64  `json:"test_coverage_signal"`
	AcceptanceAmbiguity   float64  `json:"acceptance_ambiguity"`
	PlannerConfidence     float64  `json:"planner_confidence"`
	ScopeConflictScore    float64  `json:"scope_conflict_score"`
	PriorAttemptFailures  int      `json:"prior_attempt_failures"`
	ReviewRejections      int      `json:"review_rejections"`
	BaseBranchDrift       int      `json:"base_branch_drift"`
	LatencyPriority       float64  `json:"latency_priority"`
	ScopeGrowthRatio      float64  `json:"scope_growth_ratio"`
}

type Attempt struct {
	ID                ID           `json:"id"`
	TaskID            ID           `json:"task_id"`
	ProjectID         ID           `json:"project_id"`
	AttemptNumber     int          `json:"attempt_number"`
	SponsorPrincipal  ID           `json:"sponsor_principal"`
	ExecutorPrincipal ID           `json:"executor_principal,omitempty"`
	RunnerID          ID           `json:"runner_id,omitempty"`
	SessionID         ID           `json:"session_id,omitempty"`
	Role              AgentRole    `json:"role"`
	Harness           string       `json:"harness"`
	ModelProfileID    ID           `json:"model_profile_id,omitempty"`
	ModelAlias        string       `json:"model_alias"`
	ResolvedModel     string       `json:"resolved_model"`
	ReasoningEffort   Effort       `json:"reasoning_effort"`
	State             AttemptState `json:"state"`
	Branch            string       `json:"branch,omitempty"`
	WorktreePath      string       `json:"worktree_path,omitempty"`
	BaseCommitSHA     string       `json:"base_commit_sha,omitempty"`
	CommitSHA         string       `json:"commit_sha,omitempty"`
	ChangedPaths      []string     `json:"changed_paths,omitempty"`
	TokensIn          int64        `json:"tokens_in"`
	TokensOut         int64        `json:"tokens_out"`
	CostUSD           float64      `json:"cost_usd"`
	Turns             int          `json:"turns"`
	WorkflowSHA       string       `json:"workflow_sha,omitempty"`
	ProjectConfigSHA  string       `json:"project_config_sha,omitempty"`
	RouterPolicyVer   string       `json:"router_policy_version,omitempty"`
	ModelCatalogVer   string       `json:"model_catalog_version,omitempty"`
	FailureClass      string       `json:"failure_class,omitempty"`
	FailureSummary    string       `json:"failure_summary,omitempty"`
	StartedAt         *time.Time   `json:"started_at,omitempty"`
	EndedAt           *time.Time   `json:"ended_at,omitempty"`
	LastEventAt       time.Time    `json:"last_event_at"`
	CreatedAt         time.Time    `json:"created_at"`
}

type Lease struct {
	ID              ID         `json:"id"`
	TaskID          ID         `json:"task_id"`
	AttemptID       ID         `json:"attempt_id"`
	ProjectID       ID         `json:"project_id"`
	FencingEpoch    int64      `json:"fencing_epoch"`
	HolderPrincipal ID         `json:"holder_principal"`
	SessionID       ID         `json:"session_id,omitempty"`
	AcquiredAt      time.Time  `json:"acquired_at"`
	HeartbeatAt     time.Time  `json:"heartbeat_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	ReleasedAt      *time.Time `json:"released_at,omitempty"`
	ReleaseReason   string     `json:"release_reason,omitempty"`
}

// Active reports whether the lease is unreleased and unexpired as of now.
func (l Lease) Active(now time.Time) bool {
	return l.ReleasedAt == nil && l.ExpiresAt.After(now)
}

// Fence is the tuple every execution-time mutation must present (DESIGN.md §7.5).
type Fence struct {
	TaskID       ID    `json:"task_id"`
	AttemptID    ID    `json:"attempt_id"`
	LeaseID      ID    `json:"lease_id"`
	FencingEpoch int64 `json:"fencing_epoch"`
}

type ScopeReservation struct {
	ID               ID                `json:"id"`
	ProjectID        ID                `json:"project_id"`
	TaskID           ID                `json:"task_id"`
	AttemptID        ID                `json:"attempt_id,omitempty"`
	LeaseID          ID                `json:"lease_id,omitempty"`
	PrincipalID      ID                `json:"principal_id"`
	ResourceType     ResourceType      `json:"resource_type"`
	ResourceKey      string            `json:"resource_key"`
	Mode             ReservationMode   `json:"mode"`
	Source           ReservationSource `json:"source"`
	AlternativeGroup string            `json:"alternative_group,omitempty"`
	Active           bool              `json:"active"`
	CreatedAt        time.Time         `json:"created_at"`
	ReleasedAt       *time.Time        `json:"released_at,omitempty"`
}

// Resource renders the reservation as its canonical `type:key` string.
func (r ScopeReservation) Resource() string {
	return string(r.ResourceType) + ":" + r.ResourceKey
}

type Artifact struct {
	ID            ID         `json:"id"`
	ProjectID     ID         `json:"project_id"`
	TaskID        ID         `json:"task_id"`
	AttemptID     ID         `json:"attempt_id,omitempty"`
	Kind          string     `json:"kind"`
	URI           string     `json:"uri"`
	ContentSHA256 string     `json:"content_sha256"`
	SizeBytes     int64      `json:"size_bytes"`
	Visibility    Visibility `json:"visibility"`
	CreatedAt     time.Time  `json:"created_at"`
}

type ValidationResult struct {
	ID         ID        `json:"id"`
	AttemptID  ID        `json:"attempt_id"`
	TaskID     ID        `json:"task_id"`
	CommandID  string    `json:"command_id"`
	Command    string    `json:"command"`
	ExitCode   int       `json:"exit_code"`
	DurationMS int64     `json:"duration_ms"`
	ArtifactID ID        `json:"artifact_id,omitempty"`
	RunnerID   ID        `json:"runner_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// EvidenceManifest is the worker result contract of DESIGN.md §15.1. A completion is
// accepted only with a manifest; a model asserting success is not evidence.
type EvidenceManifest struct {
	TaskID            ID                    `json:"task_id"`
	AttemptID         ID                    `json:"attempt_id"`
	LeaseID           ID                    `json:"lease_id"`
	FencingEpoch      int64                 `json:"fencing_epoch"`
	BaseSHA           string                `json:"base_sha"`
	CommitSHA         string                `json:"commit_sha"`
	DiffSHA256        string                `json:"diff_sha256"`
	WorkflowSHA       string                `json:"workflow_sha"`
	Commands          []ValidationResult    `json:"commands"`
	ChangedPaths      []string              `json:"changed_paths"`
	AcceptanceResults []AcceptanceCriterion `json:"acceptance_results"`
	RunnerID          ID                    `json:"runner_id"`
	CreatedAt         time.Time             `json:"created_at"`
}

type ReviewFinding struct {
	ID                ID        `json:"id"`
	TaskID            ID        `json:"task_id"`
	AttemptID         ID        `json:"attempt_id,omitempty"`
	ReviewerPrincipal ID        `json:"reviewer_principal,omitempty"`
	Severity          Severity  `json:"severity"`
	Category          string    `json:"category"`
	Path              string    `json:"path,omitempty"`
	Line              int       `json:"line,omitempty"`
	Summary           string    `json:"summary"`
	Resolved          bool      `json:"resolved"`
	CreatedAt         time.Time `json:"created_at"`
}

// DecisionRecord is one durable decision carried across attempts and harnesses.
type DecisionRecord struct {
	At      time.Time `json:"at"`
	By      string    `json:"by"`
	Summary string    `json:"summary"`
	Reason  string    `json:"reason,omitempty"`
}

type Blocker struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	TaskRef string `json:"task_ref,omitempty"`
}

// HandoffBundle is DESIGN.md §22: the minimum state needed to continue a task in another
// session or harness. It is a structured summary by construction — there is no field here
// that can carry a transcript.
type HandoffBundle struct {
	TaskID                ID                    `json:"task_id"`
	TaskRef               string                `json:"task_ref"`
	FromAttemptID         ID                    `json:"from_attempt_id,omitempty"`
	CreatedAt             time.Time             `json:"created_at"`
	BaseSHA               string                `json:"base_sha"`
	Branch                string                `json:"branch,omitempty"`
	CommitSHA             string                `json:"commit_sha,omitempty"`
	PatchArtifactID       ID                    `json:"patch_artifact_id,omitempty"`
	Objective             string                `json:"objective"`
	AcceptanceCriteria    []AcceptanceCriterion `json:"acceptance_criteria"`
	CompletedWork         []string              `json:"completed_work"`
	Decisions             []DecisionRecord      `json:"decisions"`
	Assumptions           []string              `json:"assumptions"`
	OpenQuestions         []string              `json:"open_questions"`
	Blockers              []Blocker             `json:"blockers"`
	Scopes                []ScopeReservation    `json:"scopes"`
	Validation            []ValidationResult    `json:"validation"`
	RecommendedNextAction string                `json:"recommended_next_action"`
	RecommendedRole       AgentRole             `json:"recommended_role,omitempty"`
	Visibility            Visibility            `json:"visibility"`
}

type Handoff struct {
	ID            ID            `json:"id"`
	TaskID        ID            `json:"task_id"`
	FromAttemptID ID            `json:"from_attempt_id,omitempty"`
	ToAttemptID   ID            `json:"to_attempt_id,omitempty"`
	ToHarness     string        `json:"to_harness,omitempty"`
	ToRole        string        `json:"to_role,omitempty"`
	Visibility    Visibility    `json:"visibility"`
	Bundle        HandoffBundle `json:"bundle"`
	CreatedBy     ID            `json:"created_by"`
	CreatedAt     time.Time     `json:"created_at"`
}

// Intent is the sanitized coordination envelope of DESIGN.md §12.1. Note what is absent:
// there is no prompt field, and PublicSummary is supplied by the caller under an explicit
// visibility choice — the server never derives it from conversation.
type Intent struct {
	ID            ID             `json:"id"`
	ProjectID     ID             `json:"project_id"`
	SessionID     ID             `json:"session_id,omitempty"`
	PrincipalID   ID             `json:"principal_id"`
	TaskID        ID             `json:"task_id,omitempty"`
	ExternalRef   string         `json:"external_ref,omitempty"`
	Visibility    Visibility     `json:"visibility"`
	PublicSummary string         `json:"public_summary,omitempty"`
	Verb          string         `json:"verb,omitempty"`
	Resource      string         `json:"resource,omitempty"`
	Fingerprint   string         `json:"-"`
	MinHash       []int64        `json:"-"`
	Scopes        []ScopeRequest `json:"scopes"`
	Outcome       Outcome        `json:"outcome"`
	CreatedAt     time.Time      `json:"created_at"`
	ExpiresAt     time.Time      `json:"expires_at"`
}

// ScopeRequest is a requested reservation before it is granted.
type ScopeRequest struct {
	Resource         string          `json:"resource"` // canonical "type:key"
	Mode             ReservationMode `json:"mode"`
	AlternativeGroup string          `json:"alternative_group,omitempty"`
}

type ConflictEdge struct {
	ID             ID             `json:"id"`
	ProjectID      ID             `json:"project_id"`
	TaskA          ID             `json:"task_a"`
	TaskB          ID             `json:"task_b"`
	Kind           ConflictKind   `json:"kind"`
	Severity       Severity       `json:"severity"`
	Weight         float64        `json:"weight"`
	Suggestion     Outcome        `json:"suggestion"`
	Detail         ConflictDetail `json:"detail"`
	State          string         `json:"state"`
	ResolutionNote string         `json:"resolution_note,omitempty"`
	ResolvedBy     ID             `json:"resolved_by,omitempty"`
	DetectedAt     time.Time      `json:"detected_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	ResolvedAt     *time.Time     `json:"resolved_at,omitempty"`
}

// ConflictDetail explains an edge in coordination terms. It names resources and task refs,
// never content: a member on the other side of a private task learns which file is
// contested, not why the other person wants it.
type ConflictDetail struct {
	Resources   []string `json:"resources,omitempty"`
	Similarity  float64  `json:"similarity,omitempty"`
	SharedPaths []string `json:"shared_paths,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	TaskARef    string   `json:"task_a_ref,omitempty"`
	TaskBRef    string   `json:"task_b_ref,omitempty"`
	TaskAOwner  string   `json:"task_a_owner,omitempty"`
	TaskBOwner  string   `json:"task_b_owner,omitempty"`
}

type PolicyDecision struct {
	ID        ID             `json:"id"`
	ProjectID ID             `json:"project_id"`
	TaskID    ID             `json:"task_id,omitempty"`
	AttemptID ID             `json:"attempt_id,omitempty"`
	Kind      string         `json:"kind"`
	Decision  string         `json:"decision"`
	Rationale map[string]any `json:"rationale"`
	CreatedAt time.Time      `json:"created_at"`
}

type Event struct {
	ID             ID             `json:"id"`
	OrganizationID ID             `json:"organization_id"`
	ProjectID      ID             `json:"project_id"`
	AggregateType  string         `json:"aggregate_type"`
	AggregateID    ID             `json:"aggregate_id"`
	SequenceNumber int64          `json:"sequence_number"`
	Type           string         `json:"type"`
	ActorPrincipal ID             `json:"actor_principal,omitempty"`
	Visibility     Visibility     `json:"visibility"`
	Payload        map[string]any `json:"payload"`
	OccurredAt     time.Time      `json:"occurred_at"`
}

type ModelProfile struct {
	ID                ID       `json:"id"`
	OrganizationID    ID       `json:"organization_id"`
	Alias             string   `json:"alias"`
	Harness           string   `json:"harness"`
	Provider          string   `json:"provider"`
	Model             string   `json:"model"`
	ReasoningEffort   Effort   `json:"reasoning_effort"`
	Capabilities      []string `json:"capabilities"`
	Tier              Tier     `json:"tier"`
	ContextWindow     int      `json:"context_window"`
	InputCostPerMTok  *float64 `json:"input_cost_per_mtok,omitempty"`
	OutputCostPerMTok *float64 `json:"output_cost_per_mtok,omitempty"`
	Billing           string   `json:"billing"`
	Enabled           bool     `json:"enabled"`
	CatalogVersion    string   `json:"catalog_version"`
}

// HasCapabilities reports whether the profile satisfies every capability in floor.
func (p ModelProfile) HasCapabilities(floor []string) bool {
	have := make(map[string]bool, len(p.Capabilities))
	for _, c := range p.Capabilities {
		have[c] = true
	}
	for _, need := range floor {
		if !have[need] {
			return false
		}
	}
	return true
}

// PresenceEntry is the entire cross-principal surface (DESIGN.md §8.5). Adding a field here
// is a privacy decision, which is why the struct is small, explicit, and separate from
// Session.
type PresenceEntry struct {
	SessionID     ID            `json:"session_id"`
	Principal     string        `json:"principal"`
	PrincipalID   ID            `json:"principal_id"`
	Kind          PrincipalKind `json:"kind"`
	Harness       string        `json:"harness"`
	State         SessionState  `json:"state"`
	TaskRef       string        `json:"task_ref,omitempty"`
	TaskTitle     string        `json:"task_title,omitempty"`
	TaskStatus    TaskStatus    `json:"task_status,omitempty"`
	Branch        string        `json:"branch,omitempty"`
	Scopes        []string      `json:"scopes"`
	SponsoredBy   string        `json:"sponsored_by,omitempty"`
	StartedAt     time.Time     `json:"started_at"`
	LastHeartbeat time.Time     `json:"last_heartbeat"`
}
