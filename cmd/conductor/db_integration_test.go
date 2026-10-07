package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/backup/s3fake"
	"github.com/aburan28/conductor/internal/storage"
)

// TestDatabaseArchiveRestoreEndToEnd runs a real Postgres with archive_command pointed at the
// conductor binary, takes a base backup into a (fake) bucket, writes more, loses the data
// directory, and restores it twice: to the end of the archive, and to a moment between two
// batches of writes. It needs the Postgres server binaries (initdb, pg_ctl, pg_basebackup,
// psql) and is skipped without them or under -short.
func TestDatabaseArchiveRestoreEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	bin := findServerBin()
	if bin == "" {
		t.Skip("no Postgres server binaries (initdb, pg_ctl, pg_basebackup, psql)")
	}
	asRoot := os.Geteuid() == 0
	var pgUser *user.User
	if asRoot {
		u, err := user.Lookup("postgres")
		if err != nil {
			t.Skip("running as root without a postgres user to run the server as")
		}
		pgUser = u
	}

	// A workspace the server's user can reach (t.TempDir may sit under a private directory).
	// Under /tmp when it exists: the server's Unix socket lives here, and macOS caps socket
	// paths at 104 bytes, which its per-user temporary directory nearly fills on its own.
	base := ""
	if info, err := os.Stat("/tmp"); err == nil && info.IsDir() {
		base = "/tmp"
	}
	work, err := os.MkdirTemp(base, "cdb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	if err := os.Chmod(work, 0o755); err != nil {
		t.Fatal(err)
	}
	chownPG := func(path string) {
		if pgUser != nil {
			uid, _ := strconv.Atoi(pgUser.Uid)
			gid, _ := strconv.Atoi(pgUser.Gid)
			_ = filepath.Walk(path, func(p string, _ os.FileInfo, err error) error {
				if err == nil {
					_ = os.Chown(p, uid, gid)
				}
				return nil
			})
		}
	}

	exe := filepath.Join(work, "conductor")
	build := exec.Command("go", "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building conductor: %v\n%s", err, out)
	}

	fake := s3fake.New("db-bucket")
	defer fake.Close()

	state := filepath.Join(work, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Save(func(k string) string {
		if k == "CONDUCTOR_STATE_DIR" {
			return state
		}
		return ""
	}, storage.Settings{
		S3:       storage.S3{Bucket: "db-bucket", Endpoint: fake.URL, PathStyle: true, Insecure: true, Prefix: "e2e"},
		Auth:     storage.Auth{Method: storage.AuthStatic, AccessKeyID: "AKIDDB", Secret: storage.SecretFile, SecretAccessKey: "dbsecret"},
		Database: storage.Database{ArchiveTimeoutSeconds: 1, KeepBaseBackups: 3, Seal: storage.Bool(true)},
	}); err != nil {
		t.Fatal(err)
	}
	chownPG(work)

	env := []string{"CONDUCTOR_STATE_DIR=" + state, "CONDUCTOR_CHECKPOINT_KEY=e2e passphrase",
		"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + work}
	var failNote func() string
	run := func(args ...string) string {
		t.Helper()
		var line strings.Builder
		for _, e := range env {
			k, v, _ := strings.Cut(e, "=")
			fmt.Fprintf(&line, "%s=%s ", k, shellQuote(v))
		}
		for _, a := range args {
			line.WriteString(shellQuote(a) + " ")
		}
		var cmd *exec.Cmd
		if pgUser != nil {
			cmd = exec.Command("su", "postgres", "-s", "/bin/sh", "-c", "cd "+shellQuote(work)+" && "+line.String())
		} else {
			cmd = exec.Command("/bin/sh", "-c", "cd "+shellQuote(work)+" && "+line.String())
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			note := ""
			if failNote != nil {
				note = failNote()
			}
			t.Fatalf("%s: %v\n%s%s", strings.Join(args, " "), err, out, note)
		}
		return string(out)
	}

	port := freePort(t)
	data := filepath.Join(work, "data")
	sql := func(q string) string {
		return strings.TrimSpace(run(filepath.Join(bin, "psql"), "-X", "-A", "-t", "-h", work, "-p", port, "-U", "conductor",
			"-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", q))
	}
	start := func(dir string) {
		failNote = func() string { b, _ := os.ReadFile(dir + ".log"); return "\nserver log:\n" + string(b) }
		defer func() { failNote = nil }()
		run(filepath.Join(bin, "pg_ctl"), "-D", dir, "-l", dir+".log", "-w", "-t", "120",
			"-o", fmt.Sprintf("-p %s -k %s -c listen_addresses='' -c fsync=off", port, work), "start")
	}
	stop := func(dir, mode string) { run(filepath.Join(bin, "pg_ctl"), "-D", dir, "-m", mode, "-w", "stop") }
	// A restored server accepts read-only connections as soon as it is consistent, while it
	// is still replaying the archive; wait until recovery ends and it is promoted.
	waitPromoted := func(dir string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for sql("select pg_is_in_recovery()") != "f" {
			if time.Now().After(deadline) {
				logBody, _ := os.ReadFile(dir + ".log")
				t.Fatalf("%s never finished recovery:\n%s", dir, logBody)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	run(filepath.Join(bin, "initdb"), "-D", data, "-U", "conductor", "--auth=trust", "-E", "UTF8")
	out := run(exe, "db", "archiving", "--data-dir", data, "--write")
	if !strings.Contains(out, "archive_mode = 'on'") || !strings.Contains(out, "db archive-wal %p %f") {
		t.Fatalf("archiving settings: %s", out)
	}
	start(data)
	stopped := false
	defer func() {
		if !stopped {
			stop(data, "immediate")
		}
	}()

	sql("create table rows (i int primary key, at timestamptz default clock_timestamp())")
	sql("insert into rows (i) select generate_series(1, 100)")
	run(exe, "db", "base-backup", "--dsn", fmt.Sprintf("host=%s port=%s user=conductor dbname=postgres", work, port), "--pg-bin", bin)

	sql("insert into rows (i) select generate_series(101, 200)")
	time.Sleep(1100 * time.Millisecond)
	target := sql("select to_char(clock_timestamp() at time zone 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')")
	time.Sleep(1100 * time.Millisecond)
	sql("insert into rows (i) select generate_series(201, 300)")
	last := sql("select pg_walfile_name(pg_current_wal_lsn())")
	sql("select pg_switch_wal()")

	// Wait until the segment holding the last writes is in the bucket.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, ok := fake.Object("e2e/db/" + sysIDOf(t, run, exe, data) + "/wal/" + last + ".sealed"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("segment %s never archived; bucket: %v\nstatus: %s", last, fake.Keys("e2e/"),
				run(exe, "db", "status", "--json", "--local", "--data-dir", data))
		}
		time.Sleep(200 * time.Millisecond)
	}
	var st dbStatusView
	if err := json.Unmarshal([]byte(run(exe, "db", "status", "--json", "--data-dir", data)), &st); err != nil ||
		st.Archive == nil || st.Archive.Archived == 0 || st.Failing || st.BaseBackups == nil || st.BaseBackups.Count != 1 {
		t.Fatalf("status: %+v %v", st, err)
	}
	for _, k := range fake.Keys("e2e/") {
		if strings.Contains(k, "/wal/") && !strings.HasSuffix(k, ".sealed") {
			t.Fatalf("an unsealed segment reached the bucket: %s", k)
		}
	}

	// The machine is lost.
	stop(data, "immediate")
	stopped = true
	if err := os.RemoveAll(data); err != nil {
		t.Fatal(err)
	}
	// A new machine: no key cache, only the bucket and the passphrase.
	_ = os.RemoveAll(filepath.Join(state, "db-keys"))

	restored := filepath.Join(work, "restored")
	run(exe, "db", "restore", "--data-dir", restored)
	start(restored)
	waitPromoted(restored)
	if n := sql("select count(*) from rows"); n != "300" {
		stop(restored, "immediate")
		logBody, _ := os.ReadFile(restored + ".log")
		t.Fatalf("restored to the end of the archive: %s rows, want 300\nserver log:\n%s\nbucket: %v", n, logBody, fake.Keys("e2e/"))
	}
	stop(restored, "fast")

	pitr := filepath.Join(work, "pitr")
	run(exe, "db", "restore", "--data-dir", pitr, "--target-time", target)
	start(pitr)
	waitPromoted(pitr)
	if n := sql("select count(*) from rows"); n != "200" {
		stop(pitr, "immediate")
		logBody, _ := os.ReadFile(pitr + ".log")
		t.Fatalf("restored to %s: %s rows, want 200\nserver log:\n%s", target, n, logBody)
	}
	stop(pitr, "fast")
}

func sysIDOf(t *testing.T, run func(...string) string, exe, data string) string {
	t.Helper()
	var st dbStatusView
	_ = json.Unmarshal([]byte(run(exe, "db", "status", "--json", "--local", "--data-dir", data)), &st)
	if st.SystemID == "" {
		t.Fatal("no system id")
	}
	return st.SystemID
}

func findServerBin() string {
	has := func(d string) bool {
		for _, tool := range []string{"initdb", "pg_ctl", "pg_basebackup", "psql"} {
			if _, err := os.Stat(filepath.Join(d, tool)); err != nil {
				return false
			}
		}
		return true
	}
	if p, err := exec.LookPath("initdb"); err == nil && has(filepath.Dir(p)) {
		return filepath.Dir(p)
	}
	var dirs []string
	for _, pattern := range []string{"/usr/lib/postgresql/*/bin", "/opt/homebrew/opt/postgresql@*/bin", "/usr/local/opt/postgresql@*/bin"} {
		m, _ := filepath.Glob(pattern)
		dirs = append(dirs, m...)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if has(d) {
			return d
		}
	}
	return ""
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}
