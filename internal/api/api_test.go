package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// Handler-level tests. The store's own suite proves the invariants; these prove the HTTP
// layer does not undo them — that membership is checked, that the privacy projection survives
// serialization, and that a stale fence becomes a 409 rather than a 500.

var (
	sharedStore *db.Store
	storeOnce   sync.Once
	storeErr    error
)

type harness struct {
	t        *testing.T
	server   *httptest.Server
	store    *db.Store
	org      domain.Organization
	project  domain.Project
	alice    domain.Principal
	bob      domain.Principal
	outsider domain.Principal
	aliceTok string
	bobTok   string
	outTok   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping API integration tests")
	}
	ctx := context.Background()
	storeOnce.Do(func() {
		sharedStore, storeErr = db.Open(ctx, dsn)
		if storeErr == nil {
			storeErr = sharedStore.Migrate(ctx)
		}
	})
	if storeErr != nil {
		t.Fatalf("store: %v", storeErr)
	}

	suffix := time.Now().UnixNano()
	h := &harness{t: t, store: sharedStore}

	var err error
	h.org, err = sharedStore.CreateOrganization(ctx, uniq("api-org", suffix), "API Test")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	h.project, err = sharedStore.CreateProject(ctx, db.CreateProjectParams{
		OrganizationID: h.org.ID, Slug: uniq("api-proj", suffix),
		Config: domain.DefaultProjectConfig(),
	})
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	mk := func(handle string, role domain.Role, member bool) (domain.Principal, string) {
		p, err := sharedStore.CreatePrincipal(ctx, h.org.ID, domain.PrincipalHuman, handle, handle, "")
		if err != nil {
			t.Fatalf("principal %s: %v", handle, err)
		}
		if member {
			if err := sharedStore.AddMember(ctx, h.project.ID, p.ID, role); err != nil {
				t.Fatalf("membership %s: %v", handle, err)
			}
		}
		tok, err := sharedStore.CreateToken(ctx, p.ID, "test", 0)
		if err != nil {
			t.Fatalf("token %s: %v", handle, err)
		}
		return p, tok
	}

	h.alice, h.aliceTok = mk("alice", domain.RoleProjectAdmin, true)
	h.bob, h.bobTok = mk("bob", domain.RoleContributor, true)
	// A principal in the same organization with no membership in this project.
	h.outsider, h.outTok = mk("outsider", domain.RoleContributor, false)

	srv := New(sharedStore, coord.New(sharedStore), Options{})
	h.server = httptest.NewServer(srv.Handler())
	t.Cleanup(h.server.Close)
	return h
}

func uniq(prefix string, n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buf []byte
	for n > 0 {
		buf = append([]byte{digits[n%36]}, buf...)
		n /= 36
	}
	return prefix + "-" + string(buf)
}

func (h *harness) do(token, method, path string, body any) (int, []byte) {
	h.t.Helper()
	var reader *strings.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal: %v", err)
		}
		reader = strings.NewReader(string(encoded))
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, buf
}

func (h *harness) projectPath(suffix string) string {
	return "/v1/projects/" + h.project.ID + suffix
}

// ---------------------------------------------------------------------------
// Authentication and membership
// ---------------------------------------------------------------------------

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/v1/whoami", "/v1/projects", h.projectPath("/tasks"),
		h.projectPath("/presence"), h.projectPath("/status"),
	} {
		if code, _ := h.do("", http.MethodGet, path, nil); code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, code)
		}
	}
}

func TestHealthNeedsNoToken(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do("", http.MethodGet, "/v1/health", nil); code != http.StatusOK {
		t.Errorf("health = %d, want 200", code)
	}
}

// A valid credential for the wrong project must be indistinguishable from a missing project.
// Returning 403 would confirm the project exists, which is itself a disclosure.
func TestNonMemberSeesNotFoundNotForbidden(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.outTok, http.MethodGet, h.projectPath("/tasks"), nil)
	if code != http.StatusNotFound {
		t.Errorf("non-member GET tasks = %d, want 404 (not 403)\n%s", code, body)
	}
}

func TestContributorCannotInvite(t *testing.T) {
	h := newHarness(t)
	code, _ := h.do(h.bobTok, http.MethodPost, h.projectPath("/members"),
		map[string]any{"handle": "mallory", "role": "contributor"})
	if code != http.StatusForbidden {
		t.Errorf("contributor invite = %d, want 403", code)
	}
}

func TestProjectAdminCannotGrantOrgAdmin(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/members"),
		map[string]any{"handle": "escalated", "role": "org_admin"})
	if code != http.StatusForbidden {
		t.Errorf("privilege escalation = %d, want 403\n%s", code, body)
	}
}

