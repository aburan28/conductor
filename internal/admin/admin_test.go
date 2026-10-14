package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

func ptr[T any](v T) *T { return &v }

func valid(t *testing.T, p Policy) {
	t.Helper()
	p.Normalize()
	if err := p.Validate(); err != nil {
		t.Fatalf("policy refused: %v", err)
	}
}

func invalid(t *testing.T, p Policy, want string) {
	t.Helper()
	p.Normalize()
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("validate = %v, want an error mentioning %q", err, want)
	}
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a validation error must map to 400: %v", err)
	}
}

func TestDefaultsAreValidAndSimple(t *testing.T) {
	p := Defaults()
	valid(t, p)
	for _, f := range Features {
		if p.FeatureOn(f.Key) {
			t.Errorf("feature %s is on for a new organization", f.Key)
		}
	}
	if p.HumanTokenCap() != MaxHumanTokenTTL || p.DefaultRole != domain.RoleContributor || p.MaxGroupRole != domain.RoleMaintainer {
		t.Errorf("defaults = %+v", p)
	}
}

func TestPolicyValidation(t *testing.T) {
	invalid(t, Policy{DefaultRole: domain.RoleMaintainer}, "default_role")
	invalid(t, Policy{DefaultRole: domain.RoleOrgAdmin}, "default_role")
	invalid(t, Policy{AutoProvision: true, DefaultProject: "app"}, "restrict it")
	invalid(t, Policy{AutoProvision: true, AllowedDomains: []string{"acme.com"}}, "default_project")
	valid(t, Policy{AutoProvision: true, AllowedDomains: []string{"@ACME.com"}, DefaultProject: "app"})
	invalid(t, Policy{AllowedDomains: []string{"not a domain"}}, "allowed_domains")
	invalid(t, Policy{AllowedGitHubOrgs: []string{"no/slash"}}, "allowed_github_orgs")
	invalid(t, Policy{MaxGroupRole: domain.RoleOrgAdmin}, "max_group_role")
	invalid(t, Policy{GroupRules: []GroupRule{{Group: "eng", Project: "app", Role: domain.RoleOrgAdmin}}}, "group_rules[0]")
	invalid(t, Policy{GroupRules: []GroupRule{{Group: "eng", Project: "app", Role: domain.RoleProjectAdmin}}}, "above max_group_role")
	invalid(t, Policy{GroupRules: []GroupRule{{Group: "eng", Role: domain.RoleContributor}}}, "set default_project")
	invalid(t, Policy{GroupRules: []GroupRule{{Group: "eng", Project: "app", Role: domain.RoleRunner}}}, "role must be")
	invalid(t, Policy{HumanTokenMaxTTL: Duration(91 * 24 * time.Hour)}, "human_token_max_ttl")
	invalid(t, Policy{HumanTokenMaxTTL: Duration(time.Minute)}, "human_token_max_ttl")
	invalid(t, Policy{ServiceTokenMaxTTL: Duration(time.Minute)}, "service_token_max_ttl")
	invalid(t, Policy{HumanTokenMaxTTL: Duration(24 * time.Hour), SSOSessionTTL: Duration(48 * time.Hour)}, "longer than")
	invalid(t, Policy{Features: map[string]bool{"teleport": true}}, "not a feature")
}

func TestTokenCaps(t *testing.T) {
	p := Policy{HumanTokenMaxTTL: Duration(7 * 24 * time.Hour), ServiceTokenMaxTTL: Duration(30 * 24 * time.Hour)}
	if got := p.CapTokenTTL(domain.PrincipalHuman, 90*24*time.Hour); got != 7*24*time.Hour {
		t.Errorf("human cap = %s", got)
	}
	if got := p.CapTokenTTL(domain.PrincipalHuman, time.Hour); got != time.Hour {
		t.Errorf("a shorter human token was lengthened: %s", got)
	}
	if got := p.CapTokenTTL(domain.PrincipalRunner, 0); got != 30*24*time.Hour {
		t.Errorf("a service token without expiry was not capped: %s", got)
	}
	if got := (Policy{}).CapTokenTTL(domain.PrincipalRunner, 0); got != 0 {
		t.Errorf("no service cap configured, yet no-expiry became %s", got)
	}
	if got := (Policy{}).CapTokenTTL(domain.PrincipalHuman, 0); got != MaxHumanTokenTTL {
		t.Errorf("a human token without expiry = %s", got)
	}
}

