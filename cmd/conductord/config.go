package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/adamburan/conductor/internal/api"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/secretbox"
	"github.com/adamburan/conductor/internal/sso"
)

// The server config file (`--config`, CONDUCTOR_CONFIG). Precedence, highest first:
//
//  1. a command-line flag;
//  2. the flag's environment variable (CONDUCTOR_ADDR, DATABASE_URL, …);
//  3. the config file;
//  4. the built-in default.
//
// The file's values are applied through the flags themselves (flag.FlagSet.Set), so a value
// from the file is parsed and checked exactly as the same value typed on the command line.
// Its features, branding and policy sections have no flag: they lock those organization
// settings, and the admin area shows them as managed by the file.

// flagEnv names the environment variable each flag falls back to, where it has one.
var flagEnv = map[string]string{
	"config": "CONDUCTOR_CONFIG", "addr": "CONDUCTOR_ADDR", "dsn": "DATABASE_URL",
	"tls-cert": "CONDUCTOR_TLS_CERT", "tls-key": "CONDUCTOR_TLS_KEY", "public-url": "CONDUCTOR_PUBLIC_URL",
	"peer-ca": "CONDUCTOR_PEER_CA", "peer-cert": "CONDUCTOR_PEER_CERT", "peer-key": "CONDUCTOR_PEER_KEY",
	"peer-discover-dns": "CONDUCTOR_PEER_DISCOVER_DNS", "peer-dns-server": "CONDUCTOR_PEER_DNS_SERVER",
	"security-mode": "CONDUCTOR_SECURITY_MODE", "github-api": "CONDUCTOR_GITHUB_API", "github-web": "CONDUCTOR_GITHUB_WEB",
	"retention-days": "CONDUCTOR_RETENTION_DAYS", "audit-retention-days": "CONDUCTOR_AUDIT_RETENTION_DAYS",
	"metrics-token": "CONDUCTOR_METRICS_TOKEN", "secret-key-file": secretbox.EnvKeyFile,
	"sso-auto-provision": "CONDUCTOR_SSO_AUTO_PROVISION", "sso-default-project": "CONDUCTOR_SSO_DEFAULT_PROJECT",
	"notify-proxy": "CONDUCTOR_NOTIFY_PROXY",
}

// secretFlags are never shown, only whether they are set.
var secretFlags = map[string]bool{"metrics-token": true, "notify-proxy": true}

// applyConfigFile loads the config file, if one is named, and applies each of its values to
// the flag it corresponds to — unless that flag was given, or its environment variable is
// set. It returns the file and the flags it set.
func applyConfigFile(fs *flag.FlagSet, path string) (*config.ServerFile, map[string]bool, error) {
	fromFile := map[string]bool{}
	if path == "" {
		return nil, fromFile, nil
	}
	file, err := config.LoadServerFile(path, os.Getenv)
	if err != nil {
		return nil, nil, err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	values := file.FlagValues()
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if given[name] || (flagEnv[name] != "" && os.Getenv(flagEnv[name]) != "") {
			continue
		}
		if err := fs.Set(name, values[name]); err != nil {
			return nil, nil, fmt.Errorf("config file %s: %s: %w", path, name, err)
		}
		fromFile[name] = true
	}
	return file, fromFile, nil
}

// effectiveConfig describes every setting and where its value came from, with secrets
// redacted, for the admin area (GET /v1/admin/config) and `conductord config check`.
func effectiveConfig(fs *flag.FlagSet, fromFile map[string]bool, providers []sso.Provider, ssoSource string) []api.ConfigEntry {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var out []api.ConfigEntry
	fs.VisitAll(func(f *flag.Flag) {
		source := "default"
		switch {
		// fs.Set marks a flag as given, so the file's flags are told apart first.
		case fromFile[f.Name]:
			source = "file"
		case given[f.Name]:
			source = "flag"
		case flagEnv[f.Name] != "" && os.Getenv(flagEnv[f.Name]) != "":
			source = "env"
		}
		e := api.ConfigEntry{Key: f.Name, Value: f.Value.String(), Source: source}
		switch {
		case f.Name == "dsn":
			e.Value, e.Secret = redactURL(e.Value), true
		case f.Name == "sso-provider":
			return // listed per provider below
		case secretFlags[f.Name] && e.Value != "":
			e.Value, e.Secret = "(set)", true
		}
		out = append(out, e)
	})
	if os.Getenv(secretbox.EnvKey) != "" {
		out = append(out, api.ConfigEntry{Key: "secret-key", Value: "(set by " + secretbox.EnvKey + ")", Source: "env", Secret: true})
	}
	for _, p := range providers {
		cfg := p.Config()
		desc := []string{"type=" + string(cfg.Kind)}
		if cfg.Issuer != "" {
			desc = append(desc, "issuer="+cfg.Issuer)
		}
		desc = append(desc, "client-id="+cfg.ClientID, "client-secret=(set)")
		for _, d := range cfg.Domains {
			desc = append(desc, "domain="+d)
		}
		for _, o := range cfg.Orgs {
			desc = append(desc, "org="+o)
		}
		if cfg.GroupsClaim != "" {
			desc = append(desc, "groups-claim="+cfg.GroupsClaim)
		}
		for _, d := range cfg.TrustedEmailDomains {
			desc = append(desc, "trust-email-domain="+d)
		}
		out = append(out, api.ConfigEntry{Key: "sso-provider." + cfg.Name, Value: strings.Join(desc, " "), Source: ssoSource})
	}
	return out
}

// redactURL hides a connection string's password; anything unparseable is hidden whole.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "(set)"
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	q := u.Query()
	if q.Has("password") {
		q.Set("password", "xxxxx")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// configCommand is `conductord config check [--config FILE] [flags]`: it parses the file
// and the flags exactly as the server would, without opening the database, and prints the
// effective configuration or every problem found. It exits 0 when the server would start
// with this configuration, 1 when it would not, and 2 on a usage error.
func configCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "check" && args[0] != "-h" && args[0] != "--help") {
		fmt.Fprintln(stderr, "usage: conductord config check [--config FILE] [serve flags…]")
		return 2
	}
	if args[0] != "check" {
		fmt.Fprintln(stdout, "usage: conductord config check [--config FILE] [serve flags…]")
		return 0
	}
	c, err := parseServeConfigMode(args[1:], stderr, true)
	if err != nil {
		var usage usageError
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if errors.As(err, &usage) {
			return 2
		}
		fmt.Fprintln(stderr, "conductord: configuration is invalid:", err)
		return 1
	}
	if c.admin.ConfigFile != "" {
		fmt.Fprintf(stdout, "Configuration file %s is valid.\n\n", c.admin.ConfigFile)
	} else {
		fmt.Fprint(stdout, "No configuration file (--config or CONDUCTOR_CONFIG); flags and environment are valid.\n\n")
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SETTING\tVALUE\tFROM")
	for _, e := range c.admin.Config {
		if e.Source == "default" && e.Value == "" {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Key, e.Value, e.Source)
	}
	_ = tw.Flush()
	if locked := c.admin.Locks.Keys(); len(locked) > 0 {
		fmt.Fprintf(stdout, "\nLocked for every organization (read-only in the admin area):\n  %s\n", strings.Join(locked, "\n  "))
	}
	for _, w := range c.warnings {
		fmt.Fprintln(stdout, "\nwarning:", w)
	}
	return 0
}
