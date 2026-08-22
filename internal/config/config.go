// Package config loads the repository-owned policy files of DESIGN.md §20.
//
// Policy lives in the repository, versioned with the code it governs, and every attempt
// records the hash of the files that were in force when it ran (§20.4). That is what makes
// it possible to answer "why did this task route to a frontier model in March but not in
// April" without guessing.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/resource"
)

// Dir is the conventional location of the policy files inside a repository.
const Dir = ".conductor"

// Bundle is everything loaded from a repository's .conductor directory.
type Bundle struct {
	Root        string
	Project     ProjectFile
	Policies    PoliciesFile
	Models      ModelsFile
	Workflow    WorkflowFile
	ProjectSHA  string
	PoliciesSHA string
	ModelsSHA   string
	WorkflowSHA string
}

// ProjectFile mirrors .conductor/project.yaml.
type ProjectFile struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		ID          string `yaml:"id"`
		DisplayName string `yaml:"displayName"`
	} `yaml:"metadata"`
	Repository struct {
		CanonicalRemote string `yaml:"canonicalRemote"`
		DefaultBranch   string `yaml:"defaultBranch"`
		WorktreeRoot    string `yaml:"worktreeRoot"`
	} `yaml:"repository"`
	Coordination struct {
		DefaultVisibility         string `yaml:"defaultVisibility"`
		ClaimMode                 string `yaml:"claimMode"`
		LeaseTTLSeconds           int    `yaml:"leaseTtlSeconds"`
		HeartbeatSeconds          int    `yaml:"heartbeatSeconds"`
		OfflineGraceSeconds       int    `yaml:"offlineGraceSeconds"`
		StartupTimeoutSeconds     int    `yaml:"startupTimeoutSeconds"`
		StalledTurnTimeoutSeconds int    `yaml:"stalledTurnTimeoutSeconds"`
		DuplicatePolicy           string `yaml:"duplicatePolicy"`
		WriteConflictPolicy       string `yaml:"writeConflictPolicy"`
		ReadWriteConflictPolicy   string `yaml:"readWriteConflictPolicy"`
	} `yaml:"coordination"`
	Execution struct {
		Isolation             string `yaml:"isolation"`
		Sandbox               string `yaml:"sandbox"`
		NetworkDefault        string `yaml:"networkDefault"`
		MaxConcurrentAttempts int    `yaml:"maxConcurrentAttempts"`
	} `yaml:"execution"`
	Workflow struct {
		File string `yaml:"file"`
	} `yaml:"workflow"`
	Artifacts struct {
		Backend       string `yaml:"backend"`
		Root          string `yaml:"root"`
		Bucket        string `yaml:"bucket"`
		RetentionDays int    `yaml:"retentionDays"`
	} `yaml:"artifacts"`
	Privacy struct {
		TranscriptStorage              string `yaml:"transcriptStorage"`
		PublishModelIdentity           bool   `yaml:"publishModelIdentity"`
		PublishHarnessIdentity         bool   `yaml:"publishHarnessIdentity"`
		AllowSemanticDuplicateAnalysis bool   `yaml:"allowSemanticDuplicateAnalysis"`
	} `yaml:"privacy"`
}

// PoliciesFile mirrors .conductor/policies.yaml.
type PoliciesFile struct {
	Router struct {
		Rules    []RouterRule `yaml:"rules"`
		Features struct {
			SecuritySensitivePaths     []string `yaml:"security_sensitive_paths"`
			CryptographySensitivePaths []string `yaml:"cryptography_sensitive_paths"`
			SchemaOrMigrationPaths     []string `yaml:"schema_or_migration_paths"`
			InfraPaths                 []string `yaml:"infra_paths"`
		} `yaml:"features"`
	} `yaml:"router"`
	Conflict struct {
		DuplicateExact            string  `yaml:"duplicate_exact"`
		DuplicateSimilarThreshold float64 `yaml:"duplicate_similar_threshold"`
		DuplicateSimilarAction    string  `yaml:"duplicate_similar_action"`
		WriteWrite                string  `yaml:"write_write"`
		ReadWrite                 string  `yaml:"read_write"`
		ProtectedAny              string  `yaml:"protected_any"`
		EnforcementLevel          string  `yaml:"enforcement_level"`
	} `yaml:"conflict"`
	Budget struct {
		Defaults struct {
			Class               string   `yaml:"class"`
			MaxInputTokens      int64    `yaml:"max_input_tokens"`
			MaxOutputTokens     int64    `yaml:"max_output_tokens"`
			MaxReasoningLevel   string   `yaml:"max_reasoning_level"`
			MaxWallClockSeconds int      `yaml:"max_wall_clock_seconds"`
			MaxAttempts         int      `yaml:"max_attempts"`
			MaxEstimatedCostUSD *float64 `yaml:"max_estimated_cost_usd"`
		} `yaml:"defaults"`
		Project struct {
			MonthlyUSD  float64 `yaml:"monthly_usd"`
			DownshiftAt float64 `yaml:"downshift_at"`
			PauseAt     float64 `yaml:"pause_at"`
		} `yaml:"project"`
		Member struct {
			MonthlyTokens int64 `yaml:"monthly_tokens"`
		} `yaml:"member"`
	} `yaml:"budget"`
	Concurrency struct {
		MaxConcurrentAttempts int `yaml:"max_concurrent_attempts"`
		MaxPerPrincipal       int `yaml:"max_per_principal"`
		MaxPerRunner          int `yaml:"max_per_runner"`
		MaxProtectedWriters   int `yaml:"max_protected_writers"`
	} `yaml:"concurrency"`
}

