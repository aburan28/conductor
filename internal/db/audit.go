package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// AuditEntry is one audit record, as the admin area lists and exports it.
type AuditEntry struct {
	ID             int64          `json:"id"`
	At             time.Time      `json:"at"`
	OrganizationID domain.ID      `json:"organization_id"`
	ProjectID      domain.ID      `json:"project_id,omitempty"`
	Project        string         `json:"project,omitempty"`
	ActorID        domain.ID      `json:"actor_id,omitempty"`
	Actor          string         `json:"actor,omitempty"`
	Action         string         `json:"action"`
	TargetType     string         `json:"target_type,omitempty"`
	TargetID       string         `json:"target_id,omitempty"`
	Detail         map[string]any `json:"detail"`
}

// AuditHook sees every audit record after it is written. It is the seam for streaming the
// audit log somewhere else (a notification channel, a SIEM): it runs on the writer's
// goroutine, so it must hand the entry off and return, and it never sees a record that
// failed to write.
type AuditHook func(AuditEntry)

// SetAuditHook installs (or, with nil, removes) the audit hook.
func (s *Store) SetAuditHook(fn AuditHook) {
	if fn == nil {
		s.auditHook.Store(nil)
		return
	}
	s.auditHook.Store(&fn)
}

func (s *Store) notifyAudit(e AuditEntry) {
	fn := s.auditHook.Load()
	if fn == nil {
		return
	}
	defer func() {
		// A broken sink must not take the audited operation down with it.
		if r := recover(); r != nil {
			slog.Error("audit hook panicked", "action", e.Action, "panic", fmt.Sprint(r))
		}
	}()
	(*fn)(e)
}

// AuditFilter narrows an audit listing. Zero fields do not filter.
type AuditFilter struct {
	ActorID domain.ID
	// Action matches exactly, or as a prefix when it ends in "." or "*" ("sso." or "sso*").
	Action string
	Since  time.Time
	Until  time.Time
	// Before pages backwards: only entries with an id below it.
	Before int64
	Limit  int
}

// MaxAuditPage bounds one listing; an export pages through with Before.
const MaxAuditPage = 5000

// ListAudit returns an organization's audit records, newest first.
func (s *Store) ListAudit(ctx context.Context, orgID domain.ID, f AuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 || f.Limit > MaxAuditPage {
		f.Limit = MaxAuditPage
	}
	where := []string{"a.organization_id = $1::uuid"}
	args := []any{orgID}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.ActorID != "" {
		where = append(where, "a.actor_principal = "+arg(f.ActorID)+"::uuid")
	}
	if a := strings.TrimSpace(f.Action); a != "" {
		if prefix, ok := strings.CutSuffix(a, "*"); ok || strings.HasSuffix(a, ".") {
			if !ok {
				prefix = a
			}
			// LIKE metacharacters in the prefix are escaped: an action filter is not a pattern.
			escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
			where = append(where, "a.action LIKE "+arg(escaped+"%"))
		} else {
			where = append(where, "a.action = "+arg(a))
		}
	}
	if !f.Since.IsZero() {
		where = append(where, "a.created_at >= "+arg(f.Since))
	}
	if !f.Until.IsZero() {
		where = append(where, "a.created_at < "+arg(f.Until))
	}
	if f.Before > 0 {
		where = append(where, "a.id < "+arg(f.Before))
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.created_at, a.organization_id::text, COALESCE(a.project_id::text, ''), COALESCE(pr.slug, ''),
		       COALESCE(a.actor_principal::text, ''), COALESCE(p.handle, ''), a.action, a.target_type, a.target_id, a.detail
		  FROM audit_log a
		  LEFT JOIN principals p ON p.id = a.actor_principal
		  LEFT JOIN projects pr ON pr.id = a.project_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY a.id DESC
		 LIMIT `+arg(f.Limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.At, &e.OrganizationID, &e.ProjectID, &e.Project, &e.ActorID, &e.Actor,
			&e.Action, &e.TargetType, &e.TargetID, &detail); err != nil {
			return nil, err
		}
		e.Detail = map[string]any{}
		_ = json.Unmarshal(detail, &e.Detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AuditActions lists the distinct actions an organization's audit log holds, for the
// viewer's filter.
func (s *Store) AuditActions(ctx context.Context, orgID domain.ID) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT action FROM audit_log WHERE organization_id = $1::uuid ORDER BY action LIMIT 500`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
