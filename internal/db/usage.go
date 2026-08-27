package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// Usage ledger (DESIGN.md §26.1)
// ---------------------------------------------------------------------------

// UpsertUsageBuckets records hourly usage rows, replacing counters for rows that already
// exist. Absolute values plus replace-on-conflict is what makes a collector safe to restart:
// it re-reads the log from the top, sends the same buckets, and the ledger does not move.
func (s *Store) UpsertUsageBuckets(ctx context.Context, buckets []domain.UsageBucket) (int, error) {
	n := 0
	for _, b := range buckets {
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO usage_buckets (project_id, principal_id, session_id, attempt_id, source,
			        harness, model, provider, reasoning_effort, external_session_id, bucket_start,
			        requests, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
			        reasoning_tokens, cost_usd, cost_source)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10, $11,
			        $12, $13, $14, $15, $16, $17, $18, $19)
			ON CONFLICT (project_id, principal_id, harness, external_session_id, model, provider,
			             reasoning_effort, bucket_start)
			DO UPDATE SET
			    session_id         = COALESCE(EXCLUDED.session_id, usage_buckets.session_id),
			    attempt_id         = COALESCE(EXCLUDED.attempt_id, usage_buckets.attempt_id),
			    source             = EXCLUDED.source,
			    requests           = EXCLUDED.requests,
			    input_tokens       = EXCLUDED.input_tokens,
			    cache_read_tokens  = EXCLUDED.cache_read_tokens,
			    cache_write_tokens = EXCLUDED.cache_write_tokens,
			    output_tokens      = EXCLUDED.output_tokens,
			    reasoning_tokens   = EXCLUDED.reasoning_tokens,
			    cost_usd           = EXCLUDED.cost_usd,
			    cost_source        = EXCLUDED.cost_source,
			    updated_at         = now()`,
			b.ProjectID, b.PrincipalID, nullable(b.SessionID), nullable(b.AttemptID), b.Source,
			b.Harness, b.Model, b.Provider, b.Effort, b.ExternalSessionID, b.BucketStart.UTC(),
			b.Requests, b.InputTokens, b.CacheReadTokens, b.CacheWriteTokens, b.OutputTokens,
			b.ReasoningTokens, b.CostUSD, b.CostSource)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// UsageQuery selects and groups the ledger. By names the dimensions to keep: "day", "hour",
// "harness", "model", "effort", "principal", "source", "session". Rows are always grouped by
// principal underneath, so the caller can apply per-viewer rules before collapsing.
type UsageQuery struct {
	ProjectID   domain.ID
	PrincipalID domain.ID // restrict to one principal; empty for everyone
	Since       time.Time
	Until       time.Time
	Harness     string
	Model       string
	By          []string
}

// UsageDimensions is every grouping the ledger supports, in the order rows sort by.
var UsageDimensions = []string{"day", "hour", "principal", "harness", "model", "effort", "source", "session"}

func validDimension(d string) bool {
	for _, known := range UsageDimensions {
		if d == known {
			return true
		}
	}
	return false
}

// QueryUsage aggregates the ledger over a window.
func (s *Store) QueryUsage(ctx context.Context, q UsageQuery) ([]domain.UsageRow, error) {
	want := map[string]bool{"principal": true}
	for _, d := range q.By {
		if !validDimension(d) {
			return nil, fmt.Errorf("%w: unknown usage dimension %q (want one of %s)",
				domain.ErrInvalidArgument, d, strings.Join(UsageDimensions, ", "))
		}
		want[d] = true
	}
	if want["day"] && want["hour"] {
		return nil, fmt.Errorf("%w: group by day or by hour, not both", domain.ErrInvalidArgument)
	}

	// Every dimension is selected — as itself when grouped, as a constant otherwise — so the
	// scan below is fixed regardless of the grouping asked for.
	col := func(name, expr string) string {
		if want[name] {
			return expr
		}
		switch name {
		case "day", "hour":
			return "NULL::timestamptz"
		default:
			return "''"
		}
	}
	period := "NULL::timestamptz"
	switch {
	case want["day"]:
		period = "date_trunc('day', bucket_start)"
	case want["hour"]:
		period = "date_trunc('hour', bucket_start)"
	}
	selects := []string{
		period,
		"principal_id::text",
		col("harness", "harness"),
		col("model", "model"),
		col("model", "provider"),
		col("effort", "reasoning_effort"),
		col("source", "source"),
		col("session", "external_session_id"),
		col("session", "COALESCE(session_id::text, '')"),
	}
	groups := []string{}
	for i, sel := range selects {
		if sel != "NULL::timestamptz" && sel != "''" {
			groups = append(groups, fmt.Sprint(i+1))
		}
	}

	args := []any{q.ProjectID}
	where := []string{"project_id = $1::uuid"}
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if !q.Since.IsZero() {
		add("bucket_start >= $%d", q.Since.UTC())
	}
	if !q.Until.IsZero() {
		add("bucket_start < $%d", q.Until.UTC())
	}
	if q.PrincipalID != "" {
		add("principal_id = $%d::uuid", q.PrincipalID)
	}
	if q.Harness != "" {
		add("harness = $%d", q.Harness)
	}
	if q.Model != "" {
		add("model = $%d", q.Model)
	}

	sql := `SELECT ` + strings.Join(selects, ", ") + `,
	               sum(requests)::bigint, sum(input_tokens)::bigint, sum(cache_read_tokens)::bigint,
	               sum(cache_write_tokens)::bigint, sum(output_tokens)::bigint,
	               sum(reasoning_tokens)::bigint, sum(cost_usd)::float8
	          FROM usage_buckets
	         WHERE ` + strings.Join(where, " AND ") + `
	      GROUP BY ` + strings.Join(groups, ", ") + `
	      ORDER BY 1 NULLS FIRST, ` + strings.Join(groups[1:], ", ")
	if len(groups) == 1 {
		sql = strings.TrimSuffix(sql, ", ")
	}

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.UsageRow{}
	for rows.Next() {
		var r domain.UsageRow
		var period *time.Time
		var cost float64
		if err := rows.Scan(&period, &r.PrincipalID, &r.Harness, &r.Model, &r.Provider, &r.Effort,
			&r.Source, &r.ExternalSessionID, &r.SessionID,
			&r.Requests, &r.InputTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.OutputTokens, &r.ReasoningTokens, &cost); err != nil {
			return nil, err
		}
		r.Period = period
		r.CostUSD = cost
		r.TotalTokens = r.InputTokens + r.CacheReadTokens + r.CacheWriteTokens + r.OutputTokens
		out = append(out, r)
	}
	return out, rows.Err()
}
