package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/githubapp"
	"github.com/adamburan/conductor/internal/resource"
)

// mergeGitHub is a fake GitHub whose pull requests can open, merge, and close, for the
// lifecycle half of the integration.
type mergeGitHub struct {
	repo  string
	mu    sync.Mutex
	pulls map[int]map[string]any // number -> pull request JSON
}

func (f *mergeGitHub) set(number int, pr map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[number] = pr
}

func (f *mergeGitHub) handler() http.Handler {
	m := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	m.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		write(w, []map[string]any{{"id": 77, "target_type": "Organization", "account": map[string]any{"login": "acme"}}})
	})
	m.HandleFunc("POST /app/installations/77/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	m.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"repositories": []map[string]any{{"name": f.repo, "full_name": "acme/" + f.repo, "private": true, "owner": map[string]any{"login": "acme"}}}})
	})
	m.HandleFunc("GET /repos/acme/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		open := []map[string]any{}
		for _, pr := range f.pulls {
			if pr["state"] == "open" {
				open = append(open, pr)
			}
		}
		write(w, open)
	})
	m.HandleFunc("GET /repos/acme/{repo}/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		f.mu.Lock()
		pr, ok := f.pulls[n]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		write(w, pr)
	})
	m.HandleFunc("GET /repos/acme/{repo}/pulls/{n}/files", func(w http.ResponseWriter, r *http.Request) {
		write(w, []map[string]any{{"filename": "internal/merge/a.go"}})
	})
	m.HandleFunc("POST /repos/acme/{repo}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		write(w, map[string]any{"id": 1})
	})
	return m
}

func (f *mergeGitHub) pull(number int, branch, state string, merged bool) map[string]any {
	same := map[string]any{"full_name": "acme/" + f.repo}
	return map[string]any{
		"number": number, "state": state, "merged": merged,
		"html_url":         "https://github.example/acme/" + f.repo + "/pull/" + strconv.Itoa(number),
		"merge_commit_sha": "feedface",
		"head":             map[string]any{"ref": branch, "sha": "sha-" + branch, "repo": same},
		"base":             map[string]any{"ref": "main", "repo": same},
	}
}

// newMergeIntegration wires a configured GitHub App against the fake, with the harness's
// project linked to the fake repository and alice as the machine owner.
func newMergeIntegration(t *testing.T, h *harness) (*GitHub, *mergeGitHub, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &mergeGitHub{repo: "merges-" + h.project.Slug, pulls: map[int]map[string]any{}}
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
		PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}); err != nil {
		t.Fatal(err)
	}
	integration, err := NewGitHub(GitHubOptions{CredentialsPath: creds, API: gh.URL, Web: "https://github.example", Poll: -1,
		Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetProjectRemote(ctx, h.project.ID, "https://github.com/acme/"+fake.repo); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(h.store, coord.New(h.store), Options{GitHub: integration}).Handler())
	t.Cleanup(srv.Close)
	return integration, fake, srv
}

