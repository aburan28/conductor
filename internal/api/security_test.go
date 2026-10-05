package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// Regression tests for the security audit: account takeover through invites, lease and
// transition authority, private data on read paths, runner identity, and token handling.

// member adds a principal to the harness project (or creates one with no membership when role
// is empty) and returns it with a token.
func (h *harness) member(handle string, role domain.Role) (domain.Principal, string) {
	h.t.Helper()
	ctx := context.Background()
	p, err := h.store.CreatePrincipal(ctx, h.org.ID, domain.PrincipalHuman, handle, handle, "")
	if err != nil {
		h.t.Fatalf("principal %s: %v", handle, err)
	}
	if role != "" {
		if err := h.store.AddMember(ctx, h.project.ID, p.ID, role); err != nil {
			h.t.Fatalf("membership %s: %v", handle, err)
		}
	}
	tok, err := h.store.CreateToken(ctx, p.ID, "test", 0)
	if err != nil {
		h.t.Fatalf("token %s: %v", handle, err)
	}
	return p, tok
}

// secondProject creates another project in the same organization.
func (h *harness) secondProject(slug string) domain.Project {
	h.t.Helper()
	p, err := h.store.CreateProject(context.Background(), db.CreateProjectParams{
		OrganizationID: h.org.ID, Slug: uniq(slug, time.Now().UnixNano()),
		Config: domain.DefaultProjectConfig(),
	})
	if err != nil {
		h.t.Fatalf("project: %v", err)
	}
	return p
}

func (h *harness) startWork(token string, body map[string]any) coord.StartWorkResult {
	h.t.Helper()
	code, out := h.do(token, http.MethodPost, h.projectPath("/work/start"), body)
	if code != http.StatusOK {
		h.t.Fatalf("start work = %d\n%s", code, out)
	}
	var started coord.StartWorkResult
	if err := json.Unmarshal(out, &started); err != nil {
		h.t.Fatalf("decode: %v", err)
	}
	return started
}

func (h *harness) auditActions(target string) []string {
	h.t.Helper()
	rows, err := h.store.Pool().Query(context.Background(),
		`SELECT action FROM audit_log WHERE target_id = $1 ORDER BY id`, target)
	if err != nil {
		h.t.Fatalf("audit: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		out = append(out, a)
	}
	return out
}

// ---------------------------------------------------------------------------
// C1: invites
// ---------------------------------------------------------------------------

// An invite for a handle that already has an account must not hand the inviter a credential
// for it: tokens work in every project the account belongs to, so that was a takeover of
// any account in the organization by any project admin.
func TestInviteNeverMintsATokenForAnExistingAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Carol is an org_admin elsewhere in the organization, and not a member here.
	carol, _ := h.member("carol", "")
	other := h.secondProject("carol-proj")
	if err := h.store.AddMember(ctx, other.ID, carol.ID, domain.RoleOrgAdmin); err != nil {
		t.Fatal(err)
	}
	before, _ := h.store.ListTokens(ctx, carol.ID)

	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/members"),
		map[string]any{"handle": "carol", "role": "contributor"})
	if code != http.StatusCreated {
		t.Fatalf("invite existing = %d\n%s", code, body)
	}
	var result inviteMemberResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Token != "" || strings.Contains(string(body), db.TokenPrefix) {
		t.Fatalf("an invite minted a token for an existing account:\n%s", body)
	}
	if !result.ExistingPrincipal || result.Created || result.Note == "" {
		t.Errorf("invite result should say the account already existed: %+v", result)
	}
	after, _ := h.store.ListTokens(ctx, carol.ID)
	if len(after) != len(before) {
		t.Errorf("carol has %d tokens after the invite, had %d", len(after), len(before))
	}
	if role, err := h.store.RoleIn(ctx, h.project.ID, carol.ID); err != nil || role != domain.RoleContributor {
		t.Errorf("carol's membership = %s, %v; want contributor", role, err)
	}
}

// Re-inviting a member is not a way to change their role.
func TestInviteDoesNotChangeAnExistingMembersRole(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/members"),
		map[string]any{"handle": "bob", "role": "observer"})
	if code != http.StatusConflict {
		t.Errorf("re-invite = %d, want 409\n%s", code, body)
	}
	if role, _ := h.store.RoleIn(context.Background(), h.project.ID, h.bob.ID); role != domain.RoleContributor {
		t.Errorf("bob's role became %s through an invite", role)
	}
	if strings.Contains(string(body), db.TokenPrefix) {
		t.Errorf("a refused re-invite returned a token:\n%s", body)
	}
}

// A brand-new account still gets its first token, and the mint is audited.
func TestInviteOfANewAccountMintsAnAuditedToken(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/members"),
		map[string]any{"handle": "newbie", "role": "contributor"})
	if code != http.StatusCreated {
		t.Fatalf("invite = %d\n%s", code, body)
	}
	var result inviteMemberResult
	_ = json.Unmarshal(body, &result)
	if result.Token == "" || !result.Created || result.ExpiresAt == "" {
		t.Fatalf("new account invite = %+v", result)
	}
	p, err := h.store.GetPrincipalByHandle(context.Background(), h.org.ID, "newbie")
	if err != nil {
		t.Fatal(err)
	}
	actions := strings.Join(h.auditActions(p.ID), ",")
	if !strings.Contains(actions, "token.minted_by_invite") || !strings.Contains(actions, "member.invited") {
		t.Errorf("audit for the invite = %s", actions)
	}
}

