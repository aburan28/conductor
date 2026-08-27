package integrations

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Each tool below follows the same shape: where its configuration lives, what the Conductor
// entry looks like in its dialect, how a bearer token may be referenced, and what the user
// must do afterwards. The dialects were taken from each tool's own documentation; the
// comments note the reference so a future format change has a place to start.

// ---------------------------------------------------------------------------
// Claude Code — https://code.claude.com/docs/en/mcp, /hooks
// ---------------------------------------------------------------------------

func claudeConfigDir(o Options) string {
	if v := o.getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	return filepath.Join(o.Home, ".claude")
}

func detectClaude(o Options) (bool, string) {
	if o.lookPath("claude") {
		return true, "claude on PATH"
	}
	if dirExists(claudeConfigDir(o)) {
		return true, claudeConfigDir(o)
	}
	return false, ""
}

var claudeTool = Tool{
	Name:   "claude",
	Title:  "Claude Code",
	Detect: detectClaude,
	Plan: func(o Options) (Result, error) {
		r := Result{Tool: "claude"}

		// MCP server. Project scope is a committed file, so it carries no token: the stdio
		// gateway reads ~/.conductor/credentials, and the HTTP form references the token
		// through Claude Code's own ${VAR} expansion.
		if o.Global {
			cmd := fmt.Sprintf("claude mcp add --scope user --transport stdio %s -- %s --project %s",
				ServerName, MCPCommand, o.Project)
			if o.transport() == TransportHTTP {
				cmd = fmt.Sprintf(`claude mcp add --scope user --transport http %s %s --header "Authorization: Bearer ${CONDUCTOR_TOKEN}"`,
					ServerName, o.httpURL())
			}
			if o.Remove {
				cmd = "claude mcp remove --scope user " + ServerName
			}
			r.Commands = append(r.Commands, cmd)
			if !o.lookPath("claude") {
				r.Warnings = append(r.Warnings, "claude is not on PATH; run the command above yourself once it is")
			}
		} else {
			entry := map[string]any{"type": "stdio", "command": MCPCommand, "args": o.stdioArgs()}
			if o.transport() == TransportHTTP {
				entry = map[string]any{
					"type": "http", "url": o.httpURL(),
					"headers": map[string]any{"Authorization": "Bearer ${CONDUCTOR_TOKEN}"},
				}
			}
			op, err := planJSON(filepath.Join(o.Root, ".mcp.json"), func(m map[string]any) {
				if o.Remove {
					removeKey(m, "mcpServers", ServerName)
					return
				}
				section(m, "mcpServers")[ServerName] = entry
			}, "MCP server (project scope)")
			if err != nil {
				return r, err
			}
			r.Ops = append(r.Ops, op)
		}

		// Hooks: the enforcement half. Project settings unless --global.
		if o.Hooks {
			path := filepath.Join(o.Root, ".claude", "settings.json")
			if o.Global {
				path = filepath.Join(claudeConfigDir(o), "settings.json")
			}
			op, err := planJSON(path, func(m map[string]any) { mergeClaudeHooks(m, o.Remove) },
				"PreToolUse / SessionStart / SessionEnd hooks")
			if err != nil {
				return r, err
			}
			r.Ops = append(r.Ops, op)
		}
		if o.transport() == TransportHTTP && !o.Remove {
			r.Warnings = append(r.Warnings, "export CONDUCTOR_TOKEN in the shell that launches Claude Code; the config references it, never contains it")
		}
		r.Next = "Restart Claude Code, or run /mcp in a running session, to load the conductor server."
		return r, nil
	},
	Status: func(o Options) Status {
		st := Status{Tool: "claude", Title: "Claude Code", HooksSupported: true, Fix: "conductor integrate claude"}
		st.Detected, _ = detectClaude(o)
		project := filepath.Join(o.Root, ".mcp.json")
		if m, err := readJSONObject(project); err == nil && hasKey(m, "mcpServers", ServerName) {
			st.Configured, st.ConfigPath, st.Transport = true, project, entryTransport(m, "mcpServers", ServerName)
		} else if m, err := readJSONObject(filepath.Join(o.Home, ".claude.json")); err == nil && hasKey(m, "mcpServers", ServerName) {
			st.Configured, st.ConfigPath, st.Transport = true, filepath.Join(o.Home, ".claude.json"), entryTransport(m, "mcpServers", ServerName)
		}
		for _, path := range []string{
			filepath.Join(o.Root, ".claude", "settings.json"),
			filepath.Join(o.Root, ".claude", "settings.local.json"),
			filepath.Join(claudeConfigDir(o), "settings.json"),
		} {
			if m, err := readJSONObject(path); err == nil && claudeHooksInstalled(m) {
				st.Hooks = true
				break
			}
		}
		return st
	},
}

