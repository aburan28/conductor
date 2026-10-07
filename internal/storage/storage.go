// Package storage is the bucket Conductor keeps off-machine state in: the session resume
// records, sealed checkpoints, and the control-plane database's WAL archive and base
// backups. Its settings live in storage.json beside the CLI's other state; the macOS app's
// Settings window writes the same file. docs/STORAGE.md is the contract.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aburan28/conductor/internal/awscreds"
	"github.com/aburan28/conductor/internal/backup"
)

// Version is the settings file format this build writes.
const Version = 1

// Auth methods.
const (
	AuthStatic      = "static"
	AuthProfile     = "profile"
	AuthEnvironment = "environment"
)

// Where a static secret is kept.
const (
	SecretKeychain = "keychain"
	SecretFile     = "file"
)

// Settings is storage.json.
type Settings struct {
	Version  int      `json:"version"`
	S3       S3       `json:"s3"`
	Auth     Auth     `json:"auth"`
	Uses     Uses     `json:"uses"`
	Database Database `json:"database"`
}

// S3 locates the bucket.
type S3 struct {
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	Endpoint  string `json:"endpoint"`
	PathStyle bool   `json:"path_style"`
	Insecure  bool   `json:"insecure"`
	Prefix    string `json:"prefix"`
}

// Auth says how to sign in to the bucket.
type Auth struct {
	Method          string `json:"method"`
	AccessKeyID     string `json:"access_key_id"`
	Secret          string `json:"secret"`
	SecretAccessKey string `json:"secret_access_key"`
	Profile         string `json:"profile"`
}

// Uses says which kinds of state go to the bucket. A missing field means true: a bucket is
// set up to be used.
type Uses struct {
	Sessions    *bool `json:"sessions,omitempty"`
	Checkpoints *bool `json:"checkpoints,omitempty"`
	Database    *bool `json:"database,omitempty"`
}

// Database configures the Postgres archive.
type Database struct {
	ArchiveWAL            *bool `json:"archive_wal,omitempty"`
	ArchiveTimeoutSeconds int   `json:"archive_timeout_seconds,omitempty"`
	BaseBackupEveryHours  int   `json:"base_backup_every_hours,omitempty"`
	KeepBaseBackups       int   `json:"keep_base_backups,omitempty"`
	Seal                  *bool `json:"seal,omitempty"`
}

func on(b *bool) bool { return b == nil || *b }

// Bool returns a pointer, for building Settings.
func Bool(v bool) *bool { return &v }

// SessionsOn, CheckpointsOn and DatabaseOn report the effective Uses.
func (u Uses) SessionsOn() bool    { return on(u.Sessions) }
func (u Uses) CheckpointsOn() bool { return on(u.Checkpoints) }
func (u Uses) DatabaseOn() bool    { return on(u.Database) }

// Effective database settings, with defaults applied.
func (d Database) ArchiveWALOn() bool { return on(d.ArchiveWAL) }
func (d Database) SealOn() bool       { return on(d.Seal) }
func (d Database) ArchiveTimeout() int {
	if d.ArchiveTimeoutSeconds > 0 {
		return d.ArchiveTimeoutSeconds
	}
	return 60
}
func (d Database) BaseBackupEvery() int {
	if d.BaseBackupEveryHours > 0 {
		return d.BaseBackupEveryHours
	}
	return 24
}
func (d Database) Keep() int {
	if d.KeepBaseBackups > 0 {
		return d.KeepBaseBackups
	}
	return 7
}

// Validate checks a settings value for what can be checked without the network.
func (s Settings) Validate() error {
	if strings.TrimSpace(s.S3.Bucket) == "" {
		return errors.New("a bucket is required")
	}
	if s.S3.Endpoint != "" && strings.HasPrefix(strings.ToLower(s.S3.Endpoint), "http://") && !s.S3.Insecure {
		return errors.New("the endpoint is plain http; allow it explicitly (insecure) or use https")
	}
	switch s.Auth.Method {
	case AuthStatic:
		if s.Auth.AccessKeyID == "" {
			return errors.New("the access key ID is required for the static method")
		}
		switch s.Auth.Secret {
		case SecretKeychain:
		case SecretFile:
			if s.Auth.SecretAccessKey == "" {
				return errors.New("the secret access key is required when it is kept in the file")
			}
		default:
			return fmt.Errorf("auth.secret must be %q or %q", SecretKeychain, SecretFile)
		}
	case AuthProfile, AuthEnvironment:
	default:
		return fmt.Errorf("auth.method must be %s, %s or %s", AuthStatic, AuthProfile, AuthEnvironment)
	}
	return nil
}

