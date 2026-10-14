package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

// The external observation API accepts only bounded identifiers and enums.
// In particular, it has no field for prompts, tool inputs, output, prose, or
// private evidence locations. Unknown JSON fields are rejected.
type externalObservationBody struct {
	EventID        string    `json:"event_id"`
	Source         string    `json:"source"`
	WorkerRunID    string    `json:"worker_run_id"`
	SessionID      string    `json:"session_id,omitempty"`
	TurnID         string    `json:"turn_id,omitempty"`
	TriggerEventID string    `json:"trigger_event_id,omitempty"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status,omitempty"`
	RuleID         string    `json:"rule_id,omitempty"`
	RuleVersion    string    `json:"rule_version,omitempty"`
	Action         string    `json:"action,omitempty"`
	ReasonCode     string    `json:"reason_code,omitempty"`
	SourceRevision string    `json:"source_revision,omitempty"`
	EvidenceSHA256 string    `json:"evidence_sha256,omitempty"`
	ObservedAt     time.Time `json:"observed_at"`
	TaskID         domain.ID `json:"task_id,omitempty"`
	AttemptID      domain.ID `json:"attempt_id,omitempty"`
}

var observationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]*$`)
var observationCode = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var observationHex = regexp.MustCompile(`^[a-f0-9]+$`)
var observationUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func validObservationToken(value string, max int, pattern *regexp.Regexp) bool {
	return len(value) > 0 && len(value) <= max && pattern.MatchString(value)
}

func (b externalObservationBody) validate() error {
	bad := func(field string) error { return fmt.Errorf("%w: invalid %s", domain.ErrInvalidArgument, field) }
	if !validObservationToken(b.EventID, 128, observationID) {
		return bad("event_id")
	}
	if b.Source != "ecc2k_worker" && b.Source != "runpod_agent" {
		return bad("source")
	}
	if !validObservationToken(b.WorkerRunID, 128, observationID) {
		return bad("worker_run_id")
	}
	for field, value := range map[string]string{
		"session_id": b.SessionID, "turn_id": b.TurnID, "trigger_event_id": b.TriggerEventID,
	} {
		if value != "" && !validObservationToken(value, 128, observationID) {
			return bad(field)
		}
	}
	if b.Kind != "worker.status" && b.Kind != "safety.flag" {
		return bad("kind")
	}
	if b.ObservedAt.IsZero() || b.ObservedAt.After(time.Now().Add(5*time.Minute)) {
		return bad("observed_at")
	}
	if b.Kind == "worker.status" {
		if !validObservationToken(b.Status, 48, observationCode) ||
			b.RuleID != "" || b.RuleVersion != "" || b.Action != "" || b.ReasonCode != "" {
			return bad("status or safety fields")
		}
	} else {
		if b.Status != "" || !validObservationToken(b.RuleID, 128, observationID) ||
			!validObservationToken(b.Action, 48, observationCode) ||
			!validObservationToken(b.ReasonCode, 64, observationCode) {
			return bad("safety fields")
		}
		if b.RuleVersion != "" && !validObservationToken(b.RuleVersion, 64, observationID) {
			return bad("rule_version")
		}
	}
	if b.SourceRevision != "" && (len(b.SourceRevision) < 7 || len(b.SourceRevision) > 64 || !observationHex.MatchString(b.SourceRevision)) {
		return bad("source_revision")
	}
	if b.EvidenceSHA256 != "" && (len(b.EvidenceSHA256) != 64 || !observationHex.MatchString(b.EvidenceSHA256)) {
		return bad("evidence_sha256")
	}
	if b.AttemptID != "" && b.TaskID == "" {
		return bad("attempt_id without task_id")
	}
	if (b.TaskID != "" && !observationUUID.MatchString(string(b.TaskID))) ||
		(b.AttemptID != "" && !observationUUID.MatchString(string(b.AttemptID))) {
		return bad("task_id or attempt_id")
	}
	return nil
}

func readExternalObservation(r *http.Request) (externalObservationBody, error) {
	const maxBody = 16 << 10
	var b externalObservationBody
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		return b, fmt.Errorf("%w: observation body exceeds limit", domain.ErrInvalidArgument)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&b); err != nil {
		return b, fmt.Errorf("%w: invalid observation JSON", domain.ErrInvalidArgument)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return b, fmt.Errorf("%w: trailing observation JSON", domain.ErrInvalidArgument)
	}
	return b, b.validate()
}

func (s *Server) recordExternalObservation(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	b, err := readExternalObservation(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if b.TaskID != "" {
		task, err := s.store.GetTask(r.Context(), b.TaskID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if task.ProjectID != project.ID {
			s.fail(w, r, domain.ErrNotFound)
			return
		}
	}
	if b.AttemptID != "" {
		attempt, err := s.store.GetAttempt(r.Context(), b.AttemptID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if attempt.ProjectID != project.ID || attempt.TaskID != b.TaskID {
			s.fail(w, r, domain.ErrNotFound)
			return
		}
	}
	o := domain.ExternalObservation{
		ProjectID: project.ID, TaskID: b.TaskID, AttemptID: b.AttemptID,
		EventID: b.EventID, Source: b.Source, WorkerRunID: b.WorkerRunID,
		SessionID: b.SessionID, TurnID: b.TurnID, TriggerEventID: b.TriggerEventID,
		Kind: b.Kind, Status: b.Status, RuleID: b.RuleID, RuleVersion: b.RuleVersion,
		Action: b.Action, ReasonCode: b.ReasonCode, SourceRevision: b.SourceRevision,
		EvidenceSHA256: b.EvidenceSHA256, ObservedAt: b.ObservedAt.UTC(),
	}
	o, err = s.store.RecordExternalObservation(r.Context(), o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, map[string]any{"observation": o})
}

func (s *Server) listExternalObservations(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.store.ListExternalObservations(r.Context(), project.ID, intParam(r, "limit", 50))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if rows == nil {
		rows = []domain.ExternalObservation{}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"observations": rows})
}
