package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/policy"
)

func price(v float64) *float64 { return &v }

func testCatalog() Catalog {
	return Catalog{
		Version: "test",
		Aliases: map[string]AliasPolicy{
			"worker.fast":      {CapabilityFloor: []string{"code_edit"}, DefaultEffort: domain.EffortLow, Tier: domain.TierT1},
			"worker.general":   {CapabilityFloor: []string{"code_edit", "multi_file"}, DefaultEffort: domain.EffortMedium, Tier: domain.TierT2},
			"planner.frontier": {CapabilityFloor: []string{"architecture"}, DefaultEffort: domain.EffortHigh, Tier: domain.TierT3},
			"reviewer.strong":  {CapabilityFloor: []string{"code_review"}, DefaultEffort: domain.EffortHigh, Tier: domain.TierT3},
		},
		Ladder:       []string{"worker.fast", "worker.general", "planner.frontier"},
		DefaultAlias: "worker.general",
		Profiles: []domain.ModelProfile{
			{ID: "p1", Alias: "worker.fast", Harness: "claude", Model: "cheap-model",
				Capabilities: []string{"code_edit", "tests"}, Tier: domain.TierT1,
				ReasoningEffort: domain.EffortLow, Enabled: true,
				InputCostPerMTok: price(1), OutputCostPerMTok: price(5)},
			{ID: "p2", Alias: "worker.general", Harness: "claude", Model: "mid-model",
				Capabilities: []string{"code_edit", "tests", "multi_file"}, Tier: domain.TierT2,
				ReasoningEffort: domain.EffortMedium, Enabled: true,
				InputCostPerMTok: price(3), OutputCostPerMTok: price(15)},
			{ID: "p3", Alias: "planner.frontier", Harness: "claude", Model: "frontier-model",
				Capabilities: []string{"architecture", "long_context", "code_edit"}, Tier: domain.TierT4,
				ReasoningEffort: domain.EffortHigh, Enabled: true,
				InputCostPerMTok: price(15), OutputCostPerMTok: price(75)},
			{ID: "p4", Alias: "reviewer.strong", Harness: "claude", Model: "frontier-model",
				Capabilities: []string{"code_review", "architecture"}, Tier: domain.TierT4,
				ReasoningEffort: domain.EffortHigh, Enabled: true,
				InputCostPerMTok: price(15), OutputCostPerMTok: price(75)},
			// A disabled profile must never be selected.
			{ID: "p5", Alias: "worker.general", Harness: "codex", Model: "",
				Tier: domain.TierT2, Enabled: false},
		},
	}
}

