package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/githubapp"
	"github.com/aburan28/conductor/internal/secretbox"
)

// fakeGitHub serves the slice of GitHub's API the integration uses: the manifest conversion,
// installations, one repository with one pull request, check-run creation, and the issues
// that issue sync reads and writes (github_issues_test.go).
type fakeGitHubAPI struct {
	t       *testing.T
	repo    string // unique per run: the database is shared across runs and packages
	pem     string
	mu      sync.Mutex
	checks  []map[string]any
	updates []map[string]any
	files   []string

	// public makes the repository public.
	public bool
	// appPerms and instPerms are the permissions the app and its installation hold; nil
	// means everything Conductor asks for.
	appPerms, instPerms map[string]string
	issues              map[int]map[string]any
	// issueWrites records every write to an issue, as "comment 3: text", "label+ 3: name",
	// "label- 3: name", or "close 3".
	issueWrites []string
	// notModified counts issue listings answered 304.
	notModified int
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
	perms := func(p map[string]string) map[string]string {
		if p == nil {
			return githubapp.Permissions
		}
		return p
	}
	m.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		write(w, map[string]any{"id": 1234, "slug": "conductor-test", "permissions": perms(f.appPerms),
			"owner": map[string]any{"login": "acme", "type": "Organization"}})
	})
	m.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		write(w, []map[string]any{{"id": 77, "target_type": "Organization", "account": map[string]any{"login": "acme"},
			"html_url": "https://github.example/organizations/acme/settings/installations/77", "permissions": perms(f.instPerms)}})
	})
	m.HandleFunc("POST /app/installations/77/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	m.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"repositories": []map[string]any{{"name": f.repo, "full_name": "acme/" + f.repo, "private": !f.public, "owner": map[string]any{"login": "acme"}}}})
	})
	m.HandleFunc("GET /repos/acme/{repo}", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"name": f.repo, "full_name": "acme/" + f.repo, "private": !f.public, "owner": map[string]any{"login": "acme"}})
	})
	f.issueHandlers(m, write)
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

// issueHandlers serves the issues endpoints from f.issues, the way GitHub does: listings
// oldest change first with an ETag, a 304 for an unchanged listing, and a 403 for an
// installation that has not accepted the issues permission.
func (f *fakeGitHubAPI) issueHandlers(m *http.ServeMux, write func(http.ResponseWriter, any)) {
	allowed := func(w http.ResponseWriter, level string) bool {
		if f.instPerms != nil && !githubapp.Grants(f.instPerms, "issues", level) {
			w.WriteHeader(http.StatusForbidden)
			write(w, map[string]any{"message": "Resource not accessible by integration"})
			return false
		}
		return true
	}
	number := func(r *http.Request) int {
		n, _ := strconv.Atoi(r.PathValue("n"))
		return n
	}
	m.HandleFunc("GET /repos/acme/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !allowed(w, "read") {
			return
		}
		state := r.URL.Query().Get("state")
		since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		list := []map[string]any{}
		for _, is := range f.issues {
			if state == "open" && is["state"] != "open" {
				continue
			}
			if at, _ := time.Parse(time.RFC3339Nano, is["updated_at"].(string)); at.Before(since) {
				continue
			}
			list = append(list, is)
		}
		sort.Slice(list, func(i, j int) bool { return list[i]["updated_at"].(string) < list[j]["updated_at"].(string) })
		body, _ := json.Marshal(list)
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		if r.Header.Get("If-None-Match") == etag {
			f.notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write(body)
	})
	m.HandleFunc("GET /repos/acme/{repo}/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		is, ok := f.issues[number(r)]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		write(w, is)
	})
	m.HandleFunc("PATCH /repos/acme/{repo}/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !allowed(w, "write") {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["state"] == "closed" {
			f.issueWrites = append(f.issueWrites, fmt.Sprintf("close %d", number(r)))
			f.issues[number(r)]["state"] = "closed"
			f.issues[number(r)]["updated_at"] = fakeNow()
		}
		write(w, f.issues[number(r)])
	})
	m.HandleFunc("POST /repos/acme/{repo}/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !allowed(w, "write") {
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.issueWrites = append(f.issueWrites, fmt.Sprintf("comment %d: %s", number(r), body["body"]))
		w.WriteHeader(http.StatusCreated)
		write(w, map[string]any{"id": len(f.issueWrites)})
	})
	m.HandleFunc("POST /repos/acme/{repo}/issues/{n}/labels", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !allowed(w, "write") {
			return
		}
		var body map[string][]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, l := range body["labels"] {
			f.issueWrites = append(f.issueWrites, fmt.Sprintf("label+ %d: %s", number(r), l))
		}
		write(w, []any{})
	})
	m.HandleFunc("DELETE /repos/acme/{repo}/issues/{n}/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !allowed(w, "write") {
			return
		}
		f.issueWrites = append(f.issueWrites, fmt.Sprintf("label- %d: %s", number(r), r.PathValue("name")))
		write(w, []any{})
	})
}

