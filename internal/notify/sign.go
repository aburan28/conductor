package notify

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Webhook signatures.
//
// Every generic webhook request carries
//
//	X-Conductor-Timestamp: <unix seconds>
//	X-Conductor-Signature: sha256=<hex HMAC-SHA256(secret, "<timestamp>.<raw body>")>
//
// The secret is the whole string shown once when the channel was created ("whsec_…"), used
// as the HMAC key byte for byte. Signing the timestamp with the body is what makes a captured
// request useless later: a receiver that rejects timestamps outside a few minutes cannot be
// replayed into, and the timestamp cannot be changed without breaking the signature.

// Header names on every webhook request.
const (
	HeaderSignature = "X-Conductor-Signature"
	HeaderTimestamp = "X-Conductor-Timestamp"
	HeaderEvent     = "X-Conductor-Event"
	HeaderDelivery  = "X-Conductor-Delivery"
)

// DefaultTolerance is how far a request's timestamp may be from the receiver's clock.
const DefaultTolerance = 5 * time.Minute

// secretPrefix marks a webhook signing secret, so one pasted somewhere it should not be is
// recognisable.
const secretPrefix = "whsec_"

// NewSecret returns a fresh signing secret.
func NewSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Sign returns the X-Conductor-Signature value for a body sent at timestamp.
func Sign(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verification failures.
var (
	ErrNoSignature  = errors.New("request is not signed")
	ErrBadSignature = errors.New("signature does not match")
	ErrStale        = errors.New("timestamp is outside the tolerance; the request may be a replay")
)

// Verify checks a received webhook: the signature over its timestamp and raw body, and that
// the timestamp is within tolerance of now (DefaultTolerance when zero). Receivers written in
// Go can call it directly; README.md has the same check in Python.
func Verify(secret string, header http.Header, body []byte, now time.Time, tolerance time.Duration) error {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	sig, ts := header.Get(HeaderSignature), header.Get(HeaderTimestamp)
	if sig == "" || ts == "" {
		return ErrNoSignature
	}
	timestamp, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrNoSignature
	}
	// Compare before judging the time, so a forged request learns nothing about the clock.
	if !hmac.Equal([]byte(strings.ToLower(sig)), []byte(Sign(secret, timestamp, body))) {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(timestamp, 0)); d > tolerance || d < -tolerance {
		return ErrStale
	}
	return nil
}
