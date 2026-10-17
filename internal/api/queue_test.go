package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aburan28/conductor/internal/domain"
)

func (h *harness) setQueueCap(t *testing.T, q domain.QueuePolicy) {
	t.Helper()
	cfg := domain.DefaultProjectConfig()
	cfg.Queue = q
	if err := h.store.UpdateProjectConfig(context.Background(), h.project.ID, cfg); err != nil {
		t.Fatalf("UpdateProjectConfig: %v", err)
	}
}

func (h *harness) enqueue(t *testing.T, token string, body map[string]any) domain.AdmissionTicket {
	t.Helper()
	code, resp := h.do(token, http.MethodPost, h.projectPath("/queue"), body)
	if code != http.StatusCreated {
		t.Fatalf("enqueue = %d\n%s", code, resp)
	}
	var ticket domain.AdmissionTicket
	if err := json.Unmarshal(resp, &ticket); err != nil {
		t.Fatalf("decode ticket: %v", err)
	}
	return ticket
}

// The admission queue admits up to the cap and makes the rest wait, and a member can read the
// queue, heartbeat their own ticket, and cancel it — freeing the slot for the next in line.
func TestQueueAdmissionOverHTTP(t *testing.T) {
	h := newHarness(t)
	h.setQueueCap(t, domain.QueuePolicy{MaxActiveSessions: 1, TicketTTLSeconds: 90})

	first := h.enqueue(t, h.aliceTok, map[string]any{"kind": "session", "harness": "claude"})
	if first.State != domain.TicketGranted {
		t.Fatalf("first ticket = %s, want granted", first.State)
	}
	second := h.enqueue(t, h.bobTok, map[string]any{"kind": "session", "harness": "codex"})
	if second.State != domain.TicketQueued || second.Position != 1 {
		t.Fatalf("second ticket = %s pos %d, want queued/1", second.State, second.Position)
	}

	// The whole queue is visible to a member.
	code, body := h.do(h.bobTok, http.MethodGet, h.projectPath("/queue"), nil)
	if code != http.StatusOK {
		t.Fatalf("get queue = %d\n%s", code, body)
	}
	var view struct {
		Active struct {
			Sessions int `json:"sessions"`
		} `json:"active"`
		Tickets []domain.AdmissionTicket `json:"tickets"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode queue: %v", err)
	}
	if len(view.Tickets) != 2 {
		t.Fatalf("queue has %d tickets, want 2", len(view.Tickets))
	}

	// Bob can heartbeat his own ticket.
	if code, body := h.do(h.bobTok, http.MethodPost, "/v1/queue/"+second.ID+"/heartbeat", nil); code != http.StatusOK {
		t.Fatalf("heartbeat = %d\n%s", code, body)
	}
	// A contributor cannot cancel another member's ticket (bob is a contributor).
	if code, _ := h.do(h.bobTok, http.MethodDelete, "/v1/queue/"+first.ID+"?cancel=true", nil); code != http.StatusForbidden {
		t.Errorf("bob cancelling alice's ticket = %d, want 403", code)
	}
	// Releasing the granted ticket admits the waiter.
	if code, body := h.do(h.aliceTok, http.MethodDelete, "/v1/queue/"+first.ID, nil); code != http.StatusOK {
		t.Fatalf("release = %d\n%s", code, body)
	}
	code, body = h.do(h.bobTok, http.MethodGet, "/v1/queue/"+second.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("get ticket = %d\n%s", code, body)
	}
	var granted domain.AdmissionTicket
	_ = json.Unmarshal(body, &granted)
	if granted.State != domain.TicketGranted {
		t.Fatalf("waiter after release = %s, want granted", granted.State)
	}
}

// A task id names a task in any project, but a project's tasks and tickets can only refer
// to its own: a dependency or a queue ticket on another project's task is refused as
// unknown, so it neither links across projects nor confirms the id exists.
func TestTaskReferencesStayInTheProject(t *testing.T) {
	h := newHarness(t)
	other := newHarness(t)
	foreign := other.createTask(t, other.aliceTok, "another project's task")
	own := h.createTask(t, h.aliceTok, "this project's task")

	if code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/tasks"), map[string]any{
		"title": "depends across projects", "depends_on": []string{foreign.ID}}); code != http.StatusBadRequest {
		t.Errorf("a dependency on another project's task = %d, want 400\n%s", code, body)
	}
	if code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/tasks"), map[string]any{
		"title": "depends within the project", "depends_on": []string{own.ID}}); code != http.StatusCreated {
		t.Errorf("a dependency on this project's task = %d\n%s", code, body)
	}
	if code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/queue"), map[string]any{
		"kind": "session", "harness": "claude", "task": foreign.ID}); code != http.StatusNotFound {
		t.Errorf("a ticket for another project's task = %d, want 404\n%s", code, body)
	}
}

// route/explain on someone else's private task says where it would route and nothing about
// why: no rationale, no matched rules, no features — as for its recorded decisions.
func TestRouteExplainRedactsAPrivateTask(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)
	code, body := h.do(h.bobTok, http.MethodPost, h.projectPath("/tasks"), map[string]any{
		"title": "rotate the signing keys", "visibility": "private"})
	if code != http.StatusCreated {
		t.Fatalf("create task = %d\n%s", code, body)
	}
	var task struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &task)
	explain := func(tok string) map[string]any {
		code, body := h.do(tok, http.MethodGet, "/v1/tasks/"+task.ID+"/route/explain", nil)
		if code != http.StatusOK {
			t.Fatalf("route/explain = %d\n%s", code, body)
		}
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		return out
	}
	theirs := explain(h.aliceTok)
	decision, _ := theirs["decision"].(map[string]any)
	if theirs["redacted"] != true || theirs["features"] != nil || decision["model"] == "" {
		t.Errorf("another member's explain = %v, want a redacted route", theirs)
	}
	if r, _ := decision["rationale"].([]any); len(r) != 0 || decision["matched_rules"] != nil {
		t.Errorf("another member's explain carries the rationale: %v", decision)
	}
	if mine := explain(h.bobTok); mine["redacted"] == true || mine["features"] == nil {
		t.Errorf("the owner's explain = %v, want it in full", mine)
	}
}

// The swarm view rolls up capacity and is readable by any member.
func TestSwarmView(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodGet, h.projectPath("/swarm"), nil)
	if code != http.StatusOK {
		t.Fatalf("swarm = %d\n%s", code, body)
	}
	var view struct {
		Contributors []map[string]any `json:"contributors"`
		Capacity     map[string]any   `json:"capacity"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode swarm: %v", err)
	}
	if view.Capacity == nil {
		t.Fatal("swarm view has no capacity roll-up")
	}
}

