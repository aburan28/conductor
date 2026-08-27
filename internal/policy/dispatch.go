package policy

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// Facts are the deterministic inputs a policy may read for one routing decision. Everything
// here is derived from the ledger — scopes, attempt history, budget position — never from a
// prompt. Env renders them under the dotted names expressions use.
type Facts struct {
	Task             domain.Task
	Features         domain.TaskFeatures
	Scopes           []string
	ChangedPaths     []string
	AttemptNumber    int
	PriorFailures    int
	ReviewRejections int
	Role             domain.AgentRole
	Budget           BudgetFacts
	// Harnesses lists what the deciding runner can launch. Empty means "do not filter".
	Harnesses []string
	Now       time.Time
}

// BudgetFacts is the project's spend position.
type BudgetFacts struct {
	MonthlyUSD float64
	SpentUSD   float64
}

// Fraction is spend as a share of budget; 0 when no budget is configured.
func (b BudgetFacts) Fraction() float64 {
	if b.MonthlyUSD <= 0 {
		return 0
	}
	return b.SpentUSD / b.MonthlyUSD
}

// KnownFacts lists every name Env can produce, for the linter and for documentation.
var KnownFacts = []string{
	"task.ref", "task.title", "task.objective", "task.status", "task.labels", "task.scopes",
	"task.paths", "task.risk", "task.priority", "task.visibility", "task.external_ref",
	"task.model_alias", "task.harness_pref", "task.attempts", "task.max_attempts",
	"task.depends_on", "task.estimated_files", "task.estimated_loc", "task.languages",
	"task.cross_module_edges", "task.public_api_change", "task.schema_or_migration",
	"task.security_sensitive", "task.cryptography_sensitive", "task.infra_or_deployment",
	"task.unknown_code_ratio", "task.test_coverage_signal", "task.acceptance_ambiguity",
	"task.planner_confidence", "task.scope_conflict_score", "task.scope_growth_ratio",
	"task.latency_priority", "task.base_branch_drift",
	"attempt.number", "attempt.failures", "attempt.review_rejections", "attempt.changed_files",
	"budget.fraction", "budget.monthly_usd", "budget.spent_usd",
	"role", "harnesses", "hour", "weekday",
}

// Env renders the facts for expression evaluation.
func (f Facts) Env() MapEnv {
	t := f.Task
	ft := f.Features
	now := f.Now
	if now.IsZero() {
		now = time.Now()
	}
	paths := make([]string, 0, len(f.Scopes))
	for _, s := range f.Scopes {
		typ, key, ok := strings.Cut(s, ":")
		if ok && (typ == "path" || typ == "dir") {
			paths = append(paths, key)
		}
	}
	paths = append(paths, f.ChangedPaths...)
	risk := string(t.RiskLevel)
	if risk == "" {
		risk = string(domain.RiskUnknown)
	}
	return MapEnv{
		"task.ref":                    t.Ref,
		"task.title":                  t.Title,
		"task.objective":              t.Objective,
		"task.status":                 string(t.Status),
		"task.labels":                 t.Labels,
		"task.scopes":                 f.Scopes,
		"task.paths":                  paths,
		"task.risk":                   risk,
		"task.priority":               t.Priority,
		"task.visibility":             string(t.Visibility),
		"task.external_ref":           t.ExternalRef,
		"task.model_alias":            t.ModelAlias,
		"task.harness_pref":           t.HarnessPref,
		"task.attempts":               t.AttemptsCount,
		"task.max_attempts":           t.MaxAttempts,
		"task.depends_on":             len(t.DependsOn),
		"task.estimated_files":        ft.EstimatedFiles,
		"task.estimated_loc":          ft.EstimatedLOC,
		"task.languages":              ft.Languages,
		"task.cross_module_edges":     ft.CrossModuleEdges,
		"task.public_api_change":      ft.PublicAPIChange,
		"task.schema_or_migration":    ft.SchemaOrMigration,
		"task.security_sensitive":     ft.SecuritySensitive,
		"task.cryptography_sensitive": ft.CryptographySensitive,
		"task.infra_or_deployment":    ft.InfraOrDeployment,
		"task.unknown_code_ratio":     ft.UnknownCodeRatio,
		"task.test_coverage_signal":   ft.TestCoverageSignal,
		"task.acceptance_ambiguity":   ft.AcceptanceAmbiguity,
		"task.planner_confidence":     ft.PlannerConfidence,
		"task.scope_conflict_score":   ft.ScopeConflictScore,
		"task.scope_growth_ratio":     ft.ScopeGrowthRatio,
		"task.latency_priority":       ft.LatencyPriority,
		"task.base_branch_drift":      ft.BaseBranchDrift,
		"attempt.number":              f.AttemptNumber,
		"attempt.failures":            f.PriorFailures,
		"attempt.review_rejections":   f.ReviewRejections,
		"attempt.changed_files":       len(f.ChangedPaths),
		"budget.fraction":             f.Budget.Fraction(),
		"budget.monthly_usd":          f.Budget.MonthlyUSD,
		"budget.spent_usd":            f.Budget.SpentUSD,
		"role":                        string(f.Role),
		"harnesses":                   f.Harnesses,
		"hour":                        now.Hour(),
		"weekday":                     strings.ToLower(now.Weekday().String()),
	}
}

