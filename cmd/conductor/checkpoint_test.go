package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/backup"
	"github.com/aburan28/conductor/internal/checkpoint"
)

func TestPushCheckpointRetriesMissingIndexWithVerifiedBundle(t *testing.T) {
	store, remote := memoryBackup(t)
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	m := checkpoint.Manifest{
		Schema: checkpoint.Schema, ID: checkpoint.NewID(time.Now()),
		CreatedAt: time.Now().UTC(), Harness: "claude",
	}
	local := []byte("original local archive")
	if _, err := checkpoint.Put(m, local); err != nil {
		t.Fatal(err)
	}
	sealed, err := checkpoint.Seal(local, "correct passphrase")
	if err != nil {
		t.Fatal(err)
	}
	bundleKey := "conductor/checkpoints/" + m.ID + ".ckpt"
	indexKey := "conductor/checkpoints/" + m.ID + ".json"
	remote.mu.Lock()
	remote.obj = map[string][]byte{bundleKey: sealed}
	remote.mu.Unlock()

	if err := pushCheckpoint(context.Background(), store, m, "wrong passphrase"); !errors.Is(err, backup.ErrExists) {
		t.Fatalf("wrong passphrase retry = %v, want ErrExists", err)
	}
	if _, err := checkpoint.Put(m, []byte("changed local archive")); err != nil {
		t.Fatal(err)
	}
	if err := pushCheckpoint(context.Background(), store, m, "correct passphrase"); !errors.Is(err, backup.ErrExists) {
		t.Fatalf("different local archive retry = %v, want ErrExists", err)
	}
	remote.mu.Lock()
	_, hasIndex := remote.obj[indexKey]
	remote.mu.Unlock()
	if hasIndex {
		t.Fatal("index created before the existing ciphertext was verified")
	}

	if _, err := checkpoint.Put(m, local); err != nil {
		t.Fatal(err)
	}
	if err := pushCheckpoint(context.Background(), store, m, "correct passphrase"); err != nil {
		t.Fatalf("identical local archive retry: %v", err)
	}
	remote.mu.Lock()
	gotBundle := remote.obj[bundleKey]
	_, hasIndex = remote.obj[indexKey]
	remote.mu.Unlock()
	if !bytes.Equal(gotBundle, sealed) {
		t.Error("existing ciphertext was overwritten")
	}
	if !hasIndex {
		t.Error("missing index was not created")
	}
}
