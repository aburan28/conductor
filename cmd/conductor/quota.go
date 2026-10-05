package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/checkpoint"
	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/quota"
	"github.com/adamburan/conductor/internal/usage"
)

// ---------------------------------------------------------------------------
// quota
// ---------------------------------------------------------------------------
//
// A subscription login runs out mid-task, and the tool says so only when it already has.
// `conductor quota` is the view across every login and tool on the machine — and, through
// the control plane, across the owner's other machines — of how much of each window is
// left, read from what each tool exposes locally (docs/USAGE_LIMITS.md). It pairs with
// checkpoints: `conductor quota suggest` names the login with the most headroom and the
// exact resume command for it.

func cmdQuota(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "statusline":
			return quotaStatusline(ctx, args[1:])
		case "report":
			return quotaReport(ctx, args[1:])
		case "suggest":
			return quotaSuggest(ctx, args[1:])
		}
	}
	return quotaShow(ctx, args)
}

func quotaShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quota", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug (for the team count and events)")
	local := fs.Bool("local", false, "read this machine only; send nothing to the control plane")
	watch := fs.Bool("watch", false, "refresh until interrupted")
	interval := fs.Duration("interval", 30*time.Second, "refresh interval with --watch")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor quota — how close each login is to its usage limit

Reads what Claude Code, Codex, and (opt-in) Cursor expose on this machine about their
rolling usage windows, reports the readings to the control plane under your name, and
merges in your other machines. Only you see your logins; teammates see a count.

  conductor quota                          every login and window
  conductor quota --watch                  keep it on screen
  conductor quota suggest                  the resume command with the most headroom
  conductor quota statusline install       record Claude Code's documented rate limits
  conductor quota report --harness gemini --window daily --used 640 --limit 1000

Sources and what each can be trusted for: docs/USAGE_LIMITS.md.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	for {
		view := collectQuotaView(ctx, *project, *local)
		if *asJSON {
			if err := emit(view); err != nil {
				return err
			}
		} else {
			if *watch {
				fmt.Print("\033[H\033[2J")
			}
			printQuotaView(view)
		}
		if !*watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

// quotaCLIView is what `conductor quota` shows and --json emits.
type quotaCLIView struct {
	Thresholds quota.Thresholds        `json:"thresholds"`
	Machine    string                  `json:"machine"`
	Snapshots  []quota.Row             `json:"snapshots"`
	Collectors []quota.CollectorStatus `json:"collectors"`
	Team       *quota.TeamView         `json:"team,omitempty"`
	Reported   bool                    `json:"reported"`
	Error      string                  `json:"error,omitempty"`
}

// collectQuotaView reads this machine, reports it, and merges the owner's other machines.
// Network trouble degrades to the local view with the reason attached.
func collectQuotaView(ctx context.Context, projectFlag string, local bool) quotaCLIView {
	repoRoot, _ := config.FindRoot(".")
	th := quota.LoadThresholds(repoRoot, os.Getenv)
	res := quota.Collect(ctx, quota.Options{SkipNetwork: local})
	host := quotaMachine()
	view := quotaCLIView{Thresholds: th, Machine: host, Collectors: res.Collectors}
	snaps := res.Snapshots

	creds := client.LoadCredentials()
	if !local && creds.Token != "" {
		apiClient := client.New(creds.Endpoint, creds.Token)
		ref, _ := projectRef(projectFlag, creds)
		if err := postQuota(ctx, apiClient, ref, snaps, th); err != nil {
			view.Error = err.Error()
		} else {
			view.Reported = true
		}
		var mine quota.View
		if err := apiClient.Get(ctx, "/v1/quota", &mine); err == nil {
			for _, row := range mine.Snapshots {
				if row.Machine != host {
					snaps = append(snaps, row.Snapshot)
				}
			}
		}
		if ref != "" {
			var team quota.TeamView
			if err := apiClient.Get(ctx, "/v1/projects/"+ref+"/quota", &team); err == nil {
				team.Mine = quota.View{}
				view.Team = &team
			}
		}
	}
	now := time.Now()
	for _, s := range quota.Latest(snaps) {
		view.Snapshots = append(view.Snapshots, quota.Row{Snapshot: s, Level: th.Level(s, now)})
	}
	return view
}

// quotaMachine is the host name, cut to the label the API accepts.
func quotaMachine() string {
	host, _ := os.Hostname()
	host = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '-'
	}, host)
	host = strings.TrimLeft(host, ".-_")
	if len(host) > 64 {
		host = host[:64]
	}
	return host
}

