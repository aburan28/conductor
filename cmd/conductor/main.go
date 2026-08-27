// Command conductor is the operator and developer CLI (DESIGN.md §33).
//
// Every command accepts --json so hooks, wrappers, and scripts can consume it without
// parsing terminal text (§33). Human output is the default because the primary user is a
// person deciding whether to start work.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/adamburan/conductor/internal/client"
)

const usageText = `conductor — coordinate humans and coding agents on one repository

Setup
  conductor init                     scaffold .conductor/ into this repository
  conductor login                    save endpoint, token, and project
  conductor doctor                   report which harnesses are installed
  conductor member add <handle>      give a coworker access (prints a token once)
  conductor member list|remove       see or revoke who has access
  conductor token create|list|revoke manage your own credentials
  conductor dashboard                print a ready-to-open dashboard link
  conductor integrate <tool>         connect Claude Code, Cursor, Codex, OpenCode, … to this project
  conductor models                   the model catalog; models discover finds local ones
  conductor policy lint              validate .conductor/ policy files and their rules

Coordination
  conductor status                   what is in flight, and what is contested
  conductor presence                 who is working on what, right now
  conductor capabilities             which models and effort levels are live right now
  conductor check                    can I start this work? (run before you edit)
  conductor budget                   the team's token budget for this window
  conductor usage                    tokens and cost over time, by day, harness, model, or person
  conductor usage sync               report this machine's unwrapped sessions
  conductor budget share <who> <n>   give a teammate part of your allowance

Work
  conductor task list                open tasks
  conductor task show <ref>          one task
  conductor task create              file new work
  conductor task claim <ref|--next>  take a task and its territory
  conductor task release <ref>       hand a task back
  conductor task handoff <ref>       hand off to another harness
  conductor task assign <ref>        offer work to a session that meets a capability floor
  conductor inbox                    work offered to this session
  conductor task export <ref>        write the Markdown task card

Territory
  conductor scope add <ref> <resource>   reserve a resource
  conductor scope list                   active reservations
  conductor conflicts                    open conflicts and what to do about them

Dispatch
  conductor dispatch <objective|T-n> plan work, then send it to models by policy
  conductor route <ref> --explain    show what the dispatch policy would decide, and why
  conductor swarm join|status        contribute this machine's sessions to the team's queue
  conductor queue                    the admission queue: who is waiting for a slot

Execution
  conductor worker                   run a runner: claim, execute, verify, report
  conductor wrap <harness> [args…]   register a session, heartbeat, then launch a tool
  conductor pause | resume           freeze every agent terminal on this machine, and wake them
  conductor sessions save all        keep every agent session resumable past a closed terminal or reboot
  conductor sessions list            saved, paused, and running sessions on this machine
  conductor sessions export          the project's whole session history, as JSON
  conductor pause                    freeze the live agent terminals; save how to revive them
  conductor resume                   wake paused sessions, reopening any closed terminals

Run any command with -h for its flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usageText)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[2:]
	var err error

	switch os.Args[1] {
	case "init":
		err = cmdInit(args)
	case "login":
		err = cmdLogin(ctx, args)
	case "member":
		err = cmdMember(ctx, args)
	case "token":
		err = cmdToken(ctx, args)
	case "doctor":
		err = cmdDoctor(ctx, args)
	case "dashboard":
		err = cmdDashboard(args)
	case "status":
		err = cmdStatus(ctx, args)
	case "presence":
		err = cmdPresence(ctx, args)
	case "capabilities":
		err = cmdCapabilities(ctx, args)
	case "sessions":
		err = cmdSessions(ctx, args)
	case "inbox":
		err = cmdInbox(ctx, args)
	case "check":
		err = cmdCheck(ctx, args)
	case "conflicts":
		err = cmdConflicts(ctx, args)
	case "budget":
		err = cmdBudget(ctx, args)
	case "usage":
		err = cmdUsage(ctx, args)
	case "task":
		err = cmdTask(ctx, args)
	case "scope":
		err = cmdScope(ctx, args)
	case "worker":
		err = cmdWorker(ctx, args)
	case "wrap":
		err = cmdWrap(ctx, args)
	case "pause":
		err = cmdPause(ctx, args)
	case "resume":
		err = cmdResume(ctx, args)
	case "integrate":
		err = cmdIntegrate(ctx, args)
	case "hook":
		err = cmdHook(ctx, args)
	case "dispatch":
		err = cmdDispatch(ctx, args)
	case "route":
		err = cmdRoute(ctx, args)
	case "policy":
		err = cmdPolicy(ctx, args)
	case "models":
		err = cmdModels(ctx, args)
	case "swarm":
		err = cmdSwarm(ctx, args)
	case "queue":
		err = cmdQueue(ctx, args)
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}

	if err != nil {
		// A coordination refusal is not a crash. It exits non-zero so scripts stop, but it
		// prints as guidance rather than as a stack of internal detail.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Blocked() {
			fmt.Fprintf(os.Stderr, "\n%s\n", apiErr.Message)
			os.Exit(3)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// parseFlags parses flags that may appear before, after, or between positional arguments,
// returning the positionals in order.
//
// The stdlib flag package stops at the first non-flag argument, so `conductor member add
// rachel --role maintainer` would parse zero flags and silently create a contributor. A
// silently wrong result is far worse than an error, and nobody writes flags strictly first.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// emit prints a value as JSON, for --json mode.
func emit(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func mustClient() (*client.Client, client.Credentials, error) {
	api, creds := client.FromEnvironment()
	if creds.Token == "" {
		return nil, creds, errors.New("not logged in: run `conductor login --token …`")
	}
	return api, creds, nil
}

func projectRef(override string, creds client.Credentials) (string, error) {
	if override != "" {
		return override, nil
	}
	if creds.Project != "" {
		return creds.Project, nil
	}
	return "", errors.New("no project selected: pass --project or set one with `conductor login --project …`")
}
