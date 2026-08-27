// Package runner is the execution plane (DESIGN.md §7.10).
//
// A runner is installed on each machine allowed to execute tasks. It claims work, prepares an
// isolated worktree, resolves a model, launches a harness driver, renews the lease, tracks
// what actually changed on disk, runs the required checks itself, and submits evidence.
//
// The division of labour with the control plane is deliberate: the runner never decides
// whether it may proceed. It asks, and the control plane's transactional answer is final.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/harness"
	"github.com/adamburan/conductor/internal/policy"
	"github.com/adamburan/conductor/internal/router"
	"github.com/adamburan/conductor/internal/worktree"
)

// Options configures a runner.
type Options struct {
	ProjectID   domain.ID
	Principal   domain.Principal
	RunnerID    domain.ID
	RepoPath    string
	Concurrency int
	// HarnessPref restricts this runner to one harness; empty means any registered one.
	HarnessPref string
	// PermissionMode is passed to harnesses that support one.
	PermissionMode string
	// MCPEndpoint is the control-plane URL injected into the agent's MCP config so it can
	// coordinate from inside its own session.
	MCPEndpoint    string
	MCPToken       string
	MCPCommand     string
	PollInterval   time.Duration
	MaxTurns       int
	AttemptTimeout time.Duration
	// KeepFailedWorktrees retains the tree after a failure so the branch can be inspected
	// and adopted (DESIGN.md §27.2).
	KeepFailedWorktrees bool
	DryRunHarness       string
	Logger              *slog.Logger
	Once                bool
}

func (o Options) withDefaults() Options {
	if o.Concurrency <= 0 {
		o.Concurrency = 1
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 3 * time.Second
	}
	if o.MaxTurns <= 0 {
		o.MaxTurns = 60
	}
	if o.AttemptTimeout <= 0 {
		o.AttemptTimeout = time.Hour
	}
	if o.MCPCommand == "" {
		o.MCPCommand = "conductor-mcp"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Runner executes attempts on one machine.
//
// It reaches the control plane only through a Backend, so the same loop serves both the
// single-host deployment and a laptop that speaks HTTP and holds no database credential.
type Runner struct {
	backend  Backend
	registry *harness.Registry
	trees    *worktree.Manager
	opts     Options
}

func New(backend Backend, reg *harness.Registry, opts Options) *Runner {
	opts = opts.withDefaults()
	return &Runner{
		backend:  backend,
		registry: reg,
		trees:    worktree.New(opts.RepoPath, ""),
		opts:     opts,
	}
}

// Run polls for work until the context is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	r.opts.Logger.Info("runner started",
		"project", r.opts.ProjectID, "concurrency", r.opts.Concurrency,
		"harnesses", r.registry.Available(ctx))

	slots := make(chan struct{}, r.opts.Concurrency)
	for i := 0; i < r.opts.Concurrency; i++ {
		slots <- struct{}{}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-slots:
		}

		executed, err := r.step(ctx)
		slots <- struct{}{}

		if err != nil && !errors.Is(err, context.Canceled) {
			r.opts.Logger.Error("runner step failed", "error", err)
		}
		if r.opts.Once {
			return err
		}
		if !executed {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(r.opts.PollInterval):
			}
		}
	}
}

