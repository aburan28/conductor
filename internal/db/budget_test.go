package db

import (
	"errors"
	"sync"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// DESIGN.md §13.8 — token budget sharing
// ---------------------------------------------------------------------------

func memberOf(t *testing.T, budgets []domain.MemberBudget, id domain.ID) domain.MemberBudget {
	t.Helper()
	for _, b := range budgets {
		if b.PrincipalID == id {
			return b
		}
	}
	t.Fatalf("principal %s missing from budget table", id)
	return domain.MemberBudget{}
}

func TestShareBudgetMovesAllowance(t *testing.T) {
	f := newFixture(t)
	const allowance = 1000

	grant, err := f.store.ShareBudget(f.ctx, ShareBudgetParams{
		ProjectID: f.project.ID, FromPrincipal: f.alice.ID, ToPrincipal: f.bob.ID,
		Tokens: 400, Note: "covering the router refactor", Allowance: allowance,
	})
	if err != nil {
		t.Fatalf("ShareBudget: %v", err)
	}
	if grant.FromHandle != "alice" || grant.ToHandle != "bob" || grant.Tokens != 400 {
		t.Errorf("grant = %s→%s %d, want alice→bob 400", grant.FromHandle, grant.ToHandle, grant.Tokens)
	}

	budgets, err := f.store.MemberBudgets(f.ctx, f.project.ID, allowance)
	if err != nil {
		t.Fatalf("MemberBudgets: %v", err)
	}
	alice := memberOf(t, budgets, f.alice.ID)
	bob := memberOf(t, budgets, f.bob.ID)
	if alice.SharedOut != 400 || alice.Remaining != 600 {
		t.Errorf("alice out=%d remaining=%d, want 400/600", alice.SharedOut, alice.Remaining)
	}
	if bob.SharedIn != 400 || bob.Remaining != 1400 {
		t.Errorf("bob in=%d remaining=%d, want 400/1400", bob.SharedIn, bob.Remaining)
	}

	// The transfer is conserved: nothing was minted, only moved.
	if alice.Remaining+bob.Remaining != 2*allowance {
		t.Errorf("total remaining = %d, want %d", alice.Remaining+bob.Remaining, 2*allowance)
	}

	// A second share must fit inside what is left, not inside the original allowance.
	_, err = f.store.ShareBudget(f.ctx, ShareBudgetParams{
		ProjectID: f.project.ID, FromPrincipal: f.alice.ID, ToPrincipal: f.bob.ID,
		Tokens: 700, Allowance: allowance,
	})
	if !errors.Is(err, domain.ErrBudgetExhausted) {
		t.Errorf("over-share error = %v, want ErrBudgetExhausted", err)
	}

	// The ledger shows the one transfer that happened.
	grants, err := f.store.ListBudgetGrants(f.ctx, f.project.ID, 10)
	if err != nil {
		t.Fatalf("ListBudgetGrants: %v", err)
	}
	if len(grants) != 1 || grants[0].Tokens != 400 || grants[0].Note == "" {
		t.Errorf("grants = %+v, want one 400-token grant with its note", grants)
	}

	// And the team stream carries the coordination fact.
	events, err := f.store.ListEvents(f.ctx, f.project.ID, 20)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Type == "budget.shared" {
			found = true
			if e.Payload["from"] != "alice" || e.Payload["to"] != "bob" {
				t.Errorf("budget.shared payload = %+v, want from=alice to=bob", e.Payload)
			}
		}
	}
	if !found {
		t.Error("no budget.shared event was appended")
	}
}

