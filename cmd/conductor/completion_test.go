package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func completionScript(t *testing.T, shell string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := writeCompletion(&buf, shell); err != nil {
		t.Fatalf("%s: %v", shell, err)
	}
	return buf.String()
}

func TestCompletionScriptsNameEveryCommand(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		script := completionScript(t, shell)
		for _, c := range commands {
			if !strings.Contains(script, c.name) {
				t.Errorf("%s completion lacks %q", shell, c.name)
			}
		}
		if !strings.Contains(script, "handoff") {
			t.Errorf("%s completion lacks task subcommands", shell)
		}
	}
	if err := writeCompletion(&bytes.Buffer{}, "powershell"); err == nil {
		t.Error("an unsupported shell should be an error")
	}
}

// Run the bash completion function the way bash does on <Tab>, for a command and a subcommand.
func TestBashCompletionCompletes(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	path := filepath.Join(t.TempDir(), "conductor.bash")
	if err := os.WriteFile(path, []byte(completionScript(t, "bash")), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		words string
		cword int
		want  string
	}{
		{"conductor ta", 1, "task"},
		{"conductor task han", 2, "handoff"},
		{"conductor help che", 2, "checkpoint"},
		{"conductor github issues ena", 3, "enable"},
	}
	for _, c := range cases {
		script := "source " + path + "; COMP_WORDS=(" + c.words + "); COMP_CWORD=" +
			string(rune('0'+c.cword)) + "; _conductor; printf '%s\\n' \"${COMPREPLY[@]}\""
		out, err := exec.Command(bash, "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%q: %v\n%s", c.words, err, out)
		}
		if !strings.Contains(string(out), c.want) {
			t.Errorf("%q completed to %q, want %q among them", c.words, out, c.want)
		}
	}
}

// Syntax-check the other shells' scripts where those shells are installed (CI's macOS runner
// has zsh).
func TestCompletionScriptsParse(t *testing.T) {
	for shell, args := range map[string][]string{"zsh": {"-n"}, "fish": {"--no-execute"}} {
		bin, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		path := filepath.Join(t.TempDir(), "completion."+shell)
		if err := os.WriteFile(path, []byte(completionScript(t, shell)), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(bin, append(args, path)...).CombinedOutput(); err != nil {
			t.Errorf("%s rejects its completion script: %v\n%s", shell, err, out)
		}
	}
}