// step claims and executes at most one task. It returns false when the queue is empty.
func (r *Runner) step(ctx context.Context) (bool, error) {
	snap, err := r.backend.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	project := snap.Project

	// Respect project concurrency before taking more work.
	if project.Config.MaxConcurrentAttempts > 0 &&
		snap.ActiveAttempts >= project.Config.MaxConcurrentAttempts {
		return false, nil
	}

	available := r.availableHarnesses(ctx)
	if len(available) == 0 {
		return false, fmt.Errorf("%w: no harness available on this runner", domain.ErrCapacity)
	}

	claim, err := r.backend.ClaimNext(ctx, ClaimRequest{
		Harness:          available[0],
		RunnerID:         r.opts.RunnerID,
		Role:             domain.RoleImplementer,
		WorkflowSHA:      project.WorkflowSHA,
		ProjectConfigSHA: project.ConfigSHA,
		RouterPolicyVer:  router.PolicyVersion,
		LeaseTTL:         project.Config.LeaseTTL,
		MaxCandidates:    10,
	})
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil // nothing ready
		}
		return false, err
	}

	r.opts.Logger.Info("claimed task",
		"task", claim.Task.Ref, "attempt", claim.Attempt.AttemptNumber,
		"epoch", claim.Fence.FencingEpoch)

	if err := r.execute(ctx, snap, claim, available); err != nil {
		r.opts.Logger.Error("attempt failed",
			"task", claim.Task.Ref, "error", err)
		// Hand the task back so it is retried or failed by policy rather than stranded
		// until the lease expires.
		if rerr := r.backend.Release(ctx, ReleaseRequest{
			Fence:          claim.Fence,
			Reason:         "runner error",
			AttemptState:   domain.AttemptFailedRetryable,
			FailureClass:   "runner_error",
			FailureSummary: truncate(err.Error(), 400),
		}); rerr != nil && !errors.Is(rerr, domain.ErrStaleFencing) {
			r.opts.Logger.Error("release after failure failed", "error", rerr)
		}
	}
	return true, nil
}

