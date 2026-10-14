package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/aburan28/conductor/internal/admin"
	"github.com/aburan28/conductor/internal/sso"
)

// ServerFile is conductord's own configuration file (`conductord --config conductor.yaml`,
// or CONDUCTOR_CONFIG). It is unrelated to the repository's .conductor/ policy above: it
// configures the server, not a project.
//
// Every setting it holds can also be given as a flag or an environment variable, and those
// win: a command-line flag over its environment variable over this file over the built-in
// default. The exceptions are the sections that have no flag — features, branding and
// policy — whose settings are applied to every organization on the server and LOCK them:
// the admin area shows such a setting as managed by the config file, read-only.
//
// Secrets are never written into the file. A database URL, a metrics token or a client
// secret is named by reference — an environment variable (`*_env`) or a file (`*_file`) —
// so the file can be committed, templated and reviewed like any other.
type ServerFile struct {
	// Version is 1. A file without it is refused, so a future format is never misread.
	Version       int               `yaml:"version"`
	Server        ServerSection     `yaml:"server"`
	Database      DatabaseSection   `yaml:"database"`
	Retention     RetentionSection  `yaml:"retention"`
	Metrics       MetricsSection    `yaml:"metrics"`
	Notifications NotifySection     `yaml:"notifications"`
	SSO           SSOSection        `yaml:"sso"`
	Features      map[string]bool   `yaml:"features"`
	Branding      BrandingSection   `yaml:"branding"`
	Policy        admin.Patch       `yaml:"policy"`
	Path          string            `yaml:"-"`
	logo          *admin.Logo       `yaml:"-"`
	providers     []sso.Config      `yaml:"-"`
	resolved      map[string]string `yaml:"-"`
}

// ServerSection is where and how conductord listens.
type ServerSection struct {
	Addr          string `yaml:"addr"`
	PublicURL     string `yaml:"public_url"`
	BehindProxy   *bool  `yaml:"behind_proxy"`
	Insecure      *bool  `yaml:"insecure"`
	SecurityMode  string `yaml:"security_mode"`
	TLSCert       string `yaml:"tls_cert"`
	TLSKey        string `yaml:"tls_key"`
	SecretKeyFile string `yaml:"secret_key_file"`
}

// DatabaseSection names the database by reference, and bounds statements.
type DatabaseSection struct {
	URLEnv           string          `yaml:"url_env"`
	URLFile          string          `yaml:"url_file"`
	URL              string          `yaml:"url"`
	StatementTimeout *admin.Duration `yaml:"statement_timeout"`
	LockTimeout      *admin.Duration `yaml:"lock_timeout"`
}

// RetentionSection bounds how long history is kept (docs/OPERATIONS.md, "Retention").
type RetentionSection struct {
	EventsDays            *int            `yaml:"events_days"`
	AuditDays             *int            `yaml:"audit_days"`
	OutboxUndeliveredDays *int            `yaml:"outbox_undelivered_days"`
	UsageDays             *int            `yaml:"usage_days"`
	IdempotencyTTL        *admin.Duration `yaml:"idempotency_ttl"`
}

// MetricsSection names the bearer token /metrics requires.
type MetricsSection struct {
	TokenEnv  string `yaml:"token_env"`
	TokenFile string `yaml:"token_file"`
	Token     string `yaml:"token"`
}

// NotifySection configures the notification relay.
type NotifySection struct {
	Poll                 *admin.Duration `yaml:"poll"`
	AllowPrivateNetworks *bool           `yaml:"allow_private_networks"`
	AllowHTTP            *bool           `yaml:"allow_http"`
	Proxy                string          `yaml:"proxy"`
}

// SSOSection configures single sign-on.
type SSOSection struct {
	TokenTTL       *admin.Duration `yaml:"token_ttl"`
	AutoProvision  string          `yaml:"auto_provision"`
	DefaultProject string          `yaml:"default_project"`
	Providers      []ProviderEntry `yaml:"providers"`
}

