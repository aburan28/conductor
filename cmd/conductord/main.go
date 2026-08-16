// Command conductord is the Conductor control plane: REST API, SSE event stream, dashboard,
// and the background scheduler.
//
// It is one process because DESIGN.md §28.1 targets a single host first. Nothing here
// prevents running the API and the scheduler separately later — every scheduler step is
// transactional and `SKIP LOCKED`-based, so replicas are safe without leader election.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/adamburan/conductor/internal/api"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/scheduler"
	"github.com/adamburan/conductor/internal/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		if err := bootstrap(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "bootstrap:", err)
			os.Exit(1)
		}
		return
	}
	if err := serve(os.Args[1:]); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "conductord:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("conductord", flag.ExitOnError)
	addr := fs.String("addr", envOr("CONDUCTOR_ADDR", ":8080"), "listen address")
	dsn := fs.String("dsn", envOr("DATABASE_URL", ""), "PostgreSQL connection string")
	tick := fs.Duration("tick", 2*time.Second, "scheduler tick interval")
	detect := fs.Duration("detect-every", 15*time.Second, "conflict graph recomputation interval")
	noScheduler := fs.Bool("no-scheduler", false, "serve the API without running the scheduler")
	verbose := fs.Bool("v", false, "verbose logging")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `conductord — Conductor control plane

Usage:
  conductord [flags]
  conductord bootstrap [flags]     create an organization, project, principal, and token

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
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := db.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	logger.Info("database ready")

	svc := coord.New(store)
	server := api.New(store, svc, api.Options{Logger: logger, Dashboard: web.Dashboard()})

	if !*noScheduler {
		sched := scheduler.New(store, svc, scheduler.Options{
			Tick: *tick, DetectEvery: *detect, Logger: logger,
		})
		go func() {
			if err := sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("scheduler exited", "error", err)
			}
		}()
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE event stream is a long-lived response, and a write
		// deadline would sever every dashboard connection on a fixed interval.
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("conductor listening", "addr", *addr, "dashboard", "http://localhost"+*addr+"/")
	return httpServer.ListenAndServe()
}

// bootstrap creates the first tenant and prints a token.
//
// It is a separate subcommand rather than an API endpoint on purpose: the very first
// credential in a system cannot be authenticated by that system, so it is issued by someone
// with database access, once.
func bootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	dsn := fs.String("dsn", envOr("DATABASE_URL", ""), "PostgreSQL connection string")
	orgSlug := fs.String("org", "default", "organization slug")
	projectSlug := fs.String("project", "", "project slug (defaults to the repository directory name)")
	handle := fs.String("principal", envOr("USER", "operator"), "principal handle")
	repo := fs.String("repo", ".", "path to the repository this project coordinates")
	role := fs.String("role", string(domain.RoleProjectAdmin), "role to grant the principal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return errors.New("no database configured: pass --dsn or set DATABASE_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := db.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	repoPath, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	if *projectSlug == "" {
		*projectSlug = filepath.Base(repoPath)
	}

	// Idempotent: re-running bootstrap should mint a fresh token, not fail because the org
	// already exists.
	org, err := store.GetOrganizationBySlug(ctx, *orgSlug)
	if errors.Is(err, domain.ErrNotFound) {
		org, err = store.CreateOrganization(ctx, *orgSlug, *orgSlug)
	}
	if err != nil {
		return fmt.Errorf("organization: %w", err)
	}

	principal, err := store.GetPrincipalByHandle(ctx, org.ID, *handle)
	if errors.Is(err, domain.ErrNotFound) {
		principal, err = store.CreatePrincipal(ctx, org.ID, domain.PrincipalHuman, *handle, *handle, "")
	}
	if err != nil {
		return fmt.Errorf("principal: %w", err)
	}

	// Load repository policy if the repo has a .conductor directory.
	bundle, err := config.Load(repoPath)
	if err != nil {
		return fmt.Errorf("load policy: %w", err)
	}

	project, err := store.GetProjectBySlug(ctx, org.ID, *projectSlug)
	if errors.Is(err, domain.ErrNotFound) {
		project, err = store.CreateProject(ctx, db.CreateProjectParams{
			OrganizationID:  org.ID,
			Slug:            *projectSlug,
			DisplayName:     firstNonEmpty(bundle.Project.Metadata.DisplayName, *projectSlug),
			CanonicalRemote: bundle.Project.Repository.CanonicalRemote,
			DefaultBranch:   firstNonEmpty(bundle.Project.Repository.DefaultBranch, "main"),
			WorktreeRoot:    bundle.Project.Repository.WorktreeRoot,
			RepoPath:        repoPath,
			Config:          bundle.ProjectConfig(),
		})
	} else if err == nil {
		err = store.UpdateProjectConfig(ctx, project.ID, bundle.ProjectConfig())
		if err == nil {
			err = store.SetProjectRepoPath(ctx, project.ID, repoPath)
		}
	}
	if err != nil {
		return fmt.Errorf("project: %w", err)
	}

	if err := store.AddMember(ctx, project.ID, principal.ID, domain.Role(*role)); err != nil {
		return fmt.Errorf("membership: %w", err)
	}

	if bundle.Workflow.Raw != "" {
		if _, err := store.PutWorkflow(ctx, project.ID, bundle.Workflow.Raw, nil); err != nil {
			return fmt.Errorf("workflow: %w", err)
		}
	}
	for _, profile := range bundle.ModelProfiles(org.ID) {
		if _, err := store.UpsertModelProfile(ctx, profile); err != nil {
			return fmt.Errorf("model profile %s/%s: %w", profile.Alias, profile.Harness, err)
		}
	}

	token, err := store.CreateToken(ctx, principal.ID, "bootstrap", 0)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}

	fmt.Printf(`Conductor is bootstrapped.

  organization  %s
  project       %s  (%s)
  principal     %s  (%s)
  repository    %s

Save your credentials:

  conductor login --endpoint http://localhost:8080 --token %s --project %s

This token is shown once and is stored only as a hash. Keep it out of shared logs.
`, org.Slug, project.Slug, project.ID, principal.Handle, *role, repoPath, token, project.Slug)
	return nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