// RouterRule is one hard routing rule (DESIGN.md §13.5). Rules are declarative and
// deterministic; a model recommendation can never route around one.
type RouterRule struct {
	ID                       string `yaml:"id"`
	When                     string `yaml:"when"`
	RequireTier              string `yaml:"require_tier"`
	PreferTier               string `yaml:"prefer_tier"`
	RequireIndependentReview bool   `yaml:"require_independent_review"`
	HumanMergeApproval       bool   `yaml:"human_merge_approval"`
	RequireScope             string `yaml:"require_scope"`
	MaxParallelWriters       int    `yaml:"max_parallel_writers"`
	RequireRollbackPlan      bool   `yaml:"require_rollback_plan"`
	EscalateOneTier          bool   `yaml:"escalate_one_tier"`
	RequireReplan            bool   `yaml:"require_replan"`
}

// ModelsFile mirrors .conductor/models.yaml.
type ModelsFile struct {
	Models struct {
		DefaultAlias string                 `yaml:"defaultAlias"`
		Aliases      map[string]AliasPolicy `yaml:"aliases"`
		Ladder       []string               `yaml:"ladder"`
	} `yaml:"models"`
	Profiles []ProfileSpec `yaml:"profiles"`
}

// AliasPolicy is a role definition: what a model must be able to do, not which model it is.
type AliasPolicy struct {
	CapabilityFloor []string `yaml:"capability_floor"`
	DefaultEffort   string   `yaml:"default_effort"`
	Tier            string   `yaml:"tier"`
}

// ProfileSpec binds an alias to a concrete model for one harness.
type ProfileSpec struct {
	Alias             string   `yaml:"alias"`
	Harness           string   `yaml:"harness"`
	Provider          string   `yaml:"provider"`
	Model             string   `yaml:"model"`
	ReasoningEffort   string   `yaml:"reasoning_effort"`
	Capabilities      []string `yaml:"capabilities"`
	Tier              string   `yaml:"tier"`
	ContextWindow     int      `yaml:"context_window"`
	InputCostPerMTok  *float64 `yaml:"input_cost_per_mtok"`
	OutputCostPerMTok *float64 `yaml:"output_cost_per_mtok"`
	Billing           string   `yaml:"billing"`
	Enabled           *bool    `yaml:"enabled"`
}

// WorkflowFile is WORKFLOW.md: YAML frontmatter plus prose.
type WorkflowFile struct {
	Frontmatter WorkflowFrontmatter
	Body        string
	Raw         string
}

type WorkflowFrontmatter struct {
	Version         int      `yaml:"version"`
	Planner         string   `yaml:"planner"`
	Reviewer        string   `yaml:"reviewer"`
	RequiredChecks  []string `yaml:"required_checks"`
	ProtectedScopes []string `yaml:"protected_scopes"`
	MergePolicy     struct {
		Default          string   `yaml:"default"`
		HumanApprovalFor []string `yaml:"human_approval_for"`
	} `yaml:"merge_policy"`
}

