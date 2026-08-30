package coord

import (
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// blockingConflict is the shape the store returns when a write_exclusive scope is already
// held: the matrix marks the pair block_conflict whatever the project's enforcement level —
// the level decides what the agent is told, not what the matrix computed.
func blockingConflict() db.ScopeConflict {
	return db.ScopeConflict{
		Requested:     "path:internal/router/engine.go",
		ResourceKey:   "path:internal/router/engine.go",
		Outcome:       domain.OutcomeBlockConflict,
		Severity:      domain.SeverityHigh,
		Kind:          domain.ConflictScopeOverlap,
		HolderTaskRef: "T-7",
		HolderTitle:   "router rewrite",
		HolderOwner:   "alice",
		HolderMode:    domain.ModeWriteExclusive,
	}
}

func warnConflict() db.ScopeConflict {
	c := blockingConflict()
	c.Outcome = domain.OutcomeAllowWithWarning
	return c
}

// TestDecideAppliesEnforcementLevel is §11.5 levels 1-3 at the check surface: below
// strict_harness a blocking conflict is a warning, and the warning names the level's
// obligation — cooperative demands expansion, advisory points at the dashboard.
func TestDecideAppliesEnforcementLevel(t *testing.T) {
	tests := []struct {
		name      string
		claimMode domain.EnforcementLevel
		outcome   domain.Outcome
		adviceHas string
	}{
		{"advisory warns", domain.EnforceAdvisory, domain.OutcomeAllowWithWarning, "dashboard"},
		{"cooperative warns and demands expansion", domain.EnforceCooperative, domain.OutcomeAllowWithWarning, "coord_expand_scope"},
		{"strict harness blocks", domain.EnforceStrictHarness, domain.OutcomeBlockConflict, "Wait for it"},
		{"strict filesystem blocks", domain.EnforceStrictFS, domain.OutcomeBlockConflict, "Wait for it"},
		{"strict alias normalizes", "strict", domain.OutcomeBlockConflict, "Wait for it"},
		{"absent level defaults to cooperative", "", domain.OutcomeAllowWithWarning, "coord_expand_scope"},
		{"unrecognized level defaults to cooperative", "yolo", domain.OutcomeAllowWithWarning, "coord_expand_scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := domain.DefaultProjectConfig()
			cfg.ClaimMode = tt.claimMode
			got := decide(nil, []db.ScopeConflict{blockingConflict()}, cfg)
			if got.Outcome != tt.outcome {
				t.Fatalf("outcome = %s, want %s", got.Outcome, tt.outcome)
			}
			if !strings.Contains(got.Advice, tt.adviceHas) {
				t.Errorf("advice %q does not mention %q", got.Advice, tt.adviceHas)
			}
			if want := domain.NormalizeEnforcementLevel(tt.claimMode); got.Enforcement != want {
				t.Errorf("enforcement = %s, want %s", got.Enforcement, want)
			}
		})
	}
}

// Non-blocking overlaps warn at every level; the level only governs blocking conflicts.
func TestDecideWarnClassConflictWarnsAtEveryLevel(t *testing.T) {
	for _, level := range []domain.EnforcementLevel{
		domain.EnforceAdvisory, domain.EnforceCooperative, domain.EnforceStrictHarness,
	} {
		cfg := domain.DefaultProjectConfig()
		cfg.ClaimMode = level
		got := decide(nil, []db.ScopeConflict{warnConflict()}, cfg)
		if got.Outcome != domain.OutcomeAllowWithWarning {
			t.Errorf("%s: outcome = %s, want allow_with_warning", level, got.Outcome)
		}
	}
}

// An exact duplicate outranks a scope conflict even when the conflict would only warn:
// "this work already exists" resolves both at once.
func TestDecideDuplicateOutranksConflict(t *testing.T) {
	dup := db.DuplicateCandidate{Exact: true, TaskRef: "T-3", Owner: "alice"}
	cfg := domain.DefaultProjectConfig() // duplicatePolicy: block_exact
	cfg.ClaimMode = domain.EnforceStrictHarness
	got := decide([]db.DuplicateCandidate{dup}, []db.ScopeConflict{blockingConflict()}, cfg)
	if got.Outcome != domain.OutcomeBlockDuplicate {
		t.Fatalf("outcome = %s, want block_duplicate", got.Outcome)
	}
}

// A similar (non-exact) duplicate sets suggest_join, and a blocking conflict still
// overrides it — as a warning in cooperative, as a block in strict.
func TestDecideConflictOutranksSuggestJoin(t *testing.T) {
	dup := db.DuplicateCandidate{TaskRef: "T-3", Owner: "alice"}
	similar := []db.DuplicateCandidate{dup}

	cfg := domain.DefaultProjectConfig()
	cfg.ClaimMode = domain.EnforceCooperative
	got := decide(similar, []db.ScopeConflict{blockingConflict()}, cfg)
	if got.Outcome != domain.OutcomeAllowWithWarning || !strings.Contains(got.Advice, "holds") {
		t.Fatalf("cooperative: outcome = %s advice = %q, want warning naming the holder", got.Outcome, got.Advice)
	}

	cfg.ClaimMode = domain.EnforceStrictHarness
	got = decide(similar, []db.ScopeConflict{blockingConflict()}, cfg)
	if got.Outcome != domain.OutcomeBlockConflict {
		t.Fatalf("strict: outcome = %s, want block_conflict", got.Outcome)
	}
}

func TestDecideCleanAllow(t *testing.T) {
	got := decide(nil, nil, domain.DefaultProjectConfig())
	if got.Outcome != domain.OutcomeAllow || got.Reason != "" || got.Advice != "" {
		t.Fatalf("clean allow: %+v", got)
	}
	if got.Enforcement != domain.EnforceCooperative {
		t.Fatalf("enforcement = %s, want cooperative (default)", got.Enforcement)
	}
}
