// Package wif obtains provider-issued OIDC assertions for Claude workload federation.
package wif

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const Audience = "https://api.anthropic.com"

// Provider obtains a new OIDC identity token. Its credential source remains
// outside Conductor: Google metadata, AWS STS, or a rotating file.
type Provider interface {
	Token(context.Context) (string, error)
}

type FileProvider struct{ Path string }

func (p FileProvider) Token(_ context.Context) (string, error) {
	if p.Path == "" {
		return "", errors.New("identity token file path is empty")
	}
	info, err := os.Stat(p.Path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("identity token path must be a regular file")
	}
	raw, err := os.ReadFile(p.Path)
	if err != nil {
		return "", err
	}
	return checkedToken(strings.TrimSpace(string(raw)), time.Now())
}

type GCPProvider struct{ Client *http.Client }

func (p GCPProvider) Token(ctx context.Context) (string, error) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	u := "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?" +
		url.Values{"audience": {Audience}, "format": {"full"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Google metadata token request: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Google metadata token request returned HTTP %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16*1024+1))
	if err != nil {
		return "", err
	}
	if len(raw) > 16*1024 {
		return "", errors.New("Google identity token exceeds 16 KiB")
	}
	return checkedToken(strings.TrimSpace(string(raw)), time.Now())
}

type AWSProvider struct {
	Region  string
	Command string // For tests; defaults to aws.
}

func (p AWSProvider) Token(ctx context.Context) (string, error) {
	if p.Region == "" {
		return "", errors.New("AWS STS region is required")
	}
	command := p.Command
	if command == "" {
		command = "aws"
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, command,
		"sts", "get-web-identity-token", "--region", p.Region,
		"--audience", Audience, "--signing-algorithm", "RS256",
		"--duration-seconds", "900", "--query", "WebIdentityToken", "--output", "text")
	raw, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("AWS STS identity token request failed: %w", err)
	}
	if len(raw) > 16*1024 {
		return "", errors.New("AWS identity token exceeds 16 KiB")
	}
	return checkedToken(strings.TrimSpace(string(raw)), time.Now())
}

// checkedToken checks the claims the launcher relies on. Anthropic performs
// independent signature, issuer, key, audience, and rule verification.
func checkedToken(token string, now time.Time) (string, error) {
	if len(token) == 0 || len(token) > 16*1024 {
		return "", errors.New("identity token is empty or exceeds 16 KiB")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("identity token is not a JWT")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", errors.New("identity token has invalid header encoding")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return "", errors.New("identity token has invalid header")
	}
	switch header.Algorithm {
	case "ES256", "ES384", "ES512", "RS256", "RS384", "RS512", "PS256", "PS384", "PS512":
	default:
		return "", errors.New("identity token must use an asymmetric algorithm supported by Anthropic")
	}
	if header.KeyID == "" {
		return "", errors.New("identity token has no signing key ID")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("identity token has invalid claims encoding")
	}
	var claims struct {
		Iss string          `json:"iss"`
		Sub string          `json:"sub"`
		Aud json.RawMessage `json:"aud"`
		Exp int64           `json:"exp"`
		Iat int64           `json:"iat"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", errors.New("identity token has invalid claims")
	}
	var aud string
	var audiences []string
	if json.Unmarshal(claims.Aud, &aud) == nil {
		audiences = []string{aud}
	} else if json.Unmarshal(claims.Aud, &audiences) != nil {
		return "", errors.New("identity token has invalid audience")
	}
	found := false
	for _, a := range audiences {
		if a == Audience {
			found = true
		}
	}
	if claims.Iss == "" || claims.Sub == "" || !found || claims.Iat <= 0 || claims.Exp <= claims.Iat ||
		claims.Iat > now.Add(30*time.Second).Unix() ||
		claims.Exp <= now.Add(30*time.Second).Unix() {
		return "", errors.New("identity token lacks a valid issuer, subject, Anthropic audience, or lifetime")
	}
	return token, nil
}

// TokenFile refreshes provider assertions for Claude Code, which reads a file
// on each Anthropic token exchange. It never stores a signing key.
type TokenFile struct {
	Path string
	stop context.CancelFunc
	done chan struct{}
	once sync.Once
}

func StartTokenFile(ctx context.Context, provider Provider) (*TokenFile, error) {
	dir, err := os.MkdirTemp("", "conductor-wif-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	path := filepath.Join(dir, "token")
	write := func(ctx context.Context) error {
		token, err := provider.Token(ctx)
		if err != nil {
			return err
		}
		tmp := filepath.Join(dir, "next")
		if err := os.WriteFile(tmp, []byte(token), 0600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	}
	if err := write(ctx); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	t := &TokenFile{Path: path, stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := write(runCtx); err != nil {
					// A failed refresh must not leave an old assertion available
					// for another exchange. The next tick can recover.
					_ = os.Remove(path)
				}
			}
		}
	}()
	return t, nil
}

func (t *TokenFile) Stop() error {
	var err error
	t.once.Do(func() {
		t.stop()
		<-t.done
		err = os.RemoveAll(filepath.Dir(t.Path))
	})
	return err
}
