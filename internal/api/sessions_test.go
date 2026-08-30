package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
)

// `conductor sessions save all` reads GET /v1/projects/{project}/sessions. Unlike presence,
// it must return sessions that are no longer live: a closed session is still history.
func TestListSessionsIncludesClosedOnes(t *testing.T) {
	h := newHarness(t)

	mine := h.registerSession(t, h.aliceTok, map[string]any{"model": "test-opus"})
	theirs := h.registerSession(t, h.bobTok, map[string]any{"model": "test-haiku"})
	if code, body := h.do(h.bobTok, http.MethodPost, "/v1/sessions/"+theirs.ID+"/close", nil); code != http.StatusNoContent {
		t.Fatalf("close session = %d\n%s", code, body)
	}

	code, body := h.do(h.aliceTok, http.MethodGet, h.projectPath("/sessions"), nil)
	if code != http.StatusOK {
		t.Fatalf("list sessions = %d\n%s", code, body)
	}
	var out struct {
		Sessions []privacy.SessionView `json:"sessions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byID := map[domain.ID]privacy.SessionView{}
	for _, s := range out.Sessions {
		byID[s.ID] = s
	}
	got, ok := byID[theirs.ID]
	if !ok {
		t.Fatalf("closed session %s missing from export; got %d sessions", theirs.ID, len(out.Sessions))
	}
	if got.State != domain.SessionClosed || got.ClosedAt == nil {
		t.Errorf("closed session exported as state=%s closed_at=%v", got.State, got.ClosedAt)
	}
	if got.Principal != "bob" {
		t.Errorf("principal = %q, want bob", got.Principal)
	}
	if own, ok := byID[mine.ID]; !ok {
		t.Errorf("own live session %s missing from export", mine.ID)
	} else if own.State == domain.SessionClosed {
		t.Error("live session exported as closed")
	}

	// Presence still hides the closed one: the two reads answer different questions.
	code, body = h.do(h.aliceTok, http.MethodGet, h.projectPath("/presence"), nil)
	if code != http.StatusOK {
		t.Fatalf("presence = %d\n%s", code, body)
	}
	var presence struct {
		Presence []domain.PresenceEntry `json:"presence"`
	}
	if err := json.Unmarshal(body, &presence); err != nil {
		t.Fatalf("decode presence: %v", err)
	}
	for _, p := range presence.Presence {
		if p.SessionID == theirs.ID {
			t.Error("presence still lists a closed session")
		}
	}

	// Someone outside the project gets nothing.
	if code, _ := h.do(h.outTok, http.MethodGet, h.projectPath("/sessions"), nil); code == http.StatusOK {
		t.Error("an outsider could export the project's sessions")
	}
}

// The dashboard's pause/resume: a project member records a control, the session's sidecar
// reads it from its heartbeat response, and its acknowledgement clears it once local
// reality matches. The full loop, over HTTP, exactly as the sidecar drives it.
func TestSessionControlFromDashboard(t *testing.T) {
	h := newHarness(t)

	session := h.registerSession(t, h.bobTok, map[string]any{"model": "test-opus"})
	heartbeat := func(body map[string]any) domain.Session {
		t.Helper()
		code, out := h.do(h.bobTok, http.MethodPost, "/v1/sessions/"+session.ID+"/heartbeat", body)
		if code != http.StatusOK {
			t.Fatalf("heartbeat = %d\n%s", code, out)
		}
		var s domain.Session
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatalf("decode heartbeat response: %v", err)
		}
		return s
	}

	// Any project member — not just the session's owner — may park a session.
	if code, _ := h.do(h.aliceTok, http.MethodPost, "/v1/sessions/"+session.ID+"/pause", nil); code != http.StatusNoContent {
		t.Fatalf("pause as a teammate = %d, want 204", code)
	}

	// The request is visible to the team until it is picked up.
	code, body := h.do(h.aliceTok, http.MethodGet, h.projectPath("/sessions"), nil)
	if code != http.StatusOK {
		t.Fatalf("list sessions = %d\n%s", code, body)
	}
	var list struct {
		Sessions []privacy.SessionView `json:"sessions"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, s := range list.Sessions {
		if s.ID == session.ID {
			found = true
			if s.PendingControl != domain.ControlPause {
				t.Errorf("pending_control = %q, want pause", s.PendingControl)
			}
		}
	}
	if !found {
		t.Fatal("session missing from export")
	}

	// A heartbeat that has not applied the control does not clear it: the acknowledgement
	// must match the request, not merely arrive.
	if got := heartbeat(map[string]any{"state": "working", "control_ack": "resume"}); got.PendingControl != domain.ControlPause {
		t.Errorf("pending_control after a mismatched ack = %q, want pause", got.PendingControl)
	}

	// The sidecar freezes its harness and reports paused; the matching ack clears.
	got := heartbeat(map[string]any{"state": "paused", "control_ack": "pause"})
	if got.PendingControl != "" {
		t.Errorf("pending_control after ack = %q, want cleared", got.PendingControl)
	}
	if got.State != domain.SessionPaused {
		t.Errorf("state = %s, want paused", got.State)
	}
	if got.State.Accepting() {
		t.Error("a paused session must not be offered work")
	}

	// Asking for the state the session is already in is idempotent: the next ack confirms
	// it without any action.
	if code, _ := h.do(h.aliceTok, http.MethodPost, "/v1/sessions/"+session.ID+"/pause", nil); code != http.StatusNoContent {
		t.Fatalf("repeat pause = %d, want 204", code)
	}
	if got := heartbeat(map[string]any{"state": "paused", "control_ack": "pause"}); got.PendingControl != "" {
		t.Errorf("pending_control after an already-paused ack = %q, want cleared", got.PendingControl)
	}

	// Resume: the sidecar is still paused, so a paused ack must not clear it; only the
	// report that it is running again does.
	if code, _ := h.do(h.aliceTok, http.MethodPost, "/v1/sessions/"+session.ID+"/resume", nil); code != http.StatusNoContent {
		t.Fatalf("resume = %d, want 204", code)
	}
	if got := heartbeat(map[string]any{"state": "paused", "control_ack": "pause"}); got.PendingControl != domain.ControlResume {
		t.Errorf("pending_control after a premature ack = %q, want resume", got.PendingControl)
	}
	got = heartbeat(map[string]any{"state": "working", "control_ack": "resume"})
	if got.PendingControl != "" {
		t.Errorf("pending_control after resume ack = %q, want cleared", got.PendingControl)
	}
	if got.State != domain.SessionWorking {
		t.Errorf("state = %s, want working", got.State)
	}

	// Someone outside the project cannot reach the session at all.
	if code, _ := h.do(h.outTok, http.MethodPost, "/v1/sessions/"+session.ID+"/pause", nil); code != http.StatusNotFound {
		t.Errorf("outsider pause = %d, want 404", code)
	}

	// A closed session has no sidecar to carry anything out.
	if code, _ := h.do(h.bobTok, http.MethodPost, "/v1/sessions/"+session.ID+"/close", nil); code != http.StatusNoContent {
		t.Fatalf("close = %d, want 204", code)
	}
	if code, _ := h.do(h.aliceTok, http.MethodPost, "/v1/sessions/"+session.ID+"/pause", nil); code != http.StatusNotFound {
		t.Errorf("pause a closed session = %d, want 404", code)
	}
}
