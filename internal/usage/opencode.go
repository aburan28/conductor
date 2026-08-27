package usage

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

// OpenCode keeps its sessions in a database of its own; rather than depend on its schema,
// this reader asks the `opencode` CLI, which is present wherever OpenCode is.
// `opencode session list --format json` (scoped to the current directory) names the sessions,
// and `opencode export --sanitize <id>` returns each message's tokens, cost, model, and
// provider with the transcript and file contents already redacted by OpenCode itself — the
// one harness that offers to leave the words out before we do.

type openCodeSession struct {
	ID      string `json:"id"`
	Created int64  `json:"created"` // unix milliseconds
	Updated int64  `json:"updated"`
}

type openCodeExport struct {
	Messages []struct {
		Info struct {
			Role       string  `json:"role"`
			ModelID    string  `json:"modelID"`
			ProviderID string  `json:"providerID"`
			Cost       float64 `json:"cost"`
			Tokens     struct {
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
			Time struct {
				Created   int64 `json:"created"`
				Completed int64 `json:"completed"`
			} `json:"time"`
		} `json:"info"`
	} `json:"messages"`
}

// OpenCodeTurns parses the output of `opencode export` for one session.
func OpenCodeTurns(sessionID string, export []byte) ([]Turn, error) {
	var doc openCodeExport
	if err := json.Unmarshal(export, &doc); err != nil {
		return nil, err
	}
	var out []Turn
	for _, m := range doc.Messages {
		info := m.Info
		if info.Role != "assistant" {
			continue
		}
		tk := info.Tokens
		if tk.Input+tk.Output+tk.Cache.Read+tk.Cache.Write == 0 {
			continue
		}
		ms := info.Time.Completed
		if ms == 0 {
			ms = info.Time.Created
		}
		out = append(out, Turn{
			Harness: "opencode", ExternalSessionID: sessionID, Model: info.ModelID,
			Provider: info.ProviderID, At: time.UnixMilli(ms).UTC(),
			Input: tk.Input, CacheRead: tk.Cache.Read, CacheWrite: tk.Cache.Write,
			Output: tk.Output, Reasoning: tk.Reasoning, CostUSD: info.Cost,
		})
	}
	return out, nil
}

// OpenCodeCLI runs opencode subcommands; a variable so tests can substitute canned output.
var OpenCodeCLI = func(ctx context.Context, cwd string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "opencode", args...)
	cmd.Dir = cwd
	return cmd.Output()
}

// ReadOpenCode collects the turns of every OpenCode session for cwd created since a moment.
func ReadOpenCode(ctx context.Context, cwd string, since time.Time) ([]Turn, error) {
	raw, err := OpenCodeCLI(ctx, cwd, "session", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var sessions []openCodeSession
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil, err
	}
	var out []Turn
	for _, s := range sessions {
		if time.UnixMilli(s.Updated).Before(since) {
			continue
		}
		export, err := OpenCodeCLI(ctx, cwd, "export", "--sanitize", s.ID)
		if err != nil {
			continue // a session mid-write; the next poll gets it
		}
		turns, err := OpenCodeTurns(s.ID, export)
		if err != nil {
			continue
		}
		for _, t := range turns {
			if !t.At.Before(since) {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

// Read dispatches to the harness's reader. Unknown harnesses have no usage to report,
// which is not an error: Conductor coordinates tools it cannot meter, too.
func Read(ctx context.Context, harness, cwd string, since time.Time, getenv func(string) string) ([]Turn, error) {
	switch harness {
	case "claude", "claude-code":
		return ReadClaude(ClaudeConfigDir(getenv), cwd, since)
	case "codex":
		return ReadCodex(CodexHome(getenv), cwd, since)
	case "opencode":
		return ReadOpenCode(ctx, cwd, since)
	}
	return nil, nil
}