// ---------------------------------------------------------------------------
// Catalog
// ---------------------------------------------------------------------------

// Catalog answers what the organization knows about a model: its tier, effort ceiling, and
// capabilities. A candidate the catalog does not know is still usable — it simply cannot
// satisfy a tier or capability floor, because nobody vouched for it.
type Catalog interface {
	Profile(harness, model string) (domain.ModelProfile, bool)
	Alias(alias string, harnesses []string) (domain.ModelProfile, bool)
}

// ProfileCatalog is a Catalog over a slice of profiles.
type ProfileCatalog []domain.ModelProfile

func (c ProfileCatalog) Profile(harness, model string) (domain.ModelProfile, bool) {
	var fallback domain.ModelProfile
	found := false
	for _, p := range c {
		if !strings.EqualFold(p.Model, model) {
			continue
		}
		if harness == "" || strings.EqualFold(p.Harness, harness) {
			return p, true
		}
		if !found {
			fallback, found = p, true
		}
	}
	// A model known on another harness is still the same model: its tier carries over.
	return fallback, found
}

func (c ProfileCatalog) Alias(alias string, harnesses []string) (domain.ModelProfile, bool) {
	var best domain.ModelProfile
	found := false
	for _, p := range c {
		if !p.Enabled || p.Model == "" || !strings.EqualFold(p.Alias, alias) {
			continue
		}
		if len(harnesses) > 0 && !contains(harnesses, p.Harness) {
			continue
		}
		if !found {
			best, found = p, true
		}
	}
	return best, found
}

// ---------------------------------------------------------------------------
// Compiled dispatch policy
// ---------------------------------------------------------------------------

// Dispatch is a compiled dispatch policy: every `when` parsed once, ready to resolve.
type Dispatch struct {
	Policy *domain.DispatchPolicy
	rules  []compiledRule
	lanes  map[string]compiledLane
}

type compiledRule struct {
	rule domain.DispatchRule
	when *Expr
}

type compiledLane struct {
	name string
	lane domain.DispatchLane
	cand []compiledCandidate
}

type compiledCandidate struct {
	c    domain.DispatchCandidate
	when *Expr
}

// Issue is one lint finding.
type Issue struct {
	Severity string `json:"severity"` // error | warning | info
	Where    string `json:"where"`
	Message  string `json:"message"`
}

func (i Issue) String() string { return fmt.Sprintf("%-7s %s: %s", i.Severity, i.Where, i.Message) }

