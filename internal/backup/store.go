package backup

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Store is the off-host home for one machine's resume records. It namespaces objects by
// machine under a prefix, keeps a current manifest plus timestamped snapshots, and hands the
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

// PutSessions writes the current manifest and a timestamped snapshot. The manifest is what a
// restore reads; the snapshots are history, so a machine that overwrote a good manifest with a
// bad one can still be recovered. `at` is passed in rather than read from the clock so a
// caller under shutdown pressure controls the timestamp (and tests are deterministic).
func (s *Store) PutSessions(ctx context.Context, data []byte, at time.Time) error {
	if err := s.s3.Put(ctx, s.manifestKey(), data, "application/json"); err != nil {
		return err
	}
	return s.s3.Put(ctx, s.snapshotKey(at), data, "application/json")
}

// GetSessions returns the current manifest, or ErrNotFound when this machine has none.
func (s *Store) GetSessions(ctx context.Context) ([]byte, error) {
	return s.s3.Get(ctx, s.manifestKey())
}

// ListSnapshots returns the snapshot keys for this machine, oldest first.
func (s *Store) ListSnapshots(ctx context.Context) ([]string, error) {
	return s.s3.List(ctx, s.prefix+"/machines/"+s.machine+"/snapshots/")
}

// Location returns the s3 URI of this machine's manifest, for display.
func (s *Store) Location() string {
	return fmt.Sprintf("s3://%s/%s", s.s3.cfg.Bucket, s.manifestKey())
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

func (s *Store) snapshotKey(at time.Time) string {
	return s.prefix + "/machines/" + s.machine + "/snapshots/" + at.UTC().Format("20060102T150405Z") + ".json"
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
