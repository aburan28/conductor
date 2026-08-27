package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/usage"
)

// ---------------------------------------------------------------------------
// usage
// ---------------------------------------------------------------------------
//
// Token usage is what every harness measures and nobody can see across the team. Each tool
// keeps its own log — Claude Code a transcript with a usage block per message, Codex a
// rollout with a running total, OpenCode a database with tokens on every message — and each
// answers only for itself, on one machine. `conductor usage` is the view across all of them:
// a wrap sidecar reports what its harness logged while it ran, `conductor usage sync` does
// the same after the fact for sessions that were not wrapped, and the report reads it back
// by day, harness, model, or person. Only numbers travel; the words stay in the logs.

func cmdUsage(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "sync" {
		return usageSync(ctx, args[1:])
	}
	return usageReport(ctx, args)
}

func usageReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	since := fs.String("since", "7d", "start of the window: 7d, 36h, 2026-08-01, or an RFC 3339 time")
	until := fs.String("until", "", "end of the window (default: now)")
	by := fs.String("by", "harness", "dimensions to group by, comma-separated: day, hour, harness, model, effort, principal, source, session")
	harness := fs.String("harness", "", "only this harness")
	model := fs.String("model", "", "only this model")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor usage — how many tokens the team's agents used, and on what

Reads the usage ledger the wrap sidecars, `+"`conductor usage sync`"+`, and runner attempts
feed. Team totals by day, harness, and model are visible to every member; per-session
detail is your own unless you maintain the project; other people's model names follow the
project's publishModelIdentity setting.

  conductor usage                          last 7 days, by harness
  conductor usage --by day,harness         a daily series per harness
  conductor usage --by model --since 30d   which models carry the load
  conductor usage --by principal           who used what
  conductor usage sync                     report this machine's unwrapped sessions

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	var report coord.UsageReport
	if err := api.Get(ctx, "/v1/projects/"+ref+"/usage"+client.Query(
		"since", *since, "until", *until, "by", *by, "harness", *harness, "model", *model),
		&report); err != nil {
		return err
	}
	if *asJSON {
		return emit(report)
	}
	printUsageReport(report)
	return nil
}

// printUsageReport renders the report as a table whose leading columns are the dimensions
// asked for, in the order the API lists them.
func printUsageReport(r coord.UsageReport) {
	fmt.Printf("%s — %s to %s\n\n", r.Project,
		r.Since.Local().Format("2006-01-02 15:04"), r.Until.Local().Format("2006-01-02 15:04"))
	if len(r.Rows) == 0 {
		fmt.Println("No usage recorded in this window. Sessions launched through `conductor wrap` report")
		fmt.Println("automatically; `conductor usage sync` reports the ones that were not.")
		return
	}
	type column struct {
		title string
		get   func(domain.UsageRow) string
	}
	var dims []column
	for _, d := range r.By {
		switch d {
		case "day":
			dims = append(dims, column{"DAY", func(x domain.UsageRow) string { return periodString(x.Period, "2006-01-02") }})
		case "hour":
			dims = append(dims, column{"HOUR", func(x domain.UsageRow) string { return periodString(x.Period, "2006-01-02 15:00") }})
		case "principal":
			dims = append(dims, column{"WHO", func(x domain.UsageRow) string { return x.Principal }})
		case "harness":
			dims = append(dims, column{"HARNESS", func(x domain.UsageRow) string { return x.Harness }})
		case "model":
			dims = append(dims, column{"MODEL", func(x domain.UsageRow) string {
				if x.Redacted && x.Model == "" {
					return "(undisclosed)"
				}
				return x.Model
			}})
		case "effort":
			dims = append(dims, column{"EFFORT", func(x domain.UsageRow) string { return x.Effort }})
		case "source":
			dims = append(dims, column{"SOURCE", func(x domain.UsageRow) string { return x.Source }})
		case "session":
			dims = append(dims, column{"SESSION", func(x domain.UsageRow) string { return shortID(x.ExternalSessionID) }})
		}
	}
	widths := make([]int, len(dims))
	for i, c := range dims {
		widths[i] = len(c.title)
		for _, row := range r.Rows {
			if n := len(c.get(row)); n > widths[i] {
				widths[i] = n
			}
		}
	}
	line := func(cells []string, nums domain.UsageRow) {
		fmt.Print("  ")
		for i, cell := range cells {
			fmt.Printf("%-*s  ", widths[i], cell)
		}
		fmt.Printf("%8s %10s %10s %9s %10s %10s %9s\n",
			humanTokens(nums.Requests), humanTokens(nums.InputTokens), humanTokens(nums.CacheReadTokens),
			humanTokens(nums.CacheWriteTokens), humanTokens(nums.OutputTokens), humanTokens(nums.TotalTokens),
			dollars(nums.CostUSD))
	}
	titles := make([]string, len(dims))
	for i, c := range dims {
		titles[i] = c.title
	}
	fmt.Print("  ")
	for i, t := range titles {
		fmt.Printf("%-*s  ", widths[i], t)
	}
	fmt.Printf("%8s %10s %10s %9s %10s %10s %9s\n", "CALLS", "INPUT", "CACHE-RD", "CACHE-WR", "OUTPUT", "TOTAL", "COST")
	for _, row := range r.Rows {
		cells := make([]string, len(dims))
		for i, c := range dims {
			cells[i] = orDash(c.get(row))
		}
		line(cells, row)
	}
	totalCells := make([]string, len(dims))
	if len(dims) > 0 {
		totalCells[0] = "total"
	}
	fmt.Println()
	line(totalCells, r.Total)
	fmt.Println("\n  Cost is what the harness reported, or the catalog's list price where it did not (cache reads at a tenth).")
}

