package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/db"
)

// Member and token administration from the CLI, so onboarding a coworker does not require
// database access.

func cmdMember(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: conductor member <add|list|remove>")
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "add", "invite":
		return memberAdd(ctx, rest)
	case "list":
		return memberList(ctx, rest)
	case "remove", "rm":
		return memberRemove(ctx, rest)
	default:
		return fmt.Errorf("unknown member subcommand %q", sub)
	}
}

func memberAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("member add", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	role := fs.String("role", "contributor", "org_admin | project_admin | maintainer | contributor | reviewer | observer | runner")
	kind := fs.String("kind", "human", "human | runner_service | review_bot")
	name := fs.String("name", "", "display name")
	email := fs.String("email", "", "email address")
	ttl := fs.Duration("ttl", 0, "token lifetime (default: 90d for humans, no expiry for services)")
	noToken := fs.Bool("no-token", false, "add the membership without minting a token")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor member add — give someone access to this project

  conductor member add rachel --role contributor
  conductor member add ci-runner --kind runner_service --role runner

Prints a token once. It is stored only as a hash and cannot be recovered.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: conductor member add <handle> [--role contributor]")
	}

	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	var result struct {
		Handle    string `json:"handle"`
		Role      string `json:"role"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		Created   bool   `json:"created_principal"`
	}
	if err := api.Post(ctx, "/v1/projects/"+ref+"/members", map[string]any{
		"handle": positional[0], "role": *role, "kind": *kind,
		"display_name": *name, "email": *email,
		"token_ttl": ttl.String(), "no_token": *noToken,
	}, &result); err != nil {
		return err
	}
	if *asJSON {
		return emit(result)
	}

	verb := "Added"
	if !result.Created {
		verb = "Granted"
	}
	fmt.Printf("%s %s as %s on %s.\n", verb, result.Handle, result.Role, ref)
	if result.Token == "" {
		return nil
	}

	fmt.Printf(`
Send them this, once, over a channel you trust:

  conductor login --endpoint %s --token %s --project %s
`, creds.Endpoint, result.Token, ref)
	if result.ExpiresAt != "" {
		fmt.Printf("\nThe token expires %s. Renew with `conductor token create`.\n", result.ExpiresAt)
	}
	fmt.Fprintln(os.Stderr,
		"\nThis is the only time the token is shown. It is stored as a hash and cannot be recovered.")
	return nil
}

func memberList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("member list", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
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

	var out struct {
		Members []struct {
			Handle string `json:"handle"`
			Kind   string `json:"kind"`
			Role   string `json:"role"`
		} `json:"members"`
	}
	if err := api.Get(ctx, "/v1/projects/"+ref+"/members", &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Members)
	}
	if len(out.Members) == 0 {
		fmt.Println("No members.")
		return nil
	}
	for _, m := range out.Members {
		fmt.Printf("  %-20s %-16s %s\n", m.Handle, m.Role, m.Kind)
	}
	return nil
}

func memberRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("member remove", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: conductor member remove <handle>")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	if err := api.Do(ctx, "DELETE", "/v1/projects/"+ref+"/members/"+positional[0], nil, nil); err != nil {
		return err
	}
	fmt.Printf("Removed %s from %s.\n", positional[0], ref)
	fmt.Println("If that was their only project, every token they hold is now revoked.")
	return nil
}

// ---------------------------------------------------------------------------
// token
// ---------------------------------------------------------------------------

func cmdToken(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: conductor token <create|list|revoke|revoke-all>")
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "create", "new":
		return tokenCreate(ctx, rest)
	case "list", "ls":
		return tokenList(ctx, rest)
	case "revoke":
		return tokenRevoke(ctx, rest)
	case "revoke-all":
		return tokenRevokeAll(ctx, rest)
	default:
		return fmt.Errorf("unknown token subcommand %q", sub)
	}
}

func tokenCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("token create", flag.ExitOnError)
	name := fs.String("name", "cli", "a label so you can tell your tokens apart")
	ttl := fs.Duration("ttl", 90*24*time.Hour, "lifetime (0 for no expiry)")
	save := fs.Bool("save", false, "write the new token to ~/.conductor/credentials")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}

	var result struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if err := api.Post(ctx, "/v1/tokens",
		map[string]any{"name": *name, "ttl": ttl.String()}, &result); err != nil {
		return err
	}
	if *asJSON {
		return emit(result)
	}

	if *save {
		creds.Token = result.Token
		if err := client.SaveCredentials(creds); err != nil {
			return err
		}
		fmt.Printf("Created token %q and saved it. The previous one is still valid until revoked.\n", result.Name)
		return nil
	}
	fmt.Printf("%s\n", result.Token)
	fmt.Fprintf(os.Stderr,
		"\nToken %q created. Shown once; stored only as a hash.\nUse --save to write it to your credentials file.\n",
		result.Name)
	return nil
}

func tokenList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("token list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, _, err := mustClient()
	if err != nil {
		return err
	}

	var out struct {
		Tokens []db.TokenInfo `json:"tokens"`
	}
	if err := api.Get(ctx, "/v1/tokens", &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Tokens)
	}
	if len(out.Tokens) == 0 {
		fmt.Println("No tokens.")
		return nil
	}
	fmt.Printf("  %-28s %-12s %-12s %s\n", "NAME", "CREATED", "LAST USED", "STATUS")
	for _, t := range out.Tokens {
		status := "active"
		switch {
		case t.RevokedAt != nil:
			status = "revoked"
		case t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()):
			status = "expired"
		case t.ExpiresAt != nil:
			status = "expires " + t.ExpiresAt.Format("2006-01-02")
		}
		lastUsed := "never"
		if t.LastUsedAt != nil {
			lastUsed = t.LastUsedAt.Format("2006-01-02")
		}
		fmt.Printf("  %-28s %-12s %-12s %s\n",
			t.Name, t.CreatedAt.Format("2006-01-02"), lastUsed, status)
	}
	return nil
}

func tokenRevoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("token revoke", flag.ExitOnError)
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: conductor token revoke <name>")
	}
	api, _, err := mustClient()
	if err != nil {
		return err
	}
	if err := api.Do(ctx, "DELETE", "/v1/tokens/"+positional[0], nil, nil); err != nil {
		return err
	}
	fmt.Printf("Revoked %q.\n", positional[0])
	return nil
}

func tokenRevokeAll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("token revoke-all", flag.ExitOnError)
	confirm := fs.Bool("yes", false, "confirm; this also revokes the token you are using")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*confirm {
		return errors.New(
			"this revokes every token you hold, including the one in use — re-run with --yes")
	}
	api, _, err := mustClient()
	if err != nil {
		return err
	}
	var out struct {
		Revoked int `json:"revoked"`
	}
	if err := api.Post(ctx, "/v1/tokens/revoke-all", nil, &out); err != nil {
		return err
	}
	fmt.Printf("Revoked %d token(s), including this session's.\n", out.Revoked)
	fmt.Println("Ask a project admin for a new one: `conductor member add <you>`.")
	return nil
}