// execute runs one attempt end to end.
func (r *Runner) execute(ctx context.Context, snap coord.RunnerSnapshot, claim db.ClaimResult, available []string) error {
	project := snap.Project
	task := claim.Task

	// --- Route --------------------------------------------------------------
	decision, err := r.route(ctx, snap, task, claim.Attempt, claim.Reservations, available)
	if err != nil {
		return err
	}
	if decision.Paused {
		r.opts.Logger.Warn("budget exhausted; handing task back", "task", task.Ref)
		return r.backend.Release(ctx, ReleaseRequest{
			Fence: claim.Fence, Reason: "budget paused",
			AttemptState: domain.AttemptCancelled, NextTaskStatus: domain.TaskReady,
		})
	}

	driver, err := r.registry.Get(decision.Harness)
	if err != nil {
		return err
	}

	// --- Workspace ----------------------------------------------------------
	if err := r.backend.SetAttemptState(ctx, claim.Attempt.ID,
		domain.AttemptPreparingWorkspace, "", "", false); err != nil {
		return err
	}

	ws, err := r.trees.Prepare(ctx, task.Ref, claim.Attempt.AttemptNumber, project.DefaultBranch)
	if err != nil {
		return fmt.Errorf("prepare worktree: %w", err)
	}

	if err := r.backend.SetRoute(ctx, claim.Attempt.ID, coord.RouteRecord{
		ProfileID: decision.ProfileID, Alias: decision.Alias, Model: decision.Model,
		Harness: decision.Harness, Effort: decision.Effort,
		RouterPolicyVer: router.PolicyVersion, ModelCatalogVer: decision.CatalogVersion,
		WorktreePath: ws.Path, Branch: ws.Branch, BaseCommitSHA: ws.BaseSHA,
		RunnerID: r.opts.RunnerID,
		Tier:     string(decision.Tier), Rationale: decision.Rationale,
	}); err != nil {
		return err
	}

	keepTree := r.opts.KeepFailedWorktrees
	defer func() {
		if err := r.trees.Remove(context.WithoutCancel(ctx), ws.Path, keepTree); err != nil {
			r.opts.Logger.Warn("worktree cleanup failed", "path", ws.Path, "error", err)
		}
	}()

	// --- Instruction --------------------------------------------------------
	brief, err := r.backend.Brief(ctx, claim.Attempt.ID)
	if err != nil {
		return err
	}
	cardPath, err := writeCard(ws.Path, task.Ref, brief.Card)
	if err != nil {
		return err
	}
	mcpConfig, err := r.writeMCPConfig(ws.Path, project, claim.Fence)
	if err != nil {
		return err
	}

	// --- Launch -------------------------------------------------------------
	if err := r.backend.SetAttemptState(ctx, claim.Attempt.ID,
		domain.AttemptStartingHarness, "", "", false); err != nil {
		return err
	}

	spec := harness.RunSpec{
		TaskRef: task.Ref, TaskCardPath: cardPath, Instruction: brief.Instruction,
		Cwd: ws.Path, Branch: ws.Branch, BaseSHA: ws.BaseSHA,
		Model: decision.Model, Effort: decision.Effort, Role: domain.RoleImplementer,
		MaxTurns: r.opts.MaxTurns, Timeout: r.opts.AttemptTimeout,
		MCPConfigPath: mcpConfig, PermissionMode: r.opts.PermissionMode,
		NetworkPolicy: project.Config.NetworkDefault,
		Fence:         claim.Fence, WorkflowSHA: project.WorkflowSHA,
	}
	if r.opts.DryRunHarness != "" {
		spec.Env = append(spec.Env, "FAKE_HARNESS_BEHAVIOR="+r.opts.DryRunHarness)
	}

	handle, err := driver.Start(ctx, spec)
	if err != nil {
		return fmt.Errorf("start harness %s: %w", decision.Harness, err)
	}
	if err := r.backend.SetAttemptState(ctx, claim.Attempt.ID,
		domain.AttemptRunning, ws.Branch, ws.Path, true); err != nil {
		return err
	}

	// --- Supervise ----------------------------------------------------------
	// The supervision loop is bound to its own context so it stops when the harness exits.
	// Without that it would sit on its ticker forever and the runner would never finish the
	// attempt it is waiting on.
	superviseCtx, stopSupervision := context.WithCancel(ctx)
	supervision := r.supervise(superviseCtx, project, task, claim, ws, handle)

	result, waitErr := handle.Wait(ctx)
	stopSupervision()
	<-supervision // let the heartbeat loop unwind before collecting evidence

	// The supervision ticker only ever saw the totals as of its last tick; whatever the
	// harness used in its final seconds would otherwise never reach the ledger. Totals are
	// merged monotonically server-side, so reporting them once more is safe.
	if result.TokensIn > 0 || result.TokensOut > 0 || result.CostUSD > 0 {
		if err := r.backend.ReportProgress(ctx, claim.Fence, coord.ProgressReport{
			Phase: "finishing", TokensIn: result.TokensIn, TokensOut: result.TokensOut,
			CostUSD: result.CostUSD, Turns: result.Turns,
		}); err != nil {
			r.opts.Logger.Warn("final usage report failed", "error", err)
		}
	}

	// --- Collect evidence ---------------------------------------------------
	changed, head, _ := r.trees.Status(ctx, ws.Path, ws.BaseSHA)

	commitSHA := head
	if len(changed) > 0 {
		if sha, err := r.trees.Commit(ctx, ws.Path,
			fmt.Sprintf("%s: %s\n\nConductor-Task: %s\nConductor-Attempt: %d",
				task.Ref, task.Title, task.Ref, claim.Attempt.AttemptNumber)); err == nil && sha != "" {
			commitSHA = sha
		}
	}

	validation := r.runChecks(ctx, ws.Path, project.Config.RequiredChecks)

	outcome := "succeeded"
	failureClass := result.FailureClass
	switch {
	case waitErr != nil && errors.Is(waitErr, context.DeadlineExceeded):
		outcome, failureClass = "failed", "timeout"
	case waitErr != nil:
		outcome, failureClass = "failed", "process_error"
	case !result.Succeeded:
		outcome = "failed"
	case len(changed) == 0:
		// A run that produced no diff has not done the work, whatever it reported.
		outcome, failureClass = "failed", "no_changes"
	}
	if outcome != "succeeded" {
		keepTree = true
	}

	manifest := domain.EvidenceManifest{
		BaseSHA: ws.BaseSHA, CommitSHA: commitSHA,
		Commands: validation, ChangedPaths: changed,
		RunnerID: r.opts.RunnerID, CreatedAt: time.Now().UTC(),
	}

	finish, err := r.backend.Finish(ctx, coord.FinishRequest{
		Fence: claim.Fence, ProjectID: project.ID, Outcome: outcome,
		FailureClass: failureClass, Evidence: &manifest, RunnerID: r.opts.RunnerID,
	})
	if err != nil {
		return err
	}

	r.opts.Logger.Info("attempt finished",
		"task", task.Ref, "outcome", outcome, "task_status", finish.TaskStatus,
		"changed_files", len(changed), "tokens_in", result.TokensIn,
		"tokens_out", result.TokensOut, "cost_usd", result.CostUSD,
		"missing_checks", finish.MissingChecks)
	return nil
}

