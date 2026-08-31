// Package peer maintains daemon-to-daemon links between Conductor control planes.
//
// A mesh is a set of conductord instances, each holding a certificate signed by a shared
// mesh CA. Every daemon dials its configured peers over mutual TLS — presenting its own
// mesh certificate and verifying the peer's against the same CA — and records whether the
// link is up, what it costs in round-trip time, and who answered. Link state is a
// projection, like presence: in-memory, recomputed on every probe, and never a source of
// truth. Nothing is replicated across the link; this package is connectivity and
// identity, and leaves coordination data where it lives (DESIGN.md §28).
//
// With discovery enabled, a daemon also learns peers it was not configured with: every
// probe of /v1/peer/info carries the responder's view of the mesh, and advertised
// addresses join the probe set. An advertisement is only an address hint — a discovered
// daemon must still present a certificate chained to the mesh CA before its link counts
// as up, and a discovered link adopts the name its certificate proves. Discovery widens
// dialing; the CA still gates membership.
package peer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
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
// It is deliberately identity-shaped: a mesh certificate names a daemon, not a project
// member, and no handler may read project data without a resolved membership. Peering is
// connectivity and identity, not a side door past that rule.
type Info struct {
	Name string    `json:"name"`
	Time time.Time `json:"time"`
	// Mesh is everything this daemon can dial in the mesh — its configured and discovered
	// peers, plus its own endpoint — so a probing peer can discover members it was not
	// configured with. Names and addresses only; a discovered address still has to pass
	// the mesh CA at handshake before it is anything but a down link.
	Mesh []Peer `json:"mesh,omitempty"`
}

// LinkState is the reachability of one configured peer.
type LinkState string

const (
	StateUp   LinkState = "up"   // answered /v1/peer/info with a verified certificate
	StateDown LinkState = "down" // unreachable, unverified, or erroring — see LastError
	StateSelf LinkState = "self" // this daemon's own endpoint, listed for completeness
)

// How a link became known. A configured link came from the operator's --peer list; a
// discovered link was advertised by another peer over the mesh.
const (
	SourceConfig     = "config"
	SourceDiscovered = "discovered"
)

// maxDiscoveredPeers bounds how many advertised addresses a daemon adopts. The mesh CA
// bounds who can join the mesh; this bounds how much a compromised member can make the
// mesh dial.
const maxDiscoveredPeers = 128

// LinkStatus is one peer's current link state, safe to serialize to a project member.
type LinkStatus struct {
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	State     LinkState `json:"state"`
	RTTMillis int64     `json:"rtt_ms,omitempty"`
	LastCheck time.Time `json:"last_check"`
	LastError string    `json:"last_error,omitempty"`
	Remote    *Info     `json:"remote,omitempty"`
	// Source is how this link became known: "config" or "discovered" (empty for links
	// recorded before the field existed).
	Source string `json:"source,omitempty"`
	// Via names the peer that advertised a discovered link.
	Via string `json:"via,omitempty"`
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
	// Discovery enables learning peers from /v1/peer/info advertisements: what a daemon
	// can see becomes the union of what its links advertise. Discovery only adds
	// candidate addresses — membership is still gated by the mesh CA at handshake, and a
	// discovered link that cannot present a CA-signed certificate stays down.
	Discovery bool
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
	if len(opts.Peers) == 0 {
		return nil, fmt.Errorf("no peers configured")
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
		links = append(links, LinkStatus{Name: p.Name, URL: strings.TrimRight(p.URL, "/"), State: StateDown, Source: SourceConfig})
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

// Run probes every peer once immediately, then on every tick, until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	m.Probe(ctx)
	ticker := time.NewTicker(m.opts.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.Probe(ctx)
		}
	}
}

// advertisement is one peer's view of the mesh, as told to us over its link.
type advertisement struct {
	via  string // the peer that advertised it, by its certified name
	mesh []Peer
}

// Probe checks every known peer concurrently and records the result; when discovery is
// on, the members those peers advertised are merged afterwards, so the next probe dials
// them too. A peer whose URL is this daemon's own endpoint is marked self without
// dialing.
func (m *Manager) Probe(ctx context.Context) {
	var wg sync.WaitGroup
	links := m.snapshotLocked()
	results := make([]LinkStatus, len(links))
	ads := make([]advertisement, len(links))
	for i, l := range links {
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
			results[i], ads[i] = m.probeOne(ctx, l)
		}(i, l)
	}
	wg.Wait()

	// Merge by URL rather than replace: an inbound observation (a peer knocking on our
	// /v1/peer/info) can append a link while probes are in flight, and a plain replace
	// built from the pre-probe snapshot would drop it.
	m.mu.Lock()
	for _, r := range results {
		matched := false
		for j := range m.links {
			if m.links[j].URL == r.URL {
				m.links[j] = r
				matched = true
				break
			}
		}
		if !matched {
			m.links = append(m.links, r)
		}
	}
	m.mu.Unlock()

	for _, ad := range ads {
		m.mergeDiscovered(ad)
	}
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