func TestPatchAndLocks(t *testing.T) {
	stored := Defaults()
	stored.AllowedDomains = []string{"acme.com"}
	locks := Locks{Patch: Patch{RequireSSO: ptr(true), Features: map[string]bool{"queue": true},
		Branding: &BrandingPatch{DisplayName: ptr("Acme")}}}
	eff := locks.Effective(stored)
	if !eff.RequireSSO || !eff.FeatureOn("queue") || eff.Branding.DisplayName != "Acme" || eff.AllowedDomains[0] != "acme.com" {
		t.Fatalf("effective = %+v", eff)
	}
	if stored.RequireSSO || stored.Features["queue"] {
		t.Fatal("Effective changed the stored policy")
	}
	for key, patch := range map[string]Patch{
		KeyRequireSSO:            {RequireSSO: ptr(false)},
		FeatureKey("queue"):      {Features: map[string]bool{"queue": false}},
		KeyDisplayName:           {Branding: &BrandingPatch{DisplayName: ptr("Other")}},
		KeyRequireSSO + " again": {RequireSSO: ptr(true), AllowedDomains: &[]string{"x.com"}},
	} {
		err := locks.CheckUnlocked(patch)
		var le *ErrLocked
		if !errors.As(err, &le) || !errors.Is(err, domain.ErrNotPermitted) || !strings.Contains(err.Error(), "config file") {
			t.Errorf("%s: patch of a locked setting = %v", key, err)
		}
	}
	if err := locks.CheckUnlocked(Patch{AllowedDomains: &[]string{"x.com"}, Features: map[string]bool{"swarm": true}}); err != nil {
		t.Errorf("unlocked settings refused: %v", err)
	}
	if got := strings.Join(locks.Keys(), ","); got != "branding.display_name,features.queue,require_sso" {
		t.Errorf("locked keys = %s", got)
	}
	logo := Logo{ContentType: "image/png"}
	if !(Locks{Logo: &logo}).Locked(KeyLogo) {
		t.Error("a config-file logo does not lock the logo")
	}
}

func TestPatchRoundTripsAsJSON(t *testing.T) {
	var p Patch
	if err := json.Unmarshal([]byte(`{"human_token_max_ttl":"30d","sso_session_ttl":"8h","features":{"swarm":true}}`), &p); err != nil {
		t.Fatal(err)
	}
	got := p.Apply(Defaults())
	if got.HumanTokenMaxTTL.Std() != 30*24*time.Hour || got.SSOSessionTTL.Std() != 8*time.Hour || !got.FeatureOn("swarm") {
		t.Fatalf("applied = %+v", got)
	}
	out, _ := json.Marshal(got)
	if !strings.Contains(string(out), `"human_token_max_ttl":"30d"`) {
		t.Errorf("a day duration does not read back as days: %s", out)
	}
	if err := json.Unmarshal([]byte(`{"human_token_max_ttl":"forever"}`), &p); err == nil {
		t.Error("a nonsense duration was accepted")
	}
}

func TestGroupMappingStaysWithinBounds(t *testing.T) {
	p := Policy{DefaultProject: "app", MaxGroupRole: domain.RoleMaintainer, GroupRules: []GroupRule{
		{Group: "Eng", Role: domain.RoleContributor},
		{Group: "eng-leads", Role: domain.RoleMaintainer},
		{Group: "readers", Project: "docs", Role: domain.RoleObserver},
		// A rule stored before the ceiling was lowered is capped, not honoured.
		{Group: "legacy-admins", Role: domain.RoleProjectAdmin},
	}}
	got := p.MappedRoles([]string{"eng", "EnG-LeAdS", "readers"})
	if got["app"] != domain.RoleMaintainer || got["docs"] != domain.RoleObserver || len(got) != 2 {
		t.Fatalf("mapped = %v", got)
	}
	if got := p.MappedRoles([]string{"legacy-admins"}); got["app"] != domain.RoleMaintainer {
		t.Fatalf("a rule above the ceiling mapped to %v", got)
	}
	if got := p.MappedRoles([]string{"strangers"}); len(got) != 0 {
		t.Fatalf("unmatched groups mapped to %v", got)
	}

	cases := []struct {
		current domain.Role
		member  bool
		mapped  domain.Role
		want    RoleChange
	}{
		{"", false, domain.RoleContributor, RoleAdd},
		{domain.RoleObserver, true, domain.RoleContributor, RoleSet},
		{domain.RoleMaintainer, true, domain.RoleContributor, RoleSet},    // demotion within the ceiling
		{domain.RoleProjectAdmin, true, domain.RoleContributor, RoleKeep}, // above the ceiling: a person's grant
		{domain.RoleOrgAdmin, true, domain.RoleObserver, RoleKeep},
		{domain.RoleRunner, true, domain.RoleContributor, RoleKeep},
		{domain.RoleContributor, true, domain.RoleContributor, RoleKeep},
	}
	for _, c := range cases {
		if got := p.PlanRole(c.current, c.member, c.mapped); got != c.want {
			t.Errorf("PlanRole(%s, %v, %s) = %s, want %s", c.current, c.member, c.mapped, got, c.want)
		}
	}
}

