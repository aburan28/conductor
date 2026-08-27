package policy

import (
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
)

func samplePolicy() *domain.DispatchPolicy {
	return &domain.DispatchPolicy{
		Version: 1,
		Lanes: map[string]domain.DispatchLane{
			"plan": {Role: domain.RolePlanner, Candidates: []domain.DispatchCandidate{
				{Model: "claude-opus-5", Harness: "claude", Effort: "high"},
			}},
			"implement": {Role: domain.RoleImplementer, Candidates: []domain.DispatchCandidate{
				{Model: "ollama/qwen3:27b", Harness: "opencode", Tags: []string{"local", "cheap"},
					When: "task.estimated_files <= 3 && !task.security_sensitive", MaxConcurrent: 1},
				{Model: "claude-sonnet-5", Harness: "claude", Effort: "medium"},
				{Model: "claude-opus-5", Harness: "claude", Effort: "high"},
			}},
			"review": {Role: domain.RoleCodeReviewer, Review: "required", Candidates: []domain.DispatchCandidate{
				{Alias: "reviewer.strong"},
			}},
		},
		Rules: []domain.DispatchRule{
			{ID: "docs", When: `task.labels has "docs"`, Prefer: &domain.DispatchSelector{Tag: "local"}},
			{ID: "security", When: "task.security_sensitive", Require: &domain.CapabilityRequirement{Tier: domain.TierT4}, Review: "required"},
			{ID: "big", When: "task.estimated_files > 10", Effort: domain.EffortHigh},
		},
		Defaults: domain.DispatchDefaults{Lane: "implement", OnFailure: "escalate", MaxEscalations: 2},
		Limits:   []domain.DispatchLimit{{Match: domain.DispatchSelector{Harness: "opencode"}, MaxConcurrent: 2}},
	}
}

func sampleCatalog() ProfileCatalog {
	return ProfileCatalog{
		{Alias: "worker.general", Harness: "claude", Model: "claude-sonnet-5", Tier: domain.TierT2, ReasoningEffort: "medium", Enabled: true, Capabilities: []string{"code_edit"}},
		{Alias: "planner.frontier", Harness: "claude", Model: "claude-opus-5", Tier: domain.TierT4, ReasoningEffort: "high", Enabled: true, Capabilities: []string{"architecture"}},
		{Alias: "reviewer.strong", Harness: "claude", Model: "claude-opus-5", Tier: domain.TierT4, ReasoningEffort: "high", Enabled: true, Capabilities: []string{"code_review"}},
		{Alias: "worker.local", Harness: "opencode", Model: "ollama/qwen3:27b", Tier: domain.TierT1, ReasoningEffort: "low", Enabled: true},
	}
}

func compileOK(t *testing.T) *Dispatch {
	t.Helper()
	d, issues := CompileDispatch(samplePolicy())
	if HasErrors(issues) {
		t.Fatalf("unexpected lint errors: %v", issues)
	}
	return d
}

