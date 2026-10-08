package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/backup/s3fake"
	"github.com/aburan28/conductor/internal/storage"
)

// captureStdout runs fn and returns what it printed.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	runErr := fn()
	w.Close()
	os.Stdout = old
	return <-done, runErr
}

func isolateStorage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", dir)
	for _, k := range []string{"CONDUCTOR_BACKUP", "CONDUCTOR_BACKUP_S3_BUCKET", "CONDUCTOR_CHECKPOINT_KEY",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_PROFILE"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	return dir
}

func TestStorageSetMergesAndReadsSecretFromStdin(t *testing.T) {
	isolateStorage(t)
	ctx := context.Background()
	_, err := captureStdout(t, func() error {
		return storageSet(ctx, []string{"--bucket", "b1", "--region", "eu-west-1", "--auth", "static",
			"--access-key-id", "AKIDCLI", "--secret-from", "stdin", "--secret-store", "file"}, strings.NewReader("clisecret\n"))
	})
	if err != nil {
		t.Fatal(err)
	}
	// Change one thing; the rest stays.
	if _, err := captureStdout(t, func() error {
		return storageSet(ctx, []string{"--database=false", "--prefix", "/team/"}, strings.NewReader(""))
	}); err != nil {
		t.Fatal(err)
	}
	s, ok, err := storage.Load(os.Getenv)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if s.S3.Bucket != "b1" || s.S3.Region != "eu-west-1" || s.S3.Prefix != "team" || s.Auth.AccessKeyID != "AKIDCLI" ||
		s.Auth.SecretAccessKey != "clisecret" || s.Uses.DatabaseOn() || !s.Uses.SessionsOn() {
		t.Fatalf("merged settings: %+v", s)
	}

	// Flags for another method are refused rather than silently ignored.
	err = storageSet(ctx, []string{"--auth", "profile", "--access-key-id", "X"}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "--access-key-id applies only to --auth static") {
		t.Fatalf("mismatched flag: %v", err)
	}
	// Off macOS there is no Keychain to point at.
	err = storageSet(ctx, []string{"--auth", "static", "--access-key-id", "AKID2", "--secret-from", "keychain"}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "Keychain") {
		t.Fatalf("keychain off macOS: %v", err)
	}
}