// CompileDispatch parses a policy. Errors are returned as issues so a linter can show all of
// them at once; a policy with any error-severity issue must not be used.
func CompileDispatch(p *domain.DispatchPolicy) (*Dispatch, []Issue) {
	var issues []Issue
	d := &Dispatch{Policy: p, lanes: map[string]compiledLane{}}
	if p == nil {
		return d, nil
	}
	seen := map[string]bool{}
	for i, r := range p.Rules {
		where := fmt.Sprintf("rules[%d]", i)
		if r.ID != "" {
			where = "rule " + r.ID
			if seen[r.ID] {
				issues = append(issues, Issue{"error", where, "duplicate rule id"})
			}
			seen[r.ID] = true
		} else {
			issues = append(issues, Issue{"warning", where, "rule has no id; decisions will cite it by index"})
		}
		e, err := Compile(r.When)
		if err != nil {
			issues = append(issues, Issue{"error", where, err.Error()})
			continue
		}
		issues = append(issues, unknownVars(where, e)...)
		if r.Lane != "" {
			if _, ok := p.Lanes[r.Lane]; !ok {
				issues = append(issues, Issue{"error", where, fmt.Sprintf("lane %q is not defined", r.Lane)})
			}
		}
		if r.Lane == "" && r.Prefer == nil && r.Require == nil && r.Review == "" && r.Effort == "" {
			issues = append(issues, Issue{"warning", where, "rule matches but changes nothing"})
		}
		d.rules = append(d.rules, compiledRule{rule: r, when: e})
	}
	names := make([]string, 0, len(p.Lanes))
	for name := range p.Lanes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lane := p.Lanes[name]
		where := "lane " + name
		cl := compiledLane{name: name, lane: lane}
		if len(lane.Candidates) == 0 {
			issues = append(issues, Issue{"error", where, "lane has no candidates"})
		}
		for j, c := range lane.Candidates {
			cw := fmt.Sprintf("%s candidates[%d]", where, j)
			if c.Model == "" && c.Alias == "" {
				issues = append(issues, Issue{"error", cw, "candidate needs a model or an alias"})
			}
			if c.Harness == "" && c.Alias == "" {
				issues = append(issues, Issue{"warning", cw, "no harness named; the candidate will match any runner harness that has the model"})
			}
			if c.Effort != "" {
				if err := domain.Validate(c.Effort, domain.AllEfforts, "effort"); err != nil {
					issues = append(issues, Issue{"error", cw, err.Error()})
				}
			}
			e, err := Compile(c.When)
			if err != nil {
				issues = append(issues, Issue{"error", cw, err.Error()})
				continue
			}
			issues = append(issues, unknownVars(cw, e)...)
			cl.cand = append(cl.cand, compiledCandidate{c: c, when: e})
		}
		d.lanes[name] = cl
	}
	if p.Defaults.Lane != "" {
		if _, ok := p.Lanes[p.Defaults.Lane]; !ok {
			issues = append(issues, Issue{"error", "defaults.lane", fmt.Sprintf("lane %q is not defined", p.Defaults.Lane)})
		}
	}
	switch p.Defaults.OnFailure {
	case "", "escalate", "retry", "stop":
	default:
		issues = append(issues, Issue{"error", "defaults.on_failure", "must be escalate, retry, or stop"})
	}
	for i, l := range p.Limits {
		where := fmt.Sprintf("limits[%d]", i)
		if l.MaxConcurrent <= 0 {
			issues = append(issues, Issue{"error", where, "max_concurrent must be positive"})
		}
		matched := false
		for _, lane := range p.Lanes {
			for _, c := range lane.Candidates {
				if l.Match.Matches(c) {
					matched = true
				}
			}
		}
		if !matched {
			issues = append(issues, Issue{"warning", where, "matches no candidate (" + l.Match.String() + ")"})
		}
	}
	return d, issues
}

// HasErrors reports whether any issue forbids using the policy.
func HasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Severity == "error" {
			return true
		}
	}
	return false
}

func unknownVars(where string, e *Expr) []Issue {
	var out []Issue
	for _, v := range e.Vars() {
		if !contains(KnownFacts, v) {
			out = append(out, Issue{"warning", where, fmt.Sprintf("unknown fact %q (it will always be null)", v)})
		}
	}
	return out
}

