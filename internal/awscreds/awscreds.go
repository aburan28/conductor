// Package awscreds finds AWS credentials the way the AWS CLI does, without the AWS SDK: a
// static key pair (its secret in the macOS Keychain or a file), a named profile from
// ~/.aws/config and ~/.aws/credentials (static keys, credential_process, IAM Identity Center
// SSO, and role_arn assumed through STS), or the environment (the AWS_* variables, a web
// identity token, ECS container credentials, the EC2 instance role through IMDSv2).
//
// Every source returns backup.Credentials and is wrapped in a cache that refreshes temporary
// credentials shortly before they expire, so a long-running daemon can sign requests for
// days with an SSO session or an instance role.
//
// Network calls go to fixed AWS endpoints (STS, the SSO portal, IMDS, the ECS agent); Env
// lets tests point them at a local server. Secrets are never logged and never put in an
// error message.
package awscreds

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/aburan28/conductor/internal/backup"
)

// Env is everything a source reads from the outside world. The zero value is not usable;
// start from Default and override what a test needs.
type Env struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	HomeDir  func() (string, error)
	Now      func() time.Time
	HTTP     *http.Client
	// Command runs an external program with stdin and returns its stdout. Used for
	// credential_process and the macOS `security` tool.
	Command func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
	// Endpoints, overridable for tests. Empty means the real AWS endpoint.
	STSEndpoint  string // e.g. https://sts.us-east-1.amazonaws.com
	SSOEndpoint  string // e.g. https://portal.sso.us-east-1.amazonaws.com
	IMDSEndpoint string // http://169.254.169.254
	ECSEndpoint  string // http://169.254.170.2
	GOOS         string
}

// Default is the real environment.
func Default() Env {
	return Env{
		Getenv:   os.Getenv,
		ReadFile: os.ReadFile,
		HomeDir:  os.UserHomeDir,
		Now:      time.Now,
		HTTP:     &http.Client{Timeout: 10 * time.Second},
		Command:  runCommand,
		GOOS:     goos,
	}
}

func runCommand(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		if msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Provider is one way of obtaining credentials.
type Provider interface {
	Retrieve(ctx context.Context) (backup.Credentials, error)
}

// ProviderFunc adapts a function.
type ProviderFunc func(ctx context.Context) (backup.Credentials, error)

// Retrieve calls f.
func (f ProviderFunc) Retrieve(ctx context.Context) (backup.Credentials, error) { return f(ctx) }

// Static is a fixed key pair.
type Static struct {
	Creds backup.Credentials
}

// Retrieve returns the key pair.
func (s Static) Retrieve(context.Context) (backup.Credentials, error) {
	if s.Creds.AccessKey == "" || s.Creds.SecretKey == "" {
		return backup.Credentials{}, errors.New("the access key ID and secret are both required")
	}
	return s.Creds, nil
}

// refreshBefore is how long before expiry cached credentials are refreshed.
const refreshBefore = 5 * time.Minute

// Cached wraps a provider so it is called only when there are no credentials yet or the
// ones held are about to expire. It is safe for concurrent use.
type Cached struct {
	Provider Provider
	Now      func() time.Time

	mu    sync.Mutex
	creds backup.Credentials
	have  bool
}

// NewCached wraps p.
func NewCached(p Provider, now func() time.Time) *Cached {
	if now == nil {
		now = time.Now
	}
	return &Cached{Provider: p, Now: now}
}

// Retrieve returns cached credentials, refreshing them when needed.
func (c *Cached) Retrieve(ctx context.Context) (backup.Credentials, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && (c.creds.Expires.IsZero() || c.Now().Add(refreshBefore).Before(c.creds.Expires)) {
		return c.creds, nil
	}
	creds, err := c.Provider.Retrieve(ctx)
	if err != nil {
		return backup.Credentials{}, err
	}
	c.creds, c.have = creds, true
	return creds, nil
}

// Chain tries each provider in order and returns the first that yields credentials. An
// error that says a source is simply absent (errNotConfigured) moves on; any other error
// is remembered and reported if nothing succeeds, so a broken source is not silent.
type Chain []Provider

// errNotConfigured marks a source that does not apply here (no variable, no file, no IMDS).
var errNotConfigured = errors.New("not configured")

func notConfigured(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errNotConfigured, fmt.Sprintf(format, a...))
}

// Retrieve tries each source.
func (ch Chain) Retrieve(ctx context.Context) (backup.Credentials, error) {
	var problems []string
	for _, p := range ch {
		creds, err := p.Retrieve(ctx)
		if err == nil {
			return creds, nil
		}
		if !errors.Is(err, errNotConfigured) {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) > 0 {
		return backup.Credentials{}, errors.New(strings.Join(problems, "; "))
	}
	return backup.Credentials{}, errors.New("no AWS credentials found in the environment, a web identity token, " +
		"ECS container credentials, or the EC2 instance role")
}

// FromEnvironment reads AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN.
func FromEnvironment(env Env) Provider {
	return ProviderFunc(func(context.Context) (backup.Credentials, error) {
		ak, sk := env.Getenv("AWS_ACCESS_KEY_ID"), env.Getenv("AWS_SECRET_ACCESS_KEY")
		if ak == "" && sk == "" {
			return backup.Credentials{}, notConfigured("AWS_ACCESS_KEY_ID is not set")
		}
		if ak == "" || sk == "" {
			return backup.Credentials{}, errors.New("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must both be set")
		}
		return backup.Credentials{AccessKey: ak, SecretKey: sk, SessionToken: env.Getenv("AWS_SESSION_TOKEN"),
			Source: "environment variables"}, nil
	})
}

// Environment is the `environment` sign-in method: the AWS_* variables, then a web
// identity token, then ECS container credentials, then the EC2 instance role.
func Environment(env Env, region string) Provider {
	return NewCached(Chain{
		FromEnvironment(env),
		WebIdentityFromEnv(env, region),
		ECS(env),
		IMDS(env),
	}, env.Now)
}
