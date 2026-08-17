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
// It defaults to reaching the control plane over HTTP, which is the deployment shape of
// DESIGN.md §28.2: this machine connects outbound, exposes no inbound port, and holds no
// database credential. Passing --dsn selects the in-process backend instead, for the
// single-host setup of §28.1 where the runner shares a box with the control plane.
func cmdWorker(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	dsn := fs.String("dsn", "", "connect directly to Postgres instead of over HTTP (single-host only)")
	project := fs.String("project", "", "project id or slug")
	repo := fs.String("repo", "", "repository path (defaults to the project's registered path)")
	concurrency := fs.Int("concurrency", 1, "attempts to run at once on this machine")
	harnessName := fs.String("harness", "", "restrict to one harness (default: any installed)")
	permission := fs.String("permission-mode", "acceptEdits", "permission mode passed to harnesses that support one")
	once := fs.Bool("once", false, "execute at most one task, then exit")
	dryRun := fs.String("dry-run", "", "use the built-in fake harness: succeed | fail | drift | hang")
	maxTurns := fs.Int("max-turns", 60, "turn limit per attempt")
	timeout := fs.Duration("timeout", time.Hour, "wall-clock limit per attempt")
	name := fs.String("name", "", "runner name (defaults to this machine's hostname)")
	verbose := fs.Bool("v", false, "verbose logging")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor worker — claim tasks, run a harness, verify, and report

Talks to the control plane over HTTP using your saved login, so this machine needs no
database access. Each attempt gets its own git worktree and branch; the runner renews the
lease, tracks what actually changed on disk, runs the project's required checks itself, and
submits evidence.

Prove the whole loop without a model or an API key:

  conductor worker --dry-run succeed --once

Flags:
`)
		fs.PrintDefaults()
	}
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	projectRefValue, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	// Identify who this runner acts as, and confirm the credential works, before doing
	// anything expensive.
	var who struct {
		Principal domain.Principal `json:"principal"`
	}
	if err := api.Get(ctx, "/v1/whoami", &who); err != nil {
		return fmt.Errorf("authenticating: %w", err)
	}

	backend, cleanup, err := selectBackend(ctx, *dsn, api, projectRefValue, who.Principal, logger)
	if err != nil {
		return err
	}
	defer cleanup()

	snap, err := backend.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("reading project: %w", err)
	}

	repoPath := *repo
	if repoPath == "" {
		repoPath = snap.Project.RepoPath
	}
	if repoPath == "" {
		if root, err := config.FindRoot("."); err == nil {
			repoPath = root
		}
	}
	if repoPath == "" {
		return errors.New("no repository path: pass --repo, or register one with `conductord bootstrap --repo …`")
	}
	if repoPath, err = filepath.Abs(repoPath); err != nil {
		return err
	}

	registry := harness.BuildRegistry(harness.DefaultHarnessConfigs())
	selected := *harnessName
	if *dryRun != "" {
		selected = harness.FakeHarness
	}
	if selected != "" {
		if _, err := registry.Get(selected); err != nil {
			return err
		}
	} else if len(registry.Available(ctx)) == 0 {
		return errors.New("no harness installed on this machine; try --dry-run succeed")
	}

	hostname := *name
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	runnerRecord, err := backend.RegisterRunner(ctx, domain.Runner{
		OrganizationID: snap.Project.OrganizationID,
		ProjectID:      snap.Project.ID,
		PrincipalID:    who.Principal.ID,
		Name:           hostname,
		MaxConcurrency: *concurrency,
		Capabilities: domain.RunnerCapabilities{
			Harnesses: registry.Available(ctx),
			Platform:  os.Getenv("GOOS"),
			RepoPath:  repoPath,
		},
	})
	if err != nil {
		return fmt.Errorf("registering runner: %w", err)
	}

	r := runner.New(backend, registry, runner.Options{
		ProjectID:           snap.Project.ID,
		Principal:           who.Principal,
		RunnerID:            runnerRecord.ID,
		RepoPath:            repoPath,
		Concurrency:         *concurrency,
		HarnessPref:         selected,
		PermissionMode:      *permission,
		MCPEndpoint:         creds.Endpoint,
		MCPToken:            creds.Token,
		MaxTurns:            *maxTurns,
		AttemptTimeout:      *timeout,
		KeepFailedWorktrees: true,
		DryRunHarness:       *dryRun,
		Logger:              logger,
		Once:                *once,
	})

	// Keep the advertised capacity fresh so the scheduler stops dispatching here if this
	// process dies.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = backend.HeartbeatRunner(ctx, runnerRecord.ID, 0)
			}
		}
	}()

	if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// selectBackend chooses between the in-process and HTTP backends.
func selectBackend(
	ctx context.Context,
	dsn string,
	api *client.Client,
	projectRef string,
	principal domain.Principal,
	logger *slog.Logger,
) (runner.Backend, func(), error) {
	if dsn == "" {
		logger.Info("runner connecting over HTTP", "endpoint", api.Endpoint)
		return runner.NewRemoteBackend(api, projectRef), func() {}, nil
	}

	logger.Warn("runner connecting directly to Postgres",
		"note", "this machine holds database credentials; prefer the default HTTP backend for shared deployments")

	store, err := db.Open(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	project, err := lookupProject(ctx, store, principal.OrganizationID, projectRef)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	backend := runner.NewDirectBackend(store, coord.New(store), project.ID, principal)
	return backend, func() { store.Close() }, nil
}

func lookupProject(ctx context.Context, store *db.Store, orgID domain.ID, ref string) (domain.Project, error) {
	if p, err := store.GetProject(ctx, ref); err == nil {
		return p, nil
	}
	return store.GetProjectBySlug(ctx, orgID, ref)
}
