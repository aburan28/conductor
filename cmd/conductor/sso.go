package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/sso"
)

// Single sign-on from the command line: `conductor login --sso`, and `conductor sso` to see
// providers and linked identities, link another provider, and (for administrators) register
// the address a member's first sign-in links by or unlink an identity.

// ssoFlag is --sso, which takes an optional provider name: --sso, or --sso=google.
type ssoFlag struct {
	set      bool
	provider string
}

func (f *ssoFlag) String() string   { return f.provider }
func (f *ssoFlag) IsBoolFlag() bool { return true }

func (f *ssoFlag) Set(v string) error {
	f.set = true
	switch v {
	case "true":
	case "false":
		f.set = false
	default:
		f.provider = v
	}
	return nil
}

// ssoSignIn runs a browser sign-in against endpoint. With a token and link, it adds the
// provider's account to that login instead.
func ssoSignIn(ctx context.Context, endpoint, token, provider string, link bool) (sso.LoginResult, error) {
	return sso.Login(ctx, client.New(endpoint, token), sso.LoginOptions{
		Provider: provider, Link: link, Open: openBrowser, Out: os.Stderr,
	})
}

func cmdSSO(ctx context.Context, args []string) error {
	sub, rest := "status", args
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "status":
		return ssoStatus(ctx, rest)
	case "link":
		return ssoLink(ctx, rest)
	case "unlink":
		return ssoUnlink(ctx, rest)
	case "email":
		return ssoEmail(ctx, rest)
	default:
		return fmt.Errorf("unknown sso subcommand %q (status, link, unlink, email)", sub)
	}
}

func ssoStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sso status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, _, err := mustClient()
	if err != nil {
		return err
	}
	var out struct {
		Providers     []sso.ProviderInfo    `json:"providers"`
		Identities    []db.ExternalIdentity `json:"identities"`
		TokenTTL      string                `json:"token_ttl"`
		AutoProvision *struct {
			Role    string `json:"role"`
			Project string `json:"project"`
		} `json:"auto_provision,omitempty"`
	}
	if err := api.Get(ctx, "/v1/sso/status", &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	if len(out.Providers) == 0 {
		fmt.Println("Single sign-on is not configured on this control plane (conductord --sso-provider).")
	} else {
		fmt.Println("Providers:")
		for _, p := range out.Providers {
			fmt.Printf("  %-16s %-8s %s\n", p.Name, p.Type, p.Label)
		}
		fmt.Printf("A sign-in issues a token that lasts %s.\n", out.TokenTTL)
		if out.AutoProvision != nil {
			fmt.Printf("A first sign-in with no registered account joins %s as %s.\n",
				out.AutoProvision.Project, out.AutoProvision.Role)
		} else {
			fmt.Println("A first sign-in needs an account registered with that address (conductor sso email).")
		}
	}
	if len(out.Identities) == 0 {
		fmt.Println("\nNo identities are linked to your account.")
		return nil
	}
	fmt.Println("\nLinked to your account:")
	for _, id := range out.Identities {
		last := "never"
		if id.LastLoginAt != nil {
			last = id.LastLoginAt.Local().Format(time.DateTime)
		}
		fmt.Printf("  %-16s %-32s last sign-in %s\n", id.Provider, id.Email, last)
	}
	return nil
}

func ssoLink(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sso link", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor sso link — add a provider's account to your login

  conductor sso link github

Signs you in at the provider in your browser and links that account to the account you are
logged in as now, so either can sign you in. Each account can have one identity per provider.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	_, creds, err := mustClient()
	if err != nil {
		return err
	}
	provider := ""
	if len(positional) > 0 {
		provider = positional[0]
	}
	res, err := ssoSignIn(ctx, creds.Endpoint, creds.Token, provider, true)
	if err != nil {
		return err
	}
	fmt.Printf("Linked your %s account to %s. You can now sign in with `conductor login --sso %s`.\n",
		res.Provider, res.Handle, res.Provider)
	return nil
}

func ssoUnlink(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sso unlink", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: conductor sso unlink <handle> <provider>")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	var out struct {
		Revoked int `json:"revoked_tokens"`
	}
	if err := api.Do(ctx, http.MethodDelete, "/v1/projects/"+url.PathEscape(ref)+"/members/"+
		url.PathEscape(positional[0])+"/identities/"+url.PathEscape(positional[1]), nil, &out); err != nil {
		return err
	}
	fmt.Printf("Unlinked %s's %s identity and revoked %d token(s) it issued.\n", positional[0], positional[1], out.Revoked)
	return nil
}

func ssoEmail(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sso email", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor sso email — register the address a member signs in with

  conductor sso email rachel rachel@example.com
  conductor sso email rachel ""                   clear it

A member's first single sign-on links to their account only when the provider verifies this
address. A new account gets one with `+"`conductor member add rachel --email …`"+`. Only someone who
administers every project the member belongs to can set it.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: conductor sso email <handle> <address>")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	if err := api.Do(ctx, http.MethodPut, "/v1/projects/"+url.PathEscape(ref)+"/members/"+
		url.PathEscape(positional[0])+"/email", map[string]string{"email": positional[1]}, nil); err != nil {
		return err
	}
	if positional[1] == "" {
		fmt.Printf("Cleared %s's sign-in address.\n", positional[0])
		return nil
	}
	fmt.Printf("%s can now sign in with single sign-on as %s.\n", positional[0], positional[1])
	return nil
}
