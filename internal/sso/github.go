package sso

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// GitHub's OAuth web flow. GitHub is not an OpenID Connect provider for user sign-in, so
// there is no ID token to verify: the identity is whatever GitHub's API says the access
// token belongs to, fetched over TLS straight from GitHub with the token this server just
// redeemed. The account is named by its numeric id, which never changes; a login can be
// renamed and then claimed by someone else.

type githubProvider struct {
	cfg  Config
	opts Options
}

func newGitHub(cfg Config, opts Options) *githubProvider {
	return &githubProvider{cfg: cfg, opts: opts}
}

func (p *githubProvider) Config() Config { return p.cfg }

// issuer is what identities from this provider are recorded under: the GitHub instance's
// web URL, so github.com and an Enterprise Server never share an id space.
func (p *githubProvider) issuer() string { return p.cfg.WebURL }

func (p *githubProvider) scopes() string {
	scopes := "read:user user:email"
	if len(p.cfg.Orgs) > 0 {
		scopes += " read:org"
	}
	return scopes
}

func (p *githubProvider) AuthCodeURL(_ context.Context, req AuthRequest) (string, error) {
	q := url.Values{
		"client_id":    {p.cfg.ClientID},
		"redirect_uri": {req.RedirectURI},
		"scope":        {p.scopes()},
		"state":        {req.State},
		// GitHub accepts PKCE; where it does not, the parameters are ignored and the state
		// still binds the flow.
		"code_challenge":        {req.Challenge},
		"code_challenge_method": {"S256"},
		// Signing in must not be a way to create a GitHub account mid-flow.
		"allow_signup": {"false"},
	}
	return p.cfg.WebURL + "/login/oauth/authorize?" + q.Encode(), nil
}

func (p *githubProvider) Exchange(ctx context.Context, code, verifier, _ string, redirectURI string) (Identity, error) {
	form := url.Values{
		"client_id":     {p.cfg.ClientID},
		"client_secret": {p.cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.WebURL+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, fail(CodeUpstream, "GitHub is misconfigured", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := doJSON(p.opts.HTTPClient, req, &tok); err != nil {
		return Identity{}, fail(CodeUpstream, "GitHub refused the sign-in", err)
	}
	if tok.AccessToken == "" {
		// GitHub answers a bad code with 200 and an error field.
		return Identity{}, fail(CodeUpstream, "GitHub refused the sign-in", errors.New(tok.Error))
	}

	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := getJSON(ctx, p.opts.HTTPClient, p.cfg.APIURL+"/user", tok.AccessToken, &user); err != nil {
		return Identity{}, fail(CodeUpstream, "GitHub did not say who signed in", err)
	}
	if user.ID == 0 {
		return Identity{}, fail(CodeUpstream, "GitHub did not say who signed in", errors.New("no user id"))
	}

	// The profile's public email is whatever the user typed; /user/emails says which
	// addresses GitHub has verified. Only the verified primary is used.
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := getJSON(ctx, p.opts.HTTPClient, p.cfg.APIURL+"/user/emails", tok.AccessToken, &emails); err != nil {
		return Identity{}, fail(CodeUpstream, "GitHub did not list the account's email addresses", err)
	}
	email := ""
	for _, e := range emails {
		if e.Primary && e.Verified {
			email = strings.ToLower(strings.TrimSpace(e.Email))
		}
	}
	if email == "" {
		return Identity{}, fail(CodeEmailUnverified,
			"this GitHub account has no verified primary email address", nil)
	}
	if err := p.cfg.checkDomain(email); err != nil {
		return Identity{}, err
	}
	if err := p.checkOrgs(ctx, tok.AccessToken); err != nil {
		return Identity{}, err
	}
	return Identity{
		Provider: p.cfg.Name, Issuer: p.issuer(), Subject: strconv.FormatInt(user.ID, 10),
		Email: email, Name: user.Name, Username: user.Login,
	}, nil
}

// checkOrgs admits an active member of any allowed organization. A pending invitation is not
// membership, and an organization that has not approved this OAuth App answers as if the
// user were not a member at all — the message says so, since that is a setup problem.
func (p *githubProvider) checkOrgs(ctx context.Context, accessToken string) error {
	if len(p.cfg.Orgs) == 0 {
		return nil
	}
	for _, org := range p.cfg.Orgs {
		var m struct {
			State string `json:"state"`
		}
		err := getJSON(ctx, p.opts.HTTPClient,
			p.cfg.APIURL+"/user/memberships/orgs/"+url.PathEscape(org), accessToken, &m)
		var status *statusError
		switch {
		case err == nil && m.State == "active":
			return nil
		case err == nil, errors.As(err, &status) && (status.Status == http.StatusNotFound || status.Status == http.StatusForbidden):
			continue
		default:
			return fail(CodeUpstream, "GitHub organization membership could not be checked", err)
		}
	}
	return fail(CodeOrgNotAllowed, fmt.Sprintf(
		"this GitHub account is not an active member of %s (if it is, an organization owner may need to approve this OAuth App)",
		strings.Join(p.cfg.Orgs, " or ")), nil)
}
