package config

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/admin"
)

const tenantID = "8f1c2b3a-4d5e-6f70-8192-a3b4c5d6e7f8"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestServerFileFullExample(t *testing.T) {
	dir := t.TempDir()
	var logo bytes.Buffer
	_ = png.Encode(&logo, image.NewRGBA(image.Rect(0, 0, 16, 16)))
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), logo.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "okta.secret"), []byte("okta-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `
version: 1
server:
  addr: 0.0.0.0:8443
  public_url: https://conductor.acme.com
  security_mode: enhanced
  tls_cert: tls/cert.pem
  tls_key: tls/key.pem
database:
  url_env: CONDUCTOR_DB
  statement_timeout: 20s
retention:
  events_days: 30
  audit_days: 365
metrics:
  token_env: METRICS_TOKEN
notifications:
  poll: 5s
sso:
  token_ttl: 8h
  providers:
    - name: okta
      issuer: https://acme.okta.com
      client_id: 0oa1
      client_secret_file: okta.secret
      domains: [acme.com]
      groups_claim: groups
    - name: entra
      issuer: https://login.microsoftonline.com/` + tenantID + `/v2.0
      client_id: app-id
      client_secret_env: ENTRA_SECRET
      trusted_email_domains: [acme.com]
features:
  queue: true
branding:
  display_name: Acme Engineering
  accent_color: "#1d4ed8"
  login_banner: Authorized use only.
  logo_file: logo.png
policy:
  require_sso: true
  human_token_max_ttl: 30d
  allowed_domains: [acme.com]
`
	path := filepath.Join(dir, "conductor.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := LoadServerFile(path, env(map[string]string{"CONDUCTOR_DB": "postgres://u:p@db/c", "METRICS_TOKEN": "m", "ENTRA_SECRET": "e"}))
	if err != nil {
		t.Fatal(err)
	}
	fv := sf.FlagValues()
	for flag, want := range map[string]string{
		"addr": "0.0.0.0:8443", "public-url": "https://conductor.acme.com", "security-mode": "enhanced",
		"tls-cert": dir + "/tls/cert.pem", "dsn": "postgres://u:p@db/c", "db-statement-timeout": "20s",
		"retention-days": "30", "audit-retention-days": "365", "metrics-token": "m", "notify-poll": "5s", "sso-token-ttl": "8h0m0s",
	} {
		if fv[flag] != want {
			t.Errorf("flag %s = %q, want %q", flag, fv[flag], want)
		}
	}
	if _, set := fv["behind-proxy"]; set {
		t.Error("an absent setting produced a flag value")
	}
	ps := sf.Providers()
	if len(ps) != 2 || ps[0].ClientSecret != "okta-secret" || ps[1].ClientSecret != "e" || ps[1].TrustedEmailDomains[0] != "acme.com" {
		t.Fatalf("providers = %+v", ps)
	}
	locks := sf.Locks()
	want := []string{admin.KeyAllowedDomains, admin.KeyAccentColor, admin.KeyDisplayName, admin.KeyLoginBanner, admin.KeyLogo,
		admin.FeatureKey("queue"), admin.KeyHumanTokenMaxTTL, admin.KeyRequireSSO}
	for _, k := range want {
		if !locks.Locked(k) {
			t.Errorf("%s is not locked (locked: %v)", k, locks.Keys())
		}
	}
	eff := locks.Effective(admin.Defaults())
	if !eff.RequireSSO || eff.HumanTokenCap() != 30*24*time.Hour || eff.Branding.DisplayName != "Acme Engineering" || locks.Logo == nil {
		t.Fatalf("effective = %+v", eff)
	}
}

func TestServerFileRefusals(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"no version":         {"server:\n  addr: x\n", "version: 1 is required"},
		"future version":     {"version: 2\n", "not supported"},
		"unknown key":        {"version: 1\nserver:\n  adress: x\n", "adress"},
		"wrong type":         {"version: 1\nretention:\n  events_days: lots\n", "line 3"},
		"inline secret":      {"version: 1\nsso:\n  providers:\n    - name: okta\n      issuer: https://acme.okta.com\n      client_id: c\n      client_secret: hunter2\n", "never written into the config file"},
		"inline dsn":         {"version: 1\ndatabase:\n  url: postgres://u:p@h/d\n", "never written into the config file"},
		"empty env":          {"version: 1\nmetrics:\n  token_env: NOPE\n", "NOPE is empty"},
		"missing secret":     {"version: 1\nsso:\n  providers:\n    - name: okta\n      issuer: https://acme.okta.com\n      client_id: c\n", "CONDUCTOR_SSO_OKTA_CLIENT_SECRET"},
		"bad mode":           {"version: 1\nserver:\n  security_mode: open\n", "security_mode"},
		"bad usage days":     {"version: 1\nretention:\n  usage_days: 7\n", "usage_days"},
		"bad policy":         {"version: 1\npolicy:\n  default_role: org_admin\n", "default_role"},
		"bad feature":        {"version: 1\nfeatures:\n  warp: true\n", "not a feature"},
		"bad color":          {"version: 1\nbranding:\n  accent_color: \"#ffff00\"\n", "contrast"},
		"features in policy": {"version: 1\npolicy:\n  features:\n    queue: true\n", "top-level features"},
		"entra common":       {"version: 1\nsso:\n  providers:\n    - name: entra\n      issuer: https://login.microsoftonline.com/common/v2.0\n      client_id: c\n      client_secret_env: S\n      trusted_email_domains: [acme.com]\n", "single-tenant"},
		"proxy credentials":  {"version: 1\nnotifications:\n  proxy: http://user:pw@proxy:3128\n", "credentials"},
		"bad duration":       {"version: 1\nsso:\n  token_ttl: soon\n", "not a duration"},
	}
	for name, c := range cases {
		_, err := ParseServerFile([]byte(c.body), env(map[string]string{"S": "s"}), "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// Every problem is reported, not just the first.
	_, err := ParseServerFile([]byte("version: 1\nserver:\n  security_mode: open\nretention:\n  usage_days: 7\n"), env(nil), "")
	if err == nil || !strings.Contains(err.Error(), "2 problems") {
		t.Errorf("several problems = %v", err)
	}
}

func TestServerFileEmptyIsVersionOnly(t *testing.T) {
	sf, err := ParseServerFile([]byte("version: 1\n"), env(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.FlagValues()) != 0 || len(sf.Locks().Keys()) != 0 {
		t.Errorf("an empty file set something: %v %v", sf.FlagValues(), sf.Locks().Keys())
	}
}
