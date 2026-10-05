package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every fixture under testdata/ is synthetic, built from the formats the vendors document or
// publish in their source (docs/USAGE_LIMITS.md cites each). None is copied from a real
// machine, and nothing here touches the network beyond a local test server.

func fixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func byWindow(snaps []Snapshot) map[string]Snapshot {
	out := map[string]Snapshot{}
	for _, s := range snaps {
		out[s.Window] = s
	}
	return out
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCodexRolloutKeepsTheLatestReadingPerWindow(t *testing.T) {
	snaps := byWindow(CodexSnapshots(fixture(t, "codex-rollout.jsonl"), "work"))
	if len(snaps) != 3 {
		t.Fatalf("windows = %v, want 5h, weekly, and the model bucket", snaps)
	}
	five := snaps["5h"]
	if five.UsedPercent == nil || *five.UsedPercent != 83 {
		t.Errorf("5h used = %v, want the later reading 83", five.UsedPercent)
	}
	if five.WindowMinutes != 300 || five.Plan != "plus" || five.Account != "work" || five.Harness != "codex" {
		t.Errorf("5h snapshot = %+v", five)
	}
	if five.ResetsAt == nil || !five.ResetsAt.Equal(mustTime(t, "2026-10-05T15:00:00Z")) {
		t.Errorf("5h resets_at = %v", five.ResetsAt)
	}
	if five.SourceKind != KindLocalFile || five.Source != "codex-rollout" {
		t.Errorf("source = %s/%s", five.Source, five.SourceKind)
	}
	if !five.ObservedAt.Equal(mustTime(t, "2026-10-05T09:05:10Z")) {
		t.Errorf("observed_at = %v, want the event's own timestamp", five.ObservedAt)
	}
	if w := snaps["weekly"]; w.UsedPercent == nil || *w.UsedPercent != 14 {
		t.Errorf("weekly = %+v", w)
	}
	// A model-specific bucket is kept apart from the default one rather than overwriting it.
	if b, ok := snaps["codex_bengalfox:weekly"]; !ok || *b.UsedPercent != 5 {
		t.Errorf("model bucket = %+v (present %v)", b, ok)
	}
}

func TestCodexWindowIsNamedByLengthNotSlot(t *testing.T) {
	// A Pro plan's only window is weekly, and arrives as "primary" with resets_in_seconds
	// (older builds) — the two mistakes community parsers made with it.
	snaps := CodexSnapshots(fixture(t, "codex-rollout-legacy.jsonl"), "default")
	if len(snaps) != 1 {
		t.Fatalf("snaps = %+v", snaps)
	}
	s := snaps[0]
	if s.Window != "weekly" || s.WindowMinutes != 10080 {
		t.Errorf("window = %q (%d min), want weekly", s.Window, s.WindowMinutes)
	}
	want := mustTime(t, "2026-09-01T13:00:10Z")
	if s.ResetsAt == nil || !s.ResetsAt.Equal(want) {
		t.Errorf("resets_at = %v, want event time + resets_in_seconds = %v", s.ResetsAt, want)
	}
}

func TestCodexLimitReachedMarksTheFullWindow(t *testing.T) {
	snaps := byWindow(CodexSnapshots(fixture(t, "codex-rollout-reached.jsonl"), "default"))
	if !snaps["5h"].LimitReached {
		t.Error("the window at 100% should be marked reached")
	}
	if snaps["weekly"].LimitReached {
		t.Error("the weekly window at 60% did not run out")
	}
	if r := snaps["5h"].ResetsAt; r == nil || !r.Equal(mustTime(t, "2026-10-05T12:00:00Z")) {
		t.Errorf("RFC 3339 resets_at = %v", r)
	}
}

func TestCodexTornAndForeignLinesAreSkipped(t *testing.T) {
	in := strings.NewReader("not json\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"rate_limits\":{\"primary\":{\"used_percent\":\"oops\"}}}}\n{\"rate_limits\":")
	if snaps := CodexSnapshots(in, "default"); len(snaps) != 0 {
		t.Errorf("garbage produced readings: %+v", snaps)
	}
}

func TestReadCodexFindsTheNewestReadingAcrossFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	day := filepath.Join(home, ".codex", "sessions", "2026", "10", "05")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	// The older reading sits in the more recently modified file: Codex rewrites old
	// rollouts on resume, so ordering by mtime would pick the wrong one.
	newer := filepath.Join(day, "rollout-2026-10-05T09-00-00-a.jsonl")
	older := filepath.Join(day, "rollout-2026-10-05T08-00-00-b.jsonl")
	write := func(path, line string, mod time.Time) {
		if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	now := mustTime(t, "2026-10-05T10:00:00Z")
	write(newer, `{"timestamp":"2026-10-05T09:30:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":70,"window_minutes":300,"resets_at":1791212400}}}}`, now.Add(-time.Hour))
	write(older, `{"timestamp":"2026-10-05T08:30:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":20,"window_minutes":300,"resets_at":1791212400}}}}`, now.Add(-time.Minute))
	snaps := ReadCodex(filepath.Join(home, ".codex"), "default", now)
	if len(snaps) != 1 || *snaps[0].UsedPercent != 70 {
		t.Fatalf("snaps = %+v, want the 09:30 reading", snaps)
	}
	if snaps[0].Account != "default" || snaps[0].StateDir != filepath.Join(home, ".codex") {
		t.Errorf("account/state = %q/%q", snaps[0].Account, snaps[0].StateDir)
	}
}

func TestStatuslinePayload(t *testing.T) {
	now := mustTime(t, "2025-02-01T12:00:00Z")
	snaps := byWindow(StatuslineSnapshots(fixtureBytes(t, "claude-statusline.json"), "default", now))
	five, week := snaps["5h"], snaps["weekly"]
	if five.UsedPercent == nil || *five.UsedPercent != 23.5 || five.WindowMinutes != 300 {
		t.Errorf("five_hour = %+v", five)
	}
	if five.ResetsAt == nil || !five.ResetsAt.Equal(time.Unix(1738425600, 0)) {
		t.Errorf("five_hour resets_at = %v", five.ResetsAt)
	}
	if week.UsedPercent == nil || *week.UsedPercent != 41.2 || week.WindowMinutes != 10080 {
		t.Errorf("seven_day = %+v", week)
	}
	if five.SourceKind != KindDocumented || five.Source != "claude-statusline" || !five.ObservedAt.Equal(now) {
		t.Errorf("source = %+v", five)
	}

	gw := StatuslineSnapshots(fixtureBytes(t, "claude-statusline-gateway.json"), "default", now)
	if len(gw) != 1 || gw[0].Window != "spend:monthly" || *gw[0].Used != 314.12 || *gw[0].Limit != 500 || gw[0].Unit != "usd" {
		t.Errorf("spend limit = %+v", gw)
	}

	// An API-key login, or the first render before any response, has no rate_limits.
	if s := StatuslineSnapshots([]byte(`{"session_id":"x","model":{"id":"m"}}`), "default", now); len(s) != 0 {
		t.Errorf("payload without rate_limits produced %+v", s)
	}
	if s := StatuslineSnapshots([]byte(`{"rate_limits":`), "default", now); len(s) != 0 {
		t.Errorf("torn payload produced %+v", s)
	}
}

func TestClaudeTranscriptLimitRecords(t *testing.T) {
	snaps := byWindow(ClaudeLimitSnapshots(fixture(t, "claude-transcript.jsonl"), "work"))
	// The user's own prompt quoting a limit message, an ordinary reply that mentions one,
	// and an overload error must all be ignored.
	if len(snaps) != 2 {
		t.Fatalf("windows = %+v, want 5h and weekly", snaps)
	}
	five := snaps["5h"]
	if !five.LimitReached || five.Account != "work" || *five.UsedPercent != 100 {
		t.Errorf("5h = %+v", five)
	}
	// The later of the two five-hour records wins: "resets 2:40pm (UTC)" written at 08:20Z.
	if five.ResetsAt == nil || !five.ResetsAt.Equal(mustTime(t, "2026-10-05T14:40:00Z")) {
		t.Errorf("5h resets_at = %v", five.ResetsAt)
	}
	week := snaps["weekly"]
	if week.ResetsAt == nil || !week.ResetsAt.Equal(mustTime(t, "2026-10-09T14:00:00Z")) {
		t.Errorf("weekly resets_at = %v, want Oct 9 4pm Berlin", week.ResetsAt)
	}
}

func TestClaudeLimitText(t *testing.T) {
	at := mustTime(t, "2026-10-05T08:20:00Z") // a Monday
	cases := []struct {
		text   string
		window string
		resets string // RFC 3339, or "" for unknown
	}{
		{"Claude AI usage limit reached|1791158400", "5h", "2026-10-05T00:00:00Z"},
		{"You've hit your limit · resets 3pm (UTC)", "5h", "2026-10-05T15:00:00Z"},
		{"You've hit your session limit · resets 2:40am (UTC)", "5h", "2026-10-06T02:40:00Z"},
		{"You've hit your weekly limit · resets Mon 12:00am (UTC)", "weekly", "2026-10-12T00:00:00Z"},
		{"You've hit your weekly limit · resets Jun 3 at 4pm (Europe/Berlin)", "weekly", "2027-06-03T14:00:00Z"},
		{"You've hit your Opus weekly limit · resets Fri 9am (America/Los_Angeles)", "opus:weekly", "2026-10-09T16:00:00Z"},
		{"You've hit your limit", "5h", ""},
	}
	for _, c := range cases {
		s, ok := ParseClaudeLimitText(c.text, at)
		if !ok {
			t.Errorf("%q: not recognised", c.text)
			continue
		}
		if s.Window != c.window {
			t.Errorf("%q: window %q, want %q", c.text, s.Window, c.window)
		}
		switch {
		case c.resets == "" && s.ResetsAt != nil:
			t.Errorf("%q: resets_at %v, want unknown", c.text, s.ResetsAt)
		case c.resets != "" && (s.ResetsAt == nil || !s.ResetsAt.Equal(mustTime(t, c.resets))):
			t.Errorf("%q: resets_at %v, want %s", c.text, s.ResetsAt, c.resets)
		}
	}
	for _, text := range []string{"API Error: 529 Overloaded", "", "Request timed out"} {
		if _, ok := ParseClaudeLimitText(text, at); ok {
			t.Errorf("%q recognised as a usage limit", text)
		}
	}
}

func TestReadClaudeTranscriptsDropsWindowsThatReset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude-work", "projects", "-work-repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fixtureBytes(t, "claude-transcript.jsonl")
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	now := mustTime(t, "2026-10-05T15:00:00Z") // after the 14:40 five-hour reset
	snaps := ReadClaudeTranscripts(filepath.Join(home, ".claude-work"), "work", now)
	if len(snaps) != 1 || snaps[0].Window != "weekly" || snaps[0].Account != "work" {
		t.Fatalf("snaps = %+v, want only the still-open weekly limit for login \"work\"", snaps)
	}
}

