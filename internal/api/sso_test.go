package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/sso"
	"github.com/aburan28/conductor/internal/sso/ssotest"
)

// Single sign-on, end to end: a fake OpenID Connect issuer and a fake GitHub, a real control
// plane on Postgres, and a cookie-keeping http.Client standing in for the browser.

type ssoHarness struct {
	*harness
	idp *ssotest.OIDC
	gh  *ssotest.GitHub
	srv *httptest.Server
	// tag makes emails and subjects unique to this test: identities and principal emails
	// are global, and the test database is shared.
	tag string
}

func newSSOHarness(t *testing.T, configure func(o *SSOOptions, sh *ssoHarness)) *ssoHarness {
	t.Helper()
	sh := &ssoHarness{harness: newHarness(t), idp: ssotest.NewOIDC(t), gh: ssotest.NewGitHub(t),
		tag: fmt.Sprintf("%x", time.Now().UnixNano())}
	sh.idp.SetUser(ssotest.User{Subject: "sub-" + sh.tag, Email: sh.email("bob"), EmailVerified: true, Name: "Bob"})
	sh.gh.SetUser(ssotest.GitHubUser{ID: time.Now().UnixNano() % 1e12, Login: "octo",
		Emails: []ssotest.GitHubEmail{{Email: sh.email("bob"), Primary: true, Verified: true}},
		Orgs:   map[string]string{"acme": "active"}})

	var handler http.Handler
	sh.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(sh.srv.Close)

	oidc, err := sso.New(sso.Config{Name: "test", Kind: sso.KindOIDC, Label: "Test IdP", Issuer: sh.idp.Issuer,
		ClientID: sh.idp.ClientID, ClientSecret: sh.idp.ClientSecret}, sso.Options{})
	if err != nil {
		t.Fatal(err)
	}
	gh, err := sso.New(sso.Config{Name: "github", Kind: sso.KindGitHub, ClientID: sh.gh.ClientID,
		ClientSecret: sh.gh.ClientSecret, APIURL: sh.gh.Server.URL, WebURL: sh.gh.Server.URL, Orgs: []string{"acme"}}, sso.Options{})
	if err != nil {
		t.Fatal(err)
	}
	opts := SSOOptions{Providers: []sso.Provider{oidc, gh}, PublicURL: sh.srv.URL}
	if configure != nil {
		configure(&opts, sh)
	}
	handler = New(sh.store, coord.New(sh.store), Options{SSO: opts, SelfEndpoint: sh.srv.URL}).Handler()
	return sh
}

func (sh *ssoHarness) email(who string) string { return who + "-" + sh.tag + "@example.com" }

// register sets the address an administrator registered on a principal.
func (sh *ssoHarness) register(p domain.Principal, email string) {
	sh.t.Helper()
	if err := sh.store.SetPrincipalEmail(context.Background(), p.ID, email); err != nil {
		sh.t.Fatal(err)
	}
}

// browser is a fresh browser: its own cookies, following redirects.
func browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

// signIn runs a dashboard sign-in in b and returns the fragment it ends on.
func (sh *ssoHarness) signIn(b *http.Client, provider string) url.Values {
	sh.t.Helper()
	resp, err := b.Get(sh.srv.URL + "/v1/sso/" + provider + "/start?next=/tasks")
	if err != nil {
		sh.t.Fatal(err)
	}
	resp.Body.Close()
	frag, _ := url.ParseQuery(resp.Request.URL.Fragment)
	if frag.Get("sso") == "" && frag.Get("sso_error") == "" {
		sh.t.Fatalf("sign-in ended at %s (%d) with neither a ticket nor an error", resp.Request.URL, resp.StatusCode)
	}
	if frag.Get("sso") != "" && resp.Request.URL.Path != "/tasks" {
		sh.t.Fatalf("sign-in returned to %s, want /tasks", resp.Request.URL.Path)
	}
	return frag
}

