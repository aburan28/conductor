package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// The live task log stream (DESIGN.md §26.3's sanitized summaries, not harness output):
// existing content first, then appends as they are written, then a final frame once the
// attempt is terminal. Without an attempt, the stream says so instead of hanging silently.

// streamTaskLogs opens the SSE endpoint and returns its decoded frames.
func streamTaskLogs(t *testing.T, h *harness, token, taskID string) (<-chan map[string]any, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.server.URL+"/v1/tasks/"+taskID+"/logs", nil)
	if err != nil {
		cancel()
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := h.server.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		cancel()
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	frames := make(chan map[string]any, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var frame map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) == nil {
				frames <- frame
			}
		}
	}()
	return frames, func() { cancel(); <-done }
}

func nextFrame(t *testing.T, frames <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case f := <-frames:
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("no frame arrived on the log stream within the timeout")
		return nil
	}
}

func TestTaskLogsStreamAndEnd(t *testing.T) {
	h := newHarness(t)

	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"), map[string]any{
		"summary": "log streaming", "title": "Stream my log",
	})
	if code != http.StatusOK {
		t.Fatalf("start = %d\n%s", code, body)
	}
	var started coord.StartWorkResult
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Give the attempt a worktree with a log in it, the way the runner does, and walk the
	// attempt to running — the state at which its log is live.
	worktree := t.TempDir()
	runtimeDir := filepath.Join(worktree, ".conductor", "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(runtimeDir, "attempt.log")
	if err := os.WriteFile(logPath, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, state := range []domain.AttemptState{
		domain.AttemptPreparingWorkspace,
		domain.AttemptStartingHarness,
		domain.AttemptRunning,
	} {
		progress := db.AttemptProgress{State: state}
		if state == domain.AttemptRunning {
			progress.WorktreePath = worktree
		}
		if _, err := h.store.UpdateAttempt(context.Background(), started.AttemptID, progress); err != nil {
			t.Fatalf("advance to %s: %v", state, err)
		}
	}

	frames, stop := streamTaskLogs(t, h, h.aliceTok, started.TaskID)
	defer stop()

	// Existing content first.
	var text string
	for text == "" || !strings.Contains(text, "line two") {
		f := nextFrame(t, frames)
		if f["type"] != "log" {
			t.Fatalf("first frame = %v, want log content", f)
		}
		text += f["text"].(string)
	}

	// Then appends, as they are written.
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line three\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for {
		frame := nextFrame(t, frames)
		if frame["type"] != "log" {
			t.Fatalf("frame after append = %v, want log", frame)
		}
		if strings.Contains(frame["text"].(string), "line three") {
			break
		}
	}

	// A terminal attempt ends the stream with a final frame.
	if _, err := h.store.UpdateAttempt(context.Background(), started.AttemptID,
		db.AttemptProgress{State: domain.AttemptSucceeded}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}
	for {
		frame := nextFrame(t, frames)
		if frame["type"] == "end" {
			if frame["state"] != string(domain.AttemptSucceeded) {
				t.Errorf("end state = %v, want succeeded", frame["state"])
			}
			break
		}
	}

	// A member who cannot see the task gets nothing. Private visibility is not needed to
	// prove the membership check: an outsider is refused at the project boundary.
	if code, _ := h.do(h.outTok, http.MethodGet, "/v1/tasks/"+started.TaskID+"/logs", nil); code != http.StatusNotFound {
		t.Errorf("outsider logs = %d, want 404", code)
	}
}

func TestTaskLogsWaitWhenNoAttempt(t *testing.T) {
	h := newHarness(t)

	task := h.createTask(t, h.aliceTok, "Never claimed")

	frames, stop := streamTaskLogs(t, h, h.aliceTok, task.ID)
	defer stop()

	f := nextFrame(t, frames)
	if f["type"] != "waiting" {
		t.Fatalf("first frame = %v, want waiting", f)
	}
	if reason, _ := f["reason"].(string); !strings.Contains(reason, "no attempt") {
		t.Errorf("waiting reason = %q, want no attempt yet", reason)
	}
}