func TestResolveSmallDocsTaskPrefersLocal(t *testing.T) {
	d := compileOK(t)
	facts := Facts{
		Task:     domain.Task{Ref: "T-1", Labels: []string{"docs"}, RiskLevel: domain.RiskLow},
		Features: domain.TaskFeatures{EstimatedFiles: 2},
		Scopes:   []string{"path:docs/guide.md"},
		Role:     domain.RoleImplementer,
	}
	dec, err := d.Resolve(facts, ResolveOptions{Catalog: sampleCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "ollama/qwen3:27b" || dec.Harness != "opencode" {
		t.Fatalf("chose %s on %s, want the local model: %+v", dec.Model, dec.Harness, dec.Rationale)
	}
	if dec.Tier != domain.TierT1 {
		t.Errorf("tier = %s, want T1 from catalog", dec.Tier)
	}
	if len(dec.Rules) != 1 || dec.Rules[0] != "docs" {
		t.Errorf("rules = %v", dec.Rules)
	}
}

func TestResolveSecurityFloorSkipsCheapModels(t *testing.T) {
	d := compileOK(t)
	facts := Facts{
		Task:     domain.Task{Ref: "T-2"},
		Features: domain.TaskFeatures{EstimatedFiles: 1, SecuritySensitive: true},
		Role:     domain.RoleImplementer,
	}
	dec, err := d.Resolve(facts, ResolveOptions{Catalog: sampleCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "claude-opus-5" {
		t.Fatalf("chose %s, want opus for a T4 floor: %v", dec.Model, dec.Rationale)
	}
	if dec.Review != "required" {
		t.Errorf("review = %q, want required", dec.Review)
	}
	var rejected int
	for _, c := range dec.Candidates {
		if !c.Eligible {
			rejected++
			if c.Reason == "" {
				t.Errorf("rejected candidate %s has no reason", c.Model)
			}
		}
	}
	if rejected != 2 {
		t.Errorf("rejected = %d, want 2: %+v", rejected, dec.Candidates)
	}
}

func TestResolveEscalatesOnFailures(t *testing.T) {
	d := compileOK(t)
	facts := Facts{
		Task:          domain.Task{Ref: "T-3"},
		Features:      domain.TaskFeatures{EstimatedFiles: 6},
		Role:          domain.RoleImplementer,
		PriorFailures: 1,
	}
	dec, err := d.Resolve(facts, ResolveOptions{Catalog: sampleCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	// The local candidate is ineligible (6 files), so the ladder is sonnet → opus; one
	// failure steps to opus.
	if dec.Model != "claude-opus-5" || dec.Escalations != 1 {
		t.Fatalf("chose %s after %d escalations: %v", dec.Model, dec.Escalations, dec.Rationale)
	}
	facts.PriorFailures = 5
	dec, err = d.Resolve(facts, ResolveOptions{Catalog: sampleCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "claude-opus-5" {
		t.Fatalf("escalation must saturate at the top of the ladder, got %s", dec.Model)
	}
}

func TestResolveHonoursHarnessAvailabilityAndLimits(t *testing.T) {
	d := compileOK(t)
	facts := Facts{
		Task:      domain.Task{Ref: "T-4", Labels: []string{"docs"}},
		Features:  domain.TaskFeatures{EstimatedFiles: 1},
		Role:      domain.RoleImplementer,
		Harnesses: []string{"claude"},
	}
	dec, err := d.Resolve(facts, ResolveOptions{Catalog: sampleCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "claude-sonnet-5" {
		t.Fatalf("with only claude available, want sonnet, got %s: %v", dec.Model, dec.Rationale)
	}
	facts.Harnesses = nil
	dec, err = d.Resolve(facts, ResolveOptions{Catalog: sampleCatalog(), InFlight: map[string]int{"opencode:ollama/qwen3:27b": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "claude-sonnet-5" {
		t.Fatalf("local model at its limit; want sonnet, got %s: %+v", dec.Model, dec.Candidates)
	}
	found := false
	for _, c := range dec.Candidates {
		if c.Model == "ollama/qwen3:27b" && strings.Contains(c.Reason, "concurrency limit") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a concurrency-limit rejection: %+v", dec.Candidates)
	}
}

func TestResolveAliasLaneAndRuleEffort(t *testing.T) {
	d := compileOK(t)
	dec, err := d.Resolve(Facts{Task: domain.Task{Ref: "T-5"}, Role: domain.RoleCodeReviewer},
		ResolveOptions{Catalog: sampleCatalog(), Lane: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "claude-opus-5" || dec.Alias != "reviewer.strong" || dec.Review != "required" {
		t.Fatalf("review lane resolved to %+v", dec)
	}
	dec, err = d.Resolve(Facts{Task: domain.Task{Ref: "T-6"}, Features: domain.TaskFeatures{EstimatedFiles: 12}, Role: domain.RoleImplementer},
		ResolveOptions{Catalog: sampleCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Effort != domain.EffortHigh || dec.Model != "claude-sonnet-5" {
		t.Fatalf("big rule should set effort high on sonnet, got %s at %s", dec.Model, dec.Effort)
	}
}

func TestResolveNoEligible(t *testing.T) {
	d := compileOK(t)
	_, err := d.Resolve(Facts{Task: domain.Task{Ref: "T-7"}, Role: domain.RoleImplementer, Harnesses: []string{"codex"}},
		ResolveOptions{Catalog: sampleCatalog()})
	if err == nil || !strings.Contains(err.Error(), "no eligible candidate") {
		t.Fatalf("expected no-eligible error, got %v", err)
	}
}

func TestLintFindsProblems(t *testing.T) {
	p := &domain.DispatchPolicy{
		Lanes: map[string]domain.DispatchLane{
			"implement": {Candidates: []domain.DispatchCandidate{{Harness: "claude"}, {Model: "x", Harness: "claude", When: "task.risk =="}}},
			"empty":     {},
		},
		Rules: []domain.DispatchRule{
			{ID: "a", When: "task.nope == 1", Lane: "missing"},
			{ID: "a", When: "true"},
		},
		Defaults: domain.DispatchDefaults{Lane: "nowhere", OnFailure: "explode"},
		Limits:   []domain.DispatchLimit{{Match: domain.DispatchSelector{Harness: "codex"}, MaxConcurrent: 0}},
	}
	_, issues := CompileDispatch(p)
	if !HasErrors(issues) {
		t.Fatal("expected errors")
	}
	var text []string
	for _, i := range issues {
		text = append(text, i.String())
	}
	joined := strings.Join(text, "\n")
	for _, want := range []string{"needs a model or an alias", "lane has no candidates", `lane "missing" is not defined`,
		"duplicate rule id", "unknown fact", `lane "nowhere" is not defined`, "on_failure", "max_concurrent must be positive", "matches no candidate"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing issue %q in:\n%s", want, joined)
		}
	}
}

func TestApplyRouterRules(t *testing.T) {
	rules := []domain.RouterRule{
		{ID: "security-floor", When: "task.security_sensitive || task.cryptography_sensitive", RequireTier: domain.TierT4, RequireIndependentReview: true, HumanMergeApproval: true},
		{ID: "trivial", When: "task.estimated_files <= 2 && task.acceptance_ambiguity < 0.2", PreferTier: domain.TierT1},
		{ID: "retry", When: "attempt.failures >= 2", EscalateOneTier: true},
		{ID: "drift", When: "task.scope_growth_ratio > 1.5", RequireReplan: true},
	}
	env := Facts{Features: domain.TaskFeatures{EstimatedFiles: 1, SecuritySensitive: true, ScopeGrowthRatio: 2}, PriorFailures: 2}.Env()
	eff, err := ApplyRouterRules(rules, env)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Floor != domain.TierT4 || !eff.RequireIndependentReview || !eff.HumanMergeApproval {
		t.Errorf("security floor not applied: %+v", eff)
	}
	if eff.PreferTier != domain.TierT1 || eff.EscalateSteps != 1 || !eff.RequireReplan {
		t.Errorf("effects = %+v", eff)
	}
	if len(eff.Matched) != 4 {
		t.Errorf("matched = %v", eff.Matched)
	}
	if issues := LintRouterRules(append(rules, domain.RouterRule{ID: "bad", When: "(("})); !HasErrors(issues) {
		t.Errorf("expected a lint error for the bad rule")
	}
}

func TestFactsEnvCoversKnownFacts(t *testing.T) {
	env := Facts{}.Env()
	for _, name := range KnownFacts {
		if _, ok := env[name]; !ok {
			t.Errorf("KnownFacts lists %q but Env does not produce it", name)
		}
	}
	for name := range env {
		if !contains(KnownFacts, name) {
			t.Errorf("Env produces %q but KnownFacts does not list it", name)
		}
	}
}
