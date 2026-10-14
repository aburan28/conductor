package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
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
		return fmt.Errorf("pick a subcommand: add, list, switch, exhaust, ok")
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
		Harness string `json:"harness"`
		Name    string `json:"name"`
		Active  bool   `json:"active"`
		ExhaustedAt *time.Time `json:"exhausted_at,omitempty"`
	}
	var rows []row
	for _, harness := range harnesses {
		profiles, err := accounts.List(harness)
		if err != nil {
			return err
		}
		active := accounts.Active(harness)
		for _, p := range profiles {
			rows = append(rows, row{Harness: harness, Name: p.Name, Active: p.Name == active, ExhaustedAt: p.ExhaustedAt})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Harness != rows[j].Harness {
			return rows[i].Harness < rows[j].Harness
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
		fmt.Println("No accounts saved. Log in with a harness, then `conductor accounts add <harness> <name>`.")
		return nil
	}
	for _, r := range rows {
		state := "usable"
		if r.ExhaustedAt != nil {
			state = "out of usage since " + r.ExhaustedAt.Format(time.RFC3339)
		}
		mark := "  "
		if r.Active {
			mark = "* "
		}
		fmt.Printf("  %s%-9s %-20s %s\n", mark, r.Harness, r.Name, state)
	}
	fmt.Println("\n* = the account the harness is logged in as. resume-all skips accounts out of usage.")
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
