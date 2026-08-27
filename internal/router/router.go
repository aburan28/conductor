// Package router implements the two-dimensional model and harness routing of DESIGN.md §13.
//
// The routing decision has three independent parts — role, harness, and model profile —
// because "Codex", "Claude", and "OpenCode" are harnesses, not model tiers, and conflating
// them produces nonsense like "use the expensive tool for the easy task".
//
// Everything here is deterministic and dependency-free. A model may propose a tier; it can
// never route around a hard rule (§4.1, §13.5).
package router

import (
	"fmt"
	"sort"
	"strings"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/policy"
)

// AliasPolicy is a role definition: the capability floor a model must meet, not a model name.
type AliasPolicy struct {
	CapabilityFloor []string
	DefaultEffort   domain.Effort
	Tier            domain.Tier
}

// Catalog is the resolvable universe of models for one organization.
type Catalog struct {
	Profiles     []domain.ModelProfile
	Aliases      map[string]AliasPolicy
	Ladder       []string // cheapest to most capable
	DefaultAlias string
	Version      string
}

// BudgetState is what the router needs to know about spend (DESIGN.md §13.8).
type BudgetState struct {
	MonthlyUSD  float64
	SpentUSD    float64
	DownshiftAt float64
	PauseAt     float64
}

// Fraction returns spend as a fraction of budget; 0 when no budget is configured.
func (b BudgetState) Fraction() float64 {
	if b.MonthlyUSD <= 0 {
		return 0
	}
	return b.SpentUSD / b.MonthlyUSD
}

// Request is everything the router considers.
type Request struct {
	Role               domain.AgentRole
	Features           domain.TaskFeatures
	RiskLevel          domain.RiskLevel
	AttemptNumber      int
	PriorFailures      int
	ReviewRejections   int
	RequestedAlias     string
	HarnessPref        string
	AvailableHarnesses []string
	Budget             BudgetState
	// Mechanical marks work that is applying an already-approved plan, addressing
	// line-level review comments, or similar (DESIGN.md §13.7).
	Mechanical bool
	// Rules are the repository's policies.yaml router rules. Their `when` expressions are
	// evaluated against Facts; the built-in floors below still apply underneath them, so a
	// repository can only ever add floors, never remove the security one.
	Rules []domain.RouterRule
	// Facts are what the rules read. Nil means "derive from Features and history".
	Facts *policy.Facts
}

// facts renders the request for rule evaluation.
func (r Request) facts() policy.Env {
	if r.Facts != nil {
		return r.Facts.Env()
	}
	return policy.Facts{
		Features:         r.Features,
		Task:             domain.Task{RiskLevel: r.RiskLevel, ModelAlias: r.RequestedAlias, HarnessPref: r.HarnessPref},
		AttemptNumber:    r.AttemptNumber,
		PriorFailures:    r.PriorFailures,
		ReviewRejections: r.ReviewRejections,
		Role:             r.Role,
		Harnesses:        r.AvailableHarnesses,
		Budget:           policy.BudgetFacts{MonthlyUSD: r.Budget.MonthlyUSD, SpentUSD: r.Budget.SpentUSD},
	}.Env()
}

// Decision is the routing outcome plus the reasoning that produced it, which is recorded as
// a policy decision so the audit view can answer "why did this route the way it did".
type Decision struct {
	Alias                    string        `json:"alias"`
	Harness                  string        `json:"harness"`
	Model                    string        `json:"model"`
	Provider                 string        `json:"provider"`
	ProfileID                domain.ID     `json:"profile_id,omitempty"`
	Effort                   domain.Effort `json:"reasoning_effort"`
	Tier                     domain.Tier   `json:"tier"`
	RequireIndependentReview bool          `json:"require_independent_review"`
	HumanMergeApproval       bool          `json:"human_merge_approval"`
	RequireReplan            bool          `json:"require_replan"`
	MaxParallelWriters       int           `json:"max_parallel_writers,omitempty"`
	Paused                   bool          `json:"paused"`
	RequireRollbackPlan      bool          `json:"require_rollback_plan,omitempty"`
	MatchedRules             []string      `json:"matched_rules,omitempty"`
	Rationale                []string      `json:"rationale"`
	CatalogVersion           string        `json:"catalog_version,omitempty"`
}