// Load reads the .conductor bundle from a repository root. Missing files fall back to
// defaults so a repository can adopt Conductor incrementally rather than all at once.
func Load(root string) (Bundle, error) {
	b := Bundle{Root: root}
	dir := filepath.Join(root, Dir)

	if err := readYAML(filepath.Join(dir, "project.yaml"), &b.Project, &b.ProjectSHA); err != nil {
		return b, err
	}
	if err := readYAML(filepath.Join(dir, "policies.yaml"), &b.Policies, &b.PoliciesSHA); err != nil {
		return b, err
	}
	if err := readYAML(filepath.Join(dir, "models.yaml"), &b.Models, &b.ModelsSHA); err != nil {
		return b, err
	}

	workflowPath := b.Project.Workflow.File
	if workflowPath == "" {
		workflowPath = filepath.Join(Dir, "WORKFLOW.md")
	}
	wf, sha, err := readWorkflow(filepath.Join(root, workflowPath))
	if err != nil {
		return b, err
	}
	b.Workflow, b.WorkflowSHA = wf, sha
	return b, nil
}

func readYAML(path string, dst any, sha *string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // absent policy means defaults, not failure
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	sum := sha256.Sum256(body)
	*sha = hex.EncodeToString(sum[:])
	return nil
}

// readWorkflow splits YAML frontmatter from the prose body.
func readWorkflow(path string) (WorkflowFile, string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return WorkflowFile{}, "", nil
		}
		return WorkflowFile{}, "", fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	wf := WorkflowFile{Raw: string(body)}
	text := string(body)
	if strings.HasPrefix(text, "---\n") {
		if end := strings.Index(text[4:], "\n---"); end >= 0 {
			front := text[4 : 4+end]
			wf.Body = strings.TrimPrefix(text[4+end+4:], "\n")
			if err := yaml.Unmarshal([]byte(front), &wf.Frontmatter); err != nil {
				return wf, sha, fmt.Errorf("parse %s frontmatter: %w", path, err)
			}
			return wf, sha, nil
		}
	}
	wf.Body = text
	return wf, sha, nil
}

// ProjectConfig converts the bundle into the runtime policy stored on the project row.
func (b Bundle) ProjectConfig() domain.ProjectConfig {
	c := domain.DefaultProjectConfig()

	if v := b.Project.Coordination.DefaultVisibility; v != "" {
		c.DefaultVisibility = domain.Visibility(v)
	}
	if v := b.Project.Coordination.ClaimMode; v != "" {
		c.ClaimMode = domain.EnforcementLevel(v)
	}
	setSeconds(&c.LeaseTTL, b.Project.Coordination.LeaseTTLSeconds)
	setSeconds(&c.HeartbeatInterval, b.Project.Coordination.HeartbeatSeconds)
	setSeconds(&c.OfflineGrace, b.Project.Coordination.OfflineGraceSeconds)
	setSeconds(&c.StartupTimeout, b.Project.Coordination.StartupTimeoutSeconds)
	setSeconds(&c.StalledTurnTimeout, b.Project.Coordination.StalledTurnTimeoutSeconds)

	if v := b.Project.Coordination.DuplicatePolicy; v != "" {
		c.DuplicatePolicy = v
	}
	if v := b.Policies.Conflict.DuplicateSimilarThreshold; v > 0 {
		c.DuplicateThreshold = v
	}
	if v := b.Policies.Conflict.WriteWrite; v != "" {
		c.WriteConflict = domain.Outcome(v)
	} else if v := b.Project.Coordination.WriteConflictPolicy; v == "block" {
		c.WriteConflict = domain.OutcomeBlockConflict
	}
	if v := b.Policies.Conflict.ReadWrite; v != "" {
		c.ReadWriteConflict = domain.Outcome(v)
	} else if v := b.Project.Coordination.ReadWriteConflictPolicy; v == "warn" {
		c.ReadWriteConflict = domain.OutcomeAllowWithWarning
	}

	if v := b.Policies.Concurrency.MaxConcurrentAttempts; v > 0 {
		c.MaxConcurrentAttempts = v
	} else if v := b.Project.Execution.MaxConcurrentAttempts; v > 0 {
		c.MaxConcurrentAttempts = v
	}
	if v := b.Policies.Concurrency.MaxPerPrincipal; v > 0 {
		c.MaxPerPrincipal = v
	}
	if v := b.Policies.Budget.Defaults.MaxAttempts; v > 0 {
		c.MaxAttempts = v
	}

	c.ProtectedScopes = b.Workflow.Frontmatter.ProtectedScopes
	c.RequiredChecks = b.Workflow.Frontmatter.RequiredChecks

	if v := b.Project.Execution.Isolation; v != "" {
		c.Isolation = v
	}
	if v := b.Project.Execution.NetworkDefault; v != "" {
		c.NetworkDefault = v
	}
	c.PublishModelIdentity = b.Project.Privacy.PublishModelIdentity
	c.PublishHarnessIdentity = b.Project.Privacy.PublishHarnessIdentity

	if p := b.Policies.Budget.Project; p.MonthlyUSD > 0 || p.DownshiftAt > 0 || p.PauseAt > 0 {
		c.Budget = domain.BudgetPolicy{
			MonthlyUSD: p.MonthlyUSD, DownshiftAt: p.DownshiftAt, PauseAt: p.PauseAt,
		}
	}
	if v := b.Policies.Budget.Member.MonthlyTokens; v > 0 {
		c.Budget.MemberTokens = v
	}
	return c
}

