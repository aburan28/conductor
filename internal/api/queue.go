package api

import (
	"net/http"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/domain"
)

// The admission queue and swarm endpoints (domain.AdmissionTicket, coord.Swarm).
//
// When a project caps active sessions or attempts, work past the cap takes a ticket here and
// waits; the swarm view rolls up who is contributing capacity and who has budget to share.
func (s *Server) queueRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("GET /v1/projects/{project}/queue", auth(s.getQueue))
	m.HandleFunc("POST /v1/projects/{project}/queue", auth(s.enqueue))
	m.HandleFunc("GET /v1/queue/{ticket}", auth(s.getTicket))
	m.HandleFunc("POST /v1/queue/{ticket}/heartbeat", auth(s.heartbeatTicket))
	m.HandleFunc("DELETE /v1/queue/{ticket}", auth(s.releaseTicket))
	m.HandleFunc("GET /v1/projects/{project}/swarm", auth(s.getSwarm))
}

func (s *Server) getQueue(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.svc.Queue(r.Context(), project.ID, r.URL.Query().Get("closed") == "true")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, view)
}

type enqueueBody struct {
	Kind      domain.TicketKind `json:"kind"`
	SessionID domain.ID         `json:"session_id"`
	Task      string            `json:"task"`
	Harness   string            `json:"harness"`
	Model     string            `json:"model"`
	Priority  int               `json:"priority"`
	Note      string            `json:"note"`
}

func (s *Server) enqueue(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body enqueueBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Kind == "" {
		body.Kind = domain.TicketSession
	}
	ticket, err := s.svc.Enqueue(r.Context(), caller, project.ID, coord.EnqueueRequest{
		Kind: body.Kind, SessionID: body.SessionID, TaskRef: body.Task,
		Harness: body.Harness, Model: body.Model, Priority: body.Priority, Note: body.Note,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, ticket)
}

func (s *Server) getTicket(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	ticket, err := s.store.GetTicket(r.Context(), r.PathValue("ticket"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Authorize against the ticket's project; a member may read the queue they are in.
	if _, err := s.svc.Authorize(r.Context(), p, ticket.ProjectID, domain.RoleObserver); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, ticket)
}

func (s *Server) heartbeatTicket(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	caller, err := s.callerFromTicket(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ticket, err := s.svc.HeartbeatTicket(r.Context(), caller, r.PathValue("ticket"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, ticket)
}

func (s *Server) releaseTicket(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	caller, err := s.callerFromTicket(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cancelled := r.URL.Query().Get("cancel") == "true"
	ticket, err := s.svc.ReleaseTicket(r.Context(), caller, r.PathValue("ticket"), cancelled)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, ticket)
}

// callerFromTicket resolves the caller against the ticket's project. A ticket is addressed by
// its own id, so the project comes from the ticket rather than the path.
func (s *Server) callerFromTicket(r *http.Request, p domain.Principal) (coord.Caller, error) {
	ticket, err := s.store.GetTicket(r.Context(), r.PathValue("ticket"))
	if err != nil {
		return coord.Caller{}, err
	}
	return s.svc.Authorize(r.Context(), p, ticket.ProjectID, domain.RoleContributor)
}

func (s *Server) getSwarm(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.svc.Swarm(r.Context(), caller, project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, view)
}
