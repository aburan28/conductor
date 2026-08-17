package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// The SSE stream, presence, and attempt provenance were all previously unasserted. The
// dashboard reads exactly these three, so a regression in any of them would have shown up as
// a blank page rather than a failing test.

func TestEventStreamDeliversEvents(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.server.URL+h.projectPath("/events/stream"), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.aliceTok)

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Produce an event after the stream is open, so this proves live delivery rather than
	// replay of history.
	go func() {
		time.Sleep(300 * time.Millisecond)
		h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"),
			map[string]any{"summary": "stream me", "title": "Stream me"})
	}()

	type frame struct {
		event string
		data  string
	}
	frames := make(chan frame, 8)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		var current frame
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				current.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				current.data = strings.TrimPrefix(line, "data: ")
			case line == "" && current.data != "":
				select {
				case frames <- current:
				default:
				}
				current = frame{}
			}
		}
	}()

	select {
	case f := <-frames:
		if f.event == "" {
			t.Error("frame carried no event type")
		}
		var event domain.Event
		if err := json.Unmarshal([]byte(f.data), &event); err != nil {
			t.Fatalf("event payload is not JSON: %v\n%s", err, f.data)
		}
		if event.ProjectID != h.project.ID {
			t.Errorf("event project = %s, want %s", event.ProjectID, h.project.ID)
		}
		if event.SequenceNumber == 0 {
			t.Error("event has no sequence number; consumers cannot detect a gap")
		}
		// The stream is the widest-reaching surface in the system.
		for _, banned := range []string{"prompt", "transcript", "reasoning"} {
			if _, ok := event.Payload[banned]; ok {
				t.Errorf("streamed event carries %q", banned)
			}
		}
	case <-ctx.Done():
		t.Fatal("no event arrived on the stream within the timeout")
	}
}

func TestPresenceReportsSessionAndBranch(t *testing.T) {
	h := newHarness(t)

	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/sessions"), map[string]any{
		"harness": "claude-code", "branch": "feature/x", "base_sha": "abc123",
		"machine_id": "laptop",
	})
	if code != http.StatusCreated {
		t.Fatalf("register session = %d\n%s", code, body)
	}
	var session domain.Session
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// A teammate's view of that session.
	code, body = h.do(h.bobTok, http.MethodGet, h.projectPath("/presence"), nil)
	if code != http.StatusOK {
		t.Fatalf("presence = %d\n%s", code, body)
	}
	var out struct {
		Presence []domain.PresenceEntry `json:"presence"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Presence) == 0 {
		t.Fatal("presence is empty right after registering a session")
	}

	var found *domain.PresenceEntry
	for i := range out.Presence {
		if out.Presence[i].SessionID == session.ID {
			found = &out.Presence[i]
		}
	}
	if found == nil {
		t.Fatal("the registered session does not appear in presence")
	}

	// DESIGN.md §31.7: the view must show who, what harness, which branch, and liveness.
	if found.Principal != "alice" || found.Harness != "claude-code" {
		t.Errorf("presence identity wrong: %+v", *found)
	}
	if found.Branch != "feature/x" {
		t.Errorf("branch = %q, want feature/x", found.Branch)
	}
	if found.LastHeartbeat.IsZero() {
		t.Error("presence carries no heartbeat, so staleness is invisible")
	}
	if found.State == "" {
		t.Error("presence carries no state")
	}
}

// DESIGN.md §31.10 and §31.12: every attempt records the rules and routing that produced it.
// Nothing could read these back until the attempts endpoint existed, so nothing verified them.
func TestAttemptRecordsProvenance(t *testing.T) {
	h := newHarness(t)

	// Give the project a workflow so there is a hash to record.
	if _, err := h.store.PutWorkflow(context.Background(), h.project.ID,
		"# Workflow\n\nrules", nil); err != nil {
		t.Fatalf("PutWorkflow: %v", err)
	}

	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"), map[string]any{
		"summary": "provenance check", "title": "Provenance",
		"harness": "claude", "model_alias": "worker.fast",
	})
	if code != http.StatusOK {
		t.Fatalf("start = %d\n%s", code, body)
	}
	var started coord.StartWorkResult
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatalf("decode: %v", err)
	}

	code, body = h.do(h.aliceTok, http.MethodGet, "/v1/tasks/"+started.TaskID+"/attempts", nil)
	if code != http.StatusOK {
		t.Fatalf("attempts = %d\n%s", code, body)
	}
	var out struct {
		Attempts []struct {
			AttemptNumber    int    `json:"attempt_number"`
			Harness          string `json:"harness"`
			ModelAlias       string `json:"model_alias"`
			Effort           string `json:"reasoning_effort"`
			WorkflowSHA      string `json:"workflow_sha"`
			ProjectConfigSHA string `json:"project_config_sha"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Attempts) != 1 {
		t.Fatalf("got %d attempts, want 1", len(out.Attempts))
	}

	a := out.Attempts[0]
	if a.AttemptNumber != 1 {
		t.Errorf("attempt number = %d, want 1", a.AttemptNumber)
	}
	if a.Harness != "claude" {
		t.Errorf("harness = %q, want claude", a.Harness)
	}
	if a.ModelAlias != "worker.fast" {
		t.Errorf("model alias = %q, want worker.fast", a.ModelAlias)
	}
	if a.Effort == "" {
		t.Error("reasoning effort was not recorded (DESIGN.md §31.12)")
	}
	if a.WorkflowSHA == "" {
		t.Error("workflow hash was not recorded (DESIGN.md §31.10)")
	}
	if a.ProjectConfigSHA == "" {
		t.Error("project config hash was not recorded (DESIGN.md §31.10)")
	}
}