// Role changes have their own endpoint and their own rules.
func TestMemberRoleChangeRules(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	path := func(handle string) string { return h.projectPath("/members/" + handle) }

	// A contributor cannot change roles at all.
	if code, _ := h.do(h.bobTok, http.MethodPatch, path("bob"), map[string]any{"role": "maintainer"}); code != http.StatusForbidden {
		t.Errorf("contributor self-promotion = %d, want 403", code)
	}
	// An admin can, and it is audited.
	code, body := h.do(h.aliceTok, http.MethodPatch, path("bob"), map[string]any{"role": "maintainer"})
	if code != http.StatusOK {
		t.Fatalf("promote = %d\n%s", code, body)
	}
	if role, _ := h.store.RoleIn(ctx, h.project.ID, h.bob.ID); role != domain.RoleMaintainer {
		t.Errorf("bob = %s, want maintainer", role)
	}
	if !strings.Contains(strings.Join(h.auditActions(h.bob.ID), ","), "member.role_changed") {
		t.Error("role change was not audited")
	}
	// Never above the caller.
	if code, _ := h.do(h.aliceTok, http.MethodPatch, path("bob"), map[string]any{"role": "org_admin"}); code != http.StatusForbidden {
		t.Errorf("project_admin granting org_admin = %d, want 403", code)
	}
	// Never on someone who outranks the caller.
	boss, _ := h.member("boss", domain.RoleOrgAdmin)
	if code, _ := h.do(h.aliceTok, http.MethodPatch, path("boss"), map[string]any{"role": "observer"}); code != http.StatusForbidden {
		t.Errorf("project_admin demoting an org_admin = %d, want 403", code)
	}
	if code, _ := h.do(h.aliceTok, http.MethodDelete, path("boss"), nil); code != http.StatusForbidden {
		t.Errorf("project_admin removing an org_admin = %d, want 403", code)
	}
	if role, _ := h.store.RoleIn(ctx, h.project.ID, boss.ID); role != domain.RoleOrgAdmin {
		t.Errorf("boss = %s, want org_admin untouched", role)
	}
	// Never the last administrator. Remove boss's admin weight first so alice is alone.
	if err := h.store.SetMemberRole(ctx, h.project.ID, boss.ID, domain.RoleObserver); err != nil {
		t.Fatal(err)
	}
	if code, body := h.do(h.aliceTok, http.MethodPatch, path("alice"), map[string]any{"role": "contributor"}); code != http.StatusForbidden {
		t.Errorf("demoting the last admin = %d, want 403\n%s", code, body)
	}
	// Not a member: 404.
	h.member("stranger", "")
	if code, _ := h.do(h.aliceTok, http.MethodPatch, path("stranger"), map[string]any{"role": "observer"}); code != http.StatusNotFound {
		t.Errorf("role change for a non-member = %d, want 404", code)
	}
}

// ---------------------------------------------------------------------------
// H3: lease and transition authority
// ---------------------------------------------------------------------------

// The server used to fill in the live lease for whoever asked, so any contributor could
// release, re-scope, or hand off a teammate's running work by omitting lease_id.
func TestOnlyTheHolderOrAMaintainerActsOnALease(t *testing.T) {
	h := newHarness(t)
	started := h.startWork(h.bobTok, map[string]any{"summary": "bob's running work", "title": "Bob's work"})
	mallory, malTok := h.member("mallory", domain.RoleContributor)
	_ = mallory
	task := "/v1/tasks/" + started.TaskID
	fence := map[string]any{"lease_id": started.LeaseID, "attempt_id": started.AttemptID,
		"fencing_epoch": started.FencingEpoch, "task_id": started.TaskID}

	for name, req := range map[string]struct {
		path string
		body map[string]any
	}{
		"release, no lease named":    {task + "/release", map[string]any{"reason": "mine now"}},
		"release, lease named":       {task + "/release", fence},
		"expand scope":               {task + "/scopes", map[string]any{"scopes": []map[string]any{{"resource": "dir:x", "mode": "write_exclusive"}}}},
		"handoff":                    {task + "/handoff", map[string]any{"to_harness": "codex"}},
		"progress with a full fence": {"/v1/attempts/" + started.AttemptID + "/progress", fence},
		"finish with a full fence":   {"/v1/attempts/" + started.AttemptID + "/result", mergeMaps(fence, map[string]any{"outcome": "cancelled"})},
		"lease heartbeat":            {"/v1/leases/heartbeat", fence},
	} {
		if code, body := h.do(malTok, http.MethodPost, req.path, req.body); code != http.StatusForbidden {
			t.Errorf("%s by a non-holder = %d, want 403\n%s", name, code, body)
		}
	}
	// The lease is still bob's.
	if lease, err := h.store.ActiveLeaseForTask(context.Background(), started.TaskID); err != nil || lease.ID != started.LeaseID {
		t.Fatalf("the lease changed hands: %+v, %v", lease, err)
	}

	// A maintainer may take back a stuck task, and that is on the record.
	_, maintTok := h.member("maint", domain.RoleMaintainer)
	if code, body := h.do(maintTok, http.MethodPost, task+"/release", map[string]any{"reason": "stuck"}); code != http.StatusOK {
		t.Fatalf("maintainer release = %d\n%s", code, body)
	}
	if !strings.Contains(strings.Join(h.auditActions(started.TaskID), ","), "lease.released_by_other") {
		t.Error("a maintainer releasing someone's lease was not audited")
	}
}

