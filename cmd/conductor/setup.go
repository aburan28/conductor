package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/harness"
	"github.com/adamburan/conductor/internal/integrations"
)

// ---------------------------------------------------------------------------
// init
// ---------------------------------------------------------------------------

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", ".", "repository root")
	force := fs.Bool("force", false, "overwrite existing policy files")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	target := filepath.Join(root, config.Dir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}

	files := map[string]string{
		"project.yaml":  scaffoldProject(filepath.Base(root)),
		"policies.yaml": scaffoldPolicies,
		"models.yaml":   scaffoldModels,
		"dispatch.yaml": scaffoldDispatch,
		"WORKFLOW.md":   scaffoldWorkflow,
	}
	for name, body := range files {
		path := filepath.Join(target, name)
		if _, err := os.Stat(path); err == nil && !*force {
			fmt.Printf("  skip   %s (exists)\n", filepath.Join(config.Dir, name))
			continue
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Printf("  write  %s\n", filepath.Join(config.Dir, name))
	}

	// The generated and runtime directories are working state, not source.
	gitignore := filepath.Join(target, ".gitignore")
	if err := os.WriteFile(gitignore, []byte("generated/\nruntime/\n"), 0o644); err != nil {
		return err
	}

	// A managed block in the agent instruction files, inserted without disturbing whatever
	// the user already wrote there (DESIGN.md §17.2).
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if err := ensureManagedBlock(filepath.Join(root, name)); err != nil {
			return err
		}
	}

	fmt.Printf(`
Scaffolded %s.

Next:
  1. Edit %s/WORKFLOW.md — it is the contract every agent reads.
  2. Start the control plane:   docker compose up -d db && conductord
  3. Bootstrap and log in:      conductord bootstrap --repo %s
`, config.Dir, config.Dir, root)
	return nil
}

const managedBegin = "<!-- conductor:begin -->"
const managedEnd = "<!-- conductor:end -->"

const managedBlock = managedBegin + `
Before making code changes, obtain or attach to a Conductor task. Run
` + "`conductor check --summary \"…\" --scope path:…`" + ` first — if someone already holds
those files, it will tell you who and what to do about it. Read ` + "`.conductor/WORKFLOW.md`" + `
and the active task card. Report scope expansion before editing outside the reserved paths.
Do not publish chat transcripts or secrets as task metadata.
` + managedEnd

// ensureManagedBlock adds or refreshes the managed section without touching user content.
func ensureManagedBlock(path string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(existing)

	if start := strings.Index(text, managedBegin); start >= 0 {
		end := strings.Index(text, managedEnd)
		if end < 0 {
			return fmt.Errorf("%s has an unterminated conductor block; fix it by hand", path)
		}
		text = text[:start] + managedBlock + text[end+len(managedEnd):]
	} else {
		if text != "" && !strings.HasSuffix(text, "\n\n") {
			text = strings.TrimRight(text, "\n") + "\n\n"
		}
		text += managedBlock + "\n"
	}

	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return err
	}
	fmt.Printf("  update %s\n", filepath.Base(path))
	return nil
}

// ---------------------------------------------------------------------------
// login
// ---------------------------------------------------------------------------

