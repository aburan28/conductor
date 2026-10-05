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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/githubapp"
)

// fakeGitHub serves the slice of GitHub's API the integration uses: the manifest conversion,
// installations, one repository with one pull request, and check-run creation.
type fakeGitHubAPI struct {
	t       *testing.T
	repo    string // unique per run: the database is shared across runs and packages
	pem     string
	mu      sync.Mutex
	checks  []map[string]any
	updates []map[string]any
	files   []string
}

func (f *fakeGitHubAPI) handler() http.Handler {
	m := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	m.HandleFunc("POST /app-manifests/{code}/conversions", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("code") != "good-code" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		write(w, map[string]any{"id": 1234, "slug": "conductor-test", "name": "Conductor test",
			"html_url": "https://github.example/apps/conductor-test", "webhook_secret": "whsec",
			"client_id": "Iv1", "client_secret": "cs", "pem": f.pem, "owner": map[string]any{"login": "acme"}})
	})
	m.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		write(w, []map[string]any{{"id": 77, "target_type": "Organization", "account": map[string]any{"login": "acme"}}})
	})
	m.HandleFunc("POST /app/installations/77/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	m.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"repositories": []map[string]any{{"name": f.repo, "full_name": "acme/" + f.repo, "private": true, "owner": map[string]any{"login": "acme"}}}})
	})
	m.HandleFunc("GET /repos/acme/{repo}", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"name": f.repo, "full_name": "acme/" + f.repo, "private": true, "owner": map[string]any{"login": "acme"}})
	})
	m.HandleFunc("GET /repos/acme/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"id": 77})
	})
	m.HandleFunc("GET /repos/acme/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		same := map[string]any{"full_name": "acme/" + f.repo}
		write(w, []map[string]any{{"number": 5, "state": "open", "head": map[string]any{"ref": "feature/retry", "sha": "abc123", "repo": same}, "base": map[string]any{"ref": "main", "repo": same}}})
	})
	m.HandleFunc("GET /repos/acme/{repo}/pulls/5/files", func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for _, f := range f.files {
			out = append(out, map[string]any{"filename": f})
		}
		write(w, out)
	})
	m.HandleFunc("POST /repos/acme/{repo}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghs_test" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.checks = append(f.checks, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		write(w, map[string]any{"id": len(f.checks)})
	})
	m.HandleFunc("PATCH /repos/acme/{repo}/check-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["id"] = r.PathValue("id")
		f.mu.Lock()
		f.updates = append(f.updates, body)
		f.mu.Unlock()
		write(w, map[string]any{"id": r.PathValue("id")})
	})
	return m
}

func (f *fakeGitHubAPI) checkUpdates() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.updates...)
}

// keepGitHubApp restores the stored app after a test: the row is one per database, and the
// database is shared across packages.
func keepGitHubApp(t *testing.T, store *db.Store) {
	t.Helper()
	before, found, err := store.GitHubApp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if found {
			_ = store.SaveGitHubApp(ctx, before)
		} else {
			_, _ = store.Pool().Exec(ctx, `DELETE FROM github_app`)
		}
	})
}

func (f *fakeGitHubAPI) checkRuns() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.checks...)
}

func TestGitHubAppEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeGitHubAPI{t: t, pem: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		files: []string{"internal/router/retry.go", "internal/api/handlers.go", "README.md"}, repo: "widgets-" + h.project.Slug}
	repoName := "acme/" + fake.repo
	gh := httptest.NewServer(fake.handler())
	defer gh.Close()

	// alice owns the machine; the settings row is shared, so put it back afterwards.
	before, _ := h.store.GetServerSettings(ctx)
	t.Cleanup(func() {
		_, _ = h.store.Pool().Exec(context.Background(),
			`UPDATE server_settings SET local_owner_id = NULLIF($1,'')::uuid WHERE singleton`, before.LocalOwnerID)
	})
	if _, err := h.store.SetLocalOwner(ctx, h.alice.ID, false); err != nil {
		t.Fatal(err)
	}
	keepGitHubApp(t, h.store)
	if _, err := h.store.Pool().Exec(ctx, `DELETE FROM github_app`); err != nil {
		t.Fatal(err)
	}

	credsPath := filepath.Join(t.TempDir(), "github-app.json")
	integration, err := NewGitHub(GitHubOptions{CredentialsPath: credsPath, API: gh.URL, Web: "https://github.example", Poll: -1,
		Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(h.store, coord.New(h.store), Options{GitHub: integration}).Handler())
	defer srv.Close()
	integration.opts.BaseURL = srv.URL
	// A second replica over the same database, with its own integration and no file.
	replica, err := NewGitHub(GitHubOptions{API: gh.URL, Web: "https://github.example", Poll: -1,
		Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(New(h.store, coord.New(h.store), Options{GitHub: replica}).Handler())
	defer srv2.Close()
	replica.opts.BaseURL = srv2.URL

	// --- Setup: only the machine's owner can start it.
	if code, _ := h.doJSONOn(srv, h.bobTok, http.MethodPost, "/v1/github/setup", map[string]string{}); code != http.StatusForbidden {
		t.Errorf("non-owner starting setup = %d, want 403", code)
	}
	code, started := h.doJSONOn(srv, h.aliceTok, http.MethodPost, "/v1/github/setup", map[string]string{"org": "acme"})
	if code != http.StatusCreated {
		t.Fatalf("setup = %d %v", code, started)
	}
	setupURL, _ := started["setup_url"].(string)
	u, _ := url.Parse(setupURL)
	state := u.Query().Get("state")
	page := httpGet(t, srv, "/github/setup?state="+state)
	if !strings.Contains(page, `action="https://github.example/organizations/acme/settings/apps/new?state=`+state) ||
		!strings.Contains(page, `name="manifest"`) || !strings.Contains(page, "default_permissions") {
		t.Errorf("setup page does not post a manifest to GitHub:\n%s", page)
	}
	if !strings.Contains(httpGet(t, srv, "/github/setup?state=forged"), "expired") {
		t.Error("a forged state rendered a setup form")
	}

	// --- Callback: bad state refused; good one stores the credentials; reuse refused. GitHub's
	// redirect lands on the other replica, which must know the setup the first one started.
	if code := httpStatus(t, srv2, "/github/callback?state=forged&code=good-code"); code != http.StatusBadRequest {
		t.Errorf("forged callback = %d", code)
	}
	if code := httpStatus(t, srv2, "/github/callback?state="+state+"&code=good-code"); code != http.StatusOK {
		t.Fatalf("callback on another replica = %d", code)
	}
	if _, err := os.Stat(credsPath); !os.IsNotExist(err) {
		t.Errorf("the callback wrote a local credentials file (%v); the app belongs in the database", err)
	}
	if raw, found, err := h.store.GitHubApp(ctx); err != nil || !found || !strings.Contains(string(raw), `"slug": "conductor-test"`) {
		// Not the raw row: it holds the app's private key.
		t.Fatalf("stored app found=%v err=%v", found, err)
	}
	if !replica.Configured() {
		t.Fatal("the replica that took the callback is not configured")
	}
	// The replica that started setup picks the app up from the database.
	if err := integration.Refresh(ctx); err != nil || !integration.Configured() {
		t.Fatalf("the first replica did not load the stored app: %v", err)
	}
	if code := httpStatus(t, srv, "/github/callback?state="+state+"&code=good-code"); code != http.StatusBadRequest {
		t.Errorf("replayed callback = %d", code)
	}
	// Replacing a connected app must be asked for explicitly.
	if code, _ := h.doJSONOn(srv, h.aliceTok, http.MethodPost, "/v1/github/setup", map[string]any{}); code != http.StatusConflict {
		t.Errorf("setup over a connected app without replace = %d, want 409", code)
	}
	if code, _ := h.doJSONOn(srv, h.aliceTok, http.MethodPost, "/v1/github/setup", map[string]any{"replace": true}); code != http.StatusCreated {
		t.Errorf("setup with replace = %d, want 201", code)
	}

	// --- Another organization on the same control plane cannot link to the owner's app.
	otherOrg, err := h.store.CreateOrganization(ctx, uniq("gh-other", time.Now().UnixNano()), "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherProj, err := h.store.CreateProject(ctx, db.CreateProjectParams{OrganizationID: otherOrg.ID, Slug: uniq("gh-other-p", time.Now().UnixNano()), Config: domain.DefaultProjectConfig()})
	if err != nil {
		t.Fatal(err)
	}
	carol, err := h.store.CreatePrincipal(ctx, otherOrg.ID, domain.PrincipalHuman, "carol", "carol", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = h.store.AddMember(ctx, otherProj.ID, carol.ID, domain.RoleProjectAdmin)
	carolTok, _ := h.store.CreateToken(ctx, carol.ID, "test", 0)
	if code, _ := h.doJSONOn(srv, carolTok, http.MethodPost, "/v1/projects/"+otherProj.ID+"/github", map[string]string{"repository": repoName}); code != http.StatusForbidden {
		t.Errorf("another organization linking = %d, want 403", code)
	}
	if code, st := h.doJSONOn(srv, carolTok, http.MethodGet, "/v1/github/status", nil); code != http.StatusOK || st["app"] != nil || st["available"] != false {
		t.Errorf("another organization's status = %d %v; the app is not theirs to inspect", code, st)
	}
	if code, _ := h.doJSONOn(srv, carolTok, http.MethodPost, "/v1/security", map[string]string{"security_mode": "enhanced"}); code != http.StatusForbidden {
		t.Errorf("another organization's admin tightening the owner's machine = %d, want 403", code)
	}

	// --- Link the project to the repository.
	if code, _ := h.doJSONOn(srv, h.bobTok, http.MethodPost, h.projectPath("/github"), map[string]string{"repository": repoName}); code != http.StatusForbidden {
		t.Errorf("contributor linking = %d, want 403", code)
	}
	if code, body := h.doJSONOn(srv, h.aliceTok, http.MethodPost, h.projectPath("/github"), map[string]string{"repository": repoName}); code != http.StatusOK {
		t.Fatalf("link = %d %v", code, body)
	}

	// --- bob holds the router directory for work whose title must never reach GitHub.
	const secretTitle = "quietly rewrite the router for the acquisition"
	code, raw := h.do(h.bobTok, http.MethodPost, h.projectPath("/work/start"), map[string]any{
		"summary": secretTitle, "visibility": "private",
		"scopes": []map[string]any{{"resource": "dir:internal/router", "mode": "write_exclusive"}},
	})
	if code != http.StatusOK {
		t.Fatalf("bob start = %d %s", code, raw)
	}
	// alice holds a team-visible file: in a private repository, her task is named.
	code, raw = h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"), map[string]any{
		"summary": "tidy the API handlers", "visibility": "team_summary",
		"scopes": []map[string]any{{"resource": "path:internal/api/handlers.go", "mode": "write_exclusive"}},
	})
	if code != http.StatusOK {
		t.Fatalf("alice start = %d %s", code, raw)
	}

	// --- Check the pull request on demand.
	if code, _ := h.doJSONOn(srv, h.outTok, http.MethodPost, "/v1/github/check", map[string]any{"repository": repoName, "number": 5}); code != http.StatusNotFound {
		t.Errorf("non-member check = %d, want 404", code)
	}
	code, rep := h.doJSONOn(srv, h.aliceTok, http.MethodPost, "/v1/github/check", map[string]any{"repository": repoName, "number": 5})
	if code != http.StatusOK {
		t.Fatalf("check = %d %v", code, rep)
	}
	if rep["conclusion"] != "neutral" || rep["posted"] != true {
		t.Errorf("report = %v", rep)
	}
	runs := fake.checkRuns()
	if len(runs) != 1 {
		t.Fatalf("%d check runs posted, want 1", len(runs))
	}
	out := runs[0]["output"].(map[string]any)
	text := out["text"].(string) + out["summary"].(string) + out["title"].(string)
	if runs[0]["head_sha"] != "abc123" || runs[0]["name"] != githubapp.CheckName || !strings.Contains(text, "internal/router/retry.go") {
		t.Errorf("check run = %v", runs[0])
	}
	if strings.Contains(text, "acquisition") || strings.Contains(text, "rewrite the router") || strings.Contains(text, "tidy the API") {
		t.Errorf("a task title reached GitHub:\n%s", text)
	}
	// bob's task is private: the file is reported, his name and task ref are not.
	if strings.Contains(text, "bob") || !strings.Contains(text, "a private task") {
		t.Errorf("a private task was identified on GitHub:\n%s", text)
	}
	if !strings.Contains(text, "alice") || !strings.Contains(text, "internal/api/handlers.go") {
		t.Errorf("a team-visible task in a private repository was not named:\n%s", text)
	}
	if strings.Contains(text, "README.md") {
		t.Error("an unreserved file was reported as an overlap")
	}

	// The same result for the same commit is not posted twice.
	if code, rep := h.doJSONOn(srv, h.aliceTok, http.MethodPost, "/v1/github/check", map[string]any{"repository": repoName, "number": 5}); code != http.StatusOK || rep["posted"] != false {
		t.Errorf("repeat check = %d %v", code, rep)
	}

	// --- Polling finds the same pull request and has nothing new to say — on this replica,
	// on the other, or after a restart (a fresh integration with nothing in memory).
	if err := integration.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if err := replica.pollOnce(ctx); err != nil {
		t.Fatalf("poll on the replica: %v", err)
	}
	restarted, _ := NewGitHub(GitHubOptions{API: gh.URL, Poll: -1, Getenv: func(string) string { return "" }})
	_ = New(h.store, coord.New(h.store), Options{GitHub: restarted})
	if err := restarted.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.pollOnce(ctx); err != nil {
		t.Fatalf("poll after a restart: %v", err)
	}
	if n := len(fake.checkRuns()); n != 1 || len(fake.checkUpdates()) != 0 {
		t.Errorf("an unchanged pull request was posted again (%d runs, %d updates)", n, len(fake.checkUpdates()))
	}

	// --- A changed result updates the commit's run instead of stacking a second one.
	code, raw = h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"), map[string]any{
		"summary": "touch the readme", "visibility": "team_summary",
		"scopes": []map[string]any{{"resource": "path:README.md", "mode": "write_exclusive"}},
	})
	if code != http.StatusOK {
		t.Fatalf("start = %d %s", code, raw)
	}
	if err := replica.pollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if ups := fake.checkUpdates(); len(fake.checkRuns()) != 1 || len(ups) != 1 || ups[0]["id"] != "1" ||
		!strings.Contains(toJSON(ups[0]["output"]), "README.md") {
		t.Errorf("a changed result: %d runs, updates %v", len(fake.checkRuns()), ups)
	}

	// --- Webhooks: unsigned refused, signed accepted.
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	if code := webhook(t, srv, "ping", body, "sha256=00"); code != http.StatusUnauthorized {
		t.Errorf("unsigned webhook = %d", code)
	}
	if code := webhook(t, srv, "ping", body, githubapp.Sign("whsec", body)); code != http.StatusOK {
		t.Errorf("signed ping = %d", code)
	}
	prEvent, _ := json.Marshal(map[string]any{"action": "synchronize", "installation": map[string]any{"id": 77},
		"repository": map[string]any{"name": fake.repo, "private": true, "owner": map[string]any{"login": "acme"}},
		"pull_request": map[string]any{"number": 5,
			"head": map[string]any{"ref": "feature/retry", "sha": "def456", "repo": map[string]any{"full_name": "acme/" + fake.repo}},
			"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "acme/" + fake.repo}}}})
	if code := webhook(t, srv, "pull_request", prEvent, githubapp.Sign("whsec", prEvent)); code != http.StatusAccepted {
		t.Errorf("signed pull_request = %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(fake.checkRuns()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if runs := fake.checkRuns(); len(runs) != 2 || runs[1]["head_sha"] != "def456" {
		t.Errorf("the webhook did not check the new commit: %d runs", len(runs))
	}

	// --- Status shows the app and the link.
	code, st := h.doJSONOn(srv, h.aliceTok, http.MethodGet, "/v1/github/status", nil)
	if code != http.StatusOK || st["configured"] != true || !strings.Contains(toJSON(st["linked"]), repoName) {
		t.Errorf("status = %d %v", code, st)
	}
}

func httpGet(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func httpStatus(t *testing.T, srv *httptest.Server, path string) int {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func webhook(t *testing.T, srv *httptest.Server, event string, body []byte, sig string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/github/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sig)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A public repository gets no names; a fork's branch name never claims a task.
func TestPullReportRedaction(t *testing.T) {
	rep := PullReport{Files: 2, Public: true, Overlaps: []pullOverlap{{File: "a.go", Resource: "path:a.go", Mode: "write_exclusive"}}}
	_, _, text := renderPullReport(rep)
	if !strings.Contains(text, "in-flight work") || !strings.Contains(text, "public repository") {
		t.Errorf("public report:\n%s", text)
	}
	var pr githubapp.PullRequest
	pr.Head.Repo.FullName, pr.Base.Repo.FullName = "mallory/widgets", "acme/widgets"
	if !pr.FromFork() {
		t.Error("a fork was not recognised")
	}
	pr.Head.Repo.FullName = "acme/widgets"
	if pr.FromFork() {
		t.Error("a same-repository branch was taken for a fork")
	}
}

// An app set up before credentials lived in the database is imported from its file once;
// an app stored since is never replaced by a file.
func TestGitHubAppImportsLegacyFileOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	keepGitHubApp(t, h.store)
	if _, err := h.store.Pool().Exec(ctx, `DELETE FROM github_app`); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	dir := t.TempDir()
	legacy := filepath.Join(dir, "github-app.json")
	if err := githubapp.Save(legacy, githubapp.Credentials{AppID: 41, Slug: "legacy", PrivateKeyPEM: pemKey}); err != nil {
		t.Fatal(err)
	}
	noEnv := func(string) string { return "" }
	g, err := NewGitHub(GitHubOptions{CredentialsPath: legacy, Poll: -1, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	_ = New(h.store, coord.New(h.store), Options{GitHub: g})
	if err := g.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	var source, slug string
	if err := h.store.Pool().QueryRow(ctx, `SELECT source, credentials->>'slug' FROM github_app`).Scan(&source, &slug); err != nil || source != "imported" || slug != "legacy" {
		t.Fatalf("after import: source=%q slug=%q err=%v", source, slug, err)
	}

	// Another host still has an older file; the database's app wins.
	other := filepath.Join(dir, "other.json")
	if err := githubapp.Save(other, githubapp.Credentials{AppID: 99, Slug: "stale", PrivateKeyPEM: pemKey}); err != nil {
		t.Fatal(err)
	}
	g2, _ := NewGitHub(GitHubOptions{CredentialsPath: other, Poll: -1, Getenv: noEnv})
	_ = New(h.store, coord.New(h.store), Options{GitHub: g2})
	if err := g2.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if c := g2.appClient(); c == nil || c.Credentials().Slug != "legacy" {
		t.Errorf("a stale file replaced the stored app: %+v", c)
	}
}

// One replica polls at a time, and a replica that just polled is not followed by another.
func TestGitHubPollerIsExclusive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	g, err := NewGitHub(GitHubOptions{Poll: -1, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	_ = New(h.store, coord.New(h.store), Options{GitHub: g})

	// Another replica is polling right now.
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.store.TryExclusive(ctx, pollerLock, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	if outcome, err := g.pollExclusive(ctx, 0); outcome != "skipped" || err != nil {
		t.Errorf("poll while another replica holds the poller = %s %v", outcome, err)
	}
	close(release)
	<-done

	// It just finished: a timed poll defers to it.
	if err := h.store.RecordHeartbeat(ctx, db.ComponentGitHubPoller, "other", ""); err != nil {
		t.Fatal(err)
	}
	if outcome, err := g.pollExclusive(ctx, time.Minute); outcome != "skipped" || err != nil {
		t.Errorf("poll right after another replica's = %s %v", outcome, err)
	}
}
