package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
)

// conductor up: one command from nothing to a running, logged-in control plane.
//
// Each step is a no-op when already satisfied:
//
//	database  Postgres for a loopback DSN, started in Docker when down
//	server    conductord in the background (pidfile + log under ~/.conductor/runtime)
//	login     saved credentials verified, or a fresh bootstrap minting this machine's token
//
// The inverse is `conductor down`.
//
// The local database is published on 127.0.0.1 only, under a password generated on the first
// `up` and kept in ~/.conductor/database.url (0600). It used to listen on every interface with
// the password "conductor", and since bearer tokens are stored as plain SHA-256 hashes, anyone
// who could write to that database could mint themselves a login. The DSN reaches conductord
// through its environment, never its argv, which any local user can read with ps.
func cmdUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "control plane URL (default: saved login, else http://localhost:8080)")
	addr := fs.String("addr", "", "listen address for a server started here (default: the endpoint's host:port)")
	dsnFlag := fs.String("dsn", "",
		"PostgreSQL connection string (default: $DATABASE_URL, else the one saved in ~/.conductor/database.url, else a new local database with a generated password)")
	project := fs.String("project", "", "project slug when the database is bootstrapped fresh (default: directory name)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dsnValue, dsnSource, err := resolveDSN(*dsnFlag, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	dsn := &dsnValue

	creds := client.LoadCredentials()
	ep := strings.TrimRight(*endpoint, "/")
	if ep == "" {
		ep = creds.Endpoint
	}

	daemon, err := locateDaemon()
	if err != nil {
		return err
	}

	if remoteEndpoint(ep) {
		// A non-local control plane has no local database or server to manage; the only
		// thing this machine can fix is its own login, and it can bootstrap one only with a
		// database it was told about.
		remoteDSN := *dsn
		if dsnSource == dsnGenerated {
			remoteDSN = ""
		}
		if err := ensureLogin(ctx, daemon, ep, remoteDSN, *project); err != nil {
			return err
		}
		fmt.Printf("control plane already serving at %s (remote; nothing started locally)\n", ep)
		return nil
	}

	if err := ensureDatabase(*dsn); err != nil {
		return err
	}
	if dsnSource == dsnGenerated {
		// A database that was already there (an installation from before passwords were
		// generated) still has the old fixed one; settle which DSN actually works, and keep
		// it, before anything else depends on it.
		settled, err := settleGeneratedDSN(ctx, *dsn)
		if err != nil {
			return err
		}
		*dsn = settled
		if err := saveDSN(*dsn); err != nil {
			return fmt.Errorf("saving the database DSN: %w", err)
		}
	}
	if err := ensureServer(ctx, daemon, ep, *addr, *dsn); err != nil {
		return err
	}
	if err := ensureLogin(ctx, daemon, ep, *dsn, *project); err != nil {
		return err
	}

	logFile := ""
	if dir, err := runtimeDir(); err == nil {
		logFile = filepath.Join(dir, "conductord.log")
	}
	fmt.Printf(`Conductor is up.

  endpoint    %s
  dashboard   %s/
  server log  %s
  stop with   conductor down
`, ep, ep, logFile)
	return nil
}

// locateDaemon finds the conductord binary. Release installs put both binaries in the
// same directory, so the neighbour of the running conductor binary is the first guess.
func locateDaemon() (string, error) {
	if p := os.Getenv("CONDUCTOR_DAEMON"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil || fileExists(abs) != nil {
			return "", fmt.Errorf("CONDUCTOR_DAEMON: %s not found", p)
		}
		return abs, nil
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "conductord")
		if fileExists(p) == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("conductord"); err == nil {
		return p, nil
	}
	return "", errors.New("conductord not found next to the conductor binary or on PATH; install the release (make install) or set CONDUCTOR_DAEMON")
}

func fileExists(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("is a directory")
	}
	return nil
}

// runtimeDir is where `conductor up` keeps its pidfile and log: the user's home, so the
// command works with no repository checkout at all.
func runtimeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "runtime"), nil
}

