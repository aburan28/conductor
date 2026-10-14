package sso

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ID token signature verification: a compact JWS (RFC 7515) signed with RS256 or ES256 under
// a key from the issuer's JWKS (RFC 7517). Those two algorithms are what every mainstream
// issuer signs ID tokens with. Everything else — "none", HMAC (which would let anyone who
// knows the client secret forge a token), and algorithms this code does not implement — is
// refused before a key is even looked up.

// jwsHeader is the protected header of a compact JWS.
type jwsHeader struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Typ  string   `json:"typ"`
	Crit []string `json:"crit"`
}

// parseJWS splits a compact JWS and decodes its header and payload, without verifying it.
func parseJWS(raw string) (h jwsHeader, payload, signingInput, sig []byte, err error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return h, nil, nil, nil, errors.New("not a compact JWS")
	}
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return h, nil, nil, nil, fmt.Errorf("header: %w", err)
	}
	if err := json.Unmarshal(head, &h); err != nil {
		return h, nil, nil, nil, fmt.Errorf("header: %w", err)
	}
	if payload, err = base64.RawURLEncoding.DecodeString(parts[1]); err != nil {
		return h, nil, nil, nil, fmt.Errorf("payload: %w", err)
	}
	if sig, err = base64.RawURLEncoding.DecodeString(parts[2]); err != nil {
		return h, nil, nil, nil, fmt.Errorf("signature: %w", err)
	}
	return h, payload, []byte(parts[0] + "." + parts[1]), sig, nil
}

// supportedAlg reports whether alg is one this package verifies.
func supportedAlg(alg string) bool { return alg == "RS256" || alg == "ES256" }

// verifySignature checks sig over input with key under alg. The key's type must match the
// algorithm: an RSA key never verifies an ES256 signature, or the reverse.
func verifySignature(alg string, key crypto.PublicKey, input, sig []byte) error {
	sum := sha256.Sum256(input)
	switch alg {
	case "RS256":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("RS256 token, but the key is not RSA")
		}
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig)
	case "ES256":
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() {
			return errors.New("ES256 token, but the key is not P-256")
		}
		// A JWS ECDSA signature is r||s, each exactly 32 bytes, not ASN.1 (RFC 7518 §3.4).
		if len(sig) != 64 {
			return errors.New("ES256 signature is not 64 bytes")
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(pub, sum[:], r, s) {
			return errors.New("ES256 signature does not verify")
		}
		return nil
	}
	return fmt.Errorf("unsupported alg %q", alg)
}

// ---------------------------------------------------------------------------
// JWKS
// ---------------------------------------------------------------------------

// jwk is one key of a JWKS document.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type publicKey struct {
	kid string
	alg string // the key's declared alg, if any
	key crypto.PublicKey
}

// keyFits reports whether a key may verify a token signed with alg.
func (k publicKey) fits(alg string) bool {
	if k.alg != "" && k.alg != alg {
		return false
	}
	switch k.key.(type) {
	case *rsa.PublicKey:
		return alg == "RS256"
	case *ecdsa.PublicKey:
		return alg == "ES256"
	}
	return false
}

// decodeJWK turns a JWK into a public key. Keys this package cannot use — encryption keys,
// other curves, short RSA moduli — are skipped by the caller, not treated as an error, since
// an issuer's set routinely carries keys for other purposes.
func decodeJWK(k jwk) (publicKey, error) {
	if k.Use != "" && k.Use != "sig" {
		return publicKey{}, fmt.Errorf("key %q is for %q, not signatures", k.Kid, k.Use)
	}
	out := publicKey{kid: k.Kid, alg: k.Alg}
	switch k.Kty {
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return out, fmt.Errorf("key %q: n: %w", k.Kid, err)
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return out, fmt.Errorf("key %q: bad exponent", k.Kid)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			return out, fmt.Errorf("key %q: RSA modulus of %d bits is too short", k.Kid, pub.N.BitLen())
		}
		if pub.E < 3 || pub.E%2 == 0 {
			return out, fmt.Errorf("key %q: bad exponent", k.Kid)
		}
		out.key = pub
	case "EC":
		if k.Crv != "P-256" {
			return out, fmt.Errorf("key %q: curve %q is not supported", k.Kid, k.Crv)
		}
		x, errX := base64.RawURLEncoding.DecodeString(k.X)
		y, errY := base64.RawURLEncoding.DecodeString(k.Y)
		if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
			return out, fmt.Errorf("key %q: bad coordinates", k.Kid)
		}
		// ParseUncompressedPublicKey rejects a point that is not on the curve, which a
		// hand-built ecdsa.PublicKey would accept and an invalid-curve attack relies on.
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			return out, fmt.Errorf("key %q: %w", k.Kid, err)
		}
		out.key = pub
	default:
		return out, fmt.Errorf("key %q: type %q is not supported", k.Kid, k.Kty)
	}
	return out, nil
}

const (
	// jwksMaxAge is how long a fetched key set is trusted before it is fetched again.
	jwksMaxAge = time.Hour
	// jwksMinRefetch bounds refetches triggered by an unknown key id. Rotation publishes the
	// new key before signing with it, so one refetch finds it; without a bound, a stream of
	// tokens naming made-up key ids would turn this server into a request amplifier aimed at
	// the issuer.
	jwksMinRefetch = 30 * time.Second
)

// keySet caches an issuer's JWKS and refreshes it on rotation.
type keySet struct {
	uri  string
	http *http.Client
	now  func() time.Time

	mu        sync.Mutex
	keys      []publicKey
	fetchedAt time.Time
}

// find returns the keys that may verify a token with this kid and alg, fetching the set when
// it is stale or does not have the key yet.
func (s *keySet) find(ctx context.Context, kid, alg string) ([]crypto.PublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.fetchedAt.IsZero() || now.Sub(s.fetchedAt) > jwksMaxAge {
		if err := s.fetchLocked(ctx); err != nil {
			return nil, err
		}
	}
	match := s.matchLocked(kid, alg)
	if len(match) == 0 && now.Sub(s.fetchedAt) >= jwksMinRefetch {
		// An unknown key id is what a rotation looks like from here.
		if err := s.fetchLocked(ctx); err != nil {
			return nil, err
		}
		match = s.matchLocked(kid, alg)
	}
	if len(match) == 0 {
		return nil, invalidToken("no key in the issuer's key set matches kid %q and alg %s", kid, alg)
	}
	return match, nil
}

func (s *keySet) matchLocked(kid, alg string) []crypto.PublicKey {
	var out []crypto.PublicKey
	for _, k := range s.keys {
		if (kid == "" || k.kid == kid) && k.fits(alg) {
			out = append(out, k.key)
		}
	}
	return out
}

func (s *keySet) fetchLocked(ctx context.Context) error {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := getJSON(ctx, s.http, s.uri, "", &doc); err != nil {
		return fail(CodeUpstream, "the identity provider's signing keys could not be fetched", err)
	}
	keys := make([]publicKey, 0, len(doc.Keys))
	for _, k := range doc.Keys {
		if pk, err := decodeJWK(k); err == nil {
			keys = append(keys, pk)
		}
	}
	// A successful fetch replaces the set, so a key the issuer withdrew stops verifying
	// tokens here within jwksMaxAge (or at once, when a rotation forces a refetch).
	s.keys, s.fetchedAt = keys, s.now()
	return nil
}
