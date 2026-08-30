package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/localstate"
	"github.com/adamburan/conductor/internal/privacy"
)

// scopeFlag collects repeatable --scope values.
type scopeFlag []domain.ScopeRequest

func (s *scopeFlag) String() string { return "" }

func (s *scopeFlag) Set(v string) error {
	mode := domain.ModeWriteExclusive
	resource := v
	// "path/to/file.go:read" selects a mode without a second flag.
	if idx := strings.LastIndex(v, ":"); idx > 0 {
		switch suffix := v[idx+1:]; suffix {
		case "read", "read_shared":
			resource, mode = v[:idx], domain.ModeReadShared
		case "write", "write_exclusive":
			resource, mode = v[:idx], domain.ModeWriteExclusive
		case "review", "review_shared":
			resource, mode = v[:idx], domain.ModeReviewShared
		case "protected", "protected_exclusive":
			resource, mode = v[:idx], domain.ModeProtectedExclusive
		case "speculative", "speculative_write":
			resource, mode = v[:idx], domain.ModeSpeculativeWrite
		}
	}
	*s = append(*s, domain.ScopeRequest{Resource: resource, Mode: mode})
	return nil
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	var summary coord.StatusSummary
	if err := api.Get(ctx, "/v1/projects/"+ref+"/status", &summary); err != nil {
		return err
	}
	if *asJSON {
		return emit(summary)
	}

	fmt.Printf("%s\n\n", summary.Project)

	if len(summary.Counts) > 0 {
		keys := make([]string, 0, len(summary.Counts))
		for k := range summary.Counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, summary.Counts[k]))
		}
		fmt.Printf("  %s\n\n", strings.Join(parts, "   "))
	}

	if len(summary.Active) > 0 {
		fmt.Println("In flight")
		for _, t := range summary.Active {
			printTaskLine(t)
		}
		fmt.Println()
	}
	if len(summary.Ready) > 0 {
		fmt.Println("Ready")
		for _, t := range summary.Ready {
			printTaskLine(t)
		}
		fmt.Println()
	}
	if len(summary.Conflicts) > 0 {
		fmt.Println("Conflicts")
		for _, c := range summary.Conflicts {
			printConflict(c)
		}
		fmt.Println()
	}
	if len(summary.Presence) > 0 {
		fmt.Println("Live now")
		for _, p := range summary.Presence {
			printPresence(p)
		}
	}
	return nil
}

func printTaskLine(t privacy.TaskView) {
	title := t.Title
	if title == "" {
		title = "(private)"
	}
	fmt.Printf("  %-8s %-18s %-10s %s\n", t.Ref, t.Status, t.Owner, title)
	if len(t.Scopes) > 0 {
		fmt.Printf("           %s\n", strings.Join(t.Scopes, "  "))
	}
}

func printConflict(c privacy.ConflictView) {
	fmt.Printf("  [%s] %s ↔ %s (%s)\n", c.Severity, c.Mine.TaskRef, c.Other.TaskRef, c.Other.Owner)
	fmt.Printf("           %s → %s\n", c.Reason, c.Suggestion)
	if len(c.Resources) > 0 {
		fmt.Printf("           %s\n", strings.Join(c.Resources, "  "))
	}
}

func printPresence(p domain.PresenceEntry) {
	task := p.TaskRef
	if p.TaskTitle != "" {
		task += " — " + p.TaskTitle
	} else if p.TaskRef != "" {
		task += " — (private)"
	}
	sponsor := ""
	if p.SponsoredBy != "" {
		sponsor = " for " + p.SponsoredBy
	}
	fmt.Printf("  %-12s %-12s %-10s %s\n", p.Principal, p.Harness+sponsor, p.State, task)
	if len(p.Scopes) > 0 {
		fmt.Printf("           %s\n", strings.Join(p.Scopes, "  "))
	}
}

// ---------------------------------------------------------------------------
// presence
// ---------------------------------------------------------------------------

