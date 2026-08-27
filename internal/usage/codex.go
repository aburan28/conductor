package usage

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Codex writes one rollout log per session under $CODEX_HOME/sessions/YYYY/MM/DD/
// rollout-<time>-<id>.jsonl. A session_meta record names the session and its cwd; each
// turn_context names the model and effort in force; and a token_count event after every
// response carries the running total for the session (info.total_token_usage). The totals are
// what this reader trusts: differencing consecutive totals yields each response's usage
// exactly once even if the same event is logged twice, which a per-event field could not.

type codexRecord struct {
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexSessionMeta struct {
	ID            string `json:"id"`
	Cwd           string `json:"cwd"`
	ModelProvider string `json:"model_provider"`
}

type codexTurnContext struct {
	Model           string `json:"model"`
	Effort          string `json:"effort"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type codexTokenTotals struct {
	Input      int64 `json:"input_tokens"`
	CacheRead  int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
}

type codexTokenCount struct {
	Type string `json:"type"`
	Info *struct {
		Total codexTokenTotals `json:"total_token_usage"`
	} `json:"info"`
}

// CodexMeta reads just the session_meta record of a rollout: enough to decide whether the
// session belongs to a working directory without parsing the rest.
func CodexMeta(r io.Reader) (codexSessionMeta, bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var rec codexRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "session_meta" {
			continue
		}
		var meta codexSessionMeta
		if json.Unmarshal(rec.Payload, &meta) == nil && meta.ID != "" {
			return meta, true
		}
	}
	return codexSessionMeta{}, false
}

// CodexTurns parses a rollout log into turns.
func CodexTurns(r io.Reader) ([]Turn, error) {
	var out []Turn
	var meta codexSessionMeta
	var ctx codexTurnContext
	var prev codexTokenTotals
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var rec codexRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		switch rec.Type {
		case "session_meta":
			_ = json.Unmarshal(rec.Payload, &meta)
		case "turn_context":
			_ = json.Unmarshal(rec.Payload, &ctx)
		case "event_msg":
			var ev codexTokenCount
			if json.Unmarshal(rec.Payload, &ev) != nil || ev.Type != "token_count" || ev.Info == nil {
				continue
			}
			cur := ev.Info.Total
			d := codexTokenTotals{
				Input: cur.Input - prev.Input, CacheRead: cur.CacheRead - prev.CacheRead,
				CacheWrite: cur.CacheWrite - prev.CacheWrite, Output: cur.Output - prev.Output,
				Reasoning: cur.Reasoning - prev.Reasoning,
			}
			if d.Input < 0 || d.Output < 0 || d.CacheRead < 0 {
				d = cur // the counter reset; count what is there now
			}
			prev = cur
			if d.Input+d.CacheRead+d.CacheWrite+d.Output == 0 {
				continue
			}
			effort := ctx.ReasoningEffort
			if effort == "" {
				effort = ctx.Effort
			}
			provider := meta.ModelProvider
			if provider == "" {
				provider = "openai"
			}
			// Codex reports cached tokens as part of input; keep Input as the uncached share so
			// the columns mean the same thing across harnesses.
			out = append(out, Turn{
				Harness: "codex", ExternalSessionID: meta.ID, Model: ctx.Model, Provider: provider,
				Effort: effort, At: rec.Timestamp,
				Input: d.Input - d.CacheRead, CacheRead: d.CacheRead, CacheWrite: d.CacheWrite,
				Output: d.Output, Reasoning: d.Reasoning,
			})
		}
	}
	return out, sc.Err()
}

// CodexHome honors CODEX_HOME, defaulting to ~/.codex.
func CodexHome(getenv func(string) string) string {
	if v := getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// CodexSessionFiles lists the rollouts for cwd written to since a moment. Rollouts are laid
// out by day, so only the days from `since` on are opened.
func CodexSessionFiles(codexHome, cwd string, since time.Time) ([]string, error) {
	root := filepath.Join(codexHome, "sessions")
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		meta, ok := CodexMeta(f)
		f.Close()
		if ok && (cwd == "" || meta.Cwd == cwd) {
			out = append(out, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return out, nil
}

// ReadCodex collects the turns of every Codex session for cwd active since a moment.
func ReadCodex(codexHome, cwd string, since time.Time) ([]Turn, error) {
	files, err := CodexSessionFiles(codexHome, cwd, since)
	if err != nil {
		return nil, err
	}
	var out []Turn
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		turns, err := CodexTurns(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		for _, t := range turns {
			if !t.At.Before(since) {
				out = append(out, t)
			}
		}
	}
	return out, nil
}