// The holder keeps working exactly as before, with or without naming the lease.
func TestTheHolderStillReleasesWithoutNamingTheLease(t *testing.T) {
	h := newHarness(t)
	started := h.startWork(h.bobTok, map[string]any{"summary": "release me", "title": "Release me"})
	if code, body := h.do(h.bobTok, http.MethodPost, "/v1/tasks/"+started.TaskID+"/release",
		map[string]any{"reason": "done for today"}); code != http.StatusOK {
		t.Fatalf("holder release = %d\n%s", code, body)
	}
}

func mergeMaps(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// Moving a task by hand: its creator or lease holder for ordinary moves, a reviewer or
// maintainer for accepting verified work, and nobody else.
func TestTransitionAuthority(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, malTok := h.member("mallory", domain.RoleContributor)
	_, revTok := h.member("rita", domain.RoleReviewer)

	// Alice's task: mallory cannot cancel it.
	alices := h.createTask(t, h.aliceTok, "Alice's task")
	tr := func(tok, id, to string) (int, []byte) {
		return h.do(tok, http.MethodPost, "/v1/tasks/"+id+"/transition", map[string]any{"status": to})
	}
	if code, body := tr(malTok, alices.ID, "cancelled"); code != http.StatusForbidden {
		t.Errorf("cancel someone else's task = %d, want 403\n%s", code, body)
	}
	// A reviewer cannot make ordinary moves.
	if code, _ := tr(revTok, alices.ID, "cancelled"); code != http.StatusForbidden {
		t.Errorf("reviewer cancelling = %d, want 403", code)
	}

	// Bob's own task: he can make ordinary moves...
	bobs := h.createTask(t, h.bobTok, "Bob's task")
	if code, body := tr(h.bobTok, bobs.ID, "ready"); code != http.StatusOK && code != http.StatusConflict {
		t.Errorf("creator move = %d\n%s", code, body)
	}
	// ...but cannot accept his own verified work.
	for _, st := range []domain.TaskStatus{domain.TaskReady, domain.TaskClaimed, domain.TaskRunning, domain.TaskVerifying} {
		if _, err := h.store.UpdateTaskStatus(ctx, bobs.ID, st); err != nil && !errors.Is(err, domain.ErrIllegalTransition) {
			t.Fatalf("advance to %s: %v", st, err)
		}
	}
	if code, body := tr(h.bobTok, bobs.ID, "done"); code != http.StatusForbidden {
		t.Errorf("author marking own work done = %d, want 403\n%s", code, body)
	}
	// A reviewer can, and the dashboard's {"to": …} body works too.
	code, body := h.do(revTok, http.MethodPost, "/v1/tasks/"+bobs.ID+"/transition", map[string]any{"to": "done"})
	if code != http.StatusOK {
		t.Errorf("reviewer accepting work = %d\n%s", code, body)
	}
	if !strings.Contains(strings.Join(h.auditActions(bobs.ID), ","), "task.transitioned_by_other") {
		t.Error("a review decision on someone else's task was not audited")
	}
}

// The card tells everyone who holds the task and until when; only the holder sees the lease
// identifiers that act on it.
func TestTaskCardShowsLeaseIdentifiersOnlyToTheHolder(t *testing.T) {
	h := newHarness(t)
	started := h.startWork(h.bobTok, map[string]any{"summary": "card lease", "title": "Card lease"})
	card := func(tok string) string {
		code, body := h.do(tok, http.MethodGet, "/v1/tasks/"+started.TaskID+"/card", nil)
		if code != http.StatusOK {
			t.Fatalf("card = %d\n%s", code, body)
		}
		return string(body)
	}
	mine, theirs := card(h.bobTok), card(h.aliceTok)
	if !strings.Contains(mine, started.LeaseID) {
		t.Error("the holder's card lacks the lease id")
	}
	if strings.Contains(theirs, started.LeaseID) || strings.Contains(theirs, "fencing_epoch") {
		t.Errorf("another member's card carries lease identifiers:\n%s", theirs)
	}
	if !strings.Contains(theirs, "holder: bob") || !strings.Contains(theirs, "expires_at") {
		t.Errorf("another member's card lost who holds the task and until when:\n%s", theirs)
	}
}

// ---------------------------------------------------------------------------
// H4: private task content on read paths
// ---------------------------------------------------------------------------

// privateMarkers is every string the private task's owner wrote that must not reach anyone
// else through any read path.
var privateMarkers = []string{
	"ZQXTITLE", "ZQXOBJECTIVE", "ZQXCRITERION", "ZQXINTENT", "ZQXSUMMARY", "ZQXBLOCKER",
	"ZQXHANDOFFWORK", "ZQXDECISION", "ZQXQUESTION", "ZQXPATH", "ZQXWORKTREE",
}

// TestNoGETRouteLeaksAPrivateTask builds a private task with an attempt, progress, a handoff,
// a session, and a queue ticket, then reads every authenticated GET route in this package as
// another contributor and as an observer, asserting none of the owner's words come back. The
// route list is checked against the source, so a new GET route fails this test until it is
// added here.
func TestNoGETRouteLeaksAPrivateTask(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)
	ctx := context.Background()
	_, obsTok := h.member("olive", domain.RoleObserver)

	// The private work.
	started := h.startWork(h.aliceTok, map[string]any{
		"summary": "ZQXINTENT rework billing", "title": "ZQXTITLE billing rework",
		"visibility":          "private",
		"acceptance_criteria": []map[string]any{{"text": "ZQXCRITERION holds"}},
		"scopes":              []map[string]any{{"resource": "dir:internal/billing", "mode": "write_exclusive"}},
	})
	if _, err := h.store.PatchTask(ctx, started.TaskID, db.TaskPatch{Objective: ptr("ZQXOBJECTIVE explained")}); err != nil {
		t.Fatal(err)
	}
	fence := map[string]any{"task_id": started.TaskID, "lease_id": started.LeaseID,
		"attempt_id": started.AttemptID, "fencing_epoch": started.FencingEpoch}
	if code, body := h.do(h.aliceTok, http.MethodPost, "/v1/attempts/"+started.AttemptID+"/progress",
		mergeMaps(fence, map[string]any{"phase": "implementing", "summary": "ZQXSUMMARY so far",
			"blocker": "ZQXBLOCKER waiting", "tokens_in": 10, "cost_usd": 0.5,
			"changed_paths": []string{"internal/billing/ZQXPATH.go"}})); code != http.StatusAccepted {
		t.Fatalf("progress = %d\n%s", code, body)
	}
	if _, err := h.store.UpdateAttempt(ctx, started.AttemptID, db.AttemptProgress{
		State: domain.AttemptRunning, WorktreePath: "/work/ZQXWORKTREE"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.RecordDecision(ctx, domain.PolicyDecision{ProjectID: h.project.ID, TaskID: started.TaskID,
		AttemptID: started.AttemptID, Kind: "route", Decision: "routed",
		Rationale: map[string]any{"why": "ZQXDECISION"}}); err != nil {
		t.Fatal(err)
	}
	session := h.registerSession(t, h.aliceTok, nil)
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/queue"),
		map[string]any{"kind": "session", "session_id": session.ID})
	if code != http.StatusCreated {
		t.Fatalf("enqueue = %d\n%s", code, body)
	}
	var ticket domain.AdmissionTicket
	_ = json.Unmarshal(body, &ticket)
	if code, body := h.do(h.aliceTok, http.MethodPost, "/v1/tasks/"+started.TaskID+"/handoff", map[string]any{
		"to_harness": "codex", "bundle": map[string]any{
			"completed_work": []string{"ZQXHANDOFFWORK"}, "open_questions": []string{"ZQXQUESTION"}},
	}); code != http.StatusCreated {
		t.Fatalf("handoff = %d\n%s", code, body)
	}

	// The owner sees their own words — otherwise this test proves nothing.
	if _, body := h.do(h.aliceTok, http.MethodGet, "/v1/tasks/"+started.TaskID+"/handoff", nil); !strings.Contains(string(body), "ZQXHANDOFFWORK") {
		t.Fatalf("the owner cannot see their own handoff:\n%s", body)
	}
	if _, body := h.do(h.aliceTok, http.MethodGet, "/v1/tasks/"+started.TaskID+"/attempts", nil); !strings.Contains(string(body), "ZQXPATH") || !strings.Contains(string(body), "ZQXWORKTREE") {
		t.Fatalf("the owner cannot see their own attempt's paths:\n%s", body)
	}

	subst := map[string]string{
		"{project}": h.project.ID, "{task}": started.TaskID, "{attempt}": started.AttemptID,
		"{session}": session.ID, "{ticket}": ticket.ID,
	}
	routes := []string{
		"/v1/whoami", "/v1/projects", "/v1/projects/{project}", "/v1/projects/{project}/members",
		"/v1/projects/{project}/budget", "/v1/projects/{project}/budget/grants",
		"/v1/projects/{project}/capabilities", "/v1/projects/{project}/conflicts?all=true",
		"/v1/projects/{project}/events", "/v1/projects/{project}/models",
		"/v1/projects/{project}/presence", "/v1/projects/{project}/queue?closed=true",
		"/v1/projects/{project}/reservations", "/v1/projects/{project}/runner/snapshot",
		"/v1/projects/{project}/runners", "/v1/projects/{project}/sessions",
		"/v1/projects/{project}/status", "/v1/projects/{project}/swarm",
		"/v1/projects/{project}/tasks", "/v1/projects/{project}/usage",
		"/v1/queue/{ticket}", "/v1/sessions/{session}/assignments?all=true",
		"/v1/tasks/{task}", "/v1/tasks/{task}/attempts", "/v1/tasks/{task}/card",
		"/v1/tasks/{task}/decisions", "/v1/tasks/{task}/handoff", "/v1/tasks/{task}/route/explain",
		"/v1/tasks/{task}/validation", "/v1/attempts/{attempt}/brief",
		"/v1/tokens", "/v1/peers", "/v1/security", "/v1/github/status",
		"/v1/ready", "/metrics", "/v1/quota", "/v1/projects/{project}/quota",
		"/v1/projects/{project}/notifications",
		// The stream is read separately below; it never ends on its own.
	}
	assertRoutesCovered(t, append(routes, "/v1/projects/{project}/events/stream"))

	for _, viewer := range []struct{ name, tok string }{{"contributor", h.bobTok}, {"observer", obsTok}} {
		for _, route := range routes {
			path := route
			for k, v := range subst {
				path = strings.ReplaceAll(path, k, v)
			}
			code, body := h.do(viewer.tok, http.MethodGet, path, nil)
			if code == http.StatusInternalServerError {
				t.Errorf("%s GET %s = %d\n%s", viewer.name, route, code, body)
			}
			for _, marker := range privateMarkers {
				if strings.Contains(string(body), marker) {
					t.Errorf("%s GET %s leaks %s:\n%s", viewer.name, route, marker, body)
				}
			}
		}
		if leaked := readStream(t, h, viewer.tok); leaked != "" {
			t.Errorf("%s event stream leaks %s", viewer.name, leaked)
		}
	}
}

// readStream reads the event stream (which replays the last two minutes) for a moment and
// reports the first private marker it carries.
func readStream(t *testing.T, h *harness, token string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.server.URL+h.projectPath("/events/stream"), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body) // ends when the context does
	if !strings.Contains(string(body), "event: ") {
		t.Fatalf("the stream delivered nothing; the replay should include this test's events")
	}
	for _, marker := range privateMarkers {
		if strings.Contains(string(body), marker) {
			return marker
		}
	}
	return ""
}