// supervise runs the heartbeat and drift-detection loop alongside the harness.
//
// It does two jobs the harness cannot do for itself: keep the lease alive so the task is not
// reclaimed mid-run, and compare the actual git diff against the declared scope so drift is
// caught before the next turn rather than at merge (DESIGN.md §8.6).
func (r *Runner) supervise(
	ctx context.Context,
	project domain.Project,
	task domain.Task,
	claim db.ClaimResult,
	ws worktree.Workspace,
	handle harness.Handle,
) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		interval := project.Config.HeartbeatInterval.OrDefault(20 * time.Second)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		var tokensIn, tokensOut int64
		var cost float64
		var turns int
		events := handle.Events()
		declared := scopeStrings(claim.Reservations)
		reported := map[string]bool{}

		for {
			select {
			case <-ctx.Done():
				return

			case ev, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				tokensIn += ev.TokensIn
				tokensOut += ev.TokensOut
				if ev.CostUSD > cost {
					cost = ev.CostUSD
				}
				if ev.Turns > turns {
					turns = ev.Turns
				}

			case <-ticker.C:
				changed, _, err := r.trees.Status(ctx, ws.Path, ws.BaseSHA)
				if err != nil {
					r.opts.Logger.Warn("worktree status failed", "error", err)
				}

				if err := r.backend.ReportProgress(ctx, claim.Fence,
					coord.ProgressReport{
						Phase: "implementing", ChangedPaths: changed,
						TokensIn: tokensIn, TokensOut: tokensOut, CostUSD: cost, Turns: turns,
					}); err != nil {
					if errors.Is(err, domain.ErrStaleFencing) || errors.Is(err, domain.ErrLeaseNotHeld) {
						// The lease was reclaimed underneath us. Stop the harness rather
						// than letting a zombie keep editing (DESIGN.md §7.5).
						r.opts.Logger.Warn("lease lost; cancelling harness", "task", task.Ref)
						_ = handle.Cancel(ctx)
						return
					}
					r.opts.Logger.Warn("progress report failed", "error", err)
					continue
				}

				// Scope drift: request reservations for anything touched outside the
				// declared set.
				var drifted []domain.ScopeRequest
				for _, p := range changed {
					if reported[p] || coveredBy(p, declared) {
						continue
					}
					reported[p] = true
					drifted = append(drifted, domain.ScopeRequest{
						Resource: "path:" + p, Mode: domain.ModeWriteExclusive,
					})
				}
				if len(drifted) == 0 {
					continue
				}

				res, err := r.backend.ExpandScope(ctx, claim.Fence,
					drifted, domain.SourceObserved)
				if err != nil {
					r.opts.Logger.Warn("scope expansion failed", "error", err)
					continue
				}
				if res.Outcome.Blocks() {
					r.opts.Logger.Warn("scope conflict; pausing harness",
						"task", task.Ref, "advice", res.Advice)
					_ = handle.Cancel(ctx)
					return
				}
				declared = append(declared, scopeStrings(res.Granted)...)
			}
		}
	}()

	return done
}

