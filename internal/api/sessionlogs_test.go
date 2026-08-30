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
)

// The live session log stream: the wrapped harness's combined output, teed by the sidecar
// into the localstate sessions directory — existing content first, then appends, then a
// done frame once the session closes. A session wrapped on another machine (or a terminal
// wrap that did not opt into capture) streams waiting frames instead of hanging silently.

// streamSessionLogs points the state dir at a temp directory, registers nothing, and
// opens the SSE endpoint; callers write the log file under that root.
func streamSessionLogs(t *testing.T, h *harness, token, sessionID string) (<-chan map[string]any, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.server.URL+"/v1/sessions/"+sessionID+"/logs", nil)
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
	frames := make(chan map[string]any, 32)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) > 6 && string(line[:6]) == "data: " {
				var m map[string]any
				if json.Unmarshal(line[6:], &m) == nil {
					frames <- m
				}
			}
		}
	}()
	return frames, func() { resp.Body.Close(); cancel() }
}

func sessionLogFile(t *testing.T, sessionID string) string {
	t.Helper()
	dir := os.Getenv("CONDUCTOR_STATE_DIR")
	if dir == "" {
		t.Fatal("CONDUCTOR_STATE_DIR not set for the test")
	}
	path := filepath.Join(dir, "sessions", sessionID, "harness.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return path
}

func TestSessionLogsStreamsBacklogThenAppendsWithDoneOnClose(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", state)

	h := newHarness(t)
	sess := h.registerSession(t, h.aliceTok, nil)
	log := sessionLogFile(t, sess.ID)
	if err := os.WriteFile(log, []byte("first line\nsecond line\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	frames, stop := streamSessionLogs(t, h, h.aliceTok, sess.ID)
	defer stop()

	var text strings.Builder
	done := false
	for frame := range frames {
		switch frame["type"] {
		case "log":
			text.WriteString(frame["text"].(string))
		case "done":
			done = true
		}
		// Close the session after the backlog arrives so the done frame follows.
		if strings.Contains(text.String(), "second line") {
			f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o644)
			if err == nil {
				_, _ = f.WriteString("appended after open\n")
				_ = f.Close()
			}
			if code, body := h.do(h.aliceTok, http.MethodPost, "/v1/sessions/"+sess.ID+"/close", nil); code != http.StatusNoContent {
				t.Fatalf("close session = %d\n%s", code, body)
			}
			text.Reset()
		}
		if done {
			return
		}
	}
	t.Fatalf("stream ended without a done frame; got %q", text.String())
}

func TestSessionLogsWaitsWhenNoLogOnThisHost(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", state)

	h := newHarness(t)
	sess := h.registerSession(t, h.aliceTok, nil)

	frames, stop := streamSessionLogs(t, h, h.aliceTok, sess.ID)
	defer stop()

	select {
	case frame := <-frames:
		if frame["type"] != "waiting" {
			t.Fatalf("first frame = %v, want waiting", frame)
		}
		if reason, _ := frame["reason"].(string); reason != "no harness log yet" {
			t.Fatalf("reason = %q, want no harness log yet", reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no waiting frame within 10s")
	}
}

func TestSessionLogsUnknownSessionIsNotFound(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", state)

	h := newHarness(t)
	code, _ := h.do(h.aliceTok, http.MethodGet, "/v1/sessions/00000000-0000-0000-0000-000000000000/logs", nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}
