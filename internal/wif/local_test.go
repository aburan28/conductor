package wif

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalAssertionVerifiesAgainstExportedJWKS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issuer.pem")
	created, err := CreateLocalKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateLocalKey(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing key was not protected: %v", err)
	}
	exported, err := PublicLocalJWK(path)
	if err != nil || exported != created {
		t.Fatalf("public JWKS changed: %v", err)
	}
	public, err := LocalPublicKey(exported)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	provider := LocalProvider{KeyPath: path, Issuer: "https://laptop.example.test/issuer", Subject: "conductor:laptop:adam", Now: func() time.Time { return now }}
	first, err := provider.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Token(context.Background())
	if err != nil || first == second {
		t.Fatalf("assertions must be freshly signed: %v", err)
	}
	firstClaims := verifyLocalJWT(t, public, exported.Kid, first)
	secondClaims := verifyLocalJWT(t, public, exported.Kid, second)
	if firstClaims.Iss != provider.Issuer || firstClaims.Sub != provider.Subject || firstClaims.Aud != Audience ||
		firstClaims.Iat != now.Unix() || firstClaims.Exp != now.Add(5*time.Minute).Unix() ||
		firstClaims.Jti == "" || firstClaims.Jti == secondClaims.Jti {
		t.Fatalf("wrong identity, audience, lifetime or nonce: %+v", firstClaims)
	}
	parts := strings.Split(first, ".")
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"forged"}`))
	if verifyLocalSignature(public, strings.Join(parts, ".")) {
		t.Fatal("tampered claims verified")
	}
}

type localClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

func verifyLocalJWT(t *testing.T, key *ecdsa.PublicKey, kid, token string) localClaims {
	t.Helper()
	if !verifyLocalSignature(key, token) {
		t.Fatal("local JWT signature did not verify with exported JWK")
	}
	parts := strings.Split(token, ".")
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var header struct{ Alg, Kid string }
	if err := json.Unmarshal(headerRaw, &header); err != nil || header.Alg != "ES256" || header.Kid != kid {
		t.Fatalf("invalid JOSE header: %s, %v", headerRaw, err)
	}
	claimRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims localClaims
	if err := json.Unmarshal(claimRaw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func verifyLocalSignature(key *ecdsa.PublicKey, token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return ecdsa.Verify(key, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

func TestLocalIssuerRejectsUnsafeKeyAndClaims(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "issuer.pem")
	if _, err := CreateLocalKey(path); err != nil {
		t.Fatal(err)
	}
	provider := LocalProvider{KeyPath: path, Issuer: "https://laptop.example.test", Subject: "laptop-1"}
	for _, changed := range []LocalProvider{
		{KeyPath: path, Issuer: "http://laptop.example.test", Subject: "laptop-1"},
		{KeyPath: path, Issuer: "https://127.0.0.1", Subject: "laptop-1"},
		{KeyPath: path, Issuer: "https://laptop.example.test:8443", Subject: "laptop-1"},
		{KeyPath: path, Issuer: provider.Issuer, Subject: ""},
		{KeyPath: path, Issuer: provider.Issuer, Subject: "laptop\nforged"},
	} {
		if _, err := changed.Token(context.Background()); err == nil {
			t.Fatalf("accepted invalid issuer or subject: %+v", changed)
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Token(context.Background()); err == nil {
		t.Fatal("accepted group/world readable issuer key")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	provider.KeyPath = link
	if _, err := provider.Token(context.Background()); err == nil {
		t.Fatal("accepted issuer key symlink")
	}
}
