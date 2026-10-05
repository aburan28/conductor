package harness

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func adaptFixture(t *testing.T, name string) []Event {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	adapt := newCodexAdapter()
	var out []Event
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		ev, ok := adapt(scanner.Bytes())
		if !ok {
			t.Fatalf("line not understood: %s", scanner.Text())
		}
		eventContainsSecret(t, ev)
		out = append(out, ev)
	}
	return out
}

// A completed `codex exec --json` run: item.completed is not the end of the run, tools are
// counted once each, and usage — a running total — is metered by difference.
func TestCodexAdapterReadsAnExecTranscript(t *testing.T) {
	events := adaptFixture(t, "codex_exec.jsonl")

	var tools []string
	var turns int
	var in, out int64
	for _, ev := range events {
		switch ev.Kind {
		case EventFinished, EventError:
			t.Errorf("a successful run produced %s (%q)", ev.Kind, ev.Note)
		case EventToolUse:
			tools = append(tools, ev.Tool)
		case EventTurn:
			turns++
		}
		in += ev.TokensIn
		out += ev.TokensOut
	}
	want := "command_execution,file_change,mcp:conductor/coord_report_progress,web_search"
	if got := strings.Join(tools, ","); got != want {
		t.Errorf("tools = %s, want %s", got, want)
	}
	if turns != 2 {
		t.Errorf("turns = %d, want 2", turns)
	}
	// Totals of the last turn.completed: cached input is part of input, not extra.
	if in != 30000 || out != 200 {
		t.Errorf("tokens = (%d, %d), want (30000, 200)", in, out)
	}
	var sawExit bool
	for _, ev := range events {
		if strings.Contains(ev.Note, "exit code 1") {
			sawExit = true
		}
	}
	if !sawExit {
		t.Error("a failed command's exit code was not noted")
	}
}

func TestCodexAdapterReportsFailure(t *testing.T) {
	events := adaptFixture(t, "codex_exec_failed.jsonl")
	var errs []string
	for _, ev := range events {
		if ev.Kind == EventError {
			errs = append(errs, ev.Note)
		}
	}
	if strings.Join(errs, ",") != "error,turn.failed" {
		t.Errorf("errors = %v, want the stream error and the failed turn", errs)
	}
}

// Each run gets its own adapter: one run's running total must not shift the next run's.
func TestCodexDriverAdaptsPerRun(t *testing.T) {
	d := NewCodexDriver(HarnessConfig{})
	if d.NewAdapt == nil {
		t.Fatal("the Codex driver has no per-run adapter")
	}
	line := []byte(`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10}}`)
	first, _ := d.NewAdapt()(line)
	second, _ := d.NewAdapt()(line)
	if first.TokensIn != 100 || second.TokensIn != 100 {
		t.Errorf("per-run usage = %d then %d, want 100 both times", first.TokensIn, second.TokensIn)
	}
}

func TestCodexAdapterRejectsGarbage(t *testing.T) {
	adapt := newCodexAdapter()
	for _, line := range []string{"", "not json", "{", `{"type":"item.completed"}`, `{"type":"something.new"}`} {
		if _, ok := adapt([]byte(line)); ok {
			t.Errorf("accepted %q", line)
		}
	}
}
