package main

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// --sso takes an optional provider: bare, as --sso=NAME, or followed by the name as a
// positional argument (which cmdLogin picks up).
func TestSSOFlagForms(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		set      bool
		provider string
		rest     []string
	}{
		{args: []string{"--sso"}, set: true},
		{args: []string{"--sso=google"}, set: true, provider: "google"},
		{args: []string{"--sso", "google"}, set: true, rest: []string{"google"}},
		{args: []string{"--project", "web", "--sso"}, set: true},
		{args: []string{"--project", "web"}},
	} {
		fs := flag.NewFlagSet("login", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var f ssoFlag
		fs.Var(&f, "sso", "")
		fs.String("project", "", "")
		rest, err := parseFlags(fs, tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if f.set != tc.set || f.provider != tc.provider || strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
			t.Errorf("%v: set=%v provider=%q rest=%v", tc.args, f.set, f.provider, rest)
		}
	}
}