// postQuota reports readings. ref names the project whose stream hears about crossed
// levels; it may be empty.
func postQuota(ctx context.Context, apiClient *client.Client, ref string, snaps []quota.Snapshot, th quota.Thresholds) error {
	if len(snaps) == 0 {
		return nil
	}
	host := quotaMachine()
	out := make([]quota.Snapshot, len(snaps))
	for i, s := range snaps {
		if s.Machine == "" || len(s.Machine) > 64 {
			s.Machine = host
		}
		out[i] = s
	}
	return apiClient.Post(ctx, "/v1/quota", map[string]any{
		"snapshots": out, "thresholds": th, "project": ref,
	}, nil)
}

func printQuotaView(v quotaCLIView) {
	now := time.Now()
	if len(v.Snapshots) == 0 {
		fmt.Println("No usage-limit readings yet.")
		fmt.Println("  Claude Code (Pro/Max): `conductor quota statusline install`, then use Claude as usual.")
		fmt.Println("  Codex (ChatGPT login): readings appear after the first response of a session.")
		fmt.Println("  Others: docs/USAGE_LIMITS.md, or `conductor quota report`.")
	} else {
		machines := map[string]bool{}
		for _, r := range v.Snapshots {
			machines[r.Machine] = true
		}
		multi := len(machines) > 1
		header := []string{"TOOL", "ACCOUNT"}
		if multi {
			header = append(header, "MACHINE")
		}
		header = append(header, "WINDOW", "USED", "RESETS IN", "SOURCE", "AGE", "")
		rows := [][]string{header}
		for _, r := range v.Snapshots {
			row := []string{r.Harness, r.Account}
			if multi {
				row = append(row, r.Machine)
			}
			used := "?"
			if pct, ok := r.Percent(now); ok {
				used = quota.FormatPercent(pct)
			}
			resets := "—"
			switch {
			case r.Reset(now):
				resets = "reset"
			case r.ResetsAt != nil:
				resets = quota.FormatIn(*r.ResetsAt, now)
			}
			source := r.Source
			if r.SourceKind == quota.KindUndocumented {
				source += " (undocumented)"
			}
			row = append(row, r.Window, used, resets, source, quotaAge(r.ObservedAt, now), quotaLevelMark(r.Level))
			rows = append(rows, row)
		}
		printColumns(rows)
	}
	fmt.Printf("\n  Warn at %s, act at %s (CONDUCTOR_QUOTA_WARN / _CRITICAL, or quota: in .conductor/project.yaml).\n",
		quota.FormatPercent(v.Thresholds.Warn), quota.FormatPercent(v.Thresholds.Critical))
	if v.Team != nil {
		fmt.Printf("  Team (%s): %d of %d logins reported in the last %dh are near their limit",
			v.Team.Project, v.Team.NearLimit, v.Team.Logins, v.Team.WindowHours)
		if v.Team.Exhausted > 0 {
			fmt.Printf(", %d exhausted", v.Team.Exhausted)
		}
		fmt.Println(".")
	}
	if v.Error != "" {
		fmt.Printf("  Not reported to the control plane: %s\n", v.Error)
	}
}

func quotaLevelMark(l quota.Level) string {
	switch l {
	case quota.LevelWarning:
		return "warning"
	case quota.LevelCritical:
		return "CRITICAL"
	case quota.LevelExhausted:
		return "EXHAUSTED"
	}
	return ""
}