// remoteEndpoint reports whether the control plane URL does not live on this machine, in
// which case `conductor up` must not try to start a local substitute for it.
func remoteEndpoint(ep string) bool {
	u, err := url.Parse(ep)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host != "" && !isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// Database
// ---------------------------------------------------------------------------

// ensureDatabase makes sure the DSN's database accepts connections.
//
// A non-loopback DSN points at a database this machine does not own, so it is left alone.
// For a loopback DSN the repository's compose file is preferred when it is found in the
// working directory or the project root — same container name and volume as `make db-up`,
// so an existing installation is reused. Without a compose file a standalone container
// with the same name (conductor-db) and a named volume is started; an already-present
// container is always adopted, never shadowed with an empty volume.
func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse --dsn: %w", err)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	if !isLoopbackHost(host) || portOpen(host, port) {
		return nil
	}

	// A container created here gets the DSN's own password, through the environment of the
	// docker command rather than its argv. It only takes effect on an empty volume; an
	// existing volume keeps the password it was initialized with.
	password, _ := u.User.Password()
	dir, name := findComposeFile()
	if dir != "" {
		cmd := exec.Command("docker", "compose", "-f", filepath.Join(dir, name), "up", "-d", "db")
		cmd.Dir = dir
		cmd.Env = append(withoutEnv(os.Environ(), "CONDUCTOR_DB_PASSWORD"), "CONDUCTOR_DB_PASSWORD="+password)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("docker compose up -d db: %v\n%s", err, out)
		}
	} else if dockerAvailable() {
		if err := ensurePostgresContainer(port, password); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("database at %s:%s is not running and Docker is not available — start Docker and re-run, or point --dsn at a reachable Postgres", host, port)
	}

	fmt.Print("waiting for Postgres")
	deadline := time.Now().Add(3 * time.Minute)
	for i := 0; ; i++ {
		if exec.Command("docker", "exec", "conductor-db", "pg_isready").Run() == nil {
			fmt.Println(" ready")
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Println()
			return errors.New("Postgres did not become ready — check `docker logs conductor-db`")
		}
		if i%2 == 1 {
			fmt.Print(".")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// findComposeFile looks for the repository's compose file in the working directory and
// the project root only. An ancestor directory may hold an unrelated compose project,
// and starting a stranger's services on a guess is worse than asking for Docker directly.
// The file must pin our container name; a compose file for something else is not the
// database this command manages.
func findComposeFile() (dir, name string) {
	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
		if root, err := config.FindRoot(cwd); err == nil {
			candidates = append(candidates, root)
		}
	}
	seen := map[string]bool{}
	for _, dir := range candidates {
		abs, err := filepath.Abs(dir)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		for _, name := range []string{"docker-compose.yml", "compose.yml"} {
			path := filepath.Join(abs, name)
			body, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(body), "conductor-db") {
				continue
			}
			return abs, name
		}
	}
	return "", ""
}

func dockerAvailable() bool {
	return exec.Command("docker", "info", "-f", "{{.ServerVersion}}").Run() == nil
}

// ensurePostgresContainer starts conductor-db when it is stopped, and creates it when it
// does not exist, matching the repository's compose definition (image, published port,
// named volume). The port is published on loopback only, and the password comes from the
// DSN, handed to docker in its environment so it never appears in a process listing.
func ensurePostgresContainer(port, password string) error {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", "conductor-db").CombinedOutput()
	if err == nil {
		warnIfPublished()
		if strings.TrimSpace(string(out)) == "true" {
			return nil
		}
		if out, err := exec.Command("docker", "start", "conductor-db").CombinedOutput(); err != nil {
			return fmt.Errorf("docker start conductor-db: %v\n%s", err, out)
		}
		return nil
	}
	if !strings.Contains(string(out), "No such object") {
		return fmt.Errorf("docker inspect conductor-db: %v\n%s", err, out)
	}
	if password == "" {
		return errors.New("the database DSN has no password; refusing to create a database without one")
	}
	cmd := exec.Command("docker", "run", "-d",
		"--name", "conductor-db", "--restart", "unless-stopped",
		"-e", "POSTGRES_USER=conductor", "-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB=conductor",
		"-p", "127.0.0.1:"+port+":5432",
		"-v", "conductor-pgdata:/var/lib/postgresql/data",
		"postgres:17-alpine")
	cmd.Env = append(withoutEnv(os.Environ(), "POSTGRES_PASSWORD"), "POSTGRES_PASSWORD="+password)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker run conductor-db: %v\n%s", err, out)
	}
	return nil
}

// warnIfPublished tells the operator when an existing conductor-db container publishes its
// port beyond this machine, as every container created before loopback binding did. A
// published port cannot be changed in place; recreating the container keeps the volume.
func warnIfPublished() {
	out, err := exec.Command("docker", "inspect", "-f",
		"{{range $p, $b := .HostConfig.PortBindings}}{{range $b}}{{.HostIp}}|{{end}}{{end}}",
		"conductor-db").Output()
	if err != nil || !publishedBeyondLoopback(string(out)) {
		return
	}
	fmt.Fprint(os.Stderr, `
Warning: the conductor-db container publishes Postgres on every network interface. Anyone who
can reach this machine can try its password. Recreate it bound to loopback (the data volume is
kept):

  docker rm -f conductor-db && conductor up

`)
}

