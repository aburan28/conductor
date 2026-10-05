package api

import (
	"encoding/json"
	"net/http"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/notify"
)

// Notification channels (internal/notify): where a project's events go besides the dashboard.
//
// Managing them is a maintainer's job. A channel decides what leaves the system and where it
// goes, and its URL or secret is a credential, so contributors and observers can neither add
// one nor list them. The credentials themselves are returned to nobody after creation; the
// webhook signing secret is shown once, in the response that creates it.

func (s *Server) notifyRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("GET /v1/projects/{project}/notifications", auth(s.listNotifications))
	m.HandleFunc("POST /v1/projects/{project}/notifications", auth(s.createNotification))
	m.HandleFunc("DELETE /v1/projects/{project}/notifications/{channel}", auth(s.deleteNotification))
	m.HandleFunc("POST /v1/projects/{project}/notifications/{channel}/test", auth(s.testNotification))
}

// notificationsOff answers when this server was started without the relay's configuration.
func (s *Server) notificationsOff(w http.ResponseWriter, r *http.Request) bool {
	if s.notify != nil {
		return false
	}
	s.ok(w, r, http.StatusServiceUnavailable, ErrorBody{
		Error: "notifications are not enabled on this server", Code: "not_configured"})
	return true
}

type notificationList struct {
	Channels []notify.ChannelView `json:"channels"`
	// Events is the catalog a channel can subscribe to, and Defaults what it gets when it
	// names none, so clients do not hard-code either.
	Events   []notify.EventType `json:"events"`
	Defaults []string           `json:"defaults"`
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.notificationsOff(w, r) {
		return
	}
	channels, err := s.notify.List(r.Context(), project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, notificationList{
		Channels: channels, Events: notify.Catalog, Defaults: notify.DefaultEvents})
}

func (s *Server) createNotification(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.notificationsOff(w, r) {
		return
	}
	var body notify.CreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	created, err := s.notify.Create(r.Context(), project, p.ID, body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Where a project's events are sent is on the record; the URL is not.
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID,
		"notification.channel_added", "notification_channel", created.ID,
		map[string]any{"kind": created.Kind, "count": len(created.Events)})
	s.ok(w, r, http.StatusCreated, created)
}

func (s *Server) deleteNotification(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.notificationsOff(w, r) {
		return
	}
	id := r.PathValue("channel")
	if err := s.notify.Delete(r.Context(), project.ID, id); err != nil {
		if isBadUUID(err) {
			err = domain.ErrNotFound
		}
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID,
		"notification.channel_removed", "notification_channel", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testNotification(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.notificationsOff(w, r) {
		return
	}
	res, err := s.notify.Test(r.Context(), project, r.PathValue("channel"))
	if err != nil {
		if isBadUUID(err) {
			err = domain.ErrNotFound
		}
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, res)
}
