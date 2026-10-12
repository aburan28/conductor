package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExternalObservationRejectsUnstructuredContext(t *testing.T) {
	observed := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	base := `{"event_id":"run-1:guard-1","source":"ecc2k_worker","worker_run_id":"run-1","kind":"safety.flag","rule_id":"lease_lost","action":"stop","reason_code":"lease_expired","observed_at":"` + observed + `"`
	for name, body := range map[string]string{
		"prompt":       base + `,"prompt":"private task text"}`,
		"tool input":   base + `,"tool_input":{"command":"secret"}}`,
		"prose reason": base + `,"reason_code":"lease expired after prompt"}`,
		"bad digest":   base + `,"evidence_sha256":"not-a-digest"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			if _, err := readExternalObservation(r); err == nil {
				t.Fatal("unstructured context was accepted")
			}
		})
	}
}

func TestExternalObservationProjectReadAndReplay(t *testing.T) {
	h := newHarness(t)
	path := "/v1/projects/" + string(h.project.ID) + "/external-observations"
	observed := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	status := map[string]any{
		"event_id": "worker-7:heartbeat-1", "source": "ecc2k_worker",
		"worker_run_id": "worker-7", "kind": "worker.status", "status": "running",
		"observed_at": observed,
	}
	if code, body := h.do(h.outTok, http.MethodPost, path, status); code != http.StatusForbidden {
		t.Fatalf("outsider POST: %d %s", code, body)
	}
	code, body := h.do(h.bobTok, http.MethodPost, path, status)
	if code != http.StatusCreated {
		t.Fatalf("first POST: %d %s", code, body)
	}
	var first struct {
		Observation struct {
			ID string `json:"id"`
		} `json:"observation"`
	}
	if err := json.Unmarshal(body, &first); err != nil || first.Observation.ID == "" {
		t.Fatalf("first response: %s (%v)", body, err)
	}
	code, body = h.do(h.bobTok, http.MethodPost, path, status)
	if code != http.StatusCreated {
		t.Fatalf("replay POST: %d %s", code, body)
	}
	var replayed struct {
		Observation struct {
			ID string `json:"id"`
		} `json:"observation"`
	}
	if err := json.Unmarshal(body, &replayed); err != nil || replayed.Observation.ID != first.Observation.ID {
		t.Fatalf("replay changed id: %s (%v)", body, err)
	}
	status["status"] = "failed"
	if code, body := h.do(h.bobTok, http.MethodPost, path, status); code != http.StatusConflict {
		t.Fatalf("conflicting replay: %d %s", code, body)
	}
	flag := map[string]any{
		"event_id": "worker-7:lease-1", "source": "ecc2k_worker",
		"worker_run_id": "worker-7", "kind": "safety.flag",
		"rule_id": "lease_lost", "rule_version": "v1", "action": "stop",
		"reason_code": "lease_expired", "trigger_event_id": "heartbeat-1",
		"evidence_sha256": strings.Repeat("a", 64), "observed_at": observed,
	}
	if code, body := h.do(h.bobTok, http.MethodPost, path, flag); code != http.StatusCreated {
		t.Fatalf("flag POST: %d %s", code, body)
	}
	if code, body := h.do(h.outTok, http.MethodGet, path, nil); code != http.StatusForbidden {
		t.Fatalf("outsider GET: %d %s", code, body)
	}
	code, body = h.do(h.aliceTok, http.MethodGet, path, nil)
	if code != http.StatusOK {
		t.Fatalf("project GET: %d %s", code, body)
	}
	var listed struct {
		Observations []struct {
			ID     string `json:"id"`
			TaskID string `json:"task_id"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
			RuleID string `json:"rule_id"`
		} `json:"observations"`
	}
	if err := json.Unmarshal(body, &listed); err != nil || len(listed.Observations) != 2 {
		t.Fatalf("taskless project result: %s (%v)", body, err)
	}
	seenStatus, seenFlag := false, false
	for _, o := range listed.Observations {
		if o.TaskID != "" {
			t.Fatalf("taskless result gained task: %s", body)
		}
		seenStatus = seenStatus || o.Kind == "worker.status" && o.ID == first.Observation.ID && o.Status == "running"
		seenFlag = seenFlag || o.Kind == "safety.flag" && o.RuleID == "lease_lost"
	}
	if !seenStatus || !seenFlag {
		t.Fatalf("missing status or flag: %s", body)
	}
}
