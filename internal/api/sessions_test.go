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