func quotaAge(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

func printColumns(rows [][]string) {
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			if n := len([]rune(c)); n > widths[i] {
				widths[i] = n
			}
		}
	}
	for _, r := range rows {
		var b strings.Builder
		b.WriteString("  ")
		for i, c := range r {
			if i == len(r)-1 {
				b.WriteString(c)
				break
			}
			b.WriteString(c)
			b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(c))+2))
		}
		fmt.Println(strings.TrimRight(b.String(), " "))
	}
}

// ---------------------------------------------------------------------------
// quota report — a reading from somewhere Conductor cannot look
// ---------------------------------------------------------------------------

func quotaReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quota report", flag.ExitOnError)
	harness := fs.String("harness", "", "the tool (gemini, copilot, opencode, …)")
	account := fs.String("account", "default", "a label for the login")
	window := fs.String("window", "", "the window: 5h, daily, weekly, monthly, …")
	minutes := fs.Int("window-minutes", 0, "the window's length in minutes, when not implied by its name")
	usedPct := fs.Float64("used-percent", -1, "share of the window used, 0-100")
	used := fs.Float64("used", -1, "amount used (with --limit)")
	limit := fs.Float64("limit", -1, "the window's allowance (with --used)")
	unit := fs.String("unit", "", "the unit of --used and --limit (requests, usd, …)")
	resets := fs.String("resets-at", "", "when the window resets: an RFC 3339 time, or a duration from now such as 3h")
	reached := fs.Bool("limit-reached", false, "the tool says the limit is hit")
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *harness == "" || *window == "" {
		return errors.New("usage: conductor quota report --harness H --window W (--used-percent P | --used N --limit N) [--resets-at T]")
	}
	now := time.Now().UTC()
	s := quota.Snapshot{
		Harness: *harness, Account: *account, Machine: quotaMachine(), Window: *window, WindowMinutes: *minutes,
		Unit: *unit, LimitReached: *reached, Source: "manual", SourceKind: quota.KindManual, ObservedAt: now,
	}
	if *usedPct >= 0 {
		s.UsedPercent = quota.Float(*usedPct)
	}
	if *used >= 0 && *limit > 0 {
		s.Used, s.Limit = quota.Float(*used), quota.Float(*limit)
	}
	if s.UsedPercent == nil && s.Used == nil && !s.LimitReached {
		return errors.New("give --used-percent, or --used with --limit, or --limit-reached")
	}
	if *resets != "" {
		if d, err := time.ParseDuration(*resets); err == nil {
			s.ResetsAt = quota.Time(now.Add(d))
		} else if t, err := time.Parse(time.RFC3339, *resets); err == nil {
			s.ResetsAt = quota.Time(t)
		} else {
			return fmt.Errorf("--resets-at %q is neither a duration nor an RFC 3339 time", *resets)
		}
	}
	if err := quota.Save([]quota.Snapshot{s}); err != nil {
		return err
	}
	reported := false
	if creds := client.LoadCredentials(); creds.Token != "" {
		ref, _ := projectRef(*project, creds)
		repoRoot, _ := config.FindRoot(".")
		if err := postQuota(ctx, client.New(creds.Endpoint, creds.Token), ref, []quota.Snapshot{s},
			quota.LoadThresholds(repoRoot, os.Getenv)); err != nil {
			return err
		}
		reported = true
	}
	if *asJSON {
		return emit(map[string]any{"snapshot": s, "reported": reported})
	}
	fmt.Printf("Recorded %s %q %s.", s.Harness, s.Account, s.Window)
	if reported {
		fmt.Print(" Reported to the control plane.")
	}
	fmt.Println()
	return nil
}

// ---------------------------------------------------------------------------
// quota suggest — where to continue
// ---------------------------------------------------------------------------

