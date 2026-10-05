package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/adamburan/conductor/internal/version"
)

// `conductor doctor` compares its own version with the server's, so health must report it.
func TestHealthReportsServerVersion(t *testing.T) {
	h := newHarness(t)
	code, body := h.do("", http.MethodGet, "/v1/health", nil)
	if code != http.StatusOK {
		t.Fatalf("health = %d, want 200\n%s", code, body)
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if out.Version == "" || out.Version != version.Version() {
		t.Errorf("health version = %q, want %q", out.Version, version.Version())
	}
}
