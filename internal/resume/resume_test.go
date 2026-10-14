package resume

import (
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/localstate"
)

func TestPlan(t *testing.T) {
	paused := localstate.Record{Status: localstate.StatusPaused}
	pausedSaved := localstate.Record{Status: localstate.StatusPaused, Saved: true}
	saved := localstate.Record{Status: localstate.StatusSaved, Saved: true}
	cases := []struct {
		name                  string
		rec                   localstate.Record
		wrapLive, harnessLive bool
		want                  string
	}{
		{"paused under a live wrap: wake via sidecar", paused, true, true, PlanViaWrap},
		{"paused bare: SIGCONT directly", paused, false, true, PlanDirect},
		{"paused, terminal gone: reopen", paused, false, false, PlanReopen},
		{"paused and saved, terminal gone: reopen", pausedSaved, false, false, PlanReopen},
		{"saved, process gone: reopen", saved, false, false, PlanReopen},
		{"saved but alive again: never signal it", saved, false, true, PlanRunning},
		{"saved but its wrap is alive: never signal it", saved, true, false, PlanRunning},
	}
	for _, tc := range cases {
		if got := Plan(tc.rec, tc.wrapLive, tc.harnessLive); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

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
