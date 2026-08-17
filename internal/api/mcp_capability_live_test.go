package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/mcp"
)

// The escalation path an agent actually walks, driven through the MCP gateway against a real
// control plane: a coordinator running at medium effort discovers the work needs more, asks
// what is available, and hands it to a session that can do it.

// driveSession is drive() for a gateway that knows which session it is running inside.
func driveSession(t *testing.T, endpoint, token, project string, session domain.ID, requests ...string) []map[string]any {
	t.Helper()
	server := mcp.New(mcp.Options{
		Endpoint: endpoint, Token: token, Project: project, SessionID: session,
	})

	var out strings.Builder
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	if err := server.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var decoded []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, line)
		}
		decoded = append(decoded, m)
	}
	return decoded
}

func TestMCPCapabilitiesReportsWhatIsLive(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)
	h.registerSession(t, h.bobTok, map[string]any{"model": "test-haiku", "reasoning_effort": "low"})

	responses := drive(t, h.server.URL, h.aliceTok, h.project.ID,
		call("coord_capabilities", map[string]any{"tier": "T4", "effort": "xhigh"}))

	text, isErr := toolText(t, responses[0])
	if isErr {
		t.Fatalf("coord_capabilities errored: %s", text)
	}
	if !strings.Contains(text, "test-haiku") {
		t.Errorf("inventory does not name the live model:\n%s", text)
	}
	if !strings.Contains(text, `"servable": false`) {
		t.Errorf("a T4/xhigh requirement should be reported unservable here:\n%s", text)
	}
}

// The headline case: a coordinator hands work up to a session with a more capable model, and
// the receiving session finds it through the tool it already calls.
func TestMCPDelegateReachesTheMoreCapableSession(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)

	// Bob is the coordinator, on the cheap model. Alice has the frontier session.
	coordinator := h.registerSession(t, h.bobTok, map[string]any{
		"model": "test-haiku", "reasoning_effort": "medium",
	})
	frontier := h.registerSession(t, h.aliceTok, map[string]any{
		"model": "test-opus", "reasoning_effort": "high", "max_reasoning_effort": "xhigh",
	})

	responses := driveSession(t, h.server.URL, h.bobTok, h.project.ID, coordinator.ID,
		call("coord_start_work", map[string]any{
			"summary": "redesign the routing policy",
			"scopes":  []map[string]any{{"resource": "dir:internal/router", "mode": "write_exclusive"}},
		}),
		call("coord_delegate", map[string]any{
			"tier": "T4", "effort": "xhigh",
			"next_action":    "design the tier ladder, then implement it",
			"completed":      []string{"mapped the current policy"},
			"open_questions": []string{"should budget pressure ever cross a floor?"},
		}))

	if text, isErr := toolText(t, responses[0]); isErr {
		t.Fatalf("start_work errored: %s", text)
	}
	text, isErr := toolText(t, responses[1])
	if isErr {
		t.Fatalf("coord_delegate errored: %s", text)
	}
	if !strings.Contains(text, frontier.ID) {
		t.Errorf("delegation did not reach the frontier session:\n%s", text)
	}
	if !strings.Contains(text, "alice") {
		t.Errorf("the delegating agent was not told who took the work:\n%s", text)
	}

	// The receiving session sees the offer through the tool it already calls for work.
	responses = driveSession(t, h.server.URL, h.aliceTok, h.project.ID, frontier.ID,
		call("coord_get_work", nil))
	text, isErr = toolText(t, responses[0])
	if isErr {
		t.Fatalf("coord_get_work errored: %s", text)
	}
	if !strings.Contains(text, "offered to this session") {
		t.Errorf("the offer is not visible to the session it was made to:\n%s", text)
	}
	if !strings.Contains(text, "xhigh") {
		t.Errorf("the offer does not say what floor it was placed against:\n%s", text)
	}

	// The handoff bundle went with it, and carries no transcript.
	code, body := h.do(h.aliceTok, http.MethodGet,
		"/v1/tasks/"+strings.TrimSpace(taskRefFrom(t, text))+"/handoff?project="+h.project.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("handoff = %d\n%s", code, body)
	}
	if !strings.Contains(string(body), "design the tier ladder") {
		t.Errorf("the continuation bundle did not survive delegation:\n%s", body)
	}
}

// A delegation nobody can serve still writes the bundle rather than losing the work.
func TestMCPDelegateWithoutACapableSessionStillPreservesTheHandoff(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)
	coordinator := h.registerSession(t, h.bobTok, map[string]any{
		"model": "test-haiku", "reasoning_effort": "medium",
	})

	responses := driveSession(t, h.server.URL, h.bobTok, h.project.ID, coordinator.ID,
		call("coord_start_work", map[string]any{
			"summary": "rewrite the crypto layer",
			"scopes":  []map[string]any{{"resource": "dir:internal/privacy", "mode": "write_exclusive"}},
		}),
		call("coord_delegate", map[string]any{
			"tier": "T4", "effort": "xhigh", "next_action": "audit the key handling",
		}))

	text, isErr := toolText(t, responses[1])
	if isErr {
		t.Fatalf("coord_delegate errored: %s", text)
	}
	if !strings.Contains(text, "unplaced") {
		t.Errorf("the agent was not told the work could not be placed:\n%s", text)
	}
	if !strings.Contains(text, "handoff") {
		t.Errorf("the bundle should still be written when placement fails:\n%s", text)
	}
}

// taskRefFrom pulls the first `T-n` out of an offer listing.
func taskRefFrom(t *testing.T, text string) string {
	t.Helper()
	for _, field := range strings.Fields(text) {
		if strings.HasPrefix(field, "T-") {
			return field
		}
	}
	t.Fatalf("no task ref in:\n%s", text)
	return ""
}
