// Package peer maintains daemon-to-daemon links between Conductor control planes.
//
// A mesh is a set of conductord instances, each holding a certificate signed by a shared
// mesh CA. Every daemon dials its configured peers over mutual TLS — presenting its own
// mesh certificate and verifying the peer's against the same CA — and records whether the
// link is up, what it costs in round-trip time, and who answered. Link state is a
// projection, like presence: in-memory, recomputed on every probe, and never a source of
// truth. Nothing is replicated across the link; this package is connectivity and
// identity, and leaves coordination data where it lives (DESIGN.md §28).
package peer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Peer is one configured remote daemon: a name chosen by the operator and the URL it is
// reached at. Peering is TLS-only, so the URL scheme must be https.
type Peer struct {
	Name string
	URL  string
}

// Info is what a daemon reports about itself over the peer link (GET /v1/peer/info).
// It is deliberately just identity: a mesh certificate names a daemon, not a project
// member, and no handler may read project data without a resolved membership. Peering is
// connectivity and identity, not a side door past that rule.
type Info struct {
	Name string    `json:"name"`
	Time time.Time `json:"time"`
}

// LinkState is the reachability of one configured peer.
type LinkState string

const (
	StateUp   LinkState = "up"   // answered /v1/peer/info with a verified certificate
	StateDown LinkState = "down" // unreachable, unverified, or erroring — see LastError
	StateSelf LinkState = "self" // this daemon's own endpoint, listed for completeness
)

// LinkStatus is one peer's current link state, safe to serialize to a project member.
type LinkStatus struct {
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	State      LinkState `json:"state"`
	RTTMillis  int64     `json:"rtt_ms,omitempty"`
	LastCheck  time.Time `json:"last_check"`
	LastError  string    `json:"last_error,omitempty"`
	Remote     *Info     `json:"remote,omitempty"`
	Discovered bool      `json:"discovered,omitempty"` // found via DiscoverDNS, not --peer
}

// SRVResolver resolves a DNS SRV record set. *net.Resolver satisfies it with its usual
// method signature, so a test can substitute a fake without touching real DNS.
type SRVResolver interface {
	LookupSRV(ctx context.Context, service, proto, name string) (cname string, addrs []*net.SRV, err error)
}

// Options configures a Manager.
type Options struct {
	Peers    []Peer
	SelfURL  string // this daemon's public endpoint; a peer matching it is marked self
	CAPath   string // mesh CA bundle (PEM): the only root trusted for peer certificates
	CertPath string
	KeyPath  string
	Tick     time.Duration // probe interval; defaults to 10s
	Timeout  time.Duration // per-probe timeout; defaults to 5s
	Logger   *slog.Logger

	// DiscoverDNS, when set, is a DNS SRV record name (RFC 2782, e.g.
	// "_conductor-mesh._tcp.mesh.internal") resolved on every tick to find mesh peers
	// automatically. It composes with Peers rather than replacing it: discovery only adds
	// links, so it lets a mesh grow without an operator hand-listing every daemon's
	// address, while a hand-listed peer is never displaced by a DNS hiccup. At least one
	// of Peers or DiscoverDNS is required — peering needs somewhere to start.
	DiscoverDNS string
	// Resolver performs the DiscoverDNS lookup. Defaults to net.DefaultResolver.
	Resolver SRVResolver
}

// Manager dials the configured peers on a ticker and records link state.
type Manager struct {
	opts    Options
	client  *http.Client
	selfURL string
	mu      sync.Mutex
	links   []LinkStatus
}

// LoadCA reads a mesh CA bundle. The returned pool trusts only this file — not the system
// roots — so a peer certificate signed by any public CA can never pass as a mesh member.
func LoadCA(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in %s", path)
	}
	return pool, nil
}

// LoadCert reads this daemon's mesh certificate and key.
func LoadCert(certPath, keyPath string) (tls.Certificate, error) {
	return tls.LoadX509KeyPair(certPath, keyPath)
}

// CertName is the daemon's mesh identity: the first DNS SAN, falling back to the common
// name. Both sides of a link use this to name each other.
func CertName(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0]
	}
	if cert.Subject.CommonName != "" {
		return cert.Subject.CommonName
	}
	return cert.Subject.String()
}

