package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/admin"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/sso/ssotest"
)

// The admin area: organization policy, its enforcement, branding, the audit log and SCIM
// (DESIGN.md §25.8).

// orgAdmin adds an org_admin to the harness project.
func (h *harness) orgAdmin(handle string) (domain.Principal, string) {
	return h.member(handle, domain.RoleOrgAdmin)
}

// serverWith starts another server on the harness store with these options.
func (h *harness) serverWith(opts Options) *httptest.Server {
	h.t.Helper()
	srv := httptest.NewServer(New(h.store, coord.New(h.store), opts).Handler())
	h.t.Cleanup(srv.Close)
	return srv
}

func (h *harness) raw(srv *httptest.Server, token, method, path string, body []byte, contentType string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestAdminPolicyAuthorization(t *testing.T) {
	h := newHarness(t)
	root, rootTok := h.orgAdmin("root")
	patch := map[string]any{"allowed_domains": []string{"acme.com"}}

	// A contributor, and a project_admin (alice), are not organization administrators.
	for name, tok := range map[string]string{"contributor": h.bobTok, "project_admin": h.aliceTok} {
		for _, req := range []struct{ method, path string }{{http.MethodGet, "/v1/admin/policy"}, {http.MethodPatch, "/v1/admin/policy"},
			{http.MethodGet, "/v1/admin/members"}, {http.MethodGet, "/v1/admin/audit"}, {http.MethodPost, "/v1/admin/scim/tokens"}} {
			if code, body := h.do(tok, req.method, req.path, patch); code != http.StatusForbidden {
				t.Errorf("%s %s %s = %d, want 403\n%s", name, req.method, req.path, code, body)
			}
		}
	}
	// A project-scoped token of the administrator is refused too: it names no project.
	scoped, err := h.store.CreateScopedToken(context.Background(), root.ID, h.project.ID, "attempt", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := h.do(scoped, http.MethodPatch, "/v1/admin/policy", patch); code != http.StatusForbidden {
		t.Errorf("scoped token PATCH policy = %d, want 403", code)
	}

	code, body := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", patch)
	if code != http.StatusOK {
		t.Fatalf("org admin PATCH = %d\n%s", code, body)
	}
	var out struct {
		Policy admin.Policy `json:"policy"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Policy.AllowedDomains) != 1 || out.Policy.AllowedDomains[0] != "acme.com" {
		t.Errorf("policy after PATCH = %+v", out.Policy)
	}
	if !strings.Contains(strings.Join(h.auditActions(string(h.org.ID)), ","), "org.policy_updated") {
		t.Error("a policy change was not audited")
	}
	// Unknown settings and invalid values are refused, not ignored.
	if code, body := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"allowed_domain": []string{"x.com"}}); code != http.StatusBadRequest {
		t.Errorf("misspelled setting = %d\n%s", code, body)
	}
	if code, body := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"default_role": "org_admin"}); code != http.StatusBadRequest {
		t.Errorf("org_admin as the provisioning role = %d\n%s", code, body)
	}
	// Single sign-on cannot be required on a server that has no provider.
	if code, body := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"require_sso": true}); code != http.StatusBadRequest ||
		!strings.Contains(string(body), "no single sign-on provider") {
		t.Errorf("require_sso without providers = %d\n%s", code, body)
	}
	// Projects a policy names must exist.
	if code, _ := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"default_project": "no-such-project"}); code != http.StatusBadRequest {
		t.Errorf("unknown default project = %d", code)
	}
}

func TestAdminLockedSettingsAreReadOnly(t *testing.T) {
	h := newHarness(t)
	_, rootTok := h.orgAdmin("root")
	yes := true
	logo, err := admin.CheckLogo(pngBytes(t, 8), "")
	if err != nil {
		t.Fatal(err)
	}
	srv := h.serverWith(Options{Admin: AdminOptions{Locks: admin.Locks{
		Patch: admin.Patch{Features: map[string]bool{"queue": true}, AllowedDomains: &[]string{"locked.example"},
			Branding: &admin.BrandingPatch{DisplayName: ptr("Locked Co")}},
		Logo: &logo}, ConfigFile: "/etc/conductor.yaml"}})
	_ = yes

	code, out := h.doJSONOn(srv, rootTok, http.MethodGet, "/v1/admin/policy", nil)
	if code != http.StatusOK {
		t.Fatalf("GET policy = %d %v", code, out)
	}
	locked, _ := json.Marshal(out["locked"])
	for _, k := range []string{"features.queue", "allowed_domains", "branding.display_name", "branding.logo"} {
		if !strings.Contains(string(locked), k) {
			t.Errorf("%s is not reported locked: %s", k, locked)
		}
	}
	for name, body := range map[string]map[string]any{
		"feature": {"features": map[string]bool{"queue": false}},
		"domains": {"allowed_domains": []string{"other.example"}},
		"name":    {"branding": map[string]any{"display_name": "Mine"}},
	} {
		code, out := h.doJSONOn(srv, rootTok, http.MethodPatch, "/v1/admin/policy", body)
		if code != http.StatusForbidden || !strings.Contains(out["error"].(string), "config file") {
			t.Errorf("changing locked %s = %d %v", name, code, out)
		}
	}
	// An unlocked setting still changes, and the effective policy keeps the locked values.
	code, out = h.doJSONOn(srv, rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"features": map[string]bool{"swarm": true}})
	if code != http.StatusOK {
		t.Fatalf("unlocked change = %d %v", code, out)
	}
	code, org := h.doJSONOn(srv, h.bobTok, http.MethodGet, "/v1/org", nil)
	features, _ := org["features"].(map[string]any)
	branding, _ := org["branding"].(map[string]any)
	if code != http.StatusOK || features["queue"] != true || features["swarm"] != true || branding["display_name"] != "Locked Co" || branding["has_logo"] != true {
		t.Errorf("GET /v1/org = %d %v", code, org)
	}
	// The locked logo cannot be replaced or removed.
	if code, _ := h.doJSONOn(srv, rootTok, http.MethodDelete, "/v1/admin/branding/logo", nil); code != http.StatusForbidden {
		t.Errorf("delete locked logo = %d", code)
	}
}

func pngBytes(t *testing.T, side int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, side, side))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBrandingValidationAndLogoServing(t *testing.T) {
	h := newHarness(t)
	_, rootTok := h.orgAdmin("root")
	if code, body := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"branding": map[string]any{"accent_color": "#ffff00"}}); code != http.StatusBadRequest ||
		!strings.Contains(string(body), "contrast") {
		t.Errorf("unreadable accent = %d\n%s", code, body)
	}
	if code, body := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"branding": map[string]any{"accent_color": "javascript:alert(1)"}}); code != http.StatusBadRequest {
		t.Errorf("non-color accent = %d\n%s", code, body)
	}
	banner := `<script>alert("x")</script> Authorized use only.`
	if code, body := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"branding": map[string]any{
		"display_name": "Acme", "accent_color": "#1d4ed8", "login_banner": banner}}); code != http.StatusOK {
		t.Fatalf("valid branding = %d\n%s", code, body)
	}
	put := func(data []byte, ct string) (int, []byte) {
		return h.do(rootTok, http.MethodPut, "/v1/admin/branding/logo", map[string]any{"content_type": ct, "data": base64.StdEncoding.EncodeToString(data)})
	}
	for name, c := range map[string]struct {
		data []byte
		ct   string
	}{
		"svg":       {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "image/svg+xml"},
		"html":      {[]byte(`<html><body>hi</body></html>`), "image/png"},
		"oversized": {append(pngBytes(t, 4), make([]byte, admin.MaxLogoBytes)...), "image/png"},
	} {
		if code, body := put(c.data, c.ct); code != http.StatusBadRequest {
			t.Errorf("%s logo = %d\n%s", name, code, body)
		}
	}
	if code, body := h.do(h.bobTok, http.MethodPut, "/v1/admin/branding/logo", map[string]any{"data": base64.StdEncoding.EncodeToString(pngBytes(t, 4))}); code != http.StatusForbidden {
		t.Errorf("contributor sets the logo = %d\n%s", code, body)
	}
	if code, body := put(pngBytes(t, 16), "image/png"); code != http.StatusOK {
		t.Fatalf("PNG logo = %d\n%s", code, body)
	}
	resp := h.raw(h.server, h.bobTok, http.MethodGet, "/v1/org/logo", nil, "")
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") ||
		!strings.HasPrefix(body, "\x89PNG") {
		t.Errorf("logo = %d %v", resp.StatusCode, resp.Header)
	}
	if resp := h.raw(h.server, "", http.MethodGet, "/v1/org/logo", nil, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("org logo without a token = %d", resp.StatusCode)
	}
	_, org := h.doJSONOn(h.server, h.bobTok, http.MethodGet, "/v1/org", nil)
	b := org["branding"].(map[string]any)
	if b["login_banner"] != banner || b["on_accent"] != "#ffffff" || b["has_logo"] != true {
		t.Errorf("branding = %v", b)
	}
}

func TestFeatureFlags(t *testing.T) {
	h := newHarness(t)
	_, rootTok := h.orgAdmin("root")
	_, org := h.doJSONOn(h.server, h.bobTok, http.MethodGet, "/v1/org", nil)
	for k, v := range org["features"].(map[string]any) {
		if v != false {
			t.Errorf("a new organization has %s on", k)
		}
	}
	if org["is_org_admin"] != false {
		t.Error("bob is reported an org admin")
	}
	if code, _ := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"features": map[string]bool{"swarm": true, "queue": true}}); code != http.StatusOK {
		t.Fatal(code)
	}
	_, org = h.doJSONOn(h.server, rootTok, http.MethodGet, "/v1/org", nil)
	f := org["features"].(map[string]any)
	if f["swarm"] != true || f["queue"] != true || f["mesh"] != false || org["is_org_admin"] != true {
		t.Errorf("features = %v admin %v", f, org["is_org_admin"])
	}
	// A flag is presentation, not authorization: the queue route still answers with it off.
	if code, _ := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"features": map[string]bool{"queue": false}}); code != http.StatusOK {
		t.Fatal(code)
	}
	if code, body := h.do(h.bobTok, http.MethodGet, h.projectPath("/queue"), nil); code != http.StatusOK {
		t.Errorf("queue with the flag off = %d\n%s", code, body)
	}
}

// ---------------------------------------------------------------------------
// Require SSO, token lifetimes
// ---------------------------------------------------------------------------

func TestRequireSSO(t *testing.T) {
	sh := newSSOHarness(t, nil)
	ctx := context.Background()
	root, plainRootTok := sh.orgAdmin("root")
	sh.register(sh.bob, sh.email("bob"))
	// Turning require-SSO on takes a single sign-on session, so the administrator who does it
	// has proved they can still get in.
	if code, out := sh.doJSONOn(sh.srv, plainRootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"require_sso": true}); code != http.StatusBadRequest ||
		!strings.Contains(out["error"].(string), "locked out") {
		t.Errorf("require_sso from a non-SSO session = %d %v", code, out)
	}
	sh.register(root, sh.email("root"))
	sh.idp.SetUser(ssotest.User{Subject: "sub-root-" + sh.tag, Email: sh.email("root"), EmailVerified: true})
	rootTok := sh.signInToken("test")
	sh.idp.SetUser(ssotest.User{Subject: "sub-" + sh.tag, Email: sh.email("bob"), EmailVerified: true})
	runner, err := sh.store.CreatePrincipal(ctx, sh.org.ID, domain.PrincipalRunner, "runner-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.store.AddMember(ctx, sh.project.ID, runner.ID, domain.RoleRunner); err != nil {
		t.Fatal(err)
	}
	runnerTok, _ := sh.store.CreateToken(ctx, runner.ID, "runner", 0)

	if code, out := sh.doJSONOn(sh.srv, rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"require_sso": true}); code != http.StatusOK {
		t.Fatalf("require_sso = %d %v", code, out)
	}
	// Bob's ordinary token is refused, with a reason a client can act on.
	code, out := sh.doJSONOn(sh.srv, sh.bobTok, http.MethodGet, "/v1/whoami", nil)
	if code != http.StatusUnauthorized || out["code"] != "sso_required" {
		t.Errorf("non-SSO human token = %d %v", code, out)
	}
	// A service principal is not a person and keeps working.
	if code, _ := sh.whoami(runnerTok); code != http.StatusOK {
		t.Errorf("runner token under require_sso = %d", code)
	}
	// Signing in through the provider works, and that token is accepted.
	ssoTok := sh.signInToken("test")
	if code, who := sh.whoami(ssoTok); code != http.StatusOK || who != "bob" {
		t.Fatalf("SSO token = %d %s", code, who)
	}
	// A token minted with it belongs to the same sign-in: named under it, no longer-lived.
	code, minted := sh.doJSONOn(sh.srv, ssoTok, http.MethodPost, "/v1/tokens", map[string]any{"name": "laptop", "ttl": "2160h"})
	if code != http.StatusCreated || minted["name"] != "sso:test/laptop" {
		t.Fatalf("mint from SSO = %d %v", code, minted)
	}
	exp, _ := time.Parse(time.RFC3339, minted["expires_at"].(string))
	if time.Until(exp) > 13*time.Hour {
		t.Errorf("a derived token outlives its sign-in: expires %s", exp)
	}
	if code, who := sh.whoami(minted["token"].(string)); code != http.StatusOK || who != "bob" {
		t.Errorf("derived token = %d %s", code, who)
	}
	// The sso: prefix is reserved: an ordinary login cannot dress a token up as one.
	if code, out := sh.doJSONOn(sh.srv, rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"require_sso": false}); code != http.StatusOK {
		t.Fatalf("turn off = %d %v", code, out)
	}
	if code, out := sh.doJSONOn(sh.srv, sh.bobTok, http.MethodPost, "/v1/tokens", map[string]any{"name": "sso:test"}); code != http.StatusBadRequest {
		t.Errorf("a self-named sso: token = %d %v", code, out)
	}
	// Turning the policy off restores the tokens it refused (they were never revoked).
	if code, _ := sh.whoami(sh.bobTok); code != http.StatusOK {
		t.Errorf("bob's token after require_sso is off = %d", code)
	}
	// Unlinking the identity ends the sign-in and everything minted from it.
	if code, out := sh.doJSONOn(sh.srv, sh.aliceTok, http.MethodDelete, sh.projectPath("/members/bob/identities/test"), nil); code != http.StatusOK {
		t.Fatalf("unlink = %d %v", code, out)
	}
	if code, _ := sh.whoami(minted["token"].(string)); code != http.StatusUnauthorized {
		t.Errorf("a derived token survived unlinking = %d", code)
	}

	// An invite still adds a person, but mints them no token while SSO is required.
	if code, _ := sh.doJSONOn(sh.srv, rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"require_sso": true}); code != http.StatusOK {
		t.Fatal(code)
	}
	code, inv := sh.doJSONOn(sh.srv, sh.aliceTok, http.MethodPost, sh.projectPath("/members"), map[string]any{"handle": "newperson"})
	if code != http.StatusUnauthorized {
		// alice's own token is not from SSO, so she cannot invite at all now.
		t.Errorf("alice's non-SSO token while SSO is required = %d %v", code, inv)
	}
	code, inv = sh.doJSONOn(sh.srv, plainRootTok, http.MethodPost, sh.projectPath("/members"), map[string]any{"handle": "newperson"})
	if code != http.StatusUnauthorized {
		t.Errorf("root's non-SSO token while SSO is required = %d %v", code, inv)
	}
}

func TestInviteUnderRequireSSOMintsNoToken(t *testing.T) {
	sh := newSSOHarness(t, nil)
	root, _ := sh.orgAdmin("root")
	// The administrator works through a single sign-on token, as the policy requires.
	sh.register(root, sh.email("root"))
	sh.idp.SetUser(ssotest.User{Subject: "sub-root-" + sh.tag, Email: sh.email("root"), EmailVerified: true})
	ssoTok := sh.signInToken("test")
	if code, _ := sh.doJSONOn(sh.srv, ssoTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"require_sso": true}); code != http.StatusOK {
		t.Fatal(code)
	}
	code, inv := sh.doJSONOn(sh.srv, ssoTok, http.MethodPost, sh.projectPath("/members"), map[string]any{"handle": "newperson"})
	if code != http.StatusCreated || inv["token"] != nil || !strings.Contains(inv["note"].(string), "single sign-on") {
		t.Errorf("invite under require_sso = %d %v", code, inv)
	}
	code, inv = sh.doJSONOn(sh.srv, ssoTok, http.MethodPost, sh.projectPath("/members"),
		map[string]any{"handle": "ci-runner", "kind": "runner_service", "role": "runner"})
	if code != http.StatusCreated || inv["token"] == nil {
		t.Errorf("a runner invite still mints its token = %d %v", code, inv)
	}
}

func TestTokenLifetimePolicy(t *testing.T) {
	h := newHarness(t)
	_, rootTok := h.orgAdmin("root")
	if code, body := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{
		"human_token_max_ttl": "2d", "service_token_max_ttl": "7d"}); code != http.StatusOK {
		t.Fatalf("PATCH = %d\n%s", code, body)
	}
	expiry := func(body []byte) time.Duration {
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		exp, err := time.Parse(time.RFC3339, out["expires_at"].(string))
		if err != nil {
			t.Fatalf("expires_at: %v (%s)", err, body)
		}
		return time.Until(exp)
	}
	code, body := h.do(h.bobTok, http.MethodPost, "/v1/tokens", map[string]any{"name": "long", "ttl": "2000h"})
	if code != http.StatusCreated || expiry(body) > 49*time.Hour {
		t.Errorf("human token = %d, lives %s\n%s", code, expiry(body), body)
	}
	code, body = h.do(h.aliceTok, http.MethodPost, h.projectPath("/members"), map[string]any{"handle": "bot", "kind": "runner_service", "role": "runner"})
	if code != http.StatusCreated || expiry(body) > 7*24*time.Hour+time.Minute {
		t.Errorf("service token without expiry = %d\n%s", code, body)
	}
}

// ---------------------------------------------------------------------------
// Sign-in: admission, provisioning, group mapping, deactivation
// ---------------------------------------------------------------------------

func TestOrgAdmissionRules(t *testing.T) {
	sh := newSSOHarness(t, nil)
	_, rootTok := sh.orgAdmin("root")
	sh.register(sh.bob, sh.email("bob"))
	if code, _ := sh.doJSONOn(sh.srv, rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"allowed_domains": []string{"elsewhere.example"}}); code != http.StatusOK {
		t.Fatal(code)
	}
	wantSSOError(t, sh.signIn(browser(), "test"), "not in a domain your organization allows")
	if code, _ := sh.doJSONOn(sh.srv, rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{
		"allowed_domains": []string{"example.com"}, "allowed_github_orgs": []string{"other-org"}}); code != http.StatusOK {
		t.Fatal(code)
	}
	// The OIDC sign-in passes now; GitHub's must also come from an allowed organization.
	_ = sh.signInToken("test")
	wantSSOError(t, sh.signIn(browser(), "github"), "other-org")
}

func TestPolicyAutoProvision(t *testing.T) {
	sh := newSSOHarness(t, nil)
	_, rootTok := sh.orgAdmin("root")
	// A domain unique to this test, so no other test's organization claims it.
	domainName := "prov-" + sh.tag + ".example"
	sh.idp.SetUser(ssotest.User{Subject: "sub-new-" + sh.tag, Email: "newcomer@" + domainName, EmailVerified: true, Name: "New Comer"})
	wantSSOError(t, sh.signIn(browser(), "test"), "No Conductor account")
	if code, out := sh.doJSONOn(sh.srv, rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{
		"auto_provision": true, "allowed_domains": []string{domainName}, "default_project": sh.project.Slug, "default_role": "reviewer"}); code != http.StatusOK {
		t.Fatalf("PATCH = %d %v", code, out)
	}
	tok := sh.signInToken("test")
	code, who := sh.whoami(tok)
	if code != http.StatusOK || who != "newcomer" {
		t.Fatalf("provisioned = %d %s", code, who)
	}
	p, err := sh.store.GetPrincipalByHandle(context.Background(), sh.org.ID, "newcomer")
	if err != nil {
		t.Fatal(err)
	}
	if role, _ := sh.store.RoleIn(context.Background(), sh.project.ID, p.ID); role != domain.RoleReviewer {
		t.Errorf("provisioned role = %s", role)
	}
}

func TestGroupMappingAtSignIn(t *testing.T) {
	sh := newSSOHarness(t, nil)
	ctx := context.Background()
	root, rootTok := sh.orgAdmin("root")
	sh.register(sh.bob, sh.email("bob"))
	set := func(body map[string]any) {
		t.Helper()
		if code, out := sh.doJSONOn(sh.srv, rootTok, http.MethodPatch, "/v1/admin/policy", body); code != http.StatusOK {
			t.Fatalf("PATCH %v = %d %v", body, code, out)
		}
	}
	role := func(p domain.Principal) domain.Role {
		r, _ := sh.store.RoleIn(ctx, sh.project.ID, p.ID)
		return r
	}
	slug := sh.project.Slug
	// A rule above the ceiling is refused outright, and org_admin is never mappable.
	if code, _ := sh.doJSONOn(sh.srv, rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{
		"group_rules": []map[string]any{{"group": "admins", "project": slug, "role": "project_admin"}}}); code != http.StatusBadRequest {
		t.Errorf("rule above max_group_role = %d", code)
	}
	if code, _ := sh.doJSONOn(sh.srv, rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{
		"max_group_role": "org_admin"}); code != http.StatusBadRequest {
		t.Errorf("org_admin ceiling = %d", code)
	}
	set(map[string]any{"group_rules": []map[string]any{
		{"group": "eng-leads", "project": slug, "role": "maintainer"},
		{"group": "eng", "project": slug, "role": "contributor"},
		{"group": "readers", "project": slug, "role": "observer"}}})

	// Promotion from the token's groups claim.
	sh.idp.SetUser(ssotest.User{Subject: "sub-" + sh.tag, Email: sh.email("bob"), EmailVerified: true, Groups: []string{"ENG-leads"}})
	sh.signInToken("test")
	if role(sh.bob) != domain.RoleMaintainer {
		t.Errorf("bob after eng-leads = %s", role(sh.bob))
	}
	// Demotion within the ceiling follows the provider.
	sh.idp.SetUser(ssotest.User{Subject: "sub-" + sh.tag, Email: sh.email("bob"), EmailVerified: true, Groups: []string{"readers"}})
	sh.signInToken("test")
	if role(sh.bob) != domain.RoleObserver {
		t.Errorf("bob after readers = %s", role(sh.bob))
	}
	// No matching group removes nothing.
	sh.idp.SetUser(ssotest.User{Subject: "sub-" + sh.tag, Email: sh.email("bob"), EmailVerified: true, Groups: []string{"strangers"}})
	sh.signInToken("test")
	if role(sh.bob) != domain.RoleObserver {
		t.Errorf("bob after an unmapped group = %s", role(sh.bob))
	}
	// A role above the ceiling, granted by a person, is never touched: root stays org_admin.
	sh.register(root, sh.email("root"))
	sh.idp.SetUser(ssotest.User{Subject: "sub-root-" + sh.tag, Email: sh.email("root"), EmailVerified: true, Groups: []string{"readers"}})
	sh.signInToken("test")
	if role(root) != domain.RoleOrgAdmin {
		t.Errorf("root after readers = %s", role(root))
	}
	// With a project_admin ceiling, mapping may demote an administrator — but never the last.
	set(map[string]any{"max_group_role": "project_admin"})
	carol, _ := sh.member("carol", "")
	solo := sh.secondProject("solo")
	if err := sh.store.AddMember(ctx, solo.ID, carol.ID, domain.RoleProjectAdmin); err != nil {
		t.Fatal(err)
	}
	set(map[string]any{"group_rules": []map[string]any{{"group": "readers", "project": solo.Slug, "role": "observer"}}})
	sh.register(carol, sh.email("carol"))
	sh.idp.SetUser(ssotest.User{Subject: "sub-carol-" + sh.tag, Email: sh.email("carol"), EmailVerified: true, Groups: []string{"readers"}})
	sh.signInToken("test")
	if r, _ := sh.store.RoleIn(ctx, solo.ID, carol.ID); r != domain.RoleProjectAdmin {
		t.Errorf("the last administrator was demoted by a group: %s", r)
	}
	if !strings.Contains(strings.Join(sh.auditActions(string(sh.bob.ID)), ","), "member.role_mapped") {
		t.Error("a mapped role change was not audited")
	}
}

func TestGroupMappingGrantsFirstAccessAndSCIMGroupsCount(t *testing.T) {
	sh := newSSOHarness(t, nil)
	ctx := context.Background()
	_, rootTok := sh.orgAdmin("root")
	sh.register(sh.outsider, sh.email("outsider"))
	sh.idp.SetUser(ssotest.User{Subject: "sub-out-" + sh.tag, Email: sh.email("outsider"), EmailVerified: true})
	wantSSOError(t, sh.signIn(browser(), "test"), "not a member of any project")
	if code, _ := sh.doJSONOn(sh.srv, rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{
		"group_rules": []map[string]any{{"group": "Platform", "project": sh.project.Slug, "role": "contributor"}}}); code != http.StatusOK {
		t.Fatal(code)
	}
	// The group arrives through SCIM, not the token.
	_, scimTok := mintSCIM(t, sh.harness, sh.srv, rootTok)
	code, _ := scimDo(t, sh.srv, scimTok, http.MethodPost, "/scim/v2/Groups",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"platform","members":[{"value":"`+string(sh.outsider.ID)+`"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("SCIM group = %d", code)
	}
	tok := sh.signInToken("test")
	if code, who := sh.whoami(tok); code != http.StatusOK || who != "outsider" {
		t.Fatalf("first access through a group = %d %s", code, who)
	}
	if r, _ := sh.store.RoleIn(ctx, sh.project.ID, sh.outsider.ID); r != domain.RoleContributor {
		t.Errorf("outsider role = %s", r)
	}
}

func TestDeactivationEndsAccess(t *testing.T) {
	sh := newSSOHarness(t, nil)
	root, rootTok := sh.orgAdmin("root")
	sh.register(sh.bob, sh.email("bob"))
	ssoTok := sh.signInToken("test")
	if code, out := sh.doJSONOn(sh.srv, sh.aliceTok, http.MethodPost, "/v1/admin/members/bob/deactivate", nil); code != http.StatusForbidden {
		t.Errorf("project_admin deactivating = %d %v", code, out)
	}
	if code, out := sh.doJSONOn(sh.srv, rootTok, http.MethodPost, "/v1/admin/members/root/deactivate", nil); code != http.StatusForbidden {
		t.Errorf("self-deactivation = %d %v", code, out)
	}
	code, out := sh.doJSONOn(sh.srv, rootTok, http.MethodPost, "/v1/admin/members/bob/deactivate", nil)
	if code != http.StatusOK || out["revoked_tokens"].(float64) < 2 {
		t.Fatalf("deactivate = %d %v", code, out)
	}
	for _, tok := range []string{sh.bobTok, ssoTok} {
		if code, _ := sh.whoami(tok); code != http.StatusUnauthorized {
			t.Errorf("a deactivated principal's token = %d", code)
		}
	}
	wantSSOError(t, sh.signIn(browser(), "test"), "deactivated")
	// Even a token minted after deactivation (bootstrap has the database) is refused.
	late, _ := sh.store.CreateToken(context.Background(), sh.bob.ID, "late", time.Hour)
	if code, _ := sh.whoami(late); code != http.StatusUnauthorized {
		t.Errorf("a token minted for a deactivated principal = %d", code)
	}
	_, members := sh.doJSONOn(sh.srv, rootTok, http.MethodGet, "/v1/admin/members", nil)
	listed, _ := json.Marshal(members)
	if !strings.Contains(string(listed), `"deactivated_at"`) || !strings.Contains(string(listed), `"handle":"bob"`) {
		t.Errorf("members = %s", listed)
	}
	if code, _ := sh.doJSONOn(sh.srv, rootTok, http.MethodPost, "/v1/admin/members/bob/reactivate", nil); code != http.StatusOK {
		t.Fatal(code)
	}
	if code, _ := sh.whoami(sh.signInToken("test")); code != http.StatusOK {
		t.Error("a reactivated principal cannot sign in again")
	}
	_ = root
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

func TestAdminAuditViewerAndExport(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, rootTok := h.orgAdmin("root")
	// A principal whose handle a spreadsheet would run as a formula.
	evil, err := h.store.CreatePrincipal(ctx, h.org.ID, domain.PrincipalHuman, "=HYPERLINK(1)", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = h.store.AddMember(ctx, h.project.ID, evil.ID, domain.RoleContributor)
	evilTok, _ := h.store.CreateToken(ctx, evil.ID, "x", time.Hour)
	if code, _ := h.do(evilTok, http.MethodPost, "/v1/tokens", map[string]any{"name": "laptop"}); code != http.StatusCreated {
		t.Fatal(code)
	}
	if code, _ := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"features": map[string]bool{"swarm": true}}); code != http.StatusOK {
		t.Fatal(code)
	}

	code, body := h.do(rootTok, http.MethodGet, "/v1/admin/audit?action=org.", nil)
	var page struct {
		Entries []db.AuditEntry `json:"entries"`
		Actions []string        `json:"actions"`
	}
	_ = json.Unmarshal(body, &page)
	if code != http.StatusOK || len(page.Entries) != 1 || page.Entries[0].Action != "org.policy_updated" || page.Entries[0].Actor != "root" {
		t.Fatalf("filtered by action = %d\n%s", code, body)
	}
	if len(page.Actions) < 2 {
		t.Errorf("actions = %v", page.Actions)
	}
	if res, _ := json.Marshal(page.Entries[0].Detail["resources"]); string(res) != `["features.swarm"]` {
		t.Errorf("the audit entry should name the setting changed: %s", res)
	}
	_, body = h.do(rootTok, http.MethodGet, "/v1/admin/audit?actor="+"%3DHYPERLINK(1)", nil)
	_ = json.Unmarshal(body, &page)
	if len(page.Entries) != 1 || page.Entries[0].Action != "token.created" {
		t.Errorf("filtered by actor = %s", body)
	}
	_, body = h.do(rootTok, http.MethodGet, "/v1/admin/audit?actor=nobody-at-all", nil)
	if _ = json.Unmarshal(body, &page); len(page.Entries) != 0 {
		t.Errorf("an unknown actor matched %d entries", len(page.Entries))
	}
	if code, _ := h.do(rootTok, http.MethodGet, "/v1/admin/audit?since=yesterday", nil); code != http.StatusBadRequest {
		t.Errorf("bad since = %d", code)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	_, body = h.do(rootTok, http.MethodGet, "/v1/admin/audit?since="+future, nil)
	if _ = json.Unmarshal(body, &page); len(page.Entries) != 0 {
		t.Errorf("since did not filter: %s", body)
	}

	resp := h.raw(h.server, rootTok, http.MethodGet, "/v1/admin/audit?format=csv", nil, "")
	csvBody := readAll(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("csv = %d %v", resp.StatusCode, resp.Header)
	}
	rows, err := csv.NewReader(strings.NewReader(csvBody)).ReadAll()
	if err != nil || len(rows) < 3 || rows[0][0] != "id" || rows[0][4] != "action" {
		t.Fatalf("csv rows = %v, %v", rows, err)
	}
	for _, row := range rows[1:] {
		for _, cell := range row {
			if strings.HasPrefix(cell, "=") {
				t.Errorf("a formula reached the CSV: %q", cell)
			}
		}
	}
	if !strings.Contains(csvBody, "'=HYPERLINK(1)") {
		t.Errorf("the formula handle is not defused:\n%s", csvBody)
	}
	resp = h.raw(h.server, rootTok, http.MethodGet, "/v1/admin/audit?format=jsonl&action=token.created", nil, "")
	lines := strings.Split(strings.TrimSpace(readAll(t, resp)), "\n")
	for _, l := range lines {
		var e db.AuditEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil || e.Action != "token.created" {
			t.Errorf("jsonl line %q: %v", l, err)
		}
	}
	if !strings.Contains(strings.Join(h.auditActions(string(h.org.ID)), ","), "audit.exported") {
		t.Error("an export was not audited")
	}
	if code, _ := h.do(h.bobTok, http.MethodGet, "/v1/admin/audit?format=csv", nil); code != http.StatusForbidden {
		t.Errorf("contributor export = %d", code)
	}
	// The hook sees records as they are written.
	var seen []string
	h.store.SetAuditHook(func(e db.AuditEntry) { seen = append(seen, e.Action) })
	defer h.store.SetAuditHook(nil)
	h.do(h.bobTok, http.MethodPost, "/v1/tokens", map[string]any{"name": "hooked"})
	if len(seen) == 0 || seen[len(seen)-1] != "token.created" {
		t.Errorf("audit hook saw %v", seen)
	}
}

func TestAdminConfigView(t *testing.T) {
	h := newHarness(t)
	root, rootTok := h.orgAdmin("root")
	srv := h.serverWith(Options{Admin: AdminOptions{ConfigFile: "/etc/conductor.yaml", Config: []ConfigEntry{
		{Key: "addr", Value: "0.0.0.0:8443", Source: "file"}, {Key: "dsn", Value: "postgres://u:xxxxx@db/c", Source: "env", Secret: true}}}})
	if _, err := h.store.SetLocalOwner(context.Background(), root.ID, false); err != nil {
		t.Fatal(err)
	}
	code, out := h.doJSONOn(srv, rootTok, http.MethodGet, "/v1/admin/config", nil)
	if code != http.StatusOK || out["config_file"] != "/etc/conductor.yaml" || len(out["settings"].([]any)) != 2 {
		t.Fatalf("config = %d %v", code, out)
	}
	if code, _ := h.doJSONOn(srv, h.aliceTok, http.MethodGet, "/v1/admin/config", nil); code != http.StatusForbidden {
		t.Errorf("project_admin reads config = %d", code)
	}
	// Another organization's administrator does not see this server's configuration.
	other, _ := h.store.CreateOrganization(context.Background(), uniq("other-org", time.Now().UnixNano()), "Other")
	op, _ := h.store.CreateProject(context.Background(), db.CreateProjectParams{OrganizationID: other.ID, Slug: "p", Config: domain.DefaultProjectConfig()})
	stranger, _ := h.store.CreatePrincipal(context.Background(), other.ID, domain.PrincipalHuman, "stranger", "", "")
	_ = h.store.AddMember(context.Background(), op.ID, stranger.ID, domain.RoleOrgAdmin)
	strangerTok, _ := h.store.CreateToken(context.Background(), stranger.ID, "t", time.Hour)
	if code, _ := h.doJSONOn(srv, strangerTok, http.MethodGet, "/v1/admin/config", nil); code != http.StatusForbidden {
		t.Errorf("another tenant's admin reads config = %d", code)
	}
}

// ---------------------------------------------------------------------------
// SCIM
// ---------------------------------------------------------------------------

func mintSCIM(t *testing.T, h *harness, srv *httptest.Server, adminTok string) (string, string) {
	t.Helper()
	code, out := h.doJSONOn(srv, adminTok, http.MethodPost, "/v1/admin/scim/tokens", map[string]any{"name": "okta"})
	if code != http.StatusCreated || !strings.HasPrefix(out["token"].(string), db.SCIMTokenPrefix) {
		t.Fatalf("mint SCIM token = %d %v", code, out)
	}
	return out["id"].(string), out["token"].(string)
}

func scimDo(t *testing.T, srv *httptest.Server, tok, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/scim+json")
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
	if resp.StatusCode != http.StatusNoContent && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/scim+json") {
		t.Errorf("%s %s answered %s", method, path, resp.Header.Get("Content-Type"))
	}
	return resp.StatusCode, out
}

func TestSCIMTokenAuthentication(t *testing.T) {
	h := newHarness(t)
	_, rootTok := h.orgAdmin("root")
	id, tok := mintSCIM(t, h, h.server, rootTok)
	for name, bad := range map[string]string{"none": "", "an ordinary token": rootTok, "garbage": db.SCIMTokenPrefix + "nope"} {
		if code, out := scimDo(t, h.server, bad, http.MethodGet, "/scim/v2/Users", ""); code != http.StatusUnauthorized || out["schemas"] == nil {
			t.Errorf("%s = %d %v", name, code, out)
		}
	}
	if code, _ := scimDo(t, h.server, tok, http.MethodGet, "/scim/v2/ServiceProviderConfig", ""); code != http.StatusOK {
		t.Errorf("SPConfig = %d", code)
	}
	// A SCIM token is not a bearer token for the API.
	if code, _ := h.do(tok, http.MethodGet, "/v1/whoami", nil); code != http.StatusUnauthorized {
		t.Errorf("SCIM token on the API = %d", code)
	}
	// The token list never shows the secret; revoking stops it.
	_, list := h.doJSONOn(h.server, rootTok, http.MethodGet, "/v1/admin/scim/tokens", nil)
	if raw, _ := json.Marshal(list); strings.Contains(string(raw), tok) {
		t.Error("the SCIM token list shows the secret")
	}
	if code, _ := h.doJSONOn(h.server, rootTok, http.MethodDelete, "/v1/admin/scim/tokens/"+id, nil); code != http.StatusNoContent {
		t.Fatalf("revoke = %d", code)
	}
	if code, _ := scimDo(t, h.server, tok, http.MethodGet, "/scim/v2/Users", ""); code != http.StatusUnauthorized {
		t.Errorf("revoked SCIM token = %d", code)
	}
	// Another organization's SCIM token sees none of this organization's users.
	other := newHarness(t)
	_, otherRoot := other.orgAdmin("root")
	_, otherTok := mintSCIM(t, other, other.server, otherRoot)
	h.member("only-here", domain.RoleContributor)
	code, out := scimDo(t, h.server, otherTok, http.MethodGet, "/scim/v2/Users?filter="+urlq(`userName eq "only-here"`), "")
	if code != http.StatusOK || out["totalResults"].(float64) != 0 {
		t.Errorf("cross-organization lookup = %d %v", code, out)
	}
	if code, _ := scimDo(t, h.server, otherTok, http.MethodGet, "/scim/v2/Users/"+string(h.alice.ID), ""); code != http.StatusNotFound {
		t.Errorf("cross-organization GET = %d", code)
	}
}

func urlq(s string) string { return strings.NewReplacer(" ", "%20", `"`, "%22").Replace(s) }

func TestSCIMUserLifecycleOktaShapes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, rootTok := h.orgAdmin("root")
	if code, body := h.do(rootTok, http.MethodPatch, "/v1/admin/policy", map[string]any{"default_project": h.project.Slug, "default_role": "contributor"}); code != http.StatusOK {
		t.Fatalf("policy = %d %s", code, body)
	}
	_, tok := mintSCIM(t, h, h.server, rootTok)

	// Okta looks the user up first...
	code, out := scimDo(t, h.server, tok, http.MethodGet, "/scim/v2/Users?filter="+urlq(`userName eq "ana@acme.example"`)+"&startIndex=1&count=100", "")
	if code != http.StatusOK || out["totalResults"].(float64) != 0 || out["schemas"].([]any)[0] != admin.SchemaList {
		t.Fatalf("lookup = %d %v", code, out)
	}
	// ...then creates it. Roles in the body are ignored: SCIM grants none.
	create := `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"ana@acme.example",
		"name":{"givenName":"Ana","familyName":"Diaz"},"emails":[{"primary":true,"value":"ana@acme.example","type":"work"}],
		"displayName":"Ana Diaz","externalId":"00u1okta","active":true,"roles":[{"value":"org_admin","primary":true}]}`
	code, user := scimDo(t, h.server, tok, http.MethodPost, "/scim/v2/Users", create)
	if code != http.StatusCreated || user["userName"] != "ana@acme.example" || user["active"] != true || user["externalId"] != "00u1okta" {
		t.Fatalf("create = %d %v", code, user)
	}
	id := domain.ID(user["id"].(string))
	p, err := h.store.GetPrincipal(ctx, id)
	if err != nil || p.Handle != "ana" || p.Email != "ana@acme.example" || p.DisplayName != "Ana Diaz" {
		t.Fatalf("principal = %+v, %v", p, err)
	}
	if role, _ := h.store.RoleIn(ctx, h.project.ID, id); role != domain.RoleContributor {
		t.Errorf("SCIM user role = %s, want the policy's contributor", role)
	}
	if admin, _ := h.store.IsOrgAdmin(ctx, id, h.org.ID); admin {
		t.Fatal("SCIM created an org_admin")
	}
	if code, _ := scimDo(t, h.server, tok, http.MethodPost, "/scim/v2/Users", create); code != http.StatusConflict {
		t.Errorf("duplicate create = %d", code)
	}
	code, out = scimDo(t, h.server, tok, http.MethodGet, "/scim/v2/Users?filter="+urlq(`userName eq "ANA@acme.example"`), "")
	if code != http.StatusOK || out["totalResults"].(float64) != 1 {
		t.Errorf("lookup after create = %d %v", code, out)
	}
	// An account that predates SCIM is found by its handle, so a provider can link it.
	code, out = scimDo(t, h.server, tok, http.MethodGet, "/scim/v2/Users?filter="+urlq(`userName eq "bob"`), "")
	if code != http.StatusOK || out["totalResults"].(float64) != 1 {
		t.Errorf("pre-existing account lookup = %d %v", code, out)
	}

	// Okta deactivates with a path-less replace.
	userTok, _ := h.store.CreateToken(ctx, id, "cli", time.Hour)
	if code, _ := h.do(userTok, http.MethodGet, "/v1/whoami", nil); code != http.StatusOK {
		t.Fatal("ana's token does not work before deactivation")
	}
	code, user = scimDo(t, h.server, tok, http.MethodPatch, "/scim/v2/Users/"+string(id),
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","value":{"active":false}}]}`)
	if code != http.StatusOK || user["active"] != false {
		t.Fatalf("deactivate = %d %v", code, user)
	}
	if code, _ := h.do(userTok, http.MethodGet, "/v1/whoami", nil); code != http.StatusUnauthorized {
		t.Errorf("a deactivated user's token = %d", code)
	}
	if role, err := h.store.RoleIn(ctx, h.project.ID, id); err != nil || role != domain.RoleContributor {
		t.Errorf("deactivation should keep the membership, inert: %s %v", role, err)
	}
	// PUT replaces, and active=true reactivates.
	code, user = scimDo(t, h.server, tok, http.MethodPut, "/scim/v2/Users/"+string(id),
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"ana.diaz@acme.example","displayName":"Ana D.",
		  "emails":[{"value":"ana.diaz@acme.example","primary":true}],"active":true}`)
	if code != http.StatusOK || user["userName"] != "ana.diaz@acme.example" || user["active"] != true || user["displayName"] != "Ana D." {
		t.Fatalf("replace = %d %v", code, user)
	}
	// DELETE: gone from SCIM and from every project, the row kept for the audit trail.
	if code, _ := scimDo(t, h.server, tok, http.MethodDelete, "/scim/v2/Users/"+string(id), ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := scimDo(t, h.server, tok, http.MethodGet, "/scim/v2/Users/"+string(id), ""); code != http.StatusNotFound {
		t.Errorf("GET after delete = %d", code)
	}
	if _, err := h.store.RoleIn(ctx, h.project.ID, id); err == nil {
		t.Error("a deleted user kept a membership")
	}
	if _, err := h.store.GetPrincipal(ctx, id); err != nil {
		t.Errorf("the principal row was deleted: %v", err)
	}
	actions := strings.Join(h.auditActions(string(id)), ",")
	for _, a := range []string{"scim.user_created", "scim.user_deactivated", "scim.user_reactivated", "scim.user_deleted"} {
		if !strings.Contains(actions, a) {
			t.Errorf("%s not audited (%s)", a, actions)
		}
	}
}

func TestSCIMEntraShapesAndGroups(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, rootTok := h.orgAdmin("root")
	_, tok := mintSCIM(t, h, h.server, rootTok)
	code, user := scimDo(t, h.server, tok, http.MethodPost, "/scim/v2/Users",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User","urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],
		  "externalId":"0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef","userName":"bo@contoso.example","active":"True",
		  "emails":[{"primary":"true","type":"work","value":"bo@contoso.example"}],"name":{"formatted":"Bo Ek","familyName":"Ek","givenName":"Bo"},
		  "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"Eng"}}`)
	if code != http.StatusCreated {
		t.Fatalf("entra create = %d %v", code, user)
	}
	id := user["id"].(string)
	// No default project in the policy: the account exists, with no access until a person
	// or a group mapping gives it some.
	if ok, _ := h.store.HasMembership(ctx, domain.ID(id)); ok {
		t.Error("a SCIM user got a membership with no default project configured")
	}
	code, out := scimDo(t, h.server, tok, http.MethodGet, "/scim/v2/Users?filter="+urlq(`externalId eq "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef"`), "")
	if code != http.StatusOK || out["totalResults"].(float64) != 1 {
		t.Errorf("externalId lookup = %d %v", code, out)
	}
	code, user = scimDo(t, h.server, tok, http.MethodPatch, "/scim/v2/Users/"+id,
		`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
		  {"op":"Replace","path":"active","value":"False"},
		  {"op":"Replace","path":"emails[type eq \"work\"].value","value":"bo.ek@contoso.example"},
		  {"op":"Add","path":"title","value":"Engineer"}]}`)
	if code != http.StatusOK || user["active"] != false || user["emails"].([]any)[0].(map[string]any)["value"] != "bo.ek@contoso.example" {
		t.Fatalf("entra patch = %d %v", code, user)
	}
	if code, out := scimDo(t, h.server, tok, http.MethodGet, "/scim/v2/Users?filter="+urlq(`userName co "bo"`), ""); code != http.StatusBadRequest || out["scimType"] != "invalidFilter" {
		t.Errorf("unsupported filter = %d %v", code, out)
	}

	// Groups, with Entra's member removal shape and Okta's.
	code, group := scimDo(t, h.server, tok, http.MethodPost, "/scim/v2/Groups",
		`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Engineering","externalId":"g1","members":[]}`)
	if code != http.StatusCreated {
		t.Fatalf("group = %d %v", code, group)
	}
	gid := group["id"].(string)
	patch := func(body string) map[string]any {
		t.Helper()
		code, g := scimDo(t, h.server, tok, http.MethodPatch, "/scim/v2/Groups/"+gid, body)
		if code != http.StatusOK {
			t.Fatalf("group patch %s = %d %v", body, code, g)
		}
		return g
	}
	g := patch(`{"Operations":[{"op":"Add","path":"members","value":[{"value":"` + id + `"},{"value":"` + string(h.bob.ID) + `"}]}]}`)
	if len(g["members"].([]any)) != 2 {
		t.Errorf("after add = %v", g["members"])
	}
	g = patch(`{"Operations":[{"op":"Remove","path":"members","value":[{"value":"` + id + `"}]}]}`)
	if len(g["members"].([]any)) != 1 {
		t.Errorf("after removing one by value = %v", g["members"])
	}
	g = patch(`{"Operations":[{"op":"remove","path":"members[value eq \"` + string(h.bob.ID) + `\"]"}]}`)
	if len(g["members"].([]any)) != 0 {
		t.Errorf("after removals = %v", g["members"])
	}
	g = patch(`{"Operations":[{"op":"replace","value":{"id":"` + gid + `","displayName":"Eng"}}]}`)
	if g["displayName"] != "Eng" {
		t.Errorf("rename = %v", g)
	}
	// A member from another organization is never added.
	other := newHarness(t)
	g = patch(`{"Operations":[{"op":"add","path":"members","value":[{"value":"` + string(other.alice.ID) + `"}]}]}`)
	if len(g["members"].([]any)) != 0 {
		t.Errorf("a foreign principal joined the group: %v", g["members"])
	}
	code, out = scimDo(t, h.server, tok, http.MethodGet, "/scim/v2/Groups?filter="+urlq(`displayName eq "eng"`)+"&excludedAttributes=members", "")
	if code != http.StatusOK || out["totalResults"].(float64) != 1 || out["Resources"].([]any)[0].(map[string]any)["members"] != nil {
		t.Errorf("group lookup = %d %v", code, out)
	}
	if code, _ := scimDo(t, h.server, tok, http.MethodDelete, "/scim/v2/Groups/"+gid, ""); code != http.StatusNoContent {
		t.Errorf("group delete = %d", code)
	}
	for _, path := range []string{"/scim/v2/ResourceTypes", "/scim/v2/Schemas"} {
		if code, out := scimDo(t, h.server, tok, http.MethodGet, path, ""); code != http.StatusOK || out["totalResults"].(float64) != 2 {
			t.Errorf("%s = %d %v", path, code, out)
		}
	}
}

func TestSCIMCannotStrandAProject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, rootTok := h.orgAdmin("root")
	_, tok := mintSCIM(t, h, h.server, rootTok)
	carol, _ := h.member("carol", "")
	solo := h.secondProject("solo")
	if err := h.store.AddMember(ctx, solo.ID, carol.ID, domain.RoleProjectAdmin); err != nil {
		t.Fatal(err)
	}
	for _, req := range []struct{ method, body string }{
		{http.MethodPatch, `{"Operations":[{"op":"replace","path":"active","value":false}]}`},
		{http.MethodDelete, ""},
	} {
		code, out := scimDo(t, h.server, tok, req.method, "/scim/v2/Users/"+string(carol.ID), req.body)
		if code != http.StatusForbidden || !strings.Contains(out["detail"].(string), "last administrator") {
			t.Errorf("%s of the last administrator = %d %v", req.method, code, out)
		}
	}
	if dead, _ := h.store.PrincipalDeactivated(ctx, carol.ID); dead {
		t.Error("the last administrator was deactivated")
	}
}
