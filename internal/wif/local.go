package wif

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const localAssertionLifetime = 5 * time.Minute

// LocalKeyPath is the default private-key location. Its contents must never be
// copied into a repository, Conductor task, log, or Claude prompt.
func LocalKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "wif", "local-issuer.pem"), nil
}

// LocalJWK is the public verification key to register as an inline JWKS key.
type LocalJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

func localJWK(key *ecdsa.PublicKey) (LocalJWK, error) {
	encoded, err := key.Bytes()
	if err != nil || len(encoded) != 65 || encoded[0] != 4 {
		return LocalJWK{}, errors.New("invalid local P-256 public key")
	}
	x := base64.RawURLEncoding.EncodeToString(encoded[1:33])
	y := base64.RawURLEncoding.EncodeToString(encoded[33:65])
	// RFC 7638 requires lexicographically ordered members for the thumbprint.
	thumbprint := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`))
	return LocalJWK{Kty: "EC", Crv: "P-256", X: x, Y: y,
		Kid: base64.RawURLEncoding.EncodeToString(thumbprint[:]), Alg: "ES256", Use: "sig"}, nil
}

// CreateLocalKey creates an issuer signing key exclusively, without replacing
// an existing key. Rotating it requires updating Anthropic's inline JWKS.
func CreateLocalKey(path string) (LocalJWK, error) {
	if path == "" {
		return LocalJWK{}, errors.New("local key path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return LocalJWK{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return LocalJWK{}, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return LocalJWK{}, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return LocalJWK{}, fmt.Errorf("create local issuer key: %w", err)
	}
	good := false
	defer func() {
		_ = f.Close()
		if !good {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		return LocalJWK{}, err
	}
	if err := f.Sync(); err != nil {
		return LocalJWK{}, err
	}
	if err := f.Close(); err != nil {
		return LocalJWK{}, err
	}
	good = true
	return localJWK(&key.PublicKey)
}

func loadLocalKey(path string) (*ecdsa.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("local issuer key must be a regular file readable only by its owner")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("local issuer key changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(raw) > 8192 {
		return nil, errors.New("local issuer key cannot be read or is too large")
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("local issuer key is not a single PKCS#8 PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("local issuer key is invalid")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("local issuer key must use P-256")
	}
	return key, nil
}

// PublicLocalJWK exports only the public verification key.
func PublicLocalJWK(path string) (LocalJWK, error) {
	key, err := loadLocalKey(path)
	if err != nil {
		return LocalJWK{}, err
	}
	return localJWK(&key.PublicKey)
}

// LocalProvider mints a fresh, audience-bound assertion on this machine.
// Anthropic issues the access token it returns after the assertion is exchanged;
// this assertion itself is signed by the local issuer key.
type LocalProvider struct {
	KeyPath string
	Issuer  string
	Subject string
	Now     func() time.Time // for deterministic tests
}

func (p LocalProvider) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	issuer, err := url.Parse(p.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Hostname() == "" || net.ParseIP(issuer.Hostname()) != nil || issuer.User != nil ||
		(issuer.Port() != "" && issuer.Port() != "443") || issuer.RawQuery != "" || issuer.Fragment != "" {
		return "", errors.New("local issuer must be an HTTPS URL with no query or fragment")
	}
	if p.Subject == "" || strings.TrimSpace(p.Subject) != p.Subject || len(p.Subject) > 512 ||
		strings.ContainsAny(p.Subject, "\r\n\x00") {
		return "", errors.New("local subject must be a nonempty exact identity without control characters")
	}
	key, err := loadLocalKey(p.KeyPath)
	if err != nil {
		return "", err
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	jwk, err := localJWK(&key.PublicKey)
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": jwk.Kid})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Aud string `json:"aud"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Jti string `json:"jti"`
	}{p.Issuer, p.Subject, Audience, now.Unix(), now.Add(localAssertionLifetime).Unix(),
		base64.RawURLEncoding.EncodeToString(nonce[:])})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString
	message := enc(header) + "." + enc(claims)
	digest := sha256.Sum256([]byte(message))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	assertion := message + "." + enc(signature)
	return checkedToken(assertion, now)
}

// LocalPublicKey reconstructs the verifier key from a public JWK. It is used
// by tests and by callers that want to verify an exported key independently.
func LocalPublicKey(jwk LocalJWK) (*ecdsa.PublicKey, error) {
	if jwk.Kty != "EC" || jwk.Crv != "P-256" || jwk.Alg != "ES256" {
		return nil, errors.New("unsupported local JWK")
	}
	xb, xerr := base64.RawURLEncoding.DecodeString(jwk.X)
	yb, yerr := base64.RawURLEncoding.DecodeString(jwk.Y)
	if xerr != nil || yerr != nil || len(xb) != 32 || len(yb) != 32 {
		return nil, errors.New("invalid local JWK coordinates")
	}
	encoded := make([]byte, 65)
	encoded[0] = 4
	copy(encoded[1:33], xb)
	copy(encoded[33:], yb)
	key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), encoded)
	if err != nil {
		return nil, errors.New("local JWK point is invalid")
	}
	derived, err := localJWK(key)
	if err != nil || derived.Kid != jwk.Kid {
		return nil, errors.New("local JWK key ID or point is invalid")
	}
	return key, nil
}