func quotaSuggest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quota suggest", flag.ExitOnError)
	harness := fs.String("harness", "", "the tool running out (default: $CONDUCTOR_HARNESS, else claude)")
	account := fs.String("account", "", "the login running out (default: the one the environment selects)")
	ckpt := fs.String("checkpoint", "latest", "the checkpoint to resume")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h := checkpoint.NormalizeHarness(firstNonEmptyString(*harness, os.Getenv("CONDUCTOR_HARNESS"), "claude"))
	home, _ := os.UserHomeDir()
	acct := *account
	if acct == "" {
		acct = quota.AccountLabel(h, harnessStateDir(h, os.Getenv), home)
	}
	repoRoot, _ := config.FindRoot(".")
	th := quota.LoadThresholds(repoRoot, os.Getenv)
	res := quota.Collect(ctx, quota.Options{})
	s, ok := quota.Suggest(res.Snapshots, h, acct, th, time.Now(), home, os.Getenv)
	if *asJSON {
		out := map[string]any{"from": map[string]string{"harness": h, "account": acct}, "found": ok}
		if ok {
			out["suggestion"] = s
			out["command"] = s.Command(*ckpt)
		}
		return emit(out)
	}
	if !ok {
		fmt.Printf("No other login on this machine has room. Log another one in (see docs/PORTABILITY.md, \"Another login\"),\nor wait for the window to reset.\n")
		return nil
	}
	head := "unknown headroom"
	if s.Headroom != nil {
		head = quota.FormatPercent(100-*s.Headroom) + " used"
	}
	fmt.Printf("%s %q (%s): %s\n  %s\n", s.Harness, s.Account, head, s.Why, s.Command(*ckpt))
	return nil
}

// harnessStateDir is the state directory the environment selects for a harness.
func harnessStateDir(h string, getenv func(string) string) string {
	switch h {
	case "claude":
		return usage.ClaudeConfigDir(getenv)
	case "codex":
		return usage.CodexHome(getenv)
	}
	return ""
}

// ---------------------------------------------------------------------------
// quota statusline — Claude Code's documented rate limits
// ---------------------------------------------------------------------------

// statuslineShimArg marks a status line command as Conductor's shim.
const statuslineShimArg = "quota statusline"