// redeem posts a ticket from b.
func (sh *ssoHarness) redeem(b *http.Client, body map[string]any) (int, map[string]any) {
	sh.t.Helper()
	encoded, _ := json.Marshal(body)
	resp, err := b.Post(sh.srv.URL+"/v1/sso/redeem", "application/json", strings.NewReader(string(encoded)))
	if err != nil {
		sh.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// signInToken signs in through the dashboard flow and returns the token.
func (sh *ssoHarness) signInToken(provider string) string {
	sh.t.Helper()
	b := browser()
	frag := sh.signIn(b, provider)
	if frag.Get("sso_error") != "" {
		sh.t.Fatalf("sign-in refused: %s", frag.Get("sso_error"))
	}
	code, out := sh.redeem(b, map[string]any{"ticket": frag.Get("sso")})
	if code != http.StatusCreated {
		sh.t.Fatalf("redeem = %d %v", code, out)
	}
	return out["token"].(string)
}

func (sh *ssoHarness) whoami(token string) (int, string) {
	sh.t.Helper()
	code, out := sh.doJSONOn(sh.srv, token, http.MethodGet, "/v1/whoami", nil)
	if code != http.StatusOK {
		return code, ""
	}
	return code, out["principal"].(map[string]any)["handle"].(string)
}

func wantSSOError(t *testing.T, frag url.Values, contains string) {
	t.Helper()
	if frag.Get("sso") != "" {
		t.Fatalf("sign-in succeeded, want an error containing %q", contains)
	}
	if !strings.Contains(frag.Get("sso_error"), contains) {
		t.Fatalf("error = %q, want it to contain %q", frag.Get("sso_error"), contains)
	}
}

// The happy path: a first sign-in links to the principal an administrator registered the
// address on, mints an ordinary token named sso:<provider> that authenticates as that
// principal (in enhanced mode, the harness default), and later sign-ins find the link.
func TestSSOFirstSignInLinksTheRegisteredPrincipal(t *testing.T) {
	sh := newSSOHarness(t, nil)
	sh.register(sh.bob, strings.ToUpper(sh.email("bob")))

	b := browser()
	frag := sh.signIn(b, "test")
	code, out := sh.redeem(b, map[string]any{"ticket": frag.Get("sso")})
	if code != http.StatusCreated || out["handle"] != "bob" || out["provider"] != "test" {
		t.Fatalf("redeem = %d %v", code, out)
	}
	if code, who := sh.whoami(out["token"].(string)); code != http.StatusOK || who != "bob" {
		t.Fatalf("whoami = %d %s", code, who)
	}
	// The ticket is single-use.
	if code, _ := sh.redeem(b, map[string]any{"ticket": frag.Get("sso")}); code != http.StatusUnauthorized {
		t.Fatalf("a redeemed ticket redeemed again = %d, want 401", code)
	}

	tokens, _ := sh.store.ListTokens(context.Background(), sh.bob.ID)
	named := false
	for _, tok := range tokens {
		named = named || tok.Name == "sso:test" && tok.ExpiresAt != nil && tok.ExpiresAt.Before(time.Now().Add(13*time.Hour))
	}
	if !named {
		t.Fatalf("no 12-hour sso:test token among %+v", tokens)
	}
	ids, _ := sh.store.ListIdentities(context.Background(), sh.bob.ID)
	if len(ids) != 1 || ids[0].Subject != "sub-"+sh.tag || ids[0].Issuer != sh.idp.Issuer {
		t.Fatalf("identities = %+v", ids)
	}
	if got := strings.Join(sh.auditActions(sh.bob.ID), ","); !strings.Contains(got, "sso.linked") || !strings.Contains(got, "sso.login") {
		t.Fatalf("audit = %s", got)
	}

	// Later: the identity is found by issuer and subject, whatever the address says now.
	sh.register(sh.bob, "")
	sh.idp.SetUser(ssotest.User{Subject: "sub-" + sh.tag, Email: sh.email("renamed"), EmailVerified: true})
	if _, who := sh.whoami(sh.signInToken("test")); who != "bob" {
		t.Fatalf("second sign-in is %q, want bob", who)
	}
}

func TestSSORefusesAnUnregisteredAddress(t *testing.T) {
	sh := newSSOHarness(t, nil)
	frag := sh.signIn(browser(), "test")
	wantSSOError(t, frag, "No Conductor account is registered for "+sh.email("bob"))
}

// An address registered on an account that already signs in with some identity cannot
// attach a second one — at the same provider or at another.
func TestSSOSecondIdentityCannotTakeOverAPrincipal(t *testing.T) {
	sh := newSSOHarness(t, nil)
	sh.register(sh.bob, sh.email("bob"))
	sh.signInToken("test")

	// A different account at the same issuer claiming the same verified address.
	sh.idp.SetUser(ssotest.User{Subject: "attacker-" + sh.tag, Email: sh.email("bob"), EmailVerified: true})
	wantSSOError(t, sh.signIn(browser(), "test"), "already signs in with a different identity")
	// An account at another provider with the same address.
	wantSSOError(t, sh.signIn(browser(), "github"), "already signs in with a different identity")

	ids, _ := sh.store.ListIdentities(context.Background(), sh.bob.ID)
	if len(ids) != 1 {
		t.Fatalf("bob has %d identities, want 1", len(ids))
	}
}

// A ticket is good only in the browser that started the sign-in; a failed attempt from
// elsewhere does not burn it.
func TestSSOTicketIsBoundToTheBrowser(t *testing.T) {
	sh := newSSOHarness(t, nil)
	sh.register(sh.bob, sh.email("bob"))
	b := browser()
	frag := sh.signIn(b, "test")
	if code, _ := sh.redeem(browser(), map[string]any{"ticket": frag.Get("sso")}); code != http.StatusUnauthorized {
		t.Fatalf("another browser redeemed the ticket: %d", code)
	}
	// Nor is a CLI verifier a substitute for the cookie.
	if code, _ := sh.redeem(browser(), map[string]any{"ticket": frag.Get("sso"), "code_verifier": sso.NewVerifier()}); code != http.StatusUnauthorized {
		t.Fatalf("a verifier redeemed a dashboard ticket: %d", code)
	}
	if code, out := sh.redeem(b, map[string]any{"ticket": frag.Get("sso")}); code != http.StatusCreated {
		t.Fatalf("the right browser could not redeem after a failed attempt: %d %v", code, out)
	}
}

// callbackURL starts a sign-in in b and stops at the provider's redirect back to this
// server, returning that URL unvisited.
func (sh *ssoHarness) callbackURL(b *http.Client, provider string) string {
	sh.t.Helper()
	stop := *b
	stop.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.Contains(req.URL.Path, "/callback") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := stop.Get(sh.srv.URL + "/v1/sso/" + provider + "/start")
	if err != nil {
		sh.t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/v1/sso/"+provider+"/callback?") {
		sh.t.Fatalf("stopped at %d %q, not the callback", resp.StatusCode, loc)
	}
	return loc
}

// Login CSRF: an attacker starts a sign-in with their own account, stops before the
// callback, and sends the link to a victim. The victim's browser has no binding cookie.
func TestSSOCallbackInAnotherBrowserIsRefused(t *testing.T) {
	sh := newSSOHarness(t, nil)
	sh.register(sh.bob, sh.email("bob"))
	link := sh.callbackURL(browser(), "test")
	victim := browser()
	resp, err := victim.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	frag, _ := url.ParseQuery(resp.Request.URL.Fragment)
	wantSSOError(t, frag, "started in a different browser")
}

// The state is single-use: a replayed callback finds nothing, even with the right cookie.
func TestSSOStateIsSingleUse(t *testing.T) {
	sh := newSSOHarness(t, nil)
	sh.register(sh.bob, sh.email("bob"))
	b := browser()
	link := sh.callbackURL(b, "test")
	noFollow := *b
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	first, err := noFollow.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if loc := first.Header.Get("Location"); first.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "#sso=") {
		t.Fatalf("first callback = %d %q", first.StatusCode, loc)
	}
	again, err := noFollow.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback = %d, want 400", again.StatusCode)
	}
}

