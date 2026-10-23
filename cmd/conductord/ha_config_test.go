package main

import (
	"encoding/pem"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ECS injects credentials as separate environment variables. Keep them out of shell
// commands and URL interpolation, including passwords with URL/DSN metacharacters.
func TestHAConfigUsesPostgresEnvironmentWithVerifiedTLS(t *testing.T) {
	clearEnv(t)
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	password := "a' b\\c:@/?#%=$secret"
	for k, v := range map[string]string{
		"DATABASE_URL": "connect_timeout=5", "CONDUCTOR_DATABASE_MODE": "rds",
		"PGHOST": "database.example.internal", "PGPORT": "5432", "PGUSER": "conductor",
		"PGDATABASE": "conductor", "PGPASSWORD": password,
		"PGSSLMODE": "verify-full", "PGSSLROOTCERT": ca,
		"PGSERVICE": "", "PGSERVICEFILE": "", "PGSSLKEY": "", "PGSSLCERT": "",
	} {
		t.Setenv(k, v)
	}
	cfg, err := parseServeConfig([]string{"--addr", "0.0.0.0:8080", "--insecure", "--behind-proxy", "--security-mode", "enhanced"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := pgxpool.ParseConfig(cfg.dsn)
	if err != nil {
		t.Fatal("parse deployment database configuration")
	}
	if pg.ConnConfig.Password != password || pg.ConnConfig.Host != "database.example.internal" || pg.ConnConfig.Database != "conductor" {
		t.Fatal("PostgreSQL environment was not preserved")
	}
	if pg.ConnConfig.TLSConfig == nil || pg.ConnConfig.TLSConfig.InsecureSkipVerify || pg.ConnConfig.TLSConfig.ServerName != pg.ConnConfig.Host {
		t.Fatal("database connection must verify server identity")
	}
	t.Setenv("PGSSLMODE", "require")
	if _, err := parseServeConfig(nil, io.Discard); err == nil {
		t.Fatal("HA configuration accepted TLS without server identity verification")
	}
}