// ProviderEntry is one identity provider. The client secret comes from client_secret_env,
// client_secret_file, or by default CONDUCTOR_SSO_<NAME>_CLIENT_SECRET.
type ProviderEntry struct {
	Name                string   `yaml:"name"`
	Type                string   `yaml:"type"`
	Label               string   `yaml:"label"`
	Issuer              string   `yaml:"issuer"`
	ClientID            string   `yaml:"client_id"`
	ClientSecret        string   `yaml:"client_secret"`
	ClientSecretEnv     string   `yaml:"client_secret_env"`
	ClientSecretFile    string   `yaml:"client_secret_file"`
	Domains             []string `yaml:"domains"`
	Orgs                []string `yaml:"orgs"`
	APIURL              string   `yaml:"api_url"`
	WebURL              string   `yaml:"web_url"`
	GroupsClaim         string   `yaml:"groups_claim"`
	TrustedEmailDomains []string `yaml:"trusted_email_domains"`
}

// BrandingSection locks the organization's branding; logo_file is a PNG, JPEG or GIF.
type BrandingSection struct {
	DisplayName *string `yaml:"display_name"`
	AccentColor *string `yaml:"accent_color"`
	LoginBanner *string `yaml:"login_banner"`
	LogoFile    string  `yaml:"logo_file"`
}

// maxServerFile bounds the file; a configuration is a few kilobytes.
const maxServerFile = 1 << 20

// LoadServerFile reads, parses and checks a server config file, resolving every secret
// reference through getenv and the filesystem. Any problem — an unknown key, a wrong type,
// a secret written inline, a reference that resolves to nothing, a policy the admin
// package refuses — is an error naming the setting, and every one found is reported.
func LoadServerFile(path string, getenv func(string) string) (*ServerFile, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxServerFile+1))
	if err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	if len(body) > maxServerFile {
		return nil, fmt.Errorf("config file %s is larger than %d bytes", path, maxServerFile)
	}
	sf, err := ParseServerFile(body, getenv, baseDir(path))
	if err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	sf.Path = path
	return sf, nil
}

func baseDir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i+1]
	}
	return ""
}