func TestBrandingValidation(t *testing.T) {
	invalid(t, Policy{Branding: Branding{AccentColor: "red"}}, "hex color")
	invalid(t, Policy{Branding: Branding{AccentColor: "#ffff00"}}, "contrast ratio")
	invalid(t, Policy{Branding: Branding{DisplayName: strings.Repeat("x", MaxDisplayName+1)}}, "display_name")
	invalid(t, Policy{Branding: Branding{DisplayName: "two\nlines"}}, "one line")
	invalid(t, Policy{Branding: Branding{LoginBanner: "evil \u202e override"}}, "plain text")
	invalid(t, Policy{Branding: Branding{LoginBanner: strings.Repeat("x", MaxLoginBanner+1)}}, "login_banner")
	p := Policy{Branding: Branding{AccentColor: "#26A", LoginBanner: "Authorized use only.\nActivity is logged."}}
	valid(t, p)
	p.Normalize()
	if p.Branding.AccentColor != "#2266aa" {
		t.Fatalf("a short hex color is not expanded: %s", p.Branding.AccentColor)
	}
	invalid(t, Policy{Branding: Branding{AccentColor: "#2F6"}}, "contrast ratio")
}

func TestContrast(t *testing.T) {
	if r := ContrastRatio("#000000", "#ffffff"); r < 20.9 || r > 21.1 {
		t.Errorf("black on white = %.2f", r)
	}
	if OnAccent("#2f6f4e") != "#ffffff" || OnAccent("#ffd700") != "#111111" {
		t.Error("OnAccent picks the wrong text color")
	}
}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLogoChecks(t *testing.T) {
	ok, err := CheckLogo(pngOf(t, 32, 32), "image/png")
	if err != nil || ok.ContentType != "image/png" || len(ok.SHA256) != 64 {
		t.Fatalf("a small PNG = %+v, %v", ok, err)
	}
	for name, c := range map[string]struct {
		data     []byte
		declared string
		want     string
	}{
		"svg":         {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "", "PNG, JPEG or GIF"},
		"html":        {[]byte("<html><script>alert(1)</script></html>"), "image/png", "PNG, JPEG or GIF"},
		"empty":       {nil, "", "empty"},
		"oversized":   {append(pngOf(t, 8, 8), make([]byte, MaxLogoBytes)...), "", "at most"},
		"lying type":  {pngOf(t, 8, 8), "image/gif", "sent as image/gif"},
		"huge pixels": {pngOf(t, MaxLogoDimension+1, 1), "", "at most"},
		"truncated":   {pngOf(t, 8, 8)[:20], "", "could not be read"},
	} {
		if _, err := CheckLogo(c.data, c.declared); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestSCIMUserShapes(t *testing.T) {
	// Okta's create.
	u, err := ParseSCIMUser([]byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"ana@acme.com",
		"name":{"givenName":"Ana","familyName":"Diaz"},"emails":[{"primary":true,"value":"Ana@Acme.com","type":"work"}],
		"displayName":"Ana Diaz","locale":"en-US","externalId":"00u1","groups":[],"password":"x","active":true,
		"roles":[{"value":"org_admin"}]}`))
	if err != nil || u.PrimaryEmail() != "ana@acme.com" || u.FullName() != "Ana Diaz" || !bool(*u.Active) || u.ExternalID != "00u1" {
		t.Fatalf("okta user = %+v, %v", u, err)
	}
	// Entra's create: no displayName at the top, an enterprise extension, active as a string.
	u, err = ParseSCIMUser([]byte(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User",
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],"externalId":"0a21f0f2","userName":"bo@contoso.com",
		"active":"True","emails":[{"primary":"true","type":"work","value":"bo@contoso.com"}],
		"name":{"formatted":"Bo Ek","familyName":"Ek","givenName":"Bo"},
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User":{"department":"Eng"}}`))
	if err != nil || u.PrimaryEmail() != "bo@contoso.com" || u.FullName() != "Bo Ek" || !bool(*u.Active) {
		t.Fatalf("entra user = %+v, %v", u, err)
	}
	if _, err := ParseSCIMUser([]byte(`{"name":{}}`)); err == nil {
		t.Error("a user without userName was accepted")
	}
}

