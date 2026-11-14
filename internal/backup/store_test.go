package backup

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/backup/s3fake"
)

func TestFromEnvDisabledUntilBucketSet(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if _, ok, _ := FromEnv(get); ok {
		t.Fatal("backup should be disabled with no bucket")
	}
	env["CONDUCTOR_BACKUP_S3_BUCKET"] = "b"
	env["AWS_ACCESS_KEY_ID"] = "AK"
	env["AWS_SECRET_ACCESS_KEY"] = "SK"
	store, ok, err := FromEnv(get)
	if err != nil || !ok {
		t.Fatalf("FromEnv enabled=%v err=%v", ok, err)
	}
	if store.s3.cfg.AccessKey != "AK" || store.s3.cfg.Region != "us-east-1" {
		t.Errorf("env not applied: %+v", store.s3.cfg)
	}
	// The kill switch wins even with a bucket set.
	env["CONDUCTOR_BACKUP"] = "off"
	if _, ok, _ := FromEnv(get); ok {
		t.Fatal("CONDUCTOR_BACKUP=off must disable backup")
	}
}

func TestStorePutGetSnapshots(t *testing.T) {
	srv := s3fake.New("conductor-test")
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	store, err := Open(Config{
		S3Config: S3Config{Bucket: "conductor-test", Region: "us-east-1", AccessKey: "AK", SecretKey: "SK",
			Endpoint: u.Scheme + "://" + u.Host, PathStyle: true, Insecure: true},
		Prefix: "conductor", Machine: "laptop.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := store.GetSessions(ctx); err != ErrNotFound {
		t.Fatalf("empty machine Get = %v, want ErrNotFound", err)
	}
	data := []byte(`{"records":[{"id":"p1"}]}`)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.PutSessions(ctx, data, t0); err != nil {
		t.Fatalf("PutSessions: %v", err)
	}
	got, err := store.GetSessions(ctx)
	if err != nil || string(got) != string(data) {
		t.Fatalf("GetSessions = %q, %v", got, err)
	}
	if err := store.PutSessions(ctx, data, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	snaps, err := store.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %v, want 2", snaps)
	}
	if store.Machine() != "laptop.local" {
		t.Errorf("machine = %q", store.Machine())
	}
}

func TestWriteOnceSessionsPreferLatestSnapshotAndReadLegacy(t *testing.T) {
	srv := s3fake.New("conductor-test")
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	store, err := Open(Config{
		S3Config: S3Config{Bucket: "conductor-test", Region: "us-east-1", AccessKey: "AK", SecretKey: "SK",
			Endpoint: u.Scheme + "://" + u.Host, PathStyle: true, Insecure: true},
		Prefix: "conductor", Machine: "laptop.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	legacyKey := "conductor/machines/laptop.local/sessions.json"
	srv.Put(legacyKey, []byte("legacy"))
	if got, err := store.GetSessions(ctx); err != nil || string(got) != "legacy" {
		t.Fatalf("legacy restore = %q, %v", got, err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.PutSessions(ctx, []byte("first"), at); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSessions(ctx, []byte("second"), at.Add(time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	keys := srv.Keys("conductor/machines/laptop.local/snapshots/")
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("write-once snapshot keys = %v", keys)
	}
	if first, _ := srv.Object(keys[0]); string(first) != "first" {
		t.Fatalf("first snapshot changed: %q", first)
	}
	if legacy, _ := srv.Object(legacyKey); string(legacy) != "legacy" {
		t.Fatalf("legacy manifest overwritten: %q", legacy)
	}
	if got, err := store.GetSessions(ctx); err != nil || string(got) != "second" {
		t.Fatalf("latest restore = %q, %v", got, err)
	}
	if strings.HasSuffix(store.Location(), "sessions.json") {
		t.Fatalf("location still points to mutable manifest: %s", store.Location())
	}
}

func TestPutCheckpointNeverOverwritesEitherObject(t *testing.T) {
	srv := s3fake.New("conductor-test")
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	store, err := Open(Config{
		S3Config: S3Config{Bucket: "conductor-test", Region: "us-east-1", AccessKey: "AK", SecretKey: "SK",
			Endpoint: u.Scheme + "://" + u.Host, PathStyle: true, Insecure: true},
		Prefix: "conductor", Machine: "laptop.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sealed := func([]byte) bool { return true }
	if err := store.PutCheckpoint(ctx, "ck-1", []byte("index-1"), []byte("bundle-1"), sealed); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCheckpoint(ctx, "ck-1", []byte("index-2"), []byte("bundle-2"), sealed); !errors.Is(err, ErrExists) {
		t.Fatalf("repeat checkpoint = %v, want ErrExists", err)
	}
	for key, want := range map[string]string{
		"conductor/checkpoints/ck-1.ckpt": "bundle-1",
		"conductor/checkpoints/ck-1.json": "index-1",
	} {
		if got, _ := srv.Object(key); string(got) != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	srv.Put("conductor/checkpoints/ck-partial.ckpt", []byte("existing"))
	if err := store.PutCheckpoint(ctx, "ck-partial", []byte("new-index"), []byte("new-bundle"), sealed); !errors.Is(err, ErrExists) {
		t.Fatalf("partial checkpoint = %v, want ErrExists", err)
	}
	if got, _ := srv.Object("conductor/checkpoints/ck-partial.ckpt"); string(got) != "existing" {
		t.Errorf("partial bundle overwritten: %q", got)
	}
	if _, exists := srv.Object("conductor/checkpoints/ck-partial.json"); exists {
		t.Error("index created beside an existing partial bundle")
	}
	srv.Put("conductor/checkpoints/ck-index-only.json", []byte("existing-index"))
	if err := store.PutCheckpoint(ctx, "ck-index-only", []byte("new-index"), []byte("new-bundle"), sealed); !errors.Is(err, ErrExists) {
		t.Fatalf("index-only checkpoint = %v, want ErrExists", err)
	}
	if _, exists := srv.Object("conductor/checkpoints/ck-index-only.ckpt"); exists {
		t.Error("bundle created beside an existing partial index")
	}
}

func TestSanitizeMachine(t *testing.T) {
	cases := map[string]string{
		"laptop.local": "laptop.local", "a/b c": "a-b-c", "": "unknown-host", "ip-10-0-0-2": "ip-10-0-0-2",
	}
	for in, want := range cases {
		if got := sanitizeMachine(in); got != want {
			t.Errorf("sanitizeMachine(%q) = %q, want %q", in, got, want)
		}
	}
}

// The CONDUCTOR_BACKUP_S3_ACCESS_KEY variables take the secret and session token from the AWS_*
// variables when their own are unset. internal/storage's path must agree with this one.
func TestFromEnvAccessKeyFallsBackToAWSSecretAndToken(t *testing.T) {
	env := map[string]string{
		"CONDUCTOR_BACKUP_S3_BUCKET": "b", "CONDUCTOR_BACKUP_S3_ACCESS_KEY": "AKID",
		"AWS_SECRET_ACCESS_KEY": "awssecret", "AWS_SESSION_TOKEN": "awstok",
	}
	store, ok, err := FromEnv(func(k string) string { return env[k] })
	if err != nil || !ok {
		t.Fatalf("FromEnv enabled=%v err=%v", ok, err)
	}
	if c := store.s3.cfg; c.AccessKey != "AKID" || c.SecretKey != "awssecret" || c.SessionToken != "awstok" {
		t.Fatalf("access key path: %+v", c)
	}
}