// ParseServerFile parses a config file's bytes. Relative file references are resolved
// against dir.
func ParseServerFile(body []byte, getenv func(string) string, dir string) (*ServerFile, error) {
	var sf ServerFile
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&sf); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	sf.resolved = map[string]string{}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	path := func(p string) string {
		if p != "" && !strings.HasPrefix(p, "/") {
			return dir + p
		}
		return p
	}
	// secret resolves a reference: exactly one of the env and file forms, never inline.
	secret := func(key, inline, env, file string, required bool) string {
		if inline != "" {
			add("%s: a secret is never written into the config file; use %s_env (an environment variable) or %s_file", key, key, key)
			return ""
		}
		switch {
		case env != "" && file != "":
			add("%s: set %s_env or %s_file, not both", key, key, key)
		case env != "":
			v := getenv(env)
			if v == "" {
				add("%s_env: the environment variable %s is empty", key, env)
			}
			return v
		case file != "":
			b, err := os.ReadFile(path(file))
			if err != nil {
				add("%s_file: %v", key, err)
				return ""
			}
			v := strings.TrimSpace(string(b))
			if v == "" {
				add("%s_file: %s is empty", key, file)
			}
			return v
		case required:
			add("%s is required (%s_env or %s_file)", key, key, key)
		}
		return ""
	}

	switch sf.Version {
	case 1:
	case 0:
		add("version: 1 is required, so a later format is never misread as this one")
	default:
		add("version: %d is not supported by this conductord (it reads version 1)", sf.Version)
	}

	s := sf.Server
	switch s.SecurityMode {
	case "", "local", "enhanced":
	default:
		add("server.security_mode must be local or enhanced, not %q", s.SecurityMode)
	}
	if s.PublicURL != "" {
		if u, err := url.Parse(s.PublicURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			add("server.public_url %q is not an http(s) URL", s.PublicURL)
		}
	}
	if (s.TLSCert == "") != (s.TLSKey == "") {
		add("server.tls_cert and server.tls_key go together")
	}
	sf.Server.TLSCert, sf.Server.TLSKey, sf.Server.SecretKeyFile = path(s.TLSCert), path(s.TLSKey), path(s.SecretKeyFile)

	if dsn := secret("database.url", sf.Database.URL, sf.Database.URLEnv, sf.Database.URLFile, false); dsn != "" {
		sf.resolved["dsn"] = dsn
	}
	if tok := secret("metrics.token", sf.Metrics.Token, sf.Metrics.TokenEnv, sf.Metrics.TokenFile, false); tok != "" {
		sf.resolved["metrics-token"] = tok
	}
	r := sf.Retention
	for key, v := range map[string]*int{"retention.events_days": r.EventsDays, "retention.audit_days": r.AuditDays,
		"retention.outbox_undelivered_days": r.OutboxUndeliveredDays, "retention.usage_days": r.UsageDays} {
		if v != nil && *v < 0 {
			add("%s must not be negative", key)
		}
	}
	if r.UsageDays != nil && *r.UsageDays > 0 && *r.UsageDays < 31 {
		add("retention.usage_days must be 0 or at least 31: budgets total the last 30 days of usage")
	}
	if p := sf.Notifications.Proxy; p != "" {
		if u, err := url.Parse(p); err != nil || u.Host == "" {
			add("notifications.proxy %q is not a URL", p)
		} else if u.User != nil {
			add("notifications.proxy: credentials in the proxy URL are a secret written into the file; pass --notify-proxy or CONDUCTOR_NOTIFY_PROXY instead")
		}
	}

	switch sf.SSO.AutoProvision {
	case "", "contributor", "reviewer", "observer":
	default:
		add("sso.auto_provision must be contributor, reviewer or observer, not %q", sf.SSO.AutoProvision)
	}
	seen := map[string]bool{}
	for i, p := range sf.SSO.Providers {
		key := fmt.Sprintf("sso.providers[%d]", i)
		if p.Name != "" {
			key = "sso.providers." + p.Name
		}
		cfg := sso.Config{Name: p.Name, Label: p.Label, Kind: sso.Kind(strings.ToLower(p.Type)), Issuer: p.Issuer,
			ClientID: p.ClientID, Domains: lower(p.Domains), Orgs: p.Orgs, APIURL: p.APIURL, WebURL: p.WebURL,
			GroupsClaim: p.GroupsClaim, TrustedEmailDomains: lower(p.TrustedEmailDomains)}
		if cfg.Kind == "" {
			cfg.Kind = sso.KindOIDC
			if p.Name == "github" && p.Issuer == "" {
				cfg.Kind = sso.KindGitHub
			}
		}
		env := p.ClientSecretEnv
		if env == "" && p.ClientSecretFile == "" && p.ClientSecret == "" {
			env = sso.SecretEnv(p.Name)
		}
		cfg.ClientSecret = secret(key+".client_secret", p.ClientSecret, env, p.ClientSecretFile, true)
		if cfg.ClientSecret == "" {
			continue
		}
		if err := cfg.Validate(); err != nil {
			add("%s: %v", key, err)
			continue
		}
		if seen[cfg.Name] {
			add("%s is configured twice", key)
		}
		seen[cfg.Name] = true
		sf.providers = append(sf.providers, cfg)
	}

	if sf.Policy.Features != nil || sf.Policy.Branding != nil {
		add("policy: put feature flags under the top-level features: section and branding under branding:")
	}
	locks := sf.Locks()
	if lf := sf.Branding.LogoFile; lf != "" {
		data, err := os.ReadFile(path(lf))
		if err != nil {
			add("branding.logo_file: %v", err)
		} else if logo, err := admin.CheckLogo(data, ""); err != nil {
			add("branding.logo_file: %v", err)
		} else {
			sf.logo = &logo
			locks.Logo = &logo
		}
	}
	// The locked settings must make sense on their own; whether they agree with what an
	// organization stored is checked whenever that organization's policy is read.
	if err := locks.Effective(admin.Defaults()).Validate(); err != nil {
		var v *admin.ValidationError
		if errors.As(err, &v) {
			for _, p := range v.Problems {
				// default_project and group rules legitimately depend on the stored policy.
				if !strings.Contains(p, "default_project") {
					add("policy: %s", p)
				}
			}
		}
	}
	if len(problems) > 0 {
		return nil, &FileError{Problems: problems}
	}
	return &sf, nil
}

