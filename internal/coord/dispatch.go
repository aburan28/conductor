package coord

import (
	"context"
	"fmt"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/policy"
	"github.com/adamburan/conductor/internal/router"
)

// Dispatch resolution shared by the runner, the scheduler, and `conductor route`.
//
// One place derives the facts, applies the repository's dispatch policy when it has one, and
// falls back to the built-in tier router when it does not — so a preview and the real run
// always agree. The catalog and in-flight counts come from the store; everything the policy
// reads is deterministic ledger state.

// RouteResult is the outcome of resolving a model for a task.
type RouteResult struct {
	// Decision is the built-in tier router's answer, always populated: it carries the
	// provenance (policy version, floors, review requirements) recorded on every attempt.
	Decision router.Decision `json:"decision"`
	// Dispatch is the repository dispatch policy's answer, when one is configured. When set,
	// its Model/Harness/Effort are what actually runs; Decision still supplies the floors it
	// was clamped to.
	Dispatch *domain.DispatchDecision `json:"dispatch,omitempty"`
	Features domain.TaskFeatures      `json:"features"`
}

// Model, Harness, and Effort report what should actually run, preferring the dispatch policy.
func (r RouteResult) Model() string {
	if r.Dispatch != nil && r.Dispatch.Model != "" {
		return r.Dispatch.Model
	}
	return r.Decision.Model
}

func (r RouteResult) Harness() string {
	if r.Dispatch != nil && r.Dispatch.Harness != "" {
		return r.Dispatch.Harness
	}
	return r.Decision.Harness
}

func (r RouteResult) Effort() domain.Effort {
	if r.Dispatch != nil && r.Dispatch.Effort != "" {
		return r.Dispatch.Effort
	}
	return r.Decision.Effort
}

// ResolveRoute resolves a model for a task in a given role, using the project's stored
// policy. availableHarnesses restricts candidates to what a particular runner can launch;
// pass nil for a policy preview.
func (s *Service) ResolveRoute(ctx context.Context, projectID domain.ID, task domain.Task, role domain.AgentRole, availableHarnesses []string) (RouteResult, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return RouteResult{}, err
	}
	profiles, err := s.Store.ListModelProfiles(ctx, project.OrganizationID)
	if err != nil {
		return RouteResult{}, err
	}
	spent, err := s.Store.SpendSince(ctx, projectID, "30 days")
	if err != nil {
		return RouteResult{}, err
	}
	reservations, err := s.Store.ReservationsForTask(ctx, task.ID)
	if err != nil {
		return RouteResult{}, err
	}
	scopes := make([]string, 0, len(reservations))
	for _, r := range reservations {
		scopes = append(scopes, r.Resource())
	}

	feat := router.DeriveFeatures(scopes, nil,
		router.FeaturePolicyFrom(project.Config.FeaturePaths), task.Features)

	facts := policy.Facts{
		Task: task, Features: feat, Scopes: scopes,
		AttemptNumber: task.AttemptsCount + 1, PriorFailures: max(0, task.AttemptsCount),
		Role: role, Harnesses: availableHarnesses,
		Budget: policy.BudgetFacts{MonthlyUSD: project.Config.Budget.MonthlyUSD, SpentUSD: spent},
	}

	req := router.Request{
		Role: role, Features: feat, RiskLevel: task.RiskLevel,
		AttemptNumber: task.AttemptsCount + 1, PriorFailures: max(0, task.AttemptsCount),
		RequestedAlias: task.ModelAlias, HarnessPref: task.HarnessPref,
		AvailableHarnesses: availableHarnesses,
		Budget: router.BudgetState{
			MonthlyUSD: project.Config.Budget.MonthlyUSD, SpentUSD: spent,
			DownshiftAt: project.Config.Budget.DownshiftAt, PauseAt: project.Config.Budget.PauseAt,
		},
		Rules: project.Config.RouterRules, Facts: &facts,
	}
	decision, err := router.Route(buildRouterCatalog(profiles), req)
	if err != nil {
		return RouteResult{}, err
	}
	result := RouteResult{Decision: decision, Features: feat}

	// Repository dispatch policy, when configured, chooses the concrete model within the
	// floors the router just computed.
	if dp := project.Config.Dispatch; dp != nil && !dp.Empty() {
		compiled, issues := policy.CompileDispatch(dp)
		if policy.HasErrors(issues) {
			return result, fmt.Errorf("%w: dispatch policy has errors (%d); run `conductor policy lint`",
				domain.ErrInvalidArgument, countErrors(issues))
		}
		inFlight, err := s.Store.InFlightByModel(ctx, projectID)
		if err != nil {
			return result, err
		}
		// The router's floor is a floor the dispatch policy must also honour.
		facts.Task = task
		dd, derr := compiled.Resolve(facts, policy.ResolveOptions{
			Catalog:  policy.ProfileCatalog(profiles),
			InFlight: inFlight,
		})
		if derr == nil {
			// Clamp the dispatch choice up to the router's hard floor: a dispatch policy can
			// never route a security task below its tier, whatever its ladder says.
			if decision.Tier != "" && dd.Tier != "" && !dd.Tier.AtLeast(decision.Tier) {
				dd.Rationale = append(dd.Rationale,
					fmt.Sprintf("router floor %s exceeds this candidate's tier %s; using the router's model instead", decision.Tier, dd.Tier))
			} else {
				result.Dispatch = &dd
			}
		} else {
			// A dispatch policy that cannot place the work is not fatal — the tier router's
			// decision stands, with a note.
			decision.Rationale = append(decision.Rationale, "dispatch policy: "+derr.Error())
			result.Decision = decision
		}
	}
	return result, nil
}

func countErrors(issues []policy.Issue) int {
	n := 0
	for _, i := range issues {
		if i.Severity == "error" {
			n++
		}
	}
	return n
}

// buildRouterCatalog derives alias policies from stored profiles, mirroring the runner's own
// catalog construction so a preview matches a real run.
func buildRouterCatalog(profiles []domain.ModelProfile) router.Catalog {
	cat := router.Catalog{
		Profiles: profiles, Aliases: map[string]router.AliasPolicy{},
		DefaultAlias: "worker.general",
	}
	for _, p := range profiles {
		if _, ok := cat.Aliases[p.Alias]; ok {
			continue
		}
		cat.Aliases[p.Alias] = router.AliasPolicy{DefaultEffort: p.ReasoningEffort, Tier: p.Tier}
		cat.Version = p.CatalogVersion
	}
	for _, name := range []string{"worker.fast", "worker.general", "planner.frontier"} {
		if _, ok := cat.Aliases[name]; ok {
			cat.Ladder = append(cat.Ladder, name)
		}
	}
	return cat
}
