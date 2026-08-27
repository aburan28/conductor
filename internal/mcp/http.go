package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/adamburan/conductor/internal/client"
)

// HTTPTransport serves the gateway over MCP's Streamable HTTP transport (spec revisions
// 2025-03-26 and 2025-06-18), so a harness can reach the coordination tools with nothing but
// a bearer token and a URL — no local binary to install, no stdio to wire up.
//
// It is still the same translation layer. Each MCP session gets its own gateway Server that
// calls the control plane at the same public API every other client uses, with the caller's
// own token. The transport holds no coordination state; it multiplexes sessions, and that is
// all.
type HTTPTransport struct {
	// endpoint is the control-plane URL the per-session gateways call. It is this server's
	// own address (Options.SelfEndpoint), so a tool call is an ordinary authenticated API
	// request that re-enters the front door rather than a private path into the store.
	endpoint string
	idleTTL  time.Duration
	now      func() time.Time

	mu       sync.Mutex
	sessions map[string]*httpSession
}

type httpSession struct {
	id          string
	server      *Server
	principalID string
	tokenHash   string
	project     string
	protocol    string
	lastSeen    time.Time

	// call serializes tool calls within one session, because the gateway mutates its fence
	// as work is claimed and finished. Concurrent requests on one session are rare but must
	// not race the fence.
	call sync.Mutex
}

// NewHTTPTransport builds a transport whose per-session gateways call the control plane at
// endpoint (this server's own public URL).
func NewHTTPTransport(endpoint string) *HTTPTransport {
	return &HTTPTransport{
		endpoint: endpoint,
		idleTTL:  time.Hour,
		now:      time.Now,
		sessions: map[string]*httpSession{},
	}
}

// maxBody caps a request body. Tool arguments can carry a user's own text, so besides the
// size limit the transport never logs a body.
const maxBody = 4 << 20