// finishedTask claims a task on a branch and runs it to verifying, holding one file.
func finishedTask(t *testing.T, h *harness, branch, file string) domain.Task {
	t.Helper()
	ctx := context.Background()
	task, err := h.store.CreateTask(ctx, db.CreateTaskParams{ProjectID: h.project.ID, CreatedBy: h.alice.ID,
		Title: "work on " + branch, Status: domain.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	if branch == "" {
		branch = "agent/" + task.Ref + "/attempt-1"
	}
	claim, err := h.store.Claim(ctx, db.ClaimParams{TaskID: task.ID, ProjectID: h.project.ID, HolderPrincipal: h.alice.ID,
		Harness: "test", Branch: branch, LeaseTTL: time.Minute, ScopePolicy: resource.DefaultPolicy(),
		Scopes: []domain.ScopeRequest{{Resource: "path:" + file, Mode: domain.ModeWriteExclusive}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpdateAttempt(ctx, claim.Attempt.ID, db.AttemptProgress{State: domain.AttemptRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpdateTaskStatus(ctx, task.ID, domain.TaskRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Release(ctx, db.ReleaseParams{Fence: claim.Fence, AttemptState: domain.AttemptSucceeded,
		NextTaskStatus: domain.TaskVerifying}); err != nil {
		t.Fatal(err)
	}
	return task
}

func taskState(t *testing.T, h *harness, id domain.ID) (domain.Task, int) {
	t.Helper()
	task, err := h.store.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	held, err := h.store.ReservationsForTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task, len(held)
}

// Without a webhook, the poller links the pull request it sees open and, when it later drops
// off the open list as merged, completes the task and releases its territory.
func TestPollerCompletesTaskWhenItsPullRequestMerges(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	integration, fake, _ := newMergeIntegration(t, h)

	task := finishedTask(t, h, "feature/merge-me", "internal/merge/a.go")
	fake.set(7, fake.pull(7, "feature/merge-me", "open", false))
	if err := integration.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	got, held := taskState(t, h, task.ID)
	if got.PullRequestState != "open" || got.PullRequestURL == "" || got.Status != domain.TaskVerifying || held == 0 {
		t.Fatalf("after seeing it open: status %s, pr %q %q, %d held", got.Status, got.PullRequestState, got.PullRequestURL, held)
	}

	fake.set(7, fake.pull(7, "feature/merge-me", "closed", true))
	if err := integration.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	got, held = taskState(t, h, task.ID)
	if got.Status != domain.TaskDone || got.PullRequestState != "merged" || held != 0 {
		t.Fatalf("after the merge: status %s, pr %q, %d held; want done, merged, 0", got.Status, got.PullRequestState, held)
	}
}

func TestWebhookMergeAndCloseMoveTheTask(t *testing.T) {
	h := newHarness(t)
	_, fake, srv := newMergeIntegration(t, h)

	send := func(pr map[string]any) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"action": "closed", "installation": map[string]any{"id": 77},
			"repository":   map[string]any{"name": fake.repo, "private": true, "owner": map[string]any{"login": "acme"}},
			"pull_request": pr})
		return webhook(t, srv, "pull_request", body, githubapp.Sign("whsec", body))
	}

	// Merged: the agent/<ref>/attempt-<n> branch names the task.
	merged := finishedTask(t, h, "", "internal/merge/b.go")
	if code := send(fake.pull(8, "agent/"+merged.Ref+"/attempt-1", "closed", true)); code != http.StatusOK {
		t.Fatalf("merged delivery = %d", code)
	}
	if got, held := taskState(t, h, merged.ID); got.Status != domain.TaskDone || held != 0 || got.PullRequestState != "merged" {
		t.Fatalf("merged: status %s, %d held, pr %q", got.Status, held, got.PullRequestState)
	}

	// Closed without merging: the work goes back to the queue and drops its hold.
	closed := finishedTask(t, h, "feature/abandoned", "internal/merge/c.go")
	if code := send(fake.pull(9, "feature/abandoned", "closed", false)); code != http.StatusOK {
		t.Fatalf("closed delivery = %d", code)
	}
	if got, held := taskState(t, h, closed.ID); got.Status != domain.TaskReady || held != 0 || got.PullRequestState != "closed" {
		t.Fatalf("closed: status %s, %d held, pr %q", got.Status, held, got.PullRequestState)
	}

	// A fork's branch name never completes anyone's task.
	victim := finishedTask(t, h, "", "internal/merge/d.go")
	fork := fake.pull(10, "agent/"+victim.Ref+"/attempt-1", "closed", true)
	fork["head"].(map[string]any)["repo"] = map[string]any{"full_name": "mallory/" + fake.repo}
	if code := send(fork); code != http.StatusOK {
		t.Fatalf("fork delivery = %d", code)
	}
	if got, _ := taskState(t, h, victim.ID); got.Status != domain.TaskVerifying {
		t.Errorf("a fork's merge moved %s to %s", victim.Ref, got.Status)
	}
}

func TestParsePullURL(t *testing.T) {
	o, r, n, ok := parsePullURL("https://github.example/acme/widgets/pull/42")
	if !ok || o != "acme" || r != "widgets" || n != 42 {
		t.Errorf("parse = %q %q %d %v", o, r, n, ok)
	}
	for _, bad := range []string{"", "https://github.com/acme/widgets", "https://github.com/acme/widgets/issues/3", "https://github.com/acme/widgets/pull/x"} {
		if _, _, _, ok := parsePullURL(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