// PolicyVersion identifies the compiled hard-rule set, recorded on every attempt so two
// differently-routed attempts can be explained (DESIGN.md §20.4).
const PolicyVersion = "router/v1"

// Route chooses a tier, alias, harness, and effort.
//
// Order is load-bearing:
//  1. hard floors from risk and features — these can only raise, never lower;
//  2. base tier from task shape;
//  3. escalation from failure history;
//  4. de-escalation for mechanical work;
//  5. budget guard, which may downshift or pause but never below a floor.
//
// Step 5 last, and clamped by step 1, is the whole reason a cost control cannot quietly
// route a migration or an auth change to a cheap model (§27.4: never silently downgrade
// below a task's capability or security floor).
func Route(cat Catalog, req Request) (Decision, error) {
	d := Decision{CatalogVersion: cat.Version}

	floor, floorReason := hardFloor(req)
	if floorReason != "" {
		d.Rationale = append(d.Rationale, floorReason)
	}

	// Repository rules. They evaluate against the same facts a dispatch policy sees and can
	// only raise floors, add review requirements, or escalate — the built-in floors above
	// remain underneath them.
	var effects policy.RouterEffects
	if len(req.Rules) > 0 {
		var err error
		effects, err = policy.ApplyRouterRules(req.Rules, req.facts())
		if err != nil {
			return d, err
		}
		d.MatchedRules = effects.Matched
		if len(effects.Matched) > 0 {
			d.Rationale = append(d.Rationale, "policy rules matched: "+strings.Join(effects.Matched, ", "))
		}
		if effects.Floor != "" && effects.Floor.AtLeast(floor) && effects.Floor != floor {
			floor = effects.Floor
			d.Rationale = append(d.Rationale, fmt.Sprintf("policy raised the floor to %s", floor))
		}
	}

	tier := baseTier(req)
	d.Rationale = append(d.Rationale, fmt.Sprintf("base tier %s from task shape", tier))
	if effects.PreferTier != "" && !floorApplies(req) && effects.Floor == "" {
		tier = effects.PreferTier
		d.Rationale = append(d.Rationale, fmt.Sprintf("policy prefers tier %s", tier))
	}
	for i := 0; i < effects.EscalateSteps; i++ {
		tier = tier.Up()
		d.Rationale = append(d.Rationale, fmt.Sprintf("policy escalated to %s", tier))
	}
	if effects.RequireReplan {
		d.RequireReplan = true
		d.Rationale = append(d.Rationale, "policy requires a replan")
	}
	d.RequireRollbackPlan = effects.RequireRollbackPlan

	// Escalation: repeated failure means the current tier is not solving it (§13.6).
	if req.PriorFailures >= 2 {
		tier = tier.Up()
		d.Rationale = append(d.Rationale,
			fmt.Sprintf("escalated to %s after %d prior failures", tier, req.PriorFailures))
	}
	if req.ReviewRejections >= 1 {
		tier = tier.Up()
		d.Rationale = append(d.Rationale,
			fmt.Sprintf("escalated to %s after review rejection", tier))
	}
	if req.Features.ScopeGrowthRatio > 1.5 {
		d.RequireReplan = true
		d.Rationale = append(d.Rationale, "scope grew beyond 1.5x plan; replan required")
	}

	// De-escalation for genuinely mechanical work (§13.7).
	if req.Mechanical && !floorApplies(req) {
		tier = tier.Down()
		d.Rationale = append(d.Rationale, fmt.Sprintf("de-escalated to %s for mechanical work", tier))
	}

	// Budget guard (§13.8), clamped by the floor.
	frac := req.Budget.Fraction()
	if req.Budget.MonthlyUSD > 0 {
		if req.Budget.PauseAt > 0 && frac >= req.Budget.PauseAt {
			d.Paused = true
			d.Rationale = append(d.Rationale,
				fmt.Sprintf("budget %.0f%% consumed; dispatch paused", frac*100))
		} else if req.Budget.DownshiftAt > 0 && frac >= req.Budget.DownshiftAt {
			lowered := tier.Down()
			if lowered.AtLeast(floor) {
				tier = lowered
				d.Rationale = append(d.Rationale,
					fmt.Sprintf("budget %.0f%% consumed; downshifted to %s", frac*100, tier))
			} else {
				d.Rationale = append(d.Rationale,
					"budget pressure ignored: task floor forbids a cheaper tier")
			}
		}
	}

	if !tier.AtLeast(floor) {
		tier = floor
		d.Rationale = append(d.Rationale, fmt.Sprintf("raised to floor tier %s", floor))
	}
	d.Tier = tier

	if tier.AtLeast(domain.TierT4) {
		d.RequireIndependentReview = true
		d.HumanMergeApproval = true
	}
	if req.Features.SchemaOrMigration {
		d.MaxParallelWriters = 1
	}
	d.RequireIndependentReview = d.RequireIndependentReview || effects.RequireIndependentReview
	d.HumanMergeApproval = d.HumanMergeApproval || effects.HumanMergeApproval
	if effects.MaxParallelWriters > 0 && (d.MaxParallelWriters == 0 || effects.MaxParallelWriters < d.MaxParallelWriters) {
		d.MaxParallelWriters = effects.MaxParallelWriters
	}

	alias := req.RequestedAlias
	if alias == "" {
		alias = aliasForTier(cat, tier, req.Role)
	}
	d.Alias = alias

	profile, err := cat.Resolve(alias, tier, req)
	if err != nil {
		return d, err
	}
	d.ProfileID = profile.ID
	d.Harness = profile.Harness
	d.Model = profile.Model
	d.Provider = profile.Provider
	d.Alias = profile.Alias

	effort := profile.ReasoningEffort
	if policy, ok := cat.Aliases[profile.Alias]; ok && policy.DefaultEffort != "" {
		effort = policy.DefaultEffort
	}
	if req.PriorFailures >= 1 {
		effort = effort.Up()
		d.Rationale = append(d.Rationale, "raised reasoning effort after a prior failure")
	}
	if req.Mechanical && !floorApplies(req) {
		effort = effort.Down()
	}
	d.Effort = effort

	d.Rationale = append(d.Rationale,
		fmt.Sprintf("resolved %s on %s to %s at effort %s", d.Alias, d.Harness, d.Model, d.Effort))
	return d, nil
}

