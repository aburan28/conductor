package checkpoint

import (
	"errors"
	"testing"
)

func TestCodexThreadMissingOnlyRetriesAbsentThread(t *testing.T) {
	for _, c := range []struct {
		message string
		missing bool
	}{
		{"Codex thread/resume: thread not found (code -32602)", true},
		{"Codex thread/resume: no such thread (code -32602)", true},
		{"Codex thread/resume: authentication failed (code -32603)", false},
		{"Codex thread/resume: network timeout (code -32603)", false},
	} {
		if got := codexThreadMissing(errors.New(c.message)); got != c.missing {
			t.Errorf("%q: retry = %v, want %v", c.message, got, c.missing)
		}
	}
}
