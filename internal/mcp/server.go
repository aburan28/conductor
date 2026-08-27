// Package mcp implements the Model Context Protocol gateway of DESIGN.md §18.
//
// Two design constraints shape this package:
//
//   - It is a translation layer, not a service. Every tool call becomes an HTTP request to
//     the control plane; there is no local state, no scheduling, and no database access
//     (§7.2). Anything the gateway could decide on its own would be a second source of
//     truth.
//   - The tool surface is deliberately small (§4.9). Every tool costs context in every
//     agent's window and makes tool selection less reliable, so high-frequency signals like
//     heartbeats are handled by the local adapter over HTTP instead — a heartbeat should
//     never cost model tokens (§7.2).
//
// The JSON-RPC core is transport-agnostic: Dispatch takes one payload (a message or a batch)
// and returns the encoded answer. Serve wraps it for stdio; HTTPTransport wraps it for
// Streamable HTTP, so a harness can mount the gateway as a local process or point at the
// control plane's own /mcp endpoint — the tools, and what they may know, are identical.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/domain"
)

// Version identifies this gateway build to clients.
const Version = "0.2.0"

// ProtocolVersion is the newest MCP revision this gateway speaks. It is what a client gets
// when it asks for a revision we do not know — the spec says to answer with the server's
// preferred version — and what a client gets when it names none.
const ProtocolVersion = "2025-06-18"

// SupportedProtocolVersions lists every revision this gateway can negotiate, oldest first.
// A client that names one of these gets it back verbatim.
var SupportedProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18"}

// SupportedProtocol reports whether a protocol revision is one this gateway can speak.
func SupportedProtocol(version string) bool {
	for _, v := range SupportedProtocolVersions {
		if v == version {
			return true
		}
	}
	return false
}

// negotiateProtocol implements the initialize rule: honour a revision we know, otherwise
// answer with our preferred (newest) one, which is also what an unset request gets.
func negotiateProtocol(requested string) string {
	if requested != "" && SupportedProtocol(requested) {
		return requested
	}
	return ProtocolVersion
}

// Server serves the Conductor tool set over JSON-RPC. One Server instance is one MCP session:
// it holds the fence adopted from a successful coord_start_work and the wrapping session id,
// so a run's later tool calls are automatically fenced. The stdio gateway has exactly one;
// the HTTP transport keeps one per Mcp-Session-Id.
type Server struct {
	api     *client.Client
	project string
	// fence comes from the environment when the gateway is mounted inside a running
	// attempt, so an agent's tool calls are automatically fenced without it having to
	// track the epoch itself.
	fence domain.Fence
	// session is the wrapping `conductor wrap` session, when there is one. It is what makes
	// "work offered to me" answerable: without it the gateway knows the principal but not
	// which of that principal's live sessions it is running inside.
	session domain.ID

	// out and mu belong to the stdio transport; the HTTP transport leaves them nil and
	// writes responses itself.
	mu  sync.Mutex
	out *bufio.Writer
}

// Options configures a gateway.
type Options struct {
	Endpoint  string
	Token     string
	Project   string
	Fence     domain.Fence
	SessionID domain.ID
}

// New builds a gateway from explicit options, filling gaps from the environment. This is the
// stdio entry point (cmd/conductor-mcp); the HTTP transport builds sessions with newHTTP
// instead, from an already-authenticated request, and never reads the environment.
func New(opts Options) *Server {
	creds := client.LoadCredentials()
	endpoint := firstNonEmpty(opts.Endpoint, creds.Endpoint)
	token := firstNonEmpty(opts.Token, creds.Token)
	project := firstNonEmpty(opts.Project, creds.Project, os.Getenv("CONDUCTOR_PROJECT"))

	fence := opts.Fence
	if fence.TaskID == "" {
		fence.TaskID = os.Getenv("CONDUCTOR_TASK_ID")
	}
	if fence.AttemptID == "" {
		fence.AttemptID = os.Getenv("CONDUCTOR_ATTEMPT_ID")
	}
	if fence.LeaseID == "" {
		fence.LeaseID = os.Getenv("CONDUCTOR_LEASE_ID")
	}
	if fence.FencingEpoch == 0 {
		if n, err := strconv.ParseInt(os.Getenv("CONDUCTOR_FENCING_EPOCH"), 10, 64); err == nil {
			fence.FencingEpoch = n
		}
	}

	return &Server{
		api:     client.New(endpoint, token),
		project: project,
		fence:   fence,
		session: firstNonEmpty(string(opts.SessionID), os.Getenv("CONDUCTOR_SESSION_ID")),
	}
}

// newHTTP builds a session-scoped gateway for the HTTP transport. Everything is explicit:
// the caller's own bearer token and the project resolved from the request, with no fallback
// to environment or on-disk credentials — the process may be serving many principals at once.
func newHTTP(endpoint, token, project, session string) *Server {
	return &Server{
		api:     client.New(endpoint, token),
		project: project,
		session: domain.ID(session),
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC core
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// nullID is the id a response carries when the request had none to echo (a parse error, an
// invalid request). JSON-RPC requires the field be present and null in that case.
var nullID = json.RawMessage("null")

// Dispatch handles one JSON-RPC payload — a single message or a batch — and returns the
// encoded response, or nil when there is nothing to answer (every element was a notification).
//
// This is the whole transport-agnostic surface. Serve feeds it lines; HTTPTransport feeds it
// request bodies. Neither knows anything about tools, and Dispatch knows nothing about pipes
// or sockets.
func (s *Server) Dispatch(ctx context.Context, payload []byte) []byte {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil
	}

	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return mustMarshal(parseErrorResponse())
		}
		if len(batch) == 0 {
			// An empty batch is an invalid request per the JSON-RPC 2.0 spec.
			return mustMarshal(errorResponse(nullID, -32600, "invalid request: empty batch", nil))
		}
		var out []*rpcResponse
		for _, msg := range batch {
			if resp := s.handleOne(ctx, msg); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return mustMarshal(out)
	}

	resp := s.handleOne(ctx, trimmed)
	if resp == nil {
		return nil
	}
	return mustMarshal(resp)
}

