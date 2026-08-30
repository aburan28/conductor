package runner

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adamburan/conductor/internal/harness"
)

// The activity log is diagnostic and lives in the attempt's worktree; these are the two
// properties the live stream depends on — the file lands where the control plane expects
// it, and concurrent appends (supervision and the run loop) interleave as whole lines.
func TestAttemptLogAppendsWholeLines(t *testing.T) {
	worktree := t.TempDir()

	log, err := openAttemptLog(worktree)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer log.close()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				log.event("tool: Edit")
			}
		}()
	}
	wg.Wait()
	log.event("attempt finished: outcome=succeeded changed_files=1")
	log.close()

	body, err := os.ReadFile(filepath.Join(worktree, ".conductor", "runtime", "attempt.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 201 {
		t.Fatalf("got %d lines, want 201", len(lines))
	}
	for _, line := range lines {
		if !strings.Contains(line, " tool: Edit") && !strings.Contains(line, "attempt finished") {
			t.Fatalf("line %q is not a whole timestamped event", line)
		}
	}
}

// A run must not fail because its log could not be written; a nil log is dropped silently.
func TestAttemptLogNilIsNoop(t *testing.T) {
	var log *attemptLog
	log.event("still runs")
	log.harnessEvent(harness.Event{Kind: harness.EventToolUse, Tool: "Edit"})
	log.close()
}
