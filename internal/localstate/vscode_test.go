package localstate

import (
	"errors"
	"strings"
	"testing"
)

// fakeVSCode records what would have been executed instead of poking a real VS Code.
type fakeVSCode struct {
	vscodeEnv
	codeInstalled bool
	extensions    string
	opened        [][]string
	openErr       error
}

func newFakeVSCode(codeInstalled bool, extensions string) *fakeVSCode {
	f := &fakeVSCode{codeInstalled: codeInstalled, extensions: extensions}
	f.vscodeEnv = vscodeEnv{
		getenv: func(string) string { return "" },
		lookPath: func(name string) (string, error) {
			if name == "code" && f.codeInstalled {
				return "/usr/bin/code", nil
			}
			return "", errors.New(name + " not found")
		},
		output: func(argv []string) (string, error) {
			if len(argv) == 2 && argv[1] == "--list-extensions" {
				return f.extensions, nil
			}
			return "", errors.New("unexpected command")
		},
		run: func(argv []string) error {
			if f.openErr != nil {
				return f.openErr
			}
			f.opened = append(f.opened, argv)
			return nil
		},
	}
	return f
}

func TestVSCodeHandoffDispatchesURI(t *testing.T) {
	f := newFakeVSCode(true, "golang.go\nConductor.conductor-sessions\n")

	rec := Record{ID: "abc-123", Harness: "claude", TermProgram: TermProgramVSCode}
	where, ok := f.resume(rec)
	if !ok {
		t.Fatal("handoff refused with everything in place")
	}
	if !strings.Contains(where, "VS Code") {
		t.Errorf("where = %q", where)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened %v, want one --open-url invocation", f.opened)
	}
	got := strings.Join(f.opened[0], " ")
	want := "/usr/bin/code --open-url vscode://conductor.conductor-sessions/resume?id=abc-123"
	if got != want {
		t.Errorf("argv:\n  got  %s\n  want %s", got, want)
	}
}

func TestVSCodeHandoffRefusesWhenNotApplicable(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		fake *fakeVSCode
	}{
		{
			// A session from a plain terminal must not be teleported into an editor.
			name: "session did not live in VS Code",
			rec:  Record{ID: "x", TermProgram: "iTerm.app"},
			fake: newFakeVSCode(true, VSCodeExtensionID),
		},
		{
			name: "no code CLI on PATH",
			rec:  Record{ID: "x", TermProgram: TermProgramVSCode},
			fake: newFakeVSCode(false, ""),
		},
		{
			// Firing the URI without the extension would look like success and do nothing —
			// the worst outcome. It must fall back to a real terminal instead.
			name: "extension not installed",
			rec:  Record{ID: "x", TermProgram: TermProgramVSCode},
			fake: newFakeVSCode(true, "golang.go\nms-python.python\n"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.fake.resume(tc.rec); ok {
				t.Error("handoff dispatched")
			}
			if len(tc.fake.opened) != 0 {
				t.Errorf("opened %v, want nothing", tc.fake.opened)
			}
		})
	}
}

func TestVSCodeHandoffEscapesRecordID(t *testing.T) {
	f := newFakeVSCode(true, VSCodeExtensionID)
	if _, ok := f.resume(Record{ID: "a b&c", TermProgram: TermProgramVSCode}); !ok {
		t.Fatal("handoff refused")
	}
	uri := f.opened[0][2]
	if strings.ContainsAny(uri, " &") && !strings.Contains(uri, "id=a+b%26c") {
		t.Errorf("record id not query-escaped: %q", uri)
	}
}

// An explicit CONDUCTOR_TERMINAL outranks the VS Code handoff, exactly as it outranks tmux
// and the platform terminal in the ordinary spawn chain.
func TestVSCodeHandoffDefersToConductorTerminal(t *testing.T) {
	f := newFakeVSCode(true, VSCodeExtensionID)
	f.getenv = func(k string) string {
		if k == "CONDUCTOR_TERMINAL" {
			return "kitty sh -c {cmd}"
		}
		return ""
	}
	if _, ok := f.resume(Record{ID: "x", TermProgram: TermProgramVSCode}); ok {
		t.Error("handoff dispatched despite an explicit CONDUCTOR_TERMINAL")
	}
}

func TestVSCodeHandoffOpenFailureFallsBack(t *testing.T) {
	f := newFakeVSCode(true, VSCodeExtensionID)
	f.openErr = errors.New("code crashed")
	if _, ok := f.resume(Record{ID: "x", TermProgram: TermProgramVSCode}); ok {
		t.Error("a failed --open-url must report false so the caller opens a terminal itself")
	}
}

func TestEnvValue(t *testing.T) {
	block := []byte("HOME=/home/alice\x00TERM_PROGRAM=vscode\x00TERM_PROGRAM_VERSION=1.96.0\x00")
	if got := envValue(block, "TERM_PROGRAM"); got != "vscode" {
		t.Errorf("envValue(TERM_PROGRAM) = %q", got)
	}
	if got := envValue(block, "MISSING"); got != "" {
		t.Errorf("envValue(MISSING) = %q", got)
	}
	// TERM_PROGRAM must not match the longer TERM_PROGRAM_VERSION key.
	block = []byte("TERM_PROGRAM_VERSION=1.96.0\x00")
	if got := envValue(block, "TERM_PROGRAM"); got != "" {
		t.Errorf("envValue matched a prefix of a longer key: %q", got)
	}
}
