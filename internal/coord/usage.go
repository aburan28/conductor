package coord

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/usage"
)

// Token usage (DESIGN.md §26.1). Three producers feed one ledger: a `conductor wrap` sidecar
// reading its harness's own usage log, `conductor usage sync` doing the same for bare
// sessions after the fact, and runner attempts reporting progress. One reader serves them
// all back, grouped however the question is asked — by day, harness, model, person.

// UsageSourceSession and friends name where a ledger row came from.
const (
	UsageSourceSession = "session"
	UsageSourceSync    = "sync"
	UsageSourceAttempt = "attempt"
)

// RecordSessionUsage stores buckets a wrap sidecar collected for its own session. Only the
// session's principal may report for it: usage is charged to a person, and nobody gets to
// charge someone else.
func (s *Service) RecordSessionUsage(ctx context.Context, c Caller, session domain.Session, buckets []usage.Bucket) (int, error) {
	if session.PrincipalID != c.Principal.ID {
		return 0, domain.ErrNotPermitted
	}
	return s.recordUsage(ctx, c, session.ProjectID, session.ID, UsageSourceSession, buckets)
}

// RecordSyncedUsage stores buckets a principal collected from their own machine for
// sessions that were not wrapped.
func (s *Service) RecordSyncedUsage(ctx context.Context, c Caller, projectID domain.ID, buckets []usage.Bucket) (int, error) {
	return s.recordUsage(ctx, c, projectID, "", UsageSourceSync, buckets)
}

func (s *Service) recordUsage(ctx context.Context, c Caller, projectID, sessionID domain.ID, source string, buckets []usage.Bucket) (int, error) {
	if len(buckets) == 0 {
		return 0, nil
	}
	rows := make([]domain.UsageBucket, 0, len(buckets))
	for _, b := range buckets {
		if b.Harness == "" || b.ExternalSessionID == "" || b.Start.IsZero() {
			return 0, fmt.Errorf("%w: a usage bucket needs a harness, a session id, and a start", domain.ErrInvalidArgument)
		}
		if b.Input < 0 || b.Output < 0 || b.CacheRead < 0 || b.CacheWrite < 0 || b.Reasoning < 0 || b.CostUSD < 0 {
			return 0, fmt.Errorf("%w: usage counters cannot be negative", domain.ErrInvalidArgument)
		}
		row := domain.UsageBucket{
			ProjectID: projectID, PrincipalID: c.Principal.ID, SessionID: sessionID, Source: source,
			Harness: b.Harness, Model: b.Model, Provider: b.Provider, Effort: b.Effort,
			ExternalSessionID: b.ExternalSessionID, BucketStart: b.Start.UTC().Truncate(time.Hour),
			Requests: b.Requests, InputTokens: b.Input, CacheReadTokens: b.CacheRead,
			CacheWriteTokens: b.CacheWrite, OutputTokens: b.Output, ReasoningTokens: b.Reasoning,
			CostUSD: b.CostUSD,
		}
		if row.CostUSD > 0 {
			row.CostSource = "reported"
		}
		rows = append(rows, row)
	}
	if err := s.priceUsage(ctx, c.Principal.OrganizationID, rows); err != nil {
		return 0, err
	}
	return s.Store.UpsertUsageBuckets(ctx, rows)
}

