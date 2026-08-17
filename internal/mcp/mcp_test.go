package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/domain"
)

// The MCP gateway is a translation layer (DESIGN.md §7.2), so these tests use a stub control
// plane and assert on the translation: that tools map to the right calls, that a coordination
// refusal reaches the agent as a readable result rather than a protocol error, and that the
// gateway never invents state of its own.

type stubPlane struct {
	*httptest.Server
	requests []recorded
}

type recorded struct {
	Method string
	Path   string
	Body   map[string]any
}

func newStubPlane(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) *stubPlane {
	t.Helper()
	stub := &stubPlane{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		stub.requests = append(stub.requests, recorded{r.Method, r.URL.Path, body})

		if handler, ok := routes[r.URL.Path]; ok {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	stub.Server = httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	return stub
}

func newTestServer(endpoint string) *Server {
	return &Server{
		api:     client.New(endpoint, "test-token"),
		project: "demo",
	}
}

// call drives one tool through the dispatcher and returns its text content.
func call(t *testing.T, s *Server, tool string, args map[string]any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	result, err := s.callTool(context.Background(), tool, raw)
	if err != nil {
		return err.Error(), true
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("tool result is %T, want a map", result)
	}
	content, _ := m["content"].([]map[string]any)
	if len(content) == 0 {
		t.Fatalf("tool result has no content: %+v", m)
	}
	text, _ := content[0]["text"].(string)
	isErr, _ := m["isError"].(bool)
	return text, isErr
}

func TestToolListIsSmallAndDocumented(t *testing.T) {
	tools := toolDefinitions()

	// DESIGN.md §4.9: too many tools waste context and make selection less reliable.
	if len(tools) > 12 {
		t.Errorf("%d tools exposed; the design calls for a minimal surface", len(tools))
	}

	seen := map[string]bool{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		desc, _ := tool["description"].(string)
		if name == "" {
			t.Error("a tool has no name")
		}
		if seen[name] {
			t.Errorf("duplicate tool %q", name)
		}
		seen[name] = true
		if len(desc) < 40 {
			t.Errorf("tool %q has a thin description; agents choose tools by these", name)
		}
		if _, ok := tool["inputSchema"].(map[string]any); !ok {
			t.Errorf("tool %q has no input schema", name)
		}
	}

	// Heartbeats must not be a tool: a model should never spend tokens saying "still alive".
	for name := range seen {
		if strings.Contains(name, "heartbeat") {
			t.Errorf("%q should not be an MCP tool (DESIGN.md §7.2)", name)
		}
	}
	if !seen["conductor_check_conflicts"] {
		t.Error("the check-before-you-edit tool is missing")
	}
}

func TestCheckConflictsCallsTheIntentEndpoint(t *testing.T) {
	stub := newStubPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/projects/demo/intents/check": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"outcome":"block_conflict","advice":"alice holds it","reason":"scope conflict"}`))
		},
	})
	s := newTestServer(stub.URL)

	text, isErr := call(t, s, "conductor_check_conflicts", map[string]any{
		"summary": "touch the router",
		"scopes":  []map[string]any{{"resource": "dir:internal/router", "mode": "write_exclusive"}},
	})
	if isErr {
		t.Fatalf("check returned an error result: %s", text)
	}
	if !strings.Contains(text, "block_conflict") || !strings.Contains(text, "alice holds it") {
		t.Errorf("agent-visible result lost the verdict or the advice:\n%s", text)
	}

	if len(stub.requests) != 1 {
		t.Fatalf("made %d requests, want 1", len(stub.requests))
	}
	req := stub.requests[0]
	if req.Path != "/v1/projects/demo/intents/check" || req.Method != http.MethodPost {
		t.Errorf("called %s %s", req.Method, req.Path)
	}
}

// A 409 from the control plane is a coordination answer, not a transport fault. It must reach
// the agent as a readable tool result so it can act on it, rather than as a protocol error.
func TestBlockedStartWorkIsAResultNotAnError(t *testing.T) {
	stub := newStubPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/projects/demo/work/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"outcome":"block_conflict","code":"scope_conflict",
				"error":"blocked","advice":"alice holds dir:internal/router for T-1"}`))
		},
	})
	s := newTestServer(stub.URL)

	text, isErr := call(t, s, "coord_start_work", map[string]any{"summary": "rework the router"})
	if isErr {
		t.Errorf("a blocked claim should be a normal result, not isError: %s", text)
	}
	if !strings.Contains(text, "alice holds") {
		t.Errorf("the agent cannot see who holds the resource:\n%s", text)
	}
}

// After a successful claim the gateway adopts the fence, so later calls in the same session
// are fenced without the agent having to track an epoch.
func TestStartWorkAdoptsTheFence(t *testing.T) {
	stub := newStubPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/projects/demo/work/start": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"outcome":"allow","task_id":"t-1","attempt_id":"a-1",
				"lease_id":"l-1","fencing_epoch":7,"task_ref":"T-1"}`))
		},
	})
	s := newTestServer(stub.URL)

	if _, isErr := call(t, s, "coord_start_work", map[string]any{"summary": "do the thing"}); isErr {
		t.Fatal("start_work failed")
	}
	if s.fence.AttemptID != "a-1" || s.fence.FencingEpoch != 7 {
		t.Fatalf("gateway did not adopt the fence: %+v", s.fence)
	}

	if _, isErr := call(t, s, "coord_report_progress", map[string]any{"phase": "implementing"}); isErr {
		t.Fatal("progress failed after a successful claim")
	}
	last := stub.requests[len(stub.requests)-1]
	if last.Path != "/v1/attempts/a-1/progress" {
		t.Errorf("progress went to %s", last.Path)
	}
	if epoch, _ := last.Body["fencing_epoch"].(float64); epoch != 7 {
		t.Errorf("progress carried epoch %v, want 7", last.Body["fencing_epoch"])
	}
}

