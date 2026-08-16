package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/harness"
	"github.com/adamburan/conductor/internal/runner"
)

// cmdWorker runs the execution plane on this machine.
//
// It currently connects to Postgres directly, which suits the single-host deployment of
// DESIGN.md §28.1 — the runner lives on the same box as the control plane so it can reach
// locally-authenticated Claude/Codex/OpenCode sessions. The §28.2 shape, where a laptop
// runner speaks only outbound HTTPS to a shared control plane, needs an HTTP-backed
// implementation of the same loop; the runner's dependencies are narrow enough that it is a
// contained change, but it is not what this build does today.
func cmdWorker(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL connection string")
	project := fs.String("project", "", "project id or slug")
	repo := fs.String("repo", "", "repository path (defaults to the project's registered path)")
	concurrency := fs.Int("concurrency", 1, "attempts to run at once on this machine")
	harnessName := fs.String("harness", "", "restrict to one harness (default: any installed)")
	permission := fs.String("permission-mode", "acceptEdits", "permission mode passed to harnesses that support one")
	once := fs.Bool("once", false, "execute at most one task, then exit")
	dryRun := fs.String("dry-run", "", "use the built-in fake harness: succeed | fail | drift | hang")
	maxTurns := fs.Int("max-turns", 60, "turn limit per attempt")
	timeout := fs.Duration("timeout", time.Hour, "wall-clock limit per attempt")
	endpoint := fs.String("mcp-endpoint", "", "control plane URL injected into the agent's MCP config")
	verbose := fs.Bool("v", false, "verbose logging")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor worker — claim tasks, run a harness, verify, and report

Each attempt gets its own git worktree and branch. The runner renews the lease, tracks what
actually changed on disk, runs the project's required checks itself, and submits evidence.

Prove the whole loop without a model or an API key:

  conductor worker --dry-run succeed --once

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return errors.New("no database configured: pass --dsn or set DATABASE_URL")
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	creds := client.LoadCredentials()
	projectRefValue, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	store, err := db.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer store.Close()

	principal, resolvedProject, err := resolveWorkerIdentity(ctx, store, creds, projectRefValue)
	if err != nil {
		return err
	}

	repoPath := *repo
	if repoPath == "" {
		repoPath = resolvedProject.RepoPath
	}
	if repoPath == "" {
		if root, err := config.FindRoot("."); err == nil {
			repoPath = root
		}
	}
	if repoPath == "" {
		return errors.New("no repository path: pass --repo, or register one with `conductord bootstrap --repo …`")
	}
	repoPath, err = filepath.Abs(repoPath)
	if err != nil {
		return err
	}

	harnessConfigs := harness.DefaultHarnessConfigs()
	registry := harness.BuildRegistry(harnessConfigs)

	selected := *harnessName
	if *dryRun != "" {
		selected = "fake"
	}
	if selected != "" {
		if _, err := registry.Get(selected); err != nil {
			return err
		}
	} else if len(registry.Available(ctx)) == 0 {
		return errors.New("no harness installed on this machine; try --dry-run succeed")
	}

	host, _ := os.Hostname()
	runnerRecord, err := store.RegisterRunner(ctx, domain.Runner{
		OrganizationID: resolvedProject.OrganizationID,
		ProjectID:      resolvedProject.ID,
		PrincipalID:    principal.ID,
		Name:           host,
		MaxConcurrency: *concurrency,
		Capabilities: domain.RunnerCapabilities{
			Harnesses: registry.Available(ctx),
			Platform:  os.Getenv("GOOS"),
			RepoPath:  repoPath,
		},
	})
	if err != nil {
		return err
	}

	svc := coord.New(store)
	r := runner.New(store, svc, registry, runner.Options{
		ProjectID:           resolvedProject.ID,
		Principal:           principal,
		RunnerID:            runnerRecord.ID,
		RepoPath:            repoPath,
		Concurrency:         *concurrency,
		HarnessPref:         selected,
		PermissionMode:      *permission,
		MCPEndpoint:         firstNonEmptyStr(*endpoint, creds.Endpoint),
		MCPToken:            creds.Token,
		MaxTurns:            *maxTurns,
		AttemptTimeout:      *timeout,
		KeepFailedWorktrees: true,
		DryRunHarness:       *dryRun,
		Logger:              logger,
		Once:                *once,
	})

	// Keep the runner's advertised capacity fresh so the scheduler stops dispatching here
	// if this process dies.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = store.HeartbeatRunner(ctx, runnerRecord.ID, 0)
			}
		}
	}()

	err = r.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// resolveWorkerIdentity finds the principal and project this runner acts as.
func resolveWorkerIdentity(ctx context.Context, store *db.Store, creds client.Credentials, projectRef string) (domain.Principal, domain.Project, error) {
	if creds.Token != "" {
		if principal, err := store.AuthenticateToken(ctx, creds.Token); err == nil {
			project, err := lookupProject(ctx, store, principal.OrganizationID, projectRef)
			return principal, project, err
		}
	}
	return domain.Principal{}, domain.Project{},
		errors.New("worker could not authenticate: run `conductor login` first")
}

func lookupProject(ctx context.Context, store *db.Store, orgID domain.ID, ref string) (domain.Project, error) {
	if p, err := store.GetProject(ctx, ref); err == nil {
		return p, nil
	}
	return store.GetProjectBySlug(ctx, orgID, ref)
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
