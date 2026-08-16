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
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/domain"
)

// ProtocolVersion is the MCP revision this gateway speaks.
const ProtocolVersion = "2024-11-05"

// Server serves MCP over a JSON-RPC 2.0 stream.
type Server struct {
	api     *client.Client
	project string
	// fence comes from the environment when the gateway is mounted inside a running
	// attempt, so an agent's tool calls are automatically fenced without it having to
	// track the epoch itself.
	fence domain.Fence

	mu  sync.Mutex
	out *bufio.Writer
}

// Options configures a gateway.
type Options struct {
	Endpoint string
	Token    string
	Project  string
	Fence    domain.Fence
}

// New builds a gateway from explicit options, filling gaps from the environment.
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
	}
}

// Serve runs the JSON-RPC loop until the input stream closes.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = bufio.NewWriter(out)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.write(rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: "parse error"},
			})
			continue
		}
		s.handle(ctx, req)
	}
	return scanner.Err()
}

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

func (s *Server) handle(ctx context.Context, req rpcRequest) {
	// Notifications carry no id and expect no response.
	notify := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		s.reply(req, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "conductor", "version": "0.1.0"},
			"instructions": "Conductor coordinates humans and coding agents on one repository. " +
				"Call conductor_check_conflicts before you edit anything. Claim work with " +
				"coord_start_work. Your prompts and output are never shared with the team.",
		})

	case "notifications/initialized", "initialized":
		// Nothing to do; acknowledged by producing no response.

	case "tools/list":
		s.reply(req, map[string]any{"tools": toolDefinitions()})

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.replyError(req, -32602, "invalid params", nil)
			return
		}
		result, err := s.callTool(ctx, params.Name, params.Arguments)
		if err != nil {
			// Tool failures are returned as tool results with isError, not as protocol
			// errors: the agent should read and act on "Alice holds this file", not treat
			// it as a transport fault.
			s.reply(req, toolResult(err.Error(), true))
			return
		}
		s.reply(req, result)

	case "ping":
		s.reply(req, map[string]any{})

	default:
		if !notify {
			s.replyError(req, -32601, "method not found: "+req.Method, nil)
		}
	}
}

func (s *Server) reply(req rpcRequest, result any) {
	if len(req.ID) == 0 {
		return
	}
	s.write(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) replyError(req rpcRequest, code int, message string, data any) {
	if len(req.ID) == 0 {
		return
	}
	s.write(rpcResponse{
		JSONRPC: "2.0", ID: req.ID,
		Error: &rpcError{Code: code, Message: message, Data: data},
	})
}

func (s *Server) write(resp rpcResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = s.out.Write(body)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}

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
		return toolResult(fmt.Sprintf("failed to encode result: %v", err), true)
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
