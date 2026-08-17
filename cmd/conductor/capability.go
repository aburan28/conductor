package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/selector"
)

// Capability commands (DESIGN.md §7.3, §7.7).
//
// `conductor capabilities` answers "what can the people connected to this project actually
// do right now", and `conductor task assign` acts on the answer. Both exist as CLI commands
// and not only as MCP tools because the person deciding to escalate a task is as often a
// human looking at a terminal as an agent deciding mid-run.

// ---------------------------------------------------------------------------
// conductor capabilities
// ---------------------------------------------------------------------------

func cmdCapabilities(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("capabilities", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	tier := fs.String("require-tier", "", "ask whether a session at this tier or above is available")
	effort := fs.String("require-effort", "", "ask whether a session that can run at this effort is available")
	harness := fs.String("require-harness", "", "restrict the question to one harness")
	var capabilities stringList
	fs.Var(&capabilities, "require-capability", "capability tag the session must have (repeatable)")
	if _, err := parseFlags(fs, args); err != nil {
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

	query := url.Values{}
	for key, value := range map[string]string{
		"tier": *tier, "effort": *effort, "harness": *harness,
	} {
		if value != "" {
			query.Set(key, value)
		}
	}
	for _, c := range capabilities {
		query.Add("capability", c)
	}
	path := "/v1/projects/" + ref + "/capabilities"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	var inv coord.CapabilityInventory
	if err := api.Get(ctx, path, &inv); err != nil {
		return err
	}
	if *asJSON {
		return emit(inv)
	}
	printInventory(inv)
	return nil
}

func printInventory(inv coord.CapabilityInventory) {
	i := inv.Inventory
	fmt.Printf("%s\n\n", inv.Project)
	if i.Sessions == 0 {
		fmt.Println("  No live sessions. Nothing can be handed anywhere until someone runs `conductor wrap`.")
		return
	}

	fmt.Printf("  %d session(s), %d accepting work\n", i.Sessions, i.Available)
	fmt.Printf("  ceiling: tier %s, reasoning effort %s\n\n",
		orDash(string(i.MaxTier)), orDash(string(i.MaxEffort)))

	for _, s := range inv.Sessions {
		state := string(s.State)
		if !s.Available {
			state += " (not accepting)"
		}
		fmt.Printf("  %-12s %-9s %s\n", s.Principal, s.Harness, state)
		detail := fmt.Sprintf("       %s", orDash(s.Model))
		if s.Tier != "" {
			detail += fmt.Sprintf(" · tier %s", s.Tier)
		}
		if ceiling := s.MaxEffort; ceiling != "" {
			detail += fmt.Sprintf(" · effort %s", ceiling)
			if s.Effort != "" && s.Effort != ceiling {
				detail += fmt.Sprintf(" (running %s)", s.Effort)
			}
		}
		if !s.Resolved && s.Model != "" {
			// Worth saying plainly: an unresolved model can never satisfy a tier floor.
			detail += " · not in the model catalog"
		}
		fmt.Println(detail)
		if s.ActiveTaskRef != "" || s.QueuedOffers > 0 {
			line := "       "
			if s.ActiveTaskRef != "" {
				line += "on " + s.ActiveTaskRef
			}
			if s.QueuedOffers > 0 {
				if s.ActiveTaskRef != "" {
					line += ", "
				}
				line += fmt.Sprintf("%d offer(s) waiting", s.QueuedOffers)
			}
			fmt.Println(line)
		}
	}

	if len(i.Runners) > 0 {
		fmt.Printf("\n  Runners that could launch a fresh harness: ")
		names := make([]string, 0, len(i.Runners))
		for _, r := range i.Runners {
			names = append(names, r.Name)
		}
		fmt.Println(strings.Join(names, ", "))
	}

	if inv.Servable != nil {
		fmt.Println()
		if inv.Servable.Servable {
			fmt.Printf("  %s: yes, a live session can serve this.\n", inv.Servable.Requirement.Describe())
		} else {
			fmt.Printf("  %s: no.\n", inv.Servable.Requirement.Describe())
			for _, reason := range inv.Servable.Reasons {
				fmt.Printf("    - %s\n", reason)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// conductor task assign
// ---------------------------------------------------------------------------

func cmdTaskAssign(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task assign", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	tier := fs.String("require-tier", "", "minimum model tier (T0…T4)")
	effort := fs.String("require-effort", "", "minimum reasoning effort (none, low, medium, high, xhigh, max)")
	harness := fs.String("require-harness", "", "restrict to one harness")
	role := fs.String("require-role", "", "role the receiving session must serve")
	session := fs.String("to-session", "", "offer to a specific session (still checked against the floor)")
	ttl := fs.Int("ttl", 0, "seconds before an unanswered offer expires")
	var capabilities stringList
	fs.Var(&capabilities, "require-capability", "capability tag the session must have (repeatable)")

	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return errors.New("usage: conductor task assign <ref> [--require-effort xhigh] [--require-tier T4]")
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
		"session_id": *session,
		"require": map[string]any{
			"tier": *tier, "reasoning_effort": *effort, "capabilities": []string(capabilities),
			"harness": *harness, "role": *role,
		},
		"ttl_seconds": *ttl,
	}

	var result coord.AssignResult
	err = api.Post(ctx, "/v1/tasks/"+positional[0]+"/assign?project="+url.QueryEscape(ref), body, &result)
	if err != nil {
		// "No capacity" alone is not actionable. The refusal carries the per-session
		// reasons and the ceiling that is actually present, which is what tells the caller
		// whether to lower the floor, wait, or start a runner.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "no_capable_session" {
			printUnplaced(apiErr.Body, *asJSON)
			// Exit 3, the same code every other coordination refusal uses, so a script can
			// tell "nothing capable is online" from a genuine failure.
			os.Exit(3)
		}
		return err
	}
	if *asJSON {
		return emit(result)
	}

	fmt.Printf("%s offered to %s (%s on %s).\n",
		result.Assignment.TaskRef, result.Choice.Principal,
		orDash(result.Choice.Model), result.Choice.Harness)
	for _, line := range result.Choice.Rationale {
		fmt.Printf("  %s\n", line)
	}
	if len(result.Choice.Rejected) > 0 {
		fmt.Println("\n  Not chosen:")
		for _, r := range result.Choice.Rejected {
			fmt.Printf("    %-12s %s\n", r.Principal, r.Reason)
		}
	}
	fmt.Printf("\n  The offer expires %s unless it is answered.\n",
		result.Assignment.ExpiresAt.Local().Format("15:04:05"))
	return nil
}

// printUnplaced explains a refusal: which live sessions were considered, why each was passed
// over, and what the ceiling actually is.
func printUnplaced(body []byte, asJSON bool) {
	var out struct {
		Error    string `json:"error"`
		Rejected []struct {
			Principal string `json:"principal"`
			Reason    string `json:"reason"`
		} `json:"rejected"`
		Inventory selector.Inventory `json:"inventory"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", string(body))
		return
	}
	if asJSON {
		_ = emit(out)
		return
	}
	fmt.Println("Not placed: no live session meets the requirement.")
	for _, r := range out.Rejected {
		fmt.Printf("  %-12s %s\n", r.Principal, r.Reason)
	}
	fmt.Printf("\n  Highest available: tier %s at effort %s across %d live session(s).\n",
		orDash(string(out.Inventory.MaxTier)), orDash(string(out.Inventory.MaxEffort)),
		out.Inventory.Available)
	if len(out.Inventory.Runners) > 0 {
		fmt.Println("  A runner could launch a fresh harness instead: `conductor worker`.")
	}
}

// ---------------------------------------------------------------------------
// conductor inbox
// ---------------------------------------------------------------------------

func cmdInbox(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ExitOnError)
	session := fs.String("session", "", "session id (defaults to $CONDUCTOR_SESSION_ID)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	all := fs.Bool("all", false, "include answered and expired offers")
	accept := fs.String("accept", "", "accept an offer by assignment id")
	decline := fs.String("decline", "", "decline an offer by assignment id")
	note := fs.String("note", "", "reason to record with an accept or decline")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	api, _, err := mustClient()
	if err != nil {
		return err
	}

	if *accept != "" || *decline != "" {
		id := firstNonEmptyString(*accept, *decline)
		var out domain.Assignment
		if err := api.Post(ctx, "/v1/assignments/"+id+"/respond", map[string]any{
			"accept": *accept != "", "note": *note,
		}, &out); err != nil {
			return err
		}
		fmt.Printf("%s %s.\n", out.TaskRef, out.State)
		if out.State == domain.AssignmentAccepted {
			fmt.Printf("  Take it with: conductor task claim %s\n", out.TaskRef)
		}
		return nil
	}

	id := firstNonEmptyString(*session, os.Getenv("CONDUCTOR_SESSION_ID"))
	if id == "" {
		return errors.New("no session: pass --session, or run inside `conductor wrap`")
	}
	var out struct {
		Assignments []domain.Assignment `json:"assignments"`
	}
	path := "/v1/sessions/" + id + "/assignments"
	if *all {
		path += "?all=true"
	}
	if err := api.Get(ctx, path, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Assignments)
	}
	if len(out.Assignments) == 0 {
		fmt.Println("No work has been offered to this session.")
		return nil
	}
	for _, a := range out.Assignments {
		fmt.Printf("  %-8s %-10s %s\n", a.TaskRef, a.State, a.Requirement.Describe())
		if a.Rationale != "" {
			fmt.Printf("           %s\n", a.Rationale)
		}
		if a.State == domain.AssignmentOffered {
			fmt.Printf("           conductor inbox --accept %s\n", a.ID)
		}
	}
	return nil
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