// assertRoutesCovered fails when the package registers an authenticated GET route the walk
// does not visit, so a new read path cannot skip the privacy check by being new.
func assertRoutesCovered(t *testing.T, walked []string) {
	t.Helper()
	exempt := map[string]bool{
		"/":                 true, // the dashboard's static files
		"/v1/health":        true, // unauthenticated, no project data
		"/v1/local/status":  true, // unauthenticated, no project data
		"/v1/peer/info":     true, // mesh-certificate authenticated, no project data
		"/github/setup":     true, // GitHub App installation pages
		"/github/callback":  true,
		"/github/installed": true,
		"/mcp":              true, // GET is always 405: the gateway opens no server stream
		"/mcp/{project}":    true,
	}
	have := map[string]bool{}
	for _, w := range walked {
		path, _, _ := strings.Cut(w, "?")
		have[path] = true
	}
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`"GET (/[^"]*)"`)
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range pattern.FindAllStringSubmatch(string(src), -1) {
			if !exempt[m[1]] && !have[m[1]] {
				t.Errorf("GET %s (%s) is not covered by the private-task walk", m[1], f.Name())
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }

// Events about a private task show its territory and nothing else to other members, and an
// attempt's spend stays with its sponsor whatever the task's visibility.
func TestEventsFollowTheTasksVisibility(t *testing.T) {
	h := newHarness(t)
	private := h.startWork(h.aliceTok, map[string]any{"summary": "secret", "title": "Secret",
		"visibility": "private"})
	public := h.startWork(h.aliceTok, map[string]any{"summary": "open work", "title": "Open work"})
	for _, s := range []coord.StartWorkResult{private, public} {
		code, body := h.do(h.aliceTok, http.MethodPost, "/v1/attempts/"+s.AttemptID+"/progress", map[string]any{
			"task_id": s.TaskID, "lease_id": s.LeaseID, "attempt_id": s.AttemptID,
			"fencing_epoch": s.FencingEpoch, "phase": "implementing",
			"summary": "progress note", "changed_paths": []string{"internal/billing/fraud.go"},
			"tokens_in": 1234, "cost_usd": 9.75,
		})
		if code != http.StatusAccepted {
			t.Fatalf("progress = %d\n%s", code, body)
		}
	}
	events := func(tok string) []domain.Event {
		code, body := h.do(tok, http.MethodGet, h.projectPath("/events?limit=200"), nil)
		if code != http.StatusOK {
			t.Fatalf("events = %d", code)
		}
		var out struct {
			Events []domain.Event `json:"events"`
		}
		_ = json.Unmarshal(body, &out)
		return out.Events
	}
	progressFor := func(evs []domain.Event, ref string) domain.Event {
		for _, e := range evs {
			if e.Type == "attempt.progress" && e.Payload["task_ref"] == ref {
				return e
			}
		}
		t.Fatalf("no progress event for %s", ref)
		return domain.Event{}
	}

	bobs := events(h.bobTok)
	p := progressFor(bobs, private.TaskRef)
	for _, k := range []string{"summary", "blocker", "changed_paths", "tokens_in", "cost_usd"} {
		if _, ok := p.Payload[k]; ok {
			t.Errorf("a private task's progress shows %q to another member: %v", k, p.Payload)
		}
	}
	if p.Payload["phase"] != "implementing" {
		t.Errorf("territory-level fields were lost: %v", p.Payload)
	}
	q := progressFor(bobs, public.TaskRef)
	if q.Payload["summary"] != "progress note" {
		t.Errorf("a team task's summary was hidden from the team: %v", q.Payload)
	}
	if _, ok := q.Payload["cost_usd"]; ok {
		t.Errorf("an attempt's spend reached someone other than its sponsor: %v", q.Payload)
	}

	mine := progressFor(events(h.aliceTok), private.TaskRef)
	if mine.Payload["summary"] != "progress note" || mine.Payload["cost_usd"] == nil {
		t.Errorf("the owner lost their own event detail: %v", mine.Payload)
	}
}

// ---------------------------------------------------------------------------
// M4/M5: runners and tokens
// ---------------------------------------------------------------------------

// A runner's liveness and load are its owner's to report. Anyone else — in this organization
// or another — used to be able to hold it online or mark it full.
func TestRunnerHeartbeatBelongsToItsPrincipal(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/runners/register", map[string]any{
		"name": "alice-laptop-" + h.project.Slug, "project_id": h.project.ID,
	})
	if code != http.StatusOK {
		t.Fatalf("register = %d\n%s", code, body)
	}
	var runner domain.Runner
	_ = json.Unmarshal(body, &runner)
	beat := func(tok string) int {
		code, _ := h.do(tok, http.MethodPost, "/v1/runners/"+runner.ID+"/heartbeat", map[string]any{"in_flight": 99})
		return code
	}
	if code := beat(h.bobTok); code != http.StatusNotFound {
		t.Errorf("another member's heartbeat = %d, want 404", code)
	}
	if code := beat(h.outTok); code != http.StatusNotFound {
		t.Errorf("a non-member's heartbeat = %d, want 404", code)
	}
	if code := beat(h.aliceTok); code != http.StatusOK {
		t.Errorf("owner heartbeat = %d, want 200", code)
	}
	// Nor can someone else take the record over by registering its name.
	if code, body := h.do(h.bobTok, http.MethodPost, "/v1/runners/register", map[string]any{
		"name": runner.Name, "project_id": h.project.ID,
	}); code != http.StatusConflict {
		t.Errorf("registering another principal's runner name = %d, want 409\n%s", code, body)
	}
	if !strings.Contains(strings.Join(h.auditActions(runner.ID), ","), "runner.registered") {
		t.Error("runner registration was not audited")
	}
}

