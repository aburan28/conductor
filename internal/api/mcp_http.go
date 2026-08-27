package api

import (
	"net/http"
	"strings"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/mcp"
)

// mountMCP exposes the coordination tools over MCP's Streamable HTTP transport, so a coding
// tool can reach them with a bearer token and a URL and no local process to install.
//
// The endpoints reuse the same bearer-token authentication as every other route, then hand
// the request to a per-session gateway that calls the control plane back over the public API
// with the caller's own token. The gateway is a translation layer (DESIGN.md §7.2): it holds
// no coordination state and has no path into the store that a normal client would not have.
func (s *Server) mountMCP(m *http.ServeMux) {
	// endpoint is where a per-session gateway reaches this control plane. It defaults to the
	// self URL; if none was configured, loopback is the only safe assumption.
	endpoint := s.self
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8080"
	}
	transport := mcp.NewHTTPTransport(endpoint)

	handle := func(withProject bool) http.HandlerFunc {
		return s.authenticate(func(w http.ResponseWriter, r *http.Request, principal domain.Principal) {
			project := ""
			if withProject {
				project = r.PathValue("project")
			}
			transport.Serve(w, r, principal.ID, bearerToken(r), project)
		})
	}

	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		m.HandleFunc(method+" /mcp", handle(false))
		m.HandleFunc(method+" /mcp/{project}", handle(true))
	}
}

// bearerToken re-extracts the caller's raw token from the Authorization header. The
// per-session gateway reuses it to call the control plane, so a tool call is authorized
// exactly as the caller is — never with the server's own privileges.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")); token != header {
		return token
	}
	return ""
}
