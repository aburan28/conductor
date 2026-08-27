package integrations

import (
	_ "embed"
	"strings"
)

// HookCommand is the prefix every hook this package installs starts with. Removal and
// idempotency key on it, so a user's own hooks on the same event are never touched.
const HookCommand = "conductor hook"

// Claude Code hook events this package wires (DESIGN.md §17.4). PreToolUse on the editing
// tools is the enforcement point: a hard conflict blocks the edit before it happens, with
// the holder named in the message the model reads. SessionStart injects the active task and
// any offers as context; SessionEnd closes a bare session's presence record.
var claudeHooks = []struct {
	Event   string
	Matcher string
	Command string
	Timeout int
}{
	{"PreToolUse", "Edit|Write|MultiEdit|NotebookEdit", HookCommand + " pre-tool", 15},
	{"SessionStart", "", HookCommand + " session-start", 15},
	{"SessionEnd", "", HookCommand + " session-end", 10},
}

// mergeClaudeHooks installs (or with remove, uninstalls) Conductor's hooks in a Claude Code
// settings object, leaving every hook that is not ours exactly where it was.
func mergeClaudeHooks(settings map[string]any, remove bool) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		if remove {
			return
		}
		hooks = map[string]any{}
	}

	for _, h := range claudeHooks {
		groups, _ := hooks[h.Event].([]any)
		var kept []any
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			handlers, _ := group["hooks"].([]any)
			var others []any
			for _, hd := range handlers {
				if !isConductorHook(hd) {
					others = append(others, hd)
				}
			}
			if len(others) == len(handlers) {
				kept = append(kept, g) // nothing of ours in here
				continue
			}
			if len(others) > 0 {
				group["hooks"] = others
				kept = append(kept, group)
			}
		}
		if !remove {
			group := map[string]any{
				"hooks": []any{map[string]any{
					"type": "command", "command": h.Command, "timeout": h.Timeout,
				}},
			}
			if h.Matcher != "" {
				group["matcher"] = h.Matcher
			}
			kept = append(kept, group)
		}
		if len(kept) == 0 {
			delete(hooks, h.Event)
		} else {
			hooks[h.Event] = kept
		}
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
}

// claudeHooksInstalled reports whether every Conductor hook is present.
func claudeHooksInstalled(settings map[string]any) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	for _, h := range claudeHooks {
		if h.Event == "SessionEnd" {
			continue // optional: older Claude Code builds have no such event
		}
		found := false
		for _, g := range toSlice(hooks[h.Event]) {
			group, _ := g.(map[string]any)
			for _, hd := range toSlice(group["hooks"]) {
				if isConductorHook(hd) {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func isConductorHook(handler any) bool {
	h, ok := handler.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := h["command"].(string)
	return strings.HasPrefix(strings.TrimSpace(cmd), HookCommand)
}

func toSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// OpenCodePlugin is the plugin source `conductor integrate opencode` installs. It shells out
// to `conductor hook pre-tool` before edit-type tools run and blocks the tool when Conductor
// answers with a hard conflict.
//
//go:embed templates/opencode_plugin.js
var OpenCodePlugin string

// pluginMarker identifies a plugin file this package wrote, so removal never deletes a
// plugin the user wrote under the same name.
const pluginMarker = "conductor-plugin: generated"
