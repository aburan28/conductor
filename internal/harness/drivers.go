package harness

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/adamburan/conductor/internal/domain"
)

// HarnessConfig lets an operator override how a harness is invoked without a code change.
//
// This matters more than it looks. Agent CLIs move fast, and a flag that is correct today
// may be renamed next month. Making the argument vector configuration rather than compiled-in
// means a harness upgrade is a YAML edit, not a Conductor release (DESIGN.md §4 vendor
// neutrality).
type HarnessConfig struct {
	Enabled   bool     `yaml:"enabled" json:"enabled"`
	Command   string   `yaml:"command" json:"command"`
	ExtraArgs []string `yaml:"extra_args" json:"extra_args"`
	// ArgTemplate, when set, fully replaces the built-in argument vector. Supported
	// placeholders: {model} {effort} {max_turns} {cwd} {branch} {mcp_config} {task_card}
	// {permission_mode} {instruction}
	ArgTemplate []string `yaml:"arg_template" json:"arg_template"`
	// StdinInstruction sends the task card on stdin rather than as an argument.
	StdinInstruction *bool `yaml:"stdin_instruction" json:"stdin_instruction"`
}

// NewClaudeDriver drives Claude Code in headless mode.
//
// `-p` with `--output-format stream-json` is the documented non-interactive path, and
// `--verbose` is required for the streaming form. The instruction goes on stdin because a
// task card plus workflow rules routinely exceeds a comfortable argv length.
func NewClaudeDriver(cfg HarnessConfig) *ProcessDriver {
	command := cfg.Command
	if command == "" {
		command = "claude"
	}
	return &ProcessDriver{
		Kind:             "claude",
		Command:          command,
		VersionArgs:      []string{"--version"},
		Adapt:            claudeAdapter,
		StdinInstruction: stdinDefault(cfg, true),
		Caps: Capabilities{
			Kind: "claude", SupportsMCP: true, SupportsJSON: true,
			SupportsEffort: false, SupportsResume: true,
		},
		Args: func(spec RunSpec) []string {
			if len(cfg.ArgTemplate) > 0 {
				return expandTemplate(cfg.ArgTemplate, spec)
			}
			args := []string{"-p", "--output-format", "stream-json", "--verbose"}
			if spec.Model != "" {
				args = append(args, "--model", spec.Model)
			}
			if spec.PermissionMode != "" {
				args = append(args, "--permission-mode", spec.PermissionMode)
			}
			if spec.MCPConfigPath != "" {
				// Mounting the Conductor MCP server inside the session is what lets the
				// agent claim, reserve, and report from within its own run.
				args = append(args, "--mcp-config", spec.MCPConfigPath)
			}
			if spec.MaxTurns > 0 {
				args = append(args, "--max-turns", strconv.Itoa(spec.MaxTurns))
			}
			return append(args, cfg.ExtraArgs...)
		},
	}
}

// NewCodexDriver drives Codex through `codex exec --json`.
//
// DESIGN.md §16.3 prefers the App Server for full lifecycle control; `codex exec` is the
// simpler one-shot path and is what this build implements. The App Server's bidirectional
// JSON-RPC is a natural second driver behind the same interface — that is the point of the
// interface — but shipping an unverified binding would be worse than shipping the simple one.
func NewCodexDriver(cfg HarnessConfig) *ProcessDriver {
	command := cfg.Command
	if command == "" {
		command = "codex"
	}
	return &ProcessDriver{
		Kind:             "codex",
		Command:          command,
		VersionArgs:      []string{"--version"},
		Adapt:            genericAdapter,
		StdinInstruction: stdinDefault(cfg, true),
		Caps: Capabilities{
			Kind: "codex", SupportsMCP: true, SupportsJSON: true, SupportsEffort: true,
		},
		Args: func(spec RunSpec) []string {
			if len(cfg.ArgTemplate) > 0 {
				return expandTemplate(cfg.ArgTemplate, spec)
			}
			args := []string{"exec", "--json"}
			if spec.Model != "" {
				args = append(args, "-m", spec.Model)
			}
			if spec.Effort != "" && spec.Effort != domain.EffortNone {
				args = append(args, "-c", "model_reasoning_effort="+string(spec.Effort))
			}
			return append(args, cfg.ExtraArgs...)
		},
	}
}