func cmdPresence(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("presence", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	watch := fs.Bool("watch", false, "refresh every few seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	show := func() error {
		var out struct {
			Presence []domain.PresenceEntry `json:"presence"`
		}
		if err := api.Get(ctx, "/v1/projects/"+ref+"/presence", &out); err != nil {
			return err
		}
		if *asJSON {
			return emit(out.Presence)
		}
		if len(out.Presence) == 0 {
			fmt.Println("No live sessions.")
			return nil
		}
		for _, p := range out.Presence {
			printPresence(p)
		}
		return nil
	}

	if !*watch {
		return show()
	}
	for {
		fmt.Print("\033[H\033[2J") // clear
		fmt.Printf("%s — %s\n\n", ref, time.Now().Format(time.Kitchen))
		if err := show(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

// ---------------------------------------------------------------------------
// check
// ---------------------------------------------------------------------------

func cmdCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	summary := fs.String("summary", "", "one sentence describing the work")
	externalRef := fs.String("ref", "", "issue key, if any")
	asJSON := fs.Bool("json", false, "machine-readable output")
	var scopes scopeFlag
	fs.Var(&scopes, "scope", "resource you intend to touch (repeatable), e.g. path:internal/api.go or dir:internal/router:read")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor check — can I start this work?

Runs the duplicate and conflict engine without changing anything. Exit code 0 means go,
3 means someone else is already there.

  conductor check --summary "add retry-aware routing" --scope dir:internal/router

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *summary == "" && len(scopes) == 0 {
		return errors.New("nothing to check: pass --summary and/or --scope")
	}

	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	var decision coord.IntentDecision
	if err := api.Post(ctx, "/v1/projects/"+ref+"/intents/check", map[string]any{
		"summary": *summary, "scopes": []domain.ScopeRequest(scopes), "external_ref": *externalRef,
	}, &decision); err != nil {
		return err
	}
	if *asJSON {
		return emit(decision)
	}

	switch decision.Outcome {
	case domain.OutcomeAllow:
		fmt.Println("Clear. Nothing else in flight touches this.")
		return nil
	case domain.OutcomeAllowWithWarning:
		fmt.Printf("Proceed with care: %s\n", decision.Reason)
	default:
		fmt.Printf("%s: %s\n", decision.Outcome, decision.Reason)
	}
	if decision.Advice != "" {
		fmt.Printf("\n  %s\n", decision.Advice)
	}
	for _, d := range decision.Duplicates {
		title := d.Title
		if title == "" {
			title = "(private)"
		}
		kind := "similar"
		if d.Exact {
			kind = "identical"
		}
		fmt.Printf("\n  %-9s %s  %s  owner %s  [%s]\n", kind, d.TaskRef, title, d.Owner, d.Status)
	}
	for _, c := range decision.Conflicts {
		fmt.Printf("\n  %-9s %s held by %s for %s (%s)\n",
			c.Outcome, c.ResourceKey, c.HolderOwner, c.HolderTaskRef, c.HolderMode)
	}

	if decision.Outcome.Blocks() {
		// Exit 3 so a hook or wrapper can stop the edit without treating this as a crash.
		os.Exit(3)
	}
	return nil
}

// ---------------------------------------------------------------------------
// conflicts
// ---------------------------------------------------------------------------

func cmdConflicts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("conflicts", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	resolve := fs.String("resolve", "", "conflict id to resolve")
	state := fs.String("state", "resolved", "acknowledged | resolved | ignored")
	note := fs.String("note", "", "why (required when ignoring)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	if *resolve != "" {
		var out map[string]any
		if err := api.Post(ctx, "/v1/conflicts/"+*resolve+"/resolve",
			map[string]any{"state": *state, "note": *note}, &out); err != nil {
			return err
		}
		fmt.Printf("Conflict %s marked %s.\n", *resolve, *state)
		return nil
	}

	var out struct {
		Conflicts []privacy.ConflictView `json:"conflicts"`
	}
	if err := api.Get(ctx, "/v1/projects/"+ref+"/conflicts", &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Conflicts)
	}
	if len(out.Conflicts) == 0 {
		fmt.Println("No open conflicts.")
		return nil
	}
	for _, c := range out.Conflicts {
		printConflict(c)
		fmt.Printf("           id %s\n\n", c.ID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// task
// ---------------------------------------------------------------------------

func cmdTask(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: conductor task <list|show|create|claim|release|handoff|assign|export>")
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "list":
		return taskList(ctx, rest)
	case "show":
		return taskShow(ctx, rest)
	case "create":
		return taskCreate(ctx, rest)
	case "claim":
		return taskClaim(ctx, rest)
	case "release":
		return taskRelease(ctx, rest)
	case "handoff":
		return taskHandoff(ctx, rest)
	case "assign":
		return cmdTaskAssign(ctx, rest)
	case "export":
		return taskExport(ctx, rest)
	default:
		return fmt.Errorf("unknown task subcommand %q", sub)
	}
}

func taskList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task list", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	all := fs.Bool("all", false, "include finished tasks")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	query := client.Query("open", boolStr(!*all))
	var out struct {
		Tasks []privacy.TaskView `json:"tasks"`
	}
	if err := api.Get(ctx, "/v1/projects/"+ref+"/tasks"+query, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Tasks)
	}
	if len(out.Tasks) == 0 {
		fmt.Println("No tasks.")
		return nil
	}
	for _, t := range out.Tasks {
		printTaskLine(t)
	}
	return nil
}

func taskShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task show", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: conductor task show <ref>")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	if *asJSON {
		var view privacy.TaskView
		if err := api.Get(ctx, "/v1/tasks/"+positional[0]+client.Query("project", ref), &view); err != nil {
			return err
		}
		return emit(view)
	}
	card, err := api.Raw(ctx, "/v1/tasks/"+positional[0]+"/card"+client.Query("project", ref))
	if err != nil {
		return err
	}
	fmt.Print(string(card))
	return nil
}

func taskCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task create", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	title := fs.String("title", "", "task title")
	objective := fs.String("objective", "", "what done looks like")
	externalRef := fs.String("ref", "", "issue key")
	visibility := fs.String("visibility", "", "private | team_summary | team_artifacts")
	priority := fs.Int("priority", 0, "higher dispatches first")
	ready := fs.Bool("ready", true, "mark the task ready for dispatch")
	asJSON := fs.Bool("json", false, "machine-readable output")
	var scopes scopeFlag
	fs.Var(&scopes, "scope", "resource this task will touch (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *title == "" {
		return errors.New("--title is required")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	status := domain.TaskProposed
	if *ready {
		status = domain.TaskReady
	}
	var view privacy.TaskView
	if err := api.Post(ctx, "/v1/projects/"+ref+"/tasks", map[string]any{
		"title": *title, "objective": *objective, "external_ref": *externalRef,
		"visibility": *visibility, "priority": *priority, "status": status,
		"scopes": []domain.ScopeRequest(scopes),
	}, &view); err != nil {
		return err
	}
	if *asJSON {
		return emit(view)
	}
	fmt.Printf("Created %s — %s\n", view.Ref, view.Title)
	return nil
}

func taskClaim(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task claim", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	next := fs.Bool("next", false, "claim the highest-priority ready task")
	asJSON := fs.Bool("json", false, "machine-readable output")
	var scopes scopeFlag
	fs.Var(&scopes, "scope", "resource to reserve with the claim (repeatable)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	body := map[string]any{
		"harness": "cli", "role": string(domain.RoleImplementer),
		"scopes": []domain.ScopeRequest(scopes), "allow_warnings": true,
	}
	path := "/v1/projects/" + ref + "/claim-next"
	if !*next {
		if len(positional) < 1 {
			return errors.New("usage: conductor task claim <ref> | conductor task claim --next")
		}
		path = "/v1/tasks/" + positional[0] + "/claim" + client.Query("project", ref)
	}

	var out struct {
		Task  privacy.TaskView `json:"task"`
		Fence domain.Fence     `json:"fence"`
		Lease domain.Lease     `json:"lease"`
	}
	if err := api.Post(ctx, path, body, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	fmt.Printf("Claimed %s — %s\n", out.Task.Ref, out.Task.Title)
	fmt.Printf("  lease   %s  epoch %d  expires %s\n",
		out.Fence.LeaseID, out.Fence.FencingEpoch, out.Lease.ExpiresAt.Format(time.Kitchen))
	if len(out.Task.Scopes) > 0 {
		fmt.Printf("  holding %s\n", strings.Join(out.Task.Scopes, "  "))
	}
	fmt.Printf("\n  Your lease expires unless it is renewed. Run `conductor wrap <tool>` so a\n" +
		"  sidecar heartbeats while you work, or release it when you stop.\n")
	return nil
}

func taskRelease(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task release", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	state := fs.String("state", "", "next task status (default: decided by policy)")
	note := fs.String("note", "", "why you are handing it back")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: conductor task release <ref>")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	var out map[string]any
	if err := api.Post(ctx, "/v1/tasks/"+positional[0]+"/release"+client.Query("project", ref),
		map[string]any{"next_status": *state, "reason": *note}, &out); err != nil {
		return err
	}
	fmt.Printf("Released %s. Its reserved scopes are free immediately.\n", positional[0])
	return nil
}

func taskHandoff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task handoff", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	to := fs.String("to", "", "target harness, e.g. codex")
	nextAction := fs.String("next", "", "what the next session should do first")
	note := fs.String("note", "", "one completed-work note")
	tier := fs.String("require-tier", "", "offer the work to a session at this tier or above")
	effort := fs.String("require-effort", "", "offer the work to a session that can run at this effort")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: conductor task handoff <ref> --to <harness>")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	bundle := map[string]any{"recommended_next_action": *nextAction}
	if *note != "" {
		bundle["completed_work"] = []string{*note}
	}
	body := map[string]any{"to_harness": *to, "bundle": bundle}
	// A handoff that names a floor is a delegation: the server packages the bundle and then
	// offers the task to a session that clears the floor.
	if *tier != "" || *effort != "" {
		body["require"] = map[string]any{"tier": *tier, "reasoning_effort": *effort}
	}

	var out coord.DelegateResult
	if err := api.Post(ctx, "/v1/tasks/"+positional[0]+"/handoff"+client.Query("project", ref),
		body, &out); err != nil {
		return err
	}
	target := *to
	if target == "" {
		target = "whoever picks it up next"
	}
	fmt.Printf("Handed %s off to %s. The bundle carries decisions and open questions, not your chat.\n",
		positional[0], target)
	switch {
	case out.Assignment != nil:
		fmt.Printf("  Offered to %s (%s), which meets %s.\n",
			out.Choice.Principal, orDash(out.Choice.Model), out.Assignment.Requirement.Describe())
	case out.Unplaced != "":
		// The bundle is written either way; say plainly that the work is sitting ready.
		fmt.Printf("  Not offered to anyone: %s\n", out.Unplaced)
		// Per-session reasons, not just the global ceiling: with `--to codex` the ceiling
		// can look sufficient while every live session is on the wrong harness.
		if out.Choice != nil {
			for _, r := range out.Choice.Rejected {
				fmt.Printf("    %-12s %s\n", r.Principal, r.Reason)
			}
		}
		fmt.Println("  The task is ready for whoever can take it.")
	}
	return nil
}

func taskExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task export", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	output := fs.String("output", "", "write to a file instead of stdout")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: conductor task export <ref> [--output task.md]")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	card, err := api.Raw(ctx, "/v1/tasks/"+positional[0]+"/card"+client.Query("project", ref))
	if err != nil {
		return err
	}
	if *output == "" {
		fmt.Print(string(card))
		return nil
	}
	if err := os.WriteFile(*output, card, 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", *output)
	return nil
}

// ---------------------------------------------------------------------------
// scope
// ---------------------------------------------------------------------------

func cmdScope(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: conductor scope <add|list>")
	}
	sub, rest := args[0], args[1:]

	api, creds, err := mustClient()
	if err != nil {
		return err
	}

	switch sub {
	case "list":
		fs := flag.NewFlagSet("scope list", flag.ExitOnError)
		project := fs.String("project", "", "project id or slug")
		asJSON := fs.Bool("json", false, "machine-readable output")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		ref, err := projectRef(*project, creds)
		if err != nil {
			return err
		}
		var out struct {
			Reservations []domain.ScopeReservation `json:"reservations"`
		}
		if err := api.Get(ctx, "/v1/projects/"+ref+"/reservations", &out); err != nil {
			return err
		}
		if *asJSON {
			return emit(out.Reservations)
		}
		if len(out.Reservations) == 0 {
			fmt.Println("Nothing reserved.")
			return nil
		}
		for _, r := range out.Reservations {
			fmt.Printf("  %-40s %-20s %s\n", r.Resource(), r.Mode, r.Source)
		}
		return nil

	case "add":
		fs := flag.NewFlagSet("scope add", flag.ExitOnError)
		project := fs.String("project", "", "project id or slug")
		var scopes scopeFlag
		fs.Var(&scopes, "scope", "resource to reserve (repeatable)")
		positional, err := parseFlags(fs, rest)
		if err != nil {
			return err
		}
		if len(positional) < 1 {
			return errors.New("usage: conductor scope add <task-ref> --scope path:file.go")
		}
		// Bare resources after the task ref are treated as scopes, so both
		// `scope add T-1 path:a.go` and `scope add T-1 --scope path:a.go` work.
		for _, arg := range positional[1:] {
			_ = scopes.Set(arg)
		}
		if len(scopes) == 0 {
			return errors.New("no scopes given")
		}
		ref, err := projectRef(*project, creds)
		if err != nil {
			return err
		}

		var result coord.ExpandScopeResult
		err = api.Post(ctx, "/v1/tasks/"+positional[0]+"/scopes"+client.Query("project", ref),
			map[string]any{"scopes": []domain.ScopeRequest(scopes), "source": "declared"}, &result)
		if err != nil {
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.Blocked() {
				fmt.Printf("Blocked: %s\n", result.Advice)
				for _, c := range result.Conflicts {
					fmt.Printf("  %s held by %s for %s\n", c.ResourceKey, c.HolderOwner, c.HolderTaskRef)
				}
				os.Exit(3)
			}
			return err
		}
		fmt.Printf("Reserved %d resource(s).\n", len(result.Granted))
		for _, c := range result.Conflicts {
			fmt.Printf("  warning: overlaps %s (%s by %s)\n", c.ResourceKey, c.HolderMode, c.HolderOwner)
		}
		return nil

	default:
		return fmt.Errorf("unknown scope subcommand %q", sub)
	}
}

