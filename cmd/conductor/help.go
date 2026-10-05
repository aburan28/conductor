package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// One index of every command drives the top-level help, `conductor help all`, `conductor help
// <topic>`, `<command> -h`, and shell completion. Before this, help came in three styles (a
// prose page, a bare "Usage of up:" flag dump, or an "unknown subcommand" error with exit 1),
// and the top-level page listed pause/resume twice. A command added here shows up everywhere
// at once; a command missing here is caught by TestEveryDispatchedCommandHasHelp.

// scopeFlagHelp is the --scope description every command that takes one shares, so the
// resource syntax is spelled out wherever a user meets the flag.
const scopeFlagHelp = "resource to reserve (repeatable): path:FILE, dir:DIR, or migration:NAME; " +
	"append :read for shared read access, e.g. --scope path:internal/api.go --scope dir:docs:read"

type command struct {
	name    string
	group   string
	summary string
	// subs are the subcommands, for completion and for the help listing.
	subs []string
	// short marks the commands the bare `conductor help` page shows.
	short bool
	// topic is the help page printed for `conductor help <name>` and `<name> -h`. Empty means
	// the command already prints a complete page of its own on -h, which is used instead.
	topic string
	// flags means the command parses its own flags with a stock flag set, so its -h output
	// (the flag list) follows the topic.
	flags bool
}

var commandGroups = []string{"Get started", "Coordinate", "Work", "Territory", "Dispatch", "Sessions and machines", "Team and access", "Other"}

