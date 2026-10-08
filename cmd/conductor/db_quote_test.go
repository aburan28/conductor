package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/pgarchive"
)

// confUnquote reads a postgresql.conf string value the way the server does: two quotes make one,
// and a backslash escapes the next character.
func confUnquote(v string) string {
	body := strings.TrimSuffix(strings.TrimPrefix(v, "'"), "'")
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\'':
			b.WriteByte('\'')
			i++ // the second quote of the pair
		case c == '\\' && i+1 < len(body):
			i++
			b.WriteByte(body[i])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// The archive_command written to postgresql.auto.conf must run, after the server reads it back, as
// the shell command that names the binary. The binary path here has a backslash and a quote.
func TestArchiveCommandPathSurvivesConfAndShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	exe := `/opt/con\ductor/it's here/conductor`
	command := shellQuote(exe) + " db archive-wal %p %f"
	if got := confUnquote(pgarchive.ConfQuote(command)); got != command {
		t.Fatalf("the server reads the archive_command back as %q; want %q", got, command)
	}
	// What the shell runs: the quoted binary path must come out unchanged.
	out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(exe)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != exe {
		t.Fatalf("the shell reads the path as %q; want %q", out, exe)
	}
}