// Usage and cost belong to the sponsor; execution identity is publishable. A teammate should
// be able to see which model ran without seeing what it cost.
func TestAttemptUsageIsSponsorOnly(t *testing.T) {
	h := newHarness(t)

	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"),
		map[string]any{"summary": "cost privacy", "title": "Cost", "harness": "claude"})
	if code != http.StatusOK {
		t.Fatalf("start = %d\n%s", code, body)
	}
	var started coord.StartWorkResult
	_ = json.Unmarshal(body, &started)

	// An attempt reaches `running` through the lifecycle, not directly: the state machine
	// refuses queued -> running, which is what keeps a runner from skipping workspace
	// preparation.
	for _, state := range []domain.AttemptState{
		domain.AttemptPreparingWorkspace,
		domain.AttemptStartingHarness,
		domain.AttemptRunning,
	} {
		progress := db.AttemptProgress{State: state}
		if state == domain.AttemptRunning {
			progress.TokensIn, progress.TokensOut, progress.CostUSD = 1000, 500, 4.25
		}
		if _, err := h.store.UpdateAttempt(context.Background(), started.AttemptID, progress); err != nil {
			t.Fatalf("advance to %s: %v", state, err)
		}
	}

	mine := attemptsFor(t, h, h.aliceTok, started.TaskID)
	if mine[0].CostUSD == 0 || mine[0].TokensIn == 0 {
		t.Errorf("the sponsor cannot see their own usage: %+v", mine[0])
	}

	theirs := attemptsFor(t, h, h.bobTok, started.TaskID)
	if theirs[0].CostUSD != 0 || theirs[0].TokensIn != 0 {
		t.Errorf("another member can see the sponsor's spend: %+v", theirs[0])
	}
	// ...but execution identity is still visible, which is the point of publishing it.
	if theirs[0].Harness == "" {
		t.Error("harness identity was hidden despite the publication policy")
	}
}

type attemptUsage struct {
	Harness  string  `json:"harness"`
	TokensIn int64   `json:"tokens_in"`
	CostUSD  float64 `json:"cost_usd"`
}

func attemptsFor(t *testing.T, h *harness, token, taskID string) []attemptUsage {
	t.Helper()
	code, body := h.do(token, http.MethodGet, "/v1/tasks/"+taskID+"/attempts", nil)
	if code != http.StatusOK {
		t.Fatalf("attempts = %d\n%s", code, body)
	}
	var out struct {
		Attempts []attemptUsage `json:"attempts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Attempts) == 0 {
		t.Fatal("no attempts returned")
	}
	return out.Attempts
}
