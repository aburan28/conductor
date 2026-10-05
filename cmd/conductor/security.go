package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
)

// `conductor security` shows and changes whether this machine's owner can sign in without a
// token. Local mode is the convenient default for a laptop; enhanced mode is for a machine
// other people can log in to, or a control plane reachable from a network: it turns local
// sign-in off and revokes every token local sign-in handed out.
func cmdSecurity(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("security", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor security [status|local|enhanced] — how people sign in to this control plane

  local      The machine's owner (whoever first ran `+"`conductord bootstrap`"+` here) is signed
             in automatically from this machine: the dashboard at localhost, the CLI, and
             the macOS app need no token. Everyone else, and every other machine, still
             needs one. The default when conductord listens on 127.0.0.1 only.

  enhanced   Tokens only, everywhere. Turning it on revokes every token local sign-in has
             issued. Use it on a machine other people can log in to: loopback is shared by
             every account on the machine. The default for a network-reachable server.

Anyone who administers a project can turn enhanced mode on; only the machine's owner can
turn it back off. `+"`conductord --security-mode`"+` pins the mode, and then neither applies.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	api, _, err := mustClient()
	if err != nil {
		return err
	}
	action := "status"
	if len(positional) > 0 {
		action = positional[0]
	}
	switch action {
	case "status", "show":
		var st struct {
			Mode        string `json:"security_mode"`
			Source      string `json:"mode_source"`
			Owner       string `json:"owner"`
			YouAreOwner bool   `json:"you_are_owner"`
			BehindProxy bool   `json:"behind_proxy"`
		}
		if err := api.Get(ctx, "/v1/security", &st); err != nil {
			return err
		}
		if *asJSON {
			return emit(st)
		}
		fmt.Printf("Security mode: %s", st.Mode)
		switch st.Source {
		case "flag":
			fmt.Print(" (pinned by conductord --security-mode)")
		case "default":
			fmt.Print(" (default for how conductord is listening)")
		}
		fmt.Println()
		if st.Owner != "" {
			who := st.Owner
			if st.YouAreOwner {
				who += " (you)"
			}
			fmt.Printf("Machine owner: %s\n", who)
		} else {
			fmt.Println("Machine owner: none set (`conductord bootstrap` sets it)")
		}
		switch {
		case st.BehindProxy:
			fmt.Println("\nLocal sign-in is unavailable: conductord runs behind a proxy, where every connection looks local.")
		case st.Mode == "local":
			fmt.Println("\nThe owner signs in on this machine without a token. `conductor security enhanced` requires tokens everywhere.")
		default:
			fmt.Println("\nEvery client needs a token. `conductor security local` lets the owner sign in on this machine without one.")
		}
		return nil
	case "local", "enhanced":
		var out struct {
			Mode    string `json:"security_mode"`
			Revoked int64  `json:"revoked_local_tokens"`
		}
		if err := api.Post(ctx, "/v1/security", map[string]string{"security_mode": action}, &out); err != nil {
			return err
		}
		if *asJSON {
			return emit(out)
		}
		if action == "enhanced" {
			fmt.Printf("Enhanced security on: every client needs a token. Revoked %d token(s) issued by local sign-in.\n", out.Revoked)
			fmt.Println("Your own CLI login may have been one of them; `conductor login --token …` with a token from `conductor token create`.")
		} else {
			fmt.Println("Local sign-in on: the machine's owner is signed in automatically on this machine.")
		}
		return nil
	default:
		return errors.New("usage: conductor security [status|local|enhanced]")
	}
}
