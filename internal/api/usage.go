package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/usage"
)

// Token usage (DESIGN.md §26.1). Collectors post hourly buckets; reports read them back.

type usageBody struct {
	Buckets []usage.Bucket `json:"buckets"`
}

// recordSessionUsage is what a `conductor wrap` sidecar calls with what its harness logged.
func (s *Server) recordSessionUsage(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	session, err := s.store.GetSession(r.Context(), r.PathValue("session"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	caller, err := s.svc.Authorize(r.Context(), p, session.ProjectID, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body usageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	n, err := s.svc.RecordSessionUsage(r.Context(), caller, session, body.Buckets)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"recorded": n})
}

// recordSyncedUsage is what `conductor usage sync` calls for sessions that were not wrapped.
func (s *Server) recordSyncedUsage(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body usageBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	n, err := s.svc.RecordSyncedUsage(r.Context(), caller, project.ID, body.Buckets)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"recorded": n})
}

// getUsage reports usage over a window: ?since=7d&until=…&by=day,harness&harness=&model=
func (s *Server) getUsage(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := time.Now().UTC()
	qs := r.URL.Query()
	since, err := coord.ParseUsageWindow(qs.Get("since"), now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	until, err := coord.ParseUsageWindow(qs.Get("until"), now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var by []string
	for _, d := range strings.Split(qs.Get("by"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			by = append(by, d)
		}
	}
	report, err := s.svc.Usage(r.Context(), caller, project.ID, db.UsageQuery{
		Since: since, Until: until, By: by,
		Harness: qs.Get("harness"), Model: qs.Get("model"),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, report)
}