// The provider's verdict reaches the person signing in, and nothing is linked.
func TestSSOProviderRefusalsAreShown(t *testing.T) {
	sh := newSSOHarness(t, func(o *SSOOptions, sh *ssoHarness) {
		restricted, _ := sso.New(sso.Config{Name: "corp", Kind: sso.KindOIDC, Issuer: sh.idp.Issuer,
			ClientID: sh.idp.ClientID, ClientSecret: sh.idp.ClientSecret, Domains: []string{"corp.example"}}, sso.Options{})
		o.Providers = append(o.Providers, restricted)
	})
	sh.register(sh.bob, sh.email("bob"))

	sh.idp.Tamper(func(c map[string]any) { c["aud"] = "another-client" })
	wantSSOError(t, sh.signIn(browser(), "test"), "could not be verified")
	sh.idp.Tamper(func(c map[string]any) { c["email_verified"] = false })
	wantSSOError(t, sh.signIn(browser(), "test"), "verified email")
	sh.idp.Tamper(nil)
	wantSSOError(t, sh.signIn(browser(), "corp"), "not in a domain allowed")

	if ids, _ := sh.store.ListIdentities(context.Background(), sh.bob.ID); len(ids) != 0 {
		t.Fatalf("a refused sign-in linked %+v", ids)
	}
}