// fakeNow is an updated_at as GitHub writes one. Nanoseconds keep two changes in one test
// apart, which is all the ordering the sync relies on.
func fakeNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// setIssue creates or replaces an issue, stamping it as changed now.
func (f *fakeGitHubAPI) setIssue(number int, title, body, state string, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issues == nil {
		f.issues = map[int]map[string]any{}
	}
	ls := []map[string]any{}
	for _, l := range labels {
		ls = append(ls, map[string]any{"name": l})
	}
	f.issues[number] = map[string]any{"number": number, "title": title, "body": body, "state": state,
		"labels": ls, "updated_at": fakeNow(),
		"html_url": "https://github.example/acme/" + f.repo + "/issues/" + strconv.Itoa(number)}
}

func (f *fakeGitHubAPI) issue(number int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{}
	for k, v := range f.issues[number] {
		out[k] = v
	}
	return out
}

func (f *fakeGitHubAPI) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.issueWrites...)
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
	// Every replica shares one secret key, as a deployment must.
	sharedKey := testSecretKey(t)
	integration, err := NewGitHub(GitHubOptions{CredentialsPath: credsPath, SecretKey: sharedKey, API: gh.URL, Web: "https://github.example", Poll: -1,
		Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(h.store, coord.New(h.store), Options{GitHub: integration}).Handler())
	defer srv.Close()
	integration.opts.BaseURL = srv.URL
	// A second replica over the same database, with its own integration and no file.
	replica, err := NewGitHub(GitHubOptions{SecretKey: sharedKey, API: gh.URL, Web: "https://github.example", Poll: -1,
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
	stored, found, err := h.store.GitHubApp(ctx)
	if err != nil || !found || !strings.Contains(string(stored), `"slug": "conductor-test"`) || !strings.Contains(string(stored), `"sealed"`) {
		t.Fatalf("stored app found=%v err=%v", found, err)
	}
	// The row names the app but holds none of its secrets in the clear.
	for _, secret := range []string{"PRIVATE KEY", `"whsec"`, `"cs"`, `"pem"`} {
		if strings.Contains(string(stored), secret) {
			t.Errorf("the stored app row contains %s in plaintext", secret)
		}
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
	restarted, _ := NewGitHub(GitHubOptions{SecretKey: sharedKey, API: gh.URL, Poll: -1, Getenv: func(string) string { return "" }})
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
	sealKey := testSecretKey(t)
	g, err := NewGitHub(GitHubOptions{CredentialsPath: legacy, SecretKey: sealKey, Poll: -1, Getenv: noEnv})
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
	var sealed, hasPEM bool
	if err := h.store.Pool().QueryRow(ctx, `SELECT credentials ? 'sealed', credentials ? 'pem' FROM github_app`).Scan(&sealed, &hasPEM); err != nil || !sealed || hasPEM {
		t.Errorf("the imported app is not sealed: sealed=%v pem=%v err=%v", sealed, hasPEM, err)
	}

	// Another host still has an older file; the database's app wins.
	other := filepath.Join(dir, "other.json")
	if err := githubapp.Save(other, githubapp.Credentials{AppID: 99, Slug: "stale", PrivateKeyPEM: pemKey}); err != nil {
		t.Fatal(err)
	}
	g2, _ := NewGitHub(GitHubOptions{CredentialsPath: other, SecretKey: sealKey, Poll: -1, Getenv: noEnv})
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

func testSecretKey(t *testing.T) *secretbox.Source {
	t.Helper()
	return &secretbox.Source{Path: filepath.Join(t.TempDir(), "secret.key")}
}

func testAppPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// A row stored before secrets were sealed is still served, and is sealed in place.
func TestGitHubAppLegacyPlaintextRowIsSealed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	keepGitHubApp(t, h.store)
	plain, _ := json.Marshal(githubapp.Credentials{AppID: 51, Slug: "plain", PrivateKeyPEM: testAppPEM(t), WebhookSecret: "legacy-whsec"})
	if err := h.store.SaveGitHubApp(ctx, plain); err != nil {
		t.Fatal(err)
	}
	g, err := NewGitHub(GitHubOptions{SecretKey: testSecretKey(t), Poll: -1, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	_ = New(h.store, coord.New(h.store), Options{GitHub: g})
	if err := g.Refresh(ctx); err != nil {
		t.Fatalf("reading a plaintext row: %v", err)
	}
	if c := g.appClient(); c == nil || c.Credentials().Slug != "plain" || c.Credentials().WebhookSecret != "legacy-whsec" {
		t.Fatalf("plaintext row served %+v", c)
	}
	raw, _, _ := h.store.GitHubApp(ctx)
	if !strings.Contains(string(raw), `"sealed"`) || strings.Contains(string(raw), "legacy-whsec") || strings.Contains(string(raw), "PRIVATE KEY") {
		t.Error("the plaintext row was not sealed after it was read")
	}
	// And the sealed row reads back the same app.
	if err := g.Refresh(ctx); err != nil || g.appClient().Credentials().WebhookSecret != "legacy-whsec" {
		t.Errorf("sealed row read back: %v", err)
	}
}

// A replica with a different key (or none) cannot read the app, and says what to do.
func TestGitHubAppWrongKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	keepGitHubApp(t, h.store)
	right := testSecretKey(t)
	sealed, err := sealCredentials(right, githubapp.Credentials{AppID: 61, Slug: "sealed", PrivateKeyPEM: testAppPEM(t), WebhookSecret: "w"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveGitHubApp(ctx, sealed); err != nil {
		t.Fatal(err)
	}
	rightKey, _ := right.Get(false)

	for name, src := range map[string]*secretbox.Source{
		"another key":  testSecretKey(t),
		"missing file": {Path: filepath.Join(t.TempDir(), "absent.key")},
	} {
		if name == "another key" {
			if _, err := src.Get(true); err != nil {
				t.Fatal(err)
			}
		}
		g, _ := NewGitHub(GitHubOptions{SecretKey: src, Poll: -1, Getenv: func(string) string { return "" }})
		_ = New(h.store, coord.New(h.store), Options{GitHub: g})
		err := g.Refresh(ctx)
		if err == nil {
			t.Errorf("%s: the app opened", name)
			continue
		}
		for _, want := range []string{"cannot be unsealed", rightKey.ID(), "same secret key", "conductor github setup --replace"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error lacks %q: %v", name, want, err)
			}
		}
		if g.Configured() {
			t.Errorf("%s: an unreadable app counts as configured", name)
		}
	}
	// The key that sealed it still opens it.
	g, _ := NewGitHub(GitHubOptions{SecretKey: right, Poll: -1, Getenv: func(string) string { return "" }})
	_ = New(h.store, coord.New(h.store), Options{GitHub: g})
	if err := g.Refresh(ctx); err != nil || !g.Configured() {
		t.Errorf("the right key: %v", err)
	}
}
