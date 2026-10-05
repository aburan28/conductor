package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// The interactive loop over the public API, exactly as the CLI and the wrap sidecar drive it.

func (h *harness) shortLeases(ttl time.Duration) {
	h.t.Helper()
	cfg := h.project.Config
	cfg.LeaseTTL = domain.Duration(ttl)
	if err := h.store.UpdateProjectConfig(context.Background(), h.project.ID, cfg); err != nil {
		h.t.Fatalf("UpdateProjectConfig: %v", err)
	}
}

func (h *harness) jsonDo(token, method, path string, body any, out any) int {
	h.t.Helper()
	code, raw := h.do(token, method, path, body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return code
}

func (h *harness) readyTask(title string) domain.ID {
	h.t.Helper()
	var view struct {
		ID  domain.ID `json:"id"`
		Ref string    `json:"ref"`
	}
	if code := h.jsonDo(h.aliceTok, http.MethodPost, h.projectPath("/tasks"),
		map[string]any{"title": title, "status": "ready"}, &view); code != http.StatusCreated {
		h.t.Fatalf("create task = %d", code)
	}
	return view.ID
}

func (h *harness) session(token string) domain.Session {
	h.t.Helper()
	var s domain.Session
	if code := h.jsonDo(token, http.MethodPost, h.projectPath("/sessions"),
		map[string]any{"harness": "claude"}, &s); code != http.StatusCreated {
		h.t.Fatalf("register session = %d", code)
	}
	return s
}

func (h *harness) leaseOf(id domain.ID) domain.Lease {
	h.t.Helper()
	l, err := h.store.GetLease(context.Background(), id)
	if err != nil {
		h.t.Fatalf("GetLease: %v", err)
	}
	return l
}

// claim -> wrap: a claim made from a plain shell is adopted by the session that starts after
// it and lives as long as that session heartbeats; when the session dies, the claim lapses.
func TestClaimThenWrapStaysHeldWhileTheSessionLives(t *testing.T) {
	h := newHarness(t)
	const ttl = time.Second
	h.shortLeases(ttl)
	taskID := h.readyTask("claim then wrap")

	var claimed struct {
		Lease domain.Lease `json:"lease"`
		Fence domain.Fence `json:"fence"`
	}
	if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/tasks/"+taskID+"/claim",
		map[string]any{"harness": "cli", "worktree_path": "/src/checkout",
			"scopes": []map[string]any{{"resource": "path:internal/loop/a.go"}}}, &claimed); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	// With no session yet, the claim waits for one longer than a single TTL.
	if left := time.Until(claimed.Lease.ExpiresAt); left < 5*time.Minute {
		t.Fatalf("an unbound claim expires in %s; nobody could start a session in time", left)
	}

	session := h.session(h.aliceTok)
	var adopted struct {
		Adopted []struct {
			TaskID domain.ID `json:"task_id"`
		} `json:"adopted"`
	}
	if code := h.jsonDo(h.bobTok, http.MethodPost, "/v1/sessions/"+session.ID+"/adopt",
		map[string]any{"worktree_path": "/src/checkout"}, nil); code != http.StatusForbidden {
		t.Errorf("adopting through someone else's session = %d, want 403", code)
	}
	if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/sessions/"+session.ID+"/adopt",
		map[string]any{"worktree_path": "/src/checkout"}, &adopted); code != http.StatusOK || len(adopted.Adopted) != 1 {
		t.Fatalf("adopt = %d %+v", code, adopted)
	}

	// The session heartbeats (the wrap sidecar) for well past three TTLs.
	deadline := time.Now().Add(3*ttl + 500*time.Millisecond)
	for time.Now().Before(deadline) {
		if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/sessions/"+session.ID+"/heartbeat",
			map[string]any{"state": "working", "changed_paths": []string{"internal/loop/a.go"}}, nil); code != http.StatusOK {
			t.Fatalf("heartbeat = %d", code)
		}
		if _, err := h.store.ReconcileLeases(context.Background(), h.project.ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(ttl / 4)
	}
	if err := h.store.AssertFence(context.Background(), claimed.Fence); err != nil {
		t.Fatalf("the claim lapsed under a live session: %v", err)
	}

	// The session dies. One TTL later the reconciler takes the claim back.
	time.Sleep(ttl + 300*time.Millisecond)
	if _, err := h.store.ReconcileLeases(context.Background(), h.project.ID); err != nil {
		t.Fatal(err)
	}
	if h.leaseOf(claimed.Lease.ID).Active(time.Now()) {
		t.Fatal("the claim outlived its session")
	}
}

