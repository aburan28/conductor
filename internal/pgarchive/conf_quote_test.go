package pgarchive

import (
	"strings"
	"testing"
)

// confUnquote reads a postgresql.conf string value the way the server does (checked against
// PostgreSQL 16): two quotes make one quote, and a backslash escapes the character after it.
func confUnquote(t *testing.T, v string) string {
	t.Helper()
	if len(v) < 2 || v[0] != '\'' || v[len(v)-1] != '\'' {
		t.Fatalf("%s is not a quoted string", v)
	}
	body := v[1 : len(v)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\'':
			if i+1 >= len(body) || body[i+1] != '\'' {
				t.Fatalf("%s has a quote that is not doubled", v)
			}
			b.WriteByte('\'')
			i++
		case c == '\\':
			if i+1 >= len(body) {
				t.Fatalf("%s ends in a backslash", v)
			}
			i++
			b.WriteByte(body[i])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// A backslash in a value must survive the round trip. shellQuote writes a quote as the three
// characters '\” , so an unescaped backslash turned that sequence into a syntax error or lost
// the quote in archive_command and restore_command.
func TestConfQuoteRoundTripsBackslashesAndQuotes(t *testing.T) {
	for _, s := range []string{
		`plain`,
		`C:\data\it's`,
		`'\''`,
		`a\'b`,
		`trailing\`,
		`two\\backslashes`,
		`/var/lib/con\ductor/it's here/conductor`,
	} {
		if got := confUnquote(t, ConfQuote(s)); got != s {
			t.Errorf("ConfQuote(%q) reads back as %q", s, got)
		}
	}
}