func setSeconds(dst *domain.Duration, seconds int) {
	if seconds > 0 {
		*dst = domain.Duration(time.Duration(seconds) * time.Second)
	}
}

// ScopePolicy converts the conflict section into the reservation matrix policy.
func (b Bundle) ScopePolicy() resource.Policy {
	p := resource.DefaultPolicy()
	if v := b.Policies.Conflict.WriteWrite; v != "" {
		p.WriteWrite = domain.Outcome(v)
	}
	if v := b.Policies.Conflict.ReadWrite; v != "" {
		p.ReadWrite = domain.Outcome(v)
	}
	if v := b.Policies.Conflict.ProtectedAny; v != "" {
		p.ProtectedVsNonWrite = domain.Outcome(v)
	}
	return p
}

// ScopePolicyFrom derives the matrix policy from a stored project config, for the server,
// which reads policy from the database rather than the filesystem.
func ScopePolicyFrom(c domain.ProjectConfig) resource.Policy {
	p := resource.DefaultPolicy()
	if c.WriteConflict != "" {
		p.WriteWrite = c.WriteConflict
	}
	if c.ReadWriteConflict != "" {
		p.ReadWrite = c.ReadWriteConflict
	}
	return p
}

// ModelProfiles converts the models file into catalog rows for an organization.
//
// A profile with no concrete model id is skipped rather than registered as broken. The
// design forbids hardcoding provider model names we have not verified (§36), so the shipped
// codex and opencode entries are declarations an operator fills in, not defaults.
func (b Bundle) ModelProfiles(orgID domain.ID) []domain.ModelProfile {
	var out []domain.ModelProfile
	for _, spec := range b.Models.Profiles {
		enabled := spec.Model != ""
		if spec.Enabled != nil {
			enabled = *spec.Enabled && spec.Model != ""
		}
		if spec.Model == "" {
			continue
		}
		tier := spec.Tier
		if tier == "" {
			if alias, ok := b.Models.Models.Aliases[spec.Alias]; ok && alias.Tier != "" {
				tier = alias.Tier
			} else {
				tier = string(domain.TierT2)
			}
		}
		effort := spec.ReasoningEffort
		if effort == "" {
			if alias, ok := b.Models.Models.Aliases[spec.Alias]; ok {
				effort = alias.DefaultEffort
			}
		}
		if effort == "" {
			effort = string(domain.EffortMedium)
		}
		billing := spec.Billing
		if billing == "" {
			billing = "tokens"
		}
		out = append(out, domain.ModelProfile{
			OrganizationID:    orgID,
			Alias:             spec.Alias,
			Harness:           spec.Harness,
			Provider:          spec.Provider,
			Model:             spec.Model,
			ReasoningEffort:   domain.Effort(effort),
			Capabilities:      spec.Capabilities,
			Tier:              domain.Tier(tier),
			ContextWindow:     spec.ContextWindow,
			InputCostPerMTok:  spec.InputCostPerMTok,
			OutputCostPerMTok: spec.OutputCostPerMTok,
			Billing:           billing,
			Enabled:           enabled,
			CatalogVersion:    b.ModelsSHA,
		})
	}
	return out
}

// FindRoot walks up from dir looking for a .conductor directory, so CLI commands work from
// anywhere inside a repository.
func FindRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for i := 0; i < 40; i++ {
		if info, err := os.Stat(filepath.Join(abs, Dir)); err == nil && info.IsDir() {
			return abs, nil
		}
		if info, err := os.Stat(filepath.Join(abs, ".git")); err == nil && info.IsDir() {
			// A git repository without .conductor is still the right root; `conductor init`
			// will populate it.
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	return "", fmt.Errorf("no .conductor or .git directory found above %s", dir)
}
