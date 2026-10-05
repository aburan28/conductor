package api

import (
	"encoding/json"
	"errors"
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

// maxUsageBody bounds a usage upload. A sync posts hourly buckets — a few hundred bytes each —
// so even a year of buckets for every harness fits with room to spare; without a bound, any
// member could make the server buffer an arbitrarily large body.
const maxUsageBody = 8 << 20

// decodeUsage reads a usage upload under maxUsageBody, answering 413 when it is exceeded and
// 400 when it is not JSON. It reports whether the handler should continue.
func (s *Server) decodeUsage(w http.ResponseWriter, r *http.Request, dst *usageBody) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxUsageBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.ok(w, r, http.StatusRequestEntityTooLarge, ErrorBody{
				Error: "usage upload exceeds 8 MiB; sync in smaller windows", Code: "too_large"})
			return false
		}
		s.fail(w, r, domain.ErrInvalidArgument)
		return false
	}
	return true
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
	if !s.decodeUsage(w, r, &body) {
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
	if !s.decodeUsage(w, r, &body) {
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
