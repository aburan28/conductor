package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// The shipped .conductor files are documentation as much as configuration, and documentation
// drifts. This loads the repository's own policy and asserts it still produces a working
// runtime config — so a typo in the YAML is a failing test rather than a lease that expires
// instantly in production.
func TestLoadRepositoryPolicy(t *testing.T) {
	root := repoRoot(t)

	bundle, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if bundle.ProjectSHA == "" || bundle.PoliciesSHA == "" ||
		bundle.ModelsSHA == "" || bundle.WorkflowSHA == "" {
		t.Errorf("every policy file should hash: project=%q policies=%q models=%q workflow=%q",
			bundle.ProjectSHA, bundle.PoliciesSHA, bundle.ModelsSHA, bundle.WorkflowSHA)
	}

	cfg := bundle.ProjectConfig()

	// The lease defaults of DESIGN.md §10.1.
	if got := cfg.LeaseTTL.Std(); got != 90*time.Second {
		t.Errorf("lease TTL = %s, want 90s", got)
	}
	if got := cfg.HeartbeatInterval.Std(); got != 20*time.Second {
		t.Errorf("heartbeat = %s, want 20s", got)
	}
	if got := cfg.OfflineGrace.Std(); got != 45*time.Second {
		t.Errorf("offline grace = %s, want 45s", got)
	}
	if got := cfg.StalledTurnTimeout.Std(); got != 900*time.Second {
		t.Errorf("stall timeout = %s, want 900s", got)
	}

	// A heartbeat that is not comfortably shorter than the lease means every session dies
	// on a single missed beat.
	if cfg.HeartbeatInterval.Std()*3 > cfg.LeaseTTL.Std() {
		t.Errorf("heartbeat %s is too close to lease TTL %s; a single missed beat would expire the lease",
			cfg.HeartbeatInterval, cfg.LeaseTTL)
	}

	if cfg.WriteConflict != domain.OutcomeBlockConflict {
		t.Errorf("write/write conflict policy = %s, want block_conflict", cfg.WriteConflict)
	}
	if cfg.DuplicateThreshold <= 0 || cfg.DuplicateThreshold >= 1 {
		t.Errorf("duplicate threshold %.2f is not a usable fraction", cfg.DuplicateThreshold)
	}
	if cfg.MaxConcurrentAttempts <= 0 {
		t.Error("max concurrent attempts must be positive or nothing will ever dispatch")
	}
	if len(cfg.ProtectedScopes) == 0 {
		t.Error("the repository declares no protected scopes; migrations would not be serialized")
	}

	// The matrix policy derived from the file must still refuse a protected/write collision.
	policy := bundle.ScopePolicy()
	if policy.WriteWrite != domain.OutcomeBlockConflict {
		t.Errorf("derived scope policy write/write = %s, want block_conflict", policy.WriteWrite)
	}
}

func TestModelProfilesSkipUnconfiguredModels(t *testing.T) {
	bundle, err := Load(repoRoot(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	profiles := bundle.ModelProfiles("org-1")
	if len(profiles) == 0 {
		t.Fatal("no model profiles loaded")
	}
	for _, p := range profiles {
		if p.Model == "" {
			t.Errorf("profile %s/%s has no model id and should have been skipped", p.Alias, p.Harness)
		}
		if p.OrganizationID != "org-1" {
			t.Errorf("profile %s carries the wrong org", p.Alias)
		}
		if p.Tier == "" || p.ReasoningEffort == "" {
			t.Errorf("profile %s/%s is missing tier or effort", p.Alias, p.Harness)
		}
	}
}

func TestWorkflowFrontmatterParses(t *testing.T) {
	bundle, err := Load(repoRoot(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fm := bundle.Workflow.Frontmatter
	if fm.Version == 0 {
		t.Error("WORKFLOW.md frontmatter did not parse")
	}
	if bundle.Workflow.Body == "" {
		t.Error("WORKFLOW.md has no prose body")
	}
	if len(fm.ProtectedScopes) == 0 {
		t.Error("WORKFLOW.md declares no protected scopes")
	}
}

// A missing .conductor directory must yield working defaults, not an error: a repository
// should be able to adopt Conductor incrementally.
func TestLoadMissingPolicyYieldsDefaults(t *testing.T) {
	bundle, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load on an empty directory: %v", err)
	}
	cfg := bundle.ProjectConfig()
	if cfg.LeaseTTL.Std() != 90*time.Second || cfg.MaxAttempts <= 0 {
		t.Errorf("defaults are not usable: %+v", cfg)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the repository root")
	return ""
}
