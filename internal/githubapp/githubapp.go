// Package githubapp connects Conductor to GitHub as a GitHub App.
//
// A GitHub App is how a tool gets repository access without anyone's personal token: an
// organisation installs it on the repositories it chooses, grants exactly the permissions
// it asks for, and the app acts through short-lived installation tokens it mints itself.
// Conductor uses it to see pull requests as they open and to answer, on the pull request
// itself, the question it answers everywhere else: is anyone else already working on these
// files?
//
// Creating an app by hand is a dozen form fields. This package implements GitHub's App
// Manifest flow instead (https://docs.github.com/apps/sharing-github-apps/registering-a-github-app-from-a-manifest):
// conductord renders one page with one button, GitHub creates the app with Conductor's
// permissions already filled in, and redirects back with a code this package exchanges
// for the app's credentials. One click to create, one click to install.
//
// Everything here is the standard library: the RS256 JWT is signed with crypto/rsa, the
// webhook signature is crypto/hmac, the client is net/http. Same reason as the S3 client.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAPI is GitHub's REST endpoint; GitHub Enterprise Server uses <host>/api/v3.
const DefaultAPI = "https://api.github.com"

// DefaultWeb is where the manifest form is posted and apps are installed.
const DefaultWeb = "https://github.com"

// Credentials are what GitHub returns when the manifest is converted. They are a secret:
// the private key signs as the app, and the webhook secret authenticates deliveries.
type Credentials struct {
	AppID         int64  `json:"app_id"`
	Slug          string `json:"slug"`
	Name          string `json:"name,omitempty"`
	Owner         string `json:"owner,omitempty"`
	HTMLURL       string `json:"html_url,omitempty"`
	ClientID      string `json:"client_id,omitempty"`
	ClientSecret  string `json:"client_secret,omitempty"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
	PrivateKeyPEM string `json:"pem"`
	CreatedAt     string `json:"created_at,omitempty"`
}

// InstallURL is where an owner installs the app on their repositories.
func (c Credentials) InstallURL(web string) string {
	if web == "" {
		web = DefaultWeb
	}
	return strings.TrimRight(web, "/") + "/apps/" + c.Slug + "/installations/new"
}

// Validate checks that the credentials can actually sign.
func (c Credentials) Validate() error {
	if c.AppID == 0 {
		return errors.New("github app: no app id")
	}
	if _, err := ParsePrivateKey([]byte(c.PrivateKeyPEM)); err != nil {
		return fmt.Errorf("github app: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// DefaultPath is where conductord keeps the app's credentials: beside the CLI's own
// credentials, 0600 in a 0700 directory, never inside a repository checkout.
func DefaultPath() (string, error) {
	if v := os.Getenv("CONDUCTOR_GITHUB_APP_FILE"); v != "" {
		return v, nil
	}
	if v := os.Getenv("CONDUCTOR_STATE_DIR"); v != "" {
		return filepath.Join(v, "github-app.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "github-app.json"), nil
}

// Save writes credentials atomically with owner-only permissions.
func Save(path string, c Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".github-app-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// Load reads credentials from the file, then lets the environment override each field, so a
// production deployment can inject the key from a secret store without a file at all:
// CONDUCTOR_GITHUB_APP_ID, CONDUCTOR_GITHUB_APP_SLUG, CONDUCTOR_GITHUB_APP_PRIVATE_KEY (the
// PEM itself) or CONDUCTOR_GITHUB_APP_PRIVATE_KEY_FILE, CONDUCTOR_GITHUB_WEBHOOK_SECRET.
// It returns ok=false, with no error, when no app is configured.
//
// conductord keeps the app in its database so every replica serves the same one; the file
// is read only to import an app set up before that (see api.GitHub).
func Load(path string, getenv func(string) string) (Credentials, bool, error) {
	c, _, err := LoadFile(path)
	if err != nil {
		return c, false, err
	}
	return Overlay(c, getenv)
}

// LoadFile reads the credentials file alone. found is false, with no error, when there is no
// file.
func LoadFile(path string) (c Credentials, found bool, err error) {
	if path == "" {
		return c, false, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &c); err != nil {
			return c, false, fmt.Errorf("%s: %w", path, err)
		}
		return c, true, nil
	case os.IsNotExist(err):
		return c, false, nil
	default:
		return c, false, err
	}
}

// Overlay applies the environment overrides Load documents to c, and validates the result.
// ok is false, with no error, when neither c nor the environment configures an app.
func Overlay(c Credentials, getenv func(string) string) (Credentials, bool, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := getenv("CONDUCTOR_GITHUB_APP_ID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, false, fmt.Errorf("CONDUCTOR_GITHUB_APP_ID: %w", err)
		}
		c.AppID = id
	}
	if v := getenv("CONDUCTOR_GITHUB_APP_SLUG"); v != "" {
		c.Slug = v
	}
	if v := getenv("CONDUCTOR_GITHUB_APP_PRIVATE_KEY"); v != "" {
		c.PrivateKeyPEM = v
	} else if f := getenv("CONDUCTOR_GITHUB_APP_PRIVATE_KEY_FILE"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return c, false, err
		}
		c.PrivateKeyPEM = string(data)
	}
	if v := getenv("CONDUCTOR_GITHUB_WEBHOOK_SECRET"); v != "" {
		c.WebhookSecret = v
	}
	if c.AppID == 0 && c.PrivateKeyPEM == "" {
		return c, false, nil
	}
	return c, true, c.Validate()
}

// ---------------------------------------------------------------------------
// App authentication: an RS256 JWT
// ---------------------------------------------------------------------------

// ParsePrivateKey reads the PEM GitHub issues (PKCS#1) or a PKCS#8 re-encoding of it.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("private key is neither PKCS#1 nor PKCS#8 RSA")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rk, nil
}

// AppJWT signs the short-lived token GitHub accepts as the app itself. GitHub allows at
// most ten minutes; iat is backdated a minute for clock skew, as GitHub recommends.
func AppJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// ---------------------------------------------------------------------------
// Webhooks
// ---------------------------------------------------------------------------

// VerifySignature checks X-Hub-Signature-256 against the body. A missing secret refuses
// every delivery: an app that accepts unsigned webhooks accepts anyone's.
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Sign computes the X-Hub-Signature-256 value for a body; used by tests and by
// `conductor github test-webhook`.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// The manifest flow
// ---------------------------------------------------------------------------

// Permissions Conductor asks for, and why. Nothing here can push code or change settings:
// the app reads what a pull request changes and writes one check run about it, and on
// repositories whose issues are synced into tasks it comments on, labels, and closes those
// issues.
var Permissions = map[string]string{
	"metadata":      "read",  // required by every app
	"contents":      "read",  // the files a push or pull request changes
	"pull_requests": "read",  // pull request events and their file lists
	"checks":        "write", // the "Conductor" check run on each pull request
	"issues":        "write", // issue sync: read issues; comment, label, and close them
}

// Events Conductor subscribes to. Installation events are always delivered.
var Events = []string{"pull_request", "push", "issues"}

// Grants reports whether a permission set (an installation's or the app's) grants name at
// level or above: write implies read.
func Grants(perms map[string]string, name, level string) bool {
	switch perms[name] {
	case "write", "admin":
		return true
	case "read":
		return level == "read"
	}
	return false
}

// ManifestOptions shapes the app GitHub creates.
type ManifestOptions struct {
	Name    string // shown on GitHub; must be globally unique
	BaseURL string // where conductord is reached by the browser (redirects)
	// WebhookURL is where GitHub delivers events. It must be reachable from the internet;
	// a laptop-local conductord leaves it empty and polls instead.
	WebhookURL string
	Public     bool
}

// Manifest builds the JSON GitHub's /settings/apps/new form expects.
func Manifest(o ManifestOptions) ([]byte, error) {
	if o.Name == "" || o.BaseURL == "" {
		return nil, errors.New("github app manifest: name and base URL are required")
	}
	base := strings.TrimRight(o.BaseURL, "/")
	m := map[string]any{
		"name":                o.Name,
		"url":                 base,
		"redirect_url":        base + "/github/callback",
		"setup_url":           base + "/github/installed",
		"setup_on_update":     true,
		"public":              o.Public,
		"default_permissions": Permissions,
		"default_events":      Events,
		"description": "Conductor coordinates people and coding agents working the same repository. " +
			"It reads which files a pull request changes and reports, as a check run, whether anyone else is already working on them. " +
			"On repositories that opt in, it turns issues into tasks and notes on each issue when its task is claimed and done.",
	}
	if o.WebhookURL != "" {
		m["hook_attributes"] = map[string]any{"url": o.WebhookURL, "active": true}
	} else {
		// GitHub requires a hook URL in the manifest; an inactive one delivers nothing and
		// conductord polls instead. It can be activated later from the app's settings.
		m["hook_attributes"] = map[string]any{"url": base + "/github/webhook", "active": false}
	}
	return json.Marshal(m)
}

// ManifestFormURL is where the manifest form posts: a personal account, or an organisation
// that will own the app.
func ManifestFormURL(web, org, state string) string {
	if web == "" {
		web = DefaultWeb
	}
	web = strings.TrimRight(web, "/")
	path := "/settings/apps/new"
	if org != "" {
		path = "/organizations/" + url.PathEscape(org) + "/settings/apps/new"
	}
	return web + path + "?state=" + url.QueryEscape(state)
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client talks to the GitHub API as the app and as its installations.
type Client struct {
	API  string
	HTTP *http.Client
	Now  func() time.Time

	creds Credentials
	key   *rsa.PrivateKey

	mu     sync.Mutex
	tokens map[int64]installationToken
	// backoffUntil is set when GitHub answers with a rate limit: until then every call fails
	// at once instead of reaching GitHub, which is what GitHub asks of an integration that
	// hit a secondary limit (retrying early extends it).
	backoffUntil time.Time
}

type installationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// New builds a client for configured credentials. creds may be zero for a client that will
// only convert a manifest (the one call that needs no authentication).
func New(api string, creds Credentials) (*Client, error) {
	if api == "" {
		api = DefaultAPI
	}
	c := &Client{API: strings.TrimRight(api, "/"), HTTP: &http.Client{Timeout: 20 * time.Second},
		Now: time.Now, creds: creds, tokens: map[int64]installationToken{}}
	if creds.PrivateKeyPEM != "" {
		k, err := ParsePrivateKey([]byte(creds.PrivateKeyPEM))
		if err != nil {
			return nil, err
		}
		c.key = k
	}
	return c, nil
}

// Credentials returns the configured credentials.
func (c *Client) Credentials() Credentials { return c.creds }

// APIError is a non-2xx answer from GitHub.
type APIError struct {
	Status  int
	Message string
	// RetryAfter is how long GitHub asked the caller to wait, for a rate-limited answer.
	RetryAfter time.Duration
	limited    bool
}

func (e *APIError) Error() string { return fmt.Sprintf("github: %d %s", e.Status, e.Message) }

// NotFound reports a 404, which GitHub also returns for "not installed here".
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

// RateLimited reports a primary or secondary rate limit (a 429, or a 403 that says so), as
// opposed to a 403 for a missing permission.
func (e *APIError) RateLimited() bool { return e.Status == http.StatusTooManyRequests || e.limited }

// Forbidden reports a 403 that is not a rate limit: the token lacks a permission, as when an
// installation has not accepted one the app asks for.
func (e *APIError) Forbidden() bool { return e.Status == http.StatusForbidden && !e.limited }

// Rate limits. A secondary limit says to wait at least a minute when GitHub names no time;
// an hour bounds a reset time that is wrong or far off.
const (
	defaultBackoff = time.Minute
	maxBackoff     = time.Hour
)

// call is one request to GitHub. header carries conditional-request headers (If-None-Match).
type call struct {
	method, path, bearer string
	body, out            any
	header               http.Header
}

func (c *Client) do(ctx context.Context, method, path, bearer string, body, out any) error {
	_, _, err := c.send(ctx, call{method: method, path: path, bearer: bearer, body: body, out: out})
	return err
}

// send performs a call and returns the response status and headers. A 304 Not Modified
// answer to a conditional request is not an error, and leaves out untouched.
func (c *Client) send(ctx context.Context, k call) (int, http.Header, error) {
	c.mu.Lock()
	until := c.backoffUntil
	c.mu.Unlock()
	if now := c.Now(); now.Before(until) {
		return 0, nil, &APIError{Status: http.StatusTooManyRequests, limited: true, RetryAfter: until.Sub(now),
			Message: "rate limited by GitHub; not calling it again until " + until.UTC().Format(time.RFC3339)}
	}
	var rd io.Reader
	if k.body != nil {
		b, err := json.Marshal(k.body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, k.method, c.API+k.path, rd)
	if err != nil {
		return 0, nil, err
	}
	for name, v := range k.header {
		req.Header[name] = v
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "conductor")
	if k.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+k.bearer)
	}
	if k.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode == http.StatusNotModified {
		return resp.StatusCode, resp.Header, nil
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		apiErr := &APIError{Status: resp.StatusCode, Message: e.Message}
		if wait, limited := c.rateLimit(resp, e.Message); limited {
			apiErr.limited, apiErr.RetryAfter = true, wait
			c.mu.Lock()
			if t := c.Now().Add(wait); t.After(c.backoffUntil) {
				c.backoffUntil = t
			}
			c.mu.Unlock()
		}
		return resp.StatusCode, resp.Header, apiErr
	}
	if k.out != nil && len(data) > 0 {
		return resp.StatusCode, resp.Header, json.Unmarshal(data, k.out)
	}
	return resp.StatusCode, resp.Header, nil
}

// rateLimit reads whether a failed response is a rate limit and how long to wait, the way
// GitHub documents it: Retry-After when present, else the primary limit's reset time when
// none remains, else at least a minute for a secondary limit (a 403 whose message says so).
func (c *Client) rateLimit(resp *http.Response, message string) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	wait := time.Duration(0)
	limited := resp.StatusCode == http.StatusTooManyRequests ||
		strings.Contains(strings.ToLower(message), "rate limit")
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			wait, limited = time.Duration(secs)*time.Second, true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		limited = true
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && wait == 0 {
			wait = time.Unix(reset, 0).Sub(c.Now())
		}
	}
	if !limited {
		return 0, false
	}
	if wait < defaultBackoff {
		wait = defaultBackoff
	}
	if wait > maxBackoff {
		wait = maxBackoff
	}
	return wait, true
}

// ConvertManifest exchanges the code GitHub redirected back with for the new app's
// credentials. The code is single-use and expires an hour after the app is created.
func (c *Client) ConvertManifest(ctx context.Context, code string) (Credentials, error) {
	if code == "" || strings.ContainsAny(code, "/?#") {
		return Credentials{}, errors.New("github app: invalid manifest code")
	}
	var raw struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		Name          string `json:"name"`
		HTMLURL       string `json:"html_url"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		WebhookSecret string `json:"webhook_secret"`
		PEM           string `json:"pem"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := c.do(ctx, http.MethodPost, "/app-manifests/"+url.PathEscape(code)+"/conversions", "", nil, &raw); err != nil {
		return Credentials{}, err
	}
	creds := Credentials{
		AppID: raw.ID, Slug: raw.Slug, Name: raw.Name, Owner: raw.Owner.Login, HTMLURL: raw.HTMLURL,
		ClientID: raw.ClientID, ClientSecret: raw.ClientSecret, WebhookSecret: raw.WebhookSecret,
		PrivateKeyPEM: raw.PEM, CreatedAt: c.Now().UTC().Format(time.RFC3339),
	}
	return creds, creds.Validate()
}

func (c *Client) appJWT() (string, error) {
	if c.key == nil || c.creds.AppID == 0 {
		return "", errors.New("github app is not configured")
	}
	return AppJWT(c.creds.AppID, c.key, c.Now())
}

// App describes the app itself, which is also the cheapest way to prove the key works.
func (c *Client) App(ctx context.Context) (map[string]any, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, c.do(ctx, http.MethodGet, "/app", jwt, nil, &out)
}

// Installation is one place the app is installed.
type Installation struct {
	ID      int64  `json:"id"`
	Account string `json:"account"`
	Type    string `json:"target_type"`
	HTMLURL string `json:"html_url,omitempty"`
	// Permissions are what the installation has accepted, which lags what the app asks for
	// until an owner accepts a new permission on the installation's settings page.
	Permissions map[string]string `json:"permissions,omitempty"`
}

// Installations lists where the app is installed.
func (c *Client) Installations(ctx context.Context) ([]Installation, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID          int64             `json:"id"`
		HTMLURL     string            `json:"html_url"`
		Type        string            `json:"target_type"`
		Permissions map[string]string `json:"permissions"`
		Account     struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := c.do(ctx, http.MethodGet, "/app/installations?per_page=100", jwt, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Installation, 0, len(raw))
	for _, r := range raw {
		out = append(out, Installation{ID: r.ID, Account: r.Account.Login, Type: r.Type, HTMLURL: r.HTMLURL, Permissions: r.Permissions})
	}
	return out, nil
}

// RepoInstallation finds the installation that covers owner/repo.
func (c *Client) RepoInstallation(ctx context.Context, owner, repo string) (int64, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	base, err := repoPath(owner, repo)
	if err != nil {
		return 0, err
	}
	err = c.do(ctx, http.MethodGet, base+"/installation", jwt, nil, &out)
	return out.ID, err
}

// InstallationToken returns a token for an installation, minting one when the cached token
// is missing or within five minutes of expiry. Tokens last an hour.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	c.mu.Lock()
	t, ok := c.tokens[installationID]
	c.mu.Unlock()
	if ok && c.Now().Add(5*time.Minute).Before(t.ExpiresAt) {
		return t.Token, nil
	}
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
	var fresh installationToken
	if err := c.do(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", jwt, nil, &fresh); err != nil {
		return "", err
	}
	if fresh.Token == "" {
		return "", errors.New("github returned an empty installation token")
	}
	c.mu.Lock()
	c.tokens[installationID] = fresh
	c.mu.Unlock()
	return fresh.Token, nil
}

// FromFork reports whether the pull request's head lives in another repository. A fork's
// branch name is chosen by whoever opened it, so it proves nothing about which task it is.
func (pr PullRequest) FromFork() bool {
	return pr.Head.Repo.FullName == "" || !strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName)
}

// PullRequest is the subset of a pull request Conductor reads.
type PullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	Head    struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	// Merged and MergeCommitSHA say how a closed pull request ended: merged, or closed
	// without merging. Both are present on the webhook payload and the single-PR endpoint.
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	// MergedAt is set on every merged pull request, including in list responses, which do
	// not carry Merged. UpdatedAt orders the closed list the poller pages through.
	MergedAt  *time.Time `json:"merged_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// WasMerged reports whether a closed pull request ended in a merge, whichever of the two
// signals the response carried.
func (pr PullRequest) WasMerged() bool { return pr.Merged || pr.MergedAt != nil }

// ClosedPullRequests lists a repository's pull requests closed (merged or not) since a given
// time, most recently updated first. It pages until a pull request was last updated before
// since, or maxPages pages of 100 have been read, so a busy repository costs a bounded
// number of requests per poll.
func (c *Client) ClosedPullRequests(ctx context.Context, installationID int64, owner, repo string, since time.Time, maxPages int) ([]PullRequest, error) {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	base, err := repoPath(owner, repo)
	if err != nil {
		return nil, err
	}
	if maxPages <= 0 {
		maxPages = 1
	}
	var out []PullRequest
	for page := 1; page <= maxPages; page++ {
		var batch []PullRequest
		p := fmt.Sprintf("%s/pulls?state=closed&sort=updated&direction=desc&per_page=100&page=%d", base, page)
		if err := c.do(ctx, http.MethodGet, p, tok, nil, &batch); err != nil {
			return nil, err
		}
		for _, pr := range batch {
			if pr.UpdatedAt.Before(since) {
				return out, nil
			}
			out = append(out, pr)
		}
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

// GetPullRequest fetches one pull request, open or not. The poller uses it to learn how a
// pull request it saw open has ended once it drops off the open list.
func (c *Client) GetPullRequest(ctx context.Context, installationID int64, owner, repo string, number int) (PullRequest, error) {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return PullRequest{}, err
	}
	base, err := repoPath(owner, repo)
	if err != nil {
		return PullRequest{}, err
	}
	var out PullRequest
	err = c.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", base, number), tok, nil, &out)
	return out, err
}

// OpenPullRequests lists a repository's open pull requests, newest first (up to 100).
func (c *Client) OpenPullRequests(ctx context.Context, installationID int64, owner, repo string) ([]PullRequest, error) {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	base, err := repoPath(owner, repo)
	if err != nil {
		return nil, err
	}
	var out []PullRequest
	err = c.do(ctx, http.MethodGet, base+"/pulls?state=open&per_page=100&sort=updated&direction=desc", tok, nil, &out)
	return out, err
}

// maxPRFiles bounds how much of a huge pull request is read. GitHub itself stops listing
// at 3000 files.
const maxPRFiles = 3000

// PullRequestFiles lists the paths a pull request changes (renames contribute both names).
func (c *Client) PullRequestFiles(ctx context.Context, installationID int64, owner, repo string, number int) ([]string, error) {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	base, err := repoPath(owner, repo)
	if err != nil {
		return nil, err
	}
	var paths []string
	for page := 1; len(paths) < maxPRFiles; page++ {
		var batch []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		p := fmt.Sprintf("%s/pulls/%d/files?per_page=100&page=%d", base, number, page)
		if err := c.do(ctx, http.MethodGet, p, tok, nil, &batch); err != nil {
			return nil, err
		}
		for _, f := range batch {
			paths = append(paths, f.Filename)
			if f.PreviousFilename != "" {
				paths = append(paths, f.PreviousFilename)
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	return paths, nil
}

// CheckRun is the result Conductor posts on a pull request's head commit.
type CheckRun struct {
	HeadSHA    string
	Conclusion string // success, neutral, failure
	Title      string
	Summary    string
	Text       string
	DetailsURL string
	ExternalID string
}

// CheckName is the name the check run appears under on GitHub.
const CheckName = "Conductor"

// PostCheckRun creates a completed check run and returns its id, which UpdateCheckRun takes.
func (c *Client) PostCheckRun(ctx context.Context, installationID int64, owner, repo string, run CheckRun) (int64, error) {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return 0, err
	}
	body := c.checkRunBody(run)
	body["head_sha"] = run.HeadSHA
	base, err := repoPath(owner, repo)
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, base+"/check-runs", tok, body, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// UpdateCheckRun replaces the result of a check run PostCheckRun created. Updating rather
// than posting again keeps one Conductor check per commit however many times the result
// changes. A run that no longer exists answers with an *APIError whose NotFound is true.
func (c *Client) UpdateCheckRun(ctx context.Context, installationID int64, owner, repo string, id int64, run CheckRun) error {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return err
	}
	base, err := repoPath(owner, repo)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, base+"/check-runs/"+strconv.FormatInt(id, 10), tok, c.checkRunBody(run), nil)
}

func (c *Client) checkRunBody(run CheckRun) map[string]any {
	body := map[string]any{
		"name":         CheckName,
		"status":       "completed",
		"conclusion":   run.Conclusion,
		"completed_at": c.Now().UTC().Format(time.RFC3339),
		"output": map[string]any{
			"title":   truncate(run.Title, 250),
			"summary": truncate(run.Summary, 60000),
			"text":    truncate(run.Text, 60000),
		},
	}
	if run.DetailsURL != "" {
		body["details_url"] = run.DetailsURL
	}
	if run.ExternalID != "" {
		body["external_id"] = run.ExternalID
	}
	return body
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

var namePart = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// ValidRepo reports whether owner and repo are plausible GitHub names: the characters GitHub
// allows, and never "." or "..", which would walk a request path.
func ValidRepo(owner, repo string) bool {
	for _, p := range []string{owner, repo} {
		if !namePart.MatchString(p) || p == "." || p == ".." {
			return false
		}
	}
	return true
}

func repoPath(owner, repo string) (string, error) {
	if !ValidRepo(owner, repo) {
		return "", fmt.Errorf("github: %q/%q is not a repository name", owner, repo)
	}
	return "/repos/" + owner + "/" + repo, nil
}

// ParseRemote extracts owner and repository from a git remote URL in any of the forms git
// accepts for GitHub: https://github.com/o/r(.git), git@github.com:o/r(.git),
// ssh://git@github.com/o/r. host is matched loosely so Enterprise hosts work too.
func ParseRemote(remote string) (owner, repo string, ok bool) {
	r := strings.TrimSpace(remote)
	r = strings.TrimSuffix(r, "/")
	r = strings.TrimSuffix(r, ".git")
	switch {
	case strings.HasPrefix(r, "git@"):
		if i := strings.Index(r, ":"); i >= 0 {
			r = r[i+1:]
		}
	case strings.Contains(r, "://"):
		u, err := url.Parse(r)
		if err != nil {
			return "", "", false
		}
		r = strings.TrimPrefix(u.Path, "/")
	default:
		return "", "", false
	}
	parts := strings.Split(r, "/")
	if len(parts) < 2 || !ValidRepo(parts[len(parts)-2], parts[len(parts)-1]) {
		return "", "", false
	}
	return parts[len(parts)-2], parts[len(parts)-1], true
}

// Repository is one repository an installation can see.
type Repository struct {
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

// InstallationRepositories lists the repositories an installation was granted (up to 300).
func (c *Client) InstallationRepositories(ctx context.Context, installationID int64) ([]Repository, error) {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var out []Repository
	for page := 1; page <= 3; page++ {
		var batch struct {
			Repositories []struct {
				Name     string `json:"name"`
				FullName string `json:"full_name"`
				Private  bool   `json:"private"`
				Owner    struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page), tok, nil, &batch); err != nil {
			return nil, err
		}
		for _, r := range batch.Repositories {
			out = append(out, Repository{Owner: r.Owner.Login, Name: r.Name, FullName: r.FullName, Private: r.Private})
		}
		if len(batch.Repositories) < 100 {
			break
		}
	}
	return out, nil
}

// RepositoryInfo reads one repository through an installation: mostly, whether it is private.
func (c *Client) RepositoryInfo(ctx context.Context, installationID int64, owner, repo string) (Repository, error) {
	base, err := repoPath(owner, repo)
	if err != nil {
		return Repository{}, err
	}
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return Repository{}, err
	}
	var raw struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := c.do(ctx, http.MethodGet, base, tok, nil, &raw); err != nil {
		return Repository{}, err
	}
	return Repository{Owner: raw.Owner.Login, Name: raw.Name, FullName: raw.FullName, Private: raw.Private}, nil
}
