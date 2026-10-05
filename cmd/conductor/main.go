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
	"time"

	"github.com/adamburan/conductor/internal/client"
)

const usageText = `conductor — coordinate humans and coding agents on one repository

Setup
  conductor up                       one command: Postgres, control plane, and login
  conductor down [--db]              stop the control plane (--db also stops Postgres)
  conductor init                     scaffold .conductor/ into this repository
  conductor login                    save endpoint, token, and project
  conductor doctor                   report which harnesses are installed
  conductor member add <handle>      give a coworker access (prints a token once)
  conductor invite <handle>          mint a teammate a token and print one join link
  conductor join <link>              accept an invite link and log in
  conductor member list|remove       see or revoke who has access
  conductor token create|list|reset|revoke manage your own credentials
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
  conductor task done <ref>          the work merged: finish it and free its territory
  conductor task reopen <ref>        send finished-but-unmerged work back to the queue
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
  conductor peers                    daemon-to-daemon mesh: link state per peer

Execution
  conductor worker                   run a runner: claim, execute, verify, report
  conductor wrap <harness> [args…]   register a session, heartbeat, then launch a tool
  conductor serve <flash|glm53|qwen> start local vLLM for OpenCode (GLM-5.3 / Qwen 3.8)
  conductor pause | resume           freeze every agent terminal on this machine, and wake them
  conductor sessions save all        keep every agent session resumable past a closed terminal or reboot
  conductor sessions list            saved, paused, and running sessions on this machine
  conductor sessions export          the project's whole session history, as JSON
  conductor backup push|pull|status  copy this machine's resume records to/from S3
  conductor security [local|enhanced] sign in without a token on this machine, or require tokens everywhere
  conductor github setup|link|status create the GitHub App, link a repo, see what it checks
  conductor checkpoint capture       snapshot a session: transcript + working tree, portable
  conductor checkpoint resume <id>   continue it here, under another login (--account), or in another harness
  conductor checkpoint list|export|push|pull  move checkpoints between machines, as a file or sealed via S3
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
	case "up":
		err = cmdUp(ctx, args)
	case "down":
		err = cmdDown(ctx, args)
	case "login":
		err = cmdLogin(ctx, args)
	case "member":
		err = cmdMember(ctx, args)
	case "invite":
		err = cmdInvite(ctx, args)
	case "join":
		err = cmdJoin(ctx, args)
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
	case "backup":
		err = cmdBackup(ctx, args)
	case "checkpoint":
		err = cmdCheckpoint(ctx, args)
	case "security":
		err = cmdSecurity(ctx, args)
	case "github":
		err = cmdGitHub(ctx, args)
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
	case "serve":
		err = cmdServe(args)
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
	case "peers":
		err = cmdPeers(ctx, args)
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
		// On the machine running the control plane there is nothing to log in with: ask it
		// for a token for its owner. Works only from loopback against a daemon in local
		// security mode; otherwise fall through to the usual instruction.
		if signed, err := localSignInAndSave(creds); err == nil {
			return client.New(signed.Endpoint, signed.Token), signed, nil
		} else if !errors.Is(err, errNotLocal) {
			return nil, creds, fmt.Errorf("not logged in, and local sign-in failed: %w\n(run `conductor login --token …`)", err)
		}
		return nil, creds, errors.New("not logged in: run `conductor login --token …`")
	}
	return api, creds, nil
}

// errNotLocal means local sign-in was not attempted because the endpoint is not this machine.
var errNotLocal = errors.New("endpoint is not on this machine")

// localSignInAndSave obtains a token for the machine's owner from a local control plane and
// saves it as this machine's login, exactly as `conductor login` would.
func localSignInAndSave(creds client.Credentials) (client.Credentials, error) {
	if !isLoopbackEndpoint(creds.Endpoint) || os.Getenv("CONDUCTOR_NO_LOCAL_LOGIN") != "" {
		return creds, errNotLocal
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.LocalSignIn(ctx, creds.Endpoint, "cli")
	if err != nil {
		return creds, err
	}
	creds.Token = session.Token
	saved, _, err := connectAndSave(ctx, creds)
	if err != nil {
		return creds, err
	}
	fmt.Fprintf(os.Stderr, "Signed in on this machine as %s (no token needed locally; `conductor security enhanced` turns this off).\n", saved.Handle)
	return saved, nil
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
