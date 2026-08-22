package localstate

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// VS Code integration. VS Code cannot be told to open an integrated terminal from the
// command line alone, so resume hands the session to the companion extension
// (integrations/vscode), which registers a vscode:// URI handler that can.
//
// The handoff is deliberately a pointer, not a payload: the URI carries only the record id,
// and the extension reads the record from the state directory itself. A URI that carried a
// command line would let any page that can render a vscode:// link inject one; a record id is
// useless to anyone who cannot already write this user's 0600 files.
//
// Ownership of the record passes with the handoff. The CLI does not mark the session resumed
// or remove the record — the extension deletes it once the terminal is actually open — so a
// handoff into a VS Code that never handles it (extension uninstalled mid-flight, window
// closed) leaves the session paused and retryable rather than silently lost.

// VSCodeExtensionID is the companion extension's identifier (publisher.name), which is also
// the authority of the vscode:// URIs it handles.
const VSCodeExtensionID = "conductor.conductor-sessions"

// TermProgramVSCode is what VS Code's integrated terminal sets TERM_PROGRAM to.
const TermProgramVSCode = "vscode"

// ResumeInVSCode reopens a paused session in a VS Code integrated terminal, when that is
// where the session lived and the pieces are in place: the `code` CLI on PATH and the
// companion extension installed. It reports a human-readable destination and whether the
// handoff was dispatched; false means the caller should fall back to opening a terminal
// itself.
func ResumeInVSCode(rec Record) (string, bool) {
	return defaultVSCodeEnv().resume(rec)
}

// vscodeEnv isolates the probes so the decision logic is testable without VS Code installed.
type vscodeEnv struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	output   func(argv []string) (string, error)
	run      func(argv []string) error
}

func defaultVSCodeEnv() vscodeEnv {
	return vscodeEnv{
		getenv:   os.Getenv,
		lookPath: exec.LookPath,
		output: func(argv []string) (string, error) {
			out, err := exec.Command(argv[0], argv[1:]...).Output()
			return string(out), err
		},
		run: func(argv []string) error {
			out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %v: %s", argv[0], err, strings.TrimSpace(string(out)))
			}
			return nil
		},
	}
}

func (v vscodeEnv) resume(rec Record) (string, bool) {
	// CONDUCTOR_TERMINAL is the operator saying where terminals open; an explicit choice
	// outranks this integration, exactly as it outranks tmux and the platform terminal.
	if v.getenv("CONDUCTOR_TERMINAL") != "" {
		return "", false
	}
	if rec.TermProgram != TermProgramVSCode {
		return "", false
	}
	codePath, err := v.lookPath("code")
	if err != nil {
		return "", false
	}
	// Firing the URI without the extension installed would look like success and do nothing,
	// which is the worst outcome; a listing miss just falls back to a real terminal.
	installed, err := v.output([]string{codePath, "--list-extensions"})
	if err != nil || !containsExtension(installed, VSCodeExtensionID) {
		return "", false
	}
	uri := "vscode://" + VSCodeExtensionID + "/resume?id=" + url.QueryEscape(rec.ID)
	if err := v.run([]string{codePath, "--open-url", uri}); err != nil {
		return "", false
	}
	return "VS Code (the Conductor extension is opening an integrated terminal)", true
}

func containsExtension(listing, id string) bool {
	for _, line := range strings.Split(listing, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), id) {
			return true
		}
	}
	return false
}