// handleOne parses and dispatches a single message, returning its response or nil for a
// notification.
func (s *Server) handleOne(ctx context.Context, raw json.RawMessage) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return parseErrorResponse()
	}
	if req.Method == "" {
		return errorResponse(orNull(req.ID), -32600, "invalid request: no method", nil)
	}
	return s.handle(ctx, req)
}

// handle routes one well-formed request. It returns nil for notifications (no id), which the
// caller drops.
func (s *Server) handle(ctx context.Context, req rpcRequest) *rpcResponse {
	notify := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		return result(req, map[string]any{
			"protocolVersion": negotiateProtocol(params.ProtocolVersion),
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"listChanged": false, "subscribe": false},
			},
			"serverInfo": map[string]any{"name": "conductor", "version": Version},
			"instructions": "Conductor coordinates humans and coding agents on one repository. " +
				"Call conductor_check_conflicts before you edit anything. Claim work with " +
				"coord_start_work. Your prompts and output are never shared with the team.",
		})

	case "notifications/initialized", "initialized", "notifications/cancelled":
		return nil // acknowledged by producing no response

	case "tools/list":
		return result(req, map[string]any{"tools": toolDefinitions()})

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorResponse(orNull(req.ID), -32602, "invalid params", nil)
		}
		out, err := s.callTool(ctx, params.Name, params.Arguments)
		if err != nil {
			// Tool failures are returned as tool results with isError, not as protocol
			// errors: the agent should read and act on "Alice holds this file", not treat
			// it as a transport fault.
			return result(req, toolResult(err.Error(), true))
		}
		return result(req, out)

	case "resources/list":
		return result(req, map[string]any{"resources": s.resourceList()})

	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errorResponse(orNull(req.ID), -32602, "invalid params", nil)
		}
		contents, err := s.resourceRead(ctx, params.URI)
		if err != nil {
			return errorResponse(orNull(req.ID), -32602, err.Error(), nil)
		}
		return result(req, map[string]any{"contents": contents})

	case "ping":
		return result(req, map[string]any{})

	default:
		if notify {
			return nil
		}
		return errorResponse(orNull(req.ID), -32601, "method not found: "+req.Method, nil)
	}
}

// result builds a success response, or nil for a notification (which must not be answered).
func result(req rpcRequest, payload any) *rpcResponse {
	if len(req.ID) == 0 {
		return nil
	}
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: payload}
}

func errorResponse(id json.RawMessage, code int, message string, data any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}}
}

func parseErrorResponse() *rpcResponse {
	return errorResponse(nullID, -32700, "parse error", nil)
}

func orNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return nullID
	}
	return id
}

func mustMarshal(v any) []byte {
	body, err := json.Marshal(v)
	if err != nil {
		body, _ = json.Marshal(errorResponse(nullID, -32603, "internal error encoding response", nil))
	}
	return body
}

// ---------------------------------------------------------------------------
// stdio transport
// ---------------------------------------------------------------------------

// Serve runs the newline-delimited JSON-RPC loop over stdio until the input stream closes.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = bufio.NewWriter(out)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if resp := s.Dispatch(ctx, line); resp != nil {
			s.writeLine(resp)
		}
	}
	return scanner.Err()
}

func (s *Server) writeLine(body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(body)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}

// ---------------------------------------------------------------------------
// Tool-result helpers (shared with tools.go)
// ---------------------------------------------------------------------------

// toolResult renders a tool response in MCP's content form.
func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// jsonResult renders a structured payload as pretty JSON, which is what agents parse most
// reliably.
func jsonResult(v any) map[string]any {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolResult("failed to encode result: "+err.Error(), true)
	}
	return toolResult(string(body), false)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// resources: the active task card, read-only
// ---------------------------------------------------------------------------

// resourceList advertises the current task card as an MCP resource, so a client that browses
// resources rather than calling tools can still read what it is working on. Nothing here is
// another principal's text: a task card is coordination state (DESIGN.md §21).
func (s *Server) resourceList() []map[string]any {
	if s.fence.TaskID == "" {
		return []map[string]any{}
	}
	return []map[string]any{{
		"uri":         "conductor://task/" + s.fence.TaskID,
		"name":        "Active task card",
		"description": "The coordination state of the task this session is working on.",
		"mimeType":    "text/markdown",
	}}
}

func (s *Server) resourceRead(ctx context.Context, uri string) ([]map[string]any, error) {
	ref := strings.TrimPrefix(uri, "conductor://task/")
	if ref == uri || ref == "" {
		ref = s.fence.TaskID
	}
	if ref == "" {
		return nil, errors.New("no task: pass a conductor://task/<ref> uri or start work first")
	}
	card, err := s.api.Raw(ctx, "/v1/tasks/"+ref+"/card"+client.Query("project", s.project))
	if err != nil {
		return nil, err
	}
	return []map[string]any{{
		"uri":      uri,
		"mimeType": "text/markdown",
		"text":     string(card),
	}}, nil
}