// hardFloor computes the minimum tier a task may route to, from DESIGN.md §13.5.
func hardFloor(req Request) (domain.Tier, string) {
	f := req.Features
	switch {
	case f.SecuritySensitive || f.CryptographySensitive:
		return domain.TierT4, "security or cryptography sensitive: T4 floor, independent review, human merge approval"
	case req.RiskLevel == domain.RiskCritical:
		return domain.TierT4, "critical risk: T4 floor"
	case f.SchemaOrMigration:
		return domain.TierT3, "schema or migration change: T3 floor, single writer, rollback plan required"
	case f.PublicAPIChange:
		return domain.TierT3, "public API change: T3 floor"
	case f.InfraOrDeployment:
		return domain.TierT3, "infrastructure or deployment change: T3 floor"
	case req.RiskLevel == domain.RiskHigh:
		return domain.TierT3, "high risk: T3 floor"
	}
	return domain.TierT0, ""
}

func floorApplies(req Request) bool {
	floor, _ := hardFloor(req)
	return floor.AtLeast(domain.TierT3)
}

// baseTier maps task shape onto the tier table of DESIGN.md §13.4.
func baseTier(req Request) domain.Tier {
	f := req.Features
	switch {
	case f.EstimatedFiles <= 2 && f.AcceptanceAmbiguity < 0.2 && f.CrossModuleEdges == 0:
		return domain.TierT1
	case f.AcceptanceAmbiguity >= 0.5 || f.CrossModuleEdges > 2 || f.UnknownCodeRatio > 0.6:
		return domain.TierT3
	default:
		return domain.TierT2
	}
}