// The runner role is usable for execution — claiming, reporting, finishing — without a
// contributor's right to file and move other people's tasks.
func TestRunnerRoleExecutesWithoutContributorRights(t *testing.T) {
	h := newHarness(t)
	_, runTok := h.member("ci-runner", domain.RoleRunner)
	if code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/tasks"),
		map[string]any{"title": "Queued for a runner", "status": "ready"}); code != http.StatusCreated {
		t.Fatalf("create = %d\n%s", code, body)
	}

	code, body := h.do(runTok, http.MethodGet, h.projectPath("/runner/snapshot"), nil)
	if code != http.StatusOK {
		t.Fatalf("runner snapshot = %d\n%s", code, body)
	}
	code, body = h.do(runTok, http.MethodPost, h.projectPath("/claim-next"), map[string]any{"harness": "fake"})
	if code != http.StatusOK {
		t.Fatalf("runner claim-next = %d\n%s", code, body)
	}
	var claim struct {
		Fence domain.Fence `json:"fence"`
	}
	_ = json.Unmarshal(body, &claim)
	fence := map[string]any{"task_id": claim.Fence.TaskID, "lease_id": claim.Fence.LeaseID,
		"attempt_id": claim.Fence.AttemptID, "fencing_epoch": claim.Fence.FencingEpoch}
	if code, body := h.do(runTok, http.MethodGet, "/v1/attempts/"+claim.Fence.AttemptID+"/brief", nil); code != http.StatusOK {
		t.Errorf("runner brief = %d\n%s", code, body)
	}
	if code, body := h.do(runTok, http.MethodPost, "/v1/attempts/"+claim.Fence.AttemptID+"/progress",
		mergeMaps(fence, map[string]any{"phase": "implementing"})); code != http.StatusAccepted {
		t.Errorf("runner progress = %d\n%s", code, body)
	}
	if code, body := h.do(runTok, http.MethodPost, "/v1/tasks/"+claim.Fence.TaskID+"/release",
		map[string]any{"reason": "runner error", "attempt_state": "failed_retryable"}); code != http.StatusOK {
		t.Errorf("runner release = %d\n%s", code, body)
	}

	// What a runner may not do.
	if code, _ := h.do(runTok, http.MethodPost, h.projectPath("/tasks"), map[string]any{"title": "x"}); code != http.StatusForbidden {
		t.Errorf("runner creating a task = %d, want 403", code)
	}
	if code, _ := h.do(runTok, http.MethodPost, "/v1/tasks/"+claim.Fence.TaskID+"/transition",
		map[string]any{"status": "cancelled"}); code != http.StatusForbidden {
		t.Errorf("runner transitioning a task = %d, want 403", code)
	}
}