// The pre-edit hook reserves a file under the session's claim without handling a fence.
func TestSessionScopesReserveUnderTheSessionsClaim(t *testing.T) {
	h := newHarness(t)
	session := h.session(h.aliceTok)

	// No claim yet: nothing to reserve under, and it says so rather than failing.
	var res struct {
		NoClaim bool   `json:"no_claim"`
		TaskRef string `json:"task_ref"`
		Outcome string `json:"outcome"`
	}
	if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/sessions/"+session.ID+"/scopes",
		map[string]any{"scopes": []map[string]any{{"resource": "path:internal/loop/b.go"}}}, &res); code != http.StatusOK || !res.NoClaim {
		t.Fatalf("no-claim reserve = %d %+v", code, res)
	}

	taskID := h.readyTask("hooked")
	if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/tasks/"+taskID+"/claim",
		map[string]any{"harness": "claude", "session_id": session.ID}, nil); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	if code := h.jsonDo(h.bobTok, http.MethodPost, "/v1/sessions/"+session.ID+"/scopes",
		map[string]any{"scopes": []map[string]any{{"resource": "path:internal/loop/b.go"}}}, nil); code != http.StatusForbidden {
		t.Errorf("reserving through someone else's session = %d, want 403", code)
	}
	res = struct {
		NoClaim bool   `json:"no_claim"`
		TaskRef string `json:"task_ref"`
		Outcome string `json:"outcome"`
	}{}
	if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/sessions/"+session.ID+"/scopes",
		map[string]any{"scopes": []map[string]any{{"resource": "path:internal/loop/b.go"}}}, &res); code != http.StatusOK || res.Outcome != "allow" {
		t.Fatalf("reserve = %d %+v", code, res)
	}
	held, _ := h.store.ReservationsForTask(context.Background(), taskID)
	if len(held) != 1 || held[0].Source != domain.SourceObserved || held[0].ResourceKey != "internal/loop/b.go" {
		t.Errorf("reservations = %+v, want one observed path", held)
	}
}

// The dashboard's transition request (`{"status": ...}`) completes a verifying task and
// releases its pending-merge hold; the old `{"to": ...}` body is refused, not silently
// accepted.
func TestTransitionToDoneReleasesTerritory(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	taskID := h.readyTask("finish me")
	var claimed struct {
		Fence domain.Fence `json:"fence"`
	}
	if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/tasks/"+taskID+"/claim",
		map[string]any{"scopes": []map[string]any{{"resource": "path:internal/loop/c.go"}}}, &claimed); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/attempts/"+claimed.Fence.AttemptID+"/progress",
		map[string]any{"lease_id": claimed.Fence.LeaseID, "fencing_epoch": claimed.Fence.FencingEpoch, "phase": "implementing"}, nil); code != http.StatusAccepted {
		t.Fatalf("progress = %d", code)
	}
	var finished struct {
		TaskStatus domain.TaskStatus `json:"task_status"`
	}
	if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/attempts/"+claimed.Fence.AttemptID+"/result",
		map[string]any{"lease_id": claimed.Fence.LeaseID, "fencing_epoch": claimed.Fence.FencingEpoch, "outcome": "succeeded"}, &finished); code != http.StatusOK {
		t.Fatalf("finish = %d", code)
	}
	if finished.TaskStatus != domain.TaskVerifying {
		t.Skipf("project requires checks before verifying (status %s); covered by the db suite", finished.TaskStatus)
	}
	if held, _ := h.store.ReservationsForTask(ctx, taskID); len(held) == 0 {
		t.Fatal("finished work dropped its territory before merging")
	}

	if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/tasks/"+taskID+"/transition", map[string]any{"to": "done"}, nil); code < 400 {
		t.Errorf("a body without status = %d, want a refusal", code)
	}
	var view struct {
		Status domain.TaskStatus `json:"status"`
	}
	if code := h.jsonDo(h.aliceTok, http.MethodPost, "/v1/tasks/"+taskID+"/transition", map[string]any{"status": "done"}, &view); code != http.StatusOK || view.Status != domain.TaskDone {
		t.Fatalf("transition = %d %s", code, view.Status)
	}
	if held, _ := h.store.ReservationsForTask(ctx, taskID); len(held) != 0 {
		t.Errorf("a done task still holds %d reservation(s)", len(held))
	}
}

// coord_publish_result against the real API: the commands and the commit reach the task.
func TestMCPPublishResultRecordsEvidence(t *testing.T) {
	h := newHarness(t)
	responses := drive(t, h.server.URL, h.aliceTok, h.project.ID,
		call("coord_start_work", map[string]any{"summary": "publish evidence", "title": "Evidence"}),
		call("coord_publish_result", map[string]any{
			"commit_sha": "c0ffee", "changed_paths": []string{"internal/e.go"},
			"commands": []map[string]any{{"command": "go test ./...", "exit_code": 0}},
		}),
	)
	if text, isErr := toolText(t, responses[1]); isErr {
		t.Fatalf("publish errored: %s", text)
	}
	tasks, err := h.store.ListTasks(context.Background(), h.project.ID, db.ListTasksFilter{})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %d, %v", len(tasks), err)
	}
	results, err := h.store.ListValidation(context.Background(), tasks[0].ID)
	if err != nil || len(results) != 1 || results[0].Command != "go test ./..." || results[0].RunnerID != "" {
		t.Fatalf("validation = %+v, %v", results, err)
	}
	attempt, err := h.store.LatestAttempt(context.Background(), tasks[0].ID)
	if err != nil || attempt.CommitSHA != "c0ffee" {
		t.Errorf("attempt commit = %q, %v", attempt.CommitSHA, err)
	}
}
