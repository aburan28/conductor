// Command conductord is the Conductor control plane: REST API, SSE event stream, dashboard,
// and the background scheduler.
//
// It is one process because DESIGN.md §28.1 targets a single host first. Several replicas
// may share one database: scheduler steps are transactional and `SKIP LOCKED`-based, the
// state replicas must agree on (scheduler liveness, budget alert levels, the GitHub App, its
// setups and check runs) is in Postgres, and the GitHub poller is gated by an advisory lock.
// Two things remain per process: MCP HTTP sessions, which need session affinity at the load
// balancer, and the authentication failure limiter. docs/OPERATIONS.md has the details.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adamburan/conductor/internal/api"
	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/githubapp"
	"github.com/adamburan/conductor/internal/peer"
	"github.com/adamburan/conductor/internal/scheduler"
	"github.com/adamburan/conductor/internal/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		if err := bootstrap(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "bootstrap:", err)
			os.Exit(1)
		}
		return
	}
	if err := serve(os.Args[1:]); err != nil && !errors.Is(err, http.ErrServerClosed) {
		os.Exit(exitCode(err, os.Stderr))
	}
}

// serveConfig is everything serve reads from its flags and environment, validated.
type serveConfig struct {
	addr, dsn         string
	tick, detect      time.Duration
	tickTimeout       time.Duration
	outageAfter       time.Duration
	noScheduler       bool
	tlsCert, tlsKey   string
	tlsEnabled        bool
	behindProxy       bool
	publicURL         string
	meshPeers         []peer.Peer
	meshOn            bool
	peerCA            string
	peerCert, peerKey string
	peerDiscoverDNS   string
	peerDNSServer     string
	securityMode      string
	githubPoll        time.Duration
	githubAPI         string
	githubWeb         string
	shutdownTimeout   time.Duration
	database          db.Options
	retention         db.RetentionPolicy
	ops               api.OpsOptions
	verbose           bool
}

// usageError is a command line flag could not parse. flag has already reported it, with
// the usage text, so it is not printed again.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// exitCode reports err the way the flag package's ExitOnError convention does — -h exits
// 0, a flag that does not parse exits 2 (both already printed by flag) — and any other
// failure on stderr with status 1.
func exitCode(err error, stderr io.Writer) int {
	var usage usageError
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &usage):
		return 2
	}
	fmt.Fprintln(stderr, "conductord:", err)
	return 1
}

