package main

import (
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/localstate"
)

func TestRelaunchArgv(t *testing.T) {
	cases := []struct {
		name      string
		rec       localstate.Record
		want      string
		wantFresh bool
	}{
		{
			name: "wrapped claude keeps its capability flags and re-registers",
			rec: localstate.Record{
				Harness: "claude", Wrapped: true,
				WrapFlags:  []string{"--effort", "high"},
				ResumeArgs: []string{"--continue"},
			},
			want: "/bin/conductor wrap --effort high claude --continue",
		},
		{
			name: "bare codex resumes its last conversation",
			rec: localstate.Record{
				Harness: "codex", Command: "codex",
				ResumeArgs: []string{"resume", "--last"},
			},
			want: "codex resume --last",
		},
		{
			name: "unknown harness relaunches fresh with its original arguments",
			rec: localstate.Record{
				Harness: "aider", Command: "aider",
				Args: []string{"--model", "gpt-5"},
			},
			want:      "aider --model gpt-5",
			wantFresh: true,
		},
		{
			name: "record without a command falls back to the harness name",
			rec: localstate.Record{
				Harness:    "opencode",
				ResumeArgs: []string{"--continue"},
			},
			want: "opencode --continue",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, fresh := relaunchArgv(tc.rec, "/bin/conductor")
			if got := strings.Join(argv, " "); got != tc.want {
				t.Errorf("argv = %q, want %q", got, tc.want)
			}
			if fresh != tc.wantFresh {
				t.Errorf("fresh = %v, want %v", fresh, tc.wantFresh)
			}
		})
	}
}

func TestControlAction(t *testing.T) {
	cases := []struct {
		name    string
		pending domain.SessionControl
		paused  bool
		want    domain.SessionControl
	}{
		{"pause a running session", domain.ControlPause, false, domain.ControlPause},
		{"resume a paused session", domain.ControlResume, true, domain.ControlResume},
		{"pause already paused", domain.ControlPause, true, ""},
		{"resume already running", domain.ControlResume, false, ""},
		{"nothing pending", "", true, ""},
		{"nothing pending at all", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := controlAction(tc.pending, tc.paused); got != tc.want {
				t.Errorf("controlAction(%q, %v) = %q, want %q", tc.pending, tc.paused, got, tc.want)
			}
		})
	}
}

func TestControlAck(t *testing.T) {
	if got := controlAck(true); got != domain.ControlPause {
		t.Errorf("controlAck(true) = %q, want pause", got)
	}
	if got := controlAck(false); got != domain.ControlResume {
		t.Errorf("controlAck(false) = %q, want resume", got)
	}
}
