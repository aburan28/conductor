package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The HTTP transport is the same translation layer as stdio, reached over Streamable HTTP.
// These tests drive the transport directly against a stub control plane, so they need no
// database: the transport authenticates nothing itself (the API layer does), it only
// multiplexes JSON-RPC sessions and proxies tool calls.

func newHTTPTestTransport(t *testing.T) (*HTTPTransport, *stubPlane) {
	t.Helper()
	stub := newStubPlane(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/whoami": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"projects":[{"id":"p-1","slug":"demo"}]}`))
		},
		"/v1/projects/demo/intents/check": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"outcome":"block_conflict","advice":"alice holds it","reason":"scope conflict"}`))
		},
	})
	return NewHTTPTransport(stub.URL), stub
}

// serve drives one POST through the transport and returns the recorder.
func serve(t *testing.T, tr *HTTPTransport, sessionID, project string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/mcp"
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if sessionID != "" {
		r.Header.Set("Mcp-Session-Id", sessionID)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	tr.Serve(w, r, "principal-1", "token-abc", project)
	return w
}

func decodeResp(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	return m
}

func TestHTTPInitializeNegotiatesAndIssuesSession(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)

	// A supported version is echoed back verbatim.
	w := serve(t, tr, "", "demo", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body %s", w.Code, w.Body.String())
	}
	sid := w.Header().Get("Mcp-Session-Id")
	if len(sid) != 32 {
		t.Fatalf("session id = %q, want 32 hex chars", sid)
	}
	resp := decodeResp(t, w.Body.String())
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Errorf("negotiated %v, want the supported version echoed back", result["protocolVersion"])
	}

	// An unsupported version falls back to the newest we speak.
	w = serve(t, tr, "", "demo", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	result = decodeResp(t, w.Body.String())["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Errorf("unsupported version negotiated to %v, want %s", result["protocolVersion"], ProtocolVersion)
	}
}

func TestHTTPSessionIsRequiredAfterInitialize(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)

	// tools/list without a session id (and no initialize) is a 400.
	w := serve(t, tr, "", "demo", nil, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("session-less tools/list = %d, want 400", w.Code)
	}

	// With a session, tools/list returns the full tool set.
	sid := serve(t, tr, "", "demo", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`).Header().Get("Mcp-Session-Id")
	w = serve(t, tr, sid, "demo", nil, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/list = %d", w.Code)
	}
	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Result.Tools) != 11 {
		t.Errorf("tools/list returned %d tools, want 11", len(resp.Result.Tools))
	}
}

func TestHTTPUnknownSessionIs404(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)
	w := serve(t, tr, "deadbeefdeadbeefdeadbeefdeadbeef", "demo", nil,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown session = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "re-initialize") {
		t.Errorf("404 body should tell the client to re-initialize: %s", w.Body.String())
	}
}

func TestHTTPToolCallProxiesToControlPlane(t *testing.T) {
	tr, stub := newHTTPTestTransport(t)
	sid := serve(t, tr, "", "demo", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`).Header().Get("Mcp-Session-Id")

	w := serve(t, tr, sid, "demo", nil, `{"jsonrpc":"2.0","id":3,"method":"tools/call",
		"params":{"name":"conductor_check_conflicts","arguments":{"summary":"touch the router"}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/call = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "block_conflict") || !strings.Contains(w.Body.String(), "alice holds it") {
		t.Errorf("tool result lost the verdict:\n%s", w.Body.String())
	}
	var hit bool
	for _, req := range stub.requests {
		if req.Path == "/v1/projects/demo/intents/check" {
			hit = true
		}
	}
	if !hit {
		t.Error("the transport did not proxy the tool call to the control plane")
	}
}

func TestHTTPNotificationOnlyIs202(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)
	sid := serve(t, tr, "", "demo", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`).Header().Get("Mcp-Session-Id")

	w := serve(t, tr, sid, "demo", nil, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("notification-only = %d, want 202", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "" {
		t.Errorf("202 should have an empty body, got %q", w.Body.String())
	}
}

func TestHTTPBatchReturnsBatch(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)
	sid := serve(t, tr, "", "demo", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`).Header().Get("Mcp-Session-Id")

	w := serve(t, tr, sid, "demo", nil, `[
		{"jsonrpc":"2.0","id":2,"method":"tools/list"},
		{"jsonrpc":"2.0","method":"notifications/initialized"},
		{"jsonrpc":"2.0","id":3,"method":"ping"}]`)
	if w.Code != http.StatusOK {
		t.Fatalf("batch = %d", w.Code)
	}
	var arr []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &arr); err != nil {
		t.Fatalf("batch response is not an array: %v (%s)", err, w.Body.String())
	}
	// Two requests, one notification: two responses.
	if len(arr) != 2 {
		t.Errorf("batch returned %d responses, want 2", len(arr))
	}
}

