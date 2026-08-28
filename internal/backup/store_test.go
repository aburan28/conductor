package backup

import (
	"context"
	"net/url"
	"testing"
	"time"
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
	_, srv := newFakeS3(t)
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
