package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/notify"
)

// Notifications: send the project's events to Slack, Discord, or any webhook (DESIGN.md
// §23.4). Managing channels needs maintainer or above.

func cmdNotify(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return notifyList(ctx, args)
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "list", "ls":
		return notifyList(ctx, rest)
	case "add":
		return notifyAdd(ctx, rest)
	case "remove", "rm":
		return notifyRemove(ctx, rest)
	case "test":
		return notifyTest(ctx, rest)
	case "events":
		return notifyEvents(ctx, rest)
	default:
		return fmt.Errorf("unknown notify subcommand %q (want add, list, remove, test, or events)", sub)
	}
}

type notifyListing struct {
	Channels []notify.ChannelView `json:"channels"`
	Events   []notify.EventType   `json:"events"`
	Defaults []string             `json:"defaults"`
}

func notifyClient(project string) (*client.Client, string, error) {
	api, creds, err := mustClient()
	if err != nil {
		return nil, "", err
	}
	ref, err := projectRef(project, creds)
	if err != nil {
		return nil, "", err
	}
	return api, "/v1/projects/" + ref + "/notifications", nil
}

func notifyList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notify list", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, path, err := notifyClient(*project)
	if err != nil {
		return err
	}
	var out notifyListing
	if err := api.Get(ctx, path, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Channels)
	}
	if len(out.Channels) == 0 {
		fmt.Println("No notification channels. Add one:")
		fmt.Println("  conductor notify add slack <incoming-webhook-url>")
		fmt.Println("  conductor notify add webhook <https-url>")
		return nil
	}
	fmt.Printf("%-10s %-8s %-36s %-22s %s\n", "ID", "KIND", "DESTINATION", "STATUS", "EVENTS")
	for _, c := range out.Channels {
		dest := c.URLHint
		if c.Name != "" {
			dest = c.Name + " (" + c.URLHint + ")"
		}
		fmt.Printf("%-10s %-8s %-36s %-22s %s\n", shortID(c.ID), c.Kind, dest, channelStatus(c),
			strings.Join(c.Events, ","))
		if c.LastError != "" && c.Failures > 0 {
			fmt.Printf("%10s last error: %s\n", "", c.LastError)
		}
	}
	return nil
}

func channelStatus(c notify.ChannelView) string {
	switch {
	case c.Failures > 0 && c.RetryAfter != nil && time.Until(*c.RetryAfter) > 0:
		return fmt.Sprintf("failing, retry in %s", time.Until(*c.RetryAfter).Round(time.Second))
	case c.Failures > 0:
		return fmt.Sprintf("failing (%d)", c.Failures)
	case c.LastSuccessAt != nil:
		return "ok, " + time.Since(*c.LastSuccessAt).Round(time.Second).String() + " ago"
	default:
		return "nothing sent yet"
	}
}

func notifyAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notify add", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	events := fs.String("events", "", "comma-separated event types (default: the defaults; see `conductor notify events`)")
	name := fs.String("name", "", "a label for the channel, e.g. #eng-agents")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor notify add — send this project's events to Slack, Discord, or a webhook

  conductor notify add slack https://hooks.slack.com/services/T…/B…/…
  conductor notify add discord https://discord.com/api/webhooks/…
  conductor notify add webhook https://ci.example.com/conductor --events scope.released,github.pr_merged
  conductor notify add slack -        # read the URL from stdin, keeping it out of shell history

The URL is a credential. It is stored sealed and never shown again; a webhook channel's
signing secret is printed once, now.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		fs.Usage()
		return errors.New("usage: conductor notify add slack|discord|webhook <url>")
	}
	kind, target := positional[0], positional[1]
	if target == "-" {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("reading the URL from stdin: %w", err)
		}
		target = strings.TrimSpace(line)
	}
	api, path, err := notifyClient(*project)
	if err != nil {
		return err
	}
	req := notify.CreateRequest{Kind: kind, URL: target, Name: *name}
	if *events != "" {
		req.Events = []string{*events}
	}
	var created notify.Created
	if err := api.Post(ctx, path, req, &created); err != nil {
		return err
	}
	if *asJSON {
		return emit(created)
	}
	fmt.Printf("Added %s channel %s → %s\n", created.Kind, shortID(created.ID), created.URLHint)
	fmt.Printf("Events: %s\n", strings.Join(created.Events, ", "))
	if created.Secret != "" {
		fmt.Printf("\nSigning secret (shown once; verify X-Conductor-Signature with it):\n  %s\n", created.Secret)
		fmt.Println("\nHow to verify a request: README.md, \"Notifications\".")
	}
	fmt.Printf("\nSend a test message: conductor notify test %s\n", shortID(created.ID))
	return nil
}

// resolveChannel accepts a channel id or a unique prefix of one, as `notify list` prints.
func resolveChannel(ctx context.Context, api *client.Client, path, ref string) (string, error) {
	var out notifyListing
	if err := api.Get(ctx, path, &out); err != nil {
		return "", err
	}
	var match []string
	for _, c := range out.Channels {
		if c.ID == ref {
			return c.ID, nil
		}
		if strings.HasPrefix(c.ID, ref) {
			match = append(match, c.ID)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return "", fmt.Errorf("no notification channel %q in this project (see `conductor notify list`)", ref)
	default:
		return "", fmt.Errorf("%q matches %d channels; give more of the id", ref, len(match))
	}
}

func notifyRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notify remove", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: conductor notify remove <id>")
	}
	api, path, err := notifyClient(*project)
	if err != nil {
		return err
	}
	id, err := resolveChannel(ctx, api, path, positional[0])
	if err != nil {
		return err
	}
	if err := api.Delete(ctx, path+"/"+id, nil); err != nil {
		return err
	}
	fmt.Printf("Removed notification channel %s.\n", shortID(id))
	return nil
}

func notifyTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notify test", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: conductor notify test <id>")
	}
	api, path, err := notifyClient(*project)
	if err != nil {
		return err
	}
	id, err := resolveChannel(ctx, api, path, positional[0])
	if err != nil {
		return err
	}
	var res notify.TestResult
	if err := api.Post(ctx, path+"/"+id+"/test", struct{}{}, &res); err != nil {
		return err
	}
	if *asJSON {
		return emit(res)
	}
	if !res.OK {
		return fmt.Errorf("test message not delivered: %s", res.Error)
	}
	fmt.Printf("Test message delivered (HTTP %d).\n", res.Status)
	return nil
}

func notifyEvents(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notify events", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The server's catalog, so an older CLI never offers what a newer server no longer
	// sends; this CLI's own copy only when the server cannot be asked.
	listing := notifyListing{Events: notify.Catalog, Defaults: notify.DefaultEvents}
	if api, path, err := notifyClient(*project); err == nil {
		var out notifyListing
		if err := api.Get(ctx, path, &out); err == nil && len(out.Events) > 0 {
			listing = out
		}
	}
	if *asJSON {
		return emit(map[string]any{"events": listing.Events, "defaults": listing.Defaults})
	}
	defaults := map[string]bool{}
	for _, d := range listing.Defaults {
		defaults[strings.SplitN(d, ":", 2)[0]] = true
	}
	for _, e := range listing.Events {
		mark := " "
		if defaults[e.Type] {
			mark = "*"
		}
		fmt.Printf("%s %-26s %s\n", mark, e.Type, e.Description)
	}
	fmt.Printf("\n* in the defaults: %s\n", strings.Join(listing.Defaults, ", "))
	fmt.Println("Pass --events to `conductor notify add` to choose; \"*\" sends everything above.")
	return nil
}
