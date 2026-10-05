package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/githubapp"
	"github.com/adamburan/conductor/internal/resource"
	"github.com/adamburan/conductor/internal/tracker"
)

// newIssueIntegration wires a configured GitHub App against the fake, with the harness's
// project linked to the fake repository, alice as the machine owner, and issue sync enabled
// with the default settings.
func newIssueIntegration(t *testing.T, h *harness, public bool) (*GitHub, *fakeGitHubAPI, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	fake := &fakeGitHubAPI{t: t, pem: testAppPEM(t), repo: "issues-" + h.project.Slug, public: public,
		issues: map[int]map[string]any{}}
	gh := httptest.NewServer(fake.handler())
	t.Cleanup(gh.Close)

	before, _ := h.store.GetServerSettings(ctx)
	t.Cleanup(func() {
		_, _ = h.store.Pool().Exec(context.Background(),
			`UPDATE server_settings SET local_owner_id = NULLIF($1,'')::uuid WHERE singleton`, before.LocalOwnerID)
	})
	if _, err := h.store.SetLocalOwner(ctx, h.alice.ID, false); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(t.TempDir(), "github-app.json")
	if err := githubapp.Save(creds, githubapp.Credentials{AppID: 1234, Slug: "conductor-test", WebhookSecret: "whsec",
		PrivateKeyPEM: fake.pem}); err != nil {
		t.Fatal(err)
	}
	integration, err := NewGitHub(GitHubOptions{CredentialsPath: creds, API: gh.URL, Web: "https://github.example", Poll: -1,
		Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(h.store, coord.New(h.store), Options{GitHub: integration}).Handler())
	t.Cleanup(srv.Close)
	if code, body := h.doJSONOn(srv, h.aliceTok, http.MethodPost, h.projectPath("/github"), map[string]string{"repository": "acme/" + fake.repo}); code != http.StatusOK {
		t.Fatalf("link = %d %v", code, body)
	}
	project, err := h.store.GetProject(ctx, h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.project = project
	return integration, fake, srv
}

func enableIssues(t *testing.T, h *harness, srv *httptest.Server, body map[string]any) map[string]any {
	t.Helper()
	code, out := h.doJSONOn(srv, h.aliceTok, http.MethodPost, h.projectPath("/github/issues"), body)
	if code != http.StatusOK || out["enabled"] != true {
		t.Fatalf("enable = %d %v", code, out)
	}
	return out
}

// issueTask is the task imported from issue n, and how many tasks name it.
func issueTask(t *testing.T, h *harness, fake *fakeGitHubAPI, n int) (domain.Task, int) {
	t.Helper()
	tasks, err := h.store.ListTasks(context.Background(), h.project.ID, db.ListTasksFilter{
		ExternalRef: "github:" + githubapp.IssueKey("acme", fake.repo, n)})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		return domain.Task{}, 0
	}
	return tasks[0], len(tasks)
}

func issueWebhook(t *testing.T, srv *httptest.Server, fake *fakeGitHubAPI, action string, n int) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"action": action, "issue": fake.issue(n), "installation": map[string]any{"id": 77},
		"repository": map[string]any{"name": fake.repo, "private": !fake.public, "owner": map[string]any{"login": "acme"}}})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/github/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", githubapp.Sign("whsec", body))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("issues %s webhook = %d %v", action, resp.StatusCode, out)
	}
	return out
}

