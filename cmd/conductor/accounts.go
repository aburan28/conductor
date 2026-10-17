package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/accounts"
)

// `conductor accounts` — named harness login profiles, so resume-all can move a
// machine off an account whose quota window ran out. A profile is a snapshot of
// the harness's own credential file; `add` takes it from a live login, `switch`
// copies it back, and `exhaust`/`ok` maintain the quota marker that decides
// which account `conductor resume` (and the dashboard's resume-all button)
// brings sessions back under.

func cmdAccounts(args []string) error {
	if len(args) == 0 {
		fmt.Print(accountsUsage)
		return fmt.Errorf("pick a subcommand: add, list, switch, exhaust, ok, login, use, unbind, alias, unalias")
	}
	rest := args[1:]
	var err error
	switch args[0] {
	case "add":
		err = accountsAdd(rest)
	case "list", "ls":
		err = accountsList(rest)
	case "switch":
		err = accountsSwitch(rest)
	case "exhaust", "exhausted":
		err = accountsExhaust(rest)
	case "ok", "clear":
		err = accountsClear(rest)
	case "login":
		err = accountsLogin(rest)
	case "use", "bind":
		err = accountsUse(rest)
	case "unbind":
		err = accountsUnbind(rest)
	case "alias":
		err = accountsAlias(rest)
	case "unalias":
		err = accountsUnalias(rest)
	default:
		fmt.Print(accountsUsage)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
	return err
}

const accountsUsage = `conductor accounts — manage the harness login profiles resume-all can switch between

  conductor accounts add <harness> <name>      snapshot the harness's current login as <name>
  conductor accounts list [harness]           show profiles, which is active, and quota markers
  conductor accounts switch <harness> <name> make <name> the harness's live account
  conductor accounts exhaust <harness> <name>  mark an account out of usage
  conductor accounts ok <harness> <name>         mark an account usable again (window rolled over)

Harnesses: claude, codex, opencode. An account is added by logging in with the
harness itself, then snapshotting: a quota marker makes resume-all prefer another
account when waking sessions.

Isolated accounts — a state directory per account, so projects can run under different
logins at the same time (CLAUDE_CONFIG_DIR, CODEX_HOME, XDG_DATA_HOME, Cursor's
--user-data-dir):

  conductor accounts login <harness> <name>    create the account's directory and log in there
  conductor accounts use <harness> <name>      wrap <harness> in this project under <name>
  conductor accounts unbind <harness>          back to the harness's default login here

Harnesses: claude, codex, cursor, opencode. Bindings are machine-local
(~/.conductor/accounts/bindings.json), never repository policy. CONDUCTOR_ACCOUNT=<name>
overrides the binding for one launch.

Aliases — another name for an account, or for an existing state directory:

  conductor accounts alias <harness> <alias> <account|dir>   e.g. alias claude acme work
  conductor accounts unalias <harness> <alias>

Any command above takes an alias where it takes a name. A project bound to an alias
follows it, so re-pointing the alias moves every project that uses it.
`

func accountsAdd(args []string) error {
	fs := flag.NewFlagSet("accounts add", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: conductor accounts add <harness> <name>")
	}
	if err := accounts.Add(fs.Arg(0), fs.Arg(1)); err != nil {
		return err
	}
	fmt.Printf("Saved %s account %q.\n", fs.Arg(0), fs.Arg(1))
	return nil
}

func accountsList(args []string) error {
	fs := flag.NewFlagSet("accounts list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	harnesses := accounts.Harnesses()
	if fs.NArg() == 1 {
		harnesses = []string{fs.Arg(0)}
	}

	type row struct {
		Harness     string     `json:"harness"`
		Name        string     `json:"name"`
		Kind        string     `json:"kind"`               // "snapshot" (switched into the live file), "isolated" (its own state directory) or "alias"
		AliasOf     string     `json:"alias_of,omitempty"` // an alias's account name or directory
		Active      bool       `json:"active"`
		Bound       bool       `json:"bound,omitempty"` // an isolated account this project is bound to
		Dir         string     `json:"dir,omitempty"`
		ExhaustedAt *time.Time `json:"exhausted_at,omitempty"`
	}
	project := worktreeRoot(context.Background())
	var rows []row
	for _, harness := range harnesses {
		profiles, err := accounts.List(harness)
		if err != nil {
			return err
		}
		active := accounts.Active(harness)
		for _, p := range profiles {
			rows = append(rows, row{Harness: harness, Name: p.Name, Kind: "snapshot", Active: p.Name == active, ExhaustedAt: p.ExhaustedAt})
		}
		names, err := accounts.Isolated(harness)
		if err != nil {
			return err
		}
		bound, _ := accounts.Bound(project, harness)
		// A bound account kept in ~/.<harness>-<name> is not under ~/.conductor; list it too.
		if bound != "" && !slices.Contains(names, bound) {
			names = append(names, bound)
		}
		aliases, err := accounts.Aliases(harness)
		if err != nil {
			return err
		}
		for _, name := range names {
			if slices.ContainsFunc(aliases, func(a accounts.Alias) bool { return a.Alias == name }) {
				continue // listed as the alias below
			}
			dir, _ := accounts.StateDir(harness, name)
			rows = append(rows, row{Harness: harness, Name: name, Kind: "isolated", Bound: name == bound, Dir: dir})
		}
		for _, a := range aliases {
			dir, _ := accounts.StateDir(harness, a.Alias)
			rows = append(rows, row{Harness: harness, Name: a.Alias, Kind: "alias", AliasOf: a.Target, Bound: a.Alias == bound, Dir: dir})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Harness != rows[j].Harness {
			return rows[i].Harness < rows[j].Harness
		}
		if rows[i].Kind != rows[j].Kind {
			return kindOrder[rows[i].Kind] < kindOrder[rows[j].Kind]
		}
		return rows[i].Name < rows[j].Name
	})
	if *asJSON {
		if rows == nil {
			rows = []row{}
		}
		return emit(rows)
	}
	if len(rows) == 0 {
		fmt.Println("No accounts saved. Log in with a harness, then `conductor accounts add <harness> <name>`,")
		fmt.Println("or give a project its own login with `conductor accounts login <harness> <name>`.")
		return nil
	}
	for _, r := range rows {
		mark := "  "
		if r.Active || r.Bound {
			mark = "* "
		}
		if r.Kind == "alias" {
			target := r.AliasOf
			if filepath.IsAbs(target) {
				target = shortPath(target)
			}
			state := "alias for " + target
			if r.Bound {
				state += " (this project)"
			}
			fmt.Printf("  %s%-9s %-20s %s\n", mark, r.Harness, r.Name, state)
			continue
		}
		if r.Kind == "isolated" {
			state := "isolated, " + shortPath(r.Dir)
			if r.Bound {
				state += " (this project)"
			}
			fmt.Printf("  %s%-9s %-20s %s\n", mark, r.Harness, r.Name, state)
			continue
		}
		state := "usable"
		if r.ExhaustedAt != nil {
			state = "out of usage since " + r.ExhaustedAt.Format(time.RFC3339)
		}
		fmt.Printf("  %s%-9s %-20s %s\n", mark, r.Harness, r.Name, state)
	}
	fmt.Println("\n* = the account the harness runs as here. resume-all skips accounts out of usage.")
	return nil
}

func accountsSwitch(args []string) error {
	fs := flag.NewFlagSet("accounts switch", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: conductor accounts switch <harness> <name>")
	}
	if err := accounts.Switch(fs.Arg(0), fs.Arg(1)); err != nil {
		return err
	}
	fmt.Printf("Switched %s to account %q. New sessions start under it.\n", fs.Arg(0), fs.Arg(1))
	return nil
}

func accountsExhaust(args []string) error {
	fs := flag.NewFlagSet("accounts exhaust", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: conductor accounts exhaust <harness> <name>")
	}
	if err := accounts.MarkExhausted(fs.Arg(0), fs.Arg(1)); err != nil {
		return err
	}
	fmt.Printf("Marked %s account %q out of usage. Resume-all will prefer another account; `conductor accounts ok` reverses this.\n", fs.Arg(0), fs.Arg(1))
	return nil
}

func accountsClear(args []string) error {
	fs := flag.NewFlagSet("accounts ok", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: conductor accounts ok <harness> <name>")
	}
	if err := accounts.ClearExhausted(fs.Arg(0), fs.Arg(1)); err != nil {
		return err
	}
	fmt.Printf("Marked %s account %q usable again.\n", fs.Arg(0), fs.Arg(1))
	return nil
}

// kindOrder lists snapshots, then isolated accounts, then aliases within a harness.
var kindOrder = map[string]int{"snapshot": 0, "isolated": 1, "alias": 2}

// loginCommand is how each harness signs in, run with the isolated account's directory in
// place so the credentials land there and nowhere else.
func loginCommand(iso accounts.Isolation) []string {
	switch iso.Harness {
	case "codex":
		return []string{"codex", "login"}
	case "opencode":
		return []string{"opencode", "auth", "login"}
	case "cursor":
		return append([]string{"cursor"}, iso.Args...)
	default: // claude: a first launch from an empty config directory walks through login
		return []string{"claude"}
	}
}

func accountsLogin(args []string) error {
	fs := flag.NewFlagSet("accounts login", flag.ExitOnError)
	use := fs.Bool("use", false, "also bind this project to the account")
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountsUsage) }
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return fmt.Errorf("usage: conductor accounts login <harness> <name> [--use]")
	}
	iso, err := accounts.Prepare(positional[0], positional[1])
	if err != nil {
		return err
	}
	if *use {
		if err := accounts.Bind(worktreeRoot(context.Background()), iso.Harness, iso.Account); err != nil {
			return err
		}
	}
	argv := loginCommand(iso)
	fmt.Printf("Account %q for %s lives in %s.\n", iso.Account, iso.Harness, shortPath(iso.Dir))
	if iso.Harness == "claude" {
		fmt.Println("Claude Code opens there; sign in (or type /login), then quit.")
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		fmt.Printf("%s is not installed here; sign in later with:\n  %s\n", argv[0], isolationCommand(iso, argv))
		return nil
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if iso.Env != "" {
		cmd.Env = append(cmd.Env, iso.Env+"="+iso.Dir)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s login: %w", iso.Harness, err)
	}
	if !*use {
		fmt.Printf("Bind a project to it with `conductor accounts use %s %s`.\n", iso.Harness, iso.Account)
	}
	return nil
}

