package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aburan28/conductor/internal/domain"
)

func TestObjectiveTasksCarryATimelineAndSettleOnce(t *testing.T) {
	h := newHarness(t)
	create := func(body map[string]any) map[string]any {
		t.Helper()
		code, out := h.do(h.bobTok, "POST", h.projectPath("/tasks"), body)
		if code != http.StatusCreated {
			t.Fatalf("create: %d %s", code, out)
		}
		var v map[string]any
		_ = json.Unmarshal(out, &v)
		return v
	}
	root := create(map[string]any{"title": "Compare A and B", "labels": []string{ObjectiveLabel}, "external_ref": "delphi-objective:r1:n0"})
	child := create(map[string]any{"title": "Summarize A", "labels": []string{ObjectiveLabel}, "external_ref": "delphi-objective:r1:n0.1", "parent": root["ref"]})
	if child["parent_id"] != root["id"] {
		t.Fatalf("child parent_id = %v, want %v", child["parent_id"], root["id"])
	}
	// The same external ref is a conflict, and finds the task through the list filter.
	if code, out := h.do(h.bobTok, "POST", h.projectPath("/tasks"), map[string]any{"title": "again", "external_ref": "delphi-objective:r1:n0"}); code != http.StatusConflict {
		t.Fatalf("duplicate external ref: %d %s", code, out)
	}
	code, out := h.do(h.bobTok, "GET", h.projectPath("/tasks?external_ref=delphi-objective:r1:n0.1"), nil)
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if code != 200 || json.Unmarshal(out, &list) != nil || len(list.Tasks) != 1 || list.Tasks[0]["id"] != child["id"] {
		t.Fatalf("by external ref: %d %s", code, out)
	}
	code, out = h.do(h.bobTok, "GET", h.projectPath("/tasks?parent="+root["ref"].(string)), nil)
	if code != 200 || json.Unmarshal(out, &list) != nil || len(list.Tasks) != 1 {
		t.Fatalf("by parent: %d %s", code, out)
	}

	post := func(task any, body map[string]any) (int, []byte) {
		return h.do(h.bobTok, "POST", "/v1/tasks/"+task.(string)+"/timeline", body)
	}
	if code, out := post(child["id"], map[string]any{"type": "objective.checked", "payload": map[string]any{"node": "n0.1", "verdict": "accept", "target": "openai:gpt"}}); code != http.StatusCreated {
		t.Fatalf("timeline: %d %s", code, out)
	}
	if code, _ := post(child["id"], map[string]any{"type": "task.done", "payload": map[string]any{}}); code != http.StatusBadRequest {
		t.Errorf("a non-objective event type was accepted: %d", code)
	}
	if code, _ := post(child["id"], map[string]any{"type": "objective.answered", "payload": map[string]any{"text": "the model's answer"}}); code != http.StatusBadRequest {
		t.Errorf("an answer text was accepted on the timeline: %d", code)
	}
	plain := create(map[string]any{"title": "ordinary work"})
	if code, _ := post(plain["id"], map[string]any{"type": "objective.checked", "payload": map[string]any{}}); code != http.StatusBadRequest {
		t.Errorf("a non-objective task accepted an objective event: %d", code)
	}
	// Only the creator (or a maintainer) drives it.
	if code, _ := h.do(h.outTok, "POST", "/v1/tasks/"+child["id"].(string)+"/timeline", map[string]any{"type": "objective.checked", "payload": map[string]any{}}); code == http.StatusCreated {
		t.Error("an outsider appended to the timeline")
	}

	settle := func(task any, status string) (int, []byte) {
		return h.do(h.bobTok, "POST", "/v1/tasks/"+task.(string)+"/settle", map[string]any{"status": status, "reason": "passed evaluator and openai:gpt"})
	}
	if code, out := settle(child["id"], "done"); code != 200 {
		t.Fatalf("settle done: %d %s", code, out)
	}
	if code, out := settle(child["id"], "done"); code != 200 {
		t.Fatalf("settle done again (idempotent): %d %s", code, out)
	}
	if code, _ := settle(child["id"], "failed"); code == 200 {
		t.Error("a settled task settled again differently")
	}
	if code, out := settle(root["id"], "failed"); code != 200 {
		t.Fatalf("settle failed: %d %s", code, out)
	}
	tr, err := h.store.GetTask(h.t.Context(), root["id"].(string))
	if err != nil || tr.Status != domain.TaskFailed {
		t.Fatalf("root status %s %v", tr.Status, err)
	}

	// The root's timeline with its subtree shows the child's events and both settles.
	code, out = h.do(h.bobTok, "GET", "/v1/tasks/"+root["id"].(string)+"/timeline?subtree=true", nil)
	var tl struct {
		Events []domain.Event `json:"events"`
	}
	if code != 200 || json.Unmarshal(out, &tl) != nil {
		t.Fatalf("timeline: %d %s", code, out)
	}
	seen := map[string]bool{}
	for _, e := range tl.Events {
		seen[e.Type] = true
	}
	if !seen["objective.checked"] {
		t.Errorf("subtree timeline lacks the child's event: %s", out)
	}
	code, out = h.do(h.bobTok, "GET", "/v1/tasks/"+root["id"].(string)+"/timeline", nil)
	if code != 200 || json.Unmarshal(out, &tl) != nil {
		t.Fatalf("timeline: %d %s", code, out)
	}
	for _, e := range tl.Events {
		if e.Type == "objective.checked" {
			t.Errorf("the root's own timeline includes a child event without subtree")
		}
	}
}
