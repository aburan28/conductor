package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/adamburan/conductor/internal/version"
)

// The CLI under test, built once per test run with a stamped version, so exit codes and the
// -ldflags path are exercised exactly as a user meets them.
var (
	cliOnce sync.Once
	cliPath string
	cliErr  error
)

const stampedTestVersion = "v9.8.7-test"

func buildCLI(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the CLI; skipped in -short mode")
	}
	cliOnce.Do(func() {
		dir, err := os.MkdirTemp("", "conductor-cli-test")
		if err != nil {
			cliErr = err
			return
		}
		cliPath = filepath.Join(dir, "conductor")
		pkg := reflect.TypeOf(version.Info{}).PkgPath()
		cmd := exec.Command("go", "build", "-o", cliPath,
			"-ldflags", "-X "+pkg+".version="+stampedTestVersion, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			cliErr = err
			cliPath = string(out)
		}
	})
	if cliErr != nil {
		t.Fatalf("building the CLI: %v\n%s", cliErr, cliPath)
	}
	return cliPath
}

// runCLI runs the CLI with no login and an endpoint nothing listens on, so a command that
// wrongly goes to the network instead of printing help fails fast and visibly.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(buildCLI(t), args...)
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"CONDUCTOR_ENDPOINT=http://127.0.0.1:1",
		"CONDUCTOR_NO_LOCAL_LOGIN=1",
		"CONDUCTOR_TOKEN=",
	)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

// Every command answers -h and --help with its own page on stdout and exit 0. Before, seven
// command groups answered `-h` with `unknown … subcommand "-h"` and exit 1, and wrap called
// it an unknown flag.
func TestEveryCommandAnswersHelp(t *testing.T) {
	for _, c := range commands {
		for _, flag := range []string{"-h", "--help"} {
			t.Run(c.name+" "+flag, func(t *testing.T) {
				stdout, stderr, code := runCLI(t, c.name, flag)
				if code != 0 {
					t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
				}
				if !strings.Contains(stdout, "conductor "+c.name) && !strings.Contains(stdout, "Usage of "+c.name) {
					t.Errorf("stdout does not describe %s:\n%s\nstderr:\n%s", c.name, stdout, stderr)
				}
			})
		}
	}
}

// `conductor help <topic>` shows that topic, not the whole page.
func TestHelpTopicShowsThatTopic(t *testing.T) {
	general, _, _ := runCLI(t, "help")
	for _, c := range commands {
		t.Run(c.name, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, "help", c.name)
			if code != 0 {
				t.Fatalf("exit %d, want 0\n%s%s", code, stdout, stderr)
			}
			if stdout == general {
				t.Fatalf("help %s printed the general help", c.name)
			}
			if !strings.Contains(stdout, c.name) {
				t.Errorf("help %s does not mention %s:\n%s", c.name, c.name, stdout)
			}
		})
	}
}

// Groups also take the bare word: `conductor task help`.
func TestGroupsAcceptHelpWord(t *testing.T) {
	for _, c := range commands {
		if len(c.subs) == 0 {
			continue
		}
		stdout, stderr, code := runCLI(t, c.name, "help")
		if code != 0 || !strings.Contains(stdout, "conductor "+c.name) {
			t.Errorf("%s help: exit %d\n%s%s", c.name, code, stdout, stderr)
		}
	}
}

// Each group's page carries an example line, so a user is never left with a bare flag list.
func TestEveryTopicHasAnExample(t *testing.T) {
	for _, c := range commands {
		if c.topic != "" && !strings.Contains(c.topic, "\nExample:\n") {
			t.Errorf("%s: help topic has no Example section", c.name)
		}
	}
}

func TestTopLevelHelp(t *testing.T) {
	stdout, _, code := runCLI(t, "help")
	if code != 0 {
		t.Fatalf("help: exit %d", code)
	}
	if n := strings.Count(stdout, "\n"); n > 30 {
		t.Errorf("top-level help is %d lines; keep it short and leave the rest to `help all`", n)
	}
	for _, want := range []string{"conductor up", "conductor help all", "conductor help <command>"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("top-level help lacks %q", want)
		}
	}

	all, _, code := runCLI(t, "help", "all")
	if code != 0 {
		t.Fatalf("help all: exit %d", code)
	}
	for _, c := range commands {
		// Each command is listed once: the old page listed pause and resume twice.
		if n := strings.Count(all, "  conductor "+c.name+" ") + strings.Count(all, "  conductor "+c.name+"\n"); n != 1 {
			t.Errorf("help all lists %q %d times, want 1", c.name, n)
		}
	}

	// No arguments is a usage error, but still shows where to go.
	_, stderr, code := runCLI(t)
	if code != 2 || !strings.Contains(stderr, "conductor help all") {
		t.Errorf("bare conductor: exit %d\n%s", code, stderr)
	}
	_, stderr, code = runCLI(t, "no-such-command")
	if code != 2 || !strings.Contains(stderr, `unknown command "no-such-command"`) {
		t.Errorf("unknown command: exit %d\n%s", code, stderr)
	}
	_, stderr, code = runCLI(t, "help", "no-such-command")
	if code != 2 || !strings.Contains(stderr, "no help topic") {
		t.Errorf("unknown topic: exit %d\n%s", code, stderr)
	}
}

// A command added to the dispatcher without an index entry would have no help and no
// completion; this catches it.
func TestEveryDispatchedCommandHasHelp(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var dispatched []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runCommand" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					if lit, ok := e.(*ast.BasicLit); ok {
						name, _ := strconv.Unquote(lit.Value)
						dispatched = append(dispatched, name)
					}
				}
			}
			return true
		})
		return false
	})
	if len(dispatched) < 10 {
		t.Fatalf("found only %d commands in runCommand; did it move?", len(dispatched))
	}
	sort.Strings(dispatched)
	if got, want := dispatched, topicNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("dispatched commands and the help index differ\ndispatched: %v\nindex:      %v", got, want)
	}
}

func TestVersionCommand(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		stdout, stderr, code := runCLI(t, args...)
		if code != 0 || !strings.HasPrefix(stdout, "conductor "+stampedTestVersion) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestNotLoggedInHint(t *testing.T) {
	if hint := notLoggedInHint("http://localhost:8080"); !strings.Contains(hint, "conductor up") {
		t.Errorf("loopback hint does not suggest conductor up: %q", hint)
	}
	if hint := notLoggedInHint("https://conductor.example.com"); strings.Contains(hint, "conductor up") {
		t.Errorf("remote hint suggests starting a local server: %q", hint)
	}

	// The CLI itself, not just the helper: a fresh machine with nothing running.
	_, stderr, code := runCLI(t, "status")
	if code != 1 || !strings.Contains(stderr, "conductor up") {
		t.Errorf("status with nothing running: exit %d\n%s", code, stderr)
	}
}

func TestInitSuggestsConductorUp(t *testing.T) {
	steps := initNextSteps("/src/myrepo")
	if !strings.Contains(steps, "conductor up") || strings.Contains(steps, "docker compose") {
		t.Errorf("init's next steps:\n%s", steps)
	}
}