// parseServeConfig parses serve's flags, writing usage and parse errors to output.
// Everything that can be checked without touching the network or the filesystem is checked
// here, so a bad invocation fails before anything starts.
func parseServeConfig(args []string, output io.Writer) (*serveConfig, error) {
	c := &serveConfig{}
	fs := flag.NewFlagSet("conductord", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&c.addr, "addr", envOr("CONDUCTOR_ADDR", "127.0.0.1:8080"), "listen address")
	fs.StringVar(&c.dsn, "dsn", envOr("DATABASE_URL", ""), "PostgreSQL connection string")
	fs.DurationVar(&c.tick, "tick", 2*time.Second, "scheduler tick interval")
	fs.DurationVar(&c.detect, "detect-every", 15*time.Second, "conflict graph recomputation interval")
	fs.DurationVar(&c.tickTimeout, "tick-timeout", 30*time.Second, "longest one scheduler pass may run before it is cancelled")
	fs.DurationVar(&c.outageAfter, "outage-after", 0,
		"how long no scheduler replica may have ticked before open leases are extended by the gap instead of reclaimed (default: tick-timeout + 3 ticks, at least 30s)")
	fs.BoolVar(&c.noScheduler, "no-scheduler", false, "serve the API without running the scheduler")
	fs.StringVar(&c.tlsCert, "tls-cert", envOr("CONDUCTOR_TLS_CERT", ""), "TLS certificate file")
	fs.StringVar(&c.tlsKey, "tls-key", envOr("CONDUCTOR_TLS_KEY", ""), "TLS private key file")
	fs.BoolVar(&c.behindProxy, "behind-proxy", false,
		"trust X-Forwarded-For (only set this when a proxy you control rewrites it)")
	insecure := fs.Bool("insecure", false,
		"permit binding a non-loopback address without TLS")
	fs.StringVar(&c.publicURL, "public-url", envOr("CONDUCTOR_PUBLIC_URL", ""),
		"URL at which clients reach this server (used for the MCP endpoint and self-calls); defaults to the bind address")
	peers := &peerFlags{}
	fs.Var(peers, "peer", "peer daemon as name=https://host:port (repeatable; CONDUCTOR_PEERS accepts a comma-separated list)")
	fs.StringVar(&c.peerCA, "peer-ca", envOr("CONDUCTOR_PEER_CA", ""),
		"mesh CA bundle (PEM) that peer certificates are signed by; enables the peer surface")
	fs.StringVar(&c.peerCert, "peer-cert", envOr("CONDUCTOR_PEER_CERT", ""),
		"this daemon's mesh certificate (PEM), presented to peers and served as the TLS certificate when --tls-cert is absent")
	fs.StringVar(&c.peerKey, "peer-key", envOr("CONDUCTOR_PEER_KEY", ""),
		"this daemon's mesh private key")
	fs.StringVar(&c.peerDiscoverDNS, "peer-discover-dns", envOr("CONDUCTOR_PEER_DISCOVER_DNS", ""),
		"DNS SRV record resolved on every tick to find mesh peers automatically, instead of a hand-maintained --peer per daemon (e.g. _conductor-mesh._tcp.mesh.internal)")
	fs.StringVar(&c.peerDNSServer, "peer-dns-server", envOr("CONDUCTOR_PEER_DNS_SERVER", ""),
		"host:port of the DNS server used for --peer-discover-dns lookups (default: system DNS). For a laptop-local directory: 127.0.0.1:15353")
	fs.StringVar(&c.securityMode, "security-mode", envOr("CONDUCTOR_SECURITY_MODE", ""),
		"local: this machine's owner can sign in without a token; enhanced: tokens only, everywhere. "+
			"Unset: local when bound to loopback, enhanced otherwise, changeable with 'conductor security'")
	fs.DurationVar(&c.githubPoll, "github-poll", 2*time.Minute,
		"how often the GitHub App re-checks open pull requests (negative disables polling)")
	fs.StringVar(&c.githubAPI, "github-api", envOr("CONDUCTOR_GITHUB_API", ""), "GitHub API base URL (GitHub Enterprise: https://HOST/api/v3)")
	fs.StringVar(&c.githubWeb, "github-web", envOr("CONDUCTOR_GITHUB_WEB", ""), "GitHub web base URL (GitHub Enterprise: https://HOST)")

	// Operations.
	fs.DurationVar(&c.shutdownTimeout, "shutdown-timeout", 25*time.Second,
		"on SIGTERM, how long in-flight requests and background work get to finish before the process exits")
	fs.DurationVar(&c.database.StatementTimeout, "db-statement-timeout", db.DefaultStatementTimeout,
		"longest a single SQL statement may run (negative: no limit; migrations are exempt)")
	fs.DurationVar(&c.database.LockTimeout, "db-lock-timeout", db.DefaultLockTimeout,
		"longest a statement may wait for a lock (negative: no limit)")
	retention := scheduler.DefaultRetention()
	const day = 24 * time.Hour
	eventsDays := fs.Int("retention-days", envInt("CONDUCTOR_RETENTION_DAYS", int(retention.Events/day)),
		"days to keep domain events and delivered outbox rows (0 keeps them forever)")
	auditDays := fs.Int("audit-retention-days", envInt("CONDUCTOR_AUDIT_RETENTION_DAYS", int(retention.Audit/day)),
		"days to keep the audit log (0 keeps it forever)")
	outboxDays := fs.Int("outbox-undelivered-days", int(retention.OutboxUndelivered/day),
		"days to keep outbox rows no consumer delivered (0 keeps them forever)")
	usageDays := fs.Int("usage-retention-days", int(retention.Usage/day),
		"days to keep usage buckets (0 keeps them forever; at least 31, since budgets look back 30 days)")
	fs.DurationVar(&retention.Idempotency, "idempotency-ttl", retention.Idempotency,
		"how long an Idempotency-Key's stored response is kept")
	fs.StringVar(&c.ops.MetricsToken, "metrics-token", envOr("CONDUCTOR_METRICS_TOKEN", ""),
		"bearer token /metrics requires; unset, /metrics answers only loopback connections (and nothing behind --behind-proxy)")
	fs.DurationVar(&c.ops.BodyTimeout, "body-timeout", 30*time.Second,
		"how long a client may take to send a request body (negative disables)")
	fs.IntVar(&c.ops.MaxStreams, "max-streams", 1000, "most concurrent event-stream connections")
	fs.IntVar(&c.ops.MaxStreamsPerPrincipal, "max-streams-per-principal", 16, "most concurrent event-stream connections per principal")
	fs.IntVar(&c.ops.MaxWebhookChecks, "max-webhook-checks", 8, "most pull request checks running at once on behalf of webhook deliveries")
	fs.BoolVar(&c.verbose, "v", false, "verbose logging")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `conductord — Conductor control plane

Usage:
  conductord [flags]
  conductord bootstrap [flags]     create an organization, project, principal, and token

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// flag has already printed the error and the usage text.
		return nil, usageError{err}
	}
	if c.dsn == "" {
		return nil, errors.New("no database configured: pass --dsn or set DATABASE_URL")
	}

	for name, days := range map[string]int{"--retention-days": *eventsDays, "--audit-retention-days": *auditDays,
		"--outbox-undelivered-days": *outboxDays, "--usage-retention-days": *usageDays} {
		if days < 0 {
			return nil, fmt.Errorf("%s must not be negative", name)
		}
	}
	if *usageDays > 0 && *usageDays < 31 {
		return nil, errors.New("--usage-retention-days must be 0 or at least 31: budgets total the last 30 days of usage")
	}
	retention.Events = time.Duration(*eventsDays) * day
	retention.OutboxDelivered = min(retention.OutboxDelivered, retention.Events)
	if retention.Events == 0 {
		retention.OutboxDelivered = 0
	}
	retention.Audit = time.Duration(*auditDays) * day
	retention.OutboxUndelivered = time.Duration(*outboxDays) * day
	retention.Usage = time.Duration(*usageDays) * day
	c.retention = retention

	c.tlsEnabled = c.tlsCert != "" && c.tlsKey != ""
	if (c.tlsCert == "") != (c.tlsKey == "") {
		return nil, errors.New("--tls-cert and --tls-key must be given together")
	}
	var err error
	c.meshPeers, err = peers.mergeEnv(os.Getenv("CONDUCTOR_PEERS"))
	if err != nil {
		return nil, err
	}
	// The mesh identity is one certificate in two roles: presented as the TLS client
	// certificate when dialing peers, and served as this daemon's TLS certificate. A peer
	// verifying the server against the mesh CA is what makes the link mutual, so a
	// separate non-mesh --tls-cert cannot coexist with peering.
	c.meshOn = c.peerCA != "" || c.peerCert != "" || c.peerKey != "" || len(c.meshPeers) > 0 || c.peerDiscoverDNS != ""
	if (c.peerCert == "") != (c.peerKey == "") {
		return nil, errors.New("--peer-cert and --peer-key must be given together")
	}
	if c.meshOn {
		if c.peerCA == "" || c.peerCert == "" {
			return nil, errors.New("peering requires --peer-ca, --peer-cert and --peer-key together")
		}
		if c.tlsCert != "" && c.tlsCert != c.peerCert {
			return nil, errors.New("--tls-cert must be the mesh certificate when peering (pass --peer-cert as --tls-cert, or drop --tls-cert)")
		}
		if c.tlsCert == "" {
			c.tlsCert, c.tlsKey = c.peerCert, c.peerKey
			c.tlsEnabled = true
		}
	}
	// Bearer tokens cross this connection. Binding a reachable address in plaintext puts
	// them on the wire, so it requires saying so out loud — a default that fails safe is
	// worth more than one that is convenient.
	if !c.tlsEnabled && !*insecure && !isLoopback(c.addr) && !c.behindProxy {
		return nil, fmt.Errorf(`refusing to serve %s without TLS.

Bearer tokens would cross the network in the clear. Choose one:

  --tls-cert cert.pem --tls-key key.pem   terminate TLS here
  --peer-ca ca.pem --peer-cert/--peer-key your mesh identity (implies TLS)
  --behind-proxy                          a proxy you control terminates TLS
  --insecure                              you accept the risk (trusted network only)

Binding 127.0.0.1 needs none of these.`, c.addr)
	}
	switch c.securityMode {
	case "", db.SecurityLocal, db.SecurityEnhanced:
	default:
		return nil, fmt.Errorf("--security-mode must be local or enhanced, not %q", c.securityMode)
	}
	if c.shutdownTimeout <= 0 {
		return nil, errors.New("--shutdown-timeout must be positive")
	}
	return c, nil
}

func serve(args []string) error {
	cfg, err := parseServeConfig(args, os.Stderr)
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// sigCtx ends on SIGINT/SIGTERM; life ends with it, or when the listener fails, and is
	// what every background goroutine and long-lived response runs under.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	life, endLife := context.WithCancel(sigCtx)
	defer endLife()
	// background tracks every goroutine started here. The store is closed only after they
	// have all returned, so none of them is cut off mid-transaction.
	var background sync.WaitGroup
	goBackground := func(name string, fn func(ctx context.Context) error) {
		background.Add(1)
		go func() {
			defer background.Done()
			if err := fn(life); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error(name+" exited", "error", err)
			}
		}()
	}

	store, err := db.Open(life, cfg.dsn, cfg.database)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.Migrate(life); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	logger.Info("database ready")
	api.RegisterPoolMetrics(store)

	scheme := "http"
	if cfg.tlsEnabled {
		scheme = "https"
	}
	selfEndpoint := cfg.publicURL
	if selfEndpoint == "" {
		selfEndpoint = scheme + "://" + displayHost(cfg.addr)
	}

	// Mesh identity and, when peers are configured, the link keeper that dials them.
	// Link state is a projection, so it lives in memory and disappears with the process.
	var (
		meshName   string
		meshPool   *x509.CertPool
		peerStatus func() []peer.LinkStatus
	)
	if cfg.meshOn {
		meshPool, err = peer.LoadCA(cfg.peerCA)
		if err != nil {
			return fmt.Errorf("mesh CA: %w", err)
		}
		ourCert, err := peer.LoadCert(cfg.peerCert, cfg.peerKey)
		if err != nil {
			return fmt.Errorf("mesh certificate: %w", err)
		}
		leaf, err := x509.ParseCertificate(ourCert.Certificate[0])
		if err != nil {
			return fmt.Errorf("mesh certificate: %w", err)
		}
		meshName = peer.CertName(leaf)
		if meshName == "" {
			return errors.New("mesh certificate carries no name (no DNS SAN, no common name)")
		}
		if len(cfg.meshPeers) > 0 || cfg.peerDiscoverDNS != "" {
			// A pinned DNS server bypasses the system resolver for discovery
			// lookups. The Go resolver ignores /etc/resolver, so on a laptop the
			// directory has to live somewhere the OS resolver never looks —
			// this points the lookup straight at it instead.
			var resolver peer.SRVResolver
			if cfg.peerDNSServer != "" {
				dnsServer := cfg.peerDNSServer
				resolver = &net.Resolver{
					PreferGo: true,
					Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
						return net.Dial(network, dnsServer)
					},
				}
			}
			mgr, err := peer.New(peer.Options{
				Peers: cfg.meshPeers, SelfURL: selfEndpoint,
				CAPath: cfg.peerCA, CertPath: cfg.peerCert, KeyPath: cfg.peerKey,
				DiscoverDNS: cfg.peerDiscoverDNS,
				Resolver:    resolver,
				Logger:      logger,
			})
			if err != nil {
				return err
			}
			goBackground("peer link keeper", mgr.Run)
			peerStatus = mgr.Snapshot
		}
	}

	// Local sign-in. A daemon only this machine can reach defaults to letting its owner in
	// without a token; one reachable from the network defaults to tokens only. Either can be
	// pinned with --security-mode, or switched at runtime with `conductor security`.
	defaultMode := db.SecurityEnhanced
	if isLoopback(cfg.addr) && !cfg.behindProxy {
		defaultMode = db.SecurityLocal
	}

	// The GitHub App. Its credentials live in the database, shared by every replica; an app
	// set up by an older conductord (a 0600 file beside the CLI's credentials) is imported
	// once. With none, the integration serves only its setup flow. GitHub can deliver
	// webhooks only to a public URL; anywhere else the poller finds pull requests itself.
	githubCreds, err := githubapp.DefaultPath()
	if err != nil {
		return err
	}
	webhookURL := ""
	if cfg.publicURL != "" && !isLoopbackURL(cfg.publicURL) {
		webhookURL = strings.TrimRight(cfg.publicURL, "/") + "/github/webhook"
	}
	gh, ghErr := api.NewGitHub(api.GitHubOptions{
		CredentialsPath: githubCreds, API: cfg.githubAPI, Web: cfg.githubWeb,
		BaseURL: selfEndpoint, WebhookURL: webhookURL, Poll: cfg.githubPoll, Logger: logger,
	})

	ops := cfg.ops
	ops.BaseContext = life
	svc := coord.New(store)
	server := api.New(store, svc, api.Options{
		Logger:       logger,
		Web:          web.Handler(),
		BehindProxy:  cfg.behindProxy,
		TLSEnabled:   cfg.tlsEnabled,
		SelfEndpoint: selfEndpoint,
		PeerName:     meshName,
		PeerStatus:   peerStatus,
		LocalLogin:   api.LocalLoginOptions{DefaultMode: defaultMode, ForcedMode: cfg.securityMode},
		GitHub:       gh,
		Ops:          ops,
	})
	if ghErr == nil {
		ghErr = gh.Refresh(life)
	}
	if ghErr != nil {
		logger.Warn("github app credentials could not be loaded; run `conductor github setup` again", "error", ghErr)
	} else if gh.Configured() {
		logger.Info("github app ready", "mode", map[bool]string{true: "webhook", false: "polling"}[webhookURL != ""])
	}
	goBackground("github poller", gh.Run)
	if settings, err := store.GetServerSettings(life); err == nil {
		mode := firstNonEmpty(cfg.securityMode, settings.SecurityMode, defaultMode)
		logger.Info("security mode", "mode", mode, "local_owner_set", settings.LocalOwnerID != "")
	}

	sched := scheduler.New(store, svc, scheduler.Options{
		Tick: cfg.tick, DetectEvery: cfg.detect, TickTimeout: cfg.tickTimeout,
		OutageAfter: cfg.outageAfter, Retention: cfg.retention, Logger: logger,
	})
	// Before the first request: if every replica was down long enough for leases to lapse,
	// extend them now, or a worker's first heartbeat after the outage would be refused.
	// This runs with --no-scheduler too; an API-only replica takes heartbeats as well.
	if _, err := sched.RecoverOutage(life); err != nil {
		logger.Warn("lease outage recovery failed; the scheduler retries on its first tick", "error", err)
	}
	if !cfg.noScheduler {
		goBackground("scheduler", sched.Run)
	}

	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout or WriteTimeout: both stay armed for the whole response, and the
		// event stream is a long-lived one. Request bodies get their own deadline instead
		// (--body-timeout, api.OpsOptions.BodyTimeout).
		IdleTimeout: 120 * time.Second,
		ErrorLog:    slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	if cfg.tlsEnabled {
		httpServer.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Prefer forward-secret suites. Go picks sensibly for TLS 1.3; this only
			// constrains the 1.2 fallback.
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			},
		}
		if cfg.meshOn {
			// Verify peer certificates when presented, without demanding one from human
			// clients. The /v1/peer/* routes are the only consumers of the result.
			httpServer.TLSConfig.ClientCAs = meshPool
			httpServer.TLSConfig.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}
	logger.Info("conductor listening",
		"addr", cfg.addr, "tls", cfg.tlsEnabled, "behind_proxy", cfg.behindProxy,
		"mesh", cfg.meshOn,
		"dashboard", selfEndpoint+"/", "mcp", selfEndpoint+"/mcp")

	listen := httpServer.ListenAndServe
	if cfg.tlsEnabled {
		listen = func() error { return httpServer.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey) }
	}
	// Background work started by requests (webhook checks) is awaited with the rest.
	background.Add(1)
	go func() {
		defer background.Done()
		<-life.Done()
		server.Wait()
	}()
	return runServer(life, endLife, httpServer, listen, &background, cfg.shutdownTimeout, logger)
}

// runServer serves until ctx ends or the listener fails, then shuts down in order:
//
//  1. end ctx (via cancel), which stops the scheduler, the poller and peer links, ends event
//     streams, and cancels background webhook checks;
//  2. stop accepting connections and wait for in-flight requests to complete;
//  3. wait for every background goroutine to return.
//
// All of it is bounded by timeout; past it, open connections are closed. It returns only
// after step 3 (or the timeout), so the caller can close the store knowing nothing still
// uses it.
//
// http.Server.ListenAndServe returns ErrServerClosed the moment Shutdown begins, not when it
// ends — returning then, as serve once did, closed the store under requests still running.
func runServer(ctx context.Context, cancel context.CancelFunc, srv *http.Server, listen func() error,
	background *sync.WaitGroup, timeout time.Duration, logger *slog.Logger) error {
	listenErr := make(chan error, 1)
	go func() { listenErr <- listen() }()

	var result error
	select {
	case <-ctx.Done():
		logger.Info("shutting down", "timeout", timeout.String())
	case err := <-listenErr:
		if !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	}
	cancel()

	deadline, done := context.WithTimeout(context.Background(), timeout)
	defer done()
	if err := srv.Shutdown(deadline); err != nil {
		logger.Warn("in-flight requests did not finish in time; closing their connections", "error", err)
		_ = srv.Close()
	}

	finished := make(chan struct{})
	go func() {
		background.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		logger.Info("shutdown complete")
	case <-deadline.Done():
		logger.Warn("background work did not stop in time; exiting anyway")
	}
	return result
}

// isLoopback reports whether a listen address is reachable only from this machine.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "localhost":
		return true
	case "", "0.0.0.0", "::", "[::]":
		// An empty host means "all interfaces", which is the case this check exists for.
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func displayHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "localhost" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

// bootstrap creates the first tenant and prints a token.
//
// It is a separate subcommand rather than an API endpoint on purpose: the very first
// credential in a system cannot be authenticated by that system, so it is issued by someone
// with database access, once.
//
// Because the single-host deployment (DESIGN.md §28.1) runs the CLI next to the database it
// just bootstrapped, the minted token is also written straight into the operator's login
// file — the copy-paste `conductor login` round trip only remains for other machines.
func bootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	dsn := fs.String("dsn", envOr("DATABASE_URL", ""), "PostgreSQL connection string")
	orgSlug := fs.String("org", "default", "organization slug")
	projectSlug := fs.String("project", "", "project slug (defaults to the repository directory name)")
	handle := fs.String("principal", envOr("USER", "operator"), "principal handle")
	repo := fs.String("repo", ".", "path to the repository this project coordinates")
	role := fs.String("role", string(domain.RoleProjectAdmin), "role to grant the principal")
	endpoint := fs.String("endpoint", envOr("CONDUCTOR_PUBLIC_URL", "http://localhost:8080"),
		"control plane URL saved into this machine's login")
	noLogin := fs.Bool("no-login", false, "do not write this machine's login file; print the token only")
	owner := fs.Bool("owner", false, "make this principal the machine's owner even if another is already set (local sign-in acts as the owner)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return errors.New("no database configured: pass --dsn or set DATABASE_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := db.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	repoPath, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	if *projectSlug == "" {
		*projectSlug = filepath.Base(repoPath)
	}

	// Idempotent: re-running bootstrap should mint a fresh token, not fail because the org
	// already exists.
	org, err := store.GetOrganizationBySlug(ctx, *orgSlug)
	if errors.Is(err, domain.ErrNotFound) {
		org, err = store.CreateOrganization(ctx, *orgSlug, *orgSlug)
	}
	if err != nil {
		return fmt.Errorf("organization: %w", err)
	}

	principal, err := store.GetPrincipalByHandle(ctx, org.ID, *handle)
	if errors.Is(err, domain.ErrNotFound) {
		principal, err = store.CreatePrincipal(ctx, org.ID, domain.PrincipalHuman, *handle, *handle, "")
	}
	if err != nil {
		return fmt.Errorf("principal: %w", err)
	}

	// Load repository policy if the repo has a .conductor directory.
	bundle, err := config.Load(repoPath)
	if err != nil {
		return fmt.Errorf("load policy: %w", err)
	}

	project, err := store.GetProjectBySlug(ctx, org.ID, *projectSlug)
	if errors.Is(err, domain.ErrNotFound) {
		project, err = store.CreateProject(ctx, db.CreateProjectParams{
			OrganizationID:  org.ID,
			Slug:            *projectSlug,
			DisplayName:     firstNonEmpty(bundle.Project.Metadata.DisplayName, *projectSlug),
			CanonicalRemote: bundle.Project.Repository.CanonicalRemote,
			DefaultBranch:   firstNonEmpty(bundle.Project.Repository.DefaultBranch, "main"),
			WorktreeRoot:    bundle.Project.Repository.WorktreeRoot,
			RepoPath:        repoPath,
			Config:          bundle.ProjectConfig(),
		})
	} else if err == nil {
		err = store.UpdateProjectConfig(ctx, project.ID, bundle.ProjectConfig())
		if err == nil {
			err = store.SetProjectRepoPath(ctx, project.ID, repoPath)
		}
	}
	if err != nil {
		return fmt.Errorf("project: %w", err)
	}
	// Record the repository the project governs, so the GitHub App can match pull requests
	// to it. project.yaml wins; otherwise the checkout's own origin.
	if project.CanonicalRemote == "" {
		if remote := gitOrigin(repoPath); remote != "" {
			if err := store.SetProjectRemote(ctx, project.ID, remote); err == nil {
				project.CanonicalRemote = remote
			}
		}
	}

	if err := store.AddMember(ctx, project.ID, principal.ID, domain.Role(*role)); err != nil {
		return fmt.Errorf("membership: %w", err)
	}

	// The first person to bootstrap a machine owns it: local sign-in (`conductor security`)
	// acts as them. A later bootstrap for a teammate does not take that over unless --owner.
	isOwner := false
	if principal.Kind == domain.PrincipalHuman || principal.Kind == "" {
		isOwner, err = store.SetLocalOwner(ctx, principal.ID, !*owner)
		if err != nil {
			return fmt.Errorf("owner: %w", err)
		}
	}

	if bundle.Workflow.Raw != "" {
		if _, err := store.PutWorkflow(ctx, project.ID, bundle.Workflow.Raw, nil); err != nil {
			return fmt.Errorf("workflow: %w", err)
		}
	}
	for _, profile := range bundle.ModelProfiles(org.ID) {
		if _, err := store.UpsertModelProfile(ctx, profile); err != nil {
			return fmt.Errorf("model profile %s/%s: %w", profile.Alias, profile.Harness, err)
		}
	}

	token, err := store.CreateToken(ctx, principal.ID, "bootstrap", 0)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}

	loginSaved := false
	if !*noLogin {
		creds := client.Credentials{
			Endpoint: *endpoint,
			Token:    token,
			Project:  project.Slug,
			Handle:   principal.Handle,
		}
		if err := client.SaveCredentials(creds); err != nil {
			// The tenant exists and the token was minted; losing the convenience of a
			// saved login must not fail the bootstrap. The printed login line still works.
			fmt.Fprintf(os.Stderr, "bootstrap: could not save login: %v\n", err)
		} else {
			loginSaved = true
		}
	}

	fmt.Printf(`Conductor is bootstrapped.

  organization  %s
  project       %s  (%s)
  principal     %s  (%s)
  repository    %s
`, org.Slug, project.Slug, project.ID, principal.Handle, *role, repoPath)

	if loginSaved {
		path, _ := client.CredentialsPath()
		fmt.Printf("\nLogged in as %s — credentials saved to %s (mode 0600).\n", principal.Handle, path)
	} else {
		fmt.Println("\nThis machine was not logged in (--no-login).")
	}
	if isOwner {
		fmt.Printf("%s owns this machine: the dashboard at %s signs them in without a token.\n", principal.Handle, *endpoint)
	}

	fmt.Printf(`
To log in on another machine, or again on this one:

  conductor login --endpoint %s --token %s --project %s

The token is shown once and is stored only as a hash. Keep it out of shared logs.
`, *endpoint, token, project.Slug)
	return nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// envInt reads an integer from the environment; an unset or malformed value is the fallback
// (a malformed one is then reported by flag parsing only if passed as a flag).
func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return fallback
}

// peerFlags collects repeated --peer name=url flags.
type peerFlags []peer.Peer

func (p *peerFlags) String() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%v", []peer.Peer(*p))
}

func (p *peerFlags) Set(v string) error {
	name, url, ok := strings.Cut(v, "=")
	if !ok || name == "" || url == "" {
		return fmt.Errorf("expected name=https://host:port, got %q", v)
	}
	*p = append(*p, peer.Peer{Name: name, URL: url})
	return nil
}

// mergeEnv appends the CONDUCTOR_PEERS comma-separated list (name=url,name=url) to the
// flags. Flags win only in the sense that both are kept; a name used twice is rejected by
// peer.New.
func (p *peerFlags) mergeEnv(env string) ([]peer.Peer, error) {
	out := []peer.Peer(*p)
	for _, entry := range strings.Split(env, ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		name, url, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(url) == "" {
			return nil, fmt.Errorf("CONDUCTOR_PEERS entry %q must be name=https://host:port", entry)
		}
		out = append(out, peer.Peer{Name: strings.TrimSpace(name), URL: strings.TrimSpace(url)})
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// isLoopbackURL reports whether a URL's host is loopback.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// gitOrigin returns the checkout's origin URL, or "".
func gitOrigin(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
