package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/config"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/router"
)

// The files `conductor setup` writes must be internally consistent: every tier a scaffold
// policy rule requires is served by an enabled scaffold profile, and the ladder tops out at
// the frontier tier. Before the four-tier topology the scaffold labelled Opus T4; once Opus
// became core (T3) a fresh install's security floor would have had nothing to route to.
func TestScaffoldTiersAreServed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, config.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"project.yaml":  scaffoldProject("demo"),
		"policies.yaml": scaffoldPolicies,
		"models.yaml":   scaffoldModels,
		"dispatch.yaml": scaffoldDispatch,
		"WORKFLOW.md":   scaffoldWorkflow,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundle, err := config.Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var served []domain.Tier
	for _, p := range bundle.ModelProfiles("org-1") {
		if p.Enabled {
			served = append(served, p.Tier)
		}
	}
	for _, rule := range bundle.Policies.Router.Rules {
		if rule.RequireTier == "" {
			continue
		}
		ok := false
		for _, tier := range served {
			ok = ok || tier.AtLeast(rule.RequireTier)
		}
		if !ok {
			t.Errorf("rule %s requires %s but no scaffold profile serves it (served: %v)", rule.ID, rule.RequireTier, served)
		}
	}

	aliases := bundle.Models.Models.Aliases
	ladder := bundle.Models.Models.Ladder
	if strings.Join(ladder, ",") != strings.Join(router.DefaultLadder, ",") {
		t.Errorf("scaffold ladder %v differs from router.DefaultLadder %v", ladder, router.DefaultLadder)
	}
	if top := ladder[len(ladder)-1]; domain.Tier(aliases[top].Tier) != domain.TierT4 {
		t.Errorf("top of the scaffold ladder %s is not the frontier tier", top)
	}
}
