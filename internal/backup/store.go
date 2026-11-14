package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Store is the off-host home for one machine's resume records. It namespaces objects by
// machine under a prefix, keeps write-once snapshots, and hands the
// bytes back on another machine (or the same one rebuilt) so `conductor resume` can pick up
// where a terminated instance left off.
//
// It works on opaque bytes on purpose: what a session record contains is the caller's
// business (it is coordination metadata — pids, cwds, resume invocations — never a
// transcript), and keeping the S3 layer ignorant of it keeps the privacy boundary obvious.
type Store struct {
	s3      *S3
	prefix  string
	machine string
}

// Config configures a Store. Bucket is required; everything else has a default.
type Config struct {
	S3Config
	// Prefix is the key prefix inside the bucket. Default "conductor".
	Prefix string
	// Machine names this host in the key layout. Default: CONDUCTOR_MACHINE_ID or the hostname.
	Machine string
}

// Open builds a Store. It returns an error only for a plainly unusable config (no bucket);
// credential problems surface on the first request, where the S3 error is specific.
func Open(cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("backup: no bucket configured")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "conductor"
	}
	if cfg.Machine == "" {
		cfg.Machine = "unknown-host"
	}
	return &Store{
		s3:      New(cfg.S3Config),
		prefix:  sanitizePrefix(cfg.Prefix),
		machine: sanitizeMachine(cfg.Machine),
	}, nil
}

// sanitizePrefix keeps a key prefix to safe path characters. A prefix is operator-chosen and
// need not carry reserved characters; restricting it means an odd value degrades to a usable
// key rather than a SignatureDoesNotMatch surprise, and the keys Conductor generates encode
// identically on the wire and in the signature.
func sanitizePrefix(p string) string {
	p = strings.Trim(p, "/")
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '/':
			return r
		}
		return '-'
	}, p)
	if out == "" {
		return "conductor"
	}
	return out
}

// FromEnv builds a Store from the environment, or reports enabled=false when no bucket is
// set (backup is opt-in) or CONDUCTOR_BACKUP is turned off. getenv is injected for tests.
//
// Bucket/region/prefix/endpoint come from CONDUCTOR_BACKUP_S3_*; credentials from the
// standard AWS_* variables (so an instance role or `aws configure export-credentials`
// environment just works), with CONDUCTOR_BACKUP_S3_ACCESS_KEY / _SECRET_KEY overrides.
func FromEnv(getenv func(string) string) (*Store, bool, error) {
	switch strings.ToLower(getenv("CONDUCTOR_BACKUP")) {
	case "off", "0", "false", "no":
		return nil, false, nil
	}
	bucket := getenv("CONDUCTOR_BACKUP_S3_BUCKET")
	if bucket == "" {
		return nil, false, nil
	}
	cfg := Config{
		S3Config: S3Config{
			Bucket:       bucket,
			Region:       firstNonEmpty(getenv("CONDUCTOR_BACKUP_S3_REGION"), getenv("AWS_REGION"), getenv("AWS_DEFAULT_REGION"), "us-east-1"),
			Endpoint:     getenv("CONDUCTOR_BACKUP_S3_ENDPOINT"),
			AccessKey:    firstNonEmpty(getenv("CONDUCTOR_BACKUP_S3_ACCESS_KEY"), getenv("AWS_ACCESS_KEY_ID")),
			SecretKey:    firstNonEmpty(getenv("CONDUCTOR_BACKUP_S3_SECRET_KEY"), getenv("AWS_SECRET_ACCESS_KEY")),
			SessionToken: firstNonEmpty(getenv("CONDUCTOR_BACKUP_S3_SESSION_TOKEN"), getenv("AWS_SESSION_TOKEN")),
			PathStyle:    isTrue(getenv("CONDUCTOR_BACKUP_S3_PATH_STYLE")),
			Insecure:     isTrue(getenv("CONDUCTOR_BACKUP_S3_INSECURE")),
		},
		Prefix:  getenv("CONDUCTOR_BACKUP_S3_PREFIX"),
		Machine: firstNonEmpty(getenv("CONDUCTOR_MACHINE_ID"), hostname()),
	}
	store, err := Open(cfg)
	if err != nil {
		return nil, false, err
	}
	return store, true, nil
}

// PutSessions writes a uniquely named, write-once snapshot. `at` is passed in rather than
// read from the clock so a caller under shutdown pressure controls the timestamp. The
// old fixed sessions.json manifest is left untouched for legacy restores.
func (s *Store) PutSessions(ctx context.Context, data []byte, at time.Time) error {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	key := s.snapshotKey(at, hex.EncodeToString(nonce[:]))
	// Check first as well as sending If-None-Match: *. AWS enforces the condition; the
	// preflight also avoids replacing an existing key on an S3-compatible store that
	// silently ignores it.
	exists, err := s.s3.Exists(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: %s", ErrExists, key)
	}
	return s.s3.PutIfAbsent(ctx, key, data, "application/json")
}

