package main

import (
	"io"
	"strings"
	"testing"
)

func TestNATForcesEnhancedAuthentication(t *testing.T) {
	clearEnv(t)
	for _, mode := range []string{"upnp", "tailscale"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"--dsn", "postgres://localhost/conductor", "--nat-mode", mode}
			if mode == "upnp" {
				args = append(args, "--addr", "0.0.0.0:8443", "--tls-cert", "cert.pem", "--tls-key", "key.pem")
			}
			cfg, err := parseServeConfig(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.securityMode != "enhanced" {
				t.Fatalf("NAT must force token authentication, got %q", cfg.securityMode)
			}
		})
	}
}

func TestUnsafeNATConfigurationsFailBeforeStartup(t *testing.T) {
	clearEnv(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"local authentication through proxy", []string{"--nat-mode", "tailscale", "--security-mode", "local"}},
		{"plaintext router mapping", []string{"--nat-mode", "upnp", "--addr", "0.0.0.0:8443", "--insecure"}},
		{"unknown NAT backend", []string{"--nat-mode", "unknown"}},
		{"unknown database backend", []string{"--database", "unknown"}},
		{"unverified RDS connection", []string{"--database", "rds"}},
		{"plaintext advertised proxy", []string{"--nat-mode", "tailscale", "--public-url", "http://node.example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--dsn", "postgres://localhost/conductor?sslmode=disable"}, tc.args...)
			if _, err := parseServeConfig(args, io.Discard); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestConnectivityEnvironmentOverridesFile(t *testing.T) {
	clearEnv(t)
	t.Setenv("CONDUCTOR_DATABASE_MODE", "external")
	t.Setenv("CONDUCTOR_NAT_MODE", "off")
	path := writeConfig(t, `version: 1
database:
  mode: rds
server:
  addr: 127.0.0.1:8080
nat:
  mode: tailscale
  https_port: 8443
`)
	cfg, err := parseServeConfig([]string{"--config", path, "--dsn", "postgres://localhost/conductor?sslmode=disable"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.databaseMode != "external" || cfg.nat.Mode != "off" || cfg.nat.HTTPSPort != 8443 {
		t.Fatalf("incorrect merged configuration: database=%s NAT=%+v", cfg.databaseMode, cfg.nat)
	}
	for _, e := range cfg.admin.Config {
		if e.Key == "database" && e.Source != "env" {
			t.Fatalf("database source = %s", e.Source)
		}
	}
}

func TestRDSConfigCheckReportsMissingDatabase(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "")
	cfg, err := parseServeConfigMode([]string{"--database", "rds"}, io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.warnings) != 1 || !strings.Contains(cfg.warnings[0], "no database") {
		t.Fatalf("missing RDS connection did not produce a warning: %v", cfg.warnings)
	}
}

func TestDiscoveredEndpointAlsoUpdatesSSOCallbacks(t *testing.T) {
	clearEnv(t)
	for _, explicit := range []string{"", "https://configured.example.com"} {
		args := []string{"--dsn", "postgres://localhost/conductor", "--nat-mode", "tailscale"}
		if explicit != "" {
			args = append(args, "--public-url", explicit)
		}
		cfg, err := parseServeConfig(args, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		want := "https://node.tailnet.ts.net"
		if explicit != "" {
			want = explicit
		}
		if endpoint := cfg.clientEndpoint("https://node.tailnet.ts.net"); endpoint != want || cfg.sso.PublicURL != want {
			t.Fatalf("API and SSO callbacks disagree: API=%s SSO=%s want=%s", endpoint, cfg.sso.PublicURL, want)
		}
	}
}