// ResolveOptions tunes one resolution.
type ResolveOptions struct {
	Catalog Catalog
	// InFlight counts running attempts per "harness:model" key, for concurrency limits.
	InFlight map[string]int
	// Lane forces a lane, bypassing rules and defaults (e.g. a planner run).
	Lane string
}

// Resolve chooses the model for one attempt and explains the choice.
//
// Order: rules pick the lane and any floors or preferences; the lane's ladder is filtered to
// eligible candidates; preferences reorder without excluding; failure history walks down the
// ladder. A floor is a floor — a preferred candidate below a required tier is not chosen,
// however cheap or idle it is.
func (d *Dispatch) Resolve(f Facts, opts ResolveOptions) (domain.DispatchDecision, error) {
	dec := domain.DispatchDecision{}
	if d == nil || d.Policy.Empty() {
		return dec, fmt.Errorf("%w: no dispatch policy", domain.ErrNotFound)
	}
	env := f.Env()
	cat := opts.Catalog
	if cat == nil {
		cat = ProfileCatalog(nil)
	}

	// 1. Rules.
	var (
		laneName = opts.Lane
		prefer   *domain.DispatchSelector
		require  domain.CapabilityRequirement
		review   string
		effort   domain.Effort
	)
	for i, cr := range d.rules {
		ok, err := cr.when.Bool(env)
		if err != nil {
			return dec, fmt.Errorf("rule %s: %w", ruleName(cr.rule, i), err)
		}
		if !ok {
			continue
		}
		dec.Rules = append(dec.Rules, ruleName(cr.rule, i))
		if cr.rule.Lane != "" && opts.Lane == "" {
			laneName = cr.rule.Lane
		}
		if cr.rule.Prefer != nil {
			p := *cr.rule.Prefer
			prefer = &p
		}
		if cr.rule.Require != nil {
			require = mergeRequirement(require, *cr.rule.Require)
		}
		if cr.rule.Review != "" {
			review = cr.rule.Review
		}
		if cr.rule.Effort != "" {
			effort = cr.rule.Effort
		}
		if cr.rule.Stop {
			break
		}
	}
	if len(dec.Rules) > 0 {
		dec.Rationale = append(dec.Rationale, "matched rules: "+strings.Join(dec.Rules, ", "))
	}

	// 2. Lane.
	if laneName == "" {
		laneName = d.Policy.Defaults.Lane
	}
	if laneName == "" {
		laneName = d.laneForRole(f.Role)
	}
	lane, ok := d.lanes[laneName]
	if !ok {
		return dec, fmt.Errorf("%w: dispatch lane %q is not defined", domain.ErrInvalidArgument, laneName)
	}
	dec.Lane = laneName
	dec.Role = lane.lane.Role
	if dec.Role == "" {
		dec.Role = f.Role
	}
	if review == "" {
		review = lane.lane.Review
	}
	dec.Rationale = append(dec.Rationale, fmt.Sprintf("lane %s (%d candidate(s))", laneName, len(lane.cand)))
	if !require.Empty() {
		dec.Rationale = append(dec.Rationale, "floor: "+require.Describe())
	}

	// 3. Eligibility.
	type scored struct {
		idx     int
		verdict domain.DispatchCandidateVerdict
		effort  domain.Effort
		profile domain.ModelProfile
		known   bool
		cand    domain.DispatchCandidate
	}
	var eligible []scored
	for _, cc := range lane.cand {
		c := cc.c
		v := domain.DispatchCandidateVerdict{Lane: laneName, Model: c.Model, Harness: c.Harness}
		reject := func(reason string) {
			v.Eligible, v.Reason = false, reason
			dec.Candidates = append(dec.Candidates, v)
		}

		// Alias → concrete profile.
		profile, known := domain.ModelProfile{}, false
		if c.Alias != "" && c.Model == "" {
			profile, known = cat.Alias(c.Alias, f.Harnesses)
			if !known {
				reject(fmt.Sprintf("alias %q has no enabled profile", c.Alias))
				continue
			}
			c.Model, c.Harness, c.Provider = profile.Model, profile.Harness, profile.Provider
			v.Model, v.Harness = c.Model, c.Harness
		} else {
			profile, known = cat.Profile(c.Harness, c.Model)
		}
		if known {
			v.Tier = profile.Tier
		}

		if ok, err := cc.when.Bool(env); err != nil {
			return dec, fmt.Errorf("candidate %s: %w", c.Name(), err)
		} else if !ok {
			reject("condition not met: " + cc.c.When)
			continue
		}
		if len(f.Harnesses) > 0 && c.Harness != "" && !contains(f.Harnesses, c.Harness) {
			reject(fmt.Sprintf("harness %s is not available here (have %s)", c.Harness, strings.Join(f.Harnesses, ", ")))
			continue
		}
		if require.Harness != "" && !strings.EqualFold(require.Harness, c.Harness) {
			reject("floor requires harness " + require.Harness)
			continue
		}
		if require.Model != "" && !strings.EqualFold(require.Model, c.Model) {
			reject("floor requires model " + require.Model)
			continue
		}
		if require.Tier != "" {
			if !known {
				reject(fmt.Sprintf("floor requires tier ≥ %s but %s is not in the model catalog", require.Tier, c.Model))
				continue
			}
			if !profile.Tier.AtLeast(require.Tier) {
				reject(fmt.Sprintf("tier %s is below the required %s", profile.Tier, require.Tier))
				continue
			}
		}
		if len(require.Capabilities) > 0 {
			if !known {
				reject("floor names capabilities but the model is not in the catalog")
				continue
			}
			if !profile.HasCapabilities(require.Capabilities) {
				reject("missing a required capability from " + strings.Join(require.Capabilities, ", "))
				continue
			}
		}

		// Effort: candidate > profile > medium; floors raise, never lower.
		eff := c.Effort
		if eff == "" && known && profile.ReasoningEffort != "" {
			eff = profile.ReasoningEffort
		}
		if eff == "" {
			eff = domain.EffortMedium
		}
		if require.Effort != "" && !eff.AtLeast(require.Effort) {
			eff = require.Effort
		}
		v.Effort = eff

		// Concurrency.
		key := c.Harness + ":" + c.Model
		if c.MaxConcurrent > 0 && opts.InFlight[key] >= c.MaxConcurrent {
			reject(fmt.Sprintf("at its concurrency limit (%d/%d running)", opts.InFlight[key], c.MaxConcurrent))
			continue
		}
		if limited, n, max := d.limited(c, opts.InFlight); limited {
			reject(fmt.Sprintf("limit %s reached (%d/%d running)", limitName(c, d.Policy.Limits), n, max))
			continue
		}

		v.Eligible = true
		dec.Candidates = append(dec.Candidates, v)
		eligible = append(eligible, scored{idx: len(dec.Candidates) - 1, verdict: v, effort: eff, profile: profile, known: known, cand: c})
	}
	if len(eligible) == 0 {
		return dec, fmt.Errorf("%w: no eligible candidate on lane %s", domain.ErrCapacity, laneName)
	}

	// 4. Preference reorders without excluding.
	if prefer != nil && !prefer.Empty() {
		var first, rest []scored
		for _, s := range eligible {
			if prefer.Matches(s.cand) {
				first = append(first, s)
			} else {
				rest = append(rest, s)
			}
		}
		if len(first) > 0 {
			dec.Rationale = append(dec.Rationale, fmt.Sprintf("preferred %s (%d match)", prefer.String(), len(first)))
			eligible = append(first, rest...)
		} else {
			dec.Rationale = append(dec.Rationale, fmt.Sprintf("preference %s matched no eligible candidate; using ladder order", prefer.String()))
		}
	}

	// 5. Failure history walks the ladder.
	pick := 0
	switch d.Policy.Defaults.OnFailure {
	case "stop":
		if f.PriorFailures > 0 {
			return dec, fmt.Errorf("%w: policy stops after a failure (%d so far)", domain.ErrCapacity, f.PriorFailures)
		}
	case "retry":
	default: // escalate
		maxSteps := d.Policy.Defaults.MaxEscalations
		if maxSteps <= 0 {
			maxSteps = len(eligible) - 1
		}
		steps := min(f.PriorFailures, maxSteps, len(eligible)-1)
		if steps > 0 {
			dec.Escalations = steps
			dec.Rationale = append(dec.Rationale, fmt.Sprintf("escalated %d step(s) after %d prior failure(s)", steps, f.PriorFailures))
		}
		pick = steps
	}

	chosen := eligible[pick]
	dec.Candidates[chosen.idx].Chosen = true
	dec.Harness = chosen.cand.Harness
	dec.Model = chosen.cand.Model
	dec.Provider = chosen.cand.Provider
	dec.Alias = chosen.cand.Alias
	if chosen.known {
		dec.Tier = chosen.profile.Tier
		if dec.Provider == "" {
			dec.Provider = chosen.profile.Provider
		}
		if dec.Alias == "" {
			dec.Alias = chosen.profile.Alias
		}
	}
	dec.Effort = chosen.effort
	if effort != "" {
		dec.Effort = effort
		dec.Rationale = append(dec.Rationale, "effort set to "+string(effort)+" by rule")
	}
	dec.Review = review
	if dec.Tier.AtLeast(domain.TierT4) && dec.Tier != "" {
		dec.Review = "required"
	}
	dec.Rationale = append(dec.Rationale, fmt.Sprintf("chose %s at effort %s", chosen.cand.Name(), dec.Effort))
	return dec, nil
}