// route resolves the model for this attempt.
//
// Every input comes from the snapshot and the claim, so this makes no additional round trips
// — which matters when the control plane is across a network rather than in the same process.
func (r *Runner) route(ctx context.Context, snap coord.RunnerSnapshot, task domain.Task, attempt domain.Attempt, reservations []domain.ScopeReservation, available []string) (router.Decision, error) {
	project := snap.Project
	profiles := snap.Profiles

	features := router.DeriveFeatures(scopeStrings(reservations), attempt.ChangedPaths,
		router.FeaturePolicyFrom(project.Config.FeaturePaths), task.Features)

	// The fake harness has no catalog entries of its own, so supply them in memory — but
	// only when it was explicitly asked for. Its profiles are capacity-billed and therefore
	// sort as free, so leaving them in the catalog during normal operation would make every
	// task quietly route to the fake instead of a real model.
	if r.usingFake() {
		profiles = append(profiles, harness.FakeProfiles(project.OrganizationID, aliasesOf(profiles))...)
	}

	facts := policy.Facts{
		Task: task, Features: features, Scopes: scopeStrings(reservations),
		ChangedPaths:  attempt.ChangedPaths,
		AttemptNumber: attempt.AttemptNumber, PriorFailures: max(0, task.AttemptsCount-1),
		Role: domain.RoleImplementer, Harnesses: available,
		Budget: policy.BudgetFacts{MonthlyUSD: project.Config.Budget.MonthlyUSD, SpentUSD: snap.SpentUSD},
	}

	cat := buildCatalog(profiles)
	decision, err := router.Route(cat, router.Request{
		Role:               domain.RoleImplementer,
		Features:           features,
		RiskLevel:          task.RiskLevel,
		AttemptNumber:      attempt.AttemptNumber,
		PriorFailures:      max(0, task.AttemptsCount-1),
		RequestedAlias:     task.ModelAlias,
		HarnessPref:        firstNonEmpty(r.opts.HarnessPref, task.HarnessPref),
		AvailableHarnesses: available,
		Budget: router.BudgetState{
			MonthlyUSD:  project.Config.Budget.MonthlyUSD,
			SpentUSD:    snap.SpentUSD,
			DownshiftAt: project.Config.Budget.DownshiftAt,
			PauseAt:     project.Config.Budget.PauseAt,
		},
		Rules: project.Config.RouterRules,
		Facts: &facts,
	})
	if err != nil || decision.Paused {
		return decision, err
	}

	// Repository dispatch policy, when configured, chooses the concrete model within the
	// floors the router just computed. It never routes below the router's hard floor.
	if dp := project.Config.Dispatch; dp != nil && !dp.Empty() && !r.usingFake() {
		compiled, issues := policy.CompileDispatch(dp)
		if policy.HasErrors(issues) {
			r.opts.Logger.Warn("dispatch policy has errors; using the tier router", "task", task.Ref)
			return decision, nil
		}
		dd, derr := compiled.Resolve(facts, policy.ResolveOptions{Catalog: policy.ProfileCatalog(profiles)})
		if derr != nil {
			r.opts.Logger.Info("dispatch policy did not place the task; using the tier router",
				"task", task.Ref, "reason", derr.Error())
			return decision, nil
		}
		if decision.Tier == "" || dd.Tier == "" || dd.Tier.AtLeast(decision.Tier) {
			decision.Model, decision.Harness, decision.Effort = dd.Model, dd.Harness, dd.Effort
			if dd.Provider != "" {
				decision.Provider = dd.Provider
			}
			if dd.Alias != "" {
				decision.Alias = dd.Alias
			}
			if dd.Tier != "" {
				decision.Tier = dd.Tier
			}
			decision.Rationale = append(decision.Rationale, dd.Rationale...)
		} else {
			decision.Rationale = append(decision.Rationale,
				"dispatch candidate below the router floor; kept the router's model")
		}
	}
	return decision, nil
}

// usingFake reports whether this runner was explicitly asked to use the built-in fake
// harness, rather than merely having it registered.
func (r *Runner) usingFake() bool {
	return r.opts.DryRunHarness != "" || r.opts.HarnessPref == harness.FakeHarness
}

// availableHarnesses lists what this runner may dispatch to.
//
// The fake is filtered out unless requested. It is always "installed", so leaving it in the
// list would let it be selected on a machine that has a real harness — turning a production
// run into a no-op that reports success.
func (r *Runner) availableHarnesses(ctx context.Context) []string {
	if r.usingFake() {
		return []string{harness.FakeHarness}
	}
	if r.opts.HarnessPref != "" {
		return []string{r.opts.HarnessPref}
	}
	all := r.registry.Available(ctx)
	out := make([]string, 0, len(all))
	for _, name := range all {
		if name != harness.FakeHarness {
			out = append(out, name)
		}
	}
	return out
}

// aliasesOf returns the distinct aliases present in a profile set, so synthetic profiles
// cover exactly the roles this project actually defines.
func aliasesOf(profiles []domain.ModelProfile) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range profiles {
		if !seen[p.Alias] {
			seen[p.Alias] = true
			out = append(out, p.Alias)
		}
	}
	return out
}

