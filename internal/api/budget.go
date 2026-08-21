package api

import (
	"fmt"
	"net/http"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
)

// Token budget endpoints (DESIGN.md §13.8).
//
// Budgets are coordination state, so the whole team may read them: who has headroom and
// who is running dry is exactly the kind of signal that lets one member offer another a
// grant before work stalls. What was spent *on* stays private — balances are token counts,
// never content.

func (s *Server) getBudget(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	policy := project.Config.Budget
	members, err := s.store.MemberBudgets(r.Context(), project.ID, policy.MemberTokens)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if members == nil {
		members = []domain.MemberBudget{}
	}
	spentUSD, err := s.store.SpendSince(r.Context(), project.ID, "30 days")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{
		"policy":  policy,
		"window":  "30 days",
		"project": map[string]any{"monthly_usd": policy.MonthlyUSD, "spent_usd": spentUSD},
		"members": members,
	})
}

type shareBudgetBody struct {
	To     string `json:"to"`
	Tokens int64  `json:"tokens"`
	Note   string `json:"note"`
}

// shareBudget moves part of the caller's window allowance to a teammate.
//
// The giver is always the caller: sharing spends your own headroom, so it needs no role
// beyond contributor and no consent flow — the recipient only gains.
func (s *Server) shareBudget(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body shareBudgetBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.idempotent(w, r, p) {
		return
	}
	policy := project.Config.Budget
	if policy.MemberTokens <= 0 {
		s.fail(w, r, fmt.Errorf(
			"%w: per-member token budgets are not enabled; set budget.member.monthly_tokens in .conductor/policies.yaml",
			domain.ErrInvalidArgument))
		return
	}
	if body.To == "" {
		s.fail(w, r, fmt.Errorf("%w: 'to' (recipient handle) is required", domain.ErrInvalidArgument))
		return
	}
	recipient, err := s.store.GetPrincipalByHandle(r.Context(), project.OrganizationID, body.To)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	grant, err := s.store.ShareBudget(r.Context(), db.ShareBudgetParams{
		ProjectID:     project.ID,
		FromPrincipal: caller.Principal.ID,
		ToPrincipal:   recipient.ID,
		Tokens:        body.Tokens,
		Note:          privacy.ClampSummary(body.Note),
		Allowance:     policy.MemberTokens,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.store.Audit(r.Context(), project.OrganizationID, project.ID, caller.Principal.ID,
		"budget.shared", "principal", recipient.ID,
		map[string]any{"to": recipient.Handle, "tokens": grant.Tokens})

	// Return both updated positions so the CLI can confirm the transfer without a second
	// round trip.
	giver, err := s.store.MemberBudget(r.Context(), project.ID, caller.Principal.ID, policy.MemberTokens)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	receiver, err := s.store.MemberBudget(r.Context(), project.ID, recipient.ID, policy.MemberTokens)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	result := map[string]any{"grant": grant, "from": giver, "to": receiver}
	s.remember(r, p, http.StatusCreated, result)
	s.ok(w, r, http.StatusCreated, result)
}

func (s *Server) listBudgetGrants(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	grants, err := s.store.ListBudgetGrants(r.Context(), project.ID, intParam(r, "limit", 50))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if grants == nil {
		grants = []domain.BudgetGrant{}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"grants": grants})
}