func periodString(t *time.Time, layout string) string {
	if t == nil {
		return ""
	}
	return t.Local().Format(layout)
}

func dollars(v float64) string {
	if v == 0 {
		return "—"
	}
	if v < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

// ---------------------------------------------------------------------------
// usage sync — report sessions that were not wrapped
// ---------------------------------------------------------------------------

func usageSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("usage sync", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	since := fs.String("since", "7d", "how far back to read the harness logs: 7d, 36h, 2026-08-01")
	harness := fs.String("harness", "", "only this harness (claude, codex, opencode)")
	dryRun := fs.Bool("dry-run", false, "show what would be reported without sending it")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor usage sync — report this machine's unwrapped sessions

Reads the usage logs Claude Code, Codex, and OpenCode keep for the current directory and
reports hourly token totals to the project. Sessions launched through `+"`conductor wrap`"+`
already report as they run; this is for the ones that were not. Re-running is safe: the
ledger replaces rather than adds. Nothing but numbers, model names, and timestamps leaves
the machine — the transcripts are read for their usage blocks and discarded.

  conductor usage sync
  conductor usage sync --since 30d --harness codex

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	from, err := coord.ParseUsageWindow(*since, time.Now().UTC())
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	harnesses := []string{"claude", "codex", "opencode"}
	if *harness != "" {
		harnesses = []string{*harness}
	}
	var buckets []usage.Bucket
	for _, h := range harnesses {
		turns, err := usage.Read(ctx, h, cwd, from, os.Getenv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "conductor: %s: %v\n", h, err)
			continue
		}
		buckets = append(buckets, usage.Aggregate(turns)...)
	}

	if *dryRun {
		if *asJSON {
			return emit(buckets)
		}
		printBuckets(buckets)
		return nil
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	var out struct {
		Recorded int `json:"recorded"`
	}
	if len(buckets) > 0 {
		if err := api.Post(ctx, "/v1/projects/"+ref+"/usage", map[string]any{"buckets": buckets}, &out); err != nil {
			return err
		}
	}
	if *asJSON {
		return emit(map[string]any{"recorded": out.Recorded, "buckets": buckets})
	}
	printBuckets(buckets)
	fmt.Printf("\nReported %d hourly bucket(s) since %s to %s.\n", out.Recorded, from.Local().Format("2006-01-02 15:04"), ref)
	return nil
}

func printBuckets(buckets []usage.Bucket) {
	if len(buckets) == 0 {
		fmt.Println("No harness usage found for this directory in the window.")
		return
	}
	fmt.Printf("  %-16s %-10s %-9s %-28s %8s %10s %10s %9s\n", "HOUR", "HARNESS", "SESSION", "MODEL", "CALLS", "INPUT", "OUTPUT", "COST")
	for _, b := range buckets {
		fmt.Printf("  %-16s %-10s %-9s %-28s %8s %10s %10s %9s\n",
			b.Start.Local().Format("2006-01-02 15:00"), b.Harness, shortID(b.ExternalSessionID), b.Model,
			humanTokens(b.Requests), humanTokens(b.Input+b.CacheRead+b.CacheWrite), humanTokens(b.Output), dollars(b.CostUSD))
	}
}

// ---------------------------------------------------------------------------
// the wrap sidecar's collector
// ---------------------------------------------------------------------------

// usageCollector reports what a wrapped harness logs while it runs. It re-reads the
// harness's own usage log on each tick and sends only the hourly buckets that changed; a
// final flush at exit catches the tail. Failures are silent — a usage report is never a
// reason to disturb the session it describes — but the last error is kept for the exit line.
type usageCollector struct {
	api       *client.Client
	sessionID domain.ID
	harness   string
	cwd       string
	since     time.Time
	getenv    func(string) string
	read      func(ctx context.Context, harness, cwd string, since time.Time, getenv func(string) string) ([]usage.Turn, error)

	prev    []usage.Bucket
	sent    int
	lastErr error
}

func newUsageCollector(api *client.Client, sessionID domain.ID, harness, cwd string) *usageCollector {
	return &usageCollector{
		api: api, sessionID: sessionID, harness: harness, cwd: cwd,
		since:  time.Now().Add(-time.Minute), // a transcript's first write can predate our clock read
		getenv: os.Getenv, read: usage.Read,
	}
}

// usageDisabled reports whether the operator turned collection off (CONDUCTOR_USAGE=off).
func usageDisabled(getenv func(string) string) bool {
	switch strings.ToLower(getenv("CONDUCTOR_USAGE")) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

func (c *usageCollector) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.flush(ctx)
		}
	}
}

// flush reads the log and reports what moved. It returns how many buckets were sent.
func (c *usageCollector) flush(ctx context.Context) int {
	turns, err := c.read(ctx, c.harness, c.cwd, c.since, c.getenv)
	if err != nil {
		c.lastErr = err
		return 0
	}
	cur := usage.Aggregate(turns)
	changed := usage.Changed(c.prev, cur)
	if len(changed) == 0 {
		return 0
	}
	if err := c.api.Post(ctx, "/v1/sessions/"+c.sessionID+"/usage", map[string]any{"buckets": changed}, nil); err != nil {
		c.lastErr = err
		return 0 // keep prev as it was: the next flush re-sends what did not land
	}
	c.prev = cur
	c.sent += len(changed)
	return len(changed)
}

// totals sums what the collector last saw, for the exit line.
func (c *usageCollector) totals() (calls, tokens int64, cost float64) {
	for _, b := range c.prev {
		calls += b.Requests
		tokens += b.Total()
		cost += b.CostUSD
	}
	return calls, tokens, cost
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var _ = errors.New // keep the import stable for the small helpers above

// shortID abbreviates a session or record id for a table cell.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
