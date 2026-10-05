package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/usage"
)

// OpenCode keeps sessions in its own database and offers the round trip this package
// wants from every harness: `opencode export <id>` writes the session as JSON
// ({info, messages: [{info, parts}]}) and `opencode import <file>` loads it back, re-homed
// to the directory it is run in. The checkpoint carries the unsanitised export — the
// sanitised one the usage reader asks for has the words removed, which is the opposite of
// what a resume needs — and `opencode --session <id>` reopens it.

// OpenCodeSource is an exported session.
type OpenCodeSource struct {
	SessionID string
	Export    []byte
	Title     string
}

// LocateOpenCode exports a session by id, or the session for cwd most recently updated
// since `since`.
func LocateOpenCode(ctx context.Context, cwd, sessionID string, since time.Time) (OpenCodeSource, error) {
	if sessionID == "" {
		raw, err := usage.OpenCodeCLI(ctx, cwd, "session", "list", "--format", "json")
		if err != nil {
			return OpenCodeSource{}, fmt.Errorf("listing OpenCode sessions: %w", err)
		}
		var sessions []struct {
			ID      string `json:"id"`
			Updated int64  `json:"updated"`
		}
		if err := json.Unmarshal(raw, &sessions); err != nil {
			return OpenCodeSource{}, fmt.Errorf("opencode session list: %w", err)
		}
		var newestAt int64
		for _, s := range sessions {
			if time.UnixMilli(s.Updated).Before(since) {
				continue
			}
			if sessionID == "" || s.Updated > newestAt {
				sessionID, newestAt = s.ID, s.Updated
			}
		}
		if sessionID == "" {
			return OpenCodeSource{}, fmt.Errorf("no OpenCode session for %s has been updated since %s", cwd, since.Format(time.RFC3339))
		}
	}
	export, err := usage.OpenCodeCLI(ctx, cwd, "export", sessionID)
	if err != nil {
		return OpenCodeSource{}, fmt.Errorf("exporting OpenCode session %s: %w", sessionID, err)
	}
	src := OpenCodeSource{SessionID: sessionID, Export: export}
	var head struct {
		Info struct {
			Title string `json:"title"`
		} `json:"info"`
	}
	if json.Unmarshal(export, &head) == nil {
		src.Title = head.Info.Title
	}
	return src, nil
}

// ParseOpenCode builds the neutral conversation from an export.
func ParseOpenCode(data []byte) *Conversation {
	c := &Conversation{Harness: "opencode"}
	var doc struct {
		Info struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Directory string `json:"directory"`
			Version   string `json:"version"`
			Time      struct {
				Created int64 `json:"created"`
				Updated int64 `json:"updated"`
			} `json:"time"`
		} `json:"info"`
		Messages []struct {
			Info struct {
				Role string `json:"role"`
				Time struct {
					Created int64 `json:"created"`
				} `json:"time"`
			} `json:"info"`
			Parts []struct {
				Type  string `json:"type"`
				Text  string `json:"text"`
				Tool  string `json:"tool"`
				State struct {
					Status string         `json:"status"`
					Input  map[string]any `json:"input"`
					Title  string         `json:"title"`
				} `json:"state"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return c
	}
	c.SessionID, c.Title, c.Cwd, c.Version = doc.Info.ID, doc.Info.Title, doc.Info.Directory, doc.Info.Version
	if doc.Info.Time.Created > 0 {
		c.Started = time.UnixMilli(doc.Info.Time.Created).UTC()
	}
	if doc.Info.Time.Updated > 0 {
		c.Last = time.UnixMilli(doc.Info.Time.Updated).UTC()
	}
	c.Records = len(doc.Messages)
	for _, m := range doc.Messages {
		t := Turn{Role: m.Info.Role}
		if m.Info.Time.Created > 0 {
			t.At = time.UnixMilli(m.Info.Time.Created).UTC()
		}
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				t.Text = joinText(t.Text, p.Text)
			case "tool":
				summary, path := ToolSummary(p.Tool, p.State.Input)
				if summary == "" {
					summary = p.State.Title
				}
				t.Tools = append(t.Tools, ToolCall{Name: p.Tool, Summary: summary, Path: path, Error: p.State.Status == "error"})
			}
		}
		if t.Role == "user" && isInjectedContext(strings.TrimSpace(t.Text)) {
			continue
		}
		if t.Role != "user" && t.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(t.Text) == "" && len(t.Tools) == 0 {
			continue
		}
		c.Turns = append(c.Turns, t)
	}
	return c
}

// InstallOpenCode imports a bundle's export into the OpenCode instance for cwd.
func InstallOpenCode(ctx context.Context, b *Bundle, cwd string) (string, error) {
	m := b.Manifest
	data, ok := b.File(m.Transcript.Path)
	if !ok {
		return "", errors.New("the checkpoint carries no transcript")
	}
	tmp, err := os.CreateTemp("", "conductor-opencode-*.json")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	if _, err := usage.OpenCodeCLI(ctx, cwd, "import", name); err != nil {
		return "", fmt.Errorf("opencode import: %w", err)
	}
	return m.SessionID, nil
}
