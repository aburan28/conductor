package localstate

import (
	"errors"
	"strings"
	"testing"
)

// fakeLauncher records what would have been executed instead of opening windows.
type fakeLauncher struct {
	launcher
	installed map[string]bool
	env       map[string]string
	ran       [][]string
	spawned   [][]string
	runErr    map[string]error // keyed by argv[1] (the subcommand), for tmux failures
}

func newFakeLauncher(goos string, env map[string]string, installed ...string) *fakeLauncher {
	f := &fakeLauncher{installed: map[string]bool{}, env: env, runErr: map[string]error{}}
	for _, name := range installed {
		f.installed[name] = true
	}
	f.launcher = launcher{
		goos:   goos,
		getenv: func(k string) string { return env[k] },
		lookPath: func(name string) (string, error) {
			if f.installed[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New(name + " not found")
		},
		run: func(argv []string) error {
			if len(argv) > 1 {
				if err := f.runErr[argv[1]]; err != nil {
					return err
				}
			}
			f.ran = append(f.ran, argv)
			return nil
		},
		spawn: func(argv []string) error {
			f.spawned = append(f.spawned, argv)
			return nil
		},
	}
	return f
}

func TestOpenTerminalPrefersCurrentTmux(t *testing.T) {
	f := newFakeLauncher("linux", map[string]string{"TMUX": "/tmp/tmux-1000/default,123,0"}, "tmux")

	where, err := f.open("claude", "/home/alice/repo", []string{"claude", "--continue"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !strings.Contains(where, "tmux") {
		t.Errorf("where = %q, want a tmux window", where)
	}
	if len(f.ran) != 1 {
		t.Fatalf("ran %v, want one tmux invocation", f.ran)
	}
	got := strings.Join(f.ran[0], " ")
	want := "/usr/bin/tmux new-window -c /home/alice/repo -n claude cd /home/alice/repo && exec claude --continue"
	if got != want {
		t.Errorf("tmux argv:\n  got  %s\n  want %s", got, want)
	}
}

func TestOpenTerminalTemplateWithCmdPlaceholder(t *testing.T) {
	f := newFakeLauncher("linux", map[string]string{
		"CONDUCTOR_TERMINAL": "kitty --directory {cwd} sh -c {cmd}",
		"TMUX":               "ignored-when-template-set",
	}, "tmux")

	if _, err := f.open("codex", "/w", []string{"codex", "resume", "--last"}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(f.spawned) != 1 {
		t.Fatalf("spawned %v, want the template command", f.spawned)
	}
	argv := f.spawned[0]
	if argv[0] != "kitty" || argv[1] != "--directory" || argv[2] != "/w" {
		t.Errorf("template argv = %v", argv)
	}
	// {cmd} is substituted as ONE argument, never re-split.
	last := argv[len(argv)-1]
	if last != "cd /w && exec codex resume --last" {
		t.Errorf("cmd argument = %q", last)
	}
	if len(f.ran) != 0 {
		t.Error("a template must preempt tmux, not run alongside it")
	}
}

func TestOpenTerminalTemplateWithoutCmdAppendsShell(t *testing.T) {
	f := newFakeLauncher("linux", map[string]string{"CONDUCTOR_TERMINAL": "myterm -e"})

	if _, err := f.open("claude", "", []string{"claude", "--continue"}); err != nil {
		t.Fatalf("open: %v", err)
	}
	got := strings.Join(f.spawned[0], " ")
	if got != "myterm -e sh -c exec claude --continue" {
		t.Errorf("argv = %q, want sh -c appended", got)
	}
}

func TestOpenTerminalDarwinUsesTerminalApp(t *testing.T) {
	f := newFakeLauncher("darwin", map[string]string{}, "osascript")

	where, err := f.open("claude", `/tmp/it's here`, []string{"claude", "--continue"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !strings.Contains(where, "Terminal") {
		t.Errorf("where = %q", where)
	}
	script := f.ran[0][2]
	// The shell quoting survives inside the AppleScript string, escaped for AppleScript.
	if !strings.Contains(script, `do script`) || !strings.Contains(script, `--continue`) {
		t.Errorf("osascript = %q", script)
	}
}

func TestOpenTerminalPicksInstalledGUIEmulator(t *testing.T) {
	f := newFakeLauncher("linux", map[string]string{"DISPLAY": ":0"}, "alacritty", "xterm")

	where, err := f.open("opencode", "/w", []string{"opencode", "--continue"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !strings.Contains(where, "alacritty") {
		t.Errorf("where = %q; alacritty outranks xterm in the preference order", where)
	}
	got := strings.Join(f.spawned[0], " ")
	if got != "/usr/bin/alacritty -e sh -c cd /w && exec opencode --continue" {
		t.Errorf("argv = %q", got)
	}
}

func TestOpenTerminalHeadlessFallsBackToDetachedTmux(t *testing.T) {
	f := newFakeLauncher("linux", map[string]string{}, "tmux")
	// The shared "conductor" session does not exist yet: new-window into it fails once.
	f.runErr["new-window"] = errors.New("no such session: conductor")

	where, err := f.open("claude", "/w", []string{"claude", "--continue"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !strings.Contains(where, "tmux attach -t conductor") {
		t.Errorf("where = %q; the user must be told how to reach a detached session", where)
	}
	got := strings.Join(f.ran[0], " ")
	if !strings.HasPrefix(got, "/usr/bin/tmux new-session -d -s conductor") {
		t.Errorf("argv = %q", got)
	}
}

func TestOpenTerminalWithNothingAvailableSaysHowToRunManually(t *testing.T) {
	f := newFakeLauncher("linux", map[string]string{})

	_, err := f.open("claude", "/w", []string{"claude", "--continue"})
	if err == nil {
		t.Fatal("open succeeded with no terminal mechanism at all")
	}
	if !strings.Contains(err.Error(), "cd /w && exec claude --continue") {
		t.Errorf("error %q must include the manual command", err)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":        "plain",
		"":             "''",
		"two words":    "'two words'",
		"it's":         `'it'\''s'`,
		"a\"b":         `'a"b'`,
		"$HOME":        "'$HOME'",
		"--continue":   "--continue",
		"/path/to/dir": "/path/to/dir",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
