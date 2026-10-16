package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aburan28/conductor/internal/wif"
)

// cmdWIF runs Claude Code with a renewable workload identity token.
func cmdWIF(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: conductor wif local init|jwks or conductor wif exec [OPTIONS] -- claude [ARGS...]")
	}
	switch args[0] {
	case "local":
		return cmdWIFLocal(args[1:])
	case "exec":
		return cmdWIFExec(ctx, args[1:])
	default:
		return errors.New("usage: conductor wif local init|jwks or conductor wif exec [OPTIONS] -- claude [ARGS...]")
	}
}

func cmdWIFLocal(args []string) error {
	if len(args) == 0 || (args[0] != "init" && args[0] != "jwks") {
		return errors.New("usage: conductor wif local init|jwks [--key-file PATH]")
	}
	fs := flag.NewFlagSet("wif local "+args[0], flag.ContinueOnError)
	keyFile := fs.String("key-file", "", "private issuer key path (defaults to ~/.conductor/wif/local-issuer.pem)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("wif local accepts no positional arguments")
	}
	path := *keyFile
	if path == "" {
		var err error
		path, err = wif.LocalKeyPath()
		if err != nil {
			return err
		}
	}
	var key wif.LocalJWK
	var err error
	if args[0] == "init" {
		key, err = wif.CreateLocalKey(path)
	} else {
		key, err = wif.PublicLocalJWK(path)
	}
	if err != nil {
		return err
	}
	// Only public verification material appears on stdout.
	raw, err := json.Marshal(map[string]any{"keys": []wif.LocalJWK{key}})
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

func cmdWIFExec(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("wif exec", flag.ContinueOnError)
	providerName := fs.String("provider", "", "gcp, aws, file, or local")
	tokenPath := fs.String("identity-token-file", "", "rotating OIDC token file for provider=file")
	awsRegion := fs.String("aws-region", "", "regional STS endpoint for provider=aws")
	keyFile := fs.String("key-file", "", "private issuer key path for provider=local")
	issuer := fs.String("issuer", "", "exact issuer URL registered in Anthropic for provider=local")
	subject := fs.String("subject", "", "exact workload subject allowed by the federation rule")
	rule := fs.String("rule-id", "", "Anthropic federation rule ID")
	org := fs.String("organization-id", "", "Anthropic organization ID")
	service := fs.String("service-account-id", "", "Anthropic service account ID")
	workspace := fs.String("workspace-id", "", "Anthropic workspace ID (required for multi-workspace rules)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	argv := fs.Args()
	if *providerName == "" || *rule == "" || *org == "" || *service == "" || len(argv) == 0 {
		return errors.New("usage: conductor wif exec --provider gcp|aws|file|local --rule-id fdrl_... --organization-id UUID --service-account-id svac_... [--workspace-id wrkspc_...] [PROVIDER OPTIONS] -- claude [ARGS...]")
	}
	if argv[0] != "claude" && !(argv[0] == "conductor" && len(argv) >= 3 && argv[1] == "wrap" && argv[2] == "claude") {
		return errors.New("wif exec accepts only claude or conductor wrap claude")
	}
	var provider wif.Provider
	var identityPath string
	switch *providerName {
	case "gcp":
		if *tokenPath != "" || *awsRegion != "" || *keyFile != "" || *issuer != "" || *subject != "" {
			return errors.New("gcp provider does not accept other provider options")
		}
		provider = wif.GCPProvider{}
	case "aws":
		if *tokenPath != "" || *awsRegion == "" || *keyFile != "" || *issuer != "" || *subject != "" {
			return errors.New("aws provider requires --aws-region and no other provider options")
		}
		provider = wif.AWSProvider{Region: *awsRegion}
	case "file":
		if *tokenPath == "" || *awsRegion != "" || *keyFile != "" || *issuer != "" || *subject != "" {
			return errors.New("file provider requires --identity-token-file and no other provider options")
		}
		provider = wif.FileProvider{Path: *tokenPath}
		identityPath = *tokenPath
	case "local":
		if *tokenPath != "" || *awsRegion != "" || *issuer == "" || *subject == "" {
			return errors.New("local provider requires --issuer and --subject and no AWS or token-file options")
		}
		path := *keyFile
		if path == "" {
			var err error
			path, err = wif.LocalKeyPath()
			if err != nil {
				return err
			}
		}
		provider = wif.LocalProvider{KeyPath: path, Issuer: *issuer, Subject: *subject}
	default:
		return fmt.Errorf("unknown WIF provider %q", *providerName)
	}
	if identityPath != "" {
		if _, err := provider.Token(ctx); err != nil {
			return fmt.Errorf("OIDC identity token preflight failed: %w", err)
		}
	} else {
		tokens, err := wif.StartTokenFile(ctx, provider)
		if err != nil {
			return fmt.Errorf("OIDC token setup failed: %w", err)
		}
		defer tokens.Stop()
		identityPath = tokens.Path
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = wifChildEnv(os.Environ(), map[string]string{
		"ANTHROPIC_FEDERATION_RULE_ID":  *rule,
		"ANTHROPIC_ORGANIZATION_ID":     *org,
		"ANTHROPIC_SERVICE_ACCOUNT_ID":  *service,
		"ANTHROPIC_IDENTITY_TOKEN_FILE": identityPath,
		"ANTHROPIC_WORKSPACE_ID":        *workspace,
	})
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Claude workload failed: %w", err)
	}
	return nil
}

func wifChildEnv(parent []string, add map[string]string) []string {
	// These variables outrank federation or route Claude to a different API.
	remove := map[string]bool{
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
		"ANTHROPIC_PROFILE": true, "ANTHROPIC_IDENTITY_TOKEN": true,
		"ANTHROPIC_IDENTITY_TOKEN_FILE": true,
		"ANTHROPIC_FEDERATION_RULE_ID":  true,
		"ANTHROPIC_ORGANIZATION_ID":     true,
		"ANTHROPIC_SERVICE_ACCOUNT_ID":  true,
		"ANTHROPIC_WORKSPACE_ID":        true, "ANTHROPIC_BASE_URL": true,
		"CLAUDE_CODE_OAUTH_TOKEN": true, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN": true,
		"CLAUDE_CODE_OAUTH_SCOPES": true,
		"CLAUDE_CODE_USE_BEDROCK":  true, "CLAUDE_CODE_USE_VERTEX": true,
		"CLAUDE_CODE_USE_FOUNDRY": true,
	}
	out := make([]string, 0, len(parent)+len(add))
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if ok && !remove[name] {
			out = append(out, kv)
		}
	}
	for name, value := range add {
		if value != "" {
			out = append(out, name+"="+value)
		}
	}
	return out
}