// GetSessions returns the latest snapshot. Existing buckets with only the old fixed
// sessions.json manifest remain readable. ErrNotFound means this machine has neither.
func (s *Store) GetSessions(ctx context.Context) ([]byte, error) {
	snapshots, err := s.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	if len(snapshots) > 0 {
		return s.s3.Get(ctx, snapshots[len(snapshots)-1])
	}
	return s.s3.Get(ctx, s.manifestKey())
}

// ListSnapshots returns the snapshot keys for this machine, oldest first.
func (s *Store) ListSnapshots(ctx context.Context) ([]string, error) {
	keys, err := s.s3.List(ctx, s.prefix+"/machines/"+s.machine+"/snapshots/")
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

// Location returns the s3 URI of this machine's write-once snapshots, for display.
func (s *Store) Location() string {
	return fmt.Sprintf("s3://%s/%s/machines/%s/snapshots/", s.s3.cfg.Bucket, s.prefix, s.machine)
}

// Describe renders the backup target without secrets.
func (s *Store) Describe() string {
	target := s.s3.cfg.Bucket
	if s.s3.cfg.Endpoint != "" {
		target += " @ " + s.s3.cfg.Endpoint
	}
	return fmt.Sprintf("bucket %s, region %s, machine %s, prefix %s",
		target, s.s3.cfg.Region, s.machine, s.prefix)
}

// Machine is the machine id this store writes under.
func (s *Store) Machine() string { return s.machine }

func (s *Store) manifestKey() string {
	return s.prefix + "/machines/" + s.machine + "/sessions.json"
}

func (s *Store) snapshotKey(at time.Time, nonce string) string {
	return s.prefix + "/machines/" + s.machine + "/snapshots/" +
		at.UTC().Format("20060102T150405Z") + fmt.Sprintf("~%09d-%s.json", at.UTC().Nanosecond(), nonce)
}

// sanitizeMachine keeps a machine id to a safe key segment.
func sanitizeMachine(m string) string {
	m = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, m)
	if m == "" {
		return "unknown-host"
	}
	return m
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-host"
	}
	return h
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func isTrue(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Checkpoints are the one artifact this store carries that contains a conversation, so they
// travel sealed (internal/checkpoint.Seal) and the store refuses an unsealed one. They are
// keyed by checkpoint id rather than by machine, because moving between machines is their
// purpose: a bundle pushed from a laptop is pulled onto a workstation under the same key.
//
//	<prefix>/checkpoints/<id>.json   an index entry, in the clear: id, time, harness, size
//	<prefix>/checkpoints/<id>.ckpt   the sealed bundle, manifest included
//
// The index entry is deliberately not the manifest. A manifest carries the harness's title
// for the conversation, the note, the working directory, the repository, and the machine
// name: no transcript, but plenty about what someone was doing, which is content.

// ErrUnsealed is returned when a caller tries to push a plaintext checkpoint.
var ErrUnsealed = errors.New("refusing to upload an unsealed checkpoint; set CONDUCTOR_CHECKPOINT_KEY")

// PutCheckpoint uploads a sealed bundle and its index entry (checkpoint.IndexEntry).
// Both keys are create-only. A partly uploaded ID is left for explicit repair rather than
// attaching a new bundle or index to an existing object with the same ID.
func (s *Store) PutCheckpoint(ctx context.Context, id string, index, sealed []byte, isSealed func([]byte) bool) error {
	if isSealed != nil && !isSealed(sealed) {
		return ErrUnsealed
	}
	for _, ext := range []string{".ckpt", ".json"} {
		key := s.checkpointKey(id, ext)
		exists, err := s.s3.Exists(ctx, key)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: %s", ErrExists, key)
		}
	}
	if err := s.s3.PutIfAbsent(ctx, s.checkpointKey(id, ".ckpt"), sealed, "application/octet-stream"); err != nil {
		return err
	}
	return s.s3.PutIfAbsent(ctx, s.checkpointKey(id, ".json"), index, "application/json")
}

// GetCheckpoint downloads a sealed bundle. ErrNotFound when there is none.
func (s *Store) GetCheckpoint(ctx context.Context, id string) ([]byte, error) {
	return s.s3.Get(ctx, s.checkpointKey(id, ".ckpt"))
}

// GetCheckpointManifest downloads a checkpoint's index entry. ErrNotFound when there is none.
func (s *Store) GetCheckpointManifest(ctx context.Context, id string) ([]byte, error) {
	return s.s3.Get(ctx, s.checkpointKey(id, ".json"))
}

// ListCheckpoints returns the ids of every checkpoint under the prefix, sorted.
func (s *Store) ListCheckpoints(ctx context.Context) ([]string, error) {
	keys, err := s.s3.List(ctx, s.prefix+"/checkpoints/")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, k := range keys {
		if strings.HasSuffix(k, ".json") {
			ids = append(ids, strings.TrimSuffix(k[strings.LastIndex(k, "/")+1:], ".json"))
		}
	}
	return ids, nil
}

// CheckpointLocation is the s3 URI checkpoints live under, for display.
func (s *Store) CheckpointLocation() string {
	return fmt.Sprintf("s3://%s/%s/checkpoints/", s.s3.cfg.Bucket, s.prefix)
}

func (s *Store) checkpointKey(id, ext string) string {
	return s.prefix + "/checkpoints/" + sanitizeMachine(id) + ext
}
