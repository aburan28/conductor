package localstate

import "testing"

// The ps snapshot below is one machine's worth of terminals:
//
//	pid 300  a bare claude session on pts/1                       → discovered
//	pid 310  a claude subagent spawned by that session            → folded into its parent
//	pid 400  conductor wrap (known record, WrapPID)               → skipped
//	pid 401  the codex it wraps (known record, PID)               → skipped
//	pid 500  `codex exec --json` — a runner attempt, not a TUI    → skipped
//	pid 510  `opencode run` — one-shot, nothing to resume         → skipped
//	pid 520  `claude -p` — headless print mode                    → skipped
//	pid 600  a bare opencode session on pts/5                     → discovered
//	pid 610  claude with no tty — no terminal to pause            → skipped
//	pid 620  someone else's claude (uid 1001)                     → skipped
//	pid 700  claude via node interpreter                          → discovered
//	pid 900  `conductor pause` itself, in its own terminal        → skipped (no harness)
const psSnapshot = `
    1     1     0    0 ?        /sbin/init
  200   200   199 1000 pts/1    -bash
  300   300   200 1000 pts/1    /usr/local/bin/claude --model claude-opus-5
  310   300   300 1000 pts/1    claude --agent reviewer
  399   399   198 1000 pts/2    -bash
  400   400   399 1000 pts/2    /home/alice/bin/conductor wrap codex
  401   400   400 1000 pts/2    codex
  500   500   450 1000 pts/3    codex exec --json
  510   510   450 1000 pts/3    opencode run --print-logs
  520   520   450 1000 pts/3    claude -p --output-format stream-json --verbose
  600   600   599 1000 pts/5    opencode
  610   610   450 1000 ?        claude
  620   620   619 1001 pts/6    claude
  700   700   699 1000 pts/7    node /home/alice/.claude/local/claude
  250   250   199 1000 pts/9    -bash
  900   900   250 1000 pts/9    conductor pause
  800   300   300 1000 pts/1    bash -c conductor pause
  801   300   800 1000 pts/1    conductor pause
`

func TestDiscoverFrom(t *testing.T) {
	known := []Record{{ID: "w", Harness: "codex", PID: 401, WrapPID: 400, Wrapped: true}}

	records := discoverFrom(psSnapshot, 900, 1000, known)

	byPID := map[int]Record{}
	for _, r := range records {
		byPID[r.PID] = r
	}
	if len(records) != 3 {
		t.Fatalf("discovered %d sessions %v, want pids 300, 600, 700", len(records), byPID)
	}

	claude, ok := byPID[300]
	if !ok {
		t.Fatal("the bare claude session on pts/1 was not discovered")
	}
	if claude.Harness != "claude" || claude.TTY != "pts/1" || claude.PGID != 300 {
		t.Errorf("claude record = %+v", claude)
	}
	if len(claude.Args) != 2 || claude.Args[0] != "--model" {
		t.Errorf("claude Args = %v, want the argv after the harness", claude.Args)
	}
	if len(claude.ResumeArgs) != 1 || claude.ResumeArgs[0] != "--continue" {
		t.Errorf("claude ResumeArgs = %v", claude.ResumeArgs)
	}
	if claude.Status != StatusRunning || claude.ID != "p300" {
		t.Errorf("claude record = %+v", claude)
	}

	if _, ok := byPID[600]; !ok {
		t.Error("the bare opencode session was not discovered")
	}
	if interp, ok := byPID[700]; !ok {
		t.Error("the interpreter-launched claude was not discovered")
	} else if interp.Harness != "claude" || interp.Command != "claude" {
		t.Errorf("interpreter-launched record = %+v; the script, not node, names the harness", interp)
	}
}

// Running `conductor pause` from inside an agent (a Bash tool call, say) must not discover
// that agent: freezing an ancestor freezes the pause command itself, and nothing would be
// left running to finish the job.
func TestDiscoverFromNeverFreezesItsOwnAncestry(t *testing.T) {
	records := discoverFrom(psSnapshot, 801, 1000, nil)
	for _, r := range records {
		if r.PID == 300 {
			t.Fatal("discovered the claude session that conductor pause is running inside of")
		}
	}
}

func TestMatchHarness(t *testing.T) {
	cases := []struct {
		args    []string
		harness string
		restLen int
		matched bool
	}{
		{[]string{"claude"}, "claude", 0, true},
		{[]string{"/usr/local/bin/codex", "-m", "gpt-5-codex"}, "codex", 2, true},
		{[]string{"node", "/home/a/.claude/local/claude", "--continue"}, "claude", 1, true},
		{[]string{"node", "--max-old-space-size=4096", "/opt/claude"}, "claude", 0, true},
		{[]string{"node", "server.js"}, "", 0, false},
		{[]string{"vim", "claude.go"}, "", 0, false},
		{[]string{}, "", 0, false},
	}
	for _, tc := range cases {
		harness, rest, ok := matchHarness(tc.args)
		if ok != tc.matched || harness != tc.harness || len(rest) != tc.restLen {
			t.Errorf("matchHarness(%v) = (%q, %v, %v), want (%q, len %d, %v)",
				tc.args, harness, rest, ok, tc.harness, tc.restLen, tc.matched)
		}
	}
}

func TestNonInteractive(t *testing.T) {
	cases := []struct {
		harness string
		rest    []string
		want    bool
	}{
		{"claude", nil, false},
		{"claude", []string{"-p", "--output-format", "stream-json"}, true},
		{"claude", []string{"--print"}, true},
		{"claude", []string{"mcp", "serve"}, true},
		{"claude", []string{"--model", "claude-opus-5"}, false},
		{"codex", nil, false},
		{"codex", []string{"exec", "--json"}, true},
		{"codex", []string{"mcp"}, true},
		{"codex", []string{"resume", "--last"}, false},
		{"opencode", nil, false},
		{"opencode", []string{"run", "hello"}, true},
		{"opencode", []string{"serve"}, true},
		{"opencode", []string{"--continue"}, false},
	}
	for _, tc := range cases {
		if got := nonInteractive(tc.harness, tc.rest); got != tc.want {
			t.Errorf("nonInteractive(%s, %v) = %v, want %v", tc.harness, tc.rest, got, tc.want)
		}
	}
}

func TestParsePSOutputSkipsMalformedLines(t *testing.T) {
	procs := parsePSOutput("garbage\n  10 x 1 1000 pts/0 claude\n  11 11 1 1000 pts/0 claude\n")
	if len(procs) != 1 || procs[0].pid != 11 {
		t.Errorf("parsePSOutput = %+v, want only the well-formed line", procs)
	}
}
