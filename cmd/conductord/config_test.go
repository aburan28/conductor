package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/admin"
	"github.com/aburan28/conductor/internal/api"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conductor.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func entry(c *serveConfig, key string) api.ConfigEntry {
	for _, e := range c.admin.Config {
		if e.Key == key {
			return e
		}
	}
	return api.ConfigEntry{}
}

// A flag beats its environment variable, which beats the config file, which beats the
// default — and the effective configuration says which one each value came from.
func TestConfigFilePrecedence(t *testing.T) {
	clearEnv(t)
	t.Setenv("CONDUCTOR_DB", "postgres://u:hunter2@db.internal/conductor")
	t.Setenv("METRICS", "metrics-secret")
	path := writeConfig(t, `version: 1
server:
  addr: 127.0.0.1:9000
  security_mode: enhanced
database:
  url_env: CONDUCTOR_DB
retention:
  events_days: 10
  audit_days: 400
metrics:
  token_env: METRICS
sso:
  token_ttl: 4h
policy:
  require_sso: false
features:
  swarm: true
`)
	t.Setenv("CONDUCTOR_AUDIT_RETENTION_DAYS", "200")
	c, err := parseServeConfig([]string{"--config", path, "--security-mode", "local"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.addr != "127.0.0.1:9000" || c.dsn != "postgres://u:hunter2@db.internal/conductor" {
		t.Errorf("file values: addr %s dsn %s", c.addr, c.dsn)
	}
	if c.securityMode != "local" {
		t.Errorf("a flag lost to the file: security mode %s", c.securityMode)
	}
	if c.retention.Events != 10*24*time.Hour || c.retention.Audit != 200*24*time.Hour {
		t.Errorf("retention = %+v (events from the file, audit from the environment)", c.retention)
	}
	if c.sso.TokenTTL != 4*time.Hour || c.ops.MetricsToken != "metrics-secret" {
		t.Errorf("sso ttl %s, metrics token %q", c.sso.TokenTTL, c.ops.MetricsToken)
	}
	for key, want := range map[string]string{"addr": "file", "security-mode": "flag", "audit-retention-days": "env",
		"retention-days": "file", "tick": "default", "config": "flag"} {
		if got := entry(c, key).Source; got != want {
			t.Errorf("%s came from %q, want %q", key, got, want)
		}
	}
	dsn := entry(c, "dsn")
	if strings.Contains(dsn.Value, "hunter2") || !dsn.Secret || !strings.Contains(dsn.Value, "db.internal") {
		t.Errorf("dsn entry = %+v", dsn)
	}
	if m := entry(c, "metrics-token"); m.Value != "(set)" {
		t.Errorf("metrics token entry = %+v", m)
	}
	for _, e := range c.admin.Config {
		if strings.Contains(e.Value, "metrics-secret") || strings.Contains(e.Value, "hunter2") {
			t.Errorf("a secret reached the effective configuration: %+v", e)
		}
	}
	if !c.admin.Locks.Locked(admin.KeyRequireSSO) || !c.admin.Locks.Locked(admin.FeatureKey("swarm")) || c.admin.ConfigFile != path {
		t.Errorf("locks = %v, file %s", c.admin.Locks.Keys(), c.admin.ConfigFile)
	}

	// CONDUCTOR_CONFIG names the file when --config does not.
	t.Setenv("CONDUCTOR_CONFIG", path)
	if c, err = parseServeConfig(nil, io.Discard); err != nil || c.addr != "127.0.0.1:9000" {
		t.Errorf("CONDUCTOR_CONFIG: %v %+v", err, c)
	}
}

func TestConfigFileErrorsStopTheServer(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "version: 1\nserver:\n  adress: typo\n")
	if _, err := parseServeConfig([]string{"--dsn", "x", "--config", path}, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "adress") || !strings.Contains(err.Error(), path) {
		t.Errorf("a bad file = %v", err)
	}
	// A value the file sets is checked as strictly as the same flag.
	path = writeConfig(t, "version: 1\nretention:\n  usage_days: 7\n")
	if _, err := parseServeConfig([]string{"--dsn", "x", "--config", path}, io.Discard); err == nil {
		t.Error("usage_days 7 was accepted")
	}
	if _, err := parseServeConfig([]string{"--dsn", "x", "--config", "/nonexistent/conductor.yaml"}, io.Discard); err == nil {
		t.Error("a missing config file was accepted")
	}
}

func TestConfigFileProviders(t *testing.T) {
	clearEnv(t)
	t.Setenv("OKTA_SECRET", "s")
	path := writeConfig(t, `version: 1
server:
  public_url: https://conductor.acme.com
sso:
  providers:
    - name: okta
      issuer: https://acme.okta.com
      client_id: c
      client_secret_env: OKTA_SECRET
`)
	c, err := parseServeConfig([]string{"--dsn", "x", "--config", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.sso.Providers) != 1 || c.sso.Providers[0].Config().Name != "okta" || entry(c, "sso-provider.okta").Source != "file" {
		t.Fatalf("providers = %d, entry %+v", len(c.sso.Providers), entry(c, "sso-provider.okta"))
	}
	if strings.Contains(entry(c, "sso-provider.okta").Value, "=s ") {
		t.Error("the client secret reached the effective configuration")
	}
	// --sso-provider replaces the file's providers rather than adding to them.
	t.Setenv("CONDUCTOR_SSO_GH_CLIENT_SECRET", "g")
	c, err = parseServeConfig([]string{"--dsn", "x", "--config", path, "--sso-provider", "name=gh,type=github,client-id=x"}, io.Discard)
	if err != nil || len(c.sso.Providers) != 1 || c.sso.Providers[0].Config().Name != "gh" {
		t.Fatalf("flag providers = %v, %v", c, err)
	}
}

func TestConfigCheckCommand(t *testing.T) {
	clearEnv(t)
	good := writeConfig(t, "version: 1\nserver:\n  addr: 127.0.0.1:9001\npolicy:\n  require_sso: false\n")
	var out, errOut bytes.Buffer
	if code := configCommand([]string{"check", "--config", good}, &out, &errOut); code != 0 {
		t.Fatalf("check of a good file = %d\n%s%s", code, out.String(), errOut.String())
	}
	for _, want := range []string{"is valid", "127.0.0.1:9001", "file", "require_sso", "no database is configured"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("check output lacks %q:\n%s", want, out.String())
		}
	}
	bad := writeConfig(t, "version: 1\npolicy:\n  default_role: org_admin\n")
	out.Reset()
	errOut.Reset()
	if code := configCommand([]string{"check", "--config", bad}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "default_role") {
		t.Errorf("check of a bad file = %d\n%s", code, errOut.String())
	}
	if code := configCommand([]string{"lint"}, &out, &errOut); code != 2 {
		t.Errorf("unknown subcommand = %d", code)
	}
}

// The first principal of a new organization becomes its org_admin, so someone can reach the
// admin area; re-running bootstrap to recover a login never demotes them.
func TestBootstrapMakesTheFirstPrincipalOrgAdmin(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	org := fmt.Sprintf("boot-%d", time.Now().UnixNano())
	repo := t.TempDir()
	run := func(extra ...string) {
		t.Helper()
		args := append([]string{"--dsn", dsn, "--org", org, "--project", "app", "--principal", "ada", "--repo", repo, "--no-login"}, extra...)
		if err := bootstrap(args); err != nil {
			t.Fatal(err)
		}
	}
	role := func(handle string) domain.Role {
		t.Helper()
		ctx := context.Background()
		store, err := db.Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		o, _ := store.GetOrganizationBySlug(ctx, org)
		p, _ := store.GetProjectBySlug(ctx, o.ID, "app")
		pr, _ := store.GetPrincipalByHandle(ctx, o.ID, handle)
		r, _ := store.RoleIn(ctx, p.ID, pr.ID)
		return r
	}
	run()
	if r := role("ada"); r != domain.RoleOrgAdmin {
		t.Fatalf("first principal = %s, want org_admin", r)
	}
	run()
	if r := role("ada"); r != domain.RoleOrgAdmin {
		t.Errorf("re-running bootstrap demoted ada to %s", r)
	}
	run("--role", "contributor")
	if r := role("ada"); r != domain.RoleContributor {
		t.Errorf("--role contributor = %s", r)
	}
	if err := bootstrap([]string{"--dsn", dsn, "--org", org, "--project", "app", "--principal", "ben", "--repo", repo, "--no-login"}); err != nil {
		t.Fatal(err)
	}
	if r := role("ben"); r != domain.RoleProjectAdmin {
		t.Errorf("a later principal in an existing organization = %s, want project_admin", r)
	}
}
