package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// Token budget sharing over HTTP (DESIGN.md §13.8)
// ---------------------------------------------------------------------------

func enableMemberBudget(t *testing.T, h *harness, tokens int64) {
	t.Helper()
	cfg := domain.DefaultProjectConfig()
	cfg.Budget.MemberTokens = tokens
	if err := h.store.UpdateProjectConfig(context.Background(), h.project.ID, cfg); err != nil {
		t.Fatalf("UpdateProjectConfig: %v", err)
	}
}

func TestBudgetShareAndSummary(t *testing.T) {
	h := newHarness(t)
	enableMemberBudget(t, h, 1000)

	// Sharing moves headroom and reports both new positions in one response.
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/budget/share"),
		map[string]any{"to": "bob", "tokens": 400, "note": "take my slack week"})
	if code != http.StatusCreated {
		t.Fatalf("share = %d, want 201\n%s", code, body)
	}
	var shared struct {
		Grant domain.BudgetGrant  `json:"grant"`
		From  domain.MemberBudget `json:"from"`
		To    domain.MemberBudget `json:"to"`
	}
	if err := json.Unmarshal(body, &shared); err != nil {
		t.Fatalf("decode share response: %v", err)
	}
	if shared.From.Remaining != 600 || shared.To.Remaining != 1400 {
		t.Errorf("balances after share = %d/%d, want 600/1400",
			shared.From.Remaining, shared.To.Remaining)
	}

	// The summary shows every member's position to every member.
	code, body = h.do(h.bobTok, http.MethodGet, h.projectPath("/budget"), nil)
	if code != http.StatusOK {
		t.Fatalf("get budget = %d\n%s", code, body)
	}
	var summary struct {
		Members []domain.MemberBudget `json:"members"`
	}
	if err := json.Unmarshal(body, &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	byHandle := map[string]domain.MemberBudget{}
	for _, m := range summary.Members {
		byHandle[m.Handle] = m
	}
	if byHandle["alice"].SharedOut != 400 || byHandle["bob"].SharedIn != 400 {
		t.Errorf("summary does not reflect the grant: %+v", byHandle)
	}

	// Overdrawing what is left is a 402, the same signal as any exhausted budget.
	code, body = h.do(h.aliceTok, http.MethodPost, h.projectPath("/budget/share"),
		map[string]any{"to": "bob", "tokens": 700})
	if code != http.StatusPaymentRequired {
		t.Errorf("over-share = %d, want 402\n%s", code, body)
	}

	// Budget cannot leave the project: the outsider is a real principal in the org, but
	// not a member here.
	code, body = h.do(h.aliceTok, http.MethodPost, h.projectPath("/budget/share"),
		map[string]any{"to": "outsider", "tokens": 100})
	if code != http.StatusForbidden {
		t.Errorf("share to non-member = %d, want 403\n%s", code, body)
	}

	// And a non-member cannot see the team's budget table at all.
	if code, _ := h.do(h.outTok, http.MethodGet, h.projectPath("/budget"), nil); code != http.StatusNotFound {
		t.Errorf("outsider GET budget = %d, want 404", code)
	}

	// The ledger is readable by any member.
	code, body = h.do(h.bobTok, http.MethodGet, h.projectPath("/budget/grants"), nil)
	if code != http.StatusOK {
		t.Fatalf("get grants = %d\n%s", code, body)
	}
	var grants struct {
		Grants []domain.BudgetGrant `json:"grants"`
	}
	if err := json.Unmarshal(body, &grants); err != nil {
		t.Fatalf("decode grants: %v", err)
	}
	if len(grants.Grants) != 1 || grants.Grants[0].FromHandle != "alice" {
		t.Errorf("grants = %+v, want the one alice→bob transfer", grants.Grants)
	}
}

func TestShareRequiresMemberBudgetsEnabled(t *testing.T) {
	h := newHarness(t)
	// Default config: MemberTokens is 0, so there is no allowance to share and the error
	// should say how to turn the feature on rather than failing arithmetic.
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/budget/share"),
		map[string]any{"to": "bob", "tokens": 100})
	if code != http.StatusBadRequest {
		t.Errorf("share with budgets disabled = %d, want 400\n%s", code, body)
	}
}