func quotaStatusline(ctx context.Context, args []string) error {
	if len(args) > 0 && (args[0] == "install" || args[0] == "uninstall") {
		return quotaStatuslineInstall(args[0] == "uninstall", args[1:])
	}
	fs := flag.NewFlagSet("quota statusline", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	chain := fs.String("chain-b64", "", "the status line command this shim wraps, base64")
	show := fs.Bool("show", false, "print a short usage segment when there is no command to chain to")
	_ = fs.Parse(args) // never fail: a status line that errors blanks the user's bar
	runStatuslineShim(ctx, os.Stdin, os.Stdout, os.Stderr, *chain, *show, os.Getenv)
	return nil
}

// runStatuslineShim records the rate limits Claude Code passes to its status line, then
// hands the very same payload to the user's own status line command and passes its output
// through. Every failure is swallowed: the shim exists to be invisible.
func runStatuslineShim(ctx context.Context, in io.Reader, out, errOut io.Writer, chainB64 string, show bool, getenv func(string) string) {
	payload, _ := io.ReadAll(io.LimitReader(in, 1<<20))
	cfg := usage.ClaudeConfigDir(getenv)
	home, _ := os.UserHomeDir()
	now := time.Now()
	snaps := quota.StatuslineSnapshots(payload, quota.AccountLabel("claude", cfg, home), now)
	for i := range snaps {
		snaps[i].Machine, snaps[i].StateDir = quotaMachine(), cfg
	}
	if len(snaps) > 0 && !quota.Disabled(getenv) {
		_ = quota.Save(snaps)
	}

	var chained string
	if chainB64 != "" {
		if b, err := base64.StdEncoding.DecodeString(chainB64); err == nil {
			chained = strings.TrimSpace(string(b))
		}
	}
	if chained != "" {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", chained)
		cmd.Stdin = strings.NewReader(string(payload))
		cmd.Stdout, cmd.Stderr = out, errOut
		_ = cmd.Run()
		return
	}
	if show && len(snaps) > 0 {
		var parts []string
		for _, s := range snaps {
			if pct, ok := s.Percent(now); ok {
				name := s.Window
				if name == quota.WindowWeekly {
					name = "wk"
				}
				parts = append(parts, name+" "+quota.FormatPercent(pct))
			}
		}
		fmt.Fprintln(out, strings.Join(parts, " · "))
	}
}

// quotaStatuslineInstall points a Claude Code config directory's status line at the shim,
// carrying the existing command inside the new one so nothing visible changes and uninstall
// can put it back exactly.
func quotaStatuslineInstall(remove bool, args []string) error {
	name := "install"
	if remove {
		name = "uninstall"
	}
	fs := flag.NewFlagSet("quota statusline "+name, flag.ExitOnError)
	configDir := fs.String("config-dir", "", "the Claude Code config directory (default: $CLAUDE_CONFIG_DIR, else ~/.claude)")
	dryRun := fs.Bool("dry-run", false, "print the new statusLine setting without writing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := *configDir
	if dir == "" {
		dir = usage.ClaudeConfigDir(os.Getenv)
	}
	exe := "conductor"
	if p, err := exec.LookPath("conductor"); err != nil || p == "" {
		if self, err := os.Executable(); err == nil {
			exe = self
		}
	}
	path := filepath.Join(dir, "settings.json")
	changed, line, err := editStatusline(path, exe, remove, *dryRun)
	if err != nil {
		return err
	}
	switch {
	case *dryRun:
		fmt.Println(line)
	case !changed && remove:
		fmt.Printf("%s has no Conductor status line shim.\n", path)
	case !changed:
		fmt.Printf("%s already records usage limits.\n", path)
	case remove:
		fmt.Printf("Restored the status line in %s.\n", path)
	default:
		fmt.Printf("Claude Code's status line in %s now records usage limits; it looks exactly as before.\n", path)
	}
	return nil
}

// editStatusline rewrites settings.json's statusLine. It returns whether anything changed
// and the resulting statusLine as JSON.
func editStatusline(path, exe string, remove, dryRun bool) (bool, string, error) {
	settings := map[string]any{}
	body, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(body, &settings); err != nil {
			return false, "", fmt.Errorf("%s is not valid JSON; leaving it alone: %w", path, err)
		}
	case !os.IsNotExist(err):
		return false, "", err
	}
	sl, _ := settings["statusLine"].(map[string]any)
	current, _ := sl["command"].(string)
	installed := strings.Contains(current, statuslineShimArg)

	if remove {
		if !installed {
			return false, "", nil
		}
		original := ""
		if _, rest, ok := strings.Cut(current, "--chain-b64 "); ok {
			if b, err := base64.StdEncoding.DecodeString(strings.Fields(rest + " ")[0]); err == nil {
				original = string(b)
			}
		}
		if original == "" {
			delete(settings, "statusLine")
		} else {
			sl["command"] = original
		}
	} else {
		if installed {
			return false, "", nil
		}
		shim := shellQuote(exe) + " " + statuslineShimArg
		if sl == nil {
			sl = map[string]any{"type": "command"}
		} else if t, _ := sl["type"].(string); t != "" && t != "command" {
			return false, "", fmt.Errorf("statusLine in %s has type %q; only command status lines can be chained", path, t)
		}
		if current != "" {
			shim += " --chain-b64 " + base64.StdEncoding.EncodeToString([]byte(current))
		}
		sl["type"] = "command"
		sl["command"] = shim
		settings["statusLine"] = sl
	}
	line, _ := json.Marshal(settings["statusLine"])
	if dryRun {
		return true, string(line), nil
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, "", err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + ".conductor-tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), mode); err != nil {
		return false, "", err
	}
	return true, string(line), os.Rename(tmp, path)
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"$`\\;&|<>()*?[]{}!#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------------------------------------------------------------------------
// conductor status / doctor / usage sync
// ---------------------------------------------------------------------------

// printQuotaStatus is the "Usage limits" lines of `conductor status`: the caller's own logins
// at or past the warning threshold, and the team count. Silent when there is nothing to say
// or the control plane predates quota tracking.
func printQuotaStatus(ctx context.Context, apiClient *client.Client, ref string) {
	var team quota.TeamView
	if err := apiClient.Get(ctx, "/v1/projects/"+ref+"/quota", &team); err != nil {
		return
	}
	var lines []string
	now := time.Now()
	for _, l := range team.Mine.Logins {
		if l.Level.Rank() < quota.LevelWarning.Rank() {
			continue
		}
		line := fmt.Sprintf("  %-8s %-12s %s", l.Harness, l.Account, quotaLevelMark(l.Level))
		if l.Headroom != nil {
			line += fmt.Sprintf(" — %s of %s used", quota.FormatPercent(100-*l.Headroom), l.Window)
		}
		for _, s := range team.Mine.Snapshots {
			if s.Harness == l.Harness && s.Account == l.Account && s.Window == l.Window && s.ResetsAt != nil {
				line += ", resets in " + quota.FormatIn(*s.ResetsAt, now)
				break
			}
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 && team.NearLimit == 0 {
		return
	}
	fmt.Println("\nUsage limits")
	for _, l := range lines {
		fmt.Println(l)
	}
	if team.Logins > 0 {
		fmt.Printf("  team: %d of %d logins near their limit (`conductor quota` for yours)\n", team.NearLimit, team.Logins)
	}
}

// printQuotaDoctor is the "Usage limits" section of `conductor doctor`: which collectors
// found data on this machine, how fresh, and what to do about the ones that did not.
func printQuotaDoctor(ctx context.Context) {
	if quota.Disabled(os.Getenv) {
		fmt.Printf("\nUsage limits\n  off (CONDUCTOR_QUOTA=off)\n")
		return
	}
	res := quota.Collect(ctx, quota.Options{SkipNetwork: true})
	fmt.Printf("\nUsage limits\n")
	now := time.Now()
	for _, c := range res.Collectors {
		state := "no readings"
		switch {
		case c.Error != "":
			state = "error: " + c.Error
		case c.Readings > 0 && c.Newest != nil:
			state = fmt.Sprintf("%d reading(s), newest %s", c.Readings, quotaAge(*c.Newest, now))
			if state[len(state)-3:] != "now" {
				state += " ago"
			}
		case !c.Enabled:
			state = "off"
		}
		label := c.Name
		if c.Kind == quota.KindUndocumented {
			label += " (undocumented)"
		}
		if c.Account != "" {
			label += " [" + c.Account + "]"
		}
		if c.Note != "" && c.Readings == 0 {
			state += " — " + c.Note
		}
		fmt.Printf("  %-36s %s\n", label, state)
	}
	if !statuslineShimInstalled(usage.ClaudeConfigDir(os.Getenv)) {
		fmt.Printf("  %-36s %s\n", "claude status line shim", "not installed (`conductor quota statusline install`)")
	}
}

func statuslineShimInstalled(configDir string) bool {
	body, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		return false
	}
	var settings struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	return json.Unmarshal(body, &settings) == nil && strings.Contains(settings.StatusLine.Command, statuslineShimArg)
}

// reportLocalQuota sends this machine's readings with `conductor usage sync`, so one command
// brings the control plane up to date on both tokens and limits.
func reportLocalQuota(ctx context.Context, apiClient *client.Client, ref string) {
	if quota.Disabled(os.Getenv) {
		return
	}
	res := quota.Collect(ctx, quota.Options{})
	repoRoot, _ := config.FindRoot(".")
	if err := postQuota(ctx, apiClient, ref, res.Snapshots, quota.LoadThresholds(repoRoot, os.Getenv)); err != nil {
		fmt.Fprintf(os.Stderr, "conductor: usage limits not reported: %v\n", err)
		return
	}
	if len(res.Snapshots) > 0 {
		fmt.Printf("Reported %d usage-limit reading(s).\n", len(res.Snapshots))
	}
}
