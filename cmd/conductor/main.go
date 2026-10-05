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
	"strings"
	"syscall"
	"time"

	"github.com/aburan28/conductor/internal/client"
)

func main() {
	if len(os.Args) < 2 {
		printShortHelp(os.Stderr)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	name, args := os.Args[1], os.Args[2:]
	switch name {
	case "help", "-h", "-help", "--help":
		os.Exit(cmdHelp(ctx, args))
	case "--version", "-version":
		name = "version"
	}

	// `<command> -h` gets the same page as `conductor help <command>`, for every command. The
	// bare word "help" is only taken as a request for help by commands with subcommands,
	// where it cannot be anything else; `conductor invite help` invites someone called help.
	if c, ok := lookupCommand(name); ok && len(args) > 0 && isHelpArg(args[0]) &&
		(args[0] != "help" || len(c.subs) > 0) {
		helpFor(name, commandRunner(ctx, name))
		return
	}

	err := runCommand(ctx, name, args)
	if errors.Is(err, errUnknownCommand) {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", name)
		printShortHelp(os.Stderr)
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

// cmdHelp answers `conductor help [topic]` and returns the exit status.
func cmdHelp(ctx context.Context, args []string) int {
	if len(args) == 0 {
		printShortHelp(os.Stdout)
		return 0
	}
	if args[0] == "all" {
		printAllHelp(os.Stdout)
		return 0
	}
	if args[0] == "help" {
		printShortHelp(os.Stdout)
		return 0
	}
	if !helpFor(args[0], commandRunner(ctx, args[0])) {
		fmt.Fprintf(os.Stderr, "no help topic %q; topics are:\n  %s\n", args[0], strings.Join(topicNames(), " "))
		return 2
	}
	return 0
}

func commandRunner(ctx context.Context, name string) func([]string) error {
	return func(args []string) error {
		return runCommand(ctx, name, args)
	}
}

// errUnknownCommand is runCommand's answer for a name that is not a command.
var errUnknownCommand = errors.New("unknown command")

// runCommand runs one top-level command.
func runCommand(ctx context.Context, name string, args []string) error {
	switch name {
	case "init":
		return cmdInit(args)
	case "up":
		return cmdUp(ctx, args)
	case "down":
		return cmdDown(ctx, args)
	case "login":
		return cmdLogin(ctx, args)
	case "version":
		return cmdVersion(args)
	case "completion":
		return cmdCompletion(args)
	case "member":
		return cmdMember(ctx, args)
	case "invite":
		return cmdInvite(ctx, args)
	case "join":
		return cmdJoin(ctx, args)
	case "token":
		return cmdToken(ctx, args)
	case "doctor":
		return cmdDoctor(ctx, args)
	case "quota":
		return cmdQuota(ctx, args)
	case "dashboard":
		return cmdDashboard(args)
	case "status":
		return cmdStatus(ctx, args)
	case "presence":
		return cmdPresence(ctx, args)
	case "capabilities":
		return cmdCapabilities(ctx, args)
	case "sessions":
		return cmdSessions(ctx, args)
	case "backup":
		return cmdBackup(ctx, args)
	case "checkpoint":
		return cmdCheckpoint(ctx, args)
	case "security":
		return cmdSecurity(ctx, args)
	case "sso":
		return cmdSSO(ctx, args)
	case "github":
		return cmdGitHub(ctx, args)
	case "inbox":
		return cmdInbox(ctx, args)
	case "check":
		return cmdCheck(ctx, args)
	case "conflicts":
		return cmdConflicts(ctx, args)
	case "budget":
		return cmdBudget(ctx, args)
	case "notify":
		return cmdNotify(ctx, args)
	case "usage":
		return cmdUsage(ctx, args)
	case "task":
		return cmdTask(ctx, args)
	case "scope":
		return cmdScope(ctx, args)
	case "worker":
		return cmdWorker(ctx, args)
	case "wrap":
		return cmdWrap(ctx, args)
	case "serve":
		return cmdServe(args)
	case "pause":
		return cmdPause(ctx, args)
	case "resume":
		return cmdResume(ctx, args)
	case "integrate":
		return cmdIntegrate(ctx, args)
	case "hook":
		return cmdHook(ctx, args)
	case "dispatch":
		return cmdDispatch(ctx, args)
	case "route":
		return cmdRoute(ctx, args)
	case "policy":
		return cmdPolicy(ctx, args)
	case "models":
		return cmdModels(ctx, args)
	case "swarm":
		return cmdSwarm(ctx, args)
	case "queue":
		return cmdQueue(ctx, args)
	case "peers":
		return cmdPeers(ctx, args)
	}
	return errUnknownCommand
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
			return nil, creds, fmt.Errorf("not logged in, and local sign-in failed: %w\n(%s)", err, notLoggedInHint(creds.Endpoint))
		}
		return nil, creds, fmt.Errorf("not logged in: %s", notLoggedInHint(creds.Endpoint))
	}
	return api, creds, nil
}

// notLoggedInHint says what to do without a login. On a loopback endpoint the usual cause is
// that nothing is running yet, and a new user has no token to log in with: `conductor up`
// starts the control plane and signs in. A remote endpoint needs a token from a teammate.
func notLoggedInHint(endpoint string) string {
	if isLoopbackEndpoint(endpoint) {
		return "start the local control plane and sign in with `conductor up`, " +
			"or log in elsewhere with `conductor login --endpoint URL --token …`"
	}
	return "run `conductor login --token …` with a token from `conductor invite`, or accept an invite with `conductor join <link>`"
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