func TestShareBudgetRejectsBadGrants(t *testing.T) {
	f := newFixture(t)

	if _, err := f.store.ShareBudget(f.ctx, ShareBudgetParams{
		ProjectID: f.project.ID, FromPrincipal: f.alice.ID, ToPrincipal: f.alice.ID,
		Tokens: 100, Allowance: 1000,
	}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("self-share error = %v, want ErrInvalidArgument", err)
	}

	if _, err := f.store.ShareBudget(f.ctx, ShareBudgetParams{
		ProjectID: f.project.ID, FromPrincipal: f.alice.ID, ToPrincipal: f.bob.ID,
		Tokens: 0, Allowance: 1000,
	}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("zero-token error = %v, want ErrInvalidArgument", err)
	}

	// A principal in the organization but outside the project cannot receive budget:
	// tokens parked on a non-member could never be spent, and the grant would disclose
	// project membership to them.
	carol, err := f.store.CreatePrincipal(f.ctx, f.org.ID, domain.PrincipalHuman, "carol", "Carol", "")
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if _, err := f.store.ShareBudget(f.ctx, ShareBudgetParams{
		ProjectID: f.project.ID, FromPrincipal: f.alice.ID, ToPrincipal: carol.ID,
		Tokens: 100, Allowance: 1000,
	}); !errors.Is(err, domain.ErrNotPermitted) {
		t.Errorf("non-member share error = %v, want ErrNotPermitted", err)
	}
}

// A member who has spent their window allowance cannot start new work — until a teammate
// shares theirs. This is the collaborative loop the feature exists for.
func TestClaimHonorsMemberTokenBudget(t *testing.T) {
	f := newFixture(t)
	const allowance = 1000

	first := f.newTask(t, "First task")
	params := f.claimParams(first, f.alice)
	params.MemberTokenBudget = allowance
	claim, err := f.store.Claim(f.ctx, params)
	if err != nil {
		t.Fatalf("initial claim with a fresh budget: %v", err)
	}

	// The attempt burns through more than the allowance. Spend is recorded in full — the
	// balance goes negative rather than the report being clipped.
	if _, err := f.store.UpdateAttempt(f.ctx, claim.Attempt.ID, AttemptProgress{
		State: domain.AttemptRunning, TokensIn: 900, TokensOut: 300,
	}); err != nil {
		t.Fatalf("UpdateAttempt: %v", err)
	}

	second := f.newTask(t, "Second task")
	params = f.claimParams(second, f.alice)
	params.MemberTokenBudget = allowance
	if _, err := f.store.Claim(f.ctx, params); !errors.Is(err, domain.ErrBudgetExhausted) {
		t.Fatalf("claim over budget = %v, want ErrBudgetExhausted", err)
	}

	// Bob has headroom and shares it. Alice is unblocked without any admin involvement.
	if _, err := f.store.ShareBudget(f.ctx, ShareBudgetParams{
		ProjectID: f.project.ID, FromPrincipal: f.bob.ID, ToPrincipal: f.alice.ID,
		Tokens: 500, Allowance: allowance,
	}); err != nil {
		t.Fatalf("ShareBudget: %v", err)
	}
	if _, err := f.store.Claim(f.ctx, params); err != nil {
		t.Fatalf("claim after grant: %v", err)
	}

	// A zero policy disables the check entirely.
	third := f.newTask(t, "Third task")
	params = f.claimParams(third, f.alice)
	params.MemberTokenBudget = 0
	if _, err := f.store.Claim(f.ctx, params); err != nil {
		t.Fatalf("claim with budgets disabled: %v", err)
	}
}

// Two simultaneous shares must not overdraw the giver. The share path runs under the
// project advisory lock, so generosity is serialized; this proves the lock is actually
// doing that job.
func TestConcurrentSharesCannotOverdraw(t *testing.T) {
	f := newFixture(t)
	const allowance = 1000

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = f.store.ShareBudget(f.ctx, ShareBudgetParams{
				ProjectID: f.project.ID, FromPrincipal: f.alice.ID, ToPrincipal: f.bob.ID,
				Tokens: 300, Allowance: allowance,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, domain.ErrBudgetExhausted):
			// Expected loss.
		default:
			t.Errorf("racer %d got unexpected error: %v", i, err)
		}
	}
	// 3 × 300 fits inside 1000; a fourth would overdraw.
	if succeeded != 3 {
		t.Errorf("%d concurrent shares succeeded, want exactly 3", succeeded)
	}

	budgets, err := f.store.MemberBudgets(f.ctx, f.project.ID, allowance)
	if err != nil {
		t.Fatalf("MemberBudgets: %v", err)
	}
	if alice := memberOf(t, budgets, f.alice.ID); alice.Remaining < 0 {
		t.Errorf("alice remaining = %d; concurrent shares overdrew the balance", alice.Remaining)
	}
}
