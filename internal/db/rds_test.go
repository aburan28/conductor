package db

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRDSCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Conductor test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRDSLiveConnection is an opt-in read-only smoke against an existing RDS endpoint.
// The file form keeps credentials out of shell history and process arguments. It does
// not apply migrations or read tenant rows; use a disposable database for write,
// migration, failover, and restore testing.
func TestRDSLiveConnection(t *testing.T) {
	file := os.Getenv("CI_RDS_DATABASE_URL_FILE")
	dsn := os.Getenv("CI_RDS_DATABASE_URL")
	if file != "" && dsn != "" {
		t.Fatal("set only one RDS test DSN source")
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read RDS test DSN file: %v", err)
		}
		dsn = strings.TrimSpace(string(data))
	}
	if dsn == "" {
		t.Skip("CI_RDS_DATABASE_URL_FILE or CI_RDS_DATABASE_URL is required for live RDS smoke")
	}
	if err := ValidateDSN("rds", dsn); err != nil {
		t.Fatalf("RDS TLS configuration: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("RDS connection failed (%T; connection details omitted)", err)
	}
	defer store.Close()
	var ssl bool
	var tlsVersion, serverVersion string
	err = store.Pool().QueryRow(ctx, `SELECT ssl, COALESCE(version, '')
		FROM pg_stat_ssl WHERE pid = pg_backend_pid()`).Scan(&ssl, &tlsVersion)
	if err != nil {
		t.Fatalf("read own TLS session (%T; connection details omitted)", err)
	}
	if !ssl || tlsVersion == "" {
		t.Fatal("RDS session did not negotiate TLS")
	}
	if err := store.Pool().QueryRow(ctx, `SHOW server_version`).Scan(&serverVersion); err != nil {
		t.Fatalf("read server version (%T; connection details omitted)", err)
	}
	t.Logf("RDS read-only smoke passed: PostgreSQL %s, %s", serverVersion, tlsVersion)
}

func TestRDSRequiresVerifiedTLSOnEveryHost(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	t.Setenv("PGSSLROOTCERT", "")
	ca := testRDSCA(t)
	for _, tc := range []struct {
		name, dsn string
		valid     bool
	}{
		{"plaintext", "postgres://user:private-secret@db.example/conductor?sslmode=disable", false},
		{"encrypted-unverified", "postgres://user@db.example/conductor?sslmode=require", false},
		{"chain-only", "postgres://user@db.example/conductor?sslmode=verify-ca&sslrootcert=" + url.QueryEscape(ca), false},
		{"missing-ca", "postgres://user@db.example/conductor?sslmode=verify-full", false},
		{"china", "postgres://user@db.abc.cn-north-1.rds.amazonaws.com.cn/conductor?sslmode=verify-full&sslrootcert=" + url.QueryEscape(ca), true},
		{"custom-dns", "postgres://user@private-db.example/conductor?sslmode=verify-full&sslrootcert=" + url.QueryEscape(ca), true},
		{"keyword", "host=db.example user=user dbname=conductor sslmode=verify-full sslrootcert='" + ca + "'", true},
		{"multiple-hosts", "host=first.example,second.example user=user dbname=conductor sslmode=verify-full sslrootcert='" + ca + "'", true},
		{"unix-socket", "host=/tmp user=user dbname=conductor sslmode=verify-full sslrootcert='" + ca + "'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDSN("rds", tc.dsn)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("validation error leaked the database password")
			}
		})
	}
}

func TestDatabaseModeValidationPreservesExternalPostgres(t *testing.T) {
	for _, mode := range []string{"", "local", "external"} {
		if err := ValidateDSN(mode, "postgres://user@localhost/conductor?sslmode=disable"); err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
	for _, tc := range []struct{ mode, dsn string }{
		{"unknown", "postgres://user@localhost/conductor"},
		{"external", ""},
		{"rds", "postgres://user:private-secret@db.example/conductor?sslmode=invalid"},
		{"rds", "postgres://user@db.example/conductor?sslmode=verify-full&sslrootcert=/does/not/exist"},
	} {
		if err := ValidateDSN(tc.mode, tc.dsn); err == nil {
			t.Fatal("invalid deployment configuration accepted")
		} else if strings.Contains(err.Error(), "private-secret") {
			t.Fatal("parse error leaked database credentials")
		}
	}
}
