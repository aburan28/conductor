package integrations

import (
	_ "embed"
	"strings"
)

// HookCommand is the prefix every hook this package installs starts with. Removal matches
// known full commands, leaving unrelated user hooks on the same event alone.
const HookCommand = "conductor hook"

// Claude Code hook events this package wires (DESIGN.md §17.4). PreToolUse on the editing
// tools is the enforcement point: a hard conflict blocks the edit before it happens, with
// the holder named in the message the model reads. SessionStart injects the active task and
// any offers as context; SessionEnd closes a bare session's presence record.
var claudeHooks = []hookSpec{
	// --auto-reserve: a file edited outside the session's claimed scope is reserved under
	// that claim on first edit, so scope drift is visible to teammates instead of silent.
	{"PreToolUse", "Edit|Write|MultiEdit|NotebookEdit", HookCommand + " pre-tool --auto-reserve", 15},
	{"SessionStart", "", HookCommand + " session-start", 15},
	{"SessionEnd", "", HookCommand + " session-end", 30},
	// Portability: a checkpoint after each turn (rate-limited and skipped when nothing
	// changed) and one right before compaction, when the transcript is at its richest.
	{"Stop", "", HookCommand + " checkpoint", 30},
	{"PreCompact", "", HookCommand + " checkpoint", 30},
	{"PostToolUse", "", HookCommand + " memory-observe --harness claude", 5},
	{"PostToolUseFailure", "", HookCommand + " memory-observe --harness claude", 5},
	{"Stop", "", HookCommand + " memory-observe --harness claude", 5},
	{"UserPromptSubmit", "", HookCommand + " memory-context", 5},
}

// hookSpec is one hook a harness runs: on Event, for tools matching Matcher, run Command.
type hookSpec struct {
	Event   string
	Matcher string
	Command string
	Timeout int
}

// codexHooks are the same hooks for Codex, whose hook system mirrors Claude Code's events
// (https://developers.openai.com/codex/hooks). Three differences shape this list. Codex
// edits files through apply_patch, so the PreToolUse matcher names it (Edit and Write are
// accepted aliases for it). Every command says --harness codex: the checkpoint has to look
// for a Codex transcript, and pre-tool has to answer in the output shape Codex accepts.
// And SessionEnd is capped at three seconds, so it gets three.
var codexHooks = []hookSpec{
	{"PreToolUse", "^(apply_patch|Edit|Write)$", HookCommand + " pre-tool --auto-reserve --harness codex", 15},
	{"SessionStart", "", HookCommand + " session-start", 15},
	{"SessionEnd", "", HookCommand + " session-end --harness codex", 3},
	{"Stop", "", HookCommand + " checkpoint --harness codex", 30},
	{"PreCompact", "", HookCommand + " checkpoint --harness codex", 30},
	{"PostToolUse", "", HookCommand + " memory-observe --harness codex", 5},
	{"Stop", "", HookCommand + " memory-observe --harness codex", 5},
	{"UserPromptSubmit", "", HookCommand + " memory-context", 5},
}

// Older builds can omit these coordination events. Memory's Stop capture is required.
func optionalHook(h hookSpec) bool {
	return h.Event == "SessionEnd" || h.Event == "PreCompact" ||
		strings.HasPrefix(h.Command, HookCommand+" checkpoint")
}

// mergeClaudeHooks installs (or with remove, uninstalls) Conductor's hooks in a Claude Code
// settings object, leaving every hook that is not ours exactly where it was.
func mergeClaudeHooks(settings map[string]any, remove bool) {
	mergeHooks(settings, claudeHooks, remove)
}

// mergeCodexHooks does the same for a Codex hooks.json, which has the same shape.
func mergeCodexHooks(settings map[string]any, remove bool) { mergeHooks(settings, codexHooks, remove) }

func mergeHooks(settings map[string]any, specs []hookSpec, remove bool) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		if remove {
			return
		}
		hooks = map[string]any{}
	}
	byEvent := make(map[string][]hookSpec)
	var events []string
	for _, h := range specs {
		if len(byEvent[h.Event]) == 0 {
			events = append(events, h.Event)
		}
		byEvent[h.Event] = append(byEvent[h.Event], h)
	}
	for _, event := range events {
		groups, _ := hooks[event].([]any)
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
				if !isManagedHook(hd) {
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
			for _, h := range byEvent[event] {
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
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
}

// claudeHooksInstalled reports whether every Conductor hook is present.
func claudeHooksInstalled(settings map[string]any) bool { return hooksInstalled(settings, claudeHooks) }

// codexHooksInstalled reports the same for a Codex hooks.json.
func codexHooksInstalled(settings map[string]any) bool { return hooksInstalled(settings, codexHooks) }

func hooksInstalled(settings map[string]any, specs []hookSpec) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	for _, h := range specs {
		if optionalHook(h) {
			continue
		}
		found := false
		for _, g := range toSlice(hooks[h.Event]) {
			group, _ := g.(map[string]any)
			for _, hd := range toSlice(group["hooks"]) {
				if hookCommand(hd) == h.Command {
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

func hookCommand(handler any) string {
	h, ok := handler.(map[string]any)
	if !ok {
		return ""
	}
	cmd, _ := h["command"].(string)
	return strings.TrimSpace(cmd)
}

func isManagedHook(handler any) bool {
	cmd := hookCommand(handler)
	if cmd == HookCommand+" pre-tool" { // old installs before --auto-reserve
		return true
	}
	for _, specs := range [][]hookSpec{claudeHooks, codexHooks} {
		for _, h := range specs {
			if cmd == h.Command {
				return true
			}
		}
	}
	return false
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