func TestSSOGitHubOrgMembership(t *testing.T) {
	sh := newSSOHarness(t, nil)
	sh.register(sh.bob, sh.email("bob"))
	sh.gh.SetUser(ssotest.GitHubUser{ID: time.Now().UnixNano() % 1e12, Login: "outsider",
		Emails: []ssotest.GitHubEmail{{Email: sh.email("bob"), Primary: true, Verified: true}},
		Orgs:   map[string]string{"elsewhere": "active"}})
	wantSSOError(t, sh.signIn(browser(), "github"), "not an active member of acme")

	sh.gh.SetUser(ssotest.GitHubUser{ID: time.Now().UnixNano() % 1e12, Login: "octo",
		Emails: []ssotest.GitHubEmail{{Email: sh.email("bob"), Primary: true, Verified: true}},
		Orgs:   map[string]string{"acme": "active"}})
	if _, who := sh.whoami(sh.signInToken("github")); who != "bob" {
		t.Fatalf("github sign-in is %q", who)
	}
}

// Removing someone from their last project revokes their tokens; signing in again must not
// mint a fresh one.
func TestSSOSignInNeedsAMembership(t *testing.T) {
	sh := newSSOHarness(t, nil)
	sh.register(sh.bob, sh.email("bob"))
	sh.signInToken("test")
	if err := sh.store.RemoveMember(context.Background(), sh.project.ID, sh.bob.ID); err != nil {
		t.Fatal(err)
	}
	wantSSOError(t, sh.signIn(browser(), "test"), "not a member of any project")
}

func TestSSOAutoProvision(t *testing.T) {
	sh := newSSOHarness(t, func(o *SSOOptions, sh *ssoHarness) {
		corp, _ := sso.New(sso.Config{Name: "corp", Kind: sso.KindOIDC, Issuer: sh.idp.Issuer,
			ClientID: sh.idp.ClientID, ClientSecret: sh.idp.ClientSecret, Domains: []string{"example.com"}}, sso.Options{})
		o.Providers = []sso.Provider{corp}
		o.AutoProvisionRole = domain.RoleContributor
		o.DefaultProject = sh.org.Slug + "/" + sh.project.Slug
	})
	sh.idp.SetUser(ssotest.User{Subject: "new-" + sh.tag, Email: "New.Hire+x-" + sh.tag + "@example.com", EmailVerified: true})
	_, who := sh.whoami(sh.signInToken("corp"))
	if !strings.HasPrefix(who, "new.hire") {
		t.Fatalf("provisioned handle = %q", who)
	}
	p, err := sh.store.GetPrincipalByHandle(context.Background(), sh.org.ID, who)
	if err != nil {
		t.Fatal(err)
	}
	if role, _ := sh.store.RoleIn(context.Background(), sh.project.ID, p.ID); role != domain.RoleContributor {
		t.Fatalf("provisioned role = %s", role)
	}

	// The bounds are enforced before the server starts.
	restricted := sh.idpProvider(t, []string{"example.com"})
	open := sh.idpProvider(t, nil)
	for _, tc := range []struct {
		opts SSOOptions
		want string
	}{
		{SSOOptions{Providers: []sso.Provider{restricted}, PublicURL: "https://c.example", AutoProvisionRole: domain.RoleMaintainer, DefaultProject: "o/p"}, "contributor, reviewer or observer"},
		{SSOOptions{Providers: []sso.Provider{restricted}, PublicURL: "https://c.example", AutoProvisionRole: domain.RoleOrgAdmin, DefaultProject: "o/p"}, "contributor, reviewer or observer"},
		{SSOOptions{Providers: []sso.Provider{restricted}, PublicURL: "https://c.example", AutoProvisionRole: domain.RoleContributor}, "--sso-default-project"},
		{SSOOptions{Providers: []sso.Provider{open}, PublicURL: "https://c.example", AutoProvisionRole: domain.RoleObserver, DefaultProject: "o/p"}, "would admit every account"},
		{SSOOptions{Providers: []sso.Provider{open}, PublicURL: "http://c.example"}, "https"},
		{SSOOptions{Providers: []sso.Provider{open}}, "--public-url"},
	} {
		if err := ValidateSSOOptions(tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateSSOOptions(%+v) = %v, want %q", tc.opts, err, tc.want)
		}
	}
	if err := ValidateSSOOptions(SSOOptions{Providers: []sso.Provider{restricted}, PublicURL: "https://c.example",
		AutoProvisionRole: domain.RoleReviewer, DefaultProject: "o/p"}); err != nil {
		t.Errorf("a bounded configuration was refused: %v", err)
	}
}