// recordAttemptUsage mirrors an attempt's running totals into the ledger, so runner work and
// interactive work answer to the same report. The attempt is its own "session": totals are
// cumulative, so the whole attempt is one bucket at the hour it started.
func (s *Service) recordAttemptUsage(ctx context.Context, c Caller, a domain.Attempt) error {
	if a.TokensIn+a.TokensOut == 0 && a.CostUSD == 0 {
		return nil
	}
	start := time.Now().UTC()
	if a.StartedAt != nil {
		start = a.StartedAt.UTC()
	}
	principal := a.SponsorPrincipal
	if principal == "" {
		principal = c.Principal.ID
	}
	row := domain.UsageBucket{
		ProjectID: a.ProjectID, PrincipalID: principal, AttemptID: a.ID, Source: UsageSourceAttempt,
		Harness: a.Harness, Model: a.ResolvedModel, Effort: string(a.ReasoningEffort),
		ExternalSessionID: "attempt:" + a.ID, BucketStart: start.Truncate(time.Hour),
		Requests: int64(a.Turns), InputTokens: a.TokensIn, OutputTokens: a.TokensOut, CostUSD: a.CostUSD,
	}
	if row.Model == "" {
		row.Model = a.ModelAlias
	}
	if row.CostUSD > 0 {
		row.CostSource = "reported"
	}
	if err := s.priceUsage(ctx, c.Principal.OrganizationID, []domain.UsageBucket{row}); err != nil {
		return err
	}
	_, err := s.Store.UpsertUsageBuckets(ctx, []domain.UsageBucket{row})
	return err
}

// priceUsage estimates a cost for rows the harness did not price, from the organization's
// model catalog. It is an estimate and says so (cost_source = catalog): list price per token,
// with cache reads at a tenth of the input price, which is what the major providers charge.
// A model the catalog does not know stays at zero rather than being guessed.
func (s *Service) priceUsage(ctx context.Context, orgID domain.ID, rows []domain.UsageBucket) error {
	needs := false
	for _, r := range rows {
		if r.CostUSD == 0 && r.Model != "" {
			needs = true
			break
		}
	}
	if !needs {
		return nil
	}
	profiles, err := s.Store.ListModelProfiles(ctx, orgID)
	if err != nil {
		return err
	}
	byModel := map[string]domain.ModelProfile{}
	for _, p := range profiles {
		if !p.Enabled || (p.InputCostPerMTok == nil && p.OutputCostPerMTok == nil) {
			continue
		}
		byModel[strings.ToLower(p.Model)] = p
		if p.Alias != "" {
			byModel[strings.ToLower(p.Alias)] = p
		}
	}
	for i := range rows {
		r := &rows[i]
		if r.CostUSD != 0 || r.Model == "" {
			continue
		}
		p, ok := byModel[strings.ToLower(r.Model)]
		if !ok {
			continue
		}
		in := float64(r.InputTokens+r.CacheWriteTokens) + 0.1*float64(r.CacheReadTokens)
		r.CostUSD = in*deref(p.InputCostPerMTok)/1e6 + float64(r.OutputTokens)*deref(p.OutputCostPerMTok)/1e6
		if r.CostUSD > 0 {
			r.CostSource = "catalog"
		}
	}
	return nil
}

// UsageReport is what `conductor usage` and the dashboard render.
type UsageReport struct {
	Project string            `json:"project"`
	Since   time.Time         `json:"since"`
	Until   time.Time         `json:"until"`
	By      []string          `json:"by"`
	Rows    []domain.UsageRow `json:"rows"`
	Total   domain.UsageRow   `json:"total"`
}