func TestCursorSummary(t *testing.T) {
	now := mustTime(t, "2026-10-05T10:00:00Z")
	snaps, err := ParseCursorSummary(fixtureBytes(t, "cursor-usage-summary.json"), "default", now)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snaps = %+v, err = %v", snaps, err)
	}
	s := snaps[0]
	if *s.UsedPercent != 82 || *s.Used != 1640 || *s.Limit != 2000 || s.Unit != "usd_cents" || s.Plan != "pro" {
		t.Errorf("snapshot = %+v", s)
	}
	if s.SourceKind != KindUndocumented || s.Window != WindowMonthly {
		t.Errorf("kind/window = %s/%s", s.SourceKind, s.Window)
	}
	if s.ResetsAt == nil || !s.ResetsAt.Equal(mustTime(t, "2026-10-20T14:11:55Z")) {
		t.Errorf("resets_at = %v", s.ResetsAt)
	}
	if _, err := ParseCursorSummary([]byte(`{"something":"else"}`), "default", now); err == nil {
		t.Error("an unrecognised response should be reported, not read as zero usage")
	}
}

func TestFetchCursorSendsOnlyTheSuppliedCookie(t *testing.T) {
	var gotCookie, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie, gotAuth = r.Header.Get("Cookie"), r.Header.Get("Authorization")
		_, _ = w.Write(fixtureBytes(t, "cursor-usage-summary.json"))
	}))
	defer srv.Close()
	now := mustTime(t, "2026-10-05T10:00:00Z")
	snaps, err := FetchCursor(context.Background(), srv.Client(), srv.URL, "user_01::tok", "default", now)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snaps = %+v, err = %v", snaps, err)
	}
	if gotCookie != "WorkosCursorSessionToken=user_01::tok" || gotAuth != "" {
		t.Errorf("cookie = %q, authorization = %q", gotCookie, gotAuth)
	}

	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer gone.Close()
	if _, err := FetchCursor(context.Background(), gone.Client(), gone.URL, "x", "default", now); err == nil {
		t.Error("an endpoint that disappeared should be an error the caller can report")
	}
}