var commands = []command{
	// Get started
	{name: "up", group: "Get started", short: true, flags: true,
		summary: "start Postgres (when none is reachable), the control plane, and log in",
		topic: `conductor up — one command from nothing to a running, logged-in control plane

Starts Postgres in Docker only when no database is reachable at the DSN, starts conductord
in the background (pidfile and log under ~/.conductor/runtime), and saves a login. Running
it again is safe: whatever is already up is reused.

Usage:
  conductor up [--endpoint URL] [--addr HOST:PORT] [--dsn URL] [--project SLUG]

Example:
  conductor up
  conductor up --dsn postgres://me@localhost:5432/conductor   # an existing Postgres
`},
	{name: "down", group: "Get started", flags: true,
		summary: "stop the control plane (--db also stops Postgres)",
		topic: `conductor down — stop the control plane that conductor up started

Postgres keeps running unless --db is given.

Usage:
  conductor down [--db]

Example:
  conductor down --db
`},
	{name: "init", group: "Get started", short: true, flags: true,
		summary: "scaffold .conductor/ policy into this repository",
		topic: `conductor init — scaffold .conductor/ policy files into a repository

Writes project.yaml, policies.yaml, models.yaml, dispatch.yaml and WORKFLOW.md under
.conductor/, and adds a managed block to CLAUDE.md and AGENTS.md. Existing files are kept
unless --force is given.

Usage:
  conductor init [--dir DIR] [--force]

Example:
  cd ~/src/myrepo && conductor init
`},
	{name: "login", group: "Get started", flags: true,
		summary: "save endpoint, token, and default project",
		topic: `conductor login — save the endpoint, token, and default project this CLI uses

On the machine running the control plane no token is needed; elsewhere use the token from
a teammate's ` + "`conductor invite`" + ` (or ` + "`conductor join <link>`" + `).

Usage:
  conductor login [--endpoint URL] [--token TOKEN] [--project SLUG]

Example:
  conductor login --endpoint https://conductor.example.com --token cdt_…
`},
	{name: "doctor", group: "Get started", short: true, flags: true,
		summary: "check this machine: database, daemon, versions, git, harnesses",
		topic: `conductor doctor — check that this machine is ready to use Conductor

Reports the control plane and its version, the database (when it is on this machine), where
conductord, git and Docker are, which coding harnesses are installed, and which tools are
connected to the project.

Usage:
  conductor doctor [--json]

Example:
  conductor doctor
`},
	{name: "version", group: "Get started", flags: true,
		summary: "print the CLI version (also: --version)",
		topic: `conductor version — print the version of this CLI

Usage:
  conductor version [--json]
  conductor --version

Example:
  conductor version
`},
	{name: "completion", group: "Get started",
		summary: "print a shell completion script (bash, zsh, fish)", subs: []string{"bash", "zsh", "fish"},
		topic: `conductor completion — print a shell completion script

Usage:
  conductor completion bash|zsh|fish

Example:
  conductor completion bash > ~/.local/share/bash-completion/completions/conductor
  conductor completion zsh  > "${fpath[1]}/_conductor"
  conductor completion fish > ~/.config/fish/completions/conductor.fish
`},
	{name: "integrate", group: "Get started",
		summary: "connect Claude Code, Cursor, Codex, OpenCode, … to this project"},
	{name: "dashboard", group: "Get started", short: true, flags: true,
		summary: "print a ready-to-open dashboard link",
		topic: `conductor dashboard — print a link that opens the dashboard signed in

Usage:
  conductor dashboard [--project SLUG]

Example:
  open "$(conductor dashboard)"
`},
	{name: "policy", group: "Get started", subs: []string{"lint"},
		summary: "validate .conductor/ policy files and their rules",
		topic: `conductor policy — validate the repository's .conductor/ policy

Usage:
  conductor policy lint [--dir DIR]

Example:
  conductor policy lint
`},

	// Coordinate
	{name: "status", group: "Coordinate", short: true, flags: true,
		summary: "what is in flight, and what is contested",
		topic: `conductor status — what is in flight in this project, and what is contested

Usage:
  conductor status [--project SLUG] [--json]

Example:
  conductor status
`},
	{name: "check", group: "Coordinate", short: true,
		summary: "can I start this work? (run before you edit)"},
	{name: "presence", group: "Coordinate", flags: true,
		summary: "who is working on what, right now",
		topic: `conductor presence — who is working on what, right now

Usage:
  conductor presence [--project SLUG] [--watch] [--json]

Example:
  conductor presence --watch
`},
	{name: "conflicts", group: "Coordinate", flags: true,
		summary: "open conflicts and what to do about them",
		topic: `conductor conflicts — open conflicts, and how to resolve them

Usage:
  conductor conflicts [--project SLUG] [--json]
  conductor conflicts --resolve ID [--state resolved|acknowledged|ignored] [--note WHY]

Example:
  conductor conflicts --resolve C-12 --state ignored --note "docs only; no overlap"
`},
	{name: "capabilities", group: "Coordinate", flags: true,
		summary: "which models and effort levels are live right now",
		topic: `conductor capabilities — which models and effort levels are live right now

Usage:
  conductor capabilities [--project SLUG] [--json]

Example:
  conductor capabilities
`},
	{name: "budget", group: "Coordinate", subs: []string{"show", "share", "grants"},
		summary: "the team's token budget; share part of yours",
		topic: `conductor budget — the team's token budget for this window

Usage:
  conductor budget [show]                   allowances, spend, and shares
  conductor budget share <handle> <tokens>  give a teammate part of your allowance
  conductor budget grants                   the history of shares

Example:
  conductor budget share rachel 200000

Run ` + "`conductor budget <subcommand> -h`" + ` for its flags.
`},
	{name: "usage", group: "Coordinate", subs: []string{"sync"},
		summary: "tokens and cost over time, by day, harness, model, or person"},

	// Work
	{name: "task", group: "Work", short: true,
		subs:    []string{"list", "show", "create", "claim", "release", "done", "reopen", "handoff", "assign", "export"},
		summary: "file, claim, release, and hand off work",
		topic: `conductor task — file work, take it, and hand it back

Usage:
  conductor task list [--all]                  open tasks
  conductor task show <ref>                    one task
  conductor task create --title T [--scope R]  file new work
  conductor task claim <ref> | --next          take a task and its territory
  conductor task release <ref>                 hand a task back
  conductor task done <ref>                    the work merged: finish it and free its territory
  conductor task reopen <ref>                  send finished-but-unmerged work back to the queue
  conductor task handoff <ref> --to <harness>  hand off to another harness
  conductor task assign <ref>                  offer work to a session that meets a floor
  conductor task export <ref>                  write the Markdown task card

Example:
  conductor task create --title "Retry-aware routing" --scope dir:internal/router
  conductor task claim --next

Run ` + "`conductor task <subcommand> -h`" + ` for its flags. --scope takes path:FILE, dir:DIR or
migration:NAME, optionally followed by :read.
`},
	{name: "inbox", group: "Work", flags: true,
		summary: "work offered to this session",
		topic: `conductor inbox — work offered to this session

Usage:
  conductor inbox [--all] [--json]
  conductor inbox --accept ID | --decline ID [--note WHY]

Example:
  conductor inbox --accept A-7
`},

	// Territory
	{name: "scope", group: "Territory", subs: []string{"add", "list"},
		summary: "reserve files and directories for a task",
		topic: `conductor scope — reserve the files and directories a task will touch

Usage:
  conductor scope add <task-ref> <resource>…   reserve resources for a task
  conductor scope list                         active reservations

A resource is path:FILE, dir:DIR, or migration:NAME. Append :read for shared read access
(the default is exclusive write).

Example:
  conductor scope add T-42 path:internal/api/handlers.go dir:docs:read
`},

	// Dispatch
	{name: "dispatch", group: "Dispatch",
		summary: "plan work, then send it to models by policy"},
	{name: "route", group: "Dispatch",
		summary: "show what the dispatch policy would decide, and why"},
	{name: "models", group: "Dispatch", subs: []string{"discover"},
		summary: "the model catalog; models discover finds local ones"},
	{name: "swarm", group: "Dispatch", subs: []string{"status", "join"},
		summary: "contribute this machine's sessions to the team's queue"},
	{name: "queue", group: "Dispatch", subs: []string{"cancel"},
		summary: "the admission queue: who is waiting for a slot"},
	{name: "peers", group: "Dispatch", flags: true,
		summary: "daemon-to-daemon mesh: link state per peer",
		topic: `conductor peers — link state for each peer daemon in the mesh

Usage:
  conductor peers [--json]

Example:
  conductor peers
`},
	{name: "worker", group: "Dispatch",
		summary: "run a runner: claim, execute, verify, report"},

	// Sessions and machines
	{name: "wrap", group: "Sessions and machines", short: true,
		subs:    []string{"claude", "codex", "opencode"},
		summary: "run Claude Code, Codex, or OpenCode as a registered session",
		topic: `conductor wrap — register a session, heartbeat, then launch a coding tool

Everything after the harness name goes to the harness unchanged. Conductor's own flags come
before it.

Usage:
  conductor wrap [--model M] [--effort E] [--max-effort E] [--alias A] [--role R] <harness> [args…]

Example:
  conductor wrap claude
  conductor wrap --effort high codex --model gpt-5
`},
	{name: "pause", group: "Sessions and machines",
		summary: "freeze every agent terminal on this machine; save how to revive them"},
	{name: "resume", group: "Sessions and machines",
		summary: "wake paused sessions, reopening any closed terminals"},
	{name: "sessions", group: "Sessions and machines", subs: []string{"save", "list", "export", "install-hook"},
		summary: "keep agent sessions resumable past a closed terminal or reboot",
		topic: `conductor sessions — keep agent sessions resumable on this machine

Usage:
  conductor sessions save <all|pid|session-id…>   record how to resume them
  conductor sessions list                         saved, paused, and running sessions
  conductor sessions export                       the project's session history, as JSON
  conductor sessions install-hook                 save sessions automatically on exit

Example:
  conductor sessions save all

Run ` + "`conductor sessions <subcommand> -h`" + ` for its flags.
`},
	{name: "checkpoint", group: "Sessions and machines",
		subs:    []string{"capture", "list", "show", "resume", "export", "import", "push", "pull", "prune"},
		summary: "portable snapshots of a session: transcript plus working tree",
		topic: `conductor checkpoint — snapshot a session and continue it elsewhere

Usage:
  conductor checkpoint capture            snapshot a session: transcript + working tree
  conductor checkpoint list | show <id>   what has been captured
  conductor checkpoint resume <id|file>   continue here, under another --account, or another --harness
  conductor checkpoint export | import    move a checkpoint as a file
  conductor checkpoint push | pull        move checkpoints sealed via S3
  conductor checkpoint prune              delete old checkpoints

Example:
  conductor checkpoint capture && conductor checkpoint list

Run ` + "`conductor checkpoint <subcommand> -h`" + ` for its flags.
`},
	{name: "backup", group: "Sessions and machines", subs: []string{"push", "pull", "status"},
		summary: "copy this machine's resume records to and from S3",
		topic: `conductor backup — copy this machine's resume records to and from S3

Usage:
  conductor backup push     upload
  conductor backup pull     download
  conductor backup status   what is where

Example:
  conductor backup push

Run ` + "`conductor backup <subcommand> -h`" + ` for its flags.
`},
	{name: "serve", group: "Sessions and machines",
		summary: "start local vLLM for OpenCode (GLM-5.3 / Qwen 3.8)"},

	// Team and access
	{name: "invite", group: "Team and access", short: true,
		summary: "give a teammate access with one link"},
	{name: "join", group: "Team and access",
		summary: "accept an invite link and log in"},
	{name: "member", group: "Team and access", subs: []string{"add", "list", "role", "remove"},
		summary: "see, add, or revoke who has access",
		topic: `conductor member — who has access to this project

Usage:
  conductor member add <handle> [--role maintainer]   add someone; a new account gets a token once
  conductor member list
  conductor member role <handle> <role>               change a member's role (never above your own)
  conductor member remove <handle>

Example:
  conductor member add rachel --role maintainer

Run ` + "`conductor member <subcommand> -h`" + ` for its flags.
`},
	{name: "token", group: "Team and access", subs: []string{"create", "list", "reset", "revoke", "revoke-all"},
		summary: "manage your own credentials",
		topic: `conductor token — manage your own credentials

Usage:
  conductor token create [--name N] [--save]   mint another token
  conductor token list                         your tokens
  conductor token reset [--save]               replace a token with a fresh one
  conductor token revoke <name>                revoke one
  conductor token revoke-all --yes             revoke every token you hold

Example:
  conductor token create --name ci

Run ` + "`conductor token <subcommand> -h`" + ` for its flags.
`},
	{name: "security", group: "Team and access", subs: []string{"status", "local", "enhanced"},
		summary: "sign in without a token on this machine, or require tokens everywhere"},
	{name: "github", group: "Team and access", subs: []string{"setup", "install", "link", "status", "check"},
		summary: "create the GitHub App, link a repo, see what it checks"},

	// Other
	{name: "hook", group: "Other", subs: []string{"pre-tool", "session-start", "session-end", "checkpoint"},
		summary: "entry points for harness hooks (installed by conductor integrate)",
		topic: `conductor hook — entry points a coding tool's hooks call

You do not run these by hand: ` + "`conductor integrate`" + ` installs them into Claude Code,
Codex, and other tools that support hooks.

Usage:
  conductor hook pre-tool | session-start | session-end | checkpoint

Example:
  conductor integrate claude    # installs the hooks
`},
}