// ClaudeProjectConfigured reports whether the repository's .mcp.json already mounts Conductor,
// so `conductor wrap claude` does not mount it a second time.
func ClaudeProjectConfigured(root string) bool {
	m, err := readJSONObject(filepath.Join(root, ".mcp.json"))
	return err == nil && hasKey(m, "mcpServers", ServerName)
}

// ---------------------------------------------------------------------------
// Cursor — https://cursor.com/docs/context/mcp
// ---------------------------------------------------------------------------

func detectCursor(o Options) (bool, string) {
	if o.lookPath("cursor") {
		return true, "cursor on PATH"
	}
	if dirExists(filepath.Join(o.Home, ".cursor")) {
		return true, filepath.Join(o.Home, ".cursor")
	}
	return false, ""
}

var cursorTool = Tool{
	Name:   "cursor",
	Title:  "Cursor",
	Detect: detectCursor,
	Plan: func(o Options) (Result, error) {
		r := Result{Tool: "cursor"}
		path := filepath.Join(o.Root, ".cursor", "mcp.json")
		if o.Global {
			path = filepath.Join(o.Home, ".cursor", "mcp.json")
		}
		entry := map[string]any{"command": MCPCommand, "args": o.stdioArgs()}
		if o.transport() == TransportHTTP {
			entry = map[string]any{
				"url":     o.httpURL(),
				"headers": map[string]any{"Authorization": "Bearer ${env:CONDUCTOR_TOKEN}"},
			}
		}
		op, err := planJSON(path, func(m map[string]any) {
			if o.Remove {
				removeKey(m, "mcpServers", ServerName)
				return
			}
			section(m, "mcpServers")[ServerName] = entry
		}, "MCP server")
		if err != nil {
			return r, err
		}
		r.Ops = append(r.Ops, op)

		if o.Rules && !o.Global {
			rules := filepath.Join(o.Root, ".cursor", "rules", "conductor.mdc")
			if o.Remove {
				r.Ops = append(r.Ops, planDelete(rules, "rules"))
			} else {
				body := "---\ndescription: Conductor coordination rules\nalwaysApply: true\n---\n\n" +
					strings.TrimSpace(o.ManagedBlock) + "\n"
				r.Ops = append(r.Ops, planWrite(rules, []byte(body), 0o644, "always-on rules"))
			}
		}
		if o.transport() == TransportHTTP && !o.Remove {
			r.Warnings = append(r.Warnings, "export CONDUCTOR_TOKEN before launching Cursor; the config references ${env:CONDUCTOR_TOKEN}")
		}
		r.Next = "Restart Cursor; Settings → MCP should list conductor with its tools enabled."
		return r, nil
	},
	Status: func(o Options) Status {
		st := Status{Tool: "cursor", Title: "Cursor", Fix: "conductor integrate cursor"}
		st.Detected, _ = detectCursor(o)
		for _, path := range []string{filepath.Join(o.Root, ".cursor", "mcp.json"), filepath.Join(o.Home, ".cursor", "mcp.json")} {
			if m, err := readJSONObject(path); err == nil && hasKey(m, "mcpServers", ServerName) {
				st.Configured, st.ConfigPath, st.Transport = true, path, entryTransport(m, "mcpServers", ServerName)
				break
			}
		}
		return st
	},
}

// ---------------------------------------------------------------------------
// Codex — https://developers.openai.com/codex/config-reference (mcp_servers)
// ---------------------------------------------------------------------------

func codexHome(o Options) string {
	if v := o.getenv("CODEX_HOME"); v != "" {
		return v
	}
	return filepath.Join(o.Home, ".codex")
}

const codexTable = "mcp_servers." + ServerName

func detectCodex(o Options) (bool, string) {
	if o.lookPath("codex") {
		return true, "codex on PATH"
	}
	if dirExists(codexHome(o)) {
		return true, codexHome(o)
	}
	return false, ""
}