// Without a claim, mutating tools must refuse rather than silently no-op.
func TestMutatingToolsRequireAClaim(t *testing.T) {
	stub := newStubPlane(t, nil)
	s := newTestServer(stub.URL)

	for _, tool := range []string{"coord_report_progress", "coord_finish_work", "coord_handoff"} {
		text, isErr := call(t, s, tool, map[string]any{"phase": "implementing", "outcome": "succeeded"})
		if !isErr {
			t.Errorf("%s without a claim should fail, got: %s", tool, text)
		}
		if !strings.Contains(text, "coord_start_work") {
			t.Errorf("%s should tell the agent how to recover, got: %s", tool, text)
		}
	}
	if len(stub.requests) != 0 {
		t.Errorf("gateway called the control plane %d times without a claim", len(stub.requests))
	}
}

func TestFinishClearsTheFenceOnlyWhenChecksPassed(t *testing.T) {
	stub := newStubPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/attempts/a-1/result": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"task_status":"running","missing_checks":["go test ./..."]}`))
		},
	})
	s := newTestServer(stub.URL)
	s.fence = domain.Fence{TaskID: "t-1", AttemptID: "a-1", LeaseID: "l-1", FencingEpoch: 3}

	text, isErr := call(t, s, "coord_finish_work", map[string]any{"outcome": "succeeded"})
	if isErr {
		t.Fatalf("finish errored: %s", text)
	}
	if !strings.Contains(text, "go test") {
		t.Errorf("the agent was not told which checks are missing:\n%s", text)
	}
	// The attempt is not over, so the fence must survive for the retry.
	if s.fence.AttemptID == "" {
		t.Error("fence was cleared even though the work was handed back")
	}
}

func TestUnknownToolIsReported(t *testing.T) {
	stub := newStubPlane(t, nil)
	s := newTestServer(stub.URL)
	if _, err := s.callTool(context.Background(), "coord_do_something_else", json.RawMessage(`{}`)); err == nil {
		t.Error("an unknown tool should be an error")
	}
}

func TestNoProjectConfiguredIsAClearError(t *testing.T) {
	s := &Server{api: client.New("http://example.invalid", "t")}
	_, err := s.callTool(context.Background(), "conductor_check_conflicts", json.RawMessage(`{"summary":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "CONDUCTOR_PROJECT") {
		t.Errorf("err = %v, want guidance about configuring a project", err)
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC framing
// ---------------------------------------------------------------------------

func TestServeHandlesInitializeAndToolsList(t *testing.T) {
	stub := newStubPlane(t, nil)
	s := newTestServer(stub.URL)

	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	}, "\n") + "\n")

	var out strings.Builder
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// A notification carries no id and must produce no response.
	if len(lines) != 3 {
		t.Fatalf("got %d responses, want 3 (a notification must not be answered):\n%s", len(lines), out.String())
	}

	var initResp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Instructions    string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if initResp.Result.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocol version = %q", initResp.Result.ProtocolVersion)
	}
	// The instructions are an agent's first contact with the coordination rules.
	if !strings.Contains(initResp.Result.Instructions, "check_conflicts") {
		t.Error("initialize should tell the agent to check before editing")
	}
	if !strings.Contains(initResp.Result.Instructions, "never shared") {
		t.Error("initialize should state the privacy guarantee")
	}
}

func TestServeRejectsMalformedJSON(t *testing.T) {
	stub := newStubPlane(t, nil)
	s := newTestServer(stub.URL)

	var out strings.Builder
	if err := s.Serve(context.Background(), strings.NewReader("{not json\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !strings.Contains(out.String(), "-32700") {
		t.Errorf("want a JSON-RPC parse error, got: %s", out.String())
	}
}

func TestServeReportsUnknownMethods(t *testing.T) {
	stub := newStubPlane(t, nil)
	s := newTestServer(stub.URL)

	var out strings.Builder
	if err := s.Serve(context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"nonsense"}`+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !strings.Contains(out.String(), "-32601") {
		t.Errorf("want method-not-found, got: %s", out.String())
	}
}
