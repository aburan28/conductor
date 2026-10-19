package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSecretsInjectSnippet(t *testing.T) {
	fish := secretsInjectSnippet("fish", "home")
	if !strings.Contains(fish, "conductor secrets env --format fish --project 'home' | source") {
		t.Fatalf("fish snippet = %q", fish)
	}
	if strings.Contains(fish, "export ") {
		t.Fatalf("fish snippet uses bash export syntax: %q", fish)
	}
	bash := secretsInjectSnippet("bash", "")
	if !strings.Contains(bash, `eval "$(conductor secrets env)"`) {
		t.Fatalf("bash snippet = %q", bash)
	}
}

func TestFormatSecretLine(t *testing.T) {
	// Fish single quotes still treat \\ and \' as escapes. Bash's '"'"'
	// break would store a different value or leave the sourced line unparsable.
	fishCases := []struct{ value, want string }{
		{"a'b", "set -gx OPENAI_API_KEY -- 'a\\'b'\n"},
		{`\server\share`, "set -gx OPENAI_API_KEY -- '\\\\server\\\\share'\n"},
		{`\'`, "set -gx OPENAI_API_KEY -- '\\\\\\''\n"},
	}
	for _, tc := range fishCases {
		fish, err := formatSecretLine("fish", "OPENAI_API_KEY", tc.value)
		if err != nil {
			t.Fatal(err)
		}
		if fish != tc.want {
			t.Fatalf("fish line for %q = %q, want %q", tc.value, fish, tc.want)
		}
	}
	bash, err := formatSecretLine("export", "OPENAI_API_KEY", "x")
	if err != nil {
		t.Fatal(err)
	}
	if bash != "export OPENAI_API_KEY='x'\n" {
		t.Fatalf("export line = %q", bash)
	}
	if _, err := formatSecretLine("json", "OPENAI_API_KEY", "x"); err == nil {
		t.Fatal("json is not a shell line format")
	}
}

// The client drops reserved names whatever the server sends: an older server, or a row
// stored before the vault refused them, must not reach a shell or wrap's child environment.
func TestDropReservedSecrets(t *testing.T) {
	var warn bytes.Buffer
	env := dropReservedSecrets(map[string]string{
		"OPENAI_API_KEY": "sk", "PATH": "/evil", "LD_PRELOAD": "/x.so", "GIT_CONFIG_COUNT": "1",
	}, &warn)
	if len(env) != 1 || env["OPENAI_API_KEY"] != "sk" {
		t.Errorf("env = %v, want only OPENAI_API_KEY", env)
	}
	for _, name := range []string{"PATH", "LD_PRELOAD", "GIT_CONFIG_COUNT"} {
		if !strings.Contains(warn.String(), name) {
			t.Errorf("no warning for %s: %q", name, warn.String())
		}
	}
}
