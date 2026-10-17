// Package client is the HTTP client for the Conductor control plane.
//
// The CLI and the MCP gateway both use it, which is what keeps DESIGN.md §7.2 honest: the
// MCP gateway is a translation layer over the same API a human hits, with no private path
// into the database and no logic of its own.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client talks to one control plane as one principal.
type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
}

// New builds a client. An empty endpoint falls back to the local default.
//
// When CONDUCTOR_CA_CERT names a PEM bundle, it is added to the trust roots. A team running
// its own certificate authority needs this, and it is a far better answer than an
// "ignore certificate errors" switch — the connection is still verified, just against a root
// you chose.
func New(endpoint, token string) *Client {
	if endpoint == "" {
		endpoint = "http://localhost:8080"
	}
	httpClient := &http.Client{Timeout: 60 * time.Second}

	if caPath := os.Getenv("CONDUCTOR_CA_CERT"); caPath != "" {
		if pool, err := caPoolFrom(caPath); err == nil {
			httpClient.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			}
		} else {
			// Warn rather than fail: the endpoint may not be TLS at all, in which case the
			// unusable CA file is irrelevant.
			fmt.Fprintf(os.Stderr, "conductor: ignoring CONDUCTOR_CA_CERT: %v\n", err)
		}
	}

	return &Client{
		Endpoint: strings.TrimRight(endpoint, "/"),
		Token:    token,
		HTTP:     httpClient,
	}
}

// caPoolFrom builds a trust pool from a PEM file, keeping the system roots so a private CA
// supplements rather than replaces public trust.
func caPoolFrom(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in %s", path)
	}
	return pool, nil
}