// NewOpenCodeDriver drives OpenCode's non-interactive run mode.
//
// `--format json` is what makes the run meterable: without it stdout is the final
// assistant text and no usage ever reaches the adapter. OpenCode addresses models as
// provider/model, which is why the router stores the resolved model as an opaque string
// rather than splitting provider out.
func NewOpenCodeDriver(cfg HarnessConfig) *ProcessDriver {
	command := cfg.Command
	if command == "" {
		command = "opencode"
	}
	return &ProcessDriver{
		Kind:             "opencode",
		Command:          command,
		VersionArgs:      []string{"--version"},
		Adapt:            opencodeAdapter,
		StdinInstruction: stdinDefault(cfg, true),
		Caps: Capabilities{
			Kind: "opencode", SupportsMCP: true, SupportsJSON: true, SupportsEffort: true,
		},
		Args: func(spec RunSpec) []string {
			if len(cfg.ArgTemplate) > 0 {
				return expandTemplate(cfg.ArgTemplate, spec)
			}
			args := []string{"run", "--print-logs", "--format", "json"}
			if spec.Model != "" {
				args = append(args, "--model", spec.Model)
			}
			return append(args, cfg.ExtraArgs...)
		},
	}
}

// NewExecDriver is the vendor-neutral escape hatch of DESIGN.md §16.5: any command, driven
// by a template. A new agent runtime needs a config block, not a pull request.
func NewExecDriver(name string, cfg HarnessConfig) *ProcessDriver {
	command := cfg.Command
	if command == "" {
		command = name
	}
	return &ProcessDriver{
		Kind:             name,
		Command:          command,
		Adapt:            genericAdapter,
		StdinInstruction: stdinDefault(cfg, true),
		Caps:             Capabilities{Kind: name, SupportsJSON: true},
		Args: func(spec RunSpec) []string {
			if len(cfg.ArgTemplate) > 0 {
				return expandTemplate(cfg.ArgTemplate, spec)
			}
			return cfg.ExtraArgs
		},
	}
}

func stdinDefault(cfg HarnessConfig, fallback bool) bool {
	if cfg.StdinInstruction != nil {
		return *cfg.StdinInstruction
	}
	return fallback
}

// expandTemplate substitutes run-spec values into an operator-supplied argument vector.
//
// Substitution is per-argument and never re-splits, so a template entry containing spaces
// stays one argument. That avoids the classic shell-injection shape where an instruction
// containing a quote turns into extra arguments.
func expandTemplate(tmpl []string, spec RunSpec) []string {
	repl := strings.NewReplacer(
		"{model}", spec.Model,
		"{effort}", string(spec.Effort),
		"{max_turns}", strconv.Itoa(spec.MaxTurns),
		"{cwd}", spec.Cwd,
		"{branch}", spec.Branch,
		"{mcp_config}", spec.MCPConfigPath,
		"{task_card}", spec.TaskCardPath,
		"{permission_mode}", spec.PermissionMode,
		"{task_ref}", spec.TaskRef,
		"{instruction}", spec.Instruction,
	)
	out := make([]string, 0, len(tmpl))
	for _, arg := range tmpl {
		expanded := repl.Replace(arg)
		// Drop arguments whose only content was an empty placeholder, so an unset model
		// does not produce a stray empty argv entry.
		if strings.TrimSpace(expanded) == "" && strings.TrimSpace(arg) != "" {
			continue
		}
		out = append(out, expanded)
	}
	return out
}

// BuildRegistry wires the configured harnesses plus the built-in fake.
func BuildRegistry(configs map[string]HarnessConfig) *Registry {
	reg := NewRegistry()

	// The fake driver is always registered. DESIGN.md §36.2 is explicit: build the CLI and a
	// fake harness before integrating any model provider, so coordination correctness can be
	// proven without spending a token or depending on a vendor being installed.
	reg.Register(NewFakeDriver())

	for name, cfg := range configs {
		if !cfg.Enabled {
			continue
		}
		switch name {
		case "claude":
			reg.Register(NewClaudeDriver(cfg))
		case "codex":
			reg.Register(NewCodexDriver(cfg))
		case "opencode":
			reg.Register(NewOpenCodeDriver(cfg))
		case "fake":
			// already registered
		default:
			reg.Register(NewExecDriver(name, cfg))
		}
	}
	return reg
}

// DefaultHarnessConfigs is what a project gets before it declares its own.
func DefaultHarnessConfigs() map[string]HarnessConfig {
	return map[string]HarnessConfig{
		"claude":   {Enabled: true, Command: "claude"},
		"codex":    {Enabled: false, Command: "codex"},
		"opencode": {Enabled: false, Command: "opencode"},
	}
}

// Describe renders a capability report as a short human-readable table.
func Describe(caps []Capabilities) string {
	var b strings.Builder
	for _, c := range caps {
		status := "not installed"
		if c.Available {
			status = "available"
			if c.Version != "" {
				status += " (" + c.Version + ")"
			}
		} else if c.Detail != "" {
			status = c.Detail
		}
		fmt.Fprintf(&b, "  %-10s %s\n", c.Kind, status)
	}
	return b.String()
}