// buildCatalog derives alias policies from the stored profiles, so the router works even when
// no models.yaml has been imported.
func buildCatalog(profiles []domain.ModelProfile) router.Catalog {
	cat := router.Catalog{
		Profiles:     profiles,
		Aliases:      map[string]router.AliasPolicy{},
		DefaultAlias: "worker.general",
	}
	for _, p := range profiles {
		if _, ok := cat.Aliases[p.Alias]; ok {
			continue
		}
		cat.Aliases[p.Alias] = router.AliasPolicy{
			DefaultEffort: p.ReasoningEffort,
			Tier:          p.Tier,
		}
		cat.Version = p.CatalogVersion
	}
	for _, name := range []string{"worker.fast", "worker.general", "planner.frontier"} {
		if _, ok := cat.Aliases[name]; ok {
			cat.Ladder = append(cat.Ladder, name)
		}
	}
	return cat
}

// writeCard materializes the rendered task card into the worktree so the agent can read it
// as a file, not only as an instruction.
func writeCard(worktreePath, taskRef, rendered string) (string, error) {
	dir := filepath.Join(worktreePath, ".conductor", "generated", "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, taskRef+".md")
	if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// writeMCPConfig drops an MCP server config into the worktree so the agent can call the
// coordination tools from inside its own session.
//
// The token written here is the runner's, and the file lives in a worktree that is removed
// on success — it is a short-lived, machine-local credential, not a shared secret.
func (r *Runner) writeMCPConfig(dir string, project domain.Project, fence domain.Fence) (string, error) {
	if r.opts.MCPEndpoint == "" {
		return "", nil
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"conductor": map[string]any{
				"command": r.opts.MCPCommand,
				"args":    []string{"--endpoint", r.opts.MCPEndpoint},
				"env": map[string]string{
					"CONDUCTOR_TOKEN":         r.opts.MCPToken,
					"CONDUCTOR_PROJECT":       project.ID,
					"CONDUCTOR_TASK_ID":       fence.TaskID,
					"CONDUCTOR_ATTEMPT_ID":    fence.AttemptID,
					"CONDUCTOR_LEASE_ID":      fence.LeaseID,
					"CONDUCTOR_FENCING_EPOCH": fmt.Sprintf("%d", fence.FencingEpoch),
				},
			},
		},
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	runtimeDir := filepath.Join(dir, ".conductor", "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(runtimeDir, "mcp.json")
	// 0600: the file carries a bearer token.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// runChecks executes the project's required checks in the worktree.
//
// The runner runs them, not the agent. That is the difference between evidence and a claim:
// a model saying "tests pass" is not accepted without a runner-observed exit code
// (DESIGN.md §25.5).
func (r *Runner) runChecks(ctx context.Context, dir string, checks []string) []domain.ValidationResult {
	var out []domain.ValidationResult
	for i, check := range checks {
		start := time.Now()
		runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		cmd := exec.CommandContext(runCtx, "sh", "-c", check)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		err := cmd.Run()
		cancel()

		exitCode := 0
		if err != nil {
			exitCode = 1
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exitCode = exitErr.ExitCode()
			}
		}
		out = append(out, domain.ValidationResult{
			CommandID:  fmt.Sprintf("check-%d", i),
			Command:    check,
			ExitCode:   exitCode,
			DurationMS: time.Since(start).Milliseconds(),
		})
		r.opts.Logger.Info("check finished", "command", check, "exit", exitCode,
			"duration", time.Since(start).Round(time.Millisecond).String())
	}
	return out
}

func scopeStrings(reservations []domain.ScopeReservation) []string {
	out := make([]string, 0, len(reservations))
	for _, r := range reservations {
		out = append(out, r.Resource())
	}
	return out
}

// coveredBy reports whether a changed path falls inside any declared scope.
func coveredBy(path string, declared []string) bool {
	for _, d := range declared {
		key := d
		if _, rest, found := strings.Cut(d, ":"); found {
			key = rest
		}
		if key == path || key == "." || strings.HasPrefix(path, strings.TrimSuffix(key, "/")+"/") {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
