package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cursor shows a plan's included usage on its dashboard and in `cursor-agent`'s /usage, and
// nowhere a program can read. The dashboard's own data comes from
// GET https://cursor.com/api/usage-summary, authenticated by the WorkosCursorSessionToken
// cookie — an endpoint Cursor does not document. So this collector is opt-in, labelled
// undocumented everywhere it appears, and never goes looking for the cookie: Cursor keeps it
// in its own credential stores, and Conductor does not read another tool's credentials. The
// user hands it over (CONDUCTOR_QUOTA_CURSOR_COOKIE, or ~/.conductor/quota/cursor-cookie),
// and it is sent to cursor.com and nowhere else.

// CursorSummaryURL is the endpoint the Cursor dashboard reads.
const CursorSummaryURL = "https://cursor.com/api/usage-summary"

// cursorSummary is the subset of the usage-summary response this package reads.
type cursorSummary struct {
	BillingCycleEnd string `json:"billingCycleEnd"`
	MembershipType  string `json:"membershipType"`
	IsUnlimited     bool   `json:"isUnlimited"`
	IndividualUsage *struct {
		Plan *struct {
			Enabled          *bool    `json:"enabled"`
			Used             *float64 `json:"used"`
			Limit            *float64 `json:"limit"`
			TotalPercentUsed *float64 `json:"totalPercentUsed"`
			APIPercentUsed   *float64 `json:"apiPercentUsed"`
		} `json:"plan"`
	} `json:"individualUsage"`
}

// ErrCursorShape means the endpoint answered with something this build does not recognise —
// the expected failure mode of an undocumented API.
var ErrCursorShape = errors.New("cursor usage-summary: unrecognised response")

// ParseCursorSummary turns a usage-summary response into the plan's monthly window.
func ParseCursorSummary(body []byte, account string, now time.Time) ([]Snapshot, error) {
	var s cursorSummary
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCursorShape, err)
	}
	if s.IsUnlimited {
		return nil, nil
	}
	if s.IndividualUsage == nil || s.IndividualUsage.Plan == nil {
		return nil, ErrCursorShape
	}
	p := s.IndividualUsage.Plan
	if p.Enabled != nil && !*p.Enabled {
		return nil, nil
	}
	snap := Snapshot{
		Harness: "cursor", Account: account, Window: WindowMonthly, Plan: s.MembershipType,
		Source: "cursor-usage-summary", SourceKind: KindUndocumented, ObservedAt: now.UTC(),
	}
	switch {
	case p.TotalPercentUsed != nil:
		snap.UsedPercent = Float(*p.TotalPercentUsed)
	case p.APIPercentUsed != nil:
		snap.UsedPercent = Float(*p.APIPercentUsed)
	}
	if p.Used != nil && p.Limit != nil && *p.Limit > 0 {
		snap.Used, snap.Limit, snap.Unit = p.Used, p.Limit, "usd_cents"
	}
	if snap.UsedPercent == nil && snap.Used == nil {
		return nil, ErrCursorShape
	}
	if t, err := time.Parse(time.RFC3339, s.BillingCycleEnd); err == nil {
		snap.ResetsAt = Time(t)
	}
	return []Snapshot{snap}, nil
}

// CursorCookie returns the session cookie the user supplied, or "" when Cursor collection is
// not opted into.
func CursorCookie(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv("CONDUCTOR_QUOTA_CURSOR_COOKIE")); v != "" {
		return v
	}
	dir, err := Dir()
	if err != nil {
		return ""
	}
	body, err := os.ReadFile(filepath.Join(dir, "cursor-cookie"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// FetchCursor asks the usage-summary endpoint once. url is CursorSummaryURL outside tests.
func FetchCursor(ctx context.Context, hc *http.Client, url, cookie, account string, now time.Time) ([]Snapshot, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// The cookie is the user's own and goes only to the host it was issued by.
	req.Header.Set("Cookie", "WorkosCursorSessionToken="+strings.TrimPrefix(cookie, "WorkosCursorSessionToken="))
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cursor usage-summary: HTTP %d", resp.StatusCode)
	}
	return ParseCursorSummary(body, account, now)
}
