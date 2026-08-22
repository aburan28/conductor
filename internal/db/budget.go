package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

// Token budget sharing (DESIGN.md §13.8).
//
// Every balance here is derived, never stored: allowance comes from project policy, spend
// from the attempts ledger, and transfers from the budget_grants ledger. Deriving instead
// of maintaining a counter means there is no counter to drift — a balance is always the
// sum of things that verifiably happened.

// budgetWindow is the rolling window over which member allowances, spend, and grants are
// reckoned. It matches the project-level SpendSince window, so "monthly" means the same
// trailing 30 days everywhere budgets are discussed.
const budgetWindow = "30 days"

// ShareBudgetParams moves tokens from one member's window allowance to another's.
type ShareBudgetParams struct {
	ProjectID     domain.ID
	FromPrincipal domain.ID
	ToPrincipal   domain.ID
	Tokens        int64
	Note          string
	// Allowance is the per-member policy allowance (BudgetPolicy.MemberTokens), passed in
	// because policy lives on the project config, not in this table.
	Allowance int64
}

// ShareBudget records a grant after proving the giver can cover it.
//
// The check and the insert run under the project advisory lock — the same lock the claim
// path takes — so a share cannot interleave with a competing share or a claim's budget
// check and overdraw the giver. Generosity is bounded by arithmetic, not by trust.
func (s *Store) ShareBudget(ctx context.Context, p ShareBudgetParams) (domain.BudgetGrant, error) {
	if p.Tokens <= 0 {
		return domain.BudgetGrant{}, fmt.Errorf("%w: a grant must move a positive number of tokens",
			domain.ErrInvalidArgument)
	}
	if p.FromPrincipal == p.ToPrincipal {
		return domain.BudgetGrant{}, fmt.Errorf("%w: cannot share budget with yourself",
			domain.ErrInvalidArgument)
	}

	var out domain.BudgetGrant
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockProject(ctx, tx, p.ProjectID); err != nil {
			return err
		}

		// The recipient must be a member: a grant to an outsider would park tokens where
		// no claim can ever spend them, and quietly leak who works on the project.
		var members int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM project_memberships
			 WHERE project_id = $1::uuid AND principal_id = ANY($2::uuid[])`,
			p.ProjectID, []string{p.FromPrincipal, p.ToPrincipal}).Scan(&members); err != nil {
			return err
		}
		if members != 2 {
			return fmt.Errorf("%w: budget can only be shared between members of the project",
				domain.ErrNotPermitted)
		}

		position, err := memberBudgetTx(ctx, tx, p.ProjectID, p.FromPrincipal, p.Allowance)
		if err != nil {
			return err
		}
		if p.Tokens > position.Remaining {
			return fmt.Errorf("%w: sharing %d tokens exceeds your remaining budget of %d in the current window",
				domain.ErrBudgetExhausted, p.Tokens, max(position.Remaining, 0))
		}

		var fromHandle, toHandle string
		if err := tx.QueryRow(ctx, `
			SELECT f.handle, t.handle FROM principals f, principals t
			 WHERE f.id = $1::uuid AND t.id = $2::uuid`,
			p.FromPrincipal, p.ToPrincipal).Scan(&fromHandle, &toHandle); err != nil {
			return noRows(err)
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO budget_grants (project_id, from_principal, to_principal, tokens, note)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5)
			RETURNING id::text, project_id::text, from_principal::text, to_principal::text,
			          tokens, note, created_at`,
			p.ProjectID, p.FromPrincipal, p.ToPrincipal, p.Tokens, p.Note,
		).Scan(&out.ID, &out.ProjectID, &out.FromPrincipal, &out.ToPrincipal,
			&out.Tokens, &out.Note, &out.CreatedAt); err != nil {
			return err
		}
		out.FromHandle, out.ToHandle = fromHandle, toHandle

		var orgID domain.ID
		if err := tx.QueryRow(ctx,
			`SELECT organization_id::text FROM projects WHERE id = $1::uuid`,
			p.ProjectID).Scan(&orgID); err != nil {
			return noRows(err)
		}
		return appendEvents(ctx, tx, orgID, p.ProjectID, p.FromPrincipal,
			eventSpec{"project", p.ProjectID, "budget.shared", domain.VisibilityTeamSummary,
				map[string]any{"from": fromHandle, "to": toHandle, "tokens": p.Tokens}})
	})
	return out, err
}