// APIError carries a structured failure from the control plane.
type APIError struct {
	Status  int             `json:"-"`
	Code    string          `json:"code"`
	Message string          `json:"error"`
	Body    json.RawMessage `json:"-"`
	// RetryAfter is set when the control plane asked us to wait (a 429 or 503 with
	// Retry-After). Callers that manage their own backoff can honour it; Do already does.
	RetryAfter time.Duration `json:"-"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s (%d %s)", e.Message, e.Status, e.Code)
	}
	return fmt.Sprintf("request failed: %d", e.Status)
}

// RateLimited reports a 429, or any error the control plane tagged rate_limited.
func (e *APIError) RateLimited() bool {
	return e != nil && (e.Status == http.StatusTooManyRequests || e.Code == "rate_limited")
}

// Transient reports a failure that is worth retrying: rate limits, gateway timeouts, and
// other 5xx answers that usually mean "try again shortly".
func (e *APIError) Transient() bool {
	if e == nil {
		return false
	}
	if e.RateLimited() {
		return true
	}
	switch e.Status {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, http.StatusRequestTimeout:
		return true
	}
	return false
}

// Blocked reports whether the error is a coordination refusal — a conflict or duplicate —
// rather than a fault. Callers present these to the user as guidance, not as breakage.
//
// Any 409 counts. Some refusals answer with the decision itself rather than an error envelope
// (a blocked start-work returns the outcome, the duplicates, and the advice), so those carry
// no error code at all — and treating them as faults would hide exactly the explanation the
// caller needs.
func (e *APIError) Blocked() bool {
	if e.Status == http.StatusConflict {
		return true
	}
	switch e.Code {
	case "scope_conflict", "duplicate", "already_claimed", "illegal_transition", "stale_fencing":
		return true
	}
	return false
}

// Retry budget for a single logical call. Four tries covers a brief blip or one rate-limit
// window without turning a hard failure into a long stall.
const maxAttempts = 4

// Do performs a request and decodes the response into out.
//
// Transient failures — transport errors, 429 (honouring Retry-After), 502/503/504, and 408 —
// are retried with exponential backoff. Permanent 4xx answers, including 404, are not: those
// are decisions, not blips, and retrying them would amplify load on a misconfigured client.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.doOnce(ctx, method, path, payload, out)
		if err == nil {
			return nil
		}
		last = err
		wait, ok := retryWait(err, attempt)
		if !ok || attempt == maxAttempts-1 {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return last
}

func (c *Client) doOnce(ctx context.Context, method, path string, payload []byte, out any) error {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.Endpoint+path, reader)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("contacting %s: %w", c.Endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		apiErr := &APIError{Status: resp.StatusCode, Body: body}
		// A conflict response carries the explanation (who holds what) in its body, so it
		// is decoded rather than discarded.
		_ = json.Unmarshal(body, apiErr)
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(body))
		}
		if wait, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			apiErr.RetryAfter = wait
		}
		if out != nil && resp.StatusCode == http.StatusConflict {
			_ = json.Unmarshal(body, out)
		}
		return apiErr
	}

	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

// retryWait reports whether err is worth another attempt and how long to wait first.
func retryWait(err error, attempt int) (time.Duration, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if !apiErr.Transient() {
			return 0, false
		}
		if apiErr.RetryAfter > 0 {
			return capWait(apiErr.RetryAfter), true
		}
	} else if err == nil {
		return 0, false
	}
	// Transport errors (connection reset, dial timeout) have no Retry-After; back off the
	// same way. Base 200ms doubles per attempt: 200ms, 400ms, 800ms.
	return capWait(200 * time.Millisecond << attempt), true
}

const maxRetryWait = 30 * time.Second

func capWait(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > maxRetryWait {
		return maxRetryWait
	}
	return d
}

// parseRetryAfter accepts either a delay-seconds integer or an HTTP-date, as RFC 7231 §7.1.3.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0, true
		}
		return d, true
	}
	return 0, false
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.Do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) Put(ctx context.Context, path string, body, out any) error {
	return c.Do(ctx, http.MethodPut, path, body, out)
}

func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodDelete, path, nil, out)
}

// Raw fetches a non-JSON endpoint, such as the Markdown task card.
func (c *Client) Raw(ctx context.Context, path string) ([]byte, error) {
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body, err := c.rawOnce(ctx, path)
		if err == nil {
			return body, nil
		}
		last = err
		wait, ok := retryWait(err, attempt)
		if !ok || attempt == maxAttempts-1 {
			return nil, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, last
}

func (c *Client) rawOnce(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contacting %s: %w", c.Endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		apiErr := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(body)), Body: body}
		_ = json.Unmarshal(body, apiErr)
		if wait, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			apiErr.RetryAfter = wait
		}
		return nil, apiErr
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

// Credentials is the on-disk login state.
type Credentials struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
	Project  string `json:"project,omitempty"`
	Handle   string `json:"handle,omitempty"`
}

// CredentialsPath is ~/.conductor/credentials.
func CredentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "credentials"), nil
}

// SaveCredentials writes the login file with owner-only permissions. The token is a bearer
// credential; a world-readable file would hand it to every process on the machine
// (DESIGN.md §25.1).
func SaveCredentials(c Credentials) error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// LoadCredentials reads the login file, with environment overrides.
//
// Environment wins over the file so a runner or CI job can be configured without writing a
// credential to disk at all.
func LoadCredentials() Credentials {
	var c Credentials
	if path, err := CredentialsPath(); err == nil {
		if body, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(body, &c)
		}
	}
	if v := os.Getenv("CONDUCTOR_ENDPOINT"); v != "" {
		c.Endpoint = v
	}
	if v := os.Getenv("CONDUCTOR_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("CONDUCTOR_PROJECT"); v != "" {
		c.Project = v
	}
	if c.Endpoint == "" {
		c.Endpoint = "http://localhost:8080"
	}
	return c
}

// FromEnvironment builds a client from saved credentials and environment overrides.
func FromEnvironment() (*Client, Credentials) {
	creds := LoadCredentials()
	return New(creds.Endpoint, creds.Token), creds
}

// Query builds a query string from non-empty pairs.
func Query(pairs ...string) string {
	values := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			values.Set(pairs[i], pairs[i+1])
		}
	}
	if len(values) == 0 {
		return ""
	}
	return "?" + values.Encode()
}

// LocalSession is what POST /v1/local/session returns.
type LocalSession struct {
	Token     string    `json:"token"`
	Handle    string    `json:"handle"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LocalSignIn asks a control plane on this machine for a token for its owner, without one.
// It works only from loopback, against a daemon in local security mode; anything else is an
// *APIError explaining why (code local_login_disabled, not_local, or no_owner).
func LocalSignIn(ctx context.Context, endpoint, clientName string) (LocalSession, error) {
	var out LocalSession
	err := New(endpoint, "").Post(ctx, "/v1/local/session", map[string]string{"client": clientName}, &out)
	if err == nil && out.Token == "" {
		err = errors.New("the control plane returned no token")
	}
	return out, err
}