var codexTool = Tool{
	Name:   "codex",
	Title:  "Codex",
	Detect: detectCodex,
	Plan: func(o Options) (Result, error) {
		r := Result{Tool: "codex"}
		path := filepath.Join(codexHome(o), "config.toml")
		existing := readTextFile(path)

		var content string
		if o.Remove {
			content = removeTOMLTable(existing, codexTable)
		} else {
			body := []string{
				"command = " + tomlString(MCPCommand),
				"args = " + tomlStringArray([]string{"--project", o.Project}),
			}
			if o.transport() == TransportHTTP {
				body = []string{
					"url = " + tomlString(o.httpURL()),
					`bearer_token_env_var = "CONDUCTOR_TOKEN"`,
				}
			}
			content = setTOMLTable(existing, codexTable, body)
		}
		r.Ops = append(r.Ops, planWrite(path, []byte(content), 0o644, "MCP server (Codex has one config, user-level)"))
		if o.transport() == TransportHTTP && !o.Remove {
			r.Warnings = append(r.Warnings, "export CONDUCTOR_TOKEN before launching Codex; bearer_token_env_var names it")
		}
		r.Next = "Restart Codex; `codex mcp list` should show conductor."
		return r, nil
	},
	Status: func(o Options) Status {
		st := Status{Tool: "codex", Title: "Codex", Fix: "conductor integrate codex"}
		st.Detected, _ = detectCodex(o)
		path := filepath.Join(codexHome(o), "config.toml")
		if content := readTextFile(path); hasTOMLTable(content, codexTable) {
			st.Configured, st.ConfigPath, st.Transport = true, path, TransportStdio
			for _, line := range tomlTableBody(content, codexTable) {
				if strings.HasPrefix(strings.TrimSpace(line), "url") {
					st.Transport = TransportHTTP
				}
			}
		}
		return st
	},
}

// CodexConfigured reports whether the user's Codex config already mounts Conductor.
func CodexConfigured(o Options) bool {
	return hasTOMLTable(readTextFile(filepath.Join(codexHome(o), "config.toml")), codexTable)
}

// ---------------------------------------------------------------------------
// OpenCode — https://opencode.ai/docs/mcp-servers/, /plugins/
// ---------------------------------------------------------------------------

func opencodeGlobalDir(o Options) string { return filepath.Join(o.configHome(), "opencode") }

// opencodePluginDir prefers a directory that already exists, since OpenCode has used both
// spellings; new installs get the documented plural.
func opencodePluginDir(base string) string {
	plural := filepath.Join(base, "plugins")
	singular := filepath.Join(base, "plugin")
	if !dirExists(plural) && dirExists(singular) {
		return singular
	}
	return plural
}

func detectOpenCode(o Options) (bool, string) {
	if o.lookPath("opencode") {
		return true, "opencode on PATH"
	}
	if dirExists(opencodeGlobalDir(o)) {
		return true, opencodeGlobalDir(o)
	}
	return false, ""
}