// A project-scoped token is a stranger to every other project, and cannot mint or revoke.
func TestProjectScopedTokens(t *testing.T) {
	h := newHarness(t)
	other := h.secondProject("scoped-other")
	if err := h.store.AddMember(context.Background(), other.ID, h.alice.ID, domain.RoleProjectAdmin); err != nil {
		t.Fatal(err)
	}
	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/tokens",
		map[string]any{"name": "attempt:x", "ttl": "1h", "project": h.project.Slug})
	if code != http.StatusCreated {
		t.Fatalf("scoped mint = %d\n%s", code, body)
	}
	var minted struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &minted)

	if code, _ := h.do(minted.Token, http.MethodGet, h.projectPath("/tasks"), nil); code != http.StatusOK {
		t.Errorf("scoped token on its project = %d, want 200", code)
	}
	if code, _ := h.do(minted.Token, http.MethodGet, "/v1/projects/"+other.ID+"/tasks", nil); code != http.StatusNotFound {
		t.Errorf("scoped token on another project = %d, want 404", code)
	}
	if code, _ := h.do(h.aliceTok, http.MethodGet, "/v1/projects/"+other.ID+"/tasks", nil); code != http.StatusOK {
		t.Errorf("alice's own token on the other project = %d, want 200", code)
	}
	for _, req := range []struct{ method, path string }{
		{http.MethodPost, "/v1/tokens"}, {http.MethodPost, "/v1/tokens/reset"},
		{http.MethodPost, "/v1/tokens/revoke-all"}, {http.MethodDelete, "/v1/tokens/test"},
	} {
		if code, _ := h.do(minted.Token, req.method, req.path, map[string]any{}); code != http.StatusForbidden {
			t.Errorf("scoped token %s %s = %d, want 403", req.method, req.path, code)
		}
	}
	// The mint is audited.
	if !strings.Contains(strings.Join(h.auditActions("attempt:x"), ","), "token.created") {
		t.Error("token creation was not audited")
	}
}

