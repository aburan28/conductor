package main

// Harness registry construction from repository configuration.
//
// harness.BuildRegistry has always ended in `default: reg.Register(NewExecDriver(name, cfg))`
// -- the vendor-neutral escape hatch of DESIGN.md §16.5, whose promise is that "a new agent
// runtime needs a config block, not a pull request." Until this file, that branch was
// unreachable: every caller passed harness.DefaultHarnessConfigs(), a hardcoded map of the
// three known harnesses, and no YAML was ever consulted. The config block had nowhere to go.
//
// This is what reads it.

import (
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/harness"
)

// buildHarnessRegistry returns the registry a repository asks for, falling back to the
// built-in defaults when it asks for nothing.
//
// repoPath may be empty, and the .conductor directory may be absent or unreadable: a harness
// registry is needed on machines that have no checkout at all (`conductor doctor` is expected
// to work from anywhere), so every failure here degrades to the defaults rather than
// propagating. A malformed project.yaml is already reported by the commands that parse it for
// coordination settings; failing a second time here would only add noise.
func buildHarnessRegistry(repoPath string) *harness.Registry {
	return harness.BuildRegistry(harnessConfigs(repoPath))
}

// harnessConfigs merges a repository's `harnesses:` block over the built-in defaults.
//
// Merging rather than replacing is what lets a repository add a harness without restating
// claude and opencode, or disable one with `enabled: false` without deleting the others. A
// declared entry replaces the default for that name entirely -- a half-overridden command
// vector is harder to reason about than a whole one.
func harnessConfigs(repoPath string) map[string]harness.HarnessConfig {
	configs := harness.DefaultHarnessConfigs()
	if repoPath == "" {
		return configs
	}
	bundle, err := config.Load(repoPath)
	if err != nil {
		return configs
	}
	for name, spec := range bundle.Project.Harnesses {
		if name == "" {
			continue
		}
		configs[name] = harness.HarnessConfig{
			Enabled:          spec.Enabled,
			Command:          spec.Command,
			ExtraArgs:        spec.ExtraArgs,
			ArgTemplate:      spec.ArgTemplate,
			StdinInstruction: spec.StdinInstruction,
			MCPServers:       mcpServers(spec.MCPServers),
		}
	}
	return configs
}

func mcpServers(specs map[string]config.MCPServerSpec) map[string]harness.MCPServer {
	if len(specs) == 0 {
		return nil
	}
	out := make(map[string]harness.MCPServer, len(specs))
	for name, s := range specs {
		out[name] = harness.MCPServer{Command: s.Command, Args: s.Args, Env: s.Env}
	}
	return out
}

// harnessMCPServers is the runner's view: extra MCP servers keyed by the harness that gets
// them. Harnesses declaring none are omitted rather than mapped to an empty map, so the
// runner's lookup misses cheaply for the common case.
func harnessMCPServers(configs map[string]harness.HarnessConfig) map[string]map[string]harness.MCPServer {
	var out map[string]map[string]harness.MCPServer
	for name, cfg := range configs {
		if !cfg.Enabled || len(cfg.MCPServers) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]map[string]harness.MCPServer)
		}
		out[name] = cfg.MCPServers
	}
	return out
}