// MemberBudget reports one principal's position in the current window.
func (s *Store) MemberBudget(ctx context.Context, projectID, principalID domain.ID, allowance int64) (domain.MemberBudget, error) {
	var out domain.MemberBudget
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = memberBudgetTx(ctx, tx, projectID, principalID, allowance)
		return err
	})
	if err != nil {
		return domain.MemberBudget{}, err
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT handle FROM principals WHERE id = $1::uuid`, principalID).Scan(&out.Handle); err != nil {
		return domain.MemberBudget{}, noRows(err)
	}
	return out, nil
}

// MemberBudgets reports every member's position, for the team budget table.
func (s *Store) MemberBudgets(ctx context.Context, projectID domain.ID, allowance int64) ([]domain.MemberBudget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.principal_id::text, p.handle,
		       COALESCE(spent.tokens, 0), COALESCE(gin.tokens, 0), COALESCE(gout.tokens, 0)
		  FROM project_memberships m
		  JOIN principals p ON p.id = m.principal_id
		  LEFT JOIN (SELECT sponsor_principal AS pid, sum(tokens_in + tokens_out) AS tokens
		               FROM attempts
		              WHERE project_id = $1::uuid AND created_at > now() - $2::interval
		              GROUP BY 1) spent ON spent.pid = m.principal_id
		  LEFT JOIN (SELECT to_principal AS pid, sum(tokens) AS tokens
		               FROM budget_grants
		              WHERE project_id = $1::uuid AND created_at > now() - $2::interval
		              GROUP BY 1) gin ON gin.pid = m.principal_id
		  LEFT JOIN (SELECT from_principal AS pid, sum(tokens) AS tokens
		               FROM budget_grants
		              WHERE project_id = $1::uuid AND created_at > now() - $2::interval
		              GROUP BY 1) gout ON gout.pid = m.principal_id
		 WHERE m.project_id = $1::uuid
		 ORDER BY p.handle`, projectID, budgetWindow)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.MemberBudget
	for rows.Next() {
		b := domain.MemberBudget{Allowance: allowance}
		if err := rows.Scan(&b.PrincipalID, &b.Handle, &b.Spent, &b.SharedIn, &b.SharedOut); err != nil {
			return nil, err
		}
		b.Remaining = allowance - b.Spent + b.SharedIn - b.SharedOut
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBudgetGrants returns the most recent transfers, newest first.
func (s *Store) ListBudgetGrants(ctx context.Context, projectID domain.ID, limit int) ([]domain.BudgetGrant, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT g.id::text, g.project_id::text, g.from_principal::text, g.to_principal::text,
		       f.handle, t.handle, g.tokens, g.note, g.created_at
		  FROM budget_grants g
		  JOIN principals f ON f.id = g.from_principal
		  JOIN principals t ON t.id = g.to_principal
		 WHERE g.project_id = $1::uuid
		 ORDER BY g.created_at DESC
		 LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.BudgetGrant
	for rows.Next() {
		var g domain.BudgetGrant
		if err := rows.Scan(&g.ID, &g.ProjectID, &g.FromPrincipal, &g.ToPrincipal,
			&g.FromHandle, &g.ToHandle, &g.Tokens, &g.Note, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// memberBudgetTx computes one principal's window position inside the caller's transaction,
// so the claim path and the share path both see balances consistent with the locks they
// already hold.
func memberBudgetTx(ctx context.Context, tx pgx.Tx, projectID, principalID domain.ID, allowance int64) (domain.MemberBudget, error) {
	b := domain.MemberBudget{PrincipalID: principalID, Allowance: allowance}
	err := tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT sum(tokens_in + tokens_out) FROM attempts
		                  WHERE project_id = $1::uuid AND sponsor_principal = $2::uuid
		                    AND created_at > now() - $3::interval), 0),
		       COALESCE((SELECT sum(tokens) FROM budget_grants
		                  WHERE project_id = $1::uuid AND to_principal = $2::uuid
		                    AND created_at > now() - $3::interval), 0),
		       COALESCE((SELECT sum(tokens) FROM budget_grants
		                  WHERE project_id = $1::uuid AND from_principal = $2::uuid
		                    AND created_at > now() - $3::interval), 0)`,
		projectID, principalID, budgetWindow).Scan(&b.Spent, &b.SharedIn, &b.SharedOut)
	if err != nil {
		return b, err
	}
	b.Remaining = allowance - b.Spent + b.SharedIn - b.SharedOut
	return b, nil
}
