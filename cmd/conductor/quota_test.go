package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/quota"
)

// The status line shim must leave the user's status line exactly as it was — same input in,
// same output out — while keeping the rate limits Claude Code passed.
func TestStatuslineShimRecordsAndChains(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", state)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".claude-work")
	env := map[string]string{"CLAUDE_CONFIG_DIR": cfg}
	getenv := func(k string) string { return env[k] }

	payload, err := os.ReadFile(filepath.Join("..", "..", "internal", "quota", "testdata", "claude-statusline.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The user's own status line: echoes a field of its input, and something to stderr.
	original := `jq -r .session_id 2>/dev/null || sed -n 's/.*"session_id": "\([^"]*\)".*/\1/p'; echo warn >&2`
	var out, errOut bytes.Buffer
	runStatuslineShim(context.Background(), bytes.NewReader(payload), &out, &errOut,
		b64(original), false, getenv)
	if strings.TrimSpace(out.String()) != "abc123" {
		t.Errorf("chained output = %q, want the original command's output", out.String())
	}
	if strings.TrimSpace(errOut.String()) != "warn" {
		t.Errorf("chained stderr = %q", errOut.String())
	}

	snaps := quota.Load(time.Now())
	if len(snaps) != 2 {
		t.Fatalf("stored = %+v, want five_hour and seven_day", snaps)
	}
	for _, s := range snaps {
		if s.Account != "work" || s.Source != "claude-statusline" || s.StateDir != cfg {
			t.Errorf("stored snapshot = %+v", s)
		}
	}

	// Without a command to chain to the shim prints nothing — unless asked to.
	out.Reset()
	runStatuslineShim(context.Background(), bytes.NewReader(payload), &out, &errOut, "", false, getenv)
	if out.Len() != 0 {
		t.Errorf("shim printed %q with nothing to chain", out.String())
	}
	runStatuslineShim(context.Background(), bytes.NewReader(payload), &out, &errOut, "", true, getenv)
	if !strings.Contains(out.String(), "%") {
		t.Errorf("--show printed %q", out.String())
	}
	// Garbage in: no panic, no output, nothing stored.
	out.Reset()
	runStatuslineShim(context.Background(), strings.NewReader("{{{"), &out, &errOut, "", true, getenv)
	if out.Len() != 0 {
		t.Errorf("garbage produced %q", out.String())
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestStatuslineInstallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := map[string]any{
		"model": "opus",
		"statusLine": map[string]any{
			"type": "command", "command": "~/.claude/statusline.sh 'with quotes'", "padding": 2, "refreshInterval": 5,
		},
	}
	body, _ := json.Marshal(original)
	if err := os.WriteFile(path, body, 0o640); err != nil {
		t.Fatal(err)
	}

	changed, _, err := editStatusline(path, "/opt/conductor bin/conductor", false, false)
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	var got map[string]any
	readJSON(t, path, &got)
	sl := got["statusLine"].(map[string]any)
	cmd := sl["command"].(string)
	if !strings.HasPrefix(cmd, "'/opt/conductor bin/conductor' quota statusline --chain-b64 ") {
		t.Errorf("command = %q", cmd)
	}
	if sl["padding"] != float64(2) || sl["refreshInterval"] != float64(5) || got["model"] != "opus" {
		t.Errorf("unrelated settings disturbed: %+v", got)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want the file's own", st.Mode().Perm())
	}
	if changed, _, _ := editStatusline(path, "conductor", false, false); changed {
		t.Error("a second install should be a no-op")
	}

	if changed, _, err := editStatusline(path, "", true, false); err != nil || !changed {
		t.Fatalf("uninstall: changed=%v err=%v", changed, err)
	}
	readJSON(t, path, &got)
	if got["statusLine"].(map[string]any)["command"] != "~/.claude/statusline.sh 'with quotes'" {
		t.Errorf("uninstall did not restore the original: %+v", got["statusLine"])
	}

	// No status line before: install adds one, uninstall removes it again.
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := editStatusline(empty, "conductor", false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := editStatusline(empty, "conductor", true, false); err != nil {
		t.Fatal(err)
	}
	readJSON(t, empty, &got)
	if _, ok := got["statusLine"]; ok || got["theme"] != "dark" {
		t.Errorf("after round trip: %+v", got)
	}

	// A file that is not JSON is left alone.
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"statusLine": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := editStatusline(broken, "conductor", false, false); err == nil {
		t.Error("invalid settings.json should be refused, not overwritten")
	}
}

func readJSON(t *testing.T, path string, v *map[string]any) {
	t.Helper()
	*v = map[string]any{}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaWatcherCheckpointsAtCriticalAndPrintsTheResume(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".claude", ".claude-work"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	resets := quota.Time(now.Add(2 * time.Hour))
	snaps := []quota.Snapshot{
		{Harness: "claude", Account: "default", Window: "5h", UsedPercent: quota.Float(82), ResetsAt: resets,
			StateDir: filepath.Join(home, ".claude"), ObservedAt: now},
		{Harness: "claude", Account: "work", Window: "5h", UsedPercent: quota.Float(12), ResetsAt: resets,
			StateDir: filepath.Join(home, ".claude-work"), ObservedAt: now},
		{Harness: "codex", Account: "default", Window: "weekly", UsedPercent: quota.Float(97), ResetsAt: resets,
			ObservedAt: now},
	}
	var notes []string
	captures := 0
	var out bytes.Buffer
	w := &quotaWatcher{
		harness: "claude", account: "default", th: quota.DefaultThresholds, home: home,
		collect: func(context.Context) quota.Result { return quota.Result{Snapshots: snaps} },
		capture: func(context.Context) (string, error) {
			captures++
			return "20261005T100000Z-claude-9a474a", nil
		},
		notifier: quota.Notifier{
			GOOS: "linux", Getenv: func(k string) string { return map[string]string{"DISPLAY": ":0"}[k] },
			LookPath: func(string) (string, error) { return "/usr/bin/notify-send", nil },
			Run: func(_ context.Context, name string, args ...string) error {
				notes = append(notes, strings.Join(args, " "))
				return nil
			},
		},
		out: &out, now: func() time.Time { return now }, sent: map[string]time.Time{},
	}
	book := map[string]map[quota.Level]*quota.Mark{}
	w.raise = func(s []quota.Snapshot, th quota.Thresholds, at time.Time) ([]quota.Alert, error) {
		var alerts []quota.Alert
		for _, x := range s {
			lvl, mark := quota.Decide(book[x.Key()], x, th, at)
			if book[x.Key()] == nil {
				book[x.Key()] = map[quota.Level]*quota.Mark{}
			}
			for _, l := range mark {
				book[x.Key()][l] = &quota.Mark{At: at, ResetsAt: x.ResetsAt}
			}
			if lvl != "" {
				alerts = append(alerts, quota.Alert{Level: lvl, Snapshot: x})
			}
		}
		return alerts, nil
	}

	// Warning on this session's login and critical on another tool's: two notifications,
	// no checkpoint — the Codex login is not the one this session runs on.
	w.tick(context.Background())
	if len(notes) != 2 || captures != 0 || out.Len() != 0 {
		t.Fatalf("first tick: notes=%v captures=%d out=%q", notes, captures, out.String())
	}

	// This login crosses the critical threshold: checkpoint, then the resume command.
	snaps[0].UsedPercent = quota.Float(96)
	snaps[0].ObservedAt = now.Add(time.Minute)
	w.tick(context.Background())
	if captures != 1 {
		t.Fatalf("captures = %d, want 1", captures)
	}
	if !strings.Contains(out.String(), "conductor checkpoint resume 9a474a --account work") {
		t.Errorf("printed %q, want the resume command for the login with room", out.String())
	}
	// The same window again: nothing more.
	w.tick(context.Background())
	if captures != 1 || len(notes) != 3 {
		t.Errorf("repeat tick: captures=%d notes=%d", captures, len(notes))
	}
}

func TestQuotaWatcherSurvivesAPanickingCollector(t *testing.T) {
	w := &quotaWatcher{
		collect: func(context.Context) quota.Result { panic("format changed") },
		now:     time.Now, sent: map[string]time.Time{},
	}
	w.tick(context.Background()) // must not panic
}
