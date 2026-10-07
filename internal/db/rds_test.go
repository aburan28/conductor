package db

import (
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