var opencodeTool = Tool{
	Name:   "opencode",
	Title:  "OpenCode",
	Detect: detectOpenCode,
	Plan: func(o Options) (Result, error) {
		r := Result{Tool: "opencode"}
		path := filepath.Join(o.Root, "opencode.json")
		pluginBase := filepath.Join(o.Root, ".opencode")
		if o.Global {
			path = filepath.Join(opencodeGlobalDir(o), "opencode.json")
			pluginBase = opencodeGlobalDir(o)
		}
		// A JSONC config cannot be round-tripped without losing comments.
		if jsonc := strings.TrimSuffix(path, ".json") + ".jsonc"; !fileExists(path) && fileExists(jsonc) {
			path = jsonc
		}

		entry := map[string]any{
			"type": "local", "command": append([]any{MCPCommand}, o.stdioArgs()...), "enabled": true,
		}
		if o.transport() == TransportHTTP {
			entry = map[string]any{
				"type": "remote", "url": o.httpURL(), "enabled": true,
				"headers": map[string]any{"Authorization": "Bearer {env:CONDUCTOR_TOKEN}"},
			}
		}
		creating := !fileExists(path)
		op, err := planJSON(path, func(m map[string]any) {
			if o.Remove {
				removeKey(m, "mcp", ServerName)
				return
			}
			if creating {
				m["$schema"] = "https://opencode.ai/config.json"
			}
			section(m, "mcp")[ServerName] = entry
		}, "MCP server")
		if errors.Is(err, ErrUnparseable) {
			r.Unparseable = path
			r.Snippet = prettyJSON(map[string]any{"mcp": map[string]any{ServerName: entry}})
			return r, nil
		}
		if err != nil {
			return r, err
		}
		r.Ops = append(r.Ops, op)

		if o.Hooks {
			plugin := filepath.Join(opencodePluginDir(pluginBase), "conductor.js")
			if o.Remove {
				if strings.Contains(readTextFile(plugin), pluginMarker) {
					r.Ops = append(r.Ops, planDelete(plugin, "plugin"))
				}
			} else {
				r.Ops = append(r.Ops, planWrite(plugin, []byte(OpenCodePlugin), 0o644, "pre-edit conflict plugin"))
			}
		}
		if o.transport() == TransportHTTP && !o.Remove {
			r.Warnings = append(r.Warnings, "export CONDUCTOR_TOKEN before launching OpenCode; the config references {env:CONDUCTOR_TOKEN}")
		}
		r.Next = "Restart OpenCode; its tool list should include conductor_check_conflicts."
		return r, nil
	},
	Status: func(o Options) Status {
		st := Status{Tool: "opencode", Title: "OpenCode", HooksSupported: true, Fix: "conductor integrate opencode"}
		st.Detected, _ = detectOpenCode(o)
		for _, path := range []string{
			filepath.Join(o.Root, "opencode.json"), filepath.Join(o.Root, "opencode.jsonc"),
			filepath.Join(opencodeGlobalDir(o), "opencode.json"),
		} {
			if m, err := readJSONObject(path); err == nil && hasKey(m, "mcp", ServerName) {
				st.Configured, st.ConfigPath = true, path
				st.Transport = TransportStdio
				if entry, _ := section(m, "mcp")[ServerName].(map[string]any); entry != nil && entry["type"] == "remote" {
					st.Transport = TransportHTTP
				}
				break
			}
		}
		for _, base := range []string{filepath.Join(o.Root, ".opencode"), opencodeGlobalDir(o)} {
			for _, dir := range []string{"plugins", "plugin"} {
				if strings.Contains(readTextFile(filepath.Join(base, dir, "conductor.js")), pluginMarker) {
					st.Hooks = true
				}
			}
		}
		return st
	},
}

// ---------------------------------------------------------------------------
// VS Code (Copilot agent mode) — https://code.visualstudio.com/docs/copilot/customization/mcp-servers
// ---------------------------------------------------------------------------

func detectVSCode(o Options) (bool, string) {
	if o.lookPath("code") {
		return true, "code on PATH"
	}
	if dirExists(filepath.Join(o.Root, ".vscode")) {
		return true, filepath.Join(o.Root, ".vscode")
	}
	return false, ""
}

var vscodeTool = Tool{
	Name:   "vscode",
	Title:  "VS Code",
	Detect: detectVSCode,
	Plan: func(o Options) (Result, error) {
		r := Result{Tool: "vscode"}
		entry := map[string]any{"type": "stdio", "command": MCPCommand, "args": o.stdioArgs()}
		if o.transport() == TransportHTTP {
			entry = map[string]any{
				"type": "http", "url": o.httpURL(),
				"headers": map[string]any{"Authorization": "Bearer ${env:CONDUCTOR_TOKEN}"},
			}
		}
		if o.Global {
			// VS Code's user-level mcp.json is opened through a command, not a fixed path.
			r.Snippet = prettyJSON(map[string]any{"servers": map[string]any{ServerName: entry}})
			r.Warnings = append(r.Warnings, "VS Code's user-level MCP config is edited via the command palette: run \"MCP: Open User Configuration\" and merge the snippet below")
			r.Next = "Then run \"MCP: List Servers\" to confirm conductor is loaded."
			return r, nil
		}
		op, err := planJSON(filepath.Join(o.Root, ".vscode", "mcp.json"), func(m map[string]any) {
			if o.Remove {
				removeKey(m, "servers", ServerName)
				return
			}
			section(m, "servers")[ServerName] = entry
		}, "MCP server (workspace)")
		if err != nil {
			return r, err
		}
		r.Ops = append(r.Ops, op)
		if o.transport() == TransportHTTP && !o.Remove {
			r.Warnings = append(r.Warnings, "export CONDUCTOR_TOKEN before launching VS Code; the config references ${env:CONDUCTOR_TOKEN}")
		}
		r.Next = "Open the workspace; VS Code offers to start the server, or run \"MCP: List Servers\"."
		return r, nil
	},
	Status: func(o Options) Status {
		st := Status{Tool: "vscode", Title: "VS Code", Fix: "conductor integrate vscode"}
		st.Detected, _ = detectVSCode(o)
		path := filepath.Join(o.Root, ".vscode", "mcp.json")
		if m, err := readJSONObject(path); err == nil && hasKey(m, "servers", ServerName) {
			st.Configured, st.ConfigPath, st.Transport = true, path, entryTransport(m, "servers", ServerName)
		}
		return st
	},
}