func TestInviteMintsAWorkingToken(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/members"),
		map[string]any{"handle": "rachel", "role": "contributor"})
	if code != http.StatusCreated {
		t.Fatalf("invite = %d\n%s", code, body)
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Token == "" {
		t.Fatal("invite returned no token")
	}
	if code, _ := h.do(result.Token, http.MethodGet, h.projectPath("/tasks"), nil); code != http.StatusOK {
		t.Errorf("minted token cannot read tasks: %d", code)
	}
}

// ---------------------------------------------------------------------------
// Privacy through the HTTP layer
// ---------------------------------------------------------------------------

// The store-level projection is tested in internal/privacy. This asserts it survives
// serialization — that the JSON a teammate actually receives has no title in it.
func TestPrivateTaskIsRedactedOverHTTP(t *testing.T) {
	h := newHarness(t)

	const secret = "Rotate the leaked production key"
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/tasks"), map[string]any{
		"title": secret, "objective": "credentials exposed",
		"visibility": "private", "status": "ready",
		"scopes": []map[string]any{{"resource": "dir:internal/billing", "mode": "write_exclusive"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d\n%s", code, body)
	}

	code, body = h.do(h.bobTok, http.MethodGet, h.projectPath("/tasks"), nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d\n%s", code, body)
	}
	if strings.Contains(string(body), secret) {
		t.Errorf("a private task's title reached another member:\n%s", body)
	}
	// Territory must still be visible, or the collision it exists to prevent can happen.
	if !strings.Contains(string(body), "dir:internal/billing") {
		t.Errorf("private task hid its territory, so collisions are possible:\n%s", body)
	}
	if !strings.Contains(string(body), "alice") {
		t.Errorf("private task hid its owner:\n%s", body)
	}
}

func TestEventStreamCarriesNoPromptFields(t *testing.T) {
	h := newHarness(t)
	code, _ := h.do(h.aliceTok, http.MethodPost, h.projectPath("/tasks"),
		map[string]any{"title": "eventful", "status": "ready"})
	if code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	code, body := h.do(h.bobTok, http.MethodGet, h.projectPath("/events"), nil)
	if code != http.StatusOK {
		t.Fatalf("events = %d", code)
	}
	for _, banned := range []string{`"prompt"`, `"transcript"`, `"messages"`, `"reasoning"`} {
		if strings.Contains(string(body), banned) {
			t.Errorf("event stream contains %s", banned)
		}
	}
}

// ---------------------------------------------------------------------------
// Coordination semantics over HTTP
// ---------------------------------------------------------------------------

func TestScopeConflictIsA409WithTheHolder(t *testing.T) {
	h := newHarness(t)

	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"), map[string]any{
		"summary": "rework the router",
		"scopes":  []map[string]any{{"resource": "dir:internal/router", "mode": "write_exclusive"}},
	})
	if code != http.StatusOK {
		t.Fatalf("alice start = %d\n%s", code, body)
	}

	code, body = h.do(h.bobTok, http.MethodPost, h.projectPath("/intents/check"), map[string]any{
		"summary": "touch the router too",
		"scopes":  []map[string]any{{"resource": "path:internal/router/policy.go", "mode": "write_exclusive"}},
	})
	if code != http.StatusOK {
		t.Fatalf("check = %d\n%s", code, body)
	}

	var decision coord.IntentDecision
	if err := json.Unmarshal(body, &decision); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decision.Outcome.Blocks() {
		t.Errorf("outcome = %s, want a blocking outcome", decision.Outcome)
	}
	if len(decision.Conflicts) == 0 {
		t.Fatal("blocked without naming a conflict")
	}
	if decision.Conflicts[0].HolderOwner != "alice" {
		t.Errorf("conflict holder = %q, want alice", decision.Conflicts[0].HolderOwner)
	}
	if decision.Advice == "" {
		t.Error("a blocked check should say what to do about it")
	}
}

// A stale fence is the caller's problem, not the server's: it must be 409 so a client stops,
// not 500 which reads as "retry might work".
func TestStaleFenceIsA409(t *testing.T) {
	h := newHarness(t)

	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"),
		map[string]any{"summary": "fenced work"})
	if code != http.StatusOK {
		t.Fatalf("start = %d\n%s", code, body)
	}
	var started coord.StartWorkResult
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatalf("decode: %v", err)
	}

	code, body = h.do(h.aliceTok, http.MethodPost,
		"/v1/attempts/"+started.AttemptID+"/progress", map[string]any{
			"task_id": started.TaskID, "lease_id": started.LeaseID,
			"fencing_epoch": started.FencingEpoch + 99, // a worker that woke up late
			"phase":         "implementing",
		})
	if code != http.StatusConflict {
		t.Errorf("stale fence = %d, want 409\n%s", code, body)
	}
	if !strings.Contains(string(body), "stale_fencing") {
		t.Errorf("response should identify the failure as stale fencing:\n%s", body)
	}
}

