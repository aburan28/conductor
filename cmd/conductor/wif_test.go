package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWIFChildEnvOverridesCredentials(t *testing.T) {
	got := wifChildEnv([]string{
		"PATH=/bin", "ANTHROPIC_API_KEY=old", "ANTHROPIC_AUTH_TOKEN=old",
		"ANTHROPIC_PROFILE=old", "ANTHROPIC_BASE_URL=https://other.example",
		"CLAUDE_CODE_USE_BEDROCK=1", "ANTHROPIC_FEDERATION_RULE_ID=old",
	}, map[string]string{
		"ANTHROPIC_FEDERATION_RULE_ID":  "fdrl_new",
		"ANTHROPIC_IDENTITY_TOKEN_FILE": "/tmp/token",
	})
	s := strings.Join(got, "\n")
	for _, forbidden := range []string{"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=",
		"ANTHROPIC_PROFILE=", "ANTHROPIC_BASE_URL=", "CLAUDE_CODE_USE_BEDROCK=", "fdrl_old"} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("conflicting credential leaked into child: %s", s)
		}
	}
	for _, required := range []string{"PATH=/bin", "ANTHROPIC_FEDERATION_RULE_ID=fdrl_new",
		"ANTHROPIC_IDENTITY_TOKEN_FILE=/tmp/token"} {
		if !strings.Contains(s, required) {
			t.Fatalf("missing child setting %q: %s", required, s)
		}
	}
}

func TestWIFExecPassesFileTokenToClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test shim uses a POSIX shell")
	}
	dir := t.TempDir()
	claims, err := json.Marshal(map[string]any{
		"iss": "https://issuer.example", "sub": "runner", "aud": "https://api.anthropic.com",
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".c2ln"
	tokenPath := filepath.Join(dir, "identity")
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "received")
	shim := filepath.Join(dir, "claude")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s\\n%s\\n%s\\n' \"$ANTHROPIC_FEDERATION_RULE_ID\" \"$ANTHROPIC_IDENTITY_TOKEN_FILE\" \"$ANTHROPIC_API_KEY\" > \"$WIF_TEST_OUTPUT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WIF_TEST_OUTPUT", output)
	t.Setenv("ANTHROPIC_API_KEY", "must-be-removed")
	if err := cmdWIFExec(context.Background(), []string{
		"--provider", "file", "--identity-token-file", tokenPath,
		"--rule-id", "fdrl_test", "--organization-id", "org-test",
		"--service-account-id", "svac_test", "--", "claude",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "fdrl_test\n"+tokenPath+"\n\n" {
		t.Fatalf("Claude received unexpected federation environment: %q", got)
	}
}