// publishedBeyondLoopback reads the host addresses from warnIfPublished's inspect template,
// each binding's address followed by "|"; an empty or wildcard address listens everywhere.
func publishedBeyondLoopback(bindings string) bool {
	parts := strings.Split(strings.TrimSpace(bindings), "|")
	for _, ip := range parts[:len(parts)-1] {
		switch strings.TrimSpace(ip) {
		case "", "0.0.0.0", "::":
			return true
		}
	}
	return false
}

func portOpen(host, port string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ---------------------------------------------------------------------------
// Database credentials
// ---------------------------------------------------------------------------

// Where the DSN came from, which decides whether `up` may settle and save it.
const (
	dsnFlag      = "flag"
	dsnEnv       = "env"
	dsnSaved     = "saved"
	dsnGenerated = "generated"
)

// legacyDSN is what every installation used before passwords were generated; a generated DSN
// is the same with a random password. A database initialized before then still has the old
// password, which is why settleGeneratedDSN tries it.
const legacyDSN = "postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable"

// dsnPath is where `conductor up` keeps the DSN of the database it manages.
func dsnPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "database.url"), nil
}

// resolveDSN picks the database: --dsn, then DATABASE_URL, then the DSN saved by an earlier
// `up`, and only then a new one with a generated password.
func resolveDSN(flagValue, envValue string) (string, string, error) {
	switch {
	case flagValue != "":
		return flagValue, dsnFlag, nil
	case envValue != "":
		return envValue, dsnEnv, nil
	}
	if path, err := dsnPath(); err == nil {
		if body, err := os.ReadFile(path); err == nil {
			if saved := strings.TrimSpace(string(body)); saved != "" {
				return saved, dsnSaved, nil
			}
		}
	}
	password, err := randomPassword()
	if err != nil {
		return "", "", err
	}
	u, _ := url.Parse(legacyDSN)
	u.User = url.UserPassword("conductor", password)
	return u.String(), dsnGenerated, nil
}

// randomPassword is 192 bits, hex-encoded so it needs no escaping in a URL or a shell.
func randomPassword() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// saveDSN writes the DSN owner-only: it is the database password.
func saveDSN(dsn string) error {
	path, err := dsnPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(dsn+"\n"), 0o600); err != nil {
		return err
	}
	// WriteFile keeps an existing file's mode; make sure an old, looser file is tightened.
	return os.Chmod(path, 0o600)
}

// settleGeneratedDSN confirms which credentials the database accepts. A database this `up`
// just created takes the generated password. One that was already running — or whose volume
// predates generated passwords — still has the legacy password: that DSN is kept so the
// installation keeps working, and the operator is told how to rotate it.
func settleGeneratedDSN(ctx context.Context, generated string) (string, error) {
	err := probeDSN(ctx, generated)
	if err == nil {
		return generated, nil
	}
	if !isAuthFailure(err) {
		return "", fmt.Errorf("connecting to the database: %w", err)
	}
	// The same database, with the password every earlier installation was given.
	u, perr := url.Parse(generated)
	if perr != nil {
		return "", perr
	}
	u.User = url.UserPassword(u.User.Username(), "conductor")
	legacy := u.String()
	if legacyErr := probeDSN(ctx, legacy); legacyErr != nil {
		return "", fmt.Errorf("the database at localhost:55432 rejected the generated password and the legacy one; "+
			"pass --dsn or set DATABASE_URL: %w", err)
	}
	fmt.Fprint(os.Stderr, `
Warning: this database still uses the old fixed password "conductor". It is published on
loopback only from now on, but any local user or process can still log in with it. Rotate it:

  docker exec conductor-db psql -U conductor -d conductor -c "ALTER ROLE conductor PASSWORD '<new>'"

then put the new DSN in ~/.conductor/database.url (and DATABASE_URL for make and scripts).

`)
	return legacy, nil
}

// probeDSN connects once, retrying briefly while the database is still starting.
func probeDSN(ctx context.Context, dsn string) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, err := pgx.Connect(cctx, dsn)
		cancel()
		if err == nil {
			return conn.Close(ctx)
		}
		if isAuthFailure(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// isAuthFailure reports Postgres rejecting the credentials (invalid_password, or
// invalid_authorization_specification), as opposed to the server not being up yet.
func isAuthFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "28P01" || pgErr.Code == "28000")
}