// laneForRole finds the lane a role should use when nothing named one.
func (d *Dispatch) laneForRole(role domain.AgentRole) string {
	if l, ok := d.lanes["implement"]; ok && (role == "" || role == domain.RoleImplementer || l.lane.Role == role) {
		return "implement"
	}
	names := make([]string, 0, len(d.lanes))
	for n := range d.lanes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if d.lanes[n].lane.Role == role {
			return n
		}
	}
	if len(names) == 1 {
		return names[0]
	}
	if _, ok := d.lanes["implement"]; ok {
		return "implement"
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// LaneForRole is the exported form, for callers that need the lane name up front.
func (d *Dispatch) LaneForRole(role domain.AgentRole) string { return d.laneForRole(role) }

// Lanes lists lane names, sorted.
func (d *Dispatch) Lanes() []string {
	out := make([]string, 0, len(d.lanes))
	for n := range d.lanes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (d *Dispatch) limited(c domain.DispatchCandidate, inFlight map[string]int) (bool, int, int) {
	for _, l := range d.Policy.Limits {
		if !l.Match.Matches(c) {
			continue
		}
		n := 0
		for _, lane := range d.Policy.Lanes {
			for _, other := range lane.Candidates {
				if l.Match.Matches(other) {
					n += inFlight[other.Harness+":"+other.Model]
				}
			}
		}
		if n >= l.MaxConcurrent {
			return true, n, l.MaxConcurrent
		}
	}
	return false, 0, 0
}

func limitName(c domain.DispatchCandidate, limits []domain.DispatchLimit) string {
	for _, l := range limits {
		if l.Match.Matches(c) {
			return l.Match.String()
		}
	}
	return ""
}

func ruleName(r domain.DispatchRule, i int) string {
	if r.ID != "" {
		return r.ID
	}
	return fmt.Sprintf("rules[%d]", i)
}

func mergeRequirement(base, add domain.CapabilityRequirement) domain.CapabilityRequirement {
	if add.Tier != "" && (base.Tier == "" || add.Tier.AtLeast(base.Tier)) {
		base.Tier = add.Tier
	}
	if add.Effort != "" && (base.Effort == "" || add.Effort.AtLeast(base.Effort)) {
		base.Effort = add.Effort
	}
	if add.Harness != "" {
		base.Harness = add.Harness
	}
	if add.Model != "" {
		base.Model = add.Model
	}
	if add.Role != "" {
		base.Role = add.Role
	}
	if add.ContextWindow > base.ContextWindow {
		base.ContextWindow = add.ContextWindow
	}
	for _, c := range add.Capabilities {
		if !contains(base.Capabilities, c) {
			base.Capabilities = append(base.Capabilities, c)
		}
	}
	return base
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Router rules (policies.yaml)
// ---------------------------------------------------------------------------

// RouterEffects is what the matching router rules demand of a routing decision.
type RouterEffects struct {
	Floor                    domain.Tier
	PreferTier               domain.Tier
	EscalateSteps            int
	RequireIndependentReview bool
	HumanMergeApproval       bool
	RequireReplan            bool
	RequireRollbackPlan      bool
	MaxParallelWriters       int
	RequireScopes            []string
	Matched                  []string
}

// ApplyRouterRules evaluates policies.yaml router rules against the facts. Rules only ever
// raise: a floor from one rule is never lowered by another.
func ApplyRouterRules(rules []domain.RouterRule, env Env) (RouterEffects, error) {
	var eff RouterEffects
	for i, r := range rules {
		e, err := Compile(r.When)
		if err != nil {
			return eff, err
		}
		ok, err := e.Bool(env)
		if err != nil {
			return eff, fmt.Errorf("router rule %s: %w", routerRuleName(r, i), err)
		}
		if !ok {
			continue
		}
		eff.Matched = append(eff.Matched, routerRuleName(r, i))
		if r.RequireTier != "" && (eff.Floor == "" || r.RequireTier.AtLeast(eff.Floor)) {
			eff.Floor = r.RequireTier
		}
		if r.PreferTier != "" && (eff.PreferTier == "" || r.PreferTier.AtLeast(eff.PreferTier)) {
			eff.PreferTier = r.PreferTier
		}
		if r.EscalateOneTier {
			eff.EscalateSteps++
		}
		eff.RequireIndependentReview = eff.RequireIndependentReview || r.RequireIndependentReview
		eff.HumanMergeApproval = eff.HumanMergeApproval || r.HumanMergeApproval
		eff.RequireReplan = eff.RequireReplan || r.RequireReplan
		eff.RequireRollbackPlan = eff.RequireRollbackPlan || r.RequireRollbackPlan
		if r.MaxParallelWriters > 0 && (eff.MaxParallelWriters == 0 || r.MaxParallelWriters < eff.MaxParallelWriters) {
			eff.MaxParallelWriters = r.MaxParallelWriters
		}
		if r.RequireScope != "" {
			eff.RequireScopes = append(eff.RequireScopes, r.RequireScope)
		}
	}
	return eff, nil
}

// LintRouterRules checks policies.yaml router rules without evaluating them.
func LintRouterRules(rules []domain.RouterRule) []Issue {
	var issues []Issue
	seen := map[string]bool{}
	for i, r := range rules {
		where := "router rule " + routerRuleName(r, i)
		if r.ID != "" {
			if seen[r.ID] {
				issues = append(issues, Issue{"error", where, "duplicate rule id"})
			}
			seen[r.ID] = true
		}
		e, err := Compile(r.When)
		if err != nil {
			issues = append(issues, Issue{"error", where, err.Error()})
			continue
		}
		issues = append(issues, unknownVars(where, e)...)
		for _, t := range []domain.Tier{r.RequireTier, r.PreferTier} {
			if t != "" {
				if err := domain.Validate(t, []domain.Tier{domain.TierT0, domain.TierT1, domain.TierT2, domain.TierT3, domain.TierT4}, "tier"); err != nil {
					issues = append(issues, Issue{"error", where, err.Error()})
				}
			}
		}
		if r.RequireTier == "" && r.PreferTier == "" && !r.EscalateOneTier && !r.RequireReplan &&
			!r.RequireIndependentReview && !r.HumanMergeApproval && r.MaxParallelWriters == 0 &&
			r.RequireScope == "" && !r.RequireRollbackPlan {
			issues = append(issues, Issue{"warning", where, "rule matches but changes nothing"})
		}
	}
	return issues
}

func routerRuleName(r domain.RouterRule, i int) string {
	if r.ID != "" {
		return r.ID
	}
	return fmt.Sprintf("rules[%d]", i)
}