// Usage answers "how much did we use" for a project, grouped by the dimensions asked for.
//
// Team-level totals by day, harness, and model are coordination state: they are what a team
// needs to notice that one harness burns three times the tokens of another for the same work.
// Two things are narrower. Session-level detail is only ever the viewer's own unless they
// maintain the project — a per-session token curve is close enough to a transcript's shape
// to be a side channel. And the concrete model name follows publishModelIdentity, exactly as
// it does in presence: a project that does not broadcast which vendor each person pays for
// still sees its totals, with other people's models folded into one redacted row.
func (s *Service) Usage(ctx context.Context, c Caller, projectID domain.ID, q db.UsageQuery) (UsageReport, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return UsageReport{}, err
	}
	q.ProjectID = projectID
	if q.Until.IsZero() {
		q.Until = time.Now().UTC()
	}
	if q.Since.IsZero() {
		q.Since = q.Until.Add(-7 * 24 * time.Hour)
	}
	if !q.Since.Before(q.Until) {
		return UsageReport{}, fmt.Errorf("%w: the window is empty", domain.ErrInvalidArgument)
	}
	bySession := hasDimension(q.By, "session")
	if bySession && !c.Role.Can(domain.RoleMaintainer) {
		q.PrincipalID = c.Principal.ID
	}
	rows, err := s.Store.QueryUsage(ctx, q)
	if err != nil {
		return UsageReport{}, err
	}

	ids := make([]domain.ID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.PrincipalID)
	}
	principals := map[domain.ID]domain.Principal{}
	if len(ids) > 0 {
		if principals, err = s.Store.PrincipalsByID(ctx, ids); err != nil {
			return UsageReport{}, err
		}
	}

	byPrincipal := hasDimension(q.By, "principal")
	byModel := hasDimension(q.By, "model")
	merged := map[string]*domain.UsageRow{}
	var order []string
	for _, r := range rows {
		self := r.PrincipalID == c.Principal.ID
		if byModel && !self && !project.Config.PublishModelIdentity && r.Model != "" {
			r.Model, r.Provider, r.Redacted = "", "", true
		}
		if byPrincipal {
			r.Principal = principals[r.PrincipalID].Handle
		} else {
			r.PrincipalID = ""
		}
		k := usageKey(r)
		if m, ok := merged[k]; ok {
			m.Requests += r.Requests
			m.InputTokens += r.InputTokens
			m.CacheReadTokens += r.CacheReadTokens
			m.CacheWriteTokens += r.CacheWriteTokens
			m.OutputTokens += r.OutputTokens
			m.ReasoningTokens += r.ReasoningTokens
			m.TotalTokens += r.TotalTokens
			m.CostUSD += r.CostUSD
			m.Redacted = m.Redacted || r.Redacted
			continue
		}
		row := r
		merged[k] = &row
		order = append(order, k)
	}

	report := UsageReport{Project: project.Slug, Since: q.Since, Until: q.Until, By: q.By,
		Rows: make([]domain.UsageRow, 0, len(order))}
	for _, k := range order {
		r := *merged[k]
		report.Rows = append(report.Rows, r)
		report.Total.Requests += r.Requests
		report.Total.InputTokens += r.InputTokens
		report.Total.CacheReadTokens += r.CacheReadTokens
		report.Total.CacheWriteTokens += r.CacheWriteTokens
		report.Total.OutputTokens += r.OutputTokens
		report.Total.ReasoningTokens += r.ReasoningTokens
		report.Total.TotalTokens += r.TotalTokens
		report.Total.CostUSD += r.CostUSD
	}
	sort.SliceStable(report.Rows, func(i, j int) bool {
		a, b := report.Rows[i], report.Rows[j]
		switch {
		case a.Period == nil && b.Period != nil:
			return true
		case a.Period != nil && b.Period == nil:
			return false
		case a.Period != nil && !a.Period.Equal(*b.Period):
			return a.Period.Before(*b.Period)
		}
		return a.TotalTokens > b.TotalTokens
	})
	if report.By == nil {
		report.By = []string{}
	}
	return report, nil
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func usageKey(r domain.UsageRow) string {
	period := ""
	if r.Period != nil {
		period = r.Period.UTC().Format(time.RFC3339)
	}
	return strings.Join([]string{period, string(r.PrincipalID), r.Harness, r.Model, r.Provider,
		r.Effort, r.Source, r.ExternalSessionID, string(r.SessionID)}, "\x00")
}

func hasDimension(by []string, d string) bool {
	for _, b := range by {
		if b == d {
			return true
		}
	}
	return false
}

// ParseUsageWindow reads the since/until forms the CLI and API accept: RFC 3339 timestamps,
// Go durations ("36h"), or day counts ("7d"), the latter two meaning "that long ago".
func ParseUsageWindow(v string, now time.Time) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), nil
	}
	if strings.HasSuffix(v, "d") {
		var days int
		if _, err := fmt.Sscanf(v, "%dd", &days); err == nil && days > 0 {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, errors.Join(domain.ErrInvalidArgument,
		fmt.Errorf("cannot read %q as a time: use 2026-08-01, an RFC 3339 timestamp, 36h, or 7d", v))
}