func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "control plane URL")
	token := fs.String("token", "", "bearer token from `conductord bootstrap`")
	project := fs.String("project", "", "default project id or slug")
	if err := fs.Parse(args); err != nil {
		return err
	}

	creds := client.LoadCredentials()
	if *endpoint != "" {
		creds.Endpoint = *endpoint
	}
	if *token != "" {
		creds.Token = *token
	}
	if *project != "" {
		creds.Project = *project
	}
	if creds.Token == "" {
		return errors.New("no token: pass --token (get one from `conductord bootstrap`)")
	}

	saved, projects, err := connectAndSave(ctx, creds)
	if err != nil {
		return fmt.Errorf("verifying token: %w", err)
	}

	path, _ := client.CredentialsPath()
	fmt.Printf("Logged in as %s at %s\n", saved.Handle, saved.Endpoint)
	if saved.Project != "" {
		fmt.Printf("Default project: %s\n", saved.Project)
	}
	for _, p := range projects {
		fmt.Printf("  %-24s %s\n", p.Slug, p.Role)
	}
	fmt.Printf("Saved to %s (mode 0600)\n", path)
	return nil
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	reg := harness.BuildRegistry(harness.DefaultHarnessConfigs())
	caps := reg.CapabilityReport(ctx)

	type report struct {
		Endpoint     string                 `json:"endpoint"`
		Reachable    bool                   `json:"reachable"`
		Principal    string                 `json:"principal,omitempty"`
		Project      string                 `json:"project,omitempty"`
		Harnesses    []harness.Capabilities `json:"harnesses"`
		Repository   string                 `json:"repository,omitempty"`
		Integrations []integrations.Status  `json:"integrations"`
	}

	creds := client.LoadCredentials()
	out := report{Endpoint: creds.Endpoint, Project: creds.Project, Harnesses: caps}

	if creds.Token != "" {
		api := client.New(creds.Endpoint, creds.Token)
		var who struct {
			Principal struct {
				Handle string `json:"handle"`
			} `json:"principal"`
		}
		if err := api.Get(ctx, "/v1/whoami", &who); err == nil {
			out.Reachable = true
			out.Principal = who.Principal.Handle
		}
	}
	if root, err := config.FindRoot("."); err == nil {
		out.Repository = root
	}
	// Which coding tools on this machine are wired to Conductor, and how.
	home, _ := os.UserHomeDir()
	out.Integrations = integrations.Statuses(integrations.Options{
		Root: out.Repository, Home: home, Getenv: os.Getenv,
	})

	if *asJSON {
		return emit(out)
	}

	fmt.Printf("Control plane\n  %-12s %s\n", "endpoint", out.Endpoint)
	if out.Reachable {
		fmt.Printf("  %-12s reachable as %s\n", "status", out.Principal)
	} else if creds.Token == "" {
		fmt.Printf("  %-12s not logged in (run `conductor login`)\n", "status")
	} else {
		fmt.Printf("  %-12s unreachable\n", "status")
	}
	if out.Project != "" {
		fmt.Printf("  %-12s %s\n", "project", out.Project)
	}
	if out.Repository != "" {
		fmt.Printf("  %-12s %s\n", "repository", out.Repository)
	}
	fmt.Printf("\nHarnesses\n%s", harness.Describe(caps))

	fmt.Printf("\nIntegrations\n")
	var absent []string
	for _, st := range out.Integrations {
		if !st.Detected && !st.Configured {
			absent = append(absent, st.Tool)
			continue
		}
		state := "not connected"
		if st.Configured {
			state = "connected"
			if st.Transport != "" {
				state += " (" + st.Transport + ")"
			}
			if st.HooksSupported {
				if st.Hooks {
					state += " · hooks on"
				} else {
					state += " · hooks off"
				}
			}
		}
		fix := ""
		if !st.Configured || (st.HooksSupported && !st.Hooks) {
			fix = st.Fix
		}
		fmt.Printf("  %-11s %-32s %s\n", st.Tool, state, fix)
	}
	if len(absent) > 0 {
		fmt.Printf("  %-11s %s\n", "not found", strings.Join(absent, ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// dashboard
// ---------------------------------------------------------------------------

func cmdDashboard(args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	creds := client.LoadCredentials()
	if creds.Token == "" {
		return errors.New("not logged in: run `conductor login --token …`")
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	// The token rides in the URL fragment, not the query string: a browser never sends the
	// fragment to the server, so the credential stays out of the request line and any access
	// log along the way. The dashboard reads it on load and strips it from the address bar.
	link := joinLink(creds.Endpoint, ref, creds.Token)
	fmt.Println(link)
	fmt.Fprintln(os.Stderr,
		"\nThis link contains your token. Do not paste it into a shared channel.")
	return nil
}

// ---------------------------------------------------------------------------
// Scaffolds
// ---------------------------------------------------------------------------

func scaffoldProject(name string) string {
	return fmt.Sprintf(`apiVersion: conductor.dev/v1alpha1
kind: Project
metadata:
  id: %s
  displayName: %s

repository:
  canonicalRemote: ""
  defaultBranch: main
  worktreeRoot: .conductor/runtime/worktrees

coordination:
  defaultVisibility: team_summary
  claimMode: cooperative          # advisory | cooperative | strict_harness (alias: strict)
  leaseTtlSeconds: 90
  heartbeatSeconds: 20
  offlineGraceSeconds: 45
  startupTimeoutSeconds: 120
  stalledTurnTimeoutSeconds: 900
  duplicatePolicy: block_exact_warn_similar
  writeConflictPolicy: block
  readWriteConflictPolicy: warn

execution:
  isolation: git-worktree
  sandbox: none
  networkDefault: deny
  maxConcurrentAttempts: 4

workflow:
  file: .conductor/WORKFLOW.md

privacy:
  transcriptStorage: local_only
  publishModelIdentity: true
  publishHarnessIdentity: true
  allowSemanticDuplicateAnalysis: false
`, name, name)
}

const scaffoldPolicies = `router:
  rules:
    - id: security-floor
      when: task.security_sensitive || task.cryptography_sensitive
      require_tier: T4
      require_independent_review: true
      human_merge_approval: true

    - id: migration-serialization
      when: task.schema_or_migration
      require_scope: "migration:*"
      max_parallel_writers: 1

    - id: retry-escalation
      when: attempt.failures >= 2
      escalate_one_tier: true

  features:
    security_sensitive_paths: ["dir:internal/auth"]
    schema_or_migration_paths: ["dir:migrations"]
    infra_paths: ["dir:.github/workflows"]

conflict:
  duplicate_exact: block_duplicate
  duplicate_similar_threshold: 0.50
  write_write: block_conflict
  read_write: allow_with_warning
  protected_any: block_conflict
  enforcement_level: cooperative  # advisory | cooperative | strict_harness (alias: strict); overrides claimMode

budget:
  defaults:
    max_attempts: 4
  project:
    monthly_usd: 0        # 0 disables budget enforcement
    downshift_at: 0.75
    pause_at: 0.95
  member:
    monthly_tokens: 0     # per-member 30-day token allowance; share with 'conductor budget share'

concurrency:
  max_concurrent_attempts: 4
  max_per_principal: 2
  max_protected_writers: 1
  # Admission queue: past these caps, new sessions and attempts wait for a slot rather than
  # failing. 0 means unlimited. See "conductor queue" and "conductor swarm".
  max_active_sessions: 0
  max_sessions_per_principal: 0
  queue_ticket_ttl_seconds: 90
`

const scaffoldModels = `models:
  defaultAlias: worker.general
  aliases:
    worker.fast:
      capability_floor: [code_edit, tests]
      default_effort: low
      tier: T1
    worker.general:
      capability_floor: [code_edit, tests, multi_file]
      default_effort: medium
      tier: T2
    planner.frontier:
      capability_floor: [architecture, long_context]
      default_effort: high
      tier: T3
    reviewer.strong:
      capability_floor: [code_review, architecture]
      default_effort: high
      tier: T3
  ladder: [worker.fast, worker.general, planner.frontier]

# Fill in the models your installation actually has. Profiles with an empty model are
# ignored rather than treated as broken.
profiles:
  - alias: worker.fast
    harness: claude
    provider: anthropic
    model: claude-haiku-4-5-20251001
    capabilities: [code_edit, tests, read_diff]
    tier: T1
  - alias: worker.general
    harness: claude
    provider: anthropic
    model: claude-sonnet-5
    capabilities: [code_edit, tests, multi_file, read_diff]
    tier: T2
  - alias: planner.frontier
    harness: claude
    provider: anthropic
    model: claude-opus-5
    capabilities: [architecture, long_context, code_edit, strong_tool_use]
    tier: T4
  - alias: reviewer.strong
    harness: claude
    provider: anthropic
    model: claude-opus-5
    capabilities: [code_review, architecture, long_context]
    tier: T4
`

const scaffoldWorkflow = `---
version: 1
planner: planner.frontier
reviewer: reviewer.strong
required_checks: []
protected_scopes:
  - migration:primary
merge_policy:
  default: pull_request
  human_approval_for:
    - security_sensitive
    - schema_or_migration
---

# Project workflow

Replace this with your project's actual rules. It is the contract every participant reads
before touching the repository, and its content hash is recorded on every attempt.

## Before you edit

Check first, then claim:

` + "```bash" + `
conductor check --summary "what you are about to do" --scope path:some/file.go
conductor task claim --next
` + "```" + `

## Scope

Reserve only what you will modify. Prefer ` + "`path:`" + ` over ` + "`dir:`" + `. Report
scope expansion before editing outside your reservation.

## Evidence

List the commands that must pass in ` + "`required_checks`" + ` above. Completion requires a
commit plus a runner-observed exit code for each — a model reporting success is not evidence.

## Privacy

Your prompts and model output stay on your machine. Do not paste private context into
task titles or progress summaries, which are visible to the project.
`
