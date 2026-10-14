package mcp

import (
	"net/http"
	"strings"
	"testing"
)

// Over HTTP the gateway lives in the control plane: coord_quota reads the caller's own
// readings from the API, makes no write, and offers no resume command (it cannot see the
// machine the session runs on).
func TestQuotaToolIsReadOnlyAndOwnerScoped(t *testing.T) {
	plane := newStubPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/quota": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"snapshots":[{"snapshot":null,"harness":"codex","account":"default","machine":"box",
				"window":"5h","window_minutes":300,"used_percent":91,"source":"codex-rollout","source_kind":"local_file",
				"observed_at":"2099-01-01T00:00:00Z","resets_at":"2099-01-01T05:00:00Z","level":"warning"}],"logins":[]}`))
		},
	})
	s := newTestServer(plane.URL)
	text, isErr := call(t, s, "coord_quota", map[string]any{})
	if isErr {
		t.Fatalf("coord_quota failed: %s", text)
	}
	if !strings.Contains(text, `"harness": "codex"`) || !strings.Contains(text, `"level": "warning"`) {
		t.Errorf("result does not show the login: %s", text)
	}
	if strings.Contains(text, "continue_with") {
		t.Errorf("the HTTP gateway cannot know a local resume command: %s", text)
	}
	for _, r := range plane.requests {
		if r.Method != http.MethodGet || r.Path != "/v1/quota" {
			t.Errorf("coord_quota made %s %s; it must only read the caller's own quota", r.Method, r.Path)
		}
	}
}