// ---------------------------------------------------------------------------
// Windsurf — https://docs.windsurf.com/windsurf/cascade/mcp
// ---------------------------------------------------------------------------

func windsurfConfig(o Options) string {
	return filepath.Join(o.Home, ".codeium", "windsurf", "mcp_config.json")
}

func detectWindsurf(o Options) (bool, string) {
	if o.lookPath("windsurf") {
		return true, "windsurf on PATH"
	}
	if dirExists(filepath.Dir(windsurfConfig(o))) {
		return true, filepath.Dir(windsurfConfig(o))
	}
	return false, ""
}

var windsurfTool = Tool{
	Name:   "windsurf",
	Title:  "Windsurf",
	Detect: detectWindsurf,
	Plan: func(o Options) (Result, error) {
		r := Result{Tool: "windsurf"}
		entry := map[string]any{"command": MCPCommand, "args": o.stdioArgs()}
		if o.transport() == TransportHTTP {
			entry = map[string]any{
				"serverUrl": o.httpURL(),
				"headers":   map[string]any{"Authorization": "Bearer ${env:CONDUCTOR_TOKEN}"},
			}
		}
		op, err := planJSON(windsurfConfig(o), func(m map[string]any) {
			if o.Remove {
				removeKey(m, "mcpServers", ServerName)
				return
			}
			section(m, "mcpServers")[ServerName] = entry
		}, "MCP server (Windsurf has one config, user-level)")
		if err != nil {
			return r, err
		}
		r.Ops = append(r.Ops, op)
		if o.transport() == TransportHTTP && !o.Remove {
			r.Warnings = append(r.Warnings, "export CONDUCTOR_TOKEN before launching Windsurf; the config references ${env:CONDUCTOR_TOKEN}")
		}
		r.Next = "In Windsurf, open Cascade → MCP servers and refresh; conductor should be listed."
		return r, nil
	},
	Status: func(o Options) Status {
		st := Status{Tool: "windsurf", Title: "Windsurf", Fix: "conductor integrate windsurf"}
		st.Detected, _ = detectWindsurf(o)
		if m, err := readJSONObject(windsurfConfig(o)); err == nil && hasKey(m, "mcpServers", ServerName) {
			st.Configured, st.ConfigPath, st.Transport = true, windsurfConfig(o), entryTransport(m, "mcpServers", ServerName)
		}
		return st
	},
}

// ---------------------------------------------------------------------------
// Zed — https://zed.dev/docs/ai/mcp
// ---------------------------------------------------------------------------

func zedSettings(o Options) string { return filepath.Join(o.configHome(), "zed", "settings.json") }

func detectZed(o Options) (bool, string) {
	if o.lookPath("zed") {
		return true, "zed on PATH"
	}
	if dirExists(filepath.Dir(zedSettings(o))) {
		return true, filepath.Dir(zedSettings(o))
	}
	return false, ""
}