func isolationCommand(iso accounts.Isolation, argv []string) string {
	prefix := ""
	if iso.Env != "" {
		prefix = iso.Env + "=" + shellQuoteArg(iso.Dir) + " "
	}
	return prefix + shellJoin(argv)
}

func accountsUse(args []string) error {
	fs := flag.NewFlagSet("accounts use", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: conductor accounts use <harness> <name>")
	}
	harness, name := fs.Arg(0), fs.Arg(1)
	dir, err := accounts.StateDir(harness, name)
	if err != nil {
		return err
	}
	project := worktreeRoot(context.Background())
	if err := accounts.Bind(project, harness, name); err != nil {
		return err
	}
	fmt.Printf("`conductor wrap %s` in %s now runs as account %q (%s).\n", harness, shortPath(project), name, shortPath(dir))
	if !dirExists(dir) {
		fmt.Printf("It has no login yet: `conductor accounts login %s %s`.\n", harness, name)
	}
	return nil
}

func accountsUnbind(args []string) error {
	fs := flag.NewFlagSet("accounts unbind", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: conductor accounts unbind <harness>")
	}
	project := worktreeRoot(context.Background())
	if err := accounts.Bind(project, fs.Arg(0), ""); err != nil {
		return err
	}
	fmt.Printf("%s in %s is back on its default login.\n", fs.Arg(0), shortPath(project))
	return nil
}