// daemonCommand runs conductord with the DSN in its environment. On the command line it
// would be readable by every local user through ps or /proc.
func daemonCommand(daemon, dsn string, args ...string) *exec.Cmd {
	cmd := exec.Command(daemon, args...)
	cmd.Env = append(withoutEnv(os.Environ(), "DATABASE_URL"), "DATABASE_URL="+dsn)
	return cmd
}

// withoutEnv drops a variable from an environment list.
func withoutEnv(environ []string, name string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if k, _, _ := strings.Cut(kv, "="); k != name {
			out = append(out, kv)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

func ensureServer(ctx context.Context, daemon, ep, addr, dsn string) error {
	if healthy(ep) {
		fmt.Printf("control plane already serving at %s\n", ep)
		return nil
	}

	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	pidFile := filepath.Join(dir, "conductord.pid")
	logFile := filepath.Join(dir, "conductord.log")

	// A previous `conductor up` may still be starting; give it a chance before
	// forking another daemon that would lose the port race.
	if pid := readPid(pidFile); pid > 0 && processAlive(pid) {
		fmt.Printf("control plane starting (pid %d), waiting for %s/v1/health\n", pid, ep)
		if waitHealthy(ep, 60*time.Second) {
			return nil
		}
	} else if pid > 0 {
		os.Remove(pidFile) // stale
	}

	listen := addr
	if listen == "" {
		listen = listenAddrFromEndpoint(ep)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}

	cmd := daemonCommand(daemon, dsn, "--addr", listen)
	cmd.Stdout = log
	cmd.Stderr = log
	// Detach into its own session so the daemon outlives the CLI and a terminal
	// hangup does not take the control plane down with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Close()
		return fmt.Errorf("start conductord: %w", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	// Reap when the daemon dies; the process is meant to outlive this command.
	go func() { _ = cmd.Wait(); close(exited) }()
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		syscall.Kill(pid, syscall.SIGTERM)
		log.Close()
		return err
	}
	log.Close()

	fmt.Printf("starting control plane (pid %d, log %s), waiting for %s/v1/health\n", pid, logFile, ep)
	deadline := time.Now().Add(90 * time.Second)
	for {
		if healthy(ep) {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("conductord exited during startup — last log lines:\n%s", tailLines(logFile, 20))
		default:
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGTERM)
			return fmt.Errorf("control plane did not become healthy at %s — last log lines:\n%s", ep, tailLines(logFile, 20))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func healthy(ep string) bool {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(ep + "/v1/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitHealthy(ep string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if healthy(ep) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return healthy(ep)
}

// listenAddrFromEndpoint turns the control plane URL into the address a local daemon
// must bind so that URL reaches it.
func listenAddrFromEndpoint(ep string) string {
	u, err := url.Parse(ep)
	if err != nil || u.Port() == "" {
		return "127.0.0.1:8080"
	}
	host := u.Hostname()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, u.Port())
}

// ---------------------------------------------------------------------------
// Login
// ---------------------------------------------------------------------------

// ensureLogin verifies the saved credentials against this endpoint, and when they are
// missing or invalid runs a bootstrap, which mints a token and writes this machine's
// login directly (see conductord bootstrap).
func ensureLogin(ctx context.Context, daemon, ep, dsn, project string) error {
	creds := client.LoadCredentials()
	if creds.Endpoint == ep && creds.Token != "" {
		var who struct{}
		if client.New(ep, creds.Token).Get(ctx, "/v1/whoami", &who) == nil {
			handle := creds.Handle
			if handle == "" {
				handle = "this principal"
			}
			fmt.Printf("logged in as %s\n", handle)
			return nil
		}
	}

	if dsn == "" {
		return fmt.Errorf("no valid login for %s; log in with `conductor login --endpoint %s --token …`, "+
			"or pass --dsn to bootstrap its database from here", ep, ep)
	}
	fmt.Println("no valid login for this endpoint — bootstrapping")
	cmd := daemonCommand(daemon, dsn, "bootstrap", "--endpoint", ep)
	if project != "" {
		cmd.Args = append(cmd.Args, "--project", project)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

func readPid(path string) int {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func tailLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return "<no log>"
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if len(lines) == 0 {
		return "<empty log>"
	}
	return strings.Join(lines, "\n")
}
