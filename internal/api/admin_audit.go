package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
)

// The audit log viewer and export (GET /v1/admin/audit).
//
//	?actor=HANDLE        records made by one principal
//	?action=sso.login    one action; "sso." or "sso*" for a family
//	?since=… &until=…    RFC 3339 times or YYYY-MM-DD
//	?before=ID&limit=N   page backwards (json only)
//	?format=json|jsonl|csv
//
// json answers one page for the viewer. jsonl and csv export every matching record, newest
// first, as a download; an export is itself audited.

// maxAuditExport bounds one export. A larger log is exported in time windows.
const maxAuditExport = 200000

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if err := s.requireOrgAdmin(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	f := db.AuditFilter{Action: q.Get("action")}
	var err error
	if f.Since, err = parseTimeParam(q.Get("since")); err != nil {
		s.fail(w, r, err)
		return
	}
	if f.Until, err = parseTimeParam(q.Get("until")); err != nil {
		s.fail(w, r, err)
		return
	}
	if v := q.Get("before"); v != "" {
		if f.Before, err = strconv.ParseInt(v, 10, 64); err != nil || f.Before < 0 {
			s.fail(w, r, fmt.Errorf("%w: before must be a record id", domain.ErrInvalidArgument))
			return
		}
	}
	format := q.Get("format")
	switch format {
	case "", "json", "jsonl", "csv":
	default:
		s.fail(w, r, fmt.Errorf("%w: format must be json, jsonl or csv", domain.ErrInvalidArgument))
		return
	}
	noActor := false
	if handle := q.Get("actor"); handle != "" {
		actor, err := s.store.GetPrincipalByHandle(r.Context(), p.OrganizationID, handle)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			noActor = true
		case err != nil:
			s.fail(w, r, err)
			return
		default:
			f.ActorID = actor.ID
		}
	}

	if format == "" || format == "json" {
		f.Limit = intParam(r, "limit", 100)
		if f.Limit <= 0 || f.Limit > 1000 {
			f.Limit = 100
		}
		entries := []db.AuditEntry{}
		if !noActor {
			if entries, err = s.store.ListAudit(r.Context(), p.OrganizationID, f); err != nil {
				s.fail(w, r, err)
				return
			}
		}
		actions, err := s.store.AuditActions(r.Context(), p.OrganizationID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		out := map[string]any{"entries": entries, "actions": actions}
		if len(entries) == f.Limit {
			out["next_before"] = entries[len(entries)-1].ID
		}
		s.ok(w, r, http.StatusOK, out)
		return
	}

	// An export: every matching record, page by page.
	var all []db.AuditEntry
	for !noActor && len(all) < maxAuditExport {
		f.Limit = min(db.MaxAuditPage, maxAuditExport-len(all))
		page, err := s.store.ListAudit(r.Context(), p.OrganizationID, f)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		all = append(all, page...)
		if len(page) < f.Limit {
			break
		}
		f.Before = page[len(page)-1].ID
	}
	s.store.Audit(r.Context(), p.OrganizationID, "", p.ID, "audit.exported", "organization", string(p.OrganizationID),
		map[string]any{"count": len(all), "kind": format})
	name := fmt.Sprintf("conductor-audit-%s.%s", time.Now().UTC().Format("20060102-150405"), format)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if format == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		for _, e := range all {
			if err := enc.Encode(e); err != nil {
				return
			}
		}
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "at", "actor", "actor_id", "action", "project", "target_type", "target_id", "detail"})
	for _, e := range all {
		detail, _ := json.Marshal(e.Detail)
		_ = cw.Write([]string{strconv.FormatInt(e.ID, 10), e.At.UTC().Format(time.RFC3339Nano), csvCell(e.Actor), string(e.ActorID),
			csvCell(e.Action), csvCell(e.Project), csvCell(e.TargetType), csvCell(e.TargetID), csvCell(string(detail))})
	}
	cw.Flush()
}

// csvCell defuses a cell a spreadsheet would read as a formula. Handles, project slugs and
// details are chosen by people other than the one opening the export, and "=HYPERLINK(…)"
// in a handle must arrive as text (OWASP, "CSV injection").
func csvCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
