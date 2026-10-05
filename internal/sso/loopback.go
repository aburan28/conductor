package sso

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/client"
)

// The command line's half of a sign-in (RFC 8252, native apps).
//
// The identity provider redirects to conductord, never to the CLI: the redirect URI
// registered with the provider is one exact URL on the control plane, and the client secret
// lives there too. conductord then sends the browser on to a listener this process opened on
// 127.0.0.1 with a one-time ticket, and the CLI redeems the ticket for a token. The ticket
// alone is worthless: redeeming it takes the PKCE verifier whose challenge the CLI sent when
// it started, so another program on this machine that catches the redirect — or a web page
// that steers the browser at the listener with a ticket of its own — gets nothing.

// ProviderInfo is a configured provider as GET /v1/sso/providers lists it.
type ProviderInfo struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Type  Kind   `json:"type"`
}

// LoginOptions configures a command-line sign-in.
type LoginOptions struct {
	// Provider is the provider's name; empty picks the only one configured.
	Provider string
	// Link adds this provider's account to the caller's existing login instead of signing
	// in; the client must carry that login's token.
	Link bool
	// Open opens a URL in the user's browser. The URL is printed to Out either way.
	Open func(string)
	// Out receives the instructions; nil is stderr.
	Out io.Writer
	// Timeout bounds the wait for the browser; zero is five minutes.
	Timeout time.Duration
}

// LoginResult is a completed sign-in.
type LoginResult struct {
	Provider    string    `json:"provider"`
	Token       string    `json:"token"`
	Handle      string    `json:"handle"`
	PrincipalID string    `json:"principal_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	// Linked is set, and Token empty, when LoginOptions.Link added an identity.
	Linked bool `json:"linked"`
}

// Providers lists the providers a control plane offers.
func Providers(ctx context.Context, api *client.Client) ([]ProviderInfo, error) {
	var out struct {
		Providers []ProviderInfo `json:"providers"`
	}
	if err := api.Get(ctx, "/v1/sso/providers", &out); err != nil {
		return nil, err
	}
	return out.Providers, nil
}

// Login runs a sign-in through the browser and returns the token conductord issued.
func Login(ctx context.Context, api *client.Client, opts LoginOptions) (LoginResult, error) {
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	provider, err := pickProvider(ctx, api, opts.Provider)
	if err != nil {
		return LoginResult{}, err
	}

	// An IP literal, not "localhost": a name can resolve somewhere else, and conductord
	// accepts only loopback addresses as a place to send the ticket.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return LoginResult{}, fmt.Errorf("open a local port for the sign-in redirect: %w", err)
	}
	callbackPath := "/conductor/sso/" + Random(9)
	redirect := "http://" + ln.Addr().String() + callbackPath
	verifier := NewVerifier()

	type outcome struct {
		ticket, err string
	}
	results := make(chan outcome, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		o := outcome{ticket: q.Get("ticket")}
		if o.ticket == "" {
			o.err = firstNonEmpty(q.Get("error_description"), q.Get("error"), "the sign-in returned nothing")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		msg := "Signed in. You can close this window and return to the terminal."
		if o.err != "" {
			msg = "Sign-in failed: " + o.err
		}
		fmt.Fprintf(w, "<!doctype html><title>Conductor</title><p>%s</p>\n", html.EscapeString(msg))
		select {
		case results <- o:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	path := "/v1/sso/" + url.PathEscape(provider) + "/start"
	if opts.Link {
		path = "/v1/sso/" + url.PathEscape(provider) + "/link"
	}
	var started struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := api.Post(ctx, path, map[string]any{
		"redirect_uri": redirect, "code_challenge": Challenge(verifier), "code_challenge_method": "S256",
	}, &started); err != nil {
		return LoginResult{}, err
	}

	fmt.Fprintf(out, "Opening your browser to sign in with %s. If it does not open, visit:\n\n  %s\n\n",
		provider, started.AuthorizationURL)
	if opts.Open != nil {
		opts.Open(started.AuthorizationURL)
	}

	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var got outcome
	select {
	case got = <-results:
	case <-wait.Done():
		if errors.Is(wait.Err(), context.DeadlineExceeded) {
			return LoginResult{}, fmt.Errorf("gave up waiting for the browser after %s; run the command again", timeout)
		}
		return LoginResult{}, wait.Err()
	}
	if got.err != "" {
		return LoginResult{}, fmt.Errorf("sign-in failed: %s", got.err)
	}

	var res LoginResult
	if err := api.Post(ctx, "/v1/sso/redeem", map[string]any{
		"ticket": got.ticket, "code_verifier": verifier,
	}, &res); err != nil {
		return LoginResult{}, err
	}
	if !res.Linked && res.Token == "" {
		return LoginResult{}, errors.New("the control plane returned no token")
	}
	return res, nil
}

// pickProvider resolves the provider to use: the one named, or the only one there is.
func pickProvider(ctx context.Context, api *client.Client, name string) (string, error) {
	providers, err := Providers(ctx, api)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		if p.Name == name {
			return name, nil
		}
		names = append(names, p.Name)
	}
	switch {
	case len(providers) == 0:
		return "", errors.New("this control plane has no single sign-on provider configured; sign in with a token")
	case name != "":
		return "", fmt.Errorf("no single sign-on provider named %q here (configured: %s)", name, strings.Join(names, ", "))
	case len(providers) > 1:
		return "", fmt.Errorf("several single sign-on providers are configured; name one: %s", strings.Join(names, ", "))
	}
	return providers[0].Name, nil
}