// lookupCommand finds a command by name.
func lookupCommand(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// isHelpArg reports whether an argument asks for help.
func isHelpArg(arg string) bool {
	switch arg {
	case "-h", "-help", "--help", "help":
		return true
	}
	return false
}

// printShortHelp is the bare `conductor` / `conductor help` page: the commands a new user
// needs, grouped, with the way to everything else.
func printShortHelp(w io.Writer) {
	fmt.Fprintln(w, "conductor — coordinate humans and coding agents on one repository")
	for _, group := range commandGroups {
		var rows []command
		for _, c := range commands {
			if c.group == group && c.short {
				rows = append(rows, c)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", group)
		for _, c := range rows {
			fmt.Fprintf(w, "  conductor %-14s %s\n", c.name, c.summary)
		}
	}
	fmt.Fprint(w, `
  conductor help <command>   help for one command (also: conductor <command> -h)
  conductor help all         every command
  conductor version          print the version
`)
}

// printAllHelp lists every command, grouped.
func printAllHelp(w io.Writer) {
	fmt.Fprintln(w, "conductor — coordinate humans and coding agents on one repository")
	for _, group := range commandGroups {
		fmt.Fprintf(w, "\n%s\n", group)
		for _, c := range commands {
			if c.group != group {
				continue
			}
			name := c.name
			if len(c.subs) > 0 && c.name != "wrap" {
				name += " " + strings.Join(c.subs, "|")
			}
			if len(name) > 26 {
				fmt.Fprintf(w, "  conductor %s\n  %-36s %s\n", name, "", c.summary)
				continue
			}
			fmt.Fprintf(w, "  conductor %-26s %s\n", name, c.summary)
		}
	}
	fmt.Fprintln(w, "\nRun `conductor help <command>` or `conductor <command> -h` for its usage and an example.")
}

// helpFor answers `conductor help <topic>` and `conductor <topic> -h`. It returns false when
// the command should handle the request itself.
//
// Commands that already print a full page of their own on -h (topic == "") are given the -h
// with their output on stdout, so `conductor help check | less` works like every other topic.
// Commands with a topic print it; when they also parse their own flags, their flag list
// follows. Either way the process exits 0.
func helpFor(name string, run func(args []string) error) bool {
	c, ok := lookupCommand(name)
	if !ok {
		return false
	}
	if c.topic != "" {
		fmt.Print(c.topic)
		if !c.flags {
			return true
		}
		fmt.Println()
	}
	if run == nil {
		return true
	}
	// The flag package writes -h output to os.Stderr, read at the time it prints.
	os.Stderr = os.Stdout
	_ = run([]string{"-h"})
	return true
}

// topicNames lists every command, sorted: the topics `conductor help` accepts.
func topicNames() []string {
	var names []string
	for _, c := range commands {
		names = append(names, c.name)
	}
	sort.Strings(names)
	return names
}

// commandNames is every word valid after `conductor`, sorted, for completion.
func commandNames() []string {
	names := append(topicNames(), "help")
	sort.Strings(names)
	return names
}