func lower(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
	}
	return out
}

// FileError lists every problem found in a config file.
type FileError struct{ Problems []string }

func (e *FileError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d problems:\n  - %s", len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

// Locks are the organization settings the file decides: features, branding, policy.
func (sf *ServerFile) Locks() admin.Locks {
	p := sf.Policy
	p.Features, p.Branding = nil, nil
	if len(sf.Features) > 0 {
		p.Features = map[string]bool{}
		for k, v := range sf.Features {
			p.Features[k] = v
		}
	}
	b := sf.Branding
	if b.DisplayName != nil || b.AccentColor != nil || b.LoginBanner != nil {
		p.Branding = &admin.BrandingPatch{DisplayName: b.DisplayName, AccentColor: b.AccentColor, LoginBanner: b.LoginBanner}
	}
	return admin.Locks{Patch: p, Logo: sf.logo}
}

// Providers returns the identity providers the file configures, secrets resolved.
func (sf *ServerFile) Providers() []sso.Config { return sf.providers }

// FlagValues maps conductord flag names to the values the file sets for them, in the
// flag's own syntax. conductord applies each one only when neither the flag nor its
// environment variable was given.
func (sf *ServerFile) FlagValues() map[string]string {
	out := map[string]string{}
	str := func(flag, v string) {
		if v != "" {
			out[flag] = v
		}
	}
	boolean := func(flag string, v *bool) {
		if v != nil {
			out[flag] = strconv.FormatBool(*v)
		}
	}
	dur := func(flag string, v *admin.Duration) {
		if v != nil {
			out[flag] = v.Std().String()
		}
	}
	num := func(flag string, v *int) {
		if v != nil {
			out[flag] = strconv.Itoa(*v)
		}
	}
	s := sf.Server
	str("addr", s.Addr)
	str("public-url", s.PublicURL)
	boolean("behind-proxy", s.BehindProxy)
	boolean("insecure", s.Insecure)
	str("security-mode", s.SecurityMode)
	str("tls-cert", s.TLSCert)
	str("tls-key", s.TLSKey)
	str("secret-key-file", s.SecretKeyFile)
	str("dsn", sf.resolved["dsn"])
	dur("db-statement-timeout", sf.Database.StatementTimeout)
	dur("db-lock-timeout", sf.Database.LockTimeout)
	num("retention-days", sf.Retention.EventsDays)
	num("audit-retention-days", sf.Retention.AuditDays)
	num("outbox-undelivered-days", sf.Retention.OutboxUndeliveredDays)
	num("usage-retention-days", sf.Retention.UsageDays)
	dur("idempotency-ttl", sf.Retention.IdempotencyTTL)
	str("metrics-token", sf.resolved["metrics-token"])
	dur("notify-poll", sf.Notifications.Poll)
	boolean("notify-allow-private-networks", sf.Notifications.AllowPrivateNetworks)
	boolean("notify-allow-http", sf.Notifications.AllowHTTP)
	str("notify-proxy", sf.Notifications.Proxy)
	dur("sso-token-ttl", sf.SSO.TokenTTL)
	str("sso-auto-provision", sf.SSO.AutoProvision)
	str("sso-default-project", sf.SSO.DefaultProject)
	return out
}
