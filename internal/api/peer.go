package api

import (
	"net/http"
	"time"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/peer"
)

// peerAuth guards the /v1/peer/* surface. It is a separate channel from authenticate on
// purpose: bearer tokens name principals inside a project, while a verified client
// certificate names a daemon in the mesh. The two never share a route.
//
// It requires a TLS connection with a client certificate that chained to the mesh CA
// (the server's ClientAuth is VerifyClientCertIfGiven, so human clients without a
// certificate are unaffected everywhere else).
func (s *Server) peerAuth(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			s.ok(w, r, http.StatusUnauthorized, ErrorBody{
				Error: "peer authentication required: present a mesh client certificate over TLS",
				Code:  "unauthenticated",
			})
			return
		}
		name := peer.CertName(r.TLS.PeerCertificates[0])
		if name == "" {
			s.ok(w, r, http.StatusUnauthorized, ErrorBody{
				Error: "peer certificate carries no usable name",
				Code:  "unauthenticated",
			})
			return
		}
		next(w, r, name)
	}
}

// peerInfo answers GET /v1/peer/info: who this daemon is, and everything it can dial in
// the mesh — itself and its known peers — so a peer running discovery can learn members
// it was not configured with. The caller's name is not echoed back — a peer asking "who
// are you" wants this daemon's mesh identity, not its own. It is what a peer link's probe
// consumes, so it stays cheap and side-effect free, and it never carries more than
// member names and addresses.
func (s *Server) peerInfo(w http.ResponseWriter, r *http.Request, _ string) {
	info := peer.Info{Name: s.peerName, Time: time.Now()}
	if s.peerStatus != nil {
		mesh := make([]peer.Peer, 0, 4)
		if s.self != "" {
			mesh = append(mesh, peer.Peer{Name: s.peerName, URL: s.self})
		}
		for _, l := range s.peerStatus() {
			if s.self != "" && l.URL == s.self {
				continue // already advertised as ourselves, above
			}
			mesh = append(mesh, peer.Peer{Name: l.Name, URL: l.URL})
		}
		info.Mesh = mesh
	}
	s.ok(w, r, http.StatusOK, info)
}

// listPeers answers GET /v1/peers for project members: the mesh link table.
func (s *Server) listPeers(w http.ResponseWriter, r *http.Request, _ domain.Principal) {
	if s.peerStatus == nil {
		s.ok(w, r, http.StatusOK, map[string]any{"peers": []peer.LinkStatus{}})
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"peers": s.peerStatus()})
}
