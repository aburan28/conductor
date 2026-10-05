package sso_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/sso"
	"github.com/aburan28/conductor/internal/sso/ssotest"
)

// Microsoft Entra ID asserts no email_verified, so without an explicit trust it cannot sign
// anyone in; with one, only a single tenant's own members in the listed domains get through.

const tenant = "8f1c2b3a-4d5e-6f70-8192-a3b4c5d6e7f8"

func entraProvider(t *testing.T, f *ssotest.OIDC, domains ...string) sso.Provider {
	t.Helper()
	return oidcProvider(t, f, func(c *sso.Config) { c.TrustedEmailDomains = domains })
}

func TestEntraIsRefusedWithoutTrust(t *testing.T) {
	f := ssotest.NewEntra(t, tenant)
	f.SetUser(ssotest.User{Subject: "oid-1", Email: "ana@contoso.com"})
	_, err := exchange(t, oidcProvider(t, f, nil))
	wantCode(t, err, sso.CodeEmailUnverified)
}

func TestEntraTrustedDomainSignsIn(t *testing.T) {
	f := ssotest.NewEntra(t, tenant)
	f.SetUser(ssotest.User{Subject: "oid-1", Email: "ana@contoso.com", Groups: []string{"eng", "ops"}})
	id, err := exchange(t, entraProvider(t, f, "contoso.com"))
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "ana@contoso.com" || id.Subject != "oid-1" || !slices.Equal(id.Groups, []string{"eng", "ops"}) {
		t.Fatalf("identity = %+v", id)
	}
}

func TestEntraPrefersTheUPN(t *testing.T) {
	// The email attribute is whatever a tenant administrator typed; the UPN's domain is one
	// the tenant proved it owns. With both present the UPN wins.
	f := ssotest.NewEntra(t, tenant)
	f.SetUser(ssotest.User{Subject: "oid-2", Email: "ceo@victim.example", UPN: "mallory@contoso.com"})
	id, err := exchange(t, entraProvider(t, f, "contoso.com", "victim.example"))
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "mallory@contoso.com" {
		t.Fatalf("email = %s, want the UPN", id.Email)
	}
}

func TestEntraRefusesUntrustedDomainsAndOtherTenants(t *testing.T) {
	f := ssotest.NewEntra(t, tenant)
	f.SetUser(ssotest.User{Subject: "oid-3", Email: "eve@fabrikam.com"})
	_, err := exchange(t, entraProvider(t, f, "contoso.com"))
	wantCode(t, err, sso.CodeEmailUnverified)

	// A token for another tenant (tid) is refused even under this tenant's issuer.
	f.SetUser(ssotest.User{Subject: "oid-4", Email: "ana@contoso.com"})
	f.Tamper(func(c map[string]any) { c["tid"] = "00000000-0000-0000-0000-000000000000" })
	_, err = exchange(t, entraProvider(t, f, "contoso.com"))
	wantCode(t, err, sso.CodeEmailUnverified)

	// A guest: authenticated by another identity provider.
	f.Tamper(func(c map[string]any) { c["idp"] = "https://sts.windows.net/another-tenant/" })
	_, err = exchange(t, entraProvider(t, f, "contoso.com"))
	wantCode(t, err, sso.CodeEmailUnverified)
	f.Tamper(func(c map[string]any) { c["acct"] = 1 })
	_, err = exchange(t, entraProvider(t, f, "contoso.com"))
	wantCode(t, err, sso.CodeEmailUnverified)

	// A guest's UPN is not an address at all (user_fabrikam.com#EXT#@contoso.onmicrosoft.com).
	f.Tamper(nil)
	f.SetUser(ssotest.User{Subject: "oid-5", Email: "x@contoso.com", UPN: "x_fabrikam.com#EXT#@contoso.onmicrosoft.com"})
	id, err := exchange(t, entraProvider(t, f, "contoso.com"))
	// The email claim is the fallback and is in a trusted domain; the guest marker above
	// is what refuses real guests. Here the UPN is skipped and the email used.
	if err != nil || id.Email != "x@contoso.com" {
		t.Fatalf("identity = %+v, %v", id, err)
	}
}

func TestEntraVerifiedEmailStillWins(t *testing.T) {
	// A token that does assert email_verified is taken at its word, trust or not.
	f := ssotest.NewEntra(t, tenant)
	f.SetUser(ssotest.User{Subject: "oid-6", Email: "ana@contoso.com"})
	f.Tamper(func(c map[string]any) { c["email_verified"] = true; c["upn"] = "other@contoso.com" })
	id, err := exchange(t, entraProvider(t, f, "contoso.com"))
	if err != nil || id.Email != "ana@contoso.com" {
		t.Fatalf("identity = %+v, %v", id, err)
	}
}

func TestEntraTrustNeedsASingleTenantIssuer(t *testing.T) {
	for _, issuer := range []string{
		"https://login.microsoftonline.com/common/v2.0",
		"https://login.microsoftonline.com/organizations/v2.0",
		"https://login.microsoftonline.com/consumers/v2.0",
		"https://login.microsoftonline.com/contoso.onmicrosoft.com/v2.0",
		"https://accounts.google.com",
		"https://evil.example/" + tenant + "/v2.0",
	} {
		cfg := sso.Config{Name: "entra", Kind: sso.KindOIDC, Issuer: issuer, ClientID: "c", ClientSecret: "s",
			TrustedEmailDomains: []string{"contoso.com"}}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "single-tenant") {
			t.Errorf("%s: trust accepted (%v)", issuer, err)
		}
	}
	ok := sso.Config{Name: "entra", Kind: sso.KindOIDC, Issuer: "https://login.microsoftonline.com/" + tenant + "/v2.0",
		ClientID: "c", ClientSecret: "s", TrustedEmailDomains: []string{"contoso.com"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok.Label != "Microsoft" {
		t.Errorf("label = %q", ok.Label)
	}
	spec, err := sso.ParseSpec("name=entra,issuer=https://login.microsoftonline.com/"+tenant+"/v2.0,client-id=c,"+
		"trust-email-domain=contoso.com,trust-email-domain=@Contoso.co.uk,groups-claim=roles",
		func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.TrustedEmailDomains, []string{"contoso.com", "contoso.co.uk"}) || spec.GroupsClaim != "roles" {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestGitHubReportsOrgsAndTeams(t *testing.T) {
	g := ssotest.NewGitHub(t)
	g.SetUser(ssotest.GitHubUser{ID: 7, Login: "octo",
		Emails: []ssotest.GitHubEmail{{Email: "octo@example.com", Primary: true, Verified: true}},
		Orgs:   map[string]string{"Acme": "active", "pending-co": "pending"},
		Teams:  map[string][]string{"Acme": {"platform", "Web"}}})
	p, err := sso.New(sso.Config{Name: "github", Kind: sso.KindGitHub, ClientID: g.ClientID, ClientSecret: g.ClientSecret,
		APIURL: g.Server.URL, WebURL: g.Server.URL}, sso.Options{})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := p.AuthCodeURL(context.Background(), sso.AuthRequest{State: "s", Challenge: sso.Challenge(sso.NewVerifier()), RedirectURI: redirectURI})
	if !strings.Contains(u, "read%3Aorg") {
		t.Errorf("authorization URL does not ask for read:org: %s", u)
	}
	id, err := exchange(t, p)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(id.Groups)
	if !slices.Equal(id.Orgs, []string{"acme"}) || !slices.Equal(id.Groups, []string{"acme/platform", "acme/web"}) {
		t.Fatalf("orgs %v groups %v", id.Orgs, id.Groups)
	}
}