func (m *Manager) probeOne(ctx context.Context, l LinkStatus) (LinkStatus, advertisement) {
	l.LastCheck = time.Now()
	l.State = StateDown
	l.Remote = nil
	l.RTTMillis = 0

	// A probing peer that runs discovery announces where it can be reached, so the daemon
	// it knocks on learns it too: without this, a new daemon pointing at the mesh is
	// invisible to members it never dials.
	target := l.URL + "/v1/peer/info"
	if m.opts.Discovery && m.selfURL != "" {
		target += "?from=" + url.QueryEscape(m.selfURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		l.LastError = err.Error()
		return l, advertisement{}
	}
	start := time.Now()
	resp, err := m.client.Do(req)
	l.RTTMillis = time.Since(start).Milliseconds()
	if err != nil {
		l.LastError = err.Error()
		return l, advertisement{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		l.LastError = fmt.Sprintf("peer answered %s for /v1/peer/info", resp.Status)
		return l, advertisement{}
	}
	var info Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		l.LastError = fmt.Sprintf("unintelligible peer info: %v", err)
		return l, advertisement{}
	}
	// The certificate answered, but under a different name than the operator configured.
	// Report it under the configured name and surface the mismatch rather than hide it.
	// A discovered link has no operator to contradict — the certificate IS the identity
	// there — so it adopts the name the daemon proved.
	if info.Name != "" && info.Name != l.Name {
		if l.Source == SourceDiscovered {
			l.Name = info.Name
		} else {
			l.LastError = fmt.Sprintf("peer answered as %q, configured as %q", info.Name, l.Name)
		}
	}
	l.State = StateUp
	l.Remote = &info
	var ad advertisement
	if m.opts.Discovery {
		ad = advertisement{via: l.Name, mesh: info.Mesh}
	}
	return l, ad
}

// adoption carries the per-merge view of the link table. The caller holds mu while
// building and spending it.
type adoption struct {
	known      map[string]bool
	discovered int
}

func (m *Manager) newAdoption() adoption {
	known := make(map[string]bool, len(m.links))
	discovered := 0
	for _, l := range m.links {
		known[l.URL] = true
		if l.Source == SourceDiscovered {
			discovered++
		}
	}
	return adoption{known: known, discovered: discovered}
}

// adopt validates and appends one advertised peer as a candidate link. It returns false
// for anything that is not worth dialing. Caller holds mu.
func (m *Manager) adopt(a *adoption, p Peer, via string) bool {
	u := strings.TrimRight(p.URL, "/")
	if p.Name == "" || u == "" || a.known[u] || u == m.selfURL {
		return false
	}
	if pu, err := url.Parse(u); err != nil || pu.Scheme != "https" || pu.Host == "" {
		return false
	}
	if a.discovered >= maxDiscoveredPeers {
		m.opts.Logger.Warn("peer discovery cap reached; ignoring advertised peer",
			"url", u, "via", via)
		return false
	}
	a.known[u] = true
	a.discovered++
	m.links = append(m.links, LinkStatus{
		Name: p.Name, URL: u, State: StateDown,
		Source: SourceDiscovered, Via: via,
	})
	return true
}

// mergeDiscovered adopts advertised mesh members as candidate links. An advertisement is
// an address hint, never an instruction: nameless entries, non-https URLs, URLs already
// known, and our own endpoint are ignored, and the operator's configuration always wins
// on a collision (dedup is by URL). The cap keeps a compromised member from turning the
// mesh into a dialer of arbitrary addresses.
func (m *Manager) mergeDiscovered(ad advertisement) {
	if len(ad.mesh) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	a := m.newAdoption()
	for _, p := range ad.mesh {
		m.adopt(&a, p, ad.via)
	}
}

// ObserveFrom records a peer that knocked: it probed us over mTLS with a verified mesh
// certificate (the name) and told us where it can be reached (the URL, a hint validated
// like any advertisement). This is the inbound half of discovery — without it, a daemon
// that points at the mesh is invisible to the members it never dials.
func (m *Manager) ObserveFrom(name, url string) {
	if !m.opts.Discovery || name == "" || url == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	a := m.newAdoption()
	m.adopt(&a, Peer{Name: name, URL: url}, name)
}