func TestLevels(t *testing.T) {
	now := mustTime(t, "2026-10-05T10:00:00Z")
	later := Time(now.Add(time.Hour))
	th := Thresholds{Warn: 80, Critical: 95}
	cases := []struct {
		s    Snapshot
		want Level
	}{
		{Snapshot{UsedPercent: Float(10), ResetsAt: later}, LevelOK},
		{Snapshot{UsedPercent: Float(80), ResetsAt: later}, LevelWarning},
		{Snapshot{UsedPercent: Float(95.5), ResetsAt: later}, LevelCritical},
		{Snapshot{UsedPercent: Float(100), ResetsAt: later}, LevelExhausted},
		{Snapshot{UsedPercent: Float(40), LimitReached: true, ResetsAt: later}, LevelExhausted},
		{Snapshot{Used: Float(1900), Limit: Float(2000)}, LevelCritical},
		{Snapshot{}, LevelUnknown},
		// The window reset since the reading: whatever it said, it is empty now.
		{Snapshot{UsedPercent: Float(100), LimitReached: true, ResetsAt: Time(now.Add(-time.Minute))}, LevelOK},
	}
	for i, c := range cases {
		if got := th.Level(c.s, now); got != c.want {
			t.Errorf("case %d: level %s, want %s", i, got, c.want)
		}
	}
}

func TestThresholdPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".conductor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".conductor", "project.yaml"),
		[]byte("kind: Project\nquota:\n  warnPercent: 70\n  criticalPercent: 90\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := LoadThresholds(repo, getenv); got != (Thresholds{70, 90}) {
		t.Errorf("project file: %+v", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".conductor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".conductor", "quota.yaml"), []byte("quota:\n  warnPercent: 60\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadThresholds(repo, getenv); got != (Thresholds{60, 90}) {
		t.Errorf("user file over project: %+v", got)
	}
	env["CONDUCTOR_QUOTA_CRITICAL"] = "97%"
	if got := LoadThresholds(repo, getenv); got != (Thresholds{60, 97}) {
		t.Errorf("env over files: %+v", got)
	}
	if got := LoadThresholds("", func(string) string { return "" }); got.Critical != 97 && got.Warn != 60 {
		t.Errorf("defaults: %+v", got)
	}
	if got := (Thresholds{Warn: 99, Critical: 50}).Normalize(); got.Critical < got.Warn {
		t.Errorf("inverted thresholds not repaired: %+v", got)
	}
}

func TestAlertsOncePerWindowPerLevel(t *testing.T) {
	th := DefaultThresholds
	t0 := mustTime(t, "2026-10-05T10:00:00Z")
	reset1 := Time(mustTime(t, "2026-10-05T15:00:00Z"))
	snap := func(pct float64, resets *time.Time) Snapshot {
		return Snapshot{Harness: "claude", Account: "default", Machine: "m", Window: "5h", WindowMinutes: 300,
			UsedPercent: Float(pct), ResetsAt: resets}
	}
	book := alertBook{}
	step := func(at time.Time, s Snapshot) []Level {
		alerts, _ := raise(book, []Snapshot{s}, th, at)
		var out []Level
		for _, a := range alerts {
			out = append(out, a.Level)
		}
		return out
	}
	expect := func(name string, got []Level, want ...Level) {
		t.Helper()
		if strings.Join(levelsToStrings(got), ",") != strings.Join(levelsToStrings(want), ",") {
			t.Errorf("%s: raised %v, want %v", name, got, want)
		}
	}

	expect("below warn", step(t0, snap(50, reset1)))
	expect("crosses warn", step(t0.Add(time.Minute), snap(81, reset1)), LevelWarning)
	expect("still warn", step(t0.Add(2*time.Minute), snap(85, reset1)))
	// The reset time a relative "resets in" produces jitters by seconds; same window.
	jitter := Time(reset1.Add(3 * time.Second))
	expect("same window, jittered reset", step(t0.Add(3*time.Minute), snap(86, jitter)))
	expect("crosses critical", step(t0.Add(4*time.Minute), snap(96, reset1)), LevelCritical)
	expect("critical again", step(t0.Add(5*time.Minute), snap(97, reset1)))
	expect("exhausted", step(t0.Add(6*time.Minute), snap(100, reset1)), LevelExhausted)
	expect("exhausted again", step(t0.Add(7*time.Minute), snap(100, reset1)))

	// The next window: everything re-arms. A jump straight to critical raises critical
	// alone, and warning is marked so it does not follow later in that window.
	reset2 := Time(reset1.Add(5 * time.Hour))
	after := reset1.Add(time.Hour)
	expect("next window jumps to critical", step(after, snap(96, reset2)), LevelCritical)
	expect("no late warning", step(after.Add(time.Minute), snap(96, reset2)))
}

func TestAlertsWithoutResetTimesUseTheWindowLength(t *testing.T) {
	th := DefaultThresholds
	t0 := mustTime(t, "2026-10-05T10:00:00Z")
	s := Snapshot{Harness: "codex", Account: "default", Window: "5h", WindowMinutes: 300, UsedPercent: Float(90)}
	book := alertBook{}
	if a, _ := raise(book, []Snapshot{s}, th, t0); len(a) != 1 {
		t.Fatalf("first crossing: %+v", a)
	}
	if a, _ := raise(book, []Snapshot{s}, th, t0.Add(4*time.Hour)); len(a) != 0 {
		t.Errorf("re-raised inside the window: %+v", a)
	}
	if a, _ := raise(book, []Snapshot{s}, th, t0.Add(5*time.Hour)); len(a) != 1 {
		t.Errorf("not re-armed after a full window: %+v", a)
	}
}

func TestRaiseLocalPersistsAcrossProcesses(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	now := mustTime(t, "2026-10-05T10:00:00Z")
	s := Snapshot{Harness: "claude", Account: "default", Machine: "m", Window: "weekly", WindowMinutes: 10080,
		UsedPercent: Float(99), ResetsAt: Time(now.Add(48 * time.Hour))}
	first, err := RaiseLocal([]Snapshot{s}, DefaultThresholds, now)
	if err != nil || len(first) != 1 || first[0].Level != LevelCritical {
		t.Fatalf("first = %+v, err = %v", first, err)
	}
	// A second sidecar (a fresh read of the same file) must not raise it again.
	second, err := RaiseLocal([]Snapshot{s}, DefaultThresholds, now.Add(time.Minute))
	if err != nil || len(second) != 0 {
		t.Errorf("second = %+v, err = %v", second, err)
	}
}

func levelsToStrings(ls []Level) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = string(l)
	}
	return out
}

