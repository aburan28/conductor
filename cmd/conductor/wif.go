package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aburan28/conductor/internal/wif"
)

// cmdWIF runs Claude Code with a provider-issued, renewable identity token.
// It does not create an issuer or hold a signing key.
func cmdWIF(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "exec" {
		return errors.New("usage: conductor wif exec --provider gcp|aws|file [OPTIONS] -- claude [ARGS...]")
	}
	return cmdWIFExec(ctx, args[1:])
}

func cmdWIFExec(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("wif exec", flag.ContinueOnError)
	providerName := fs.String("provider", "", "gcp, aws, or file")
	tokenPath := fs.String("identity-token-file", "", "rotating OIDC token file for provider=file")
	awsRegion := fs.String("aws-region", "", "regional STS endpoint for provider=aws")
	rule := fs.String("rule-id", "", "Anthropic federation rule ID")
	org := fs.String("organization-id", "", "Anthropic organization ID")
	service := fs.String("service-account-id", "", "Anthropic service account ID")
	workspace := fs.String("workspace-id", "", "Anthropic workspace ID (required for multi-workspace rules)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	argv := fs.Args()
	if *providerName == "" || *rule == "" || *org == "" || *service == "" || len(argv) == 0 {
		return errors.New("usage: conductor wif exec --provider gcp|aws|file --rule-id fdrl_... --organization-id UUID --service-account-id svac_... [--workspace-id wrkspc_...] [PROVIDER OPTIONS] -- claude [ARGS...]")
	}
	if argv[0] != "claude" && !(argv[0] == "conductor" && len(argv) >= 3 && argv[1] == "wrap" && argv[2] == "claude") {
		return errors.New("wif exec accepts only claude or conductor wrap claude")
	}
	var provider wif.Provider
	var identityPath string
	switch *providerName {
	case "gcp":
		if *tokenPath != "" || *awsRegion != "" {
			return errors.New("gcp provider does not accept file or AWS options")
		}
		provider = wif.GCPProvider{}
	case "aws":
		if *tokenPath != "" || *awsRegion == "" {
			return errors.New("aws provider requires --aws-region and no identity-token-file")
		}
		provider = wif.AWSProvider{Region: *awsRegion}
	case "file":
		if *tokenPath == "" || *awsRegion != "" {
			return errors.New("file provider requires --identity-token-file and no AWS region")
		}
		provider = wif.FileProvider{Path: *tokenPath}
		identityPath = *tokenPath
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
		"CLAUDE_CODE_USE_BEDROCK": true, "CLAUDE_CODE_USE_VERTEX": true,
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
