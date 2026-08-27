package api

import "net/http"

// mcpRoutes mounts the MCP Streamable HTTP transport. Implemented in mcp_http.go.
func (s *Server) mcpRoutes(m *http.ServeMux) { s.mountMCP(m) }
