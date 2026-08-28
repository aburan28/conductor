package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/backup"
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
