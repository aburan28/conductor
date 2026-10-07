package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/awscreds"
	"github.com/aburan28/conductor/internal/storage"
)

// The bucket Conductor keeps off-machine state in. docs/STORAGE.md is the contract; the
// macOS app's Settings → Storage pane drives these same commands.

func cmdStorage(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return storageShow(ctx, nil)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show":
		return storageShow(ctx, rest)
	case "set":
		return storageSet(ctx, rest, os.Stdin)
	case "test":
		return storageTest(ctx, rest)
	case "unset":
		return storageUnset(ctx, rest)
	case "profiles":
		return storageProfiles(rest)
	default:
		return fmt.Errorf("unknown storage subcommand %q (show, set, test, unset, profiles)", sub)
	}
}

// storageView is `conductor storage show --json`.
type storageView struct {
	Configured bool            `json:"configured"`
	Off        bool            `json:"off,omitempty"`
	Path       string          `json:"path"`
	Source     string          `json:"source"`
	S3         storage.S3      `json:"s3"`
	Auth       storage.Auth    `json:"auth"`
	Uses       map[string]bool `json:"uses"`
	Database   storageDBView   `json:"database"`
	Region     string          `json:"effective_region,omitempty"`
	Describe   string          `json:"auth_description,omitempty"`
}

type storageDBView struct {
	ArchiveWAL            bool `json:"archive_wal"`
	ArchiveTimeoutSeconds int  `json:"archive_timeout_seconds"`
	BaseBackupEveryHours  int  `json:"base_backup_every_hours"`
	KeepBaseBackups       int  `json:"keep_base_backups"`
	Seal                  bool `json:"seal"`
}

func viewOf(r storage.Resolved, env awscreds.Env) storageView {
	s := r.Settings
	v := storageView{
		Configured: r.Configured(), Off: r.Off, Path: r.Path, Source: r.Source,
		S3: s.S3, Auth: s.Auth.Redacted(),
		Uses: map[string]bool{"sessions": s.Uses.SessionsOn(), "checkpoints": s.Uses.CheckpointsOn(), "database": s.Uses.DatabaseOn()},
		Database: storageDBView{
			ArchiveWAL: s.Database.ArchiveWALOn(), ArchiveTimeoutSeconds: s.Database.ArchiveTimeout(),
			BaseBackupEveryHours: s.Database.BaseBackupEvery(), KeepBaseBackups: s.Database.Keep(), Seal: s.Database.SealOn(),
		},
	}
	if r.Configured() {
		v.Region = r.Region(env)
		v.Describe = r.DescribeAuth()
		if v.S3.Prefix == "" {
			v.S3.Prefix = r.Prefix()
		}
	}
	return v
}

func storageShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("storage show", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	env := awscreds.Default()
	r, err := storage.Resolve(env.Getenv)
	if err != nil {
		return err
	}
	v := viewOf(r, env)
	if *asJSON {
		return emit(v)
	}
	switch {
	case r.Off:
		fmt.Println("Storage: off (CONDUCTOR_BACKUP is set to off).")
		return nil
	case !r.Configured():
		fmt.Println("Storage: no bucket configured.")
		fmt.Println("\nSet one up:")
		fmt.Println("  conductor storage set --bucket my-team-conductor --region us-east-1 --auth profile --profile default")
		fmt.Println("  conductor storage test")
		fmt.Printf("\nSettings file: %s\n", r.Path)
		return nil
	}
	from := "settings file " + r.Path
	if r.Source == storage.SourceEnv {
		from = "CONDUCTOR_BACKUP_S3_* environment variables (they override " + r.Path + ")"
	}
	s := r.Settings
	fmt.Printf("Bucket      s3://%s/%s  (%s)\n", s.S3.Bucket, r.Prefix(), v.Region)
	if s.S3.Endpoint != "" {
		style := "virtual-hosted"
		if s.S3.PathStyle {
			style = "path-style"
		}
		fmt.Printf("Endpoint    %s (%s)\n", s.S3.Endpoint, style)
	}
	fmt.Printf("Sign-in     %s\n", r.DescribeAuth())
	fmt.Printf("Holds       %s\n", onList(map[string]bool{"sessions": v.Uses["sessions"], "checkpoints": v.Uses["checkpoints"], "database": v.Uses["database"]}))
	if v.Uses["database"] {
		d := v.Database
		seal := "sealed"
		if !d.Seal {
			seal = "not sealed"
		}
		fmt.Printf("Database    WAL archive %s (at least every %ds), base backup every %dh, keep %d, %s\n",
			onOff(d.ArchiveWAL), d.ArchiveTimeoutSeconds, d.BaseBackupEveryHours, d.KeepBaseBackups, seal)
	}
	fmt.Printf("From        %s\n", from)
	return nil
}

