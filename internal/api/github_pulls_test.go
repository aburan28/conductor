package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/githubapp"
)

// Pull requests as first-class objects (docs/COLLABORATION.md §4.5): ingestion by poller and
// webhook, link precedence, the lifecycle across several pull requests, and the HTTP surface.

// rollupFixture is what the fake's GraphQL answers for one pull request.
type rollupFixture struct {
	Decision string // APPROVED | CHANGES_REQUESTED | REVIEW_REQUIRED | ""
	State    string // mergeStateStatus
	Threads  int    // unresolved review threads
	// Checks are [kind, status-or-state, conclusion]: {"run", "COMPLETED", "SUCCESS"},
	// {"status", "PENDING", ""}.
	Checks [][3]string
}

// pullsGitHub is a fake GitHub for the pull-request list: pull requests that can open, move,
// merge and close; an open list that answers a conditional request with 304; commits that may
// carry a trailer; and GraphQL.
type pullsGitHub struct {
	repo string

	mu          sync.Mutex
	version     int
	pulls       map[int]map[string]any
	commits     map[int][]string
	rollups     map[int]rollupFixture
	notModified int
	graphql     int
	commitCalls int
	listCalls   int
}

func (f *pullsGitHub) put(number int, pr map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
	f.pulls[number] = pr
}

