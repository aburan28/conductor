package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/policy"
	"github.com/adamburan/conductor/internal/router"
)

// ---------------------------------------------------------------------------
// route — explain what the dispatch policy would decide for a task
// ---------------------------------------------------------------------------

func cmdRoute(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Bool("explain", true, "show the rationale (default)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor route — what would this task route to, and why?

Runs the deterministic router and the repository's dispatch policy against a task's current
facts, without dispatching anything. It is the answer to "why did (or would) this run on
that model" before you spend a token finding out.

  conductor route T-42
  conductor route T-42 --json

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: conductor route <task-ref>")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	var out routeExplain
	if err := api.Get(ctx, "/v1/tasks/"+positional[0]+"/route/explain"+client.Query("project", ref), &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	printRouteExplain(out)
	return nil
}

// routeExplain mirrors the server's /route/explain response.
type routeExplain struct {
	TaskRef  string                   `json:"task_ref"`
	Decision router.Decision          `json:"decision"`
	Dispatch *domain.DispatchDecision `json:"dispatch,omitempty"`
	Features domain.TaskFeatures      `json:"features"`
}

func printRouteExplain(r routeExplain) {
	d := r.Decision
	fmt.Printf("%s would route to:\n\n", r.TaskRef)
	if r.Dispatch != nil {
		dd := r.Dispatch
		fmt.Printf("  %s on %s  (lane %s, effort %s, tier %s)\n",
			orDash(dd.Model), orDash(dd.Harness), dd.Lane, orDash(string(dd.Effort)), orDash(string(dd.Tier)))
		if dd.Review == "required" {
			fmt.Println("  independent review required before merge")
		}
		fmt.Println("\n  Considered:")
		for _, c := range dd.Candidates {
			mark := "  ✗"
			if c.Chosen {
				mark = "  ✓"
			} else if c.Eligible {
				mark = "  ·"
			}
			line := fmt.Sprintf("%s %s on %s", mark, orDash(c.Model), orDash(c.Harness))
			if !c.Eligible && c.Reason != "" {
				line += " — " + c.Reason
			}
			fmt.Println("  " + line)
		}
		fmt.Println("\n  Rationale:")
		for _, line := range dd.Rationale {
			fmt.Printf("    • %s\n", line)
		}
		return
	}
	// Fall back to the tier router when no dispatch policy is set.
	fmt.Printf("  alias %s on %s → %s  (tier %s, effort %s)\n",
		orDash(d.Alias), orDash(d.Harness), orDash(d.Model), orDash(string(d.Tier)), orDash(string(d.Effort)))
	if d.RequireIndependentReview {
		fmt.Println("  independent review required")
	}
	if len(d.MatchedRules) > 0 {
		fmt.Printf("  policy rules: %s\n", strings.Join(d.MatchedRules, ", "))
	}
	fmt.Println("\n  Rationale:")
	for _, line := range d.Rationale {
		fmt.Printf("    • %s\n", line)
	}
}

// ---------------------------------------------------------------------------
// dispatch — plan an objective into tasks, then let the queue and runners execute
// ---------------------------------------------------------------------------

func cmdDispatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dispatch", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	title := fs.String("title", "", "title for a new task (when the argument is an objective, not a ref)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	var scopes scopeFlag
	fs.Var(&scopes, "scope", "resource the work will touch (repeatable)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor dispatch — send work to a model by policy

Given a task ref, requests an admission ticket for a runner attempt on it: the dispatch
policy chooses the model, the queue admits it when a slot is free, and a `+"`conductor worker`"+`
on some machine executes it. Given --title, files a task first, then dispatches it.

  conductor dispatch T-42
  conductor dispatch --title "add retry-aware routing" --scope dir:internal/router

The models are chosen by .conductor/dispatch.yaml; see `+"`conductor route <ref>`"+` to preview
the decision, and `+"`conductor queue`"+` to watch the admission line.

Flags:
`)
		fs.PrintDefaults()
	}
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

	taskRef := ""
	if len(positional) > 0 {
		taskRef = positional[0]
	}
	if taskRef == "" && *title == "" {
		return errors.New("usage: conductor dispatch <task-ref> | conductor dispatch --title <objective>")
	}
	if taskRef == "" {
		var view struct {
			Ref string `json:"ref"`
		}
		if err := api.Post(ctx, "/v1/projects/"+ref+"/tasks", map[string]any{
			"title": *title, "status": domain.TaskReady, "scopes": []domain.ScopeRequest(scopes),
		}, &view); err != nil {
			return err
		}
		taskRef = view.Ref
		fmt.Printf("Filed %s.\n", taskRef)
	}

	var ticket domain.AdmissionTicket
	err = api.Post(ctx, "/v1/projects/"+ref+"/queue",
		map[string]any{"kind": string(domain.TicketAttempt), "task": taskRef}, &ticket)
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(ticket)
	}
	switch ticket.State {
	case domain.TicketGranted:
		fmt.Printf("%s admitted for execution. A runner will pick it up.\n", taskRef)
	default:
		fmt.Printf("%s is queued at position %d; it starts when a slot frees up.\n", taskRef, ticket.Position)
	}
	fmt.Println("Watch it with `conductor queue` and `conductor status`.")
	return nil
}

// ---------------------------------------------------------------------------
// policy lint — validate the repository's policy files
// ---------------------------------------------------------------------------

func policyLint(args []string) error {
	fs := flag.NewFlagSet("policy lint", flag.ExitOnError)
	dir := fs.String("dir", ".", "repository root")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := config.FindRoot(*dir)
	if err != nil {
		return err
	}
	bundle, err := config.Load(root)
	if err != nil {
		return fmt.Errorf("loading policy: %w", err)
	}

	var issues []policy.Issue
	issues = append(issues, policy.LintRouterRules(bundle.Policies.Router.Rules)...)
	if !bundle.Dispatch.Empty() {
		_, di := policy.CompileDispatch(&bundle.Dispatch.DispatchPolicy)
		issues = append(issues, di...)
	}

	if *asJSON {
		return emit(map[string]any{"issues": issues, "ok": !policy.HasErrors(issues)})
	}
	if len(issues) == 0 {
		fmt.Printf("%s: policy is valid.\n", root)
		if bundle.Dispatch.Empty() {
			fmt.Println("  (no dispatch.yaml — the built-in tier router is in effect)")
		}
		return nil
	}
	errs, warns := 0, 0
	for _, i := range issues {
		fmt.Printf("  %s\n", i.String())
		if i.Severity == "error" {
			errs++
		} else {
			warns++
		}
	}
	fmt.Printf("\n%d error(s), %d warning(s).\n", errs, warns)
	if errs > 0 {
		os.Exit(3)
	}
	return nil
}
