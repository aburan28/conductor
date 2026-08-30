package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// Username/password sign-in. These tests need DATABASE_URL like the rest of the suite; the
// password mechanics themselves are unit-tested in internal/db.

// loginAs posts /v1/login and decodes the envelope.
func (h *harness) loginAs(t *testing.T, username, password, org string) (int, loginResult, []byte) {
	t.Helper()
	body := map[string]any{"username": username, "password": password}
	if org != "" {
		body["org"] = org
	}
	code, raw := h.do("", http.MethodPost, "/v1/login", body)
	var result loginResult
	_ = json.Unmarshal(raw, &result)
	return code, result, raw
}

func TestPasswordLoginMintsAWorkingToken(t *testing.T) {
	h := newHarness(t)
	ctx := h.t.Context()

	handle := uniq("loginuser", time.Now().UnixNano())
	pw := "dashboard-password-1"
	p, err := h.store.CreatePrincipal(ctx, h.org.ID, domain.PrincipalHuman, handle, handle, "")
	if err != nil {
		t.Fatalf("principal: %v", err)
	}
	if err := h.store.AddMember(ctx, h.project.ID, p.ID, domain.RoleContributor); err != nil {
		t.Fatalf("membership: %v", err)
	}
	if err := h.store.SetPassword(ctx, p.ID, pw); err != nil {
		t.Fatalf("set password: %v", err)
	}

	code, result, raw := h.loginAs(t, handle, pw, "")
	if code != http.StatusOK {
		t.Fatalf("login = %d\n%s", code, raw)
	}
	if result.Token == "" {
		t.Fatal("login returned no token")
	}
	if result.Principal.Handle != handle {
		t.Errorf("principal handle = %q, want %q", result.Principal.Handle, handle)
	}
	if len(result.Projects) != 1 || result.Projects[0].Slug != h.project.Slug {
		t.Errorf("login projects = %+v, want exactly %q", result.Projects, h.project.Slug)
	}

	// The minted token is a normal bearer token: the dashboard's first act after sign-in is
	// a whoami, and it must succeed.
	if code, raw := h.do(result.Token, http.MethodGet, "/v1/whoami", nil); code != http.StatusOK {
		t.Errorf("whoami with login token = %d\n%s", code, raw)
	}
}

func TestPasswordLoginFailuresAreIndistinguishable(t *testing.T) {
	h := newHarness(t)
	ctx := h.t.Context()

	handle := uniq("loginuser", time.Now().UnixNano())
	p, err := h.store.CreatePrincipal(ctx, h.org.ID, domain.PrincipalHuman, handle, handle, "")
	if err != nil {
		t.Fatalf("principal: %v", err)
	}
	if err := h.store.SetPassword(ctx, p.ID, "right-password-1"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	// A unique handle with no password ever set. A shared handle like "bob" would be
	// ambiguous across this test database's many harness orgs and answer 400 instead.
	nopass := uniq("nopass", time.Now().UnixNano())
	if _, err := h.store.CreatePrincipal(ctx, h.org.ID, domain.PrincipalHuman, nopass, nopass, ""); err != nil {
		t.Fatalf("principal without password: %v", err)
	}

	cases := []struct {
		name     string
		username string
		password string
	}{
		{"wrong password", handle, "wrong-password-1"},
		{"unknown user", "nobody-here", "right-password-1"},
		{"no password set", nopass, "right-password-1"},
	}
	var bodies []string
	for _, tc := range cases {
		code, _, raw := h.loginAs(t, tc.username, tc.password, "")
		if code != http.StatusUnauthorized {
			t.Errorf("%s: login = %d, want 401\n%s", tc.name, code, raw)
		}
		bodies = append(bodies, string(raw))
	}
	// A probe must not learn whether the username or the password was the wrong part.
	for _, raw := range bodies[1:] {
		if raw != bodies[0] {
			t.Errorf("failure responses differ:\n%s\n%s", bodies[0], raw)
		}
	}
}

func TestPasswordSetEnablesLogin(t *testing.T) {
	h := newHarness(t)

	handle := uniq("loginuser", time.Now().UnixNano())
	p, err := h.store.CreatePrincipal(h.t.Context(), h.org.ID, domain.PrincipalHuman, handle, handle, "")
	if err != nil {
		t.Fatalf("principal: %v", err)
	}
	if err := h.store.AddMember(h.t.Context(), h.project.ID, p.ID, domain.RoleContributor); err != nil {
		t.Fatalf("membership: %v", err)
	}
	tok, err := h.store.CreateToken(h.t.Context(), p.ID, "test", 0)
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	// Too short is rejected before anything is stored.
	if code, raw := h.do(tok, http.MethodPost, "/v1/password", map[string]any{"password": "short"}); code != http.StatusBadRequest {
		t.Errorf("short password = %d, want 400\n%s", code, raw)
	}

	const pw = "set-via-dashboard-1"
	if code, raw := h.do(tok, http.MethodPost, "/v1/password", map[string]any{"password": pw}); code != http.StatusOK {
		t.Fatalf("set password = %d\n%s", code, raw)
	}
	if code, _, raw := h.loginAs(t, handle, pw, ""); code != http.StatusOK {
		t.Errorf("login after set = %d\n%s", code, raw)
	}
}

func TestAmbiguousHandleRequiresOrg(t *testing.T) {
	h := newHarness(t)
	ctx := h.t.Context()

	handle := uniq("dupuser", time.Now().UnixNano())
	// The same handle in a second organization: handles are unique per org, not globally.
	other, err := h.store.CreateOrganization(ctx, uniq("dup-org", time.Now().UnixNano()), "Dup")
	if err != nil {
		t.Fatalf("second org: %v", err)
	}
	for _, orgID := range []domain.ID{h.org.ID, other.ID} {
		p, err := h.store.CreatePrincipal(ctx, orgID, domain.PrincipalHuman, handle, handle, "")
		if err != nil {
			t.Fatalf("principal in %s: %v", orgID, err)
		}
		if err := h.store.SetPassword(ctx, p.ID, "same-password-1"); err != nil {
			t.Fatalf("set password: %v", err)
		}
	}

	code, _, raw := h.loginAs(t, handle, "same-password-1", "")
	if code != http.StatusBadRequest {
		t.Errorf("ambiguous login = %d, want 400\n%s", code, raw)
	}
	if !strings.Contains(string(raw), "org") {
		t.Errorf("ambiguity error does not mention passing org:\n%s", raw)
	}
	code, result, raw := h.loginAs(t, handle, "same-password-1", h.org.Slug)
	if code != http.StatusOK {
		t.Fatalf("login with org = %d\n%s", code, raw)
	}
	if result.Principal.OrganizationID != h.org.ID {
		t.Errorf("login with org resolved the wrong principal")
	}
}
