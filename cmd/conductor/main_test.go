package main

import (
	"flag"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
)

// Regression test for a silent wrong-result bug: the stdlib flag package stops parsing at the
// first positional argument, so `conductor member add rachel --role maintainer` parsed no
// flags at all and created a contributor without complaining.

func TestParseFlagsAcceptsFlagsAfterPositionals(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantRole string
		wantPos  []string
	}{
		{"flags first", []string{"--role", "maintainer", "rachel"}, "maintainer", []string{"rachel"}},
		{"flags last", []string{"rachel", "--role", "maintainer"}, "maintainer", []string{"rachel"}},
		{"equals form after positional", []string{"rachel", "--role=maintainer"}, "maintainer", []string{"rachel"}},
		{"single dash", []string{"rachel", "-role", "maintainer"}, "maintainer", []string{"rachel"}},
		{"no flags", []string{"rachel"}, "contributor", []string{"rachel"}},
		{"interleaved with two positionals", []string{"T-1", "--role", "maintainer", "path:a.go"},
			"maintainer", []string{"T-1", "path:a.go"}},
		{"no positionals", []string{"--role", "maintainer"}, "maintainer", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			role := fs.String("role", "contributor", "")
			fs.SetOutput(discard{})

			positional, err := parseFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if *role != tc.wantRole {
				t.Errorf("role = %q, want %q", *role, tc.wantRole)
			}
			if len(positional) != len(tc.wantPos) {
				t.Fatalf("positional = %v, want %v", positional, tc.wantPos)
			}
			for i := range positional {
				if positional[i] != tc.wantPos[i] {
					t.Errorf("positional[%d] = %q, want %q", i, positional[i], tc.wantPos[i])
				}
			}
		})
	}
}

func TestParseFlagsCollectsRepeatableFlagsAroundPositionals(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(discard{})
	var scopes scopeFlag
	fs.Var(&scopes, "scope", "")

	positional, err := parseFlags(fs, []string{
		"--scope", "path:a.go", "T-1", "--scope", "dir:internal/api:read",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if len(positional) != 1 || positional[0] != "T-1" {
		t.Fatalf("positional = %v, want [T-1]", positional)
	}
	if len(scopes) != 2 {
		t.Fatalf("collected %d scopes, want 2", len(scopes))
	}
	if scopes[0].Resource != "path:a.go" || scopes[0].Mode != domain.ModeWriteExclusive {
		t.Errorf("scopes[0] = %+v", scopes[0])
	}
	// The `:read` suffix selects the mode without a second flag.
	if scopes[1].Resource != "dir:internal/api" || scopes[1].Mode != domain.ModeReadShared {
		t.Errorf("scopes[1] = %+v, want dir:internal/api read_shared", scopes[1])
	}
}

func TestParseFlagsReportsUnknownFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(discard{})
	fs.String("role", "", "")

	if _, err := parseFlags(fs, []string{"rachel", "--nonsense"}); err == nil {
		t.Error("an unknown flag should be an error, not silently ignored")
	}
}

func TestScopeFlagModeSuffixes(t *testing.T) {
	cases := map[string]struct {
		resource string
		mode     domain.ReservationMode
	}{
		"path:a.go":                   {"path:a.go", domain.ModeWriteExclusive},
		"path:a.go:read":              {"path:a.go", domain.ModeReadShared},
		"dir:internal:write":          {"dir:internal", domain.ModeWriteExclusive},
		"migration:primary:protected": {"migration:primary", domain.ModeProtectedExclusive},
		"dir:x:review":                {"dir:x", domain.ModeReviewShared},
		"dir:x:speculative":           {"dir:x", domain.ModeSpeculativeWrite},
		"api:GET /v1/tasks":           {"api:GET /v1/tasks", domain.ModeWriteExclusive},
	}
	for input, want := range cases {
		var s scopeFlag
		if err := s.Set(input); err != nil {
			t.Fatalf("Set(%q): %v", input, err)
		}
		if s[0].Resource != want.resource || s[0].Mode != want.mode {
			t.Errorf("Set(%q) = {%q, %s}, want {%q, %s}",
				input, s[0].Resource, s[0].Mode, want.resource, want.mode)
		}
	}
}

func TestServeVariantAliasesCoverLocalModels(t *testing.T) {
	aliases := serveVariantAliases()
	for _, name := range []string{"flash", "glm53", "qwen", "qwen3.8-27b", "glm-5.3-flash"} {
		if aliases[name] == "" {
			t.Errorf("missing serve alias %q", name)
		}
	}
	if !knownServeArg("flash") || !knownServeArg("status") {
		t.Error("flash and status should be recognized serve args")
	}
	if knownServeArg("llama") {
		t.Error("unknown models must not be treated as serve args")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
