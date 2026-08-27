package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/integrations"
)

// lookPath is exec.LookPath, replaceable in tests.
var lookPath = exec.LookPath

// wrapMCPArgs returns the harness flags that mount Conductor's MCP server inside a session
// launched through `conductor wrap`, so the agent can claim, check, and report from within
// its own conversation without anyone configuring anything.
//
// It stays out of the way when the user already handled it: an explicit MCP flag on the
// command line, or a project or user configuration that `conductor integrate` wrote, wins.
// The session id travels in the server's environment so "work offered to this session" is
// answerable from inside the harness.
func wrapMCPArgs(tool string, toolArgs []string, sessionID, project, endpoint string) (extra []string, cleanup func()) {
	noop := func() {}
	mcpPath, err := lookPath("conductor-mcp")
	if err != nil {
		return nil, noop
	}
	root, _ := config.FindRoot(".")
	home, _ := os.UserHomeDir()
	opts := integrations.Options{Root: root, Home: home, Getenv: os.Getenv}

	switch tool {
	case "claude":
		if hasFlag(toolArgs, "--mcp-config") || hasFlag(toolArgs, "--strict-mcp-config") {
			return nil, noop
		}
		if root != "" && integrations.ClaudeProjectConfigured(root) {
			return nil, noop
		}
		args := []string{"--project", project}
		if endpoint != "" {
			args = append(args, "--endpoint", endpoint)
		}
		cfg := map[string]any{"mcpServers": map[string]any{integrations.ServerName: map[string]any{
			"type": "stdio", "command": mcpPath, "args": args,
			"env": map[string]string{"CONDUCTOR_SESSION_ID": sessionID, "CONDUCTOR_PROJECT": project},
		}}}
		body, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return nil, noop
		}
		f, err := os.CreateTemp("", "conductor-mcp-*.json")
		if err != nil {
			return nil, noop
		}
		// 0600: the file names the session, and the harness reads it, nobody else needs to.
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, noop
		}
		if _, err := f.Write(body); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, noop
		}
		f.Close()
		return []string{"--mcp-config", f.Name()}, func() { os.Remove(f.Name()) }

	case "codex":
		for i, a := range toolArgs {
			if (a == "-c" || a == "--config") && i+1 < len(toolArgs) && strings.HasPrefix(toolArgs[i+1], "mcp_servers.") {
				return nil, noop
			}
			if strings.HasPrefix(a, "-c=mcp_servers.") || strings.HasPrefix(a, "--config=mcp_servers.") {
				return nil, noop
			}
		}
		if integrations.CodexConfigured(opts) {
			return nil, noop
		}
		// Codex's -c override takes a dotted key and a value parsed as JSON, falling back to
		// a bare string. The session id reaches conductor-mcp through the inherited
		// environment, which `conductor wrap` already sets.
		args, _ := json.Marshal([]string{"--project", project})
		return []string{
			"-c", "mcp_servers." + integrations.ServerName + ".command=" + mcpPath,
			"-c", "mcp_servers." + integrations.ServerName + ".args=" + string(args),
		}, noop
	}
	return nil, noop
}

// hasFlag reports whether args contain `--name` or `--name=value`.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}