// Serve handles one Streamable-HTTP request. The API layer has already authenticated the
// caller and passes the resolved principal id, the caller's own bearer token (reused to reach
// the control plane), and the project taken from the URL path ("" when the path names none).
func (t *HTTPTransport) Serve(w http.ResponseWriter, r *http.Request, principalID, token, pathProject string) {
	if !originAllowed(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !SupportedProtocol(v) {
		writeJSONRPCError(w, http.StatusBadRequest, nullID, -32600,
			"unsupported MCP-Protocol-Version "+v)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// No server-initiated stream: every response rides its POST. This is spec-compliant
		// (the server MAY decline to open an SSE stream) and keeps the transport stateless
		// between requests.
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "this MCP endpoint does not open a server stream; POST your requests",
			http.StatusMethodNotAllowed)
	case http.MethodDelete:
		t.deleteSession(w, r, principalID, token)
	case http.MethodPost:
		t.post(w, r, principalID, token, pathProject)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (t *HTTPTransport) post(w http.ResponseWriter, r *http.Request, principalID, token, pathProject string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nullID, -32700, "could not read body")
		return
	}

	tokenHash := hashToken(token)
	sessionID := r.Header.Get("Mcp-Session-Id")

	var sess *httpSession
	if sessionID != "" {
		sess = t.lookup(sessionID, principalID, tokenHash)
		if sess == nil {
			// Unknown or expired session: 404 with a JSON-RPC error tells a compliant client
			// to start over with initialize rather than to give up.
			writeJSONRPCError(w, http.StatusNotFound, nullID, -32001,
				"unknown or expired MCP session; re-initialize")
			return
		}
	}

	// A brand-new connection must lead with initialize. Anything else has nowhere to run,
	// because per-session state (the project, the fence) does not exist yet.
	if sess == nil {
		if !containsInitialize(body) {
			writeJSONRPCError(w, http.StatusBadRequest, nullID, -32600,
				"missing Mcp-Session-Id; send initialize first")
			return
		}
		project := t.resolveProject(r, token, pathProject)
		sess = t.create(principalID, token, tokenHash, project)
	}

	sess.call.Lock()
	resp := sess.server.Dispatch(r.Context(), body)
	sess.call.Unlock()
	t.touch(sess)

	// Always echo the session id so a client that just initialized learns it.
	w.Header().Set("Mcp-Session-Id", sess.id)
	if sess.protocol != "" {
		w.Header().Set("MCP-Protocol-Version", sess.protocol)
	}

	if resp == nil {
		// The payload was all notifications/responses; there is nothing to return.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func (t *HTTPTransport) deleteSession(w http.ResponseWriter, r *http.Request, principalID, token string) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		http.Error(w, "no Mcp-Session-Id", http.StatusBadRequest)
		return
	}
	if t.lookup(sessionID, principalID, hashToken(token)) == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	t.mu.Lock()
	delete(t.sessions, sessionID)
	t.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// resolveProject picks the project for a new session: the URL path, then the ?project query,
// then the X-Conductor-Project header, and finally — if the caller belongs to exactly one
// project — that one. A session with no project still initializes; its tool calls then fail
// with a clear message, which is friendlier than refusing the handshake.
func (t *HTTPTransport) resolveProject(r *http.Request, token, pathProject string) string {
	if pathProject != "" {
		return pathProject
	}
	if v := r.URL.Query().Get("project"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Conductor-Project"); v != "" {
		return v
	}
	// Fall back to the caller's sole project, if there is exactly one.
	api := client.New(t.endpoint, token)
	var who struct {
		Projects []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"projects"`
	}
	if err := api.Get(r.Context(), "/v1/whoami", &who); err == nil && len(who.Projects) == 1 {
		if who.Projects[0].Slug != "" {
			return who.Projects[0].Slug
		}
		return who.Projects[0].ID
	}
	return ""
}

func (t *HTTPTransport) create(principalID, token, tokenHash, project string) *httpSession {
	sess := &httpSession{
		id:          newSessionID(),
		server:      newHTTP(t.endpoint, token, project, ""),
		principalID: principalID,
		tokenHash:   tokenHash,
		project:     project,
		protocol:    ProtocolVersion,
		lastSeen:    t.now(),
	}
	t.mu.Lock()
	t.sweepLocked()
	t.sessions[sess.id] = sess
	t.mu.Unlock()
	return sess
}

func (t *HTTPTransport) lookup(id, principalID, tokenHash string) *httpSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()
	sess, ok := t.sessions[id]
	if !ok {
		return nil
	}
	// Bind the session to who opened it: a leaked session id is useless without the same
	// principal and the same token behind it.
	if sess.principalID != principalID || sess.tokenHash != tokenHash {
		return nil
	}
	return sess
}

func (t *HTTPTransport) touch(sess *httpSession) {
	t.mu.Lock()
	sess.lastSeen = t.now()
	t.mu.Unlock()
}

// sweepLocked drops idle sessions. Caller holds t.mu.
func (t *HTTPTransport) sweepLocked() {
	cutoff := t.now().Add(-t.idleTTL)
	for id, sess := range t.sessions {
		if sess.lastSeen.Before(cutoff) {
			delete(t.sessions, id)
		}
	}
}

// containsInitialize reports whether a payload (single message or batch) carries an
// initialize request, so a session-less POST is allowed exactly when it can create one.
func containsInitialize(body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return false
	}
	if trimmed[0] == '[' {
		var batch []struct {
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(trimmed), &batch) != nil {
			return false
		}
		for _, m := range batch {
			if m.Method == "initialize" {
				return true
			}
		}
		return false
	}
	var one struct {
		Method string `json:"method"`
	}
	return json.Unmarshal([]byte(trimmed), &one) == nil && one.Method == "initialize"
}

// originAllowed implements DNS-rebinding protection: a browser-driven request must come from
// this host or from localhost. A request with no Origin (an ordinary CLI or SDK) is allowed.
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	host := hostOf(origin)
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return host != "" && host == hostname(r.Host)
}

func hostOf(rawurl string) string {
	s := rawurl
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return hostname(s)
}

func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func writeJSONRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(mustMarshal(errorResponse(id, code, message, nil)))
}
