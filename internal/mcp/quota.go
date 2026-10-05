package mcp

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/aburan28/conductor/internal/checkpoint"
	"github.com/aburan28/conductor/internal/config"
	"github.com/aburan28/conductor/internal/quota"
	"github.com/aburan28/conductor/internal/usage"
)

// coord_quota lets an agent see how much of its own login's usage window is left, so it can
// checkpoint and say where the work stands before the limit lands rather than being cut off
// mid-edit. It is read-only, and it shows only the caller's own logins — the same thing
// `conductor quota` shows the person.

func quotaToolDefinition() map[string]any {
	return map[string]any{
		"name": "coord_quota",
		"description": "How close your subscription login is to its usage limit (Claude's 5-hour and weekly windows, " +
			"Codex's primary and secondary windows, and others the user enabled), and which other login or tool has the " +
			"most room. Read-only; shows only your own logins. When it reports warning or worse, finish the step you are " +
			"on and call coord_checkpoint with a note so the work can continue elsewhere.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

type quotaToolResult struct {
	Harness    string           `json:"harness,omitempty"`
	Account    string           `json:"account,omitempty"`
	Level      quota.Level      `json:"level"`
	Headroom   *float64         `json:"headroom_percent,omitempty"`
	Window     string           `json:"tightest_window,omitempty"`
	ResetsAt   *time.Time       `json:"resets_at,omitempty"`
	Thresholds quota.Thresholds `json:"thresholds"`
	Logins     []quota.Login    `json:"logins"`
	Continue   string           `json:"continue_with,omitempty"`
	Advice     string           `json:"advice"`
	Privacy    string           `json:"privacy"`
}

func (s *Server) quotaTool(ctx context.Context, _ json.RawMessage) (any, error) {
	now := time.Now()
	th := quota.DefaultThresholds
	var snaps []quota.Snapshot
	var harness, account, home string

	if s.local {
		// The stdio gateway runs beside the harness: read this machine directly (fresh, and
		// it works with the control plane down) and add the owner's other machines.
		cwd, _ := os.Getwd()
		root, _ := config.FindRoot(cwd)
		th = quota.LoadThresholds(root, os.Getenv)
		snaps = quota.Collect(ctx, quota.Options{SkipNetwork: true}).Snapshots
		host, _ := os.Hostname()
		var mine quota.View
		if err := s.api.Get(ctx, "/v1/quota", &mine); err == nil {
			for _, row := range mine.Snapshots {
				if row.Machine != host {
					snaps = append(snaps, row.Snapshot)
				}
			}
		}
		harness = checkpoint.NormalizeHarness(os.Getenv("CONDUCTOR_HARNESS"))
		if harness == "" && (os.Getenv("CLAUDECODE") != "" || os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "") {
			harness = "claude"
		}
		home, _ = os.UserHomeDir()
		switch harness {
		case "claude":
			account = quota.AccountLabel(harness, usage.ClaudeConfigDir(os.Getenv), home)
		case "codex":
			account = quota.AccountLabel(harness, usage.CodexHome(os.Getenv), home)
		}
	} else {
		var mine quota.View
		if err := s.api.Get(ctx, "/v1/quota", &mine); err != nil {
			return nil, err
		}
		for _, row := range mine.Snapshots {
			snaps = append(snaps, row.Snapshot)
		}
	}
	snaps = quota.Latest(snaps)

	out := quotaToolResult{
		Harness: harness, Account: account, Level: quota.LevelUnknown, Thresholds: th,
		Logins:  quota.Logins(snaps, th, now),
		Privacy: "your own logins only; teammates see a count, never your logins",
	}
	for _, l := range out.Logins {
		if l.Harness == harness && l.Account == account {
			out.Level, out.Headroom, out.Window = l.Level, l.Headroom, l.Window
		}
	}
	for _, sn := range snaps {
		if sn.Harness == harness && sn.Account == account && sn.Window == out.Window {
			out.ResetsAt = sn.ResetsAt
		}
	}
	if s.local && harness != "" && out.Level.Rank() >= quota.LevelWarning.Rank() {
		if sug, ok := quota.Suggest(snaps, harness, account, th, now, home, os.Getenv); ok {
			out.Continue = sug.Command("latest")
		}
	}
	switch {
	case harness == "" && s.local:
		out.Advice = "Could not tell which tool this session runs in; the logins above are everything known."
	case out.Level == quota.LevelUnknown:
		out.Advice = "No usage reading for this login yet. Claude Code reports one after `conductor quota statusline install`; Codex after its first response."
	case out.Level.Rank() >= quota.LevelCritical.Rank():
		out.Advice = "Nearly out. Finish the current step, then call coord_checkpoint with a note on where the work stands."
	case out.Level == quota.LevelWarning:
		out.Advice = "Past the warning threshold. Keep steps small and checkpoint (coord_checkpoint) at the next milestone."
	default:
		out.Advice = "Plenty of room."
	}
	return jsonResult(out), nil
}