// aliasForTier picks the alias whose declared tier best matches, preferring role-appropriate
// names so a reviewer never resolves to a worker alias.
func aliasForTier(cat Catalog, tier domain.Tier, role domain.AgentRole) string {
	prefix := ""
	switch role {
	case domain.RolePlanner:
		prefix = "planner."
	case domain.RoleCodeReviewer:
		prefix = "reviewer."
	case domain.RoleVerifier:
		prefix = "verifier."
	default:
		prefix = "worker."
	}

	names := make([]string, 0, len(cat.Aliases))
	for name := range cat.Aliases {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic selection when several aliases tie

	best, bestScore := "", -1
	for _, name := range names {
		policy := cat.Aliases[name]
		score := 0
		if strings.HasPrefix(name, prefix) {
			score += 10
		}
		if policy.Tier == tier {
			score += 5
		} else if policy.Tier.AtLeast(tier) {
			score += 2
		}
		if score > bestScore {
			best, bestScore = name, score
		}
	}
	if best == "" {
		best = cat.DefaultAlias
	}
	return best
}

// Resolve maps an alias to a concrete enabled profile, honouring harness preference and the
// alias capability floor.
//
// A capability floor that no available model meets is an error rather than a silent
// downgrade: routing a task that needs long context to a model without it produces a
// confidently wrong result, which is worse than a clear failure.
func (c Catalog) Resolve(alias string, tier domain.Tier, req Request) (domain.ModelProfile, error) {
	policy := c.Aliases[alias]

	available := map[string]bool{}
	for _, h := range req.AvailableHarnesses {
		available[h] = true
	}

	var candidates []domain.ModelProfile
	for _, p := range c.Profiles {
		if !p.Enabled || p.Model == "" {
			continue
		}
		if p.Alias != alias {
			continue
		}
		if len(available) > 0 && !available[p.Harness] {
			continue
		}
		if len(policy.CapabilityFloor) > 0 && !p.HasCapabilities(policy.CapabilityFloor) {
			continue
		}
		candidates = append(candidates, p)
	}

	// Fall back up the ladder: if the requested alias has no usable profile, try
	// progressively more capable aliases rather than silently doing nothing.
	if len(candidates) == 0 {
		for _, next := range c.ladderAbove(alias) {
			for _, p := range c.Profiles {
				if p.Enabled && p.Model != "" && p.Alias == next &&
					(len(available) == 0 || available[p.Harness]) {
					candidates = append(candidates, p)
				}
			}
			if len(candidates) > 0 {
				break
			}
		}
	}

	if len(candidates) == 0 {
		return domain.ModelProfile{}, fmt.Errorf(
			"%w: no enabled model profile for alias %q on harness(es) %v",
			domain.ErrCapacity, alias, req.AvailableHarnesses)
	}

	// Prefer the requested harness, then the cheapest profile that still meets the tier.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if req.HarnessPref != "" {
			if (a.Harness == req.HarnessPref) != (b.Harness == req.HarnessPref) {
				return a.Harness == req.HarnessPref
			}
		}
		aMeets, bMeets := a.Tier.AtLeast(tier), b.Tier.AtLeast(tier)
		if aMeets != bMeets {
			return aMeets
		}
		return cost(a) < cost(b)
	})
	return candidates[0], nil
}

// ladderAbove returns the aliases above the given one, cheapest first.
func (c Catalog) ladderAbove(alias string) []string {
	idx := -1
	for i, name := range c.Ladder {
		if name == alias {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(c.Ladder) {
		return nil
	}
	return c.Ladder[idx+1:]
}

// cost gives a comparable price for ranking. Capacity-billed profiles (subscription tools)
// have no per-token price, so they sort as free — which is correct, since using them consumes
// an allowance rather than money.
func cost(p domain.ModelProfile) float64 {
	if p.Billing == "capacity" {
		return 0
	}
	var total float64
	if p.InputCostPerMTok != nil {
		total += *p.InputCostPerMTok
	}
	if p.OutputCostPerMTok != nil {
		total += *p.OutputCostPerMTok
	}
	return total
}