func onList(m map[string]bool) string {
	var on []string
	for _, k := range []string{"sessions", "checkpoints", "database"} {
		if m[k] {
			on = append(on, k)
		}
	}
	if len(on) == 0 {
		return "nothing (every use is turned off)"
	}
	return strings.Join(on, ", ")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// optBool is a boolean flag that remembers whether it was given.
type optBool struct {
	set bool
	val bool
}

func (o *optBool) String() string { return strconv.FormatBool(o.val) }
func (o *optBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	o.set, o.val = true, v
	return nil
}
func (o *optBool) IsBoolFlag() bool { return true }

func storageSet(ctx context.Context, args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("storage set", flag.ContinueOnError)
	bucket := fs.String("bucket", "", "bucket name")
	region := fs.String("region", "", "bucket region (default: the profile's region, AWS_REGION, or us-east-1)")
	endpoint := fs.String("endpoint", "", "S3-compatible endpoint URL (MinIO, R2, …); empty for AWS")
	prefix := fs.String("prefix", "", "key prefix (default conductor)")
	var pathStyle, insecure, sessions, checkpoints, database, archiveWAL, seal optBool
	fs.Var(&pathStyle, "path-style", "address the bucket in the URL path (most non-AWS stores)")
	fs.Var(&insecure, "insecure", "allow a plain http:// endpoint")
	fs.Var(&sessions, "sessions", "keep session resume records in the bucket")
	fs.Var(&checkpoints, "checkpoints", "keep sealed checkpoints in the bucket")
	fs.Var(&database, "database", "archive the control-plane database to the bucket")
	fs.Var(&archiveWAL, "archive-wal", "archive each WAL segment as it is written")
	fs.Var(&seal, "seal", "encrypt database backups before upload")
	archiveTimeout := fs.Int("archive-timeout", 0, "archive at least every N seconds")
	baseEvery := fs.Int("base-backup-every", 0, "take a base backup every N hours")
	keep := fs.Int("keep", 0, "keep the newest N base backups")
	auth := fs.String("auth", "", "sign-in method: static, profile, or environment")
	accessKeyID := fs.String("access-key-id", "", "access key ID (static)")
	secretFrom := fs.String("secret-from", "", "static: stdin (read the secret now) or keychain (already stored there)")
	secretStore := fs.String("secret-store", "", "static: where to keep a secret read from stdin, keychain (macOS default) or file")
	profile := fs.String("profile", "", "AWS profile name (profile; empty means AWS_PROFILE, then default)")
	asJSON := fs.Bool("json", false, "print the resulting settings as JSON")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor storage set — choose the bucket and how to sign in to it

  conductor storage set --bucket B --region us-east-1 --auth profile --profile dev
  conductor storage set --bucket B --auth static --access-key-id AKIA… --secret-from stdin < secret.txt
  conductor storage set --bucket B --endpoint https://minio.lan:9000 --path-style --auth static …
  conductor storage set --auth environment          (instance role, ECS, web identity, AWS_*)
  conductor storage set --database=false            (change one setting, keep the rest)

Settings are merged into the existing file. See docs/STORAGE.md.

`)
		fs.PrintDefaults()
	}
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	env := awscreds.Default()
	s, existed, err := storage.Load(env.Getenv)
	if err != nil {
		return err
	}
	if !existed {
		s = storage.Settings{Auth: storage.Auth{Method: storage.AuthEnvironment}}
	}
	if given["bucket"] {
		s.S3.Bucket = strings.TrimSpace(*bucket)
	}
	if given["region"] {
		s.S3.Region = strings.TrimSpace(*region)
	}
	if given["endpoint"] {
		s.S3.Endpoint = strings.TrimSpace(*endpoint)
	}
	if given["prefix"] {
		s.S3.Prefix = strings.Trim(strings.TrimSpace(*prefix), "/")
	}
	if pathStyle.set {
		s.S3.PathStyle = pathStyle.val
	}
	if insecure.set {
		s.S3.Insecure = insecure.val
	}
	if sessions.set {
		s.Uses.Sessions = storage.Bool(sessions.val)
	}
	if checkpoints.set {
		s.Uses.Checkpoints = storage.Bool(checkpoints.val)
	}
	if database.set {
		s.Uses.Database = storage.Bool(database.val)
	}
	if archiveWAL.set {
		s.Database.ArchiveWAL = storage.Bool(archiveWAL.val)
	}
	if seal.set {
		s.Database.Seal = storage.Bool(seal.val)
	}
	if given["archive-timeout"] {
		s.Database.ArchiveTimeoutSeconds = *archiveTimeout
	}
	if given["base-backup-every"] {
		s.Database.BaseBackupEveryHours = *baseEvery
	}
	if given["keep"] {
		s.Database.KeepBaseBackups = *keep
	}

	if given["auth"] {
		switch *auth {
		case storage.AuthStatic, storage.AuthProfile, storage.AuthEnvironment:
		default:
			return fmt.Errorf("--auth must be static, profile, or environment")
		}
		if *auth != s.Auth.Method {
			s.Auth = storage.Auth{Method: *auth}
		}
	}
	switch s.Auth.Method {
	case storage.AuthProfile:
		if given["profile"] {
			s.Auth.Profile = strings.TrimSpace(*profile)
		}
	case storage.AuthStatic:
		if given["access-key-id"] {
			id := strings.TrimSpace(*accessKeyID)
			if id != s.Auth.AccessKeyID {
				s.Auth.SecretAccessKey = ""
			}
			s.Auth.AccessKeyID = id
		}
		if err := applyStaticSecret(ctx, env, &s.Auth, *secretFrom, *secretStore, stdin); err != nil {
			return err
		}
	}
	for _, f := range []string{"access-key-id", "secret-from", "secret-store"} {
		if given[f] && s.Auth.Method != storage.AuthStatic {
			return fmt.Errorf("--%s applies only to --auth static", f)
		}
	}
	if given["profile"] && s.Auth.Method != storage.AuthProfile {
		return errors.New("--profile applies only to --auth profile")
	}

	path, err := storage.Save(env.Getenv, s)
	if err != nil {
		return err
	}
	if *asJSON {
		r, err := storage.Resolve(env.Getenv)
		if err != nil {
			return err
		}
		return emit(viewOf(r, env))
	}
	fmt.Printf("Saved %s.\n", path)
	if env.Getenv("CONDUCTOR_BACKUP_S3_BUCKET") != "" {
		fmt.Println("Note: CONDUCTOR_BACKUP_S3_BUCKET is set in this environment and overrides the file.")
	}
	fmt.Println("Check it with: conductor storage test")
	return nil
}

// applyStaticSecret puts the secret where auth.secret says, or checks it is already there.
func applyStaticSecret(ctx context.Context, env awscreds.Env, a *storage.Auth, from, store string, stdin io.Reader) error {
	if a.AccessKeyID == "" {
		return errors.New("--auth static needs --access-key-id")
	}
	if store == "" {
		store = a.Secret
	}
	if store == "" {
		store = storage.SecretFile
		if env.GOOS == "darwin" {
			store = storage.SecretKeychain
		}
	}
	if store != storage.SecretKeychain && store != storage.SecretFile {
		return errors.New("--secret-store must be keychain or file")
	}
	switch from {
	case "stdin":
		secret, err := readSecret(stdin)
		if err != nil {
			return err
		}
		if store == storage.SecretKeychain {
			if err := awscreds.KeychainSet(ctx, env, awscreds.KeychainS3Service, a.AccessKeyID, secret); err != nil {
				return err
			}
			// `security -i` can report success for a command it did not carry out; read the
			// item back so a missing secret is found now rather than at the first upload.
			if got, err := awscreds.KeychainGet(ctx, env, awscreds.KeychainS3Service, a.AccessKeyID); err != nil || got != secret {
				return fmt.Errorf("the secret did not reach the Keychain (%v); use --secret-store file, or set it from the macOS app", err)
			}
			a.SecretAccessKey = ""
		} else {
			a.SecretAccessKey = secret
		}
		a.Secret = store
	case "keychain":
		if _, err := awscreds.KeychainGet(ctx, env, awscreds.KeychainS3Service, a.AccessKeyID); err != nil {
			return fmt.Errorf("--secret-from keychain: %w (service %s, account %s)", err, awscreds.KeychainS3Service, a.AccessKeyID)
		}
		a.Secret, a.SecretAccessKey = storage.SecretKeychain, ""
	case "":
		// Keep what is there; the static method needs a secret somewhere.
		if a.Secret == "" {
			a.Secret = store
		}
		if a.Secret == storage.SecretFile && a.SecretAccessKey == "" {
			return errors.New("no secret for this access key: pass --secret-from stdin")
		}
	default:
		return errors.New("--secret-from must be stdin or keychain")
	}
	return nil
}

func readSecret(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("--secret-from stdin: nothing was read from standard input")
	}
	return line, nil
}

func storageTest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("storage test", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	env := awscreds.Default()
	r, err := storage.Resolve(env.Getenv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res := storage.Test(ctx, env, r)
	if *asJSON {
		if err := emit(res); err != nil {
			return err
		}
	} else {
		fmt.Printf("Testing %s\n", res.Location)
		if res.Credentials != "" {
			fmt.Printf("  signing in with %s\n", res.Credentials)
		}
		for _, st := range res.Steps {
			mark := "ok  "
			if !st.OK {
				mark = "FAIL"
			}
			fmt.Printf("  %s %-11s %5d ms", mark, st.Name, st.MS)
			if st.Error != "" {
				fmt.Printf("  %s", st.Error)
			}
			fmt.Println()
		}
		if res.OK {
			fmt.Println("The bucket is reachable and writable.")
		}
	}
	if !res.OK {
		return fmt.Errorf("storage test failed: %s", res.Error)
	}
	return nil
}

func storageUnset(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("storage unset", flag.ContinueOnError)
	keepSecret := fs.Bool("keep-secret", false, "leave the access key's secret in the Keychain")
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	env := awscreds.Default()
	s, existed, err := storage.Load(env.Getenv)
	if err != nil {
		return err
	}
	path, err := storage.Remove(env.Getenv)
	if err != nil {
		return err
	}
	if existed && !*keepSecret && s.Auth.Method == storage.AuthStatic && s.Auth.Secret == storage.SecretKeychain && env.GOOS == "darwin" {
		if err := awscreds.KeychainDelete(ctx, env, awscreds.KeychainS3Service, s.Auth.AccessKeyID); err != nil {
			fmt.Fprintf(os.Stderr, "conductor: %v\n", err)
		}
	}
	if existed {
		fmt.Printf("Removed %s. Nothing more goes to the bucket; what is already there stays.\n", path)
	} else {
		fmt.Println("No storage settings to remove.")
	}
	return nil
}

func storageProfiles(args []string) error {
	fs := flag.NewFlagSet("storage profiles", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parseStorageFlags(fs, args); err != nil {
		return err
	}
	list, err := awscreds.Profiles(awscreds.Default())
	if err != nil && !*asJSON {
		return err
	}
	if *asJSON {
		if list == nil {
			list = []awscreds.ProfileInfo{}
		}
		return emit(list)
	}
	for _, p := range list {
		fmt.Printf("%-24s %-12s %s\n", p.Name, p.Kind, p.Region)
	}
	return nil
}

// parseStorageFlags parses and refuses leftover arguments: `--sessions false` (a space
// instead of =) would otherwise stop parsing and drop everything after it without a word.
func parseStorageFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (boolean flags take =, as in --sessions=false)", fs.Arg(0))
	}
	return nil
}