func claimFor(t *testing.T, h *harness, task domain.Task, who domain.Principal) db.ClaimResult {
	t.Helper()
	claim, err := h.store.Claim(context.Background(), db.ClaimParams{TaskID: task.ID, ProjectID: h.project.ID,
		HolderPrincipal: who.ID, Harness: "test", LeaseTTL: time.Minute, ScopePolicy: resource.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func writesFor(fake *fakeGitHubAPI, n int) []string {
	var out []string
	tag := " " + strconv.Itoa(n)
	for _, w := range fake.writes() {
		if strings.Contains(w, tag+":") || strings.HasSuffix(w, tag) {
			out = append(out, w)
		}
	}
	return out
}

// Import, idempotency, the label filter, edits and their conflict rule, close and reopen, and
// both write-backs, through the poller and through webhooks.
func TestGitHubIssueSync(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	g, fake, srv := newIssueIntegration(t, h, false)

	fake.setIssue(1, "Retry storms", "Back off with jitter.\n\n## Acceptance\n- [ ] jitter\n- [ ] at most 5 tries", "open", "conductor", "bug")
	fake.setIssue(2, "Unlabelled", "Not for Conductor yet.", "open", "bug")
	fake.setIssue(3, "Closed already", "Done long ago.", "closed", "conductor")

	// Only a project admin turns it on.
	if code, _ := h.doJSONOn(srv, h.bobTok, http.MethodPost, h.projectPath("/github/issues"), map[string]any{}); code != http.StatusForbidden {
		t.Errorf("a contributor enabling issue sync = %d, want 403", code)
	}
	enableIssues(t, h, srv, map[string]any{})

	// --- Import through the poller: the labelled open issue only.
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	one, n := issueTask(t, h, fake, 1)
	if n != 1 || one.Title != "Retry storms" || one.Objective != "Back off with jitter." || one.Status != domain.TaskReady ||
		len(one.AcceptanceCriteria) != 2 || one.AcceptanceCriteria[1].Text != "at most 5 tries" {
		t.Fatalf("issue 1 imported as %d tasks: %+v", n, one)
	}
	if one.Visibility != h.project.Config.DefaultVisibility {
		t.Errorf("a private repository's issue got %s, want the project default %s", one.Visibility, h.project.Config.DefaultVisibility)
	}
	if _, n := issueTask(t, h, fake, 2); n != 0 {
		t.Error("an issue without the label was imported")
	}
	if _, n := issueTask(t, h, fake, 3); n != 0 {
		t.Error("a closed issue was imported")
	}

	// --- Idempotent: the poller again (once its resume point settles, an unchanged listing
	// is a 304), a manual sync, and a webhook for the same issue all find the one task.
	// (The second pass is the first to list closed issues too, which moves it once more.)
	for i := 0; i < 3; i++ {
		if err := g.pollOnce(ctx); err != nil {
			t.Fatalf("poll: %v", err)
		}
	}
	if fake.notModified == 0 {
		t.Error("an unchanged repository was listed without a conditional request")
	}
	if code, rep := h.doJSONOn(srv, h.aliceTok, http.MethodPost, h.projectPath("/github/issues/sync"), map[string]any{}); code != http.StatusOK || rep["created"] != float64(0) {
		t.Errorf("manual sync = %d %v", code, rep)
	}
	issueWebhook(t, srv, fake, "opened", 1)
	if _, n := issueTask(t, h, fake, 1); n != 1 {
		t.Fatalf("issue 1 now has %d tasks", n)
	}

	// --- Label filter through a webhook: labelling issue 2 imports it.
	fake.setIssue(2, "Unlabelled", "Not for Conductor yet.", "open", "bug", "Conductor")
	if out := issueWebhook(t, srv, fake, "labeled", 2); len(out["tasks"].([]any)) != 1 {
		t.Errorf("labeled webhook = %v", out)
	}
	two, n := issueTask(t, h, fake, 2)
	if n != 1 {
		t.Fatal("labelling an issue did not import it")
	}

	// --- Edits: an issue edit reaches an untouched task.
	fake.setIssue(1, "Retry storms everywhere", "Back off with jitter.\n\n## Acceptance\n- [ ] jitter\n- [ ] at most 5 tries", "open", "conductor")
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got, _ := issueTask(t, h, fake, 1); got.Title != "Retry storms everywhere" {
		t.Fatalf("an issue edit did not reach its task: %q", got.Title)
	}
	// An issue edit older than a Conductor edit loses to it...
	fake.setIssue(1, "Issue edit A", "Back off with jitter.\n\n## Acceptance\n- [ ] jitter\n- [ ] at most 5 tries", "open", "conductor")
	time.Sleep(5 * time.Millisecond)
	if code, body := h.do(h.aliceTok, http.MethodPatch, "/v1/tasks/"+one.ID, map[string]any{"title": "Conductor's title"}); code != http.StatusOK {
		t.Fatalf("patch = %d %s", code, body)
	}
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got, _ := issueTask(t, h, fake, 1); got.Title != "Conductor's title" {
		t.Errorf("an older issue edit overwrote a newer Conductor edit: %q", got.Title)
	}
	// ...and one made after it wins.
	time.Sleep(5 * time.Millisecond)
	fake.setIssue(1, "Issue edit B", "Back off with jitter.\n\n## Acceptance\n- [ ] jitter\n- [ ] at most 5 tries", "open", "conductor")
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got, _ := issueTask(t, h, fake, 1); got.Title != "Issue edit B" {
		t.Errorf("a newer issue edit lost to an older Conductor edit: %q", got.Title)
	}

	// --- Claim write-back: one comment naming the claimant, and the label.
	claim := claimFor(t, h, one, h.alice)
	if err := g.issueWriteBack(ctx, false); err != nil {
		t.Fatalf("write-back: %v", err)
	}
	if w := writesFor(fake, 1); len(w) != 2 || w[0] != "comment 1: Claimed by `alice` via Conductor." || w[1] != "label+ 1: in-progress" {
		t.Fatalf("claim write-back = %q", w)
	}
	if err := g.issueWriteBack(ctx, false); err != nil {
		t.Fatalf("write-back: %v", err)
	}
	if w := writesFor(fake, 1); len(w) != 2 {
		t.Errorf("a second pass wrote again: %q", w)
	}

	// --- Done through a merge (github_merge.go): the label goes, one comment links the pull
	// request, and the issue is closed.
	prURL := "https://github.example/acme/" + fake.repo + "/pull/5"
	if _, err := h.store.UpdateAttempt(ctx, claim.Attempt.ID, db.AttemptProgress{State: domain.AttemptRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpdateTaskStatus(ctx, one.ID, domain.TaskRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Release(ctx, db.ReleaseParams{Fence: claim.Fence, AttemptState: domain.AttemptSucceeded, NextTaskStatus: domain.TaskVerifying}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.LinkPullRequest(ctx, one.ID, prURL); err != nil {
		t.Fatal(err)
	}
	if res, err := h.store.PullRequestMerged(ctx, one.ID, prURL); err != nil || res.Status != domain.TaskDone {
		t.Fatalf("merge = %+v %v", res, err)
	}
	if err := g.issueWriteBack(ctx, false); err != nil {
		t.Fatalf("write-back: %v", err)
	}
	want := []string{"comment 1: Claimed by `alice` via Conductor.", "label+ 1: in-progress", "label- 1: in-progress",
		"comment 1: Done via Conductor in " + prURL + ".", "close 1"}
	if w := writesFor(fake, 1); strings.Join(w, "|") != strings.Join(want, "|") {
		t.Errorf("done write-back =\n%q\nwant\n%q", w, want)
	}
	// The close reaches the poller as an issue update; the done task stays done.
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got, _ := issueTask(t, h, fake, 1); got.Status != domain.TaskDone {
		t.Errorf("issue 1's own close moved its task to %s", got.Status)
	}

	// --- A pull request's "Closes #2" closed the issue before the write-back ran: no comment,
	// no second close.
	claimFor(t, h, two, h.bob)
	if err := g.issueWriteBack(ctx, false); err != nil {
		t.Fatalf("write-back: %v", err)
	}
	if _, err := h.store.CompleteWork(ctx, two.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	fake.setIssue(2, "Unlabelled", "Not for Conductor yet.", "closed", "bug", "Conductor")
	if err := g.issueWriteBack(ctx, false); err != nil {
		t.Fatalf("write-back: %v", err)
	}
	if w := writesFor(fake, 2); len(w) != 3 || w[0] != "comment 2: Claimed by `bob` via Conductor." || w[2] != "label- 2: in-progress" {
		t.Errorf("issue closed by its pull request: writes %q", w)
	}

	// --- Close and reopen through webhooks: cancelled, then ready again.
	fake.setIssue(6, "Flaky test", "", "open", "conductor")
	issueWebhook(t, srv, fake, "opened", 6)
	six, _ := issueTask(t, h, fake, 6)
	fake.setIssue(6, "Flaky test", "", "closed", "conductor")
	issueWebhook(t, srv, fake, "closed", 6)
	if got, _ := issueTask(t, h, fake, 6); got.Status != domain.TaskCancelled {
		t.Fatalf("closed issue: task %s", got.Status)
	}
	fake.setIssue(6, "Flaky test", "", "open", "conductor")
	issueWebhook(t, srv, fake, "reopened", 6)
	if got, n := issueTask(t, h, fake, 6); got.Status != domain.TaskReady || n != 1 || got.ID != six.ID {
		t.Fatalf("reopened issue: %d tasks, %s", n, got.Status)
	}
	// A task a person cancelled is not revived by a reopen.
	if _, err := h.store.UpdateTaskStatus(ctx, six.ID, domain.TaskCancelled); err != nil {
		t.Fatal(err)
	}
	fake.setIssue(6, "Flaky test", "", "closed", "conductor")
	issueWebhook(t, srv, fake, "closed", 6)
	fake.setIssue(6, "Flaky test", "", "open", "conductor")
	issueWebhook(t, srv, fake, "reopened", 6)
	if got, _ := issueTask(t, h, fake, 6); got.Status != domain.TaskCancelled {
		t.Errorf("a reopen revived a task a person cancelled: %s", got.Status)
	}

	// The poller sees a close the same way.
	fake.setIssue(7, "Polled", "", "open", "conductor")
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	fake.setIssue(7, "Polled", "", "closed", "conductor")
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got, n := issueTask(t, h, fake, 7); n != 1 || got.Status != domain.TaskCancelled {
		t.Errorf("an issue the poller saw close: %d tasks, %s", n, got.Status)
	}

	// --- The project's sync status carries counts and settings, never an issue's text, to
	// any member.
	_, obsTok := h.member("observer-issues", domain.RoleObserver)
	code, st := h.doJSONOn(srv, obsTok, http.MethodGet, h.projectPath("/github/issues"), nil)
	if code != http.StatusOK || st["enabled"] != true || st["imported"] != float64(4) || st["label"] != "conductor" {
		t.Errorf("issue sync status = %d %v", code, st)
	}
	if raw := toJSON(st); strings.Contains(raw, "Retry") || strings.Contains(raw, "jitter") {
		t.Errorf("issue sync status carries issue text: %s", raw)
	}
	if code, _ := h.doJSONOn(srv, h.outTok, http.MethodGet, h.projectPath("/github/issues"), nil); code != http.StatusNotFound {
		t.Errorf("a non-member reading issue sync status = %d, want 404", code)
	}

	// --- Disabled: nothing is imported or written back.
	if code, out := h.doJSONOn(srv, h.aliceTok, http.MethodPost, h.projectPath("/github/issues"), map[string]any{"enabled": false}); code != http.StatusOK || out["enabled"] != false {
		t.Fatalf("disable = %d %v", code, out)
	}
	fake.setIssue(9, "After disable", "", "open", "conductor")
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if _, n := issueTask(t, h, fake, 9); n != 0 {
		t.Error("a disabled sync imported an issue")
	}
}

// An installation that has not accepted the issues permission is reported, with what to do,
// rather than failing silently.
func TestGitHubIssueSyncMissingPermission(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	g, fake, srv := newIssueIntegration(t, h, false)
	old := map[string]string{"metadata": "read", "contents": "read", "pull_requests": "read", "checks": "write"}
	fake.appPerms, fake.instPerms = old, old
	fake.setIssue(1, "Needs access", "", "open", "conductor")
	enableIssues(t, h, srv, map[string]any{})

	code, st := h.doJSONOn(srv, h.aliceTok, http.MethodGet, "/v1/github/status", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	gaps := toJSON(st["permission_gaps"])
	for _, want := range []string{`"scope":"app"`, `"scope":"acme"`, "issues: write",
		"https://github.example/organizations/acme/settings/apps/conductor-test/permissions",
		"https://github.example/organizations/acme/settings/installations/77"} {
		if !strings.Contains(gaps, want) {
			t.Errorf("permission gaps lack %q: %s", want, gaps)
		}
	}
	if !strings.Contains(toJSON(st["linked"]), `"issues":"label:conductor"`) {
		t.Errorf("status does not show the project's issue sync: %v", st["linked"])
	}

	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if _, n := issueTask(t, h, fake, 1); n != 0 {
		t.Error("an issue was imported without the permission")
	}
	_, st = h.doJSONOn(srv, h.aliceTok, http.MethodGet, h.projectPath("/github/issues"), nil)
	if msg, _ := st["last_error"].(string); !strings.Contains(msg, "accept") || !strings.Contains(msg, "installations/77") {
		t.Errorf("the sync status does not say what to do: %v", st)
	}
	if code, out := h.doJSONOn(srv, h.aliceTok, http.MethodPost, h.projectPath("/github/issues/sync"), map[string]any{}); code != http.StatusConflict || out["code"] != "missing_permission" {
		t.Errorf("manual sync without the permission = %d %v", code, out)
	}

	// Accepted (read only, say): imports work; a write-back is refused and reported.
	fake.mu.Lock()
	fake.appPerms = nil
	fake.instPerms = map[string]string{"issues": "read", "metadata": "read"}
	fake.mu.Unlock()
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	task, n := issueTask(t, h, fake, 1)
	if n != 1 {
		t.Fatal("the issue was not imported once the permission was accepted")
	}
	if code, st := h.doJSONOn(srv, h.aliceTok, http.MethodGet, "/v1/github/status", nil); code != http.StatusOK || strings.Contains(toJSON(st["permission_gaps"]), `"scope":"app"`) {
		t.Errorf("the app-level gap is still reported: %v", st["permission_gaps"])
	}
	claimFor(t, h, task, h.alice)
	if err := g.issueWriteBack(ctx, false); err == nil {
		t.Error("a write-back without issues: write succeeded")
	}
	_, st = h.doJSONOn(srv, h.aliceTok, http.MethodGet, h.projectPath("/github/issues"), nil)
	if msg, _ := st["writeback_error"].(string); !strings.Contains(msg, "write to issues") {
		t.Errorf("a refused write-back was not reported: %v", st)
	}
	// The refusal is not asked for again on every tick, only by the next poll.
	if err := g.issueWriteBack(ctx, false); err != nil {
		t.Errorf("a tick retried a refused write-back: %v", err)
	}
	// Accepted in full: the next poll writes back and clears the problem.
	fake.mu.Lock()
	fake.instPerms = nil
	fake.mu.Unlock()
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if w := writesFor(fake, 1); len(w) != 2 || !strings.HasPrefix(w[0], "comment 1: Claimed") {
		t.Errorf("write-back after the permission was accepted: %q", w)
	}
	if _, st = h.doJSONOn(srv, h.aliceTok, http.MethodGet, h.projectPath("/github/issues"), nil); st["writeback_error"] != nil || st["last_error"] != nil {
		t.Errorf("problems still reported after they cleared: %v", st)
	}
}

// A public repository's issues are public already: their tasks are shared at team_artifacts,
// and a task made private afterwards is never written back to its public issue.
func TestGitHubIssueSyncPublicRepository(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	g, fake, srv := newIssueIntegration(t, h, true)
	fake.setIssue(1, "Public one", "Anyone can read this.", "open", "conductor")
	fake.setIssue(2, "Public two", "Anyone can read this too.", "open", "conductor")
	enableIssues(t, h, srv, map[string]any{})
	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	one, _ := issueTask(t, h, fake, 1)
	two, _ := issueTask(t, h, fake, 2)
	if one.Visibility != domain.VisibilityTeamArtifacts || two.Visibility != domain.VisibilityTeamArtifacts {
		t.Fatalf("public issues imported as %s and %s, want team_artifacts", one.Visibility, two.Visibility)
	}

	// Task 2 is made private and gets a secret objective; then both are claimed and finished.
	const secret = "ZQXPRIVATEPLAN"
	if code, body := h.do(h.aliceTok, http.MethodPatch, "/v1/tasks/"+two.ID, map[string]any{"visibility": "private", "objective": secret}); code != http.StatusOK {
		t.Fatalf("patch = %d %s", code, body)
	}
	claimFor(t, h, one, h.alice)
	claimFor(t, h, two, h.alice)
	if err := g.issueWriteBack(ctx, false); err != nil {
		t.Fatalf("write-back: %v", err)
	}
	for _, id := range []domain.ID{one.ID, two.ID} {
		if _, err := h.store.CompleteWork(ctx, id, "done"); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.issueWriteBack(ctx, false); err != nil {
		t.Fatalf("write-back: %v", err)
	}
	if w := writesFor(fake, 1); len(w) < 2 || w[0] != "comment 1: Claimed by `alice` via Conductor." {
		t.Errorf("a team_artifacts task on a public repository: %q", w)
	}
	if w := writesFor(fake, 2); len(w) != 0 {
		t.Errorf("a private task was written back to a public issue: %q", w)
	}
	for _, w := range fake.writes() {
		if strings.Contains(w, secret) || strings.Contains(w, "Anyone can read") {
			t.Errorf("task content reached GitHub: %q", w)
		}
	}

	// Where a task's issue is: the address GitHub gave, for whoever may see the task's
	// external_ref, and nobody else.
	code, out := h.doJSONOn(srv, h.bobTok, http.MethodGet, "/v1/tasks/"+one.ID+"/issue", nil)
	if code != http.StatusOK || out["url"] != fake.issue(1)["html_url"] || out["synced"] != true {
		t.Errorf("issue of a shared task = %d %v", code, out)
	}
	if code, out := h.doJSONOn(srv, h.bobTok, http.MethodGet, "/v1/tasks/"+two.ID+"/issue", nil); code != http.StatusNotFound {
		t.Errorf("another member reading a private task's issue = %d %v, want 404", code, out)
	}
	if code, out := h.doJSONOn(srv, h.aliceTok, http.MethodGet, "/v1/tasks/"+two.ID+"/issue", nil); code != http.StatusOK || out["url"] == nil {
		t.Errorf("the owner reading their private task's issue = %d %v", code, out)
	}
}

// A webhook and a poll racing on a new issue, or two replicas, create one task.
func TestIssueImportIsIdempotentUnderConcurrency(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg, err := h.store.SaveTrackerConfig(ctx, db.TrackerConfig{ProjectID: h.project.ID, Tracker: db.TrackerGitHub,
		Enabled: true, Label: "conductor", InProgressLabel: "in-progress", PublicVisibility: domain.VisibilityTeamArtifacts,
		EnabledBy: h.alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	it := tracker.Item{Tracker: db.TrackerGitHub, Key: "acme/race-" + h.project.Slug + "#1", Title: "Race", Open: true,
		Labels: []string{"conductor"}, UpdatedAt: time.Now()}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tracker.Observe(ctx, h.store, cfg, h.project, it); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	tasks, err := h.store.ListTasks(ctx, h.project.ID, db.ListTasksFilter{ExternalRef: it.Ref()})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("%d tasks for one issue (%v)", len(tasks), err)
	}
	if n := eventCount(t, h, tasks[0].ID, "tracker.imported"); n != 1 {
		t.Errorf("%d import events, want 1", n)
	}
}