func TestAccountLabels(t *testing.T) {
	home := "/home/u"
	cases := map[string]string{
		"":                     "default",
		"/home/u/.claude":      "default",
		"/home/u/.claude-work": "work",
		"/home/u/.conductor/accounts/claude/team": "team",
	}
	for dir, want := range cases {
		if got := AccountLabel("claude", dir, home); got != want {
			t.Errorf("%q: %q, want %q", dir, got, want)
		}
	}
	other := AccountLabel("claude", "/srv/alice.smith/claude-state", home)
	if !strings.HasPrefix(other, "dir-") || strings.Contains(other, "alice") || len(other) != len("dir-")+10 {
		t.Errorf("an arbitrary path must become an opaque label, got %q", other)
	}
	if other != AccountLabel("claude", "/srv/alice.smith/claude-state/", home) {
		t.Error("the label must be stable for the same directory")
	}
}

func TestSuggestPrefersTheSameToolOnAnotherLogin(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".claude", ".claude-work", ".codex"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := mustTime(t, "2026-10-05T10:00:00Z")
	later := Time(now.Add(2 * time.Hour))
	snap := func(h, acct, dir string, pct float64) Snapshot {
		return Snapshot{Harness: h, Account: acct, Window: "5h", UsedPercent: Float(pct), ResetsAt: later,
			StateDir: filepath.Join(home, dir), ObservedAt: now}
	}
	none := func(string) string { return "" }
	snaps := []Snapshot{
		snap("claude", "default", ".claude", 97),
		snap("claude", "work", ".claude-work", 30),
		snap("codex", "default", ".codex", 5),
	}
	s, ok := Suggest(snaps, "claude", "default", DefaultThresholds, now, home, none)
	if !ok || s.Harness != "claude" || s.Account != "work" || !s.Native {
		t.Fatalf("suggestion = %+v", s)
	}
	if got := s.Command("9a474a"); got != "conductor checkpoint resume 9a474a --account work" {
		t.Errorf("command = %q", got)
	}

	// The other Claude login is near its own limit: Codex has the room, from a continuation.
	snaps[1] = snap("claude", "work", ".claude-work", 88)
	s, _ = Suggest(snaps, "claude", "default", DefaultThresholds, now, home, none)
	if s.Harness != "codex" || s.Native || s.Command("abc") != "conductor checkpoint resume abc --harness codex" {
		t.Errorf("suggestion = %+v, command %q", s, s.Command("abc"))
	}

	// Leaving a non-default login for the default one names its directory exactly, since
	// --account default would resolve to ~/.claude-default.
	s, _ = Suggest([]Snapshot{snap("claude", "work", ".claude-work", 99), snap("claude", "default", ".claude", 10)},
		"claude", "work", DefaultThresholds, now, home, none)
	want := "conductor checkpoint resume abc --state-dir " + filepath.Join(home, ".claude")
	if s.Command("abc") != want {
		t.Errorf("command = %q, want %q", s.Command("abc"), want)
	}

	// Nothing has room: no suggestion that would only hit the same wall.
	if s, ok := Suggest([]Snapshot{snap("claude", "default", ".claude", 100), snap("codex", "default", ".codex", 100)},
		"claude", "default", DefaultThresholds, now, home, none); ok && s.Account != "work" {
		t.Errorf("suggested an exhausted login: %+v", s)
	}
}