func TestHTTPDeleteEndsSession(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)
	sid := serve(t, tr, "", "demo", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`).Header().Get("Mcp-Session-Id")

	r := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	r.Header.Set("Mcp-Session-Id", sid)
	w := httptest.NewRecorder()
	tr.Serve(w, r, "principal-1", "token-abc", "demo")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", w.Code)
	}
	// The session is gone: a follow-up is a 404.
	w2 := serve(t, tr, sid, "demo", nil, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if w2.Code != http.StatusNotFound {
		t.Errorf("after delete, session lookup = %d, want 404", w2.Code)
	}
}

func TestHTTPRejectsCrossOrigin(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)
	w := serve(t, tr, "", "demo", map[string]string{"Origin": "https://evil.example.com"},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d, want 403", w.Code)
	}
	// A localhost origin is allowed.
	w = serve(t, tr, "", "demo", map[string]string{"Origin": "http://localhost:5173"},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if w.Code != http.StatusOK {
		t.Errorf("localhost origin = %d, want 200", w.Code)
	}
}

func TestHTTPRejectsUnsupportedProtocolHeader(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)
	w := serve(t, tr, "", "demo", map[string]string{"MCP-Protocol-Version": "1999-01-01"},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unsupported protocol header = %d, want 400", w.Code)
	}
}

func TestHTTPSessionBoundToPrincipalAndToken(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)
	sid := serve(t, tr, "", "demo", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`).Header().Get("Mcp-Session-Id")

	// Same session id, different principal → 404 (a leaked id is useless).
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	r.Header.Set("Mcp-Session-Id", sid)
	w := httptest.NewRecorder()
	tr.Serve(w, r, "someone-else", "token-abc", "demo")
	if w.Code != http.StatusNotFound {
		t.Errorf("session used by a different principal = %d, want 404", w.Code)
	}
}

func TestHTTPProjectFromSoleWhoamiProject(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)
	// No path project, no query, no header: the transport asks whoami and finds exactly one.
	sid := serve(t, tr, "", "", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`).Header().Get("Mcp-Session-Id")
	// A tool call now resolves against "demo" (the sole project) and hits the stub route.
	w := serve(t, tr, sid, "", nil, `{"jsonrpc":"2.0","id":3,"method":"tools/call",
		"params":{"name":"conductor_check_conflicts","arguments":{"summary":"x"}}}`)
	if !strings.Contains(w.Body.String(), "block_conflict") {
		t.Errorf("sole-project fallback did not route to demo:\n%s", w.Body.String())
	}
}

// A malformed body still gets a JSON-RPC parse error, not a hang or a 500.
func TestHTTPMalformedBodyIsParseError(t *testing.T) {
	tr, _ := newHTTPTestTransport(t)
	sid := serve(t, tr, "", "demo", nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`).Header().Get("Mcp-Session-Id")
	w := serve(t, tr, sid, "demo", nil, `{not json`)
	if !bytes.Contains(w.Body.Bytes(), []byte("-32700")) {
		t.Errorf("want a parse error, got %s", w.Body.String())
	}
}

// A batch delivered over stdio is answered with a batch, exercising the same transport-
// agnostic Dispatch the HTTP transport uses.
func TestServeStdioBatchYieldsBatch(t *testing.T) {
	stub := newStubPlane(t, nil)
	s := newTestServer(stub.URL)

	in := strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}},` +
		`{"jsonrpc":"2.0","method":"notifications/initialized"},` +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}]` + "\n")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &arr); err != nil {
		t.Fatalf("stdio batch response is not an array: %v (%s)", err, out.String())
	}
	if len(arr) != 2 {
		t.Errorf("stdio batch returned %d responses, want 2", len(arr))
	}
}