// New validates the peer list and loads the mesh TLS material.
func New(opts Options) (*Manager, error) {
	if len(opts.Peers) == 0 && opts.DiscoverDNS == "" {
		return nil, fmt.Errorf("no peers configured (pass Peers or DiscoverDNS)")
	}
	if opts.CAPath == "" {
		return nil, fmt.Errorf("peering requires --peer-ca (the mesh CA bundle)")
	}
	if opts.CertPath == "" || opts.KeyPath == "" {
		return nil, fmt.Errorf("peering requires --peer-cert and --peer-key together")
	}
	pool, err := LoadCA(opts.CAPath)
	if err != nil {
		return nil, fmt.Errorf("mesh CA: %w", err)
	}
	cert, err := LoadCert(opts.CertPath, opts.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("mesh certificate: %w", err)
	}
	if opts.Tick <= 0 {
		opts.Tick = 10 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}

	seen := make(map[string]bool, len(opts.Peers))
	links := make([]LinkStatus, 0, len(opts.Peers))
	for _, p := range opts.Peers {
		if p.Name == "" {
			return nil, fmt.Errorf("peer %q has no name (expected name=https://host:port)", p.URL)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("peer name %q configured twice", p.Name)
		}
		seen[p.Name] = true
		u, err := url.Parse(p.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			// mTLS is the whole point of the link; a plaintext peer would put the mesh
			// certificate's identity on an unauthenticated wire.
			return nil, fmt.Errorf("peer %s: URL must be https://host:port, got %q", p.Name, p.URL)
		}
		links = append(links, LinkStatus{Name: p.Name, URL: strings.TrimRight(p.URL, "/"), State: StateDown})
	}

	return &Manager{
		opts:    opts,
		selfURL: strings.TrimRight(opts.SelfURL, "/"),
		// The mesh pool is the only root: a peer is verified against the shared CA,
		// never against public trust.
		client: &http.Client{
			Timeout: opts.Timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:      pool,
					Certificates: []tls.Certificate{cert},
					MinVersion:   tls.VersionTLS12,
				},
			},
		},
		links: links,
	}, nil
}

// Run discovers and probes every peer once immediately, then on every tick, until ctx is
// cancelled.
func (m *Manager) Run(ctx context.Context) error {
	m.Discover(ctx)
	m.Probe(ctx)
	ticker := time.NewTicker(m.opts.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.Discover(ctx)
			m.Probe(ctx)
		}
	}
}

// Discover resolves DiscoverDNS, if configured, and merges any newly found peers into the
// link table. It only adds: a target that drops out of DNS keeps whatever link state it
// last had, so a transient resolver hiccup never drops a peer that is otherwise reachable.
// A discovered peer starts under a provisional name (its DNS target) — see probeOne for
// how it adopts its real identity.
func (m *Manager) Discover(ctx context.Context) {
	if m.opts.DiscoverDNS == "" {
		return
	}
	found, err := ResolveSRV(ctx, m.opts.Resolver, m.opts.DiscoverDNS)
	if err != nil {
		m.opts.Logger.Warn("peer discovery failed", "source", m.opts.DiscoverDNS, "error", err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	known := make(map[string]bool, len(m.links))
	for _, l := range m.links {
		known[strings.TrimRight(l.URL, "/")] = true
	}
	for _, p := range found {
		url := strings.TrimRight(p.URL, "/")
		if known[url] || url == m.selfURL {
			continue
		}
		known[url] = true
		m.links = append(m.links, LinkStatus{Name: p.Name, URL: url, State: StateDown, Discovered: true})
	}
}

// Probe checks every configured peer concurrently and records the result. A peer whose
// URL is this daemon's own endpoint is marked self without dialing.
func (m *Manager) Probe(ctx context.Context) {
	var wg sync.WaitGroup
	results := make([]LinkStatus, len(m.links))
	for i, l := range m.snapshotLocked() {
		results[i] = l
		if m.selfURL != "" && strings.TrimRight(l.URL, "/") == m.selfURL {
			results[i].State = StateSelf
			results[i].LastCheck = time.Now()
			results[i].RTTMillis = 0
			results[i].LastError = ""
			continue
		}
		wg.Add(1)
		go func(i int, l LinkStatus) {
			defer wg.Done()
			results[i] = m.probeOne(ctx, l)
		}(i, l)
	}
	wg.Wait()

	m.mu.Lock()
	m.links = results
	m.mu.Unlock()
}

// Snapshot returns a copy of the current link states, in configured order.
func (m *Manager) Snapshot() []LinkStatus {
	out := m.snapshotLocked()
	for i := range out {
		cp := out[i]
		if out[i].Remote != nil {
			info := *out[i].Remote
			cp.Remote = &info
		}
		out[i] = cp
	}
	return out
}

func (m *Manager) snapshotLocked() []LinkStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]LinkStatus, len(m.links))
	copy(out, m.links)
	return out
}

func (m *Manager) probeOne(ctx context.Context, l LinkStatus) LinkStatus {
	l.LastCheck = time.Now()
	l.State = StateDown
	l.Remote = nil
	l.RTTMillis = 0

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL+"/v1/peer/info", nil)
	if err != nil {
		l.LastError = err.Error()
		return l
	}
	start := time.Now()
	resp, err := m.client.Do(req)
	l.RTTMillis = time.Since(start).Milliseconds()
	if err != nil {
		l.LastError = err.Error()
		return l
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		l.LastError = fmt.Sprintf("peer answered %s for /v1/peer/info", resp.Status)
		return l
	}
	var info Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		l.LastError = fmt.Sprintf("unintelligible peer info: %v", err)
		return l
	}
	if info.Name != "" && info.Name != l.Name {
		if l.Discovered {
			// A discovered peer has no operator-configured name to compare against —
			// only the DNS target it was found under. Its certificate and info report
			// are the actual source of identity here, same as the rest of this package;
			// adopt it rather than reporting a "mismatch" against a placeholder.
			l.Name = info.Name
		} else {
			// The certificate answered, but under a different name than the operator
			// configured. Report it under the configured name and surface the mismatch
			// rather than hide it.
			l.LastError = fmt.Sprintf("peer answered as %q, configured as %q", info.Name, l.Name)
		}
	}
	l.State = StateUp
	l.Remote = &info
	return l
}
