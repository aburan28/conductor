package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/awscreds"
	"github.com/aburan28/conductor/internal/backup"
)

// Step is one stage of a connection test.
type Step struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	MS    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

// TestResult is what `conductor storage test --json` prints.
type TestResult struct {
	OK          bool   `json:"ok"`
	Credentials string `json:"credentials"`
	Location    string `json:"location"`
	Steps       []Step `json:"steps"`
	Error       string `json:"error"`
}

// Test resolves credentials, then puts, gets, lists and deletes a probe object under the
// prefix. It stops at the first failure and says which step failed. The probe holds no data.
func Test(ctx context.Context, env awscreds.Env, r Resolved) TestResult {
	res := TestResult{Location: fmt.Sprintf("s3://%s/%s", r.Settings.S3.Bucket, r.Prefix())}
	fail := func(step Step, err error) TestResult {
		step.Error = err.Error()
		res.Steps = append(res.Steps, step)
		res.Error = step.Name + ": " + err.Error()
		return res
	}
	if !r.Configured() {
		res.Error = "no bucket is configured"
		return res
	}

	start := time.Now()
	cfg, err := r.S3Config(env)
	if err == nil {
		var creds backup.Credentials
		creds, err = cfg.Credentials.Retrieve(ctx)
		res.Credentials = creds.Source
	}
	step := Step{Name: "credentials", MS: time.Since(start).Milliseconds()}
	if err != nil {
		return fail(step, err)
	}
	step.OK = true
	res.Steps = append(res.Steps, step)

	client := backup.New(cfg)
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	key := r.Prefix() + "/.probe/" + hex.EncodeToString(nonce[:])
	body := []byte("conductor storage test " + time.Now().UTC().Format(time.RFC3339) + "\n")

	run := func(name string, fn func() error) bool {
		start := time.Now()
		err := fn()
		step := Step{Name: name, MS: time.Since(start).Milliseconds()}
		if err != nil {
			res = fail(step, err)
			return false
		}
		step.OK = true
		res.Steps = append(res.Steps, step)
		return true
	}
	if !run("put", func() error { return client.Put(ctx, key, body, "text/plain") }) {
		return res
	}
	// From here on, try to remove the probe whatever happens.
	defer func() { _ = client.Delete(context.WithoutCancel(ctx), key) }()
	if !run("get", func() error {
		got, err := client.Get(ctx, key)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, body) {
			return fmt.Errorf("read back %d bytes that differ from the %d written", len(got), len(body))
		}
		return nil
	}) {
		return res
	}
	if !run("list", func() error {
		keys, err := client.List(ctx, r.Prefix()+"/.probe/")
		if err != nil {
			return err
		}
		for _, k := range keys {
			if k == key {
				return nil
			}
		}
		return fmt.Errorf("the probe is missing from the listing (%d keys under %s/.probe/)", len(keys), r.Prefix())
	}) {
		return res
	}
	if !run("delete", func() error { return client.Delete(ctx, key) }) {
		return res
	}
	res.OK = true
	return res
}

// DescribeAuth is a one-line summary of the sign-in method, without secrets.
func (r Resolved) DescribeAuth() string {
	a := r.Settings.Auth
	switch a.Method {
	case AuthStatic:
		where := "the Keychain"
		if a.Secret == SecretFile {
			where = "the settings file"
			if r.Source == SourceEnv {
				where = "CONDUCTOR_BACKUP_S3_SECRET_KEY"
			}
		}
		return fmt.Sprintf("access key %s, secret in %s", a.AccessKeyID, where)
	case AuthProfile:
		name := a.Profile
		if name == "" {
			name = "AWS_PROFILE, else default"
		}
		return "AWS profile " + name
	default:
		return "environment (AWS_* variables, web identity, ECS, or the EC2 instance role)"
	}
}

// Redacted is Auth without the secret, for display.
func (a Auth) Redacted() Auth {
	a.SecretAccessKey = ""
	if strings.TrimSpace(a.Secret) == "" && a.Method == AuthStatic {
		a.Secret = SecretKeychain
	}
	return a
}