func (sh *ssoHarness) idpProvider(t *testing.T, domains []string) sso.Provider {
	p, err := sso.New(sso.Config{Name: "x", Kind: sso.KindOIDC, Issuer: sh.idp.Issuer, ClientID: "c", ClientSecret: "s",
		Domains: domains}, sso.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The CLI's loopback flow, driven through sso.Login with the "browser" following redirects
// from the provider through conductord to the CLI's listener.
func TestSSOCLILoopbackSignIn(t *testing.T) {
	sh := newSSOHarness(t, nil)
	sh.register(sh.bob, sh.email("bob"))
	b := browser()
	res, err := sso.Login(context.Background(), client.New(sh.srv.URL, ""), sso.LoginOptions{
		Provider: "test", Out: &strings.Builder{}, Timeout: 10 * time.Second,
		Open: func(u string) {
			go func() {
				if resp, err := b.Get(u); err == nil {
					resp.Body.Close()
				}
			}()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Handle != "bob" || res.Token == "" {
		t.Fatalf("result = %+v", res)
	}
	if _, who := sh.whoami(res.Token); who != "bob" {
		t.Fatalf("CLI token is %q", who)
	}
}

// A ticket sent to the loopback address is useless to whoever catches it without the CLI's
// PKCE verifier.
func TestSSOCLITicketNeedsTheVerifier(t *testing.T) {
	sh := newSSOHarness(t, nil)
	sh.register(sh.bob, sh.email("bob"))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tickets := make(chan string, 1)
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tickets <- r.URL.Query().Get("ticket")
		}))
	}()
	t.Cleanup(func() { ln.Close() })

	verifier := sso.NewVerifier()
	code, out := sh.doJSONOn(sh.srv, "", http.MethodPost, "/v1/sso/test/start", map[string]any{
		"redirect_uri": "http://" + ln.Addr().String() + "/cb", "code_challenge": sso.Challenge(verifier),
		"code_challenge_method": "S256"})
	if code != http.StatusCreated {
		t.Fatalf("start = %d %v", code, out)
	}
	resp, err := browser().Get(out["authorization_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ticket := <-tickets
	if code, _ := sh.redeem(browser(), map[string]any{"ticket": ticket, "code_verifier": sso.NewVerifier()}); code != http.StatusUnauthorized {
		t.Fatalf("a wrong verifier redeemed the ticket: %d", code)
	}
	if code, _ := sh.redeem(browser(), map[string]any{"ticket": ticket}); code != http.StatusUnauthorized {
		t.Fatalf("no verifier redeemed the ticket: %d", code)
	}
	if code, out := sh.redeem(browser(), map[string]any{"ticket": ticket, "code_verifier": verifier}); code != http.StatusCreated {
		t.Fatalf("the right verifier could not redeem: %d %v", code, out)
	}
}

func TestSSOStartRefusesUnsafeRedirects(t *testing.T) {
	sh := newSSOHarness(t, nil)
	challenge := sso.Challenge(sso.NewVerifier())
	for _, redirect := range []string{
		"http://evil.example/cb", "https://127.0.0.1:4000/cb", "http://localhost:4000/cb",
		"http://127.0.0.1/cb", "http://user@127.0.0.1:4000/cb", "http://127.0.0.1:4000/cb?next=http://evil.example",
		"http://10.0.0.1:4000/cb", "",
	} {
		code, out := sh.doJSONOn(sh.srv, "", http.MethodPost, "/v1/sso/test/start", map[string]any{
			"redirect_uri": redirect, "code_challenge": challenge, "code_challenge_method": "S256"})
		if code != http.StatusBadRequest {
			t.Errorf("redirect_uri %q = %d %v, want 400", redirect, code, out)
		}
	}
	code, _ := sh.doJSONOn(sh.srv, "", http.MethodPost, "/v1/sso/test/start", map[string]any{
		"redirect_uri": "http://127.0.0.1:4000/cb", "code_challenge": challenge, "code_challenge_method": "plain"})
	if code != http.StatusBadRequest {
		t.Errorf("a plain PKCE method = %d, want 400", code)
	}
	if code, _ := sh.doJSONOn(sh.srv, "", http.MethodPost, "/v1/sso/nope/start", map[string]any{}); code != http.StatusNotFound {
		t.Errorf("an unknown provider = %d, want 404", code)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "/",
		"/tasks/T-1":                   "/tasks/T-1",
		"/tasks?status=open":           "/tasks?status=open",
		"/tasks#frag":                  "/tasks",
		"//evil.example":               "/",
		"/\\evil.example":              "/",
		"https://evil.example/x":       "/",
		"evil.example":                 "/",
		"/%2F%2Fevil.example":          "/%2F%2Fevil.example",
		"/\t/evil.example":             "/",
		"javascript:alert(1)":          "/",
		"/ok\r\nSet-Cookie: x=y":       "/",
		"///evil.example":              "/",
		"/" + strings.Repeat("a", 600): "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// Adding a second provider is done signed in, which proves control of the account; a
// second account at an issuer already linked is refused.
func TestSSOLinkAnotherProvider(t *testing.T) {
	sh := newSSOHarness(t, nil)
	sh.register(sh.bob, sh.email("bob"))
	bobSSO := sh.signInToken("test")

	link := func(token, provider string) error {
		b := browser()
		_, err := sso.Login(context.Background(), client.New(sh.srv.URL, token), sso.LoginOptions{
			Provider: provider, Link: true, Out: &strings.Builder{}, Timeout: 10 * time.Second,
			Open: func(u string) {
				go func() {
					if resp, err := b.Get(u); err == nil {
						resp.Body.Close()
					}
				}()
			},
		})
		return err
	}
	if err := link(bobSSO, "github"); err != nil {
		t.Fatalf("link github: %v", err)
	}
	ids, _ := sh.store.ListIdentities(context.Background(), sh.bob.ID)
	if len(ids) != 2 {
		t.Fatalf("identities after link = %+v", ids)
	}
	// GitHub now signs bob in directly.
	if _, who := sh.whoami(sh.signInToken("github")); who != "bob" {
		t.Fatalf("github sign-in after link is %q", who)
	}

	// Another account at the same issuer, linked by bob: refused.
	sh.idp.SetUser(ssotest.User{Subject: "second-" + sh.tag, Email: sh.email("other"), EmailVerified: true})
	if err := link(bobSSO, "test"); err == nil || !strings.Contains(err.Error(), "already has a different") {
		t.Fatalf("a second account at one issuer was linked: %v", err)
	}
	// Alice cannot link bob's GitHub account to herself.
	if err := link(sh.aliceTok, "github"); err == nil || !strings.Contains(err.Error(), "already linked to a different") {
		t.Fatalf("an identity linked to bob was relinked to alice: %v", err)
	}
	// A local sign-in session cannot link anything.
	local, _ := sh.store.CreateToken(context.Background(), sh.alice.ID, db.LocalTokenPrefix+"cli", time.Hour)
	if code, _ := sh.doJSONOn(sh.srv, local, http.MethodPost, "/v1/sso/test/link", map[string]any{}); code < 400 {
		t.Fatalf("a local sign-in token started a link: %d", code)
	}
}

func TestSSOAdministration(t *testing.T) {
	sh := newSSOHarness(t, nil)
	email := sh.email("bob")
	put := func(token, handle, email string) int {
		code, _ := sh.doJSONOn(sh.srv, token, http.MethodPut, sh.projectPath("/members/"+handle+"/email"), map[string]any{"email": email})
		return code
	}
	if code := put(sh.bobTok, "alice", email); code != http.StatusForbidden {
		t.Fatalf("a contributor set an email = %d", code)
	}
	if code := put(sh.aliceTok, "bob", "not-an-address"); code != http.StatusBadRequest {
		t.Fatalf("a malformed email = %d", code)
	}
	if code := put(sh.aliceTok, "bob", email); code != http.StatusOK {
		t.Fatalf("admin set email = %d", code)
	}
	bobSSO := sh.signInToken("test")

	// Carol is a contributor here and an administrator of another project alice does not
	// administer: alice must not be able to point carol's sign-in at a mailbox of her own.
	carol, _ := sh.member("carol", domain.RoleContributor)
	other := sh.secondProject("elsewhere")
	if err := sh.store.AddMember(context.Background(), other.ID, carol.ID, domain.RoleProjectAdmin); err != nil {
		t.Fatal(err)
	}
	if code := put(sh.aliceTok, "carol", sh.email("alice-controlled")); code != http.StatusForbidden {
		t.Fatalf("setting the email of an administrator elsewhere = %d, want 403", code)
	}

	// Status lists the caller's own identities.
	code, status := sh.doJSONOn(sh.srv, bobSSO, http.MethodGet, "/v1/sso/status", nil)
	if code != http.StatusOK || len(status["identities"].([]any)) != 1 || len(status["providers"].([]any)) != 2 {
		t.Fatalf("status = %d %v", code, status)
	}

	// Unlinking is an administrator's, and ends the sessions the identity opened.
	del := func(token string) (int, map[string]any) {
		return sh.doJSONOn(sh.srv, token, http.MethodDelete, sh.projectPath("/members/bob/identities/test"), nil)
	}
	if code, _ := del(sh.bobTok); code != http.StatusForbidden {
		t.Fatalf("a contributor unlinked = %d", code)
	}
	code, out := del(sh.aliceTok)
	if code != http.StatusOK || out["revoked_tokens"] != float64(1) {
		t.Fatalf("unlink = %d %v", code, out)
	}
	if code, _ := sh.whoami(bobSSO); code != http.StatusUnauthorized {
		t.Fatalf("the unlinked identity's token still works: %d", code)
	}
	if code, _ := sh.whoami(sh.bobTok); code != http.StatusOK {
		t.Fatalf("unlinking revoked bob's other tokens: %d", code)
	}
	if got := strings.Join(sh.auditActions(sh.bob.ID), ","); !strings.Contains(got, "member.email_set") || !strings.Contains(got, "sso.unlinked") {
		t.Fatalf("audit = %s", got)
	}
	// With the identity unlinked, the registered address links again.
	if _, who := sh.whoami(sh.signInToken("test")); who != "bob" {
		t.Fatalf("relink after unlink is %q", who)
	}
}

// The callback is throttled like any other failed authentication.
func TestSSOCallbackIsRateLimited(t *testing.T) {
	sh := newSSOHarness(t, nil)
	last := 0
	for i := 0; i < 12; i++ {
		last, _ = sh.doOn(sh.srv, "", http.MethodGet, "/v1/sso/test/callback?state=forged&code=x")
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after 12 forged callbacks = %d, want 429", last)
	}
}

func TestSSOProvidersArePublic(t *testing.T) {
	sh := newSSOHarness(t, nil)
	code, out := sh.doJSONOn(sh.srv, "", http.MethodGet, "/v1/sso/providers", nil)
	if code != http.StatusOK {
		t.Fatalf("providers = %d", code)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"label":"Test IdP"`) || strings.Contains(string(raw), "s3cret") ||
		strings.Contains(string(raw), sh.idp.ClientID) {
		t.Fatalf("providers = %s", raw)
	}
}
