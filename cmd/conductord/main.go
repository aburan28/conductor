// Command conductord is the Conductor control plane: REST API, SSE event stream, dashboard,
// and the background scheduler.
//
// It is one process because DESIGN.md §28.1 targets a single host first. Nothing here
// prevents running the API and the scheduler separately later — every scheduler step is
// transactional and `SKIP LOCKED`-based, so replicas are safe without leader election.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/adamburan/conductor/internal/api"
	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
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
		fmt.Fprintln(os.Stderr, "conductord:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("conductord", flag.ExitOnError)
	addr := fs.String("addr", envOr("CONDUCTOR_ADDR", "127.0.0.1:8080"), "listen address")
	dsn := fs.String("dsn", envOr("DATABASE_URL", ""), "PostgreSQL connection string")
	tick := fs.Duration("tick", 2*time.Second, "scheduler tick interval")
	detect := fs.Duration("detect-every", 15*time.Second, "conflict graph recomputation interval")
	noScheduler := fs.Bool("no-scheduler", false, "serve the API without running the scheduler")
	tlsCert := fs.String("tls-cert", envOr("CONDUCTOR_TLS_CERT", ""), "TLS certificate file")
	tlsKey := fs.String("tls-key", envOr("CONDUCTOR_TLS_KEY", ""), "TLS private key file")
	behindProxy := fs.Bool("behind-proxy", false,
		"trust X-Forwarded-For (only set this when a proxy you control rewrites it)")
	insecure := fs.Bool("insecure", false,
		"permit binding a non-loopback address without TLS")
	publicURL := fs.String("public-url", envOr("CONDUCTOR_PUBLIC_URL", ""),
		"URL at which clients reach this server (used for the MCP endpoint and self-calls); defaults to the bind address")
	peers := &peerFlags{}
	fs.Var(peers, "peer", "peer daemon as name=https://host:port (repeatable; CONDUCTOR_PEERS accepts a comma-separated list)")
	peerCA := fs.String("peer-ca", envOr("CONDUCTOR_PEER_CA", ""),
		"mesh CA bundle (PEM) that peer certificates are signed by; enables the peer surface")
	peerCert := fs.String("peer-cert", envOr("CONDUCTOR_PEER_CERT", ""),
		"this daemon's mesh certificate (PEM), presented to peers and served as the TLS certificate when --tls-cert is absent")
	peerKey := fs.String("peer-key", envOr("CONDUCTOR_PEER_KEY", ""),
		"this daemon's mesh private key")
	verbose := fs.Bool("v", false, "verbose logging")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `conductord — Conductor control plane

Usage:
  conductord [flags]
  conductord bootstrap [flags]     create an organization, project, principal, and token

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return errors.New("no database configured: pass --dsn or set DATABASE_URL")
	}

	tlsEnabled := *tlsCert != "" && *tlsKey != ""
	if (*tlsCert == "") != (*tlsKey == "") {
		return errors.New("--tls-cert and --tls-key must be given together")
	}
	meshPeers, err := peers.mergeEnv(os.Getenv("CONDUCTOR_PEERS"))
	if err != nil {
		return err
	}
	// The mesh identity is one certificate in two roles: presented as the TLS client
	// certificate when dialing peers, and served as this daemon's TLS certificate. A peer
	// verifying the server against the mesh CA is what makes the link mutual, so a
	// separate non-mesh --tls-cert cannot coexist with peering.
	meshOn := *peerCA != "" || *peerCert != "" || *peerKey != "" || len(meshPeers) > 0
	if (*peerCert == "") != (*peerKey == "") {
		return errors.New("--peer-cert and --peer-key must be given together")
	}
	if meshOn {
		if *peerCA == "" || *peerCert == "" {
			return errors.New("peering requires --peer-ca, --peer-cert and --peer-key together")
		}
		if *tlsCert != "" && *tlsCert != *peerCert {
			return errors.New("--tls-cert must be the mesh certificate when peering (pass --peer-cert as --tls-cert, or drop --tls-cert)")
		}
		if *tlsCert == "" {
			*tlsCert, *tlsKey = *peerCert, *peerKey
			tlsEnabled = true
		}
	}
	// Bearer tokens cross this connection. Binding a reachable address in plaintext puts
	// them on the wire, so it requires saying so out loud — a default that fails safe is
	// worth more than one that is convenient.
	if !tlsEnabled && !*insecure && !isLoopback(*addr) && !*behindProxy {
		return fmt.Errorf(`refusing to serve %s without TLS.

Bearer tokens would cross the network in the clear. Choose one:

  --tls-cert cert.pem --tls-key key.pem   terminate TLS here
  --peer-ca ca.pem --peer-cert/--peer-key your mesh identity (implies TLS)
  --behind-proxy                          a proxy you control terminates TLS
  --insecure                              you accept the risk (trusted network only)

Binding 127.0.0.1 needs none of these.`, *addr)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := db.Open(ctx, *dsn)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	logger.Info("database ready")

	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	selfEndpoint := *publicURL
	if selfEndpoint == "" {
		selfEndpoint = scheme + "://" + displayHost(*addr)
	}

	// Mesh identity and, when peers are configured, the link keeper that dials them.
	// Link state is a projection, so it lives in memory and disappears with the process.
	var (
		meshName   string
		meshPool   *x509.CertPool
		peerStatus func() []peer.LinkStatus
	)
	if meshOn {
		meshPool, err = peer.LoadCA(*peerCA)
		if err != nil {
			return fmt.Errorf("mesh CA: %w", err)
		}
		ourCert, err := peer.LoadCert(*peerCert, *peerKey)
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
		if len(meshPeers) > 0 {
			mgr, err := peer.New(peer.Options{
				Peers: meshPeers, SelfURL: selfEndpoint,
				CAPath: *peerCA, CertPath: *peerCert, KeyPath: *peerKey,
				Logger: logger,
			})
			if err != nil {
				return err
			}
			go func() {
				if err := mgr.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Error("peer link keeper exited", "error", err)
				}
			}()
			peerStatus = mgr.Snapshot
		}
	}

	svc := coord.New(store)
	server := api.New(store, svc, api.Options{
		Logger:       logger,
		Web:          web.Handler(),
		BehindProxy:  *behindProxy,
		TLSEnabled:   tlsEnabled,
		SelfEndpoint: selfEndpoint,
		PeerName:     meshName,
		PeerStatus:   peerStatus,
	})

	if !*noScheduler {
		sched := scheduler.New(store, svc, scheduler.Options{
			Tick: *tick, DetectEvery: *detect, Logger: logger,
		})
		go func() {
			if err := sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("scheduler exited", "error", err)
			}
		}()
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE event stream is a long-lived response, and a write
		// deadline would sever every dashboard connection on a fixed interval.
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	if tlsEnabled {
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
		if meshOn {
			// Verify peer certificates when presented, without demanding one from human
			// clients. The /v1/peer/* routes are the only consumers of the result.
			httpServer.TLSConfig.ClientCAs = meshPool
			httpServer.TLSConfig.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}
	logger.Info("conductor listening",
		"addr", *addr, "tls", tlsEnabled, "behind_proxy", *behindProxy,
		"mesh", meshOn,
		"dashboard", selfEndpoint+"/", "mcp", selfEndpoint+"/mcp")

	if tlsEnabled {
		return httpServer.ListenAndServeTLS(*tlsCert, *tlsKey)
	}
	return httpServer.ListenAndServe()
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

	if err := store.AddMember(ctx, project.ID, principal.ID, domain.Role(*role)); err != nil {
		return fmt.Errorf("membership: %w", err)
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