func TestSCIMUserPatchShapes(t *testing.T) {
	// Okta deactivation.
	p, err := ParseUserPatch([]byte(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"replace","value":{"active":false}}]}`))
	if err != nil || p.Active == nil || *p.Active {
		t.Fatalf("okta deactivate = %+v, %v", p, err)
	}
	// Entra deactivation and attribute updates.
	p, err = ParseUserPatch([]byte(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[
		{"op":"Replace","path":"active","value":"False"},
		{"op":"Add","path":"emails[type eq \"work\"].value","value":"New@Contoso.com"},
		{"op":"Replace","path":"name.givenName","value":"Bob"},
		{"op":"Add","path":"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department","value":"Eng"},
		{"op":"Replace","path":"title","value":"CEO"}]}`))
	if err != nil || p.Active == nil || *p.Active || deref(p.Email) != "new@contoso.com" || deref(p.GivenName) != "Bob" {
		t.Fatalf("entra patch = %+v, %v", p, err)
	}
	// Entra sends dotted names inside a path-less value too.
	p, err = ParseUserPatch([]byte(`{"Operations":[{"op":"replace","value":{"name.familyName":"Ek","displayName":"Bo Ek"}}]}`))
	if err != nil || deref(p.FamilyName) != "Ek" || deref(p.DisplayName) != "Bo Ek" {
		t.Fatalf("dotted value patch = %+v, %v", p, err)
	}
	for _, bad := range []string{`{"Operations":[]}`, `{"Operations":[{"op":"move","path":"active"}]}`,
		`{"Operations":[{"op":"remove","path":"active"}]}`, `{"Operations":[{"op":"replace","path":"active","value":"maybe"}]}`} {
		if _, err := ParseUserPatch([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestSCIMGroupPatchShapes(t *testing.T) {
	p, err := ParseGroupPatch([]byte(`{"Operations":[
		{"op":"add","path":"members","value":[{"value":"u1"},{"value":"u2"}]},
		{"op":"remove","path":"members[value eq \"u3\"]"},
		{"op":"Remove","path":"members","value":[{"value":"u4"}]},
		{"op":"replace","value":{"displayName":"Engineering"}}]}`))
	if err != nil || strings.Join(p.Add, ",") != "u1,u2" || strings.Join(p.Remove, ",") != "u3,u4" ||
		deref(p.DisplayName) != "Engineering" || p.ReplaceMembers != nil {
		t.Fatalf("group patch = %+v, %v", p, err)
	}
	p, err = ParseGroupPatch([]byte(`{"Operations":[{"op":"replace","path":"members","value":[{"value":"u9"}]}]}`))
	if err != nil || p.ReplaceMembers == nil || strings.Join(*p.ReplaceMembers, ",") != "u9" {
		t.Fatalf("replace members = %+v, %v", p, err)
	}
	if _, err := ParseGroupPatch([]byte(`{"Operations":[{"op":"add","path":"owners","value":[]}]}`)); err == nil {
		t.Error("an unknown group path was accepted")
	}
}

func TestSCIMFilter(t *testing.T) {
	f, err := ParseSCIMFilter(`userName eq "ana@acme.com"`, "userName", "externalId")
	if err != nil || f.Attribute != "username" || f.Value != "ana@acme.com" {
		t.Fatalf("filter = %+v, %v", f, err)
	}
	f, err = ParseSCIMFilter(`externalId EQ "a \"quoted\" id"`, "userName", "externalId")
	if err != nil || f.Value != `a "quoted" id` {
		t.Fatalf("escaped filter = %+v, %v", f, err)
	}
	if f, err := ParseSCIMFilter("", "userName"); f != nil || err != nil {
		t.Errorf("empty filter = %v, %v", f, err)
	}
	for _, bad := range []string{`userName co "a"`, `password eq "x"`, `userName eq "a" or userName eq "b"`, `userName eq a`} {
		var se *SCIMError
		if _, err := ParseSCIMFilter(bad, "userName"); !errors.As(err, &se) || se.SCIMType != "invalidFilter" {
			t.Errorf("%s: %v", bad, err)
		}
	}
}