var zedTool = Tool{
	Name:   "zed",
	Title:  "Zed",
	Detect: detectZed,
	Plan: func(o Options) (Result, error) {
		r := Result{Tool: "zed"}
		entry := map[string]any{"command": MCPCommand, "args": o.stdioArgs(), "env": map[string]any{}}
		if o.transport() == TransportHTTP {
			// Zed expands no variables in settings, so the token itself has to be written.
			// Its settings file is user-level, so it is never committed — but say so.
			if o.Token == "" {
				return r, errors.New("zed: the HTTP transport needs a token in settings.json and none is saved; run `conductor login` first")
			}
			entry = map[string]any{
				"url":     o.httpURL(),
				"headers": map[string]any{"Authorization": "Bearer " + o.Token},
			}
			r.Warnings = append(r.Warnings, "Zed cannot reference an environment variable, so your bearer token is written into "+zedSettings(o)+" (user-level, mode 0600 recommended)")
		}
		op, err := planJSON(zedSettings(o), func(m map[string]any) {
			if o.Remove {
				removeKey(m, "context_servers", ServerName)
				return
			}
			section(m, "context_servers")[ServerName] = entry
		}, "context server (Zed has one settings file, user-level)")
		if errors.Is(err, ErrUnparseable) {
			// Zed settings routinely carry comments and trailing commas. Do not touch them.
			r.Unparseable = zedSettings(o)
			r.Snippet = prettyJSON(map[string]any{"context_servers": map[string]any{ServerName: entry}})
			return r, nil
		}
		if err != nil {
			return r, err
		}
		r.Ops = append(r.Ops, op)
		r.Next = "Zed reloads settings live; open the Agent panel's MCP settings to confirm conductor is running."
		return r, nil
	},
	Status: func(o Options) Status {
		st := Status{Tool: "zed", Title: "Zed", Fix: "conductor integrate zed"}
		st.Detected, _ = detectZed(o)
		if m, err := readJSONObject(zedSettings(o)); err == nil && hasKey(m, "context_servers", ServerName) {
			st.Configured, st.ConfigPath, st.Transport = true, zedSettings(o), entryTransport(m, "context_servers", ServerName)
		} else if strings.Contains(readTextFile(zedSettings(o)), `"`+ServerName+`"`) {
			// JSONC we could not parse; a textual hit is the best answer available.
			st.Configured, st.ConfigPath = true, zedSettings(o)
		}
		return st
	},
}

// ---------------------------------------------------------------------------
// Gemini CLI — https://geminicli.com/docs/tools/mcp-server/
// ---------------------------------------------------------------------------

func detectGemini(o Options) (bool, string) {
	if o.lookPath("gemini") {
		return true, "gemini on PATH"
	}
	if dirExists(filepath.Join(o.Home, ".gemini")) {
		return true, filepath.Join(o.Home, ".gemini")
	}
	return false, ""
}

var geminiTool = Tool{
	Name:   "gemini",
	Title:  "Gemini CLI",
	Detect: detectGemini,
	Plan: func(o Options) (Result, error) {
		r := Result{Tool: "gemini"}
		path := filepath.Join(o.Root, ".gemini", "settings.json")
		if o.Global {
			path = filepath.Join(o.Home, ".gemini", "settings.json")
		}
		entry := map[string]any{"command": MCPCommand, "args": o.stdioArgs()}
		if o.transport() == TransportHTTP {
			// Gemini expands variables only inside `env`, not `headers`, so an HTTP mount
			// needs the literal token — which must never land in a project file.
			if !o.Global {
				return r, errors.New("gemini: the HTTP transport needs the literal token in headers, which must not go into the project's .gemini/settings.json; use --global, or use the stdio transport")
			}
			if o.Token == "" {
				return r, errors.New("gemini: the HTTP transport needs a token and none is saved; run `conductor login` first")
			}
			entry = map[string]any{
				"httpUrl": o.httpURL(),
				"headers": map[string]any{"Authorization": "Bearer " + o.Token},
			}
			r.Warnings = append(r.Warnings, "Gemini cannot reference an environment variable in headers, so your bearer token is written into "+path+" (user-level)")
		}
		op, err := planJSON(path, func(m map[string]any) {
			if o.Remove {
				removeKey(m, "mcpServers", ServerName)
				return
			}
			section(m, "mcpServers")[ServerName] = entry
		}, "MCP server")
		if err != nil {
			return r, err
		}
		r.Ops = append(r.Ops, op)
		r.Next = "Run `gemini` and `/mcp` to confirm conductor is connected."
		return r, nil
	},
	Status: func(o Options) Status {
		st := Status{Tool: "gemini", Title: "Gemini CLI", Fix: "conductor integrate gemini"}
		st.Detected, _ = detectGemini(o)
		for _, path := range []string{filepath.Join(o.Root, ".gemini", "settings.json"), filepath.Join(o.Home, ".gemini", "settings.json")} {
			if m, err := readJSONObject(path); err == nil && hasKey(m, "mcpServers", ServerName) {
				st.Configured, st.ConfigPath, st.Transport = true, path, entryTransport(m, "mcpServers", ServerName)
				break
			}
		}
		return st
	},
}