// Path is where storage.json lives: $CONDUCTOR_STATE_DIR/storage.json, else
// ~/.conductor/storage.json.
func Path(getenv func(string) string) (string, error) {
	if v := getenv("CONDUCTOR_STATE_DIR"); v != "" {
		return filepath.Join(v, "storage.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "storage.json"), nil
}

// Load reads storage.json. A missing file is (zero, false, nil).
func Load(getenv func(string) string) (Settings, bool, error) {
	path, err := Path(getenv)
	if err != nil {
		return Settings{}, false, err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	var s Settings
	if err := json.Unmarshal(body, &s); err != nil {
		return Settings{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if s.Version > Version {
		return Settings{}, false, fmt.Errorf("%s was written by a newer Conductor (version %d); update this one", path, s.Version)
	}
	return s, true, nil
}

// Save writes storage.json atomically with mode 0600.
func Save(getenv func(string) string, s Settings) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	s.Version = Version
	path, err := Path(getenv)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".storage-*.json")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

// Remove deletes storage.json.
func Remove(getenv func(string) string) (string, error) {
	path, err := Path(getenv)
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return path, nil
}

// Source values for Resolved.Source.
const (
	SourceNone = "none"
	SourceEnv  = "env"
	SourceFile = "file"
)

// Resolved is the storage configuration in force, after precedence.
type Resolved struct {
	// Source is where it came from: env (the CONDUCTOR_BACKUP_S3_* variables), file
	// (storage.json), or none.
	Source string
	// Off is true when CONDUCTOR_BACKUP turns uploads off.
	Off      bool
	Settings Settings
	// Path is storage.json's location, whether or not it exists.
	Path string
}

// Configured reports whether a bucket is in force.
func (r Resolved) Configured() bool { return !r.Off && r.Source != SourceNone }

// Resolve applies the precedence in docs/STORAGE.md: CONDUCTOR_BACKUP=off, then the
// CONDUCTOR_BACKUP_S3_* variables, then storage.json.
func Resolve(getenv func(string) string) (Resolved, error) {
	path, _ := Path(getenv)
	r := Resolved{Source: SourceNone, Path: path}
	switch strings.ToLower(getenv("CONDUCTOR_BACKUP")) {
	case "off", "0", "false", "no":
		r.Off = true
		return r, nil
	}
	if bucket := getenv("CONDUCTOR_BACKUP_S3_BUCKET"); bucket != "" {
		r.Source = SourceEnv
		r.Settings = Settings{
			Version: Version,
			S3: S3{
				Bucket:    bucket,
				Region:    getenv("CONDUCTOR_BACKUP_S3_REGION"),
				Endpoint:  getenv("CONDUCTOR_BACKUP_S3_ENDPOINT"),
				PathStyle: isTrue(getenv("CONDUCTOR_BACKUP_S3_PATH_STYLE")),
				Insecure:  isTrue(getenv("CONDUCTOR_BACKUP_S3_INSECURE")),
				Prefix:    getenv("CONDUCTOR_BACKUP_S3_PREFIX"),
			},
			Auth: Auth{Method: AuthEnvironment},
		}
		// The variables have always taken an explicit key pair before the AWS_* ones.
		if ak := getenv("CONDUCTOR_BACKUP_S3_ACCESS_KEY"); ak != "" {
			r.Settings.Auth = Auth{Method: AuthStatic, AccessKeyID: ak, Secret: SecretFile,
				SecretAccessKey: getenv("CONDUCTOR_BACKUP_S3_SECRET_KEY")}
		}
		return r, nil
	}
	s, ok, err := Load(getenv)
	if err != nil {
		return r, err
	}
	if !ok {
		return r, nil
	}
	if err := s.Validate(); err != nil {
		return r, fmt.Errorf("%s: %w", path, err)
	}
	r.Source, r.Settings = SourceFile, s
	return r, nil
}

// Region is the bucket's region: the setting, then the profile's region, then AWS_REGION,
// then us-east-1.
func (r Resolved) Region(env awscreds.Env) string {
	if r.Settings.S3.Region != "" {
		return r.Settings.S3.Region
	}
	if r.Settings.Auth.Method == AuthProfile {
		if region := awscreds.ProfileRegion(env, r.Settings.Auth.Profile); region != "" {
			return region
		}
	}
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
		if v := env.Getenv(k); v != "" {
			return v
		}
	}
	return "us-east-1"
}

// Credentials builds the credential source for the configured sign-in method.
func (r Resolved) Credentials(env awscreds.Env) (backup.CredentialSource, error) {
	a := r.Settings.Auth
	switch a.Method {
	case AuthStatic:
		switch {
		case a.Secret == SecretFile:
			sessionToken := ""
			if r.Source == SourceEnv {
				sessionToken = env.Getenv("CONDUCTOR_BACKUP_S3_SESSION_TOKEN")
			}
			return awscreds.Static{Creds: backup.Credentials{AccessKey: a.AccessKeyID, SecretKey: a.SecretAccessKey,
				SessionToken: sessionToken, Source: "access key (" + r.Source + ")"}}, nil
		default:
			return awscreds.StaticKeychain(env, a.AccessKeyID), nil
		}
	case AuthProfile:
		return awscreds.Profile(env, a.Profile), nil
	default:
		return awscreds.Environment(env, r.Region(env)), nil
	}
}

// S3Config is the client configuration for the bucket.
func (r Resolved) S3Config(env awscreds.Env) (backup.S3Config, error) {
	creds, err := r.Credentials(env)
	if err != nil {
		return backup.S3Config{}, err
	}
	return backup.S3Config{
		Bucket:      r.Settings.S3.Bucket,
		Region:      r.Region(env),
		Credentials: creds,
		Endpoint:    r.Settings.S3.Endpoint,
		PathStyle:   r.Settings.S3.PathStyle,
		Insecure:    r.Settings.S3.Insecure,
	}, nil
}

// Uses reports whether the bucket is used for one kind of state.
func (r Resolved) Uses(use string) bool {
	switch use {
	case UseSessions:
		return r.Settings.Uses.SessionsOn()
	case UseCheckpoints:
		return r.Settings.Uses.CheckpointsOn()
	case UseDatabase:
		return r.Settings.Uses.DatabaseOn()
	}
	return false
}

// Prefix is the key prefix, default "conductor".
func (r Resolved) Prefix() string {
	if r.Settings.S3.Prefix == "" {
		return "conductor"
	}
	return r.Settings.S3.Prefix
}

// Uses of the bucket, for OpenStore.
const (
	UseSessions    = "sessions"
	UseCheckpoints = "checkpoints"
	UseDatabase    = "database"
)

// OpenStore opens the backup store for this machine for one use, or reports enabled=false
// when no bucket is configured or that use is turned off.
func OpenStore(env awscreds.Env, use string) (*backup.Store, bool, error) {
	r, err := Resolve(env.Getenv)
	if err != nil {
		return nil, false, err
	}
	if !r.Configured() || !r.Uses(use) {
		return nil, false, nil
	}
	cfg, err := r.S3Config(env)
	if err != nil {
		return nil, false, err
	}
	store, err := backup.Open(backup.Config{
		S3Config: cfg,
		Prefix:   r.Prefix(),
		Machine:  firstNonEmpty(env.Getenv("CONDUCTOR_MACHINE_ID"), hostname()),
	})
	if err != nil {
		return nil, false, err
	}
	return store, true, nil
}

// SealPassphrase is the passphrase sealed uploads use: CONDUCTOR_CHECKPOINT_KEY, else on
// macOS the Keychain item dev.conductor.seal/default. Empty when neither is set.
func SealPassphrase(ctx context.Context, env awscreds.Env) string {
	if v := env.Getenv("CONDUCTOR_CHECKPOINT_KEY"); v != "" {
		return v
	}
	v, err := awscreds.KeychainGet(ctx, env, awscreds.KeychainSealService, awscreds.KeychainSealAccount)
	if err != nil {
		return ""
	}
	return v
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
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
