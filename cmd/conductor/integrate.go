package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/integrations"
)

// cmdIntegrate connects a coding tool to this project: it writes Conductor's MCP server into
// the tool's own configuration and, where the tool can run hooks, installs the pre-edit
// conflict check (DESIGN.md §17). One command per tool, idempotent, and never a bearer token
// in a file that gets committed.
func cmdIntegrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("integrate", flag.ExitOnError)
	global := fs.Bool("global", false, "write the user-level configuration instead of the project's")
	transport := fs.String("transport", integrations.TransportStdio, "how the tool reaches Conductor: stdio (conductor-mcp) or http (<endpoint>/mcp)")
	endpoint := fs.String("endpoint", "", "control plane URL for the http transport (default: your saved login)")
	project := fs.String("project", "", "project id or slug")
	printOnly := fs.Bool("print", false, "show what would be written without writing it")
	remove := fs.Bool("remove", false, "disconnect the tool instead")
	noHooks := fs.Bool("no-hooks", false, "skip enforcement hooks and plugins")
	noRules := fs.Bool("no-rules", false, "skip instruction files (CLAUDE.md, AGENTS.md, Cursor rules)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `conductor integrate — connect a coding tool to this project

Writes Conductor's MCP server into the tool's configuration and installs its hooks where
the tool has them, so every session on this repository checks for conflicts before it
edits. Safe to re-run; --remove undoes it.

  conductor integrate claude              .mcp.json + PreToolUse hook in .claude/settings.json
  conductor integrate cursor              .cursor/mcp.json + .cursor/rules/conductor.mdc
  conductor integrate codex               ~/.codex/config.toml [mcp_servers.conductor]
  conductor integrate opencode            opencode.json + .opencode/plugins/conductor.js
  conductor integrate all                 every tool found on this machine
  conductor integrate claude --transport http --print

Tools: %s

Project-level files never contain your token: the stdio gateway reads your saved login, and
the http form references $CONDUCTOR_TOKEN through the tool's own variable expansion.

Flags:
`, strings.Join(integrations.Names(), ", "))
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return fmt.Errorf("usage: conductor integrate <%s|all>", strings.Join(integrations.Names(), "|"))
	}

	opts, err := integrationOptions(*global, *transport, *endpoint, *project, !*noHooks, !*noRules, *remove)
	if err != nil {
		return err
	}

	var tools []integrations.Tool
	if positional[0] == "all" {
		tools = integrations.Detected(opts)
		if len(tools) == 0 {
			return errors.New("no supported tool detected on this machine; name one explicitly: " +
				strings.Join(integrations.Names(), ", "))
		}
	} else {
		for _, name := range positional {
			tool, ok := integrations.Lookup(name)
			if !ok {
				return fmt.Errorf("unknown tool %q; choose from %s or all", name, strings.Join(integrations.Names(), ", "))
			}
			tools = append(tools, tool)
		}
	}

	var results []integrations.Result
	for _, tool := range tools {
		res, err := tool.Plan(opts)
		if err != nil {
			return fmt.Errorf("%s: %w", tool.Title, err)
		}
		results = append(results, res)
	}

	if *printOnly {
		if *asJSON {
			return emit(map[string]any{"applied": false, "results": results})
		}
		for _, res := range results {
			printIntegrationPlan(opts, res, true)
		}
		return nil
	}

	failed := 0
	for _, res := range results {
		if res.Unparseable != "" {
			failed++
			continue
		}
		if err := integrations.Apply(res, func(cmd string) error { return runIntegrationCommand(ctx, cmd) }); err != nil {
			return fmt.Errorf("%s: %w", res.Tool, err)
		}
	}

	// Instruction files. These are the same managed block `conductor init` writes, refreshed
	// here so a tool that reads them sees the current rules. Removal leaves them: the block
	// is advice, not configuration, and deleting a user's CLAUDE.md content is not ours to do.
	if !*remove && !*noRules && !*global {
		for _, res := range results {
			switch res.Tool {
			case "claude":
				if err := ensureManagedBlock(filepath.Join(opts.Root, "CLAUDE.md")); err != nil {
					return err
				}
			case "codex":
				if err := ensureManagedBlock(filepath.Join(opts.Root, "AGENTS.md")); err != nil {
					return err
				}
			}
		}
	}

	if *asJSON {
		return emit(map[string]any{"applied": true, "results": results, "failed": failed})
	}
	for _, res := range results {
		printIntegrationPlan(opts, res, false)
	}
	if failed > 0 {
		return fmt.Errorf("%d tool(s) need the snippet above merged by hand", failed)
	}
	return nil
}

// integrationOptions assembles what every tool writer needs from the login, the repository,
// and the flags.
func integrationOptions(global bool, transport, endpoint, project string, hooks, rules, remove bool) (integrations.Options, error) {
	switch transport {
	case integrations.TransportStdio, integrations.TransportHTTP:
	default:
		return integrations.Options{}, fmt.Errorf("unknown transport %q: use stdio or http", transport)
	}
	creds := client.LoadCredentials()
	if endpoint == "" {
		endpoint = creds.Endpoint
	}
	ref, err := projectRef(project, creds)
	if err != nil {
		return integrations.Options{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return integrations.Options{}, err
	}
	root, err := config.FindRoot(".")
	if err != nil {
		if !global {
			return integrations.Options{}, fmt.Errorf("%w (run from inside the repository, or pass --global)", err)
		}
		root, _ = os.Getwd()
	}
	return integrations.Options{
		Root: root, Home: home, Project: ref, Endpoint: endpoint, Token: creds.Token,
		Transport: transport, Global: global, Hooks: hooks, Rules: rules, Remove: remove,
		ManagedBlock: managedBlockBody(),
	}, nil
}

// managedBlockBody is the instruction text without its HTML markers, for rules files that
// have their own framing.
func managedBlockBody() string {
	body := strings.TrimPrefix(managedBlock, managedBegin)
	body = strings.TrimSuffix(body, managedEnd)
	return strings.TrimSpace(body)
}

// runIntegrationCommand executes one of a plan's shell commands (only `claude mcp add`
// today), or tells the user to when the binary is not here.
func runIntegrationCommand(ctx context.Context, cmd string) error {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return nil
	}
	if _, err := exec.LookPath(fields[0]); err != nil {
		fmt.Printf("  run this yourself once %s is installed:\n    %s\n", fields[0], cmd)
		return nil
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w", fields[0], err)
	}
	return nil
}

// printIntegrationPlan renders one tool's outcome.
func printIntegrationPlan(opts integrations.Options, res integrations.Result, dryRun bool) {
	tool, _ := integrations.Lookup(res.Tool)
	title := tool.Title
	if title == "" {
		title = res.Tool
	}
	fmt.Printf("%s\n", title)
	for _, op := range res.Ops {
		fmt.Printf("  %-9s %-40s %s\n", op.Action, displayPath(opts, op.Path), op.Note)
		if dryRun && (op.Action == "create" || op.Action == "update") {
			for _, line := range strings.Split(strings.TrimRight(string(op.Content), "\n"), "\n") {
				fmt.Printf("            │ %s\n", line)
			}
		}
	}
	for _, cmd := range res.Commands {
		if dryRun {
			fmt.Printf("  %-9s %s\n", "run", cmd)
		} else {
			fmt.Printf("  %-9s %s\n", "ran", cmd)
		}
	}
	if res.Unparseable != "" {
		fmt.Printf("  %-9s %s could not be parsed (comments or trailing commas?). Merge this by hand:\n",
			"manual", displayPath(opts, res.Unparseable))
		for _, line := range strings.Split(res.Snippet, "\n") {
			fmt.Printf("            │ %s\n", line)
		}
	} else if res.Snippet != "" {
		for _, line := range strings.Split(res.Snippet, "\n") {
			fmt.Printf("            │ %s\n", line)
		}
	}
	for _, w := range res.Warnings {
		fmt.Printf("  %-9s %s\n", "note", w)
	}
	if res.Next != "" && !dryRun && !opts.Remove {
		fmt.Printf("  %-9s %s\n", "next", res.Next)
	}
	fmt.Println()
}

// displayPath shortens a path for a terminal: repository-relative inside the repo, ~ inside
// the home directory.
func displayPath(opts integrations.Options, path string) string {
	if opts.Root != "" {
		if rel, err := filepath.Rel(opts.Root, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	if opts.Home != "" {
		if rel, err := filepath.Rel(opts.Home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return path
}