func TestNotifierCommands(t *testing.T) {
	has := func(string) (string, error) { return "/usr/bin/x", nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	n := Notifier{GOOS: "darwin", Getenv: env(nil), LookPath: has}
	name, args, ok := n.Command("Conductor", `claude login "work" is at 96%`)
	if !ok || name != "osascript" || !strings.Contains(args[1], `\"work\"`) {
		t.Errorf("darwin: %s %v %v", name, args, ok)
	}
	n = Notifier{GOOS: "linux", Getenv: env(map[string]string{"DISPLAY": ":0"}), LookPath: has}
	if name, _, ok := n.Command("t", "b"); !ok || name != "notify-send" {
		t.Errorf("linux with a display: %s %v", name, ok)
	}
	n = Notifier{GOOS: "linux", Getenv: env(nil), LookPath: has}
	if _, _, ok := n.Command("t", "b"); ok {
		t.Error("a headless Linux box should stay silent")
	}
	n = Notifier{GOOS: "darwin", Getenv: env(map[string]string{"CONDUCTOR_QUOTA_NOTIFY": "off"}), LookPath: has}
	if _, _, ok := n.Command("t", "b"); ok {
		t.Error("CONDUCTOR_QUOTA_NOTIFY=off should silence notifications")
	}
	n = Notifier{GOOS: "windows", Getenv: env(nil), LookPath: has}
	if _, _, ok := n.Command("t", "b"); ok {
		t.Error("unsupported platforms stay silent")
	}
}

func TestCollectNeverFailsAndMergesStoredReadings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONDUCTOR_STATE_DIR", filepath.Join(home, ".conductor"))
	// A Claude login whose projects directory holds garbage, and a Codex login with a
	// directory where a file should be.
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects", "-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "projects", "-x", "s.jsonl"), []byte("\x00\xff{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex", "sessions", "rollout-x.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := mustTime(t, "2026-10-05T10:00:00Z")
	// What the status line shim stored a minute ago.
	if err := Save([]Snapshot{{Harness: "claude", Account: "default", Machine: "box", Window: "5h",
		UsedPercent: Float(42), ResetsAt: Time(now.Add(time.Hour)), Source: "claude-statusline",
		SourceKind: KindDocumented, ObservedAt: now.Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	res := Collect(context.Background(), Options{
		Getenv: func(string) string { return "" }, Now: func() time.Time { return now }, Home: home, Machine: "box",
	})
	if len(res.Snapshots) != 1 || *res.Snapshots[0].UsedPercent != 42 {
		t.Errorf("snapshots = %+v", res.Snapshots)
	}
	var cursor *CollectorStatus
	for i := range res.Collectors {
		if res.Collectors[i].Name == "cursor-usage-summary" {
			cursor = &res.Collectors[i]
		}
	}
	if cursor == nil || cursor.Enabled {
		t.Errorf("Cursor must stay off until opted into: %+v", cursor)
	}
}

func TestCollectAsksCursorOnlyWhenOptedInAndCaches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONDUCTOR_STATE_DIR", filepath.Join(home, ".conductor"))
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write(fixtureBytes(t, "cursor-usage-summary.json"))
	}))
	defer srv.Close()
	now := mustTime(t, "2026-10-05T10:00:00Z")
	opts := Options{
		Getenv: func(k string) string {
			if k == "CONDUCTOR_QUOTA_CURSOR_COOKIE" {
				return "c"
			}
			return ""
		},
		Now: func() time.Time { return now }, Home: home, Machine: "box",
		HTTPClient: srv.Client(), CursorURL: srv.URL,
	}
	res := Collect(context.Background(), opts)
	if calls != 1 || len(res.Snapshots) != 1 || res.Snapshots[0].Harness != "cursor" {
		t.Fatalf("calls = %d, snapshots = %+v", calls, res.Snapshots)
	}
	now = now.Add(time.Minute)
	res = Collect(context.Background(), opts)
	if calls != 1 || len(res.Snapshots) != 1 {
		t.Errorf("asked again within the refresh interval: calls = %d", calls)
	}
	opts.SkipNetwork = true
	now = now.Add(time.Hour)
	Collect(context.Background(), opts)
	if calls != 1 {
		t.Error("SkipNetwork still called out")
	}
}