// applyAccount points the harness at the isolated account this launch resolves to (the
// CONDUCTOR_ACCOUNT override, else the project's binding). It sets the variable in this
// process, not only the child's, because the usage, checkpoint and quota sidecars read the
// same variable to find the harness's logs. A variable the user already exported wins:
// they asked for that directory explicitly, and `checkpoint resume --account` arrives
// that way too. Returns extra harness arguments (Cursor's --user-data-dir).
func applyAccount(harness, project string, toolArgs []string) []string {
	iso, ok, err := accounts.Resolve(harness, project, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "conductor: account binding not applied: %v\n", err)
		return nil
	}
	if !ok {
		return nil
	}
	if iso.Env != "" {
		if cur := os.Getenv(iso.Env); cur != "" && iso.Source == "binding" {
			if cur != iso.Dir {
				fmt.Fprintf(os.Stderr, "conductor: %s is set, so this project's account %q is not applied\n", iso.Env, iso.Account)
			}
			return nil
		}
		if err := os.Setenv(iso.Env, iso.Dir); err != nil {
			fmt.Fprintf(os.Stderr, "conductor: account %q not applied: %v\n", iso.Account, err)
			return nil
		}
	}
	for _, a := range toolArgs {
		if len(iso.Args) > 0 && (a == iso.Args[0] || strings.HasPrefix(a, iso.Args[0]+"=")) {
			return nil // the user chose a profile directory on the command line
		}
	}
	name := fmt.Sprintf("%q", iso.Account)
	if iso.AliasOf != "" && !filepath.IsAbs(iso.AliasOf) {
		name += fmt.Sprintf(", alias for %q", iso.AliasOf)
	}
	fmt.Fprintf(os.Stderr, "Running %s as account %s (%s).\n", harness, name, shortPath(iso.Dir))
	return iso.Args
}

func accountsAlias(args []string) error {
	fs := flag.NewFlagSet("accounts alias", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 3 {
		return fmt.Errorf("usage: conductor accounts alias <harness> <alias> <account|dir>")
	}
	harness, alias := fs.Arg(0), fs.Arg(1)
	if err := accounts.SetAlias(harness, alias, fs.Arg(2)); err != nil {
		return err
	}
	dir, _ := accounts.StateDir(harness, alias)
	fmt.Printf("%s account %q is now an alias (%s).\n", harness, alias, shortPath(dir))
	return nil
}

func accountsUnalias(args []string) error {
	fs := flag.NewFlagSet("accounts unalias", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, accountsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: conductor accounts unalias <harness> <alias>")
	}
	if err := accounts.RemoveAlias(fs.Arg(0), fs.Arg(1)); err != nil {
		return err
	}
	fmt.Printf("Removed %s alias %q.\n", fs.Arg(0), fs.Arg(1))
	return nil
}