// route/explain previews a routing decision without dispatching, and labels round-trip on a
// task so the dashboard and dispatch rules can use them.
func TestRouteExplainAndLabels(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)

	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/tasks"), map[string]any{
		"title": "add retry-aware routing", "labels": []string{"Backend", "backend", "docs"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create task = %d\n%s", code, body)
	}
	var task struct {
		Ref    string   `json:"ref"`
		Labels []string `json:"labels"`
	}
	_ = json.Unmarshal(body, &task)
	// Labels are normalized: lower-cased, de-duplicated, sorted.
	if len(task.Labels) != 2 || task.Labels[0] != "backend" || task.Labels[1] != "docs" {
		t.Fatalf("labels = %v, want [backend docs]", task.Labels)
	}

	code, body = h.do(h.aliceTok, http.MethodGet,
		"/v1/tasks/"+task.Ref+"/route/explain?project="+h.project.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("route/explain = %d\n%s", code, body)
	}
	var explain struct {
		TaskRef  string `json:"task_ref"`
		Decision struct {
			Model string `json:"model"`
			Tier  string `json:"tier"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(body, &explain); err != nil {
		t.Fatalf("decode explain: %v", err)
	}
	if explain.TaskRef != task.Ref || explain.Decision.Model == "" {
		t.Fatalf("explain = %+v, want a resolved model", explain)
	}
}