func TestIllegalTransitionIsA409(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/tasks"),
		map[string]any{"title": "state machine", "status": "ready"})
	if code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	var view struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &view)

	code, body = h.do(h.aliceTok, http.MethodPost, "/v1/tasks/"+view.ID+"/transition",
		map[string]any{"status": "done"})
	if code != http.StatusConflict {
		t.Errorf("ready -> done = %d, want 409\n%s", code, body)
	}
}

func TestUnknownTaskIsA404NotA500(t *testing.T) {
	h := newHarness(t)
	// A non-UUID id used to reach Postgres and surface as a 500.
	for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if code, body := h.do(h.aliceTok, http.MethodGet, "/v1/tasks/"+id, nil); code != http.StatusNotFound {
			t.Errorf("GET /v1/tasks/%s = %d, want 404\n%s", id, code, body)
		}
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	h := newHarness(t)
	resp, err := h.server.Client().Get(h.server.URL + "/v1/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header")
	}
	// The dashboard's script and stylesheet are same-origin files under /static/. A policy
	// that omits 'self' from script-src/style-src forbids them, and the page dies on its
	// boot placeholder — a header that is present but wrong passes an existence check.
	for _, directive := range []string{"script-src", "style-src"} {
		value := directiveValue(csp, directive)
		if value == "" {
			t.Errorf("CSP has no %s directive: %q", directive, csp)
			continue
		}
		if !strings.Contains(value, "'self'") {
			t.Errorf("CSP %s = %q, must allow 'self' or the dashboard cannot load /static/*", directive, value)
		}
		if strings.Contains(value, "unsafe-inline") {
			t.Errorf("CSP %s = %q, the dashboard uses no inline script or style", directive, value)
		}
	}
	// HSTS over plaintext would be useless at best and lock out a local developer at worst.
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS sent over plaintext: %q", got)
	}
}

// directiveValue returns the token list of one CSP directive, or "" when absent.
func directiveValue(csp, directive string) string {
	for _, part := range strings.Split(csp, ";") {
		fields := strings.Fields(part)
		if len(fields) > 0 && fields[0] == directive {
			return strings.Join(fields[1:], " ")
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Runner endpoints
// ---------------------------------------------------------------------------

func TestRunnerSnapshotServesEverythingNeededToDispatch(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodGet, h.projectPath("/runner/snapshot"), nil)
	if code != http.StatusOK {
		t.Fatalf("snapshot = %d\n%s", code, body)
	}
	var snap coord.RunnerSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Project.ID != h.project.ID {
		t.Errorf("snapshot project = %s, want %s", snap.Project.ID, h.project.ID)
	}
	if snap.Project.Config.LeaseTTL.Std() == 0 {
		t.Error("snapshot carries no lease TTL, so a remote runner would use a zero lease")
	}
}

func TestRunnerRegistrationAcceptsASlug(t *testing.T) {
	h := newHarness(t)
	// The remote backend knows the project by whatever the operator typed, which is usually
	// a slug. Sending one used to produce a 500 from a failed UUID cast.
	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/runners/register", map[string]any{
		"name": "laptop-" + h.project.Slug, "project_id": h.project.Slug,
		"max_concurrency": 2,
	})
	if code != http.StatusOK {
		t.Fatalf("register with slug = %d\n%s", code, body)
	}
}

func TestBriefRendersInstructionAndCard(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/work/start"),
		map[string]any{"summary": "brief me", "title": "Brief me"})
	if code != http.StatusOK {
		t.Fatalf("start = %d\n%s", code, body)
	}
	var started coord.StartWorkResult
	_ = json.Unmarshal(body, &started)

	code, body = h.do(h.aliceTok, http.MethodGet, "/v1/attempts/"+started.AttemptID+"/brief", nil)
	if code != http.StatusOK {
		t.Fatalf("brief = %d\n%s", code, body)
	}
	var brief coord.AttemptBrief
	if err := json.Unmarshal(body, &brief); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(brief.Instruction, "Brief me") {
		t.Error("instruction does not mention the task")
	}
	if !strings.Contains(brief.Card, "Privacy note") {
		t.Error("card is missing the privacy note")
	}
	if brief.TaskRef == "" {
		t.Error("brief carries no task ref")
	}
}
