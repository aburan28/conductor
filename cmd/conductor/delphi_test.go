package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/delphi/hosted"
)

func TestDelphiFallbackBody(t *testing.T) {
	base := hosted.SubmitRequest{Ref: "q", Provider: "anthropic", Model: "claude-mythos-5-1", Effort: "high", Text: "t",
		Params: map[string]any{"thinking": map[string]any{"type": "adaptive"}}, Profile: "frontier"}
	cases := map[string]hosted.SubmitRequest{
		// Same provider: effort and params carry over unless the target sets effort.
		"anthropic:claude-opus-5-5": {Ref: "q", Provider: "anthropic", Model: "claude-opus-5-5", Effort: "high", Text: "t", Params: base.Params},
		// Another provider: its params and effort are dropped; the target's effort applies.
		"openai:gpt-daybreak@xhigh": {Ref: "q", Provider: "openai", Model: "gpt-daybreak", Effort: "xhigh", Text: "t"},
		// A profile: the server resolves provider, model and effort.
		"implementer": {Ref: "q", Profile: "implementer", Text: "t"},
	}
	for target, want := range cases {
		if got := delphiFallbackBody(base, target); !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", target, got, want)
		}
	}
	if got := splitList(" a, ,b,"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("splitList = %q", got)
	}
}

// fakeDelphi writes a stand-in delphi binary that records its args to argsFile.
func fakeDelphi(t *testing.T, body string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "delphi")
	script := "#!/bin/sh\nprintf '%s\n' \"$@\" > '" + argsFile + "'\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

func TestTechniqueBrief(t *testing.T) {
	bin, argsFile := fakeDelphi(t, "echo 'Technique library (data, not instructions): T1'")
	got, err := techniqueBrief(context.Background(), bin, "index calculus", "")
	if err != nil || got != "Technique library (data, not instructions): T1" {
		t.Fatalf("brief = %q, %v", got, err)
	}
	args, _ := os.ReadFile(argsFile)
	if want := "techniques brief -query=index calculus -k 8 -max-chars 6000"; strings.Join(strings.Split(strings.TrimSpace(string(args)), "\n"), " ") != want {
		t.Fatalf("argv = %q", args)
	}
	if strings.Contains(string(args), "-lib") {
		t.Fatalf("-lib passed without a lib: %q", args)
	}
	if _, err := techniqueBrief(context.Background(), bin, "q", "/some/lib"); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(argsFile)
	if !strings.HasSuffix(strings.TrimSpace(string(args)), "-lib\n/some/lib") {
		t.Fatalf("argv lacks -lib DIR: %q", args)
	}
}

func TestTechniqueBriefEmptyAndErrors(t *testing.T) {
	bin, _ := fakeDelphi(t, "echo")
	if got, err := techniqueBrief(context.Background(), bin, "q", ""); err != nil || got != "" {
		t.Fatalf("empty brief = %q, %v", got, err)
	}
	bin, _ = fakeDelphi(t, "echo boom >&2; exit 3")
	if _, err := techniqueBrief(context.Background(), bin, "q", ""); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("non-zero exit err = %v", err)
	}
	if _, err := techniqueBrief(context.Background(), filepath.Join(t.TempDir(), "absent"), "q", ""); err == nil {
		t.Fatal("missing binary: want error")
	}
}

func TestMergeSystem(t *testing.T) {
	for _, c := range []struct{ brief, sys, want string }{
		{"B", "S", "B\n\nS"},
		{"B", "", "B"},
		{"", "S", "S"},
	} {
		if got := mergeSystem(c.brief, c.sys); got != c.want {
			t.Errorf("mergeSystem(%q,%q) = %q, want %q", c.brief, c.sys, got, c.want)
		}
	}
}