// ---------------------------------------------------------------------------
// wrap
// ---------------------------------------------------------------------------

// cmdWrap registers a session, heartbeats in the background, and launches the requested tool
// (DESIGN.md §17.1).
//
// The heartbeat is a direct HTTP call from this process, not an MCP tool. A model should
// never spend tokens telling the server it is still alive (§7.2).
func cmdWrap(ctx context.Context, args []string) error {
	// Capability flags are parsed off the front only. Everything from the harness name
	// onwards belongs to the harness — `conductor wrap claude --model opus` must pass
	// `--model opus` to Claude, not eat it here.
	original := args
	declared, args := parseWrapFlags(args)
	// The consumed prefix is replayed verbatim if `conductor resume` ever relaunches this
	// session, so a revived wrap advertises the same capabilities it registered with.
	conductorFlags := original[:len(original)-len(args)]
	if len(args) == 0 {
		return errors.New("usage: conductor wrap [--model M] [--effort E] <harness> [args…]")
	}
	// Without this, a mistyped Conductor flag becomes the harness name and surfaces as a
	// baffling "executable file not found: --moddel".
	if strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("unknown flag %q before the harness name; "+
			"conductor wrap accepts --model, --effort, --max-effort, --alias, and --role", args[0])
	}
	tool, toolArgs := args[0], args[1:]

	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef("", creds)
	if err != nil {
		return err
	}

	branch, _ := gitOutput(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	baseSHA, _ := gitOutput(ctx, "rev-parse", "HEAD")
	host, _ := os.Hostname()
	caps := detectCapabilities(declared, toolArgs)

	var session domain.Session
	if err := api.Post(ctx, "/v1/projects/"+ref+"/sessions", map[string]any{
		"harness": tool, "machine_id": host, "branch": branch, "base_sha": baseSHA,
		"capabilities": caps,
	}, &session); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Conductor session %s registered for %s. Teammates can see that you are here.\n",
		session.ID[:8], tool)
	printCapabilityNotice(session)

	// Admission: if the project caps how many sessions run at once, take a ticket and wait
	// for a slot before launching. The sidecar heartbeats the ticket so the slot is not
	// reclaimed while the session runs; a closed session releases it (`admission` below).
	admission := admitSession(ctx, api, ref, session.ID, tool, caps.Model)

	// paused is flipped by SIGUSR1/SIGUSR2 from `conductor pause` and `conductor resume`,
	// and by a control the dashboard records for this session. While paused this sidecar
	// keeps heartbeating as `paused`: the session is present but frozen, must not be
	// offered work, and presence should say why nothing is moving.
	var paused atomic.Bool
	// controlCh carries a dashboard-requested pause or resume from the heartbeat loop —
	// which runs before the child exists — to the signal goroutine that owns the child's
	// signals. Buffered and lossy on purpose: a dropped control stays pending on the
	// server, so the next heartbeat offers it again.
	controlCh := make(chan domain.SessionControl, 1)

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				state := domain.SessionWorking
				if paused.Load() {
					state = domain.SessionPaused
				}
				b, _ := gitOutput(heartbeatCtx, "rev-parse", "--abbrev-ref", "HEAD")
				// The response is not discarded: it carries any control the dashboard
				// has recorded for this session, which this sidecar is the only party
				// able to carry out.
				var resp domain.Session
				if err := api.Post(heartbeatCtx, "/v1/sessions/"+session.ID+"/heartbeat",
					map[string]any{
						"state": string(state), "branch": b,
						"control_ack": string(controlAck(paused.Load())),
					}, &resp); err == nil {
					if act := controlAction(resp.PendingControl, paused.Load()); act != "" {
						select {
						case controlCh <- act:
						default: // still applying an earlier control; it will be offered again
						}
					}
				}
				admission.heartbeat(heartbeatCtx)
			}
		}
	}()

	// Use a detached context so the session is closed even when the user hit Ctrl-C.
	closeSession := func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = api.Post(closeCtx, "/v1/sessions/"+session.ID+"/close", nil, nil)
		admission.release(closeCtx)
	}

	// Auto-mount Conductor's MCP tools inside the session so the agent can claim, reserve,
	// and report from within its own run — unless the user already passed an MCP config.
	mcpArgs, mcpCleanup := wrapMCPArgs(tool, toolArgs, session.ID, ref, creds.Endpoint)
	defer mcpCleanup()
	launchArgs := append(append([]string(nil), toolArgs...), mcpArgs...)

	cmd := exec.CommandContext(ctx, tool, launchArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(),
		"CONDUCTOR_SESSION_ID="+session.ID,
		"CONDUCTOR_PROJECT="+ref,
	)

	// Harness log tee: mirror the harness's combined output into the localstate
	// sessions directory so the dashboard can stream it (GET /v1/sessions/{id}/logs).
	// Headless wraps (piped stdout, the way scripts and the runner drive wrap) always
	// tee; an interactive wrap opts in with CONDUCTOR_HARNESS_LOG=1 — teeing there
	// hands the child a pipe instead of the terminal, which would downgrade the
	// interactive harness's rendering, so the default for terminals is no capture.
	if capture, _ := harnessLogTee(session.ID); capture != nil {
		defer capture.Close()
		cmd.Stdout = io.MultiWriter(os.Stdout, capture)
		cmd.Stderr = io.MultiWriter(os.Stderr, capture)
	}

	// Token usage. The harness writes its own usage log as it runs; this sidecar reads that
	// log — numbers, model, timestamp, nothing else — and reports hourly buckets, so the
	// team's usage is one report rather than three tools' private counters. Off with
	// CONDUCTOR_USAGE=off.
	var collector *usageCollector
	if !usageDisabled(os.Getenv) {
		launchDir, _ := os.Getwd()
		collector = newUsageCollector(api, session.ID, tool, launchDir)
		go collector.run(heartbeatCtx, time.Minute)
	}

	if err := cmd.Start(); err != nil {
		stopHeartbeat()
		closeSession()
		return err
	}

	// Record the session locally so `conductor pause` and `conductor resume` can find it.
	// Losing the record only loses pause/resume for this session, so a failure here is a
	// warning, never a reason to kill a session that has already started.
	cwd, _ := os.Getwd()
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = 0
	}
	rec := localstate.Record{
		ID: session.ID, Harness: tool, Command: tool,
		Args: toolArgs, ResumeArgs: localstate.ResumeInvocation(tool), WrapFlags: conductorFlags,
		Cwd: cwd, PID: cmd.Process.Pid, PGID: pgid, WrapPID: os.Getpid(),
		TTY: localstate.CurrentTTY(), TermProgram: os.Getenv("TERM_PROGRAM"),
		SessionID: session.ID, Project: ref,
		Wrapped: true, Status: localstate.StatusRunning, StartedAt: time.Now(),
	}
	if err := localstate.Save(rec); err != nil {
		fmt.Fprintf(os.Stderr, "conductor: pause/resume unavailable for this session: %v\n", err)
	}

	// SIGUSR1 pauses: SIGSTOP to the child only, so this sidecar keeps running and the shell
	// never reclaims the terminal — which is what lets SIGCONT later hand the keyboard
	// straight back. SIGUSR2 wakes. A control the dashboard recorded arrives on controlCh
	// and does the same thing. Both report the state change immediately rather than
	// waiting for the next tick, and the report's control_ack clears the request.
	apply := func(pause bool) {
		sig := syscall.SIGCONT
		if pause {
			sig = syscall.SIGSTOP
		}
		_ = syscall.Kill(cmd.Process.Pid, sig)
		paused.Store(pause)
		state := domain.SessionWorking
		if pause {
			state = domain.SessionPaused
		}
		_ = api.Post(heartbeatCtx, "/v1/sessions/"+session.ID+"/heartbeat",
			map[string]any{"state": string(state), "control_ack": string(controlAck(pause))}, nil)
	}
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGUSR1, syscall.SIGUSR2)
	defer signal.Stop(sigCh)
	go func() {
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case sig := <-sigCh:
				apply(sig == syscall.SIGUSR1)
			case act := <-controlCh:
				apply(act == domain.ControlPause)
			}
		}
	}()

	// Decide the session's fate on the way down. A machine shutdown (SIGTERM from
	// init/systemd/launchd) or a closed terminal / dropped SSH connection (SIGHUP) should
	// KEEP the session for resume; a deliberate interrupt (SIGINT / Ctrl-C) should FORGET it,
	// matching a normal quit. The handler records that intent *before* it tears the harness
	// down, so the exit path never has to guess: the child cannot die (and cmd.Wait cannot
	// return) until after outcome is set. `captured` lets the exit path wait for a shutdown
	// backup to finish before it calls os.Exit.
	const (
		outcomeUnset int32 = iota
		outcomeKeep
		outcomeForget
	)
	var outcome atomic.Int32
	captured := make(chan struct{})
	termCh := make(chan os.Signal, 3)
	signal.Notify(termCh, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(termCh)
	go func() {
		// Wait on the signal alone, not on heartbeatCtx: this process's context is cancelled
		// only by SIGINT/SIGTERM (main's NotifyContext), which are the very signals delivered
		// here too — so selecting on ctx.Done() would race the signal and could drop the
		// keep/forget intent. On a clean exit no signal arrives and this goroutine simply ends
		// with the process.
		sig, ok := <-termCh
		if !ok {
			return
		}
		keep := sig != syscall.SIGINT
		if keep {
			outcome.Store(outcomeKeep)
			if err := localstate.KeepForResume(rec); err != nil {
				fmt.Fprintf(os.Stderr, "conductor: could not save this session for resume: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "\nConductor saved this session; `conductor resume` reopens it.\n")
			}
			// Push off-host synchronously here (bounded), so a terminated cloud instance's
			// records reach S3 before the process goes away. close(captured) then releases the
			// exit path.
			backupOnShutdown(rec)
		} else {
			outcome.Store(outcomeForget)
		}
		close(captured)
		// Tear the harness down. SIGCONT first in case it was paused (SIGSTOP'd by
		// `conductor pause`), or a stopped child would ignore the term signal and hang
		// cmd.Wait. main's ctx cancellation also kills the child on SIGTERM/SIGINT, but not on
		// SIGHUP, so this is what guarantees teardown there.
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGCONT)
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
	}()

	runErr := cmd.Wait()

	// Decide the record's fate (the exit-code path below calls os.Exit, which skips defers).
	// The handler above sets `outcome` before it kills the child, so on a signal-driven exit
	// the intent is already recorded. When a signal killed the child but no intent is set yet
	// — main's ctx-cancel can kill it a hair before the handler runs — wait briefly for the
	// handler, which only ever needs to finish a small file write.
	if outcome.Load() == outcomeUnset && killedBySignal(runErr) {
		select {
		case <-captured:
		case <-time.After(2 * time.Second):
		}
	}
	kept, _ := localstate.Get(rec.ID)
	switch {
	case outcome.Load() == outcomeForget:
		// The user interrupted the wrapped session: forget it, as a quit would.
		_ = localstate.Remove(rec.ID)
	case outcome.Load() == outcomeKeep || kept.Saved || killedBySignal(runErr):
		// Shutdown, closed terminal, an explicit `sessions save`, or a crash: keep it so
		// `conductor resume` can reopen it. (KeepForResume is idempotent if the handler
		// already ran.)
		if err := localstate.KeepForResume(rec); err != nil {
			fmt.Fprintf(os.Stderr, "conductor: could not save this session for resume: %v\n", err)
		}
	default:
		// Clean exit the user chose: a stale record would make the next pause chase a dead pid.
		_ = localstate.Remove(rec.ID)
	}
	stopHeartbeat()
	if collector != nil {
		// The harness has finished writing; report the tail, on a context that survives Ctrl-C.
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		collector.flush(flushCtx)
		cancel()
		if calls, tokens, cost := collector.totals(); calls > 0 {
			line := fmt.Sprintf("Conductor recorded %s calls, %s tokens", humanTokens(calls), humanTokens(tokens))
			if cost > 0 {
				line += ", " + dollars(cost)
			}
			if collector.lastErr != nil {
				line += " (last report failed: " + errString(collector.lastErr) + ")"
			}
			fmt.Fprintln(os.Stderr, line+" for this session. `conductor usage` has the team view.")
		}
	}
	closeSession()

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return runErr
	}
	return nil
}

func gitOutput(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return ""
}

// killedBySignal reports whether a finished child was terminated by a signal (shutdown's
// SIGTERM/SIGKILL, a terminal's SIGHUP, or a crash) rather than exiting on its own. A
// signalled exit means the session did not end by the user's choice, so its resume record is
// worth keeping.
func killedBySignal(runErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

// harnessLogTee opens the session's harness log for appending and reports whether the
// wrap should capture: always when stdout is not a terminal (headless), and for a
// terminal only when CONDUCTOR_HARNESS_LOG=1 opts in — capture replaces the child's
// stdio with pipes, which an interactive harness would notice.
func harnessLogTee(sessionID string) (*os.File, string) {
	if v := strings.TrimSpace(os.Getenv("CONDUCTOR_HARNESS_LOG")); v != "1" {
		if fi, err := os.Stdout.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			// Headless: fall through to capture below.
		} else {
			return nil, ""
		}
	}
	dir, err := localstate.Dir()
	if err != nil {
		return nil, ""
	}
	path := filepath.Join(dir, sessionID, "harness.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, ""
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, ""
	}
	return f, path
}
