package db

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

func (f *fixture) countEvents(t *testing.T, eventType string) int {
	t.Helper()
	var n int
	if err := f.store.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM domain_events WHERE project_id = $1::uuid AND event_type = $2`,
		f.project.ID, eventType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An agent polling a blocked check, from any number of replicas, writes one conflict.blocked
// per (requester, holding task) per window — and another once the window has passed.
func TestConflictAnnouncementIsDedupedAcrossReplicas(t *testing.T) {
	f := newFixture(t)
	holder := f.newTask(t, "holder")
	other := f.newTask(t, "other holder")
	second, err := Open(f.ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	announce := func(s *Store, task domain.Task, requester domain.Principal, outcome string) bool {
		ok, err := s.AnnounceConflict(f.ctx, ConflictAnnouncement{
			OrganizationID: f.org.ID, ProjectID: f.project.ID, Requester: requester.ID,
			TaskID: task.ID, Outcome: outcome,
			Payload: map[string]any{"task_ref": task.Ref, "principal": requester.Handle,
				"resources": []string{"dir:internal/x"}},
		}, ConflictAlertWindow)
		if err != nil {
			t.Error(err)
		}
		return ok
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	written := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			if announce(s, holder, f.bob, ConflictAlertBlocked) {
				mu.Lock()
				written++
				mu.Unlock()
			}
		}([]*Store{f.store, second}[i%2])
	}
	wg.Wait()
	if written != 1 || f.countEvents(t, "conflict.blocked") != 1 {
		t.Fatalf("12 racing announcements wrote %d (%d events), want 1", written, f.countEvents(t, "conflict.blocked"))
	}

	// Another holder, another requester, or another outcome is another announcement.
	if !announce(f.store, other, f.bob, ConflictAlertBlocked) || !announce(f.store, holder, f.alice, ConflictAlertBlocked) ||
		!announce(second, holder, f.bob, ConflictAlertSuggestJoin) {
		t.Error("a distinct (requester, task, outcome) was suppressed")
	}
	if announce(second, holder, f.bob, ConflictAlertSuggestJoin) {
		t.Error("a repeated suggest_join was announced again")
	}

	// Past the window, the same block is news again.
	if _, err := f.store.pool.Exec(f.ctx, `
		UPDATE conflict_alerts SET announced_at = now() - interval '16 minutes'
		 WHERE project_id = $1::uuid AND outcome = 'blocked' AND task_id = $2::uuid AND subject = $3::uuid`,
		f.project.ID, holder.ID, f.bob.ID); err != nil {
		t.Fatal(err)
	}
	if !announce(second, holder, f.bob, ConflictAlertBlocked) {
		t.Error("a block still standing after the window was not announced again")
	}
	if n := f.countEvents(t, "conflict.blocked"); n != 4 {
		t.Errorf("%d conflict.blocked events, want 4", n)
	}
}

// The conflict graph is recomputed from scratch every pass; conflict.detected is written once
// per conflict while it stays open, only from medium severity up, at the narrower of the two
// tasks' visibilities — and again if the conflict closes and comes back.
func TestDetectedConflictIsAnnouncedOncePerOpening(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.newTask(t, "a"), f.newTask(t, "b"), f.newTask(t, "c")
	if _, err := f.store.pool.Exec(f.ctx, `UPDATE tasks SET visibility = 'private' WHERE id = $1::uuid`, b.ID); err != nil {
		t.Fatal(err)
	}
	edge := func(x, y domain.Task, sev domain.Severity) domain.ConflictEdge {
		return domain.ConflictEdge{ProjectID: f.project.ID, TaskA: x.ID, TaskB: y.ID, Severity: sev,
			Suggestion: domain.OutcomeSuggestSplit, Weight: 0.6,
			Detail: domain.ConflictDetail{SharedPaths: []string{"internal/x.go"}, Reason: "both changed",
				TaskARef: x.Ref, TaskBRef: y.Ref}}
	}
	replace := func(edges ...domain.ConflictEdge) {
		t.Helper()
		if err := f.store.ReplaceConflictsOfKind(f.ctx, f.project.ID, domain.ConflictMergeRisk, edges); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		replace(edge(a, b, domain.SeverityHigh), edge(a, c, domain.SeverityLow))
	}
	if n := f.countEvents(t, "conflict.detected"); n != 1 {
		t.Fatalf("three passes over one open conflict wrote %d events, want 1 (and none for the low one)", n)
	}
	var vis domain.Visibility
	var payload map[string]any
	var raw []byte
	if err := f.store.pool.QueryRow(f.ctx, `
		SELECT visibility, payload FROM domain_events WHERE project_id = $1::uuid AND event_type = 'conflict.detected'`,
		f.project.ID).Scan(&vis, &raw); err != nil {
		t.Fatal(err)
	}
	_ = decodeJSON(raw, &payload)
	if vis != domain.VisibilityPrivate {
		t.Errorf("an edge with a private task was announced at %s", vis)
	}
	if payload["with_task_ref"] == nil || payload["conflict_id"] == nil || payload["changed_paths"] == nil {
		t.Errorf("payload %v lacks the edge's second task, id, or shared paths", payload)
	}

	replace() // the conflict goes away
	replace(edge(a, b, domain.SeverityHigh))
	if n := f.countEvents(t, "conflict.detected"); n != 2 {
		t.Errorf("a conflict that closed and reopened: %d events, want 2", n)
	}
}

// AnnounceConflict leaves no announcement behind when its transaction fails.
func TestConflictAnnouncementRollsBack(t *testing.T) {
	f := newFixture(t)
	holder := f.newTask(t, "holder")
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.AnnounceConflict(ctx, ConflictAnnouncement{
		OrganizationID: f.org.ID, ProjectID: f.project.ID, Requester: f.bob.ID,
		TaskID: holder.ID, Outcome: ConflictAlertBlocked,
	}, time.Minute); err == nil {
		t.Fatal("a cancelled announcement succeeded")
	}
	ok, err := f.store.AnnounceConflict(f.ctx, ConflictAnnouncement{
		OrganizationID: f.org.ID, ProjectID: f.project.ID, Requester: f.bob.ID,
		TaskID: holder.ID, Outcome: ConflictAlertBlocked,
	}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("after a failed attempt, the announcement = %v, %v", ok, err)
	}
}
