package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/mcp"
)

// The MCP gateway's own tests drive it against a stub. These drive it against a real control
// plane, which is the only way to catch a mismatch between what the gateway sends and what the
// API actually accepts — the class of bug a stub agrees with by construction.

// drive runs a sequence of JSON-RPC requests through a gateway pointed at the live test
// server, and returns the decoded responses.
func drive(t *testing.T, endpoint, token, project string, requests ...string) []map[string]any {
	t.Helper()
	server := mcp.New(mcp.Options{Endpoint: endpoint, Token: token, Project: project})

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

// toolText extracts the agent-visible text from a tools/call response.
func toolText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result: %+v", resp)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("result has no content: %+v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	isErr, _ := result["isError"].(bool)
	return text, isErr
}

func call(name string, args map[string]any) string {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	return string(body)
}

// An agent's first useful action: ask whether it may start, against a real server.
func TestMCPCheckConflictsAgainstLiveServer(t *testing.T) {
	h := newHarness(t)

	// Alice takes the territory through the normal API.
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"), map[string]any{
		"summary": "rework the router",
		"scopes":  []map[string]any{{"resource": "dir:internal/router", "mode": "write_exclusive"}},
	})
	if code != http.StatusOK {
		t.Fatalf("alice start = %d\n%s", code, body)
	}

	// Bob's agent asks through MCP.
	responses := drive(t, h.server.URL, h.bobTok, h.project.ID,
		call("conductor_check_conflicts", map[string]any{
			"summary": "also touch the router",
			"scopes":  []map[string]any{{"resource": "path:internal/router/policy.go", "mode": "write_exclusive"}},
		}))

	text, isErr := toolText(t, responses[0])
	if isErr {
		t.Fatalf("check_conflicts errored: %s", text)
	}
	if !strings.Contains(text, "block_conflict") {
		t.Errorf("agent was not told it is blocked:\n%s", text)
	}
	if !strings.Contains(text, "alice") {
		t.Errorf("agent was not told who holds the resource:\n%s", text)
	}
}

// The full agent lifecycle over MCP: claim, read the brief, report, and finish — every call
// hitting the real API and the real fencing checks.
func TestMCPWorkLifecycleAgainstLiveServer(t *testing.T) {
	h := newHarness(t)

	responses := drive(t, h.server.URL, h.aliceTok, h.project.ID,
		call("coord_start_work", map[string]any{
			"summary": "implement the widget",
			"title":   "Widget",
			"scopes":  []map[string]any{{"resource": "path:internal/widget.go", "mode": "write_exclusive"}},
		}),
		call("coord_get_work", map[string]any{}),
		call("coord_report_progress", map[string]any{"phase": "implementing", "summary": "wrote it"}),
		call("coord_finish_work", map[string]any{"outcome": "succeeded"}),
	)
	if len(responses) != 4 {
		t.Fatalf("got %d responses, want 4", len(responses))
	}

	started, isErr := toolText(t, responses[0])
	if isErr {
		t.Fatalf("start_work errored: %s", started)
	}
	if !strings.Contains(started, `"outcome": "allow"`) {
		t.Fatalf("start_work was not allowed:\n%s", started)
	}

	brief, isErr := toolText(t, responses[1])
	if isErr {
		t.Fatalf("get_work errored: %s", brief)
	}
	// The brief is the task card, and it must state plainly what it does not contain.
	if !strings.Contains(brief, "Widget") {
		t.Errorf("brief does not describe the task:\n%s", brief)
	}
	if !strings.Contains(brief, "Privacy note") {
		t.Error("brief is missing the privacy note")
	}

	if text, isErr := toolText(t, responses[2]); isErr {
		t.Errorf("report_progress errored: %s", text)
	}

	finished, isErr := toolText(t, responses[3])
	if isErr {
		t.Fatalf("finish_work errored: %s", finished)
	}
	if !strings.Contains(finished, "verifying") {
		t.Errorf("finishing did not move the task to verifying:\n%s", finished)
	}

	// The ledger agrees with what the agent was told.
	code, body := h.do(h.aliceTok, http.MethodGet, h.projectPath("/tasks")+"?open=true", nil)
	if code != http.StatusOK {
		t.Fatalf("tasks = %d", code)
	}
	if !strings.Contains(string(body), "verifying") {
		t.Errorf("task is not verifying after an MCP finish:\n%s", body)
	}
}

// A second agent claiming contested territory must be refused by the real server, and must
// receive that refusal as usable guidance rather than a protocol error.
func TestMCPStartWorkIsBlockedByRealConflict(t *testing.T) {
	h := newHarness(t)

	code, _ := h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"), map[string]any{
		"summary": "own the billing module",
		"scopes":  []map[string]any{{"resource": "dir:internal/billing", "mode": "write_exclusive"}},
	})
	if code != http.StatusOK {
		t.Fatalf("alice start = %d", code)
	}

	responses := drive(t, h.server.URL, h.bobTok, h.project.ID,
		call("coord_start_work", map[string]any{
			"summary": "also own billing",
			"scopes":  []map[string]any{{"resource": "dir:internal/billing", "mode": "write_exclusive"}},
		}))

	text, isErr := toolText(t, responses[0])
	if isErr {
		t.Errorf("a blocked claim should arrive as a result, not an error: %s", text)
	}
	if !strings.Contains(text, "block") {
		t.Errorf("agent was not told it is blocked:\n%s", text)
	}
	if !strings.Contains(text, "alice") {
		t.Errorf("agent cannot see who to coordinate with:\n%s", text)
	}
}

// The gateway must not let an agent act on a project it has no membership in, even though the
// token itself is valid.
func TestMCPRespectsMembership(t *testing.T) {
	h := newHarness(t)

	responses := drive(t, h.server.URL, h.outTok, h.project.ID,
		call("coord_project_status", map[string]any{}))

	text, isErr := toolText(t, responses[0])
	if !isErr {
		t.Errorf("a non-member got project status:\n%s", text)
	}
}
