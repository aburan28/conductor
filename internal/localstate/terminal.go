package localstate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Terminal spawning. When a paused session's terminal has been closed, resume must put the
// revived harness somewhere a person can type — a new window, not a background process. There
// is no portable way to open a terminal, so this walks the plausible ones in order of how
// well they match where the user already is: their explicit choice, their current tmux, their
// platform's terminal app, and finally a detached tmux session anything can attach to.

// OpenTerminal opens a new terminal running argv in cwd and returns a human-readable
// description of where the command landed.
func OpenTerminal(harness, cwd string, argv []string) (string, error) {
	return newLauncher().open(harness, cwd, argv)
}

// launcher isolates the environment probes and process spawning so the selection logic is
// testable without opening real windows.
type launcher struct {
	goos     string
	getenv   func(string) string
	lookPath func(string) (string, error)
	// run executes and waits (tmux, osascript — they return once the window exists and their
	// exit code is the verdict). spawn starts and leaves running (GUI emulators like xterm
	// live as long as their window).
	run   func(argv []string) error
	spawn func(argv []string) error
}

func newLauncher() launcher {
	return launcher{
		goos:     runtime.GOOS,
		getenv:   os.Getenv,
		lookPath: exec.LookPath,
		run: func(argv []string) error {
			out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %v: %s", filepath.Base(argv[0]), err,
					strings.TrimSpace(string(out)))
			}
			return nil
		},
		spawn: func(argv []string) error {
			cmd := exec.Command(argv[0], argv[1:]...)
			if err := cmd.Start(); err != nil {
				return err
			}
			// The terminal outlives this CLI; Release detaches so exiting does not reap it.
			return cmd.Process.Release()
		},
	}
}

// guiTerminals, in preference order. x-terminal-emulator first: on Debian-family systems it
// already is the user's choice.
var guiTerminals = []struct {
	name string
	pre  []string
}{
	{"x-terminal-emulator", []string{"-e"}},
	{"gnome-terminal", []string{"--"}},
	{"konsole", []string{"-e"}},
	{"xfce4-terminal", []string{"-x"}},
	{"kitty", nil},
	{"alacritty", []string{"-e"}},
	{"wezterm", []string{"start", "--"}},
	{"foot", nil},
	{"xterm", []string{"-e"}},
}

func (l launcher) open(harness, cwd string, argv []string) (string, error) {
	shellCmd := shellCommand(cwd, argv)

	// An operator who set CONDUCTOR_TERMINAL chose their terminal; if it fails, that is an
	// error to fix, not a cue to open something they did not ask for.
	if tmpl := l.getenv("CONDUCTOR_TERMINAL"); tmpl != "" {
		spawnArgv := expandTerminalTemplate(tmpl, shellCmd, cwd)
		if len(spawnArgv) == 0 {
			return "", fmt.Errorf("CONDUCTOR_TERMINAL expanded to an empty command")
		}
		if err := l.spawn(spawnArgv); err != nil {
			return "", fmt.Errorf("CONDUCTOR_TERMINAL: %w", err)
		}
		return "a new " + filepath.Base(spawnArgv[0]) + " terminal", nil
	}

	var attempts []string

	if l.getenv("TMUX") != "" {
		if path, err := l.lookPath("tmux"); err == nil {
			if err := l.run(tmuxNewWindow(path, harness, cwd, shellCmd, "")); err == nil {
				return "a new window in your tmux session", nil
			} else {
				attempts = append(attempts, err.Error())
			}
		}
	}

	if l.goos == "darwin" {
		if path, err := l.lookPath("osascript"); err == nil {
			if err := l.run(osascriptArgv(path, shellCmd)); err == nil {
				return "a new Terminal window", nil
			} else {
				attempts = append(attempts, err.Error())
			}
		}
	}

	if l.getenv("DISPLAY") != "" || l.getenv("WAYLAND_DISPLAY") != "" {
		for _, emu := range guiTerminals {
			path, err := l.lookPath(emu.name)
			if err != nil {
				continue
			}
			spawnArgv := append(append([]string{path}, emu.pre...), "sh", "-c", shellCmd)
			if err := l.spawn(spawnArgv); err == nil {
				return "a new " + emu.name + " window", nil
			} else {
				attempts = append(attempts, err.Error())
			}
		}
	}

	// Headless (SSH, no display): a detached tmux session still gives every revived harness
	// its own attachable terminal. New-window into the shared session first; if the session
	// does not exist yet, create it.
	if path, err := l.lookPath("tmux"); err == nil {
		if err := l.run(tmuxNewWindow(path, harness, cwd, shellCmd, "conductor:")); err == nil {
			return `a window in the detached tmux session "conductor" — attach with: tmux attach -t conductor`, nil
		}
		if err := l.run(tmuxNewSession(path, harness, cwd, shellCmd)); err == nil {
			return `a detached tmux session "conductor" — attach with: tmux attach -t conductor`, nil
		} else {
			attempts = append(attempts, err.Error())
		}
	}

	reason := "no terminal available (no $TMUX, no display, no tmux binary)"
	if len(attempts) > 0 {
		reason = strings.Join(attempts, "; ")
	}
	return "", fmt.Errorf("%s\n    set CONDUCTOR_TERMINAL, or run it yourself:  %s", reason, shellCmd)
}

func tmuxNewWindow(tmux, name, cwd, shellCmd, target string) []string {
	argv := []string{tmux, "new-window"}
	if target != "" {
		argv = append(argv, "-t", target)
	}
	if cwd != "" {
		argv = append(argv, "-c", cwd)
	}
	return append(argv, "-n", name, shellCmd)
}

func tmuxNewSession(tmux, name, cwd, shellCmd string) []string {
	argv := []string{tmux, "new-session", "-d", "-s", "conductor"}
	if cwd != "" {
		argv = append(argv, "-c", cwd)
	}
	return append(argv, "-n", name, shellCmd)
}

func osascriptArgv(osascript, shellCmd string) []string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(shellCmd)
	return []string{
		osascript,
		"-e", `tell application "Terminal" to do script "` + escaped + `"`,
		"-e", `tell application "Terminal" to activate`,
	}
}

// expandTerminalTemplate turns a CONDUCTOR_TERMINAL value into an argv. Substitution is
// per-field and never re-splits (the same rule as harness arg templates), so a command
// containing spaces stays one argument. A template without {cmd} gets `sh -c <cmd>` appended,
// which suits the common `myterm -e`-style launchers.
func expandTerminalTemplate(tmpl, shellCmd, cwd string) []string {
	var out []string
	sawCmd := false
	for _, field := range strings.Fields(tmpl) {
		if strings.Contains(field, "{cmd}") {
			sawCmd = true
		}
		expanded := strings.ReplaceAll(field, "{cmd}", shellCmd)
		expanded = strings.ReplaceAll(expanded, "{cwd}", cwd)
		if strings.TrimSpace(expanded) == "" {
			continue // an empty placeholder must not become an empty argv entry
		}
		out = append(out, expanded)
	}
	if len(out) > 0 && !sawCmd {
		out = append(out, "sh", "-c", shellCmd)
	}
	return out
}

// shellCommand renders argv as a single `cd … && exec …` string for launchers that take a
// shell command rather than an argument vector.
func shellCommand(cwd string, argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	command := "exec " + strings.Join(quoted, " ")
	if cwd != "" {
		command = "cd " + shellQuote(cwd) + " && " + command
	}
	return command
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$&|;<>()*?[]#~`!{}") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
