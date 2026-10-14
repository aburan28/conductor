package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/config"
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

	fmt.Print(initNextSteps(root))
	return nil
}

// initNextSteps is what `conductor init` prints when it is done. It used to suggest
// `docker compose up -d db && conductord`, which only works inside the Conductor checkout:
// a user's own repository has no compose file. `conductor up` works from anywhere.
func initNextSteps(root string) string {
	return fmt.Sprintf(`
Scaffolded %s.

Next:
  1. Edit %s/WORKFLOW.md — it is the contract every agent reads.
  2. Start the control plane and log in (run from %s):
       conductor up
  3. See what is going on:      conductor status
`, config.Dir, config.Dir, root)
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
	token := fs.String("token", "", "bearer token (not needed on the machine running conductord)")
	project := fs.String("project", "", "default project id or slug")
	var single ssoFlag
	fs.Var(&single, "sso", "sign in through the control plane's single sign-on provider in your browser (--sso, --sso google, or --sso=google)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(positional) == 1 && single.set && single.provider == "":
		single.provider = positional[0]
	case len(positional) > 0:
		return fmt.Errorf("unexpected argument %q (usage: conductor login [--endpoint URL] [--token TOKEN | --sso [PROVIDER]] [--project SLUG])", positional[0])
	}
	if single.set && *token != "" {
		return errors.New("--sso and --token are two ways to sign in; pass one")
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
	if single.set {
		// The browser signs in at the provider; conductord issues an ordinary token for the
		// account it maps to, saved exactly like a pasted one.
		res, err := ssoSignIn(ctx, creds.Endpoint, "", single.provider, false)
		if err != nil {
			return err
		}
		creds.Token = res.Token
		return finishLogin(ctx, creds)
	}
	if *token == "" && isLoopbackEndpoint(creds.Endpoint) {
		// Logging in on the machine that runs the control plane needs no token at all.
		local := creds
		local.Token = ""
		saved, err := localSignInAndSave(local)
		if err == nil {
			fmt.Printf("Logged in as %s at %s\n", saved.Handle, saved.Endpoint)
			if saved.Project != "" {
				fmt.Printf("Default project: %s\n", saved.Project)
			}
			return nil
		}
		if creds.Token == "" {
			return fmt.Errorf("local sign-in failed: %w\n(pass --token, from `conductord bootstrap` or a teammate's `conductor invite`)", err)
		}
		// Otherwise re-verify the token already saved, against the endpoint and project given.
	}
	if creds.Token == "" {
		return errors.New("no token: pass --token (get one from `conductord bootstrap` or a teammate's `conductor invite`)")
	}

	return finishLogin(ctx, creds)
}

// finishLogin verifies and saves a login, and says where it went.
func finishLogin(ctx context.Context, creds client.Credentials) error {
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
// dashboard
// ---------------------------------------------------------------------------

func cmdDashboard(args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	creds := client.LoadCredentials()
	// On this machine, in local security mode, the dashboard signs its owner in by itself:
	// print the plain address, with no credential in it to leak.
	if isLoopbackEndpoint(creds.Endpoint) && os.Getenv("CONDUCTOR_NO_LOCAL_LOGIN") == "" {
		var st struct {
			Available bool `json:"local_login_available"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := client.New(creds.Endpoint, "").Get(ctx, "/v1/local/status", &st)
		cancel()
		if err == nil && st.Available {
			link := strings.TrimRight(creds.Endpoint, "/") + "/"
			if creds.Project != "" || *project != "" {
				link += "#project=" + url.QueryEscape(firstNonEmptyString(*project, creds.Project))
			}
			fmt.Println(link)
			fmt.Fprintln(os.Stderr, "\nOpen it on this machine; you are signed in automatically.")
			return nil
		}
	}
	if creds.Token == "" {
		_, signed, err := mustClient()
		if err != nil {
			return err
		}
		creds = signed
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
  claimMode: cooperative          # advisory | cooperative | strict
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
  enforcement_level: cooperative

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