func TestStorageShowJSONContract(t *testing.T) {
	isolateStorage(t)
	out, err := captureStdout(t, func() error { return storageShow(context.Background(), []string{"--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var none map[string]any
	if err := json.Unmarshal([]byte(out), &none); err != nil || none["configured"] != false || none["source"] != "none" {
		t.Fatalf("unconfigured view: %s %v", out, err)
	}

	if _, err := captureStdout(t, func() error {
		return storageSet(context.Background(), []string{"--bucket", "b", "--auth", "static", "--access-key-id", "AKID",
			"--secret-from", "stdin", "--secret-store", "file"}, strings.NewReader("topsecret\n"))
	}); err != nil {
		t.Fatal(err)
	}
	out, err = captureStdout(t, func() error { return storageShow(context.Background(), []string{"--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "topsecret") {
		t.Fatalf("show printed the secret: %s", out)
	}
	var v struct {
		Configured bool   `json:"configured"`
		Source     string `json:"source"`
		Path       string `json:"path"`
		S3         struct {
			Bucket, Prefix string
		} `json:"s3"`
		Auth struct {
			Method      string `json:"method"`
			AccessKeyID string `json:"access_key_id"`
			Secret      string `json:"secret"`
		} `json:"auth"`
		Uses     map[string]bool `json:"uses"`
		Database struct {
			ArchiveWAL      bool `json:"archive_wal"`
			KeepBaseBackups int  `json:"keep_base_backups"`
			Seal            bool `json:"seal"`
		} `json:"database"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Configured || v.Source != "file" || v.S3.Bucket != "b" || v.S3.Prefix != "conductor" ||
		v.Auth.Method != "static" || v.Auth.AccessKeyID != "AKID" || v.Auth.Secret != "file" ||
		!v.Uses["sessions"] || !v.Uses["database"] || !v.Database.ArchiveWAL || v.Database.KeepBaseBackups != 7 || !v.Database.Seal {
		t.Fatalf("configured view: %+v\n%s", v, out)
	}
}

func TestStorageSetTestThenBackupPushUsesTheBucket(t *testing.T) {
	isolateStorage(t)
	t.Setenv("CONDUCTOR_MACHINE_ID", "laptop")
	fake := s3fake.New("team")
	defer fake.Close()
	ctx := context.Background()
	if _, err := captureStdout(t, func() error {
		return storageSet(ctx, []string{"--bucket", "team", "--endpoint", fake.URL, "--path-style", "--insecure",
			"--auth", "static", "--access-key-id", "AKIDE2E", "--secret-from", "stdin", "--secret-store", "file"},
			strings.NewReader("e2esecret\n"))
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return storageTest(ctx, []string{"--json"}) })
	if err != nil {
		t.Fatalf("storage test: %v\n%s", err, out)
	}
	var res storage.TestResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.OK || len(res.Steps) != 5 {
		t.Fatalf("test result: %s %v", out, err)
	}

	if _, err := captureStdout(t, func() error { return backupPush(ctx, nil) }); err != nil {
		t.Fatalf("backup push: %v", err)
	}
	if _, ok := fake.Object("conductor/machines/laptop/sessions.json"); !ok {
		t.Fatalf("backup push did not reach the configured bucket: %v", fake.Keys(""))
	}
	for _, ak := range fake.AccessKeys {
		if ak != "AKIDE2E" {
			t.Fatalf("signed with %q", ak)
		}
	}

	// Turning sessions off stops the upload, with a reason.
	if _, err := captureStdout(t, func() error { return storageSet(ctx, []string{"--sessions=false"}, strings.NewReader("")) }); err != nil {
		t.Fatal(err)
	}
	_, err = captureStdout(t, func() error { return backupPush(ctx, nil) })
	if err == nil || !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("sessions off: %v", err)
	}
}

func TestStorageTestFailsWithoutBucket(t *testing.T) {
	isolateStorage(t)
	out, err := captureStdout(t, func() error { return storageTest(context.Background(), []string{"--json"}) })
	if err == nil || !strings.Contains(out, `"ok": false`) {
		t.Fatalf("no bucket: %v %s", err, out)
	}
}

func TestStorageSetRejectsSpaceSeparatedBool(t *testing.T) {
	isolateStorage(t)
	err := storageSet(context.Background(), []string{"--bucket", "b", "--sessions", "false", "--auth", "profile"}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), `unexpected argument "false"`) {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := storage.Load(os.Getenv); ok {
		t.Fatal("a half-parsed command still wrote settings")
	}
}

// --secret-store keychain with no --secret-from used to keep a plaintext secret already in
// storage.json and still print "Saved". It is refused, and the error names both flags.
func TestStorageSecretStoreKeychainNeedsSecretFrom(t *testing.T) {
	isolateStorage(t)
	ctx := context.Background()
	if _, err := captureStdout(t, func() error {
		return storageSet(ctx, []string{"--bucket", "b1", "--auth", "static", "--access-key-id", "AKIDPLAIN",
			"--secret-from", "stdin", "--secret-store", "file"}, strings.NewReader("plainsecret\n"))
	}); err != nil {
		t.Fatal(err)
	}
	err := storageSet(ctx, []string{"--secret-store", "keychain"}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "--secret-store") || !strings.Contains(err.Error(), "--secret-from") {
		t.Fatalf("--secret-store keychain without --secret-from = %v; want an error naming both flags", err)
	}
	s, ok, err := storage.Load(os.Getenv)
	if err != nil || !ok || s.Auth.Secret != storage.SecretFile || s.Auth.SecretAccessKey != "plainsecret" {
		t.Fatalf("settings changed by the refused command: %+v, %v", s.Auth, err)
	}
}

// Database backups can be configured, but this build has no archiver. `storage show` must say
// so rather than describe a WAL archive that is not running.
func TestStorageShowSaysDatabaseArchivingIsUnavailable(t *testing.T) {
	isolateStorage(t)
	ctx := context.Background()
	if _, err := captureStdout(t, func() error {
		return storageSet(ctx, []string{"--bucket", "b1", "--database=true"}, strings.NewReader(""))
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return storageShow(ctx, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "configured") || !strings.Contains(out, "archiving is not available in this build") {
		t.Fatalf("storage show does not say database archiving is unavailable:\n%s", out)
	}
}