func TestCursorFailureIsReportedNotFatal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONDUCTOR_STATE_DIR", filepath.Join(home, ".conductor"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"renamed":{"everything":true}}`)) // the endpoint changed shape
	}))
	defer srv.Close()
	now := mustTime(t, "2026-10-05T10:00:00Z")
	opts := Options{
		Getenv: func(k string) string {
			if k == "CONDUCTOR_QUOTA_CURSOR_COOKIE" {
				return "c"
			}
			return ""
		},
		Now: func() time.Time { return now }, Home: home, Machine: "box",
		HTTPClient: srv.Client(), CursorURL: srv.URL,
	}
	cursorStatus := func(res Result) CollectorStatus {
		for _, c := range res.Collectors {
			if c.Name == "cursor-usage-summary" {
				return c
			}
		}
		t.Fatal("no cursor collector status")
		return CollectorStatus{}
	}
	res := Collect(context.Background(), opts)
	if st := cursorStatus(res); !st.Enabled || !strings.Contains(st.Error, "unrecognised") || len(res.Snapshots) != 0 {
		t.Errorf("status = %+v, snapshots = %+v", st, res.Snapshots)
	}
	// A pass that stays on the machine still says why Cursor has no reading.
	opts.SkipNetwork = true
	if st := cursorStatus(Collect(context.Background(), opts)); !strings.Contains(st.Error, "unrecognised") {
		t.Errorf("offline status lost the last error: %+v", st)
	}
}