// update changes a pull request the way GitHub would: the listing's ETag moves with it.
func (f *pullsGitHub) update(number int, fn func(pr map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
	fn(f.pulls[number])
	f.pulls[number]["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
}

func (f *pullsGitHub) setRollup(number int, r rollupFixture) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rollups[number] = r
}

func (f *pullsGitHub) counts() (graphql, commits, notModified int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.graphql, f.commitCalls, f.notModified
}

func (f *pullsGitHub) pr(number int, branch string) map[string]any {
	same := map[string]any{"full_name": "acme/" + f.repo}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	return map[string]any{
		"number": number, "state": "open", "title": "Pull request " + strconv.Itoa(number), "body": "",
		"draft": false, "merged": false, "merged_at": nil, "created_at": now, "updated_at": now,
		"html_url":         "https://github.example/acme/" + f.repo + "/pull/" + strconv.Itoa(number),
		"merge_commit_sha": "feedface",
		"user":             map[string]any{"login": "octo", "id": 99},
		"head":             map[string]any{"ref": branch, "sha": "sha-" + branch, "repo": same},
		"base":             map[string]any{"ref": "main", "repo": same},
	}
}

func (f *pullsGitHub) end(number int, merged bool) map[string]any {
	f.update(number, func(pr map[string]any) {
		pr["state"], pr["merged"] = "closed", merged
		if merged {
			pr["merged_at"] = time.Now().UTC().Format(time.RFC3339)
		}
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pulls[number]
}

func (f *pullsGitHub) handler() http.Handler {
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
	m.HandleFunc("GET /repos/acme/{repo}/installation", func(w http.ResponseWriter, r *http.Request) { write(w, map[string]any{"id": 77}) })
	m.HandleFunc("GET /repos/acme/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		etag := `"v` + strconv.Itoa(f.version) + `"`
		if state == "open" {
			f.listCalls++
			if r.Header.Get("If-None-Match") == etag {
				f.notModified++
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
		}
		list := []map[string]any{}
		for _, pr := range f.pulls {
			if pr["state"] != state {
				continue
			}
			// Like GitHub, a list entry carries merged_at but not merged, and no sizes.
			entry := map[string]any{}
			for k, v := range pr {
				if k != "merged" {
					entry[k] = v
				}
			}
			list = append(list, entry)
		}
		write(w, list)
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
		write(w, []map[string]any{{"filename": "internal/pulls/a.go"}})
	})
	m.HandleFunc("GET /repos/acme/{repo}/pulls/{n}/commits", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		f.mu.Lock()
		f.commitCalls++
		msgs := f.commits[n]
		f.mu.Unlock()
		out := []map[string]any{}
		for _, msg := range msgs {
			out = append(out, map[string]any{"commit": map[string]any{"message": msg}})
		}
		write(w, out)
	})
	m.HandleFunc("POST /repos/acme/{repo}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		write(w, map[string]any{"id": 1})
	})
	m.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Variables struct {
				Number int `json:"number"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		f.graphql++
		rf := f.rollups[req.Variables.Number]
		f.mu.Unlock()
		nodes := []map[string]any{}
		for _, c := range rf.Checks {
			if c[0] == "run" {
				nodes = append(nodes, map[string]any{"__typename": "CheckRun", "status": c[1], "conclusion": c[2]})
			} else {
				nodes = append(nodes, map[string]any{"__typename": "StatusContext", "state": c[1]})
			}
		}
		threads := []map[string]any{{"isResolved": true}}
		for i := 0; i < rf.Threads; i++ {
			threads = append(threads, map[string]any{"isResolved": false})
		}
		var decision any
		if rf.Decision != "" {
			decision = rf.Decision
		}
		state := rf.State
		if state == "" {
			state = "CLEAN"
		}
		write(w, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewDecision": decision, "mergeable": "MERGEABLE", "mergeStateStatus": state,
			"additions": 12, "deletions": 3, "changedFiles": 2,
			"reviewThreads": map[string]any{"totalCount": len(threads), "nodes": threads},
			"commits": map[string]any{"nodes": []map[string]any{{"commit": map[string]any{"statusCheckRollup": map[string]any{
				"contexts": map[string]any{"totalCount": len(nodes), "nodes": nodes}}}}}},
		}}}})
	})
	return m
}

// newPullsIntegration wires a configured GitHub App against the fake, with the harness's
// project linked to the fake repository and alice as the machine owner.
func newPullsIntegration(t *testing.T, h *harness) (*GitHub, *pullsGitHub, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &pullsGitHub{repo: "pulls-" + h.project.Slug, pulls: map[int]map[string]any{},
		commits: map[int][]string{}, rollups: map[int]rollupFixture{}}
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

func (f *pullsGitHub) name() string { return "acme/" + f.repo }

func pullRow(t *testing.T, h *harness, repo string, number int) db.PullRequest {
	t.Helper()
	row, err := h.store.GetPullRequest(context.Background(), h.project.ID, repo, number)
	if err != nil {
		t.Fatalf("pull request %s#%d: %v", repo, number, err)
	}
	return row
}

// pullEventsOf returns the events of a type about one pull request, newest first.
func pullEventsOf(t *testing.T, h *harness, typ string, number int) []domain.Event {
	t.Helper()
	events, err := h.store.ListEvents(context.Background(), h.project.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.Event
	for _, e := range events {
		if e.Type != typ {
			continue
		}
		if n, _ := e.Payload["number"].(float64); int(n) == number {
			out = append(out, e)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// call makes an authenticated request to a server and returns the status, the JSON body, and
// the headers.
func pullCall(t *testing.T, srv *httptest.Server, tok, method, path string, body any) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func pullsIn(body map[string]any) []map[string]any {
	var out []map[string]any
	list, _ := body["pulls"].([]any)
	for _, p := range list {
		out = append(out, p.(map[string]any))
	}
	return out
}

func sendEvent(t *testing.T, srv *httptest.Server, event string, payload map[string]any) int {
	t.Helper()
	body, _ := json.Marshal(payload)
	return webhook(t, srv, event, body, githubapp.Sign("whsec", body))
}

func repoPayload(fake *pullsGitHub) map[string]any {
	return map[string]any{"name": fake.repo, "private": true, "owner": map[string]any{"login": "acme"}}
}

// The poller turns the open list into rows and diffs it into events; an unchanged list costs a
// conditional request, and a settled rollup is not read again.
func TestPollerRecordsPullRequestsAndAnnouncesWhatChanged(t *testing.T) {
	h := newHarness(t)
	g, fake, srv := newPullsIntegration(t, h)
	ctx := context.Background()
	poll := func() {
		t.Helper()
		if err := g.pollOnce(ctx); err != nil {
			t.Fatalf("poll: %v", err)
		}
	}
	repo := fake.name()

	fake.put(1, fake.pr(1, "feature/a"))
	fake.setRollup(1, rollupFixture{Checks: [][3]string{{"run", "IN_PROGRESS", ""}, {"run", "COMPLETED", "SUCCESS"}}, Threads: 2})
	// A pull request opened long ago is history, not news.
	old := fake.pr(2, "feature/old")
	old["created_at"] = time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	fake.put(2, old)

	poll()
	row := pullRow(t, h, repo, 1)
	if row.State != "open" || row.Title != "Pull request 1" || row.AuthorLogin != "octo" || row.AuthorID != 99 ||
		row.HeadRef != "feature/a" || row.HeadSHA != "sha-feature/a" || row.BaseRef != "main" || row.FromFork || row.Draft ||
		row.URL != "https://github.example/"+repo+"/pull/1" {
		t.Fatalf("row = %+v", row)
	}
	if row.ChecksTotal != 2 || row.ChecksPending != 1 || row.ChecksPassed != 1 || row.UnresolvedThreads != 2 ||
		row.Mergeable != "clean" || row.ReviewDecision != "" || row.ChangedFiles != 2 || row.Additions != 12 || row.RollupAt == nil {
		t.Fatalf("rollup = %+v", row.PullRequestRollup)
	}
	if n := len(pullEventsOf(t, h, "github.pr_opened", 1)); n != 1 {
		t.Errorf("pr_opened for a new pull request = %d, want 1", n)
	}
	if n := len(pullEventsOf(t, h, "github.pr_opened", 2)); n != 0 {
		t.Errorf("pr_opened for a pull request opened three days ago = %d, want 0", n)
	}
	if graphql, _, _ := fake.counts(); graphql != 2 {
		t.Errorf("first poll made %d GraphQL calls, want one per pull request (2)", graphql)
	}

	// Nothing changed: the list answers 304. #1 still has a check running, so its rollup is read
	// again; #2 settled and is fresh, so it is not. No event: nothing differs.
	poll()
	graphql, _, notModified := fake.counts()
	if notModified != 1 || graphql != 3 {
		t.Errorf("second poll: %d 304s, %d GraphQL calls; want 1 and 3", notModified, graphql)
	}
	if n := len(pullEventsOf(t, h, "github.pr_opened", 1)); n != 1 {
		t.Errorf("an unchanged pull request was announced again: %d", n)
	}

	// CI finishes and a reviewer approves: neither moves the pull request's updated_at, and
	// both are announced once.
	fake.setRollup(1, rollupFixture{Decision: "APPROVED", Checks: [][3]string{{"run", "COMPLETED", "SUCCESS"}, {"status", "SUCCESS", ""}}})
	poll()
	row = pullRow(t, h, repo, 1)
	if row.ChecksPassed != 2 || row.ChecksPending != 0 || row.ReviewDecision != "approved" || row.UnresolvedThreads != 0 {
		t.Fatalf("after CI: %+v", row.PullRequestRollup)
	}
	checks := pullEventsOf(t, h, "github.pr_checks", 1)
	if len(checks) != 1 || checks[0].Payload["outcome"] != "passed" || checks[0].Payload["passed"] != float64(2) || checks[0].Payload["failed"] != float64(0) {
		t.Fatalf("pr_checks = %+v", checks)
	}
	review := pullEventsOf(t, h, "github.pr_review", 1)
	if len(review) != 1 || review[0].Payload["decision"] != "approved" {
		t.Fatalf("pr_review = %+v", review)
	}
	// Settled and fresh: not read again.
	before, _, _ := fake.counts()
	poll()
	if after, _, _ := fake.counts(); after != before {
		t.Errorf("a settled rollup was read again (%d -> %d GraphQL calls)", before, after)
	}

	// A new head is announced, and the rollup is read again.
	fake.update(1, func(pr map[string]any) { pr["head"].(map[string]any)["sha"] = "newhead" })
	poll()
	if row = pullRow(t, h, repo, 1); row.HeadSHA != "newhead" {
		t.Errorf("head = %s", row.HeadSHA)
	}
	if n := len(pullEventsOf(t, h, "github.pr_updated", 1)); n != 1 {
		t.Errorf("pr_updated = %d, want 1", n)
	}
	// ...and a redelivery of the same synchronize changes nothing and announces nothing.
	fake.mu.Lock()
	same := fake.pulls[1]
	fake.mu.Unlock()
	if code := sendEvent(t, srv, "pull_request", map[string]any{"action": "synchronize", "installation": map[string]any{"id": 77},
		"repository": repoPayload(fake), "pull_request": same}); code != http.StatusAccepted {
		t.Fatalf("synchronize = %d", code)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(pullEventsOf(t, h, "github.pr_updated", 1)); n != 1 {
		t.Errorf("a redelivery announced pr_updated again: %d", n)
	}

	// Failing checks are announced when they fail, not while they run.
	fake.setRollup(1, rollupFixture{Decision: "APPROVED", Checks: [][3]string{{"run", "COMPLETED", "FAILURE"}, {"run", "IN_PROGRESS", ""}}})
	fake.update(1, func(pr map[string]any) { pr["head"].(map[string]any)["sha"] = "newhead2" })
	poll()
	checks = pullEventsOf(t, h, "github.pr_checks", 1)
	if len(checks) != 2 || checks[0].Payload["outcome"] != "failed" || checks[0].Payload["failed"] != float64(1) || checks[0].Payload["pending"] != float64(1) {
		t.Fatalf("pr_checks after a failure = %+v", checks)
	}

	// A draft leaving draft is announced once.
	fake.update(1, func(pr map[string]any) { pr["draft"] = true })
	poll()
	if !pullRow(t, h, repo, 1).Draft {
		t.Fatal("the draft flag was not recorded")
	}
	fake.update(1, func(pr map[string]any) { pr["draft"] = false })
	poll()
	poll()
	if n := len(pullEventsOf(t, h, "github.pr_ready", 1)); n != 1 {
		t.Errorf("pr_ready = %d, want 1", n)
	}

	// Every event passed the allowlist whole: nothing was narrowed away, and no title or other
	// text a stranger wrote is in one.
	events, _ := h.store.ListEvents(ctx, h.project.ID, 500)
	seen := 0
	for _, e := range events {
		if !strings.HasPrefix(e.Type, "github.pr_") || e.Type == "github.pr_checked" {
			continue
		}
		seen++
		if _, dropped := e.Payload["dropped_keys"]; dropped {
			t.Errorf("%s lost keys to the allowlist: %v", e.Type, e.Payload)
		}
		for k := range e.Payload {
			if k == "title" || k == "body" {
				t.Errorf("%s carries %q", e.Type, k)
			}
		}
		if e.Payload["repository"] != repo || e.Payload["pull_request"] == "" {
			t.Errorf("%s payload = %v", e.Type, e.Payload)
		}
	}
	if seen < 6 {
		t.Errorf("only %d pull request events recorded", seen)
	}
	if opened := pullEventsOf(t, h, "github.pr_opened", 1)[0].Payload; opened["author"] != "octo" || opened["base"] != "main" || opened["draft"] != false {
		t.Errorf("pr_opened payload = %v", opened)
	}
}

// Each delivery that only says "a pull request's checks, reviews, or head may have moved" makes
// Conductor read it again — and stores nothing the delivery said.
func TestWebhooksRefreshPullRequests(t *testing.T) {
	h := newHarness(t)
	_, fake, srv := newPullsIntegration(t, h)
	repo := fake.name()
	inst := map[string]any{"id": 77}
	// Deliveries a moment after a read are skipped as one burst; this test makes them a moment
	// apart on purpose, so step past that.
	debounce := rollupDebounce
	rollupDebounce = time.Millisecond
	t.Cleanup(func() { rollupDebounce = debounce })

	fake.put(1, fake.pr(1, "feature/a"))
	fake.setRollup(1, rollupFixture{Checks: [][3]string{{"run", "IN_PROGRESS", ""}}})
	if code := sendEvent(t, srv, "pull_request", map[string]any{"action": "opened", "installation": inst, "repository": repoPayload(fake),
		"pull_request": fake.pulls[1]}); code != http.StatusAccepted {
		t.Fatalf("opened = %d", code)
	}
	waitFor(t, "the opened pull request", func() bool {
		row, err := h.store.GetPullRequest(context.Background(), h.project.ID, repo, 1)
		return err == nil && row.RollupAt != nil && row.ChecksPending == 1
	})
	if n := len(pullEventsOf(t, h, "github.pr_opened", 1)); n != 1 {
		t.Errorf("pr_opened = %d", n)
	}

	reread := func(name string, event string, payload map[string]any, ok func(db.PullRequest) bool) {
		t.Helper()
		time.Sleep(5 * time.Millisecond)
		payload["installation"], payload["repository"] = inst, repoPayload(fake)
		if code := sendEvent(t, srv, event, payload); code != http.StatusAccepted {
			t.Fatalf("%s = %d", name, code)
		}
		waitFor(t, name, func() bool { return ok(pullRow(t, h, repo, 1)) })
	}

	fake.setRollup(1, rollupFixture{Decision: "CHANGES_REQUESTED", Threads: 1, Checks: [][3]string{{"run", "IN_PROGRESS", ""}}})
	reread("a review", "pull_request_review", map[string]any{"action": "submitted", "pull_request": map[string]any{"number": 1}},
		func(r db.PullRequest) bool {
			return r.ReviewDecision == "changes_requested" && r.UnresolvedThreads == 1
		})
	// The row is stored before its events are appended, so wait for the event too.
	waitFor(t, "pr_review", func() bool { return len(pullEventsOf(t, h, "github.pr_review", 1)) > 0 })
	if review := pullEventsOf(t, h, "github.pr_review", 1); len(review) != 1 || review[0].Payload["decision"] != "changes_requested" {
		t.Errorf("pr_review = %+v", review)
	}

	fake.setRollup(1, rollupFixture{Decision: "CHANGES_REQUESTED", Threads: 0, Checks: [][3]string{{"run", "IN_PROGRESS", ""}}})
	reread("a review comment", "pull_request_review_comment", map[string]any{"action": "created", "pull_request": map[string]any{"number": 1}},
		func(r db.PullRequest) bool { return r.UnresolvedThreads == 0 })

	fake.setRollup(1, rollupFixture{Decision: "APPROVED", Checks: [][3]string{{"run", "COMPLETED", "SUCCESS"}}})
	reread("a check suite", "check_suite", map[string]any{"action": "completed",
		"check_suite": map[string]any{"head_sha": "sha-feature/a", "pull_requests": []map[string]any{{"number": 1}}}},
		func(r db.PullRequest) bool { return r.ChecksPassed == 1 && r.ReviewDecision == "approved" })
	waitFor(t, "pr_checks", func() bool { return len(pullEventsOf(t, h, "github.pr_checks", 1)) > 0 })
	if checks := pullEventsOf(t, h, "github.pr_checks", 1); len(checks) != 1 || checks[0].Payload["outcome"] != "passed" {
		t.Errorf("pr_checks = %+v", checks)
	}

	// A check run or a status names no pull request when it comes from a fork or a bare
	// commit: it is found by its head commit.
	fake.setRollup(1, rollupFixture{Decision: "APPROVED", Checks: [][3]string{{"run", "COMPLETED", "SUCCESS"}, {"status", "FAILURE", ""}}})
	reread("a commit status", "status", map[string]any{"sha": "sha-feature/a", "state": "failure"},
		func(r db.PullRequest) bool { return r.ChecksFailed == 1 })
	fake.setRollup(1, rollupFixture{Decision: "APPROVED", Checks: [][3]string{{"run", "COMPLETED", "SUCCESS"}, {"status", "SUCCESS", ""}, {"run", "COMPLETED", "SUCCESS"}}})
	reread("a check run", "check_run", map[string]any{"action": "completed",
		"check_run": map[string]any{"head_sha": "sha-feature/a", "pull_requests": []any{}}},
		func(r db.PullRequest) bool { return r.ChecksTotal == 3 && r.ChecksFailed == 0 })

	// A comment on the conversation is read again too; on an issue, it is not for us.
	fake.setRollup(1, rollupFixture{Decision: "APPROVED", Threads: 4, Checks: [][3]string{{"run", "COMPLETED", "SUCCESS"}}})
	reread("an issue comment", "issue_comment", map[string]any{"action": "created",
		"issue": map[string]any{"number": 1, "pull_request": map[string]any{"url": "x"}}},
		func(r db.PullRequest) bool { return r.UnresolvedThreads == 4 })
	if code := sendEvent(t, srv, "issue_comment", map[string]any{"action": "created", "installation": inst, "repository": repoPayload(fake),
		"issue": map[string]any{"number": 7}}); code != http.StatusAccepted {
		t.Errorf("a comment on a plain issue = %d, want 202 and nothing done", code)
	}

	// A push to a pull request's branch moves its head (and announces it); a push to its base
	// marks the rollup for a fresh read, since mergeability depends on the base.
	fake.update(1, func(pr map[string]any) { pr["head"].(map[string]any)["sha"] = "pushed" })
	reread("a push to the head", "push", map[string]any{"ref": "refs/heads/feature/a", "after": "pushed"},
		func(r db.PullRequest) bool { return r.HeadSHA == "pushed" })
	waitFor(t, "pr_updated", func() bool { return len(pullEventsOf(t, h, "github.pr_updated", 1)) == 1 })
	if row := pullRow(t, h, repo, 1); row.RollupAt == nil {
		t.Fatal("the push to the head left no rollup")
	}
	reread("a push to the base", "push", map[string]any{"ref": "refs/heads/main", "after": "mainhead"},
		func(r db.PullRequest) bool { return r.RollupAt == nil })
	// Tags and deletions are not branch updates.
	if code := sendEvent(t, srv, "push", map[string]any{"ref": "refs/tags/v1", "installation": inst, "repository": repoPayload(fake)}); code != http.StatusAccepted {
		t.Errorf("a tag push = %d", code)
	}

	// An unsigned or wrongly signed delivery changes nothing.
	body, _ := json.Marshal(map[string]any{"action": "completed", "installation": inst, "repository": repoPayload(fake)})
	if code := webhook(t, srv, "check_suite", body, githubapp.Sign("wrong", body)); code != http.StatusUnauthorized {
		t.Errorf("a badly signed delivery = %d, want 401", code)
	}
}

// Which task a pull request belongs to is decided by precedence, and only a branch in the
// repository itself can name a task.
func TestPullRequestLinkPrecedenceThroughIngestion(t *testing.T) {
	h := newHarness(t)
	g, fake, srv := newPullsIntegration(t, h)
	ctx := context.Background()
	repo := fake.name()
	newTask := func(title string) domain.Task {
		t.Helper()
		task, err := h.store.CreateTask(ctx, db.CreateTaskParams{ProjectID: h.project.ID, CreatedBy: h.alice.ID, Title: title, Status: domain.TaskReady})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	a := finishedTask(t, h, "", "internal/pulls/a.go") // branch agent/<ref>/attempt-1
	b, c, d := newTask("trailer"), newTask("body"), newTask("second body ref")
	agent := "agent/" + a.Ref + "/attempt-1"

	fake.put(10, fake.pr(10, agent))
	fake.put(11, fake.pr(11, "feature/b"))
	fake.commits[11] = []string{"work", "more\n\nConductor-Task: " + b.Ref}
	p12 := fake.pr(12, "feature/c")
	p12["body"] = "Part of " + c.Ref + ", and see " + d.Ref
	fake.put(12, p12)
	p13 := fake.pr(13, agent) // the branch names A, the body names C: the body outranks the branch
	p13["body"] = "relates to " + c.Ref
	fake.put(13, p13)
	p14 := fake.pr(14, "feature/e") // trailer and body disagree: the trailer outranks the body
	p14["body"] = "see " + c.Ref
	fake.put(14, p14)
	fake.commits[14] = []string{"x\n\nConductor-Task: " + b.Ref}
	p15 := fake.pr(15, agent) // a fork names tasks freely and links none
	p15["head"] = map[string]any{"ref": agent, "sha": "forksha", "repo": map[string]any{"full_name": "mallory/" + fake.repo}}
	p15["body"] = "closes " + c.Ref
	fake.put(15, p15)
	fake.commits[15] = []string{"x\n\nConductor-Task: " + b.Ref}
	p16 := fake.pr(16, "feature/none")
	p16["body"] = "a task that does not exist: T-999999"
	fake.put(16, p16)

	if err := g.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	for _, c := range []struct {
		number int
		task   string
		source string
	}{
		{10, a.Ref, db.LinkBranch}, {11, b.Ref, db.LinkTrailer}, {12, c.Ref, db.LinkBodyRef},
		{13, c.Ref, db.LinkBodyRef}, {14, b.Ref, db.LinkTrailer}, {15, "", ""}, {16, "", ""},
	} {
		row := pullRow(t, h, repo, c.number)
		if row.TaskRef != c.task || row.LinkSource != c.source {
			t.Errorf("#%d linked to %q by %q, want %q by %q", c.number, row.TaskRef, row.LinkSource, c.task, c.source)
		}
	}
	// The first reference in the body is the one, bounded to the first match.
	if row := pullRow(t, h, repo, 12); row.TaskRef == d.Ref {
		t.Error("#12 linked to the second reference in its body")
	}
	// The task's mirror follows its first pull request; B has two, #11 and #14, and the first
	// linked is primary.
	if got, _ := h.store.GetTask(ctx, b.ID); got.PullRequestURL != "https://github.example/"+repo+"/pull/11" && got.PullRequestURL != "https://github.example/"+repo+"/pull/14" {
		t.Errorf("%s mirrors %q", b.Ref, got.PullRequestURL)
	}

	// A head scanned for a trailer is not scanned again.
	_, commitsBefore, _ := fake.counts()
	fake.update(16, func(pr map[string]any) { pr["body"] = "now it names " + d.Ref })
	if err := g.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, commitsAfter, _ := fake.counts(); commitsAfter != commitsBefore {
		t.Errorf("commits were fetched again for heads already scanned (%d -> %d)", commitsBefore, commitsAfter)
	}
	if row := pullRow(t, h, repo, 16); row.TaskRef != d.Ref || row.LinkSource != db.LinkBodyRef {
		t.Errorf("an edited description did not link: %q by %q", row.TaskRef, row.LinkSource)
	}

	// A person's link outranks them all, and stays through a poll that finds a trailer naming
	// another task.
	code, body, _ := pullCall(t, srv, h.bobTok, http.MethodPost, "/v1/tasks/"+a.Ref+"/pull-requests?project="+h.project.ID,
		map[string]any{"pull_request": "#16"})
	if code != http.StatusCreated || body["linked"] != true {
		t.Fatalf("manual link = %d %v", code, body)
	}
	fake.commits[16] = []string{"x\n\nConductor-Task: " + b.Ref}
	fake.update(16, func(pr map[string]any) { pr["head"].(map[string]any)["sha"] = "again" })
	if err := g.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if row := pullRow(t, h, repo, 16); row.TaskRef != a.Ref || row.LinkSource != db.LinkManual {
		t.Errorf("a trailer replaced a manual link: %q by %q", row.TaskRef, row.LinkSource)
	}
	// Unlinking sticks too.
	if code, _, _ := pullCall(t, srv, h.bobTok, http.MethodDelete, "/v1/tasks/"+a.Ref+"/pull-requests/"+repo+"/16?project="+h.project.ID, nil); code != http.StatusOK {
		t.Fatalf("unlink = %d", code)
	}
	fake.update(16, func(pr map[string]any) { pr["head"].(map[string]any)["sha"] = "third" })
	if err := g.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if row := pullRow(t, h, repo, 16); row.TaskRef != "" {
		t.Errorf("a pull request a person unlinked was linked again to %s", row.TaskRef)
	}
}

// A task with several pull requests is done when its primary merges or its last open one does.
func TestSeveralPullRequestsCompleteTheTaskByTheLastMerge(t *testing.T) {
	h := newHarness(t)
	g, fake, srv := newPullsIntegration(t, h)
	ctx := context.Background()
	repo := fake.name()
	inst := map[string]any{"id": 77}

	task := finishedTask(t, h, "", "internal/pulls/multi.go")
	fake.put(20, fake.pr(20, "agent/"+task.Ref+"/attempt-1"))
	fake.put(21, fake.pr(21, "feature/second"))
	if err := g.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// #21 is not on the task's branch; a person says it belongs to the task.
	if code, body, _ := pullCall(t, srv, h.aliceTok, http.MethodPost, "/v1/tasks/"+task.Ref+"/pull-requests?project="+h.project.ID,
		map[string]any{"pull_request": "21"}); code != http.StatusCreated {
		t.Fatalf("link #21 = %d %v", code, body)
	}
	if got, _ := h.store.GetTask(ctx, task.ID); got.PullRequestURL != "https://github.example/"+repo+"/pull/21" {
		t.Fatalf("the task's primary pull request is %q; a manual link becomes primary", got.PullRequestURL)
	}
	merge := func(number int) {
		t.Helper()
		pr := fake.end(number, true)
		if code := sendEvent(t, srv, "pull_request", map[string]any{"action": "closed", "installation": inst, "repository": repoPayload(fake), "pull_request": pr}); code != http.StatusOK {
			t.Fatalf("closed #%d = %d", number, code)
		}
	}

	merge(20) // not primary, another still open: the task waits
	if got, held := taskState(t, h, task.ID); got.Status != domain.TaskVerifying || held == 0 {
		t.Fatalf("after the first merge: %s, %d held", got.Status, held)
	}
	merge(21) // primary, and the last
	if got, held := taskState(t, h, task.ID); got.Status != domain.TaskDone || held != 0 {
		t.Fatalf("after the last merge: %s, %d held", got.Status, held)
	}
	if n := len(pullEventsOf(t, h, "github.pr_merged", 20)) + len(pullEventsOf(t, h, "github.pr_merged", 21)); n != 2 {
		t.Errorf("pr_merged events = %d, want one per pull request", n)
	}

	// Closed unmerged: the task goes back to the queue only when none of its pull requests is open.
	other := finishedTask(t, h, "", "internal/pulls/closed.go")
	fake.put(22, fake.pr(22, "agent/"+other.Ref+"/attempt-1"))
	fake.put(23, fake.pr(23, "agent/"+other.Ref+"/attempt-1"))
	if err := g.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	closeUnmerged := func(number int) {
		t.Helper()
		pr := fake.end(number, false)
		if code := sendEvent(t, srv, "pull_request", map[string]any{"action": "closed", "installation": inst, "repository": repoPayload(fake), "pull_request": pr}); code != http.StatusOK {
			t.Fatalf("closed #%d = %d", number, code)
		}
	}
	closeUnmerged(22)
	if got, _ := taskState(t, h, other.ID); got.Status != domain.TaskVerifying {
		t.Fatalf("closing one of two pull requests moved the task to %s", got.Status)
	}
	closeUnmerged(23)
	if got, held := taskState(t, h, other.ID); got.Status != domain.TaskReady || held != 0 {
		t.Fatalf("closing the last: %s, %d held", got.Status, held)
	}
}

// The poller records an end it never saw a webhook for, including a pull request that was
// opened and merged between two polls, and unlinked ones.
func TestPollerRecordsEndsOfPullRequestsItNeverSawClose(t *testing.T) {
	h := newHarness(t)
	g, fake, _ := newPullsIntegration(t, h)
	ctx := context.Background()
	repo := fake.name()
	poll := func() {
		t.Helper()
		if err := g.pollOnce(ctx); err != nil {
			t.Fatalf("poll: %v", err)
		}
	}

	task := finishedTask(t, h, "", "internal/pulls/vanish.go")
	fake.put(30, fake.pr(30, "agent/"+task.Ref+"/attempt-1"))
	fake.put(31, fake.pr(31, "feature/unlinked"))
	poll()
	if pullRow(t, h, repo, 31).State != "open" {
		t.Fatal("#31 is not open")
	}
	fake.end(30, true)
	fake.end(31, false)
	// #32 was opened and merged between the polls: it was never on an open list.
	late := fake.pr(32, "feature/late")
	late["state"], late["merged"], late["merged_at"] = "closed", true, time.Now().UTC().Format(time.RFC3339)
	fake.put(32, late)
	poll()
	if got, held := taskState(t, h, task.ID); got.Status != domain.TaskDone || held != 0 {
		t.Errorf("task after its pull request vanished merged: %s, %d held", got.Status, held)
	}
	if row := pullRow(t, h, repo, 30); row.State != "merged" {
		t.Errorf("#30 is %s", row.State)
	}
	if row := pullRow(t, h, repo, 31); row.State != "closed" || row.TaskID != "" {
		t.Errorf("#31 (unlinked) is %s", row.State)
	}
	if row := pullRow(t, h, repo, 32); row.State != "merged" {
		t.Errorf("#32 (never seen open) is %s", row.State)
	}
	// Seeing the ends again changes and announces nothing.
	poll()
	if n := len(pullEventsOf(t, h, "github.pr_merged", 30)); n != 1 {
		t.Errorf("pr_merged for #30 = %d, want 1", n)
	}
}

// seedPulls puts four pull requests in a project: one linked and approved, one unlinked and
// failing, one draft, one merged.
func seedPulls(t *testing.T, h *harness, repo string, task domain.Task) {
	t.Helper()
	ctx := context.Background()
	for n := 1; n <= 4; n++ {
		if _, _, err := h.store.UpsertPullRequest(ctx, db.PullRequestUpsert{ProjectID: h.project.ID, Repository: repo, Number: n,
			URL: "https://github.com/" + repo + "/pull/" + strconv.Itoa(n), State: db.PullRequestOpen, Title: "Pull " + strconv.Itoa(n),
			Draft: n == 3, HeadRef: "feature/" + strconv.Itoa(n), HeadSHA: "sha" + strconv.Itoa(n), BaseRef: "main", AuthorLogin: "octo"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.LinkPullRequestTask(ctx, h.project.ID, repo, 1, task.ID, db.LinkManual); err != nil {
		t.Fatal(err)
	}
	rollup := func(n int, r db.PullRequestRollup) {
		if _, _, err := h.store.SetPullRequestRollup(ctx, h.project.ID, repo, n, r); err != nil {
			t.Fatal(err)
		}
	}
	rollup(1, db.PullRequestRollup{ChecksTotal: 2, ChecksPassed: 2, ReviewDecision: "approved", Mergeable: "clean"})
	rollup(2, db.PullRequestRollup{ChecksTotal: 3, ChecksPassed: 1, ChecksFailed: 2, ReviewDecision: "review_required"})
	if _, err := h.store.EndPullRequest(ctx, h.project.ID, repo, 4, true); err != nil {
		t.Fatal(err)
	}
}

func TestPullsAPIListsFiltersAndShowsOnePullRequest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	task, err := h.store.CreateTask(ctx, db.CreateTaskParams{ProjectID: h.project.ID, CreatedBy: h.alice.ID, Title: "Linked", Status: domain.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	_, obsTok := h.member("olive", domain.RoleObserver)
	seedPulls(t, h, "acme/w", task)
	list := func(tok, query string) []map[string]any {
		t.Helper()
		code, body := h.do(tok, http.MethodGet, h.projectPath("/pulls"+query), nil)
		if code != http.StatusOK {
			t.Fatalf("GET /pulls%s = %d\n%s", query, code, body)
		}
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		return pullsIn(out)
	}
	numbers := func(ps []map[string]any) []int {
		var out []int
		for _, p := range ps {
			out = append(out, int(p["number"].(float64)))
		}
		return out
	}
	eq := func(name string, got []int, want ...int) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
		seen := map[int]bool{}
		for _, n := range got {
			seen[n] = true
		}
		for _, n := range want {
			if !seen[n] {
				t.Errorf("%s = %v, want %v", name, got, want)
				return
			}
		}
	}
	eq("all", numbers(list(obsTok, "")), 1, 2, 3, 4)
	eq("open", numbers(list(obsTok, "?state=open")), 1, 2, 3)
	eq("merged", numbers(list(obsTok, "?state=merged")), 4)
	eq("unlinked", numbers(list(obsTok, "?state=open&unlinked=1")), 2, 3)
	eq("needs review", numbers(list(obsTok, "?needs_review=1")), 2)
	eq("failing", numbers(list(obsTok, "?failing=1")), 2)
	eq("by task", numbers(list(obsTok, "?task="+task.Ref)), 1)
	eq("mine, as the task's owner", numbers(list(h.aliceTok, "?mine=1")), 1)
	eq("mine, as someone else", numbers(list(h.bobTok, "?mine=1")))
	eq("by number", numbers(list(obsTok, "?number=2")), 2)
	if got := list(obsTok, "?task="+task.Ref); got[0]["task_ref"] != task.Ref || got[0]["review_decision"] != "approved" ||
		got[0]["checks"].(map[string]any)["passed"] != float64(2) || got[0]["link_source"] != "manual" || got[0]["primary"] != true {
		t.Errorf("view = %v", got[0])
	}
	if code, _ := h.do(obsTok, http.MethodGet, h.projectPath("/pulls?state=sideways"), nil); code != http.StatusBadRequest {
		t.Errorf("an unknown state = %d, want 400", code)
	}
	if code, _ := h.do(obsTok, http.MethodGet, h.projectPath("/pulls?task=T-99999"), nil); code != http.StatusNotFound {
		t.Errorf("an unknown task = %d, want 404", code)
	}
	if code, _ := h.do("", http.MethodGet, h.projectPath("/pulls"), nil); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d, want 401", code)
	}

	// One pull request, with its task.
	code, body := h.do(obsTok, http.MethodGet, h.projectPath("/pulls/acme/w/1"), nil)
	var detail map[string]any
	_ = json.Unmarshal(body, &detail)
	if code != http.StatusOK || detail["pull"].(map[string]any)["title"] != "Pull 1" || detail["task"].(map[string]any)["ref"] != task.Ref {
		t.Errorf("detail = %d %s", code, body)
	}
	if code, _ := h.do(obsTok, http.MethodGet, h.projectPath("/pulls/acme/w/99"), nil); code != http.StatusNotFound {
		t.Errorf("unknown pull request = %d, want 404", code)
	}
	if code, _ := h.do(obsTok, http.MethodGet, h.projectPath("/pulls/acme/w/zero"), nil); code != http.StatusBadRequest {
		t.Errorf("a bad number = %d, want 400", code)
	}
	// Another project's members see none of this project's pull requests.
	other := h.secondProject("pulls-other")
	if _, _, err := h.store.UpsertPullRequest(ctx, db.PullRequestUpsert{ProjectID: other.ID, Repository: "acme/x", Number: 9, URL: "u", State: "open"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range list(h.aliceTok, "") {
		if p["repository"] == "acme/x" {
			t.Error("another project's pull request is in this project's list")
		}
	}
}

// A pull request of a private task: its owner sees all of it, everyone else its number and
// state, whichever way they ask.
func TestPrivateTaskPullRequestIsRedactedForOthers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	task, err := h.store.CreateTask(ctx, db.CreateTaskParams{ProjectID: h.project.ID, CreatedBy: h.alice.ID, Title: "ZQXPRIVATE", Status: domain.TaskReady, Visibility: domain.VisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}
	seedPulls(t, h, "acme/w", task)
	get := func(tok, path string) string {
		t.Helper()
		_, body := h.do(tok, http.MethodGet, h.projectPath(path), nil)
		return string(body)
	}
	// The list also holds pull requests of ordinary tasks, which everyone reads; only the
	// private task's own entry is narrowed.
	entryOf := func(body string) string {
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		for _, p := range pullsIn(out) {
			if p["number"] == float64(1) {
				b, _ := json.Marshal(p)
				return string(b)
			}
		}
		t.Fatalf("pull request 1 is not in\n%s", body)
		return ""
	}
	for _, c := range []struct {
		path string
		view func(string) string
	}{
		{"/pulls", entryOf}, {"/pulls?task=" + task.Ref, func(b string) string { return b }}, {"/pulls/acme/w/1", func(b string) string { return b }},
	} {
		own, other := c.view(get(h.aliceTok, c.path)), c.view(get(h.bobTok, c.path))
		if !strings.Contains(own, "Pull 1") || !strings.Contains(own, "feature/1") {
			t.Errorf("the owner does not see their own pull request at %s:\n%s", c.path, own)
		}
		for _, leak := range []string{"Pull 1", "feature/1", "sha1", "octo", task.Ref, "ZQXPRIVATE", "approved"} {
			if strings.Contains(other, leak) {
				t.Errorf("%s shows another member %q:\n%s", c.path, leak, other)
			}
		}
		if !strings.Contains(other, `"number":1`) || !strings.Contains(other, `"redacted":true`) {
			t.Errorf("%s hides even the number from another member:\n%s", c.path, other)
		}
	}
	// Linking someone else's private task is the owner's or a maintainer's to do.
	if code, _, _ := pullCall(t, h.server, h.bobTok, http.MethodPost, "/v1/tasks/"+task.ID+"/pull-requests", map[string]any{"pull_request": "acme/w#2"}); code != http.StatusForbidden {
		t.Errorf("a contributor linking another's private task = %d, want 403", code)
	}
	if code, _, _ := pullCall(t, h.server, h.aliceTok, http.MethodPost, "/v1/tasks/"+task.ID+"/pull-requests", map[string]any{"pull_request": "acme/w#2"}); code != http.StatusCreated {
		t.Errorf("the owner linking their own = %d, want 201", code)
	}
}

func TestLinkingByHandThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	task, err := h.store.CreateTask(ctx, db.CreateTaskParams{ProjectID: h.project.ID, CreatedBy: h.alice.ID, Title: "By hand", Status: domain.TaskReady})
	if err != nil {
		t.Fatal(err)
	}
	_, obsTok := h.member("olive", domain.RoleObserver)
	link := func(tok string, body map[string]any) (int, map[string]any) {
		code, out, _ := pullCall(t, h.server, tok, http.MethodPost, "/v1/tasks/"+task.ID+"/pull-requests", body)
		return code, out
	}

	// An observer reads; a contributor links.
	if code, _ := link(obsTok, map[string]any{"pull_request": "acme/w#5"}); code != http.StatusForbidden {
		t.Errorf("an observer linking = %d, want 403", code)
	}
	if code, out := link(h.bobTok, map[string]any{"pull_request": "acme/w#5"}); code != http.StatusCreated || out["linked"] != true || out["task_ref"] != task.Ref {
		t.Fatalf("link = %d %v", code, out)
	}
	if code, out := link(h.bobTok, map[string]any{"pull_request": "https://github.com/acme/w/pull/5"}); code != http.StatusOK || out["linked"] != false {
		t.Errorf("linking again = %d %v, want 200 and nothing changed", code, out)
	}
	if got, _ := h.store.GetTask(ctx, task.ID); got.PullRequestURL == "" || got.PullRequestState != "open" {
		t.Errorf("the task's mirror = %q %q", got.PullRequestURL, got.PullRequestState)
	}
	// A project linked to a repository holds that repository's pull requests, and a bare
	// number means its own.
	if err := h.store.SetProjectRemote(ctx, h.project.ID, "https://github.com/acme/w"); err != nil {
		t.Fatal(err)
	}
	if code, out := link(h.bobTok, map[string]any{"pull_request": "#6"}); code != http.StatusCreated || out["pull"].(map[string]any)["repository"] != "acme/w" {
		t.Errorf("a bare number = %d %v", code, out)
	}
	if code, out := link(h.bobTok, map[string]any{"pull_request": "someone/else#6"}); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "acme/w") {
		t.Errorf("another repository = %d %v, want 400 naming the project's", code, out)
	}
	for _, bad := range []map[string]any{{}, {"pull_request": "not a pull request"}, {"number": 0}, {"pull_request": "acme/w#0"}} {
		if code, _ := link(h.bobTok, bad); code != http.StatusBadRequest {
			t.Errorf("link %v = %d, want 400", bad, code)
		}
	}
	// Two tasks can have pull requests; a pull request has one task: moving it moves it.
	second, _ := h.store.CreateTask(ctx, db.CreateTaskParams{ProjectID: h.project.ID, CreatedBy: h.alice.ID, Title: "Takes it", Status: domain.TaskReady})
	if code, _, _ := pullCall(t, h.server, h.bobTok, http.MethodPost, "/v1/tasks/"+second.ID+"/pull-requests", map[string]any{"pull_request": "5"}); code != http.StatusCreated {
		t.Errorf("moving a pull request to another task = %d", code)
	}
	if got, _ := h.store.GetTask(ctx, task.ID); got.PullRequestURL == "https://github.com/acme/w/pull/5" {
		t.Error("the first task still mirrors the pull request that moved")
	}

	// Unlinking.
	del := func(tok string, n int) int {
		code, _, _ := pullCall(t, h.server, tok, http.MethodDelete, "/v1/tasks/"+second.ID+"/pull-requests/acme/w/"+strconv.Itoa(n), nil)
		return code
	}
	if code := del(obsTok, 5); code != http.StatusForbidden {
		t.Errorf("an observer unlinking = %d, want 403", code)
	}
	if code := del(h.bobTok, 5); code != http.StatusOK {
		t.Errorf("unlink = %d", code)
	}
	if code := del(h.bobTok, 5); code != http.StatusNotFound {
		t.Errorf("unlinking what is not linked = %d, want 404", code)
	}
	if got, _ := h.store.GetTask(ctx, second.ID); got.PullRequestURL != "" || got.PullRequestState != "" {
		t.Errorf("an unlinked task still mirrors %q", got.PullRequestURL)
	}
	// Both are in the audit trail, and the link announced itself.
	if n := len(pullEventsOf(t, h, "task.pull_request_linked", 5)); n < 2 {
		t.Errorf("task.pull_request_linked events for #5 = %d, want one per link", n)
	}
}

// With a GitHub App, a pull request a person names is read from GitHub — and one that does not
// exist is refused — and refreshing reads it again, rate limited.
func TestRefreshAndLinkReadFromGitHub(t *testing.T) {
	h := newHarness(t)
	g, fake, srv := newPullsIntegration(t, h)
	ctx := context.Background()
	repo := fake.name()
	_, obsTok := h.member("olive", domain.RoleObserver)

	task, _ := h.store.CreateTask(ctx, db.CreateTaskParams{ProjectID: h.project.ID, CreatedBy: h.alice.ID, Title: "Named by hand", Status: domain.TaskReady})
	fake.put(40, fake.pr(40, "feature/named"))
	fake.setRollup(40, rollupFixture{Decision: "REVIEW_REQUIRED", Checks: [][3]string{{"run", "COMPLETED", "SUCCESS"}}})
	code, out, _ := pullCall(t, srv, h.bobTok, http.MethodPost, "/v1/tasks/"+task.Ref+"/pull-requests?project="+h.project.ID, map[string]any{"pull_request": "40"})
	if code != http.StatusCreated {
		t.Fatalf("link = %d %v", code, out)
	}
	if row := pullRow(t, h, repo, 40); row.Title != "Pull request 40" || row.ReviewDecision != "review_required" || row.ChecksPassed != 1 || row.TaskRef != task.Ref {
		t.Errorf("a pull request named by hand was not read from GitHub: %+v", row)
	}
	if code, _, _ := pullCall(t, srv, h.bobTok, http.MethodPost, "/v1/tasks/"+task.Ref+"/pull-requests?project="+h.project.ID, map[string]any{"pull_request": "41"}); code != http.StatusNotFound {
		t.Errorf("a pull request GitHub does not have = %d, want 404", code)
	}

	// Refresh: GitHub is read now. An observer may not; a second read moments later waits.
	path := "/v1/projects/" + h.project.ID + "/pulls/" + repo + "/40/refresh"
	fake.setRollup(40, rollupFixture{Decision: "APPROVED", Threads: 3, Checks: [][3]string{{"run", "COMPLETED", "SUCCESS"}, {"run", "COMPLETED", "SUCCESS"}}})
	if code, _, _ := pullCall(t, srv, obsTok, http.MethodPost, path, nil); code != http.StatusForbidden {
		t.Errorf("an observer refreshing = %d, want 403", code)
	}
	code, out, _ = pullCall(t, srv, h.bobTok, http.MethodPost, path, nil)
	pull, _ := out["pull"].(map[string]any)
	if code != http.StatusOK || pull["review_decision"] != "approved" || pull["unresolved_threads"] != float64(3) {
		t.Fatalf("refresh = %d %v", code, out)
	}
	code, out, hdr := pullCall(t, srv, h.bobTok, http.MethodPost, path, nil)
	if code != http.StatusTooManyRequests || out["code"] != "rate_limited" || hdr.Get("Retry-After") == "" {
		t.Errorf("a second refresh at once = %d %v (Retry-After %q), want 429", code, out, hdr.Get("Retry-After"))
	}
	if code, _, _ := pullCall(t, srv, h.bobTok, http.MethodPost, "/v1/projects/"+h.project.ID+"/pulls/acme/other-repo/1/refresh", nil); code != http.StatusNotFound {
		t.Errorf("a repository the project is not linked to = %d, want 404", code)
	}

	// A pull request that ended since GitHub was last read ends in Conductor on refresh.
	merged := finishedTask(t, h, "", "internal/pulls/refresh.go")
	fake.put(42, fake.pr(42, "agent/"+merged.Ref+"/attempt-1"))
	if err := g.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if row := pullRow(t, h, repo, 42); row.TaskRef != merged.Ref {
		t.Fatalf("#42 is linked to %q", row.TaskRef)
	}
	fake.end(42, true)
	if code, out, _ := pullCall(t, srv, h.bobTok, http.MethodPost, "/v1/projects/"+h.project.ID+"/pulls/"+repo+"/42/refresh", nil); code != http.StatusOK {
		t.Fatalf("refresh after the merge = %d %v", code, out)
	}
	if got, held := taskState(t, h, merged.ID); got.Status != domain.TaskDone || held != 0 {
		t.Errorf("refreshing a merged pull request left its task %s with %d held", got.Status, held)
	}

	// Without an app, the list still works and only the refresh is unavailable.
	if code, _ := h.do(h.bobTok, http.MethodPost, h.projectPath("/pulls/"+repo+"/40/refresh"), nil); code != http.StatusConflict {
		t.Errorf("refresh with no GitHub App = %d, want 409", code)
	}
}

// Subscribing to a new delivery is not enough: the app must be asked to send it. The manifest
// asks for every event the pull request list reads, and for statuses.
func TestManifestAsksForTheEventsThePullRequestListReads(t *testing.T) {
	for _, event := range []string{"pull_request", "pull_request_review", "pull_request_review_comment", "issue_comment",
		"check_suite", "check_run", "status", "push"} {
		found := false
		for _, e := range githubapp.Events {
			found = found || e == event
		}
		if !found {
			t.Errorf("the app is not subscribed to %s", event)
		}
		if !webhookEvents[event] {
			t.Errorf("the webhook handler drops %s", event)
		}
	}
	if githubapp.Permissions["statuses"] != "read" {
		t.Errorf("statuses permission = %q, want read", githubapp.Permissions["statuses"])
	}
}
