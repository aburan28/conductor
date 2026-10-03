package main

import (
	"bufio"
	"context"
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
func cmdUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "control plane URL (default: saved login, else http://localhost:8080)")
	addr := fs.String("addr", "", "listen address for a server started here (default: the endpoint's host:port)")
	dsn := fs.String("dsn", envOrStr("DATABASE_URL", "postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable"),
		"PostgreSQL connection string")
	project := fs.String("project", "", "project slug when the database is bootstrapped fresh (default: directory name)")
	if err := fs.Parse(args); err != nil {
		return err
	}

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
		// thing this machine can fix is its own login.
		if err := ensureLogin(ctx, daemon, ep, *dsn, *project); err != nil {
			return err
		}
		fmt.Printf("control plane already serving at %s (remote; nothing started locally)\n", ep)
		return nil
	}

	if err := ensureDatabase(*dsn); err != nil {
		return err
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

func envOrStr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
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

	dir, name := findComposeFile()
	if dir != "" {
		cmd := exec.Command("docker", "compose", "-f", filepath.Join(dir, name), "up", "-d", "db")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("docker compose up -d db: %v\n%s", err, out)
		}
	} else if dockerAvailable() {
		if err := ensurePostgresContainer(port); err != nil {
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
// does not exist, matching the repository's compose definition (image, credentials,
// published port, named volume).
func ensurePostgresContainer(port string) error {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", "conductor-db").CombinedOutput()
	if err == nil {
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
	cmd := exec.Command("docker", "run", "-d",
		"--name", "conductor-db", "--restart", "unless-stopped",
		"-e", "POSTGRES_USER=conductor", "-e", "POSTGRES_PASSWORD=conductor", "-e", "POSTGRES_DB=conductor",
		"-p", port+":5432",
		"-v", "conductor-pgdata:/var/lib/postgresql/data",
		"postgres:17-alpine")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker run conductor-db: %v\n%s", err, out)
	}
	return nil
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

	cmd := exec.Command(daemon, "--addr", listen, "--dsn", dsn)
	cmd.Env = os.Environ()
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

	fmt.Println("no valid login for this endpoint — bootstrapping")
	cmd := exec.Command(daemon, "bootstrap", "--dsn", dsn, "--endpoint", ep)
	if project != "" {
		cmd.Args = append(cmd.Args, "--project", project)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
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
