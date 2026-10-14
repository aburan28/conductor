package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/domain"
)

// POST /v1/local/resume-all is the dashboard's reboot-recovery button. The
// handler's whole job is to call the machine-local hook and pass its summary
// through, or say plainly when the control plane does not offer one.

func TestLocalResumeAllPassesSummaryThrough(t *testing.T) {
	summary := map[string]any{"resumed": 2, "running": 1, "failed": 0}
	called := false
	s := &Server{localResume: func() (any, error) {
		called = true
		return summary, nil
	}}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/local/resume-all", nil)
	s.localResumeAll(w, r, domain.Principal{Handle: "alice"})

	if !called {
		t.Fatal("hook was not called")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"resumed":2`) {
		t.Errorf("body = %s, want the hook's summary", w.Body.String())
	}
}

func TestLocalResumeAllWithoutHook(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/local/resume-all", nil)
	s.localResumeAll(w, r, domain.Principal{Handle: "alice"})
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 when the control plane offers no local resume", w.Code)
	}
}
