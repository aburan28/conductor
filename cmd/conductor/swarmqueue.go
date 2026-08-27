package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// swarm — the team's pooled execution capacity and shareable budget
// ---------------------------------------------------------------------------

func cmdSwarm(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return swarmStatus(ctx, args[1:])
		case "join":
			return swarmJoin(ctx, args[1:])
		}
	}
	return swarmStatus(ctx, args)
}

func swarmStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("swarm", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	watch := fs.Bool("watch", false, "refresh every few seconds")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor swarm — the team's pooled capacity, and who has budget to share

Rolls up the runners and live sessions contributing execution capacity right now, each
member's token position, and how deep the admission queue is. A coworker with headroom to
spare can see it here and hand some over with `+"`conductor budget share`"+`.

  conductor swarm
  conductor swarm --watch
  conductor swarm join --endpoint https://conductor.team --project myrepo

Flags:
`)
		fs.PrintDefaults()
	}
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
		var view coord.SwarmView
		if err := api.Get(ctx, "/v1/projects/"+ref+"/swarm", &view); err != nil {
			return err
		}
		if *asJSON {
			return emit(view)
		}
		printSwarm(view)
		return nil
	}
	if !*watch {
		return show()
	}
	for {
		fmt.Print("\033[H\033[2J")
		fmt.Printf("%s swarm — %s\n\n", ref, time.Now().Format(time.Kitchen))
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

func printSwarm(v coord.SwarmView) {
	fmt.Printf("Capacity: %d runner(s), %d session(s) accepting work, %d free slot(s), %d waiting in queue\n\n",
		v.Capacity.Runners, v.Capacity.SessionsAccepting, v.Capacity.SlotsFree, v.QueueDepth)
	if len(v.Contributors) == 0 {
		fmt.Println("No one is contributing capacity right now.")
		fmt.Println("A teammate joins with: conductor swarm join --endpoint <url> --project <slug>")
		return
	}
	fmt.Printf("  %-14s %-8s %-10s %-14s %s\n", "WHO", "KIND", "STATE", "LOAD", "BUDGET LEFT")
	for _, c := range v.Contributors {
		load := ""
		if c.MaxConcurrent > 0 {
			load = fmt.Sprintf("%d/%d", c.InFlight, c.MaxConcurrent)
		} else if !c.Accepting {
			load = "not accepting"
		} else {
			load = "ready"
		}
		budget := "—"
		if c.Budget != nil {
			budget = humanTokens(c.Budget.Remaining)
		}
		who := c.Principal
		if c.Name != "" {
			who = c.Name
		}
		fmt.Printf("  %-14s %-8s %-10s %-14s %s\n", who, c.Kind, orDash(c.State), load, budget)
	}
	fmt.Println("\nShare budget with a teammate: conductor budget share <who> <tokens>")
}

// swarmJoin points this machine's login at a shared control plane, then explains how to
// contribute — a runner for autonomous work, or wrapped sessions for interactive work. It is
// login plus guidance: the actual capacity is contributed by `conductor worker` and
// `conductor wrap`, which the message names.
func swarmJoin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("swarm join", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "the team's control plane URL")
	token := fs.String("token", "", "your access token (from a teammate's `conductor member add`)")
	project := fs.String("project", "", "project slug")
	asRunner := fs.Bool("runner", false, "start a runner immediately after joining")
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
		return errors.New("no token: pass --token (ask a teammate to run `conductor member add <you>`)")
	}

	api := client.New(creds.Endpoint, creds.Token)
	var who struct {
		Principal struct {
			Handle string `json:"handle"`
		} `json:"principal"`
		Projects []struct {
			Slug string `json:"slug"`
		} `json:"projects"`
	}
	if err := api.Get(ctx, "/v1/whoami", &who); err != nil {
		return fmt.Errorf("joining %s: %w", creds.Endpoint, err)
	}
	if creds.Project == "" && len(who.Projects) == 1 {
		creds.Project = who.Projects[0].Slug
	}
	if err := client.SaveCredentials(creds); err != nil {
		return err
	}
	ref, _ := projectRef(*project, creds)
	fmt.Printf("Joined %s as %s on %s.\n\n", creds.Endpoint, who.Principal.Handle, orDash(ref))
	fmt.Println("Contribute capacity:")
	fmt.Println("  • interactive work:  conductor wrap claude       (your sessions take work offered to them)")
	fmt.Println("  • autonomous work:   conductor worker            (this machine executes queued tasks)")
	fmt.Println("  • spare budget:      conductor budget share <teammate> <tokens>")
	fmt.Println("\nSee the whole swarm with `conductor swarm`.")

	if *asRunner {
		fmt.Println("\nStarting a runner…")
		return cmdWorker(ctx, nil)
	}
	return nil
}

// ---------------------------------------------------------------------------
// queue — the admission line
// ---------------------------------------------------------------------------

func cmdQueue(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "cancel" {
		return queueCancel(ctx, args[1:])
	}
	fs := flag.NewFlagSet("queue", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	all := fs.Bool("all", false, "include recently closed tickets")
	watch := fs.Bool("watch", false, "refresh every few seconds")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor queue — who is waiting for a slot, and who holds one

When a project caps how many sessions or attempts run at once, work past the cap waits here
in arrival order. The scheduler grants tickets as slots free up.

  conductor queue
  conductor queue --watch
  conductor queue cancel <ticket-id>

Flags:
`)
		fs.PrintDefaults()
	}
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
		var view coord.QueueView
		if err := api.Get(ctx, "/v1/projects/"+ref+"/queue"+client.Query("closed", boolStr(*all)), &view); err != nil {
			return err
		}
		if *asJSON {
			return emit(view)
		}
		printQueue(view)
		return nil
	}
	if !*watch {
		return show()
	}
	for {
		fmt.Print("\033[H\033[2J")
		fmt.Printf("%s queue — %s\n\n", ref, time.Now().Format(time.Kitchen))
		if err := show(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func printQueue(v coord.QueueView) {
	p := v.Policy
	caps := "unlimited"
	if p.MaxActiveSessions > 0 || p.MaxConcurrentAttempts > 0 {
		caps = fmt.Sprintf("≤%d sessions, ≤%d attempts", p.MaxActiveSessions, p.MaxConcurrentAttempts)
	}
	fmt.Printf("Limits: %s   Active now: %d session(s), %d attempt(s)\n\n",
		caps, v.Active.Sessions, v.Active.Attempts)
	if len(v.Tickets) == 0 {
		fmt.Println("Nothing waiting. Work starts immediately while slots are free.")
		return
	}
	fmt.Printf("  %-4s %-8s %-14s %-10s %-16s %s\n", "POS", "STATE", "WHO", "KIND", "WHAT", "WAITING")
	for _, t := range v.Tickets {
		pos := "—"
		if t.State == domain.TicketQueued {
			pos = fmt.Sprintf("%d", t.Position)
		}
		what := t.Model
		if t.TaskRef != "" {
			what = t.TaskRef
		}
		waited := time.Since(t.RequestedAt).Round(time.Second).String()
		fmt.Printf("  %-4s %-8s %-14s %-10s %-16s %s\n",
			pos, t.State, orDash(t.Principal), t.Kind, orDash(what), waited)
	}
}

func queueCancel(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: conductor queue cancel <ticket-id>")
	}
	api, _, err := mustClient()
	if err != nil {
		return err
	}
	var ticket domain.AdmissionTicket
	if err := api.Delete(ctx, "/v1/queue/"+args[0]+"?cancel=true", &ticket); err != nil {
		return err
	}
	fmt.Printf("Cancelled ticket %s.\n", args[0])
	return nil
}