// A token minted without a lifetime gets the human default, and a human cannot exceed it.
func TestCreatedTokensExpireByDefault(t *testing.T) {
	h := newHarness(t)
	for _, ttl := range []string{"0s", "8760h"} {
		code, body := h.do(h.aliceTok, http.MethodPost, "/v1/tokens", map[string]any{"name": "t-" + ttl, "ttl": ttl})
		if code != http.StatusCreated {
			t.Fatalf("mint = %d\n%s", code, body)
		}
		var out struct {
			ExpiresAt time.Time `json:"expires_at"`
		}
		_ = json.Unmarshal(body, &out)
		if out.ExpiresAt.IsZero() || out.ExpiresAt.After(time.Now().Add(tokenTTLDefault+time.Hour)) {
			t.Errorf("ttl %s: expires %v, want at most the 90-day default", ttl, out.ExpiresAt)
		}
	}
	tokens, _ := h.store.ListTokens(context.Background(), h.alice.ID)
	for _, tk := range tokens {
		if strings.HasPrefix(tk.Name, "t-") && tk.ExpiresAt == nil {
			t.Errorf("token %s never expires", tk.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// Low: tokens in URLs, error bodies, request sizes
// ---------------------------------------------------------------------------

// ?token= is for the EventSource stream only; anywhere else it would put credentials in
// proxy logs and browser history.
func TestTokenInQueryStringOnlyOnTheStream(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do("", http.MethodGet, h.projectPath("/tasks")+"?token="+h.aliceTok, nil); code != http.StatusUnauthorized {
		t.Errorf("?token= on an ordinary GET = %d, want 401", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		h.server.URL+h.projectPath("/events/stream")+"?token="+h.aliceTok, nil)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("?token= on the stream = %d, want 200", resp.StatusCode)
	}
}

// A server fault must not hand the client the database's own words.
func TestServerErrorsAreGenericWithARequestID(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()
	s.fail(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil),
		errors.New(`ERROR: relation "api_tokens" violates constraint at host db.internal (SQLSTATE 23505)`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	var body ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if strings.Contains(rec.Body.String(), "api_tokens") || strings.Contains(rec.Body.String(), "db.internal") {
		t.Errorf("the 500 body carries the raw error: %s", rec.Body.String())
	}
	if body.RequestID == "" || rec.Header().Get("X-Request-Id") != body.RequestID {
		t.Errorf("no request id to correlate with the log: %+v", body)
	}

	// Client errors keep their explanation.
	rec = httptest.NewRecorder()
	s.fail(rec, httptest.NewRequest(http.MethodGet, "/v1/x", nil),
		errors.Join(domain.ErrInvalidArgument, errors.New("handle is required")))
	if !strings.Contains(rec.Body.String(), "handle is required") {
		t.Errorf("a 400 lost its message: %s", rec.Body.String())
	}
}

func TestUsageUploadsAreBounded(t *testing.T) {
	h := newHarness(t)
	big := `{"buckets":[` + strings.Repeat(`{"harness":"claude","external_session_id":"x","start":"2026-01-01T00:00:00Z"},`, 120000) + `{}]}`
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+h.projectPath("/usage"), strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+h.aliceTok)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized usage upload = %d, want 413", resp.StatusCode)
	}
}

// A project-scoped token reaches its own project and nothing else: not its owner's sessions in
// another project, not the machine's security mode or GitHub App, not other projects in a
// listing.
func TestScopedTokenStaysInItsProject(t *testing.T) {
	h := newHarness(t)
	other := h.secondProject("scope-sessions")
	if err := h.store.AddMember(context.Background(), other.ID, h.alice.ID, domain.RoleProjectAdmin); err != nil {
		t.Fatal(err)
	}
	// Alice's session in the other project, registered with her own login.
	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/projects/"+other.ID+"/sessions",
		map[string]any{"harness": "claude"})
	if code != http.StatusCreated {
		t.Fatalf("register = %d\n%s", code, body)
	}
	var elsewhere domain.Session
	_ = json.Unmarshal(body, &elsewhere)
	here := h.registerSession(t, h.aliceTok, nil)

	scoped, err := h.store.CreateScopedToken(context.Background(), h.alice.ID, h.project.ID, "attempt:y", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/heartbeat", "/close", "/capabilities"} {
		if code, body := h.do(scoped, http.MethodPost, "/v1/sessions/"+elsewhere.ID+path, map[string]any{}); code != http.StatusNotFound {
			t.Errorf("scoped token %s on a session in another project = %d, want 404\n%s", path, code, body)
		}
	}
	if code, body := h.do(scoped, http.MethodPost, "/v1/sessions/"+here.ID+"/heartbeat", map[string]any{}); code != http.StatusOK {
		t.Errorf("scoped token heartbeat in its own project = %d\n%s", code, body)
	}
	if code, _ := h.do(scoped, http.MethodPost, "/v1/projects/"+other.ID+"/work/start",
		map[string]any{"summary": "escape"}); code != http.StatusNotFound {
		t.Errorf("scoped token starting work in another project = %d, want 404", code)
	}
	if code, _ := h.do(scoped, http.MethodPost, "/v1/security", map[string]any{"security_mode": "enhanced"}); code != http.StatusForbidden {
		t.Errorf("scoped token changing the security mode = %d, want 403", code)
	}
	// The GitHub App routes are mounted only when an app is configured; check the rule itself.
	for _, pattern := range []string{"POST /v1/github/setup", "POST /v1/github/check", "POST /v1/security",
		"POST /v1/tokens", "POST /v1/tokens/reset", "DELETE /v1/tokens/{name}"} {
		if scopedRouteAllowed(pattern) {
			t.Errorf("a scoped token may reach %s", pattern)
		}
	}
	code, body = h.do(scoped, http.MethodGet, "/v1/whoami", nil)
	if code != http.StatusOK || strings.Contains(string(body), other.ID) {
		t.Errorf("scoped whoami = %d and lists other projects:\n%s", code, body)
	}
	if !strings.Contains(string(body), h.project.ID) {
		t.Errorf("scoped whoami lost its own project:\n%s", body)
	}
}

// A task's content is its creator's, its lease holder's, or a maintainer's to edit; its
// visibility, its creator's or a maintainer's.
func TestTaskEditAuthority(t *testing.T) {
	h := newHarness(t)
	_, malTok := h.member("mallory", domain.RoleContributor)
	_, maintTok := h.member("maint", domain.RoleMaintainer)
	private := h.startWork(h.aliceTok, map[string]any{"summary": "mine", "title": "Mine",
		"visibility": "private"})
	path := "/v1/tasks/" + private.TaskID
	for name, body := range map[string]map[string]any{
		"title":               {"title": "defaced"},
		"objective":           {"objective": "defaced"},
		"acceptance criteria": {"acceptance_criteria": []map[string]any{{"text": "defaced"}}},
		"visibility":          {"visibility": "team_summary"},
	} {
		if code, _ := h.do(malTok, http.MethodPatch, path, body); code != http.StatusForbidden {
			t.Errorf("another contributor editing the %s = %d, want 403", name, code)
		}
	}
	task, _ := h.store.GetTask(context.Background(), private.TaskID)
	if task.Title != "Mine" || task.Visibility != domain.VisibilityPrivate {
		t.Fatalf("the task changed: %+v", task)
	}

	// The creator edits freely, criteria included.
	if code, body := h.do(h.aliceTok, http.MethodPatch, path, map[string]any{
		"title": "Mine, renamed", "acceptance_criteria": []map[string]any{{"text": "it works"}},
	}); code != http.StatusOK {
		t.Errorf("creator edit = %d\n%s", code, body)
	}

	// A lease holder who is not the creator may edit content but not publish the task.
	// Bob claims it; then hand the task's authorship to alice so bob is holder, not creator.
	bobs := h.startWork(h.bobTok, map[string]any{"summary": "bob holds", "title": "Bob holds"})
	if _, err := h.store.Pool().Exec(context.Background(),
		`UPDATE tasks SET created_by = $1::uuid, visibility = 'private' WHERE id = $2::uuid`, h.alice.ID, bobs.TaskID); err != nil {
		t.Fatal(err)
	}
	if code, body := h.do(h.bobTok, http.MethodPatch, "/v1/tasks/"+bobs.TaskID, map[string]any{"objective": "refined"}); code != http.StatusOK {
		t.Errorf("lease holder editing content = %d\n%s", code, body)
	}
	if code, _ := h.do(h.bobTok, http.MethodPatch, "/v1/tasks/"+bobs.TaskID, map[string]any{"visibility": "team_summary"}); code != http.StatusForbidden {
		t.Errorf("lease holder publishing someone else's private task = %d, want 403", code)
	}

	// A maintainer may, and it is on the record.
	if code, body := h.do(maintTok, http.MethodPatch, path, map[string]any{"visibility": "team_summary"}); code != http.StatusOK {
		t.Errorf("maintainer changing visibility = %d\n%s", code, body)
	}
	if !strings.Contains(strings.Join(h.auditActions(private.TaskID), ","), "task.edited_by_other") {
		t.Error("a maintainer editing someone else's task was not audited")
	}
}

// The transition rule is one exported helper, so every status-changing handler applies the
// same authority — and the control plane acting on its own (merge reconciliation) is not
// blocked by a rule meant to stop one principal acting on another's work.
func TestTransitionAuthorityIsSharedAndExemptsTheSystem(t *testing.T) {
	h := newHarness(t)
	svc := coord.New(h.store)
	task := h.createTask(t, h.bobTok, "Verified work")
	full, _ := h.store.GetTask(context.Background(), task.ID)
	full.Status = domain.TaskVerifying

	author := coord.Caller{Principal: h.bob, Role: domain.RoleContributor}
	if _, err := svc.AuthorizeTransition(context.Background(), author, full, domain.TaskDone); !errors.Is(err, domain.ErrNotPermitted) {
		t.Errorf("author accepting own work = %v, want ErrNotPermitted", err)
	}
	if _, err := svc.AuthorizeTransition(context.Background(), coord.SystemCaller(), full, domain.TaskDone); err != nil {
		t.Errorf("the system recording a merge = %v, want allowed", err)
	}
}