func TestTrivialWorkRoutesCheap(t *testing.T) {
	d, err := Route(testCatalog(), Request{
		Role:     domain.RoleImplementer,
		Features: domain.TaskFeatures{EstimatedFiles: 1, AcceptanceAmbiguity: 0.1},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Tier != domain.TierT1 {
		t.Errorf("tier = %s, want T1", d.Tier)
	}
	if d.Model != "cheap-model" {
		t.Errorf("model = %s, want cheap-model", d.Model)
	}
}

func TestAmbiguousCrossModuleWorkRoutesUp(t *testing.T) {
	d, err := Route(testCatalog(), Request{
		Role:     domain.RoleImplementer,
		Features: domain.TaskFeatures{EstimatedFiles: 12, AcceptanceAmbiguity: 0.7, CrossModuleEdges: 4},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !d.Tier.AtLeast(domain.TierT3) {
		t.Errorf("tier = %s, want at least T3", d.Tier)
	}
}

// DESIGN.md §13.5: security and cryptography work has a T4 floor with independent review and
// human merge approval. No other input may lower it.
func TestSecurityFloorIsAbsolute(t *testing.T) {
	d, err := Route(testCatalog(), Request{
		Role:       domain.RoleImplementer,
		Mechanical: true, // would normally de-escalate
		Features: domain.TaskFeatures{
			EstimatedFiles: 1, AcceptanceAmbiguity: 0.0, SecuritySensitive: true,
		},
		Budget: BudgetState{MonthlyUSD: 100, SpentUSD: 99, DownshiftAt: 0.75, PauseAt: 0.999},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Tier != domain.TierT4 {
		t.Errorf("tier = %s, want T4 despite mechanical work and budget pressure", d.Tier)
	}
	if !d.RequireIndependentReview || !d.HumanMergeApproval {
		t.Error("T4 must require independent review and human merge approval")
	}
	joined := strings.Join(d.Rationale, " | ")
	if !strings.Contains(joined, "floor forbids a cheaper tier") {
		t.Errorf("rationale should record the refused downshift: %s", joined)
	}
}

func TestMigrationFloorAndSingleWriter(t *testing.T) {
	d, err := Route(testCatalog(), Request{
		Role:     domain.RoleImplementer,
		Features: domain.TaskFeatures{EstimatedFiles: 1, SchemaOrMigration: true},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !d.Tier.AtLeast(domain.TierT3) {
		t.Errorf("tier = %s, want at least T3 for a migration", d.Tier)
	}
	if d.MaxParallelWriters != 1 {
		t.Errorf("MaxParallelWriters = %d, want 1", d.MaxParallelWriters)
	}
}

func TestEscalationOnRepeatedFailure(t *testing.T) {
	cat := testCatalog()
	base, err := Route(cat, Request{
		Role:     domain.RoleImplementer,
		Features: domain.TaskFeatures{EstimatedFiles: 4},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	escalated, err := Route(cat, Request{
		Role:          domain.RoleImplementer,
		Features:      domain.TaskFeatures{EstimatedFiles: 4},
		PriorFailures: 2,
	})
	if err != nil {
		t.Fatalf("Route escalated: %v", err)
	}
	if !escalated.Tier.AtLeast(base.Tier) || escalated.Tier == base.Tier {
		t.Errorf("tier after failures = %s, want above %s", escalated.Tier, base.Tier)
	}
}

func TestDeEscalationForMechanicalWork(t *testing.T) {
	cat := testCatalog()
	normal, _ := Route(cat, Request{
		Role:     domain.RoleImplementer,
		Features: domain.TaskFeatures{EstimatedFiles: 5},
	})
	mech, _ := Route(cat, Request{
		Role:       domain.RoleImplementer,
		Features:   domain.TaskFeatures{EstimatedFiles: 5},
		Mechanical: true,
	})
	if mech.Tier == normal.Tier {
		t.Errorf("mechanical work should drop a tier (both %s)", normal.Tier)
	}
}

func TestBudgetPauseAndDownshift(t *testing.T) {
	cat := testCatalog()

	paused, err := Route(cat, Request{
		Role:     domain.RoleImplementer,
		Features: domain.TaskFeatures{EstimatedFiles: 3},
		Budget:   BudgetState{MonthlyUSD: 100, SpentUSD: 96, DownshiftAt: 0.75, PauseAt: 0.95},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !paused.Paused {
		t.Error("dispatch should pause past the pause threshold")
	}

	down, err := Route(cat, Request{
		Role:     domain.RoleImplementer,
		Features: domain.TaskFeatures{EstimatedFiles: 5},
		Budget:   BudgetState{MonthlyUSD: 100, SpentUSD: 80, DownshiftAt: 0.75, PauseAt: 0.95},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if down.Paused {
		t.Error("should not pause below the pause threshold")
	}
	if down.Tier != domain.TierT1 {
		t.Errorf("tier under budget pressure = %s, want T1", down.Tier)
	}
}

func TestReviewerRoleResolvesReviewerAlias(t *testing.T) {
	d, err := Route(testCatalog(), Request{
		Role:     domain.RoleCodeReviewer,
		Features: domain.TaskFeatures{EstimatedFiles: 3},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !strings.HasPrefix(d.Alias, "reviewer.") {
		t.Errorf("alias = %s, want a reviewer.* alias", d.Alias)
	}
}

func TestUnavailableHarnessFallsBack(t *testing.T) {
	_, err := Route(testCatalog(), Request{
		Role:               domain.RoleImplementer,
		Features:           domain.TaskFeatures{EstimatedFiles: 1},
		AvailableHarnesses: []string{"opencode"}, // nothing enabled for it
	})
	if !errors.Is(err, domain.ErrCapacity) {
		t.Errorf("err = %v, want ErrCapacity when no profile matches the available harness", err)
	}
}

func TestDisabledProfilesAreNeverSelected(t *testing.T) {
	d, err := Route(testCatalog(), Request{
		Role:               domain.RoleImplementer,
		Features:           domain.TaskFeatures{EstimatedFiles: 4},
		AvailableHarnesses: []string{"claude", "codex"},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Harness == "codex" || d.Model == "" {
		t.Errorf("selected a disabled or empty profile: %+v", d)
	}
}

func TestRouteIsDeterministic(t *testing.T) {
	cat := testCatalog()
	req := Request{Role: domain.RoleImplementer, Features: domain.TaskFeatures{EstimatedFiles: 4}}
	first, err := Route(cat, req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := Route(cat, req)
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		if again.Model != first.Model || again.Alias != first.Alias || again.Tier != first.Tier {
			t.Fatalf("routing is not deterministic: %+v vs %+v", first, again)
		}
	}
}

// ---------------------------------------------------------------------------
// Feature derivation
// ---------------------------------------------------------------------------

func TestDeriveFeaturesFlagsSensitiveScopes(t *testing.T) {
	policy := FeaturePolicy{
		SecurityPaths:     []string{"dir:internal/auth"},
		SchemaOrMigration: []string{"dir:migrations", "migration:*"},
		InfraPaths:        []string{"path:docker-compose.yml"},
	}

	f := DeriveFeatures([]string{"path:internal/auth/token.go"}, nil, policy, domain.TaskFeatures{})
	if !f.SecuritySensitive {
		t.Error("auth path should be security sensitive")
	}

	f = DeriveFeatures([]string{"migration:primary"}, nil, policy, domain.TaskFeatures{})
	if !f.SchemaOrMigration {
		t.Error("migration scope should set SchemaOrMigration")
	}

	f = DeriveFeatures([]string{"api:POST /v1/tasks"}, nil, policy, domain.TaskFeatures{})
	if !f.PublicAPIChange {
		t.Error("api scope should set PublicAPIChange")
	}

	f = DeriveFeatures([]string{"path:README.md"}, nil, policy, domain.TaskFeatures{})
	if f.SecuritySensitive || f.SchemaOrMigration || f.InfraOrDeployment {
		t.Errorf("benign path flagged sensitive: %+v", f)
	}
}

func TestDeriveFeaturesPrefersActualDiff(t *testing.T) {
	f := DeriveFeatures(
		[]string{"dir:internal/api"}, // would estimate broadly
		[]string{"internal/api/a.go", "internal/api/b.go"},
		DefaultFeaturePolicy(), domain.TaskFeatures{})
	if f.EstimatedFiles != 2 {
		t.Errorf("EstimatedFiles = %d, want 2 from the actual diff", f.EstimatedFiles)
	}
	if len(f.Languages) == 0 || f.Languages[0] != "go" {
		t.Errorf("Languages = %v, want go", f.Languages)
	}
}

func TestScopeGrowthDetectsDrift(t *testing.T) {
	within := ScopeGrowth([]string{"dir:internal/router"},
		[]string{"internal/router/a.go", "internal/router/b.go"})
	if within > 1.0 {
		t.Errorf("in-scope work reported growth %.2f, want <= 1.0", within)
	}

	drifted := ScopeGrowth([]string{"dir:internal/router"},
		[]string{"internal/router/a.go", "internal/api/handler.go", "internal/db/store.go"})
	if drifted <= 1.0 {
		t.Errorf("out-of-scope work reported growth %.2f, want > 1.0", drifted)
	}
}

func TestRepositoryRulesRaiseFloorsAndEscalate(t *testing.T) {
	cat := Catalog{
		Profiles: []domain.ModelProfile{
			{Alias: "worker.fast", Harness: "claude", Model: "fast", Tier: domain.TierT1, ReasoningEffort: "low", Enabled: true},
			{Alias: "worker.general", Harness: "claude", Model: "general", Tier: domain.TierT2, ReasoningEffort: "medium", Enabled: true},
			{Alias: "planner.frontier", Harness: "claude", Model: "frontier", Tier: domain.TierT4, ReasoningEffort: "high", Enabled: true},
		},
		Aliases: map[string]AliasPolicy{
			"worker.fast":      {Tier: domain.TierT1, DefaultEffort: "low"},
			"worker.general":   {Tier: domain.TierT2, DefaultEffort: "medium"},
			"planner.frontier": {Tier: domain.TierT4, DefaultEffort: "high"},
		},
		Ladder:       []string{"worker.fast", "worker.general", "planner.frontier"},
		DefaultAlias: "worker.general",
	}
	rules := []domain.RouterRule{
		{ID: "infra-frontier", When: "task.infra_or_deployment", RequireTier: domain.TierT4, HumanMergeApproval: true},
		{ID: "docs-cheap", When: `task.labels has "docs"`, PreferTier: domain.TierT1},
		{ID: "two-strikes", When: "attempt.failures >= 2", EscalateOneTier: true, RequireReplan: true},
	}
	// A plain small task with the docs label prefers T1.
	d, err := Route(cat, Request{
		Role: domain.RoleImplementer, AvailableHarnesses: []string{"claude"}, Rules: rules,
		Facts:    &policy.Facts{Task: domain.Task{Labels: []string{"docs"}}, Features: domain.TaskFeatures{EstimatedFiles: 4}},
		Features: domain.TaskFeatures{EstimatedFiles: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Tier != domain.TierT1 || d.Model != "fast" {
		t.Fatalf("docs task routed to %s/%s: %v", d.Tier, d.Model, d.Rationale)
	}
	// Infra work carries a T4 floor from the repository even though the built-in floor is T3.
	d, err = Route(cat, Request{
		Role: domain.RoleImplementer, AvailableHarnesses: []string{"claude"}, Rules: rules,
		Features: domain.TaskFeatures{EstimatedFiles: 1, InfraOrDeployment: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Tier != domain.TierT4 || !d.HumanMergeApproval || len(d.MatchedRules) != 1 {
		t.Fatalf("infra task = %+v", d)
	}
	// Two failures escalate one tier via the rule (T2 → T3, then the built-in escalation
	// takes it to T4) and require a replan.
	d, err = Route(cat, Request{
		Role: domain.RoleImplementer, AvailableHarnesses: []string{"claude"}, Rules: rules,
		Features: domain.TaskFeatures{EstimatedFiles: 4}, PriorFailures: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.RequireReplan || !d.Tier.AtLeast(domain.TierT3) {
		t.Fatalf("failed task = %+v", d)
	}
	// A broken rule is an error, not a silent pass.
	if _, err := Route(cat, Request{Rules: []domain.RouterRule{{ID: "bad", When: "(("}}, AvailableHarnesses: []string{"claude"}}); err == nil {
		t.Fatal("expected a compile error from a malformed rule")
	}
}
