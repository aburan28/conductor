package storage

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/awscreds"
	"github.com/aburan28/conductor/internal/backup"
	"github.com/aburan28/conductor/internal/backup/s3fake"
)

func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func testAWSEnv(vars map[string]string) awscreds.Env {
	return awscreds.Env{
		Getenv:   envOf(vars),
		ReadFile: os.ReadFile,
		HomeDir:  func() (string, error) { return vars["HOME"], nil },
		Now:      time.Now,
		HTTP:     &http.Client{Timeout: 5 * time.Second},
		Command: func(context.Context, []byte, string, ...string) ([]byte, error) {
			return nil, errors.New("no commands")
		},
		IMDSEndpoint: "http://127.0.0.1:1",
		GOOS:         "linux",
	}
}

func TestSaveLoadRoundTripAndMode(t *testing.T) {
	dir := t.TempDir()
	getenv := envOf(map[string]string{"CONDUCTOR_STATE_DIR": dir})
	in := Settings{
		S3:       S3{Bucket: "b", Region: "eu-west-1", Prefix: "team"},
		Auth:     Auth{Method: AuthProfile, Profile: "dev"},
		Uses:     Uses{Checkpoints: Bool(false)},
		Database: Database{KeepBaseBackups: 3},
	}
	path, err := Save(getenv, in)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "storage.json") {
		t.Errorf("path %s", path)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info.Mode(), err)
	}
	out, ok, err := Load(getenv)
	if err != nil || !ok {
		t.Fatalf("load: %v %v", ok, err)
	}
	if out.Version != Version || out.S3.Bucket != "b" || out.Auth.Profile != "dev" {
		t.Errorf("round trip: %+v", out)
	}
	if !out.Uses.SessionsOn() || out.Uses.CheckpointsOn() || !out.Uses.DatabaseOn() {
		t.Errorf("uses: %+v", out.Uses)
	}
	if out.Database.Keep() != 3 || out.Database.BaseBackupEvery() != 24 || out.Database.ArchiveTimeout() != 60 || !out.Database.SealOn() {
		t.Errorf("database defaults: %+v", out.Database)
	}
}

func TestLoadRefusesNewerVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "storage.json"), []byte(`{"version": 99, "s3": {"bucket": "b"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(envOf(map[string]string{"CONDUCTOR_STATE_DIR": dir})); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate(t *testing.T) {
	ok := Settings{S3: S3{Bucket: "b"}, Auth: Auth{Method: AuthEnvironment}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]Settings{
		"no bucket":        {Auth: Auth{Method: AuthEnvironment}},
		"bad method":       {S3: S3{Bucket: "b"}, Auth: Auth{Method: "magic"}},
		"static no id":     {S3: S3{Bucket: "b"}, Auth: Auth{Method: AuthStatic, Secret: SecretKeychain}},
		"file no secret":   {S3: S3{Bucket: "b"}, Auth: Auth{Method: AuthStatic, AccessKeyID: "A", Secret: SecretFile}},
		"secret where":     {S3: S3{Bucket: "b"}, Auth: Auth{Method: AuthStatic, AccessKeyID: "A", Secret: "drawer"}},
		"plain http":       {S3: S3{Bucket: "b", Endpoint: "http://minio:9000"}, Auth: Auth{Method: AuthEnvironment}},
		"plain http lower": {S3: S3{Bucket: "b", Endpoint: "HTTP://minio:9000"}, Auth: Auth{Method: AuthEnvironment}},
	}
	for name, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	vars := map[string]string{"CONDUCTOR_STATE_DIR": dir}
	if _, err := Save(envOf(vars), Settings{S3: S3{Bucket: "from-file"}, Auth: Auth{Method: AuthProfile}}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(envOf(vars))
	if err != nil || r.Source != SourceFile || r.Settings.S3.Bucket != "from-file" || !r.Configured() {
		t.Fatalf("file: %+v %v", r, err)
	}

	vars["CONDUCTOR_BACKUP_S3_BUCKET"] = "from-env"
	vars["CONDUCTOR_BACKUP_S3_ACCESS_KEY"] = "AKIDENV"
	vars["CONDUCTOR_BACKUP_S3_SECRET_KEY"] = "envsecret"
	r, _ = Resolve(envOf(vars))
	if r.Source != SourceEnv || r.Settings.S3.Bucket != "from-env" || r.Settings.Auth.Method != AuthStatic ||
		r.Settings.Auth.Secret != SecretFile || r.Settings.Auth.SecretAccessKey != "envsecret" {
		t.Fatalf("env: %+v", r)
	}

	vars["CONDUCTOR_BACKUP"] = "off"
	r, _ = Resolve(envOf(vars))
	if !r.Off || r.Configured() {
		t.Fatalf("off: %+v", r)
	}

	r, _ = Resolve(envOf(map[string]string{"CONDUCTOR_STATE_DIR": t.TempDir()}))
	if r.Source != SourceNone || r.Configured() {
		t.Fatalf("none: %+v", r)
	}
}

func TestEnvWithoutKeysUsesEnvironmentChain(t *testing.T) {
	r, _ := Resolve(envOf(map[string]string{"CONDUCTOR_STATE_DIR": t.TempDir(), "CONDUCTOR_BACKUP_S3_BUCKET": "b"}))
	if r.Settings.Auth.Method != AuthEnvironment {
		t.Fatalf("method %q", r.Settings.Auth.Method)
	}
	env := testAWSEnv(map[string]string{"AWS_ACCESS_KEY_ID": "AKIDAWS", "AWS_SECRET_ACCESS_KEY": "s", "AWS_REGION": "ap-south-1"})
	cfg, err := r.S3Config(env)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil || c.AccessKey != "AKIDAWS" || cfg.Region != "ap-south-1" {
		t.Fatalf("creds %+v region %s err %v", c, cfg.Region, err)
	}
}

func TestProbeAgainstFakeBucket(t *testing.T) {
	fake := s3fake.New("team-bucket")
	defer fake.Close()
	dir := t.TempDir()
	vars := map[string]string{"CONDUCTOR_STATE_DIR": dir}
	s := Settings{
		S3:   S3{Bucket: "team-bucket", Endpoint: fake.URL, PathStyle: true, Insecure: true, Prefix: "acme"},
		Auth: Auth{Method: AuthStatic, AccessKeyID: "AKIDFILE", Secret: SecretFile, SecretAccessKey: "filesecret"},
	}
	if _, err := Save(envOf(vars), s); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(envOf(vars))
	if err != nil {
		t.Fatal(err)
	}
	env := testAWSEnv(vars)
	res := Test(context.Background(), env, r)
	if !res.OK {
		t.Fatalf("probe failed: %+v", res)
	}
	var names []string
	for _, st := range res.Steps {
		names = append(names, st.Name)
	}
	if strings.Join(names, ",") != "credentials,put,get,list,delete" {
		t.Errorf("steps %v", names)
	}
	if res.Location != "s3://team-bucket/acme" || res.Credentials != "access key (file)" {
		t.Errorf("result %+v", res)
	}
	if keys := fake.Keys(""); len(keys) != 0 {
		t.Errorf("probe left %v behind", keys)
	}
	for _, ak := range fake.AccessKeys {
		if ak != "AKIDFILE" {
			t.Fatalf("a request was signed with %q", ak)
		}
	}

	// A failing step is named, and the probe still stops cleanly.
	fake.FailNext = 1
	res = Test(context.Background(), env, r)
	if res.OK || !strings.HasPrefix(res.Error, "put:") {
		t.Fatalf("injected failure: %+v", res)
	}
}

func TestOpenStoreHonoursUses(t *testing.T) {
	fake := s3fake.New("b")
	defer fake.Close()
	dir := t.TempDir()
	vars := map[string]string{"CONDUCTOR_STATE_DIR": dir, "CONDUCTOR_MACHINE_ID": "mac-1"}
	if _, err := Save(envOf(vars), Settings{
		S3:   S3{Bucket: "b", Endpoint: fake.URL, PathStyle: true, Insecure: true},
		Auth: Auth{Method: AuthStatic, AccessKeyID: "A", Secret: SecretFile, SecretAccessKey: "S"},
		Uses: Uses{Checkpoints: Bool(false)},
	}); err != nil {
		t.Fatal(err)
	}
	env := testAWSEnv(vars)
	if _, ok, err := OpenStore(env, UseCheckpoints); ok || err != nil {
		t.Fatalf("checkpoints are off but the store opened: %v %v", ok, err)
	}
	store, ok, err := OpenStore(env, UseSessions)
	if !ok || err != nil {
		t.Fatalf("sessions: %v %v", ok, err)
	}
	if err := store.PutSessions(context.Background(), []byte(`{"records":[]}`), time.Unix(0, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Object("conductor/machines/mac-1/sessions.json"); !ok {
		t.Errorf("manifest not written where expected: %v", fake.Keys(""))
	}
}

func TestSealPassphrase(t *testing.T) {
	env := testAWSEnv(map[string]string{"CONDUCTOR_CHECKPOINT_KEY": "pass"})
	if got, err := SealPassphrase(context.Background(), env); err != nil || got != "pass" {
		t.Errorf("env passphrase = %q, %v", got, err)
	}
	env = testAWSEnv(nil)
	env.GOOS = "darwin"
	env.Command = func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		if args[0] == "find-generic-password" && args[2] == awscreds.KeychainSealService {
			return []byte("from-keychain\n"), nil
		}
		return nil, errors.New("exit status 44")
	}
	if got, err := SealPassphrase(context.Background(), env); err != nil || got != "from-keychain" {
		t.Errorf("keychain passphrase = %q, %v", got, err)
	}
}

// A Keychain read that fails for any reason other than "not found" must reach the caller. It
// must not become an empty passphrase, which the backup code reads as "no seal configured" and
// uploads in the clear. Off macOS, and with no item, there is no seal configured.
func TestSealPassphraseKeychainErrors(t *testing.T) {
	ctx := context.Background()
	keychain := func(out []byte, err error) awscreds.Env {
		env := testAWSEnv(nil)
		env.GOOS = "darwin"
		env.Command = func(context.Context, []byte, string, ...string) ([]byte, error) { return out, err }
		return env
	}
	if pass, err := SealPassphrase(ctx, keychain(nil, errors.New("exit status 128: User interaction is not allowed."))); err == nil || pass != "" {
		t.Fatalf("a failed Keychain read = %q, %v; want an error and no passphrase", pass, err)
	}
	if pass, err := SealPassphrase(ctx, keychain([]byte("\n"), nil)); err == nil || pass != "" {
		t.Fatalf("an empty Keychain item = %q, %v; want an error", pass, err)
	}
	if pass, err := SealPassphrase(ctx, keychain(nil, errors.New("exit status 44: could not be found"))); err != nil || pass != "" {
		t.Fatalf("a missing Keychain item = %q, %v; want no seal and no error", pass, err)
	}
	if pass, err := SealPassphrase(ctx, testAWSEnv(nil)); err != nil || pass != "" {
		t.Fatalf("off macOS = %q, %v; want no seal and no error", pass, err)
	}
}

// The CONDUCTOR_BACKUP_S3_ACCESS_KEY path must sign with the same secret and session token as
// internal/backup's FromEnv: the AWS_* values when the CONDUCTOR_BACKUP_S3_* ones are unset,
// and the explicit values when they are set.
func TestAccessKeyVariablesFallBackToAWSSecretAndToken(t *testing.T) {
	vars := map[string]string{
		"CONDUCTOR_BACKUP_S3_BUCKET": "b", "CONDUCTOR_BACKUP_S3_ACCESS_KEY": "AKID",
		"AWS_SECRET_ACCESS_KEY": "awssecret", "AWS_SESSION_TOKEN": "awstok",
	}
	sign := func() backup.Credentials {
		t.Helper()
		r, err := Resolve(envOf(vars))
		if err != nil {
			t.Fatal(err)
		}
		src, err := r.Credentials(testAWSEnv(vars))
		if err != nil {
			t.Fatal(err)
		}
		c, err := src.Retrieve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := sign(); c.AccessKey != "AKID" || c.SecretKey != "awssecret" || c.SessionToken != "awstok" {
		t.Fatalf("AWS_* fallback: %+v", c)
	}
	vars["CONDUCTOR_BACKUP_S3_SECRET_KEY"] = "explicit"
	vars["CONDUCTOR_BACKUP_S3_SESSION_TOKEN"] = "explicit-token"
	if c := sign(); c.SecretKey != "explicit" || c.SessionToken != "explicit-token" {
		t.Fatalf("explicit variables must win: %+v", c)
	}
}
