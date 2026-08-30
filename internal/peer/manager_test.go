package peer_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/peer"
)

// Mesh fixtures: one CA and any number of named daemon certificates, mirroring what
// scripts/gen-peer-certs.sh produces (one certificate, serverAuth and clientAuth).

type meshCA struct {
	cert *x509.Certificate
	key  crypto.Signer
	pem  []byte
}

func genCA(t *testing.T) meshCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test mesh CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return meshCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func genCert(t *testing.T, ca meshCA, name string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name, "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// writeMeshFiles persists a daemon identity to disk in the shape peer.Options expects.
func writeMeshFiles(t *testing.T, ca meshCA, name string) (caPath, certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPEM, keyPEM := genCert(t, ca, name)
	caPath = filepath.Join(dir, "ca.pem")
	certPath = filepath.Join(dir, name+"-cert.pem")
	keyPath = filepath.Join(dir, name+"-key.pem")
	for path, body := range map[string][]byte{
		caPath:   ca.pem,
		certPath: certPEM,
		keyPath:  keyPEM,
	} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return caPath, certPath, keyPath
}

// newMeshServer serves a stand-in control plane that answers /v1/peer/info with its own
// mesh name, verifying client certificates against the mesh CA exactly like conductord.
func newMeshServer(t *testing.T, ca meshCA, name string) string {
	t.Helper()
	return newMeshServerAdvertising(t, ca, name, nil)
}

// newMeshServerAdvertising is newMeshServer with a mesh advertisement: the named daemon
// claims these peers in its /v1/peer/info response, the way a discovery-enabled daemon
// lists everyone it can dial.
func newMeshServerAdvertising(t *testing.T, ca meshCA, name string, mesh []peer.Peer) string {
	t.Helper()
	certPEM, keyPEM := genCert(t, ca, name)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/peer/info", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(peer.Info{Name: name, Time: time.Now(), Mesh: mesh})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			ClientCAs:    pool,
			ClientAuth:   tls.VerifyClientCertIfGiven,
		},
	}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	return "https://" + ln.Addr().String()
}

func TestNewRejectsPlaintextPeers(t *testing.T) {
	ca := genCA(t)
	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	_, err := peer.New(peer.Options{
		Peers:    []peer.Peer{{Name: "beta", URL: "http://127.0.0.1:9"}},
		CAPath:   caPath,
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err == nil {
		t.Fatal("expected a plaintext peer URL to be rejected: mTLS is the point of the link")
	}
}

func TestNewRejectsPartialIdentity(t *testing.T) {
	ca := genCA(t)
	caPath, certPath, _ := writeMeshFiles(t, ca, "alpha")
	if _, err := peer.New(peer.Options{Peers: []peer.Peer{{Name: "beta", URL: "https://127.0.0.1:9"}},
		CAPath: caPath, CertPath: certPath}); err == nil {
		t.Fatal("expected a keyless identity to be rejected")
	}
	if _, err := peer.New(peer.Options{Peers: []peer.Peer{{Name: "beta", URL: "https://127.0.0.1:9"}},
		CertPath: certPath}); err == nil {
		t.Fatal("expected a missing CA to be rejected")
	}
}

func TestNewRejectsDuplicateNames(t *testing.T) {
	ca := genCA(t)
	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	_, err := peer.New(peer.Options{
		Peers: []peer.Peer{
			{Name: "beta", URL: "https://127.0.0.1:9"},
			{Name: "beta", URL: "https://127.0.0.1:10"},
		},
		CAPath: caPath, CertPath: certPath, KeyPath: keyPath,
	})
	if err == nil {
		t.Fatal("expected a duplicate peer name to be rejected")
	}
}

func TestProbeUp(t *testing.T) {
	ca := genCA(t)
	betaURL := newMeshServer(t, ca, "beta")

	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	m, err := peer.New(peer.Options{
		Peers:    []peer.Peer{{Name: "beta", URL: betaURL}},
		CAPath:   caPath,
		CertPath: certPath,
		KeyPath:  keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Probe(context.Background())

	links := m.Snapshot()
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}
	l := links[0]
	if l.State != peer.StateUp {
		t.Fatalf("expected beta up, got %q (%s)", l.State, l.LastError)
	}
	if l.Remote == nil || l.Remote.Name != "beta" {
		t.Fatalf("expected remote name beta, got %+v", l.Remote)
	}
	if l.LastError != "" {
		t.Fatalf("expected no error, got %q", l.LastError)
	}
}

func TestProbeDown(t *testing.T) {
	ca := genCA(t)
	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	m, err := peer.New(peer.Options{
		Peers:  []peer.Peer{{Name: "ghost", URL: "https://127.0.0.1:1"}},
		CAPath: caPath, CertPath: certPath, KeyPath: keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Probe(context.Background())

	l := m.Snapshot()[0]
	if l.State != peer.StateDown {
		t.Fatalf("expected ghost down, got %q", l.State)
	}
	if l.LastError == "" {
		t.Fatal("expected a recorded failure reason")
	}
}

func TestProbeSelf(t *testing.T) {
	ca := genCA(t)
	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	m, err := peer.New(peer.Options{
		Peers:   []peer.Peer{{Name: "alpha", URL: "https://127.0.0.1:8443/"}},
		SelfURL: "https://127.0.0.1:8443",
		CAPath:  caPath, CertPath: certPath, KeyPath: keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Probe(context.Background())

	l := m.Snapshot()[0]
	if l.State != peer.StateSelf {
		t.Fatalf("expected self, got %q", l.State)
	}
	if l.LastError != "" {
		t.Fatalf("self should not produce an error, got %q", l.LastError)
	}
}

func TestProbeUntrustedPeer(t *testing.T) {
	// A server certified by another CA must never pass as a peer, even if reachable.
	otherCA := genCA(t)
	strangerURL := newMeshServer(t, otherCA, "stranger")

	ourCA := genCA(t)
	caPath, certPath, keyPath := writeMeshFiles(t, ourCA, "alpha")
	m, err := peer.New(peer.Options{
		Peers:  []peer.Peer{{Name: "stranger", URL: strangerURL}},
		CAPath: caPath, CertPath: certPath, KeyPath: keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Probe(context.Background())

	l := m.Snapshot()[0]
	if l.State != peer.StateDown {
		t.Fatalf("expected an untrusted peer to be down, got %q", l.State)
	}
	if l.LastError == "" {
		t.Fatal("expected the TLS verification failure to be recorded")
	}
}

func TestProbeMismatchedName(t *testing.T) {
	// Reachable and trusted, but answering as someone else: the operator configured
	// "beta" and the daemon on the other end says "gamma". Up, with the mismatch flagged.
	ca := genCA(t)
	gammaURL := newMeshServer(t, ca, "gamma")

	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	m, err := peer.New(peer.Options{
		Peers:  []peer.Peer{{Name: "beta", URL: gammaURL}},
		CAPath: caPath, CertPath: certPath, KeyPath: keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Probe(context.Background())

	l := m.Snapshot()[0]
	if l.State != peer.StateUp {
		t.Fatalf("expected up, got %q (%s)", l.State, l.LastError)
	}
	if l.LastError == "" {
		t.Fatal("expected the name mismatch to be reported")
	}
}

func TestCertName(t *testing.T) {
	ca := genCA(t)
	certPEM, keyPEM := genCert(t, ca, "alpha")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := peer.CertName(leaf); got != "alpha" {
		t.Fatalf("expected alpha, got %q", got)
	}
	if got := peer.CertName(nil); got != "" {
		t.Fatalf("expected empty name for nil cert, got %q", got)
	}
}

// newDiscoveryManager wires a daemon named alpha to the given peer with discovery on.
func newDiscoveryManager(t *testing.T, ca meshCA, discovery bool, peers ...peer.Peer) *peer.Manager {
	t.Helper()
	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	m, err := peer.New(peer.Options{
		Peers: peers, CAPath: caPath, CertPath: certPath, KeyPath: keyPath,
		Discovery: discovery,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// findLink returns the link with the given name, failing the test when absent.
func findLink(t *testing.T, links []peer.LinkStatus, name string) peer.LinkStatus {
	t.Helper()
	for _, l := range links {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("no link named %q in %+v", name, links)
	return peer.LinkStatus{}
}

// The mesh one probe at a time: alpha is configured with beta only; beta advertises
// gamma; gamma is live but never configured anywhere. After two probes alpha dials gamma
// on its own, and the link says where it came from.
func TestDiscoveryLearnsAdvertisedPeers(t *testing.T) {
	ca := genCA(t)
	gammaURL := newMeshServer(t, ca, "gamma")
	betaURL := newMeshServerAdvertising(t, ca, "beta", []peer.Peer{
		{Name: "gamma", URL: gammaURL},
	})

	m := newDiscoveryManager(t, ca, true, peer.Peer{Name: "beta", URL: betaURL})
	m.Probe(context.Background()) // beta up; gamma discovered, still down
	m.Probe(context.Background()) // gamma probed and up

	gamma := findLink(t, m.Snapshot(), "gamma")
	if gamma.State != peer.StateUp {
		t.Fatalf("expected discovered gamma up, got %q (%s)", gamma.State, gamma.LastError)
	}
	if gamma.Source != peer.SourceDiscovered || gamma.Via != "beta" {
		t.Fatalf("expected discovered via beta, got source=%q via=%q", gamma.Source, gamma.Via)
	}
	beta := findLink(t, m.Snapshot(), "beta")
	if beta.Source != peer.SourceConfig {
		t.Fatalf("configured beta must stay config-sourced, got %q", beta.Source)
	}
}

// Discovery off: the advertisement is ignored and the roster stays what the operator set.
func TestDiscoveryDisabledIgnoresAdvertisements(t *testing.T) {
	ca := genCA(t)
	gammaURL := newMeshServer(t, ca, "gamma")
	betaURL := newMeshServerAdvertising(t, ca, "beta", []peer.Peer{{Name: "gamma", URL: gammaURL}})

	m := newDiscoveryManager(t, ca, false, peer.Peer{Name: "beta", URL: betaURL})
	m.Probe(context.Background())
	m.Probe(context.Background())

	if links := m.Snapshot(); len(links) != 1 {
		t.Fatalf("expected the configured roster only, got %+v", links)
	}
}

// An advertisement is a hint, not an instruction: nameless entries, plaintext URLs,
// already-known addresses, and our own endpoint never become links.
func TestDiscoveryFiltersAdvertisements(t *testing.T) {
	ca := genCA(t)
	gammaURL := newMeshServer(t, ca, "gamma")
	betaURL := newMeshServerAdvertising(t, ca, "beta", []peer.Peer{
		{URL: gammaURL}, // nameless
		{Name: "plaintext", URL: "http://127.0.0.1:9"}, // not https
		{Name: "alpha", URL: "https://127.0.0.1:8443"}, // us
		{Name: "gamma", URL: gammaURL},                 // the one valid hint
		{Name: "gamma-dup", URL: gammaURL},             // duplicate URL: the first advertisement of a URL wins
	})

	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	m, err := peer.New(peer.Options{
		Peers:   []peer.Peer{{Name: "beta", URL: betaURL}},
		SelfURL: "https://127.0.0.1:8443",
		CAPath:  caPath, CertPath: certPath, KeyPath: keyPath,
		Discovery: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Probe(context.Background())

	links := m.Snapshot()
	if len(links) != 2 {
		t.Fatalf("expected beta + gamma only, got %d links: %+v", len(links), links)
	}
	findLink(t, links, "gamma")
}

// A compromised or chatty member cannot make the mesh dial without bound.
func TestDiscoveryCap(t *testing.T) {
	ca := genCA(t)
	mesh := make([]peer.Peer, 0, 200)
	for i := 0; i < 200; i++ {
		mesh = append(mesh, peer.Peer{Name: fmt.Sprintf("fake-%d", i), URL: fmt.Sprintf("https://127.0.0.1:1/%d", i)})
	}
	betaURL := newMeshServerAdvertising(t, ca, "beta", mesh)

	m := newDiscoveryManager(t, ca, true, peer.Peer{Name: "beta", URL: betaURL})
	m.Probe(context.Background())

	links := m.Snapshot()
	if len(links) != 1+128 {
		t.Fatalf("expected 1 configured + 128 discovered (cap), got %d", len(links)-1)
	}
}

// A discovered link has no operator to contradict: it adopts the name its certificate
// proves rather than the name the advertisement claimed.
func TestDiscoveredPeerAdoptsCertificateName(t *testing.T) {
	ca := genCA(t)
	gammaURL := newMeshServer(t, ca, "gamma")
	betaURL := newMeshServerAdvertising(t, ca, "beta", []peer.Peer{
		{Name: "not-gamma", URL: gammaURL},
	})

	m := newDiscoveryManager(t, ca, true, peer.Peer{Name: "beta", URL: betaURL})
	m.Probe(context.Background()) // learns "not-gamma"
	m.Probe(context.Background()) // dials it; the certificate says gamma

	gamma := findLink(t, m.Snapshot(), "gamma")
	if gamma.State != peer.StateUp {
		t.Fatalf("expected gamma up, got %q (%s)", gamma.State, gamma.LastError)
	}
	if gamma.LastError != "" {
		t.Fatalf("a discovered link should not flag a name mismatch, got %q", gamma.LastError)
	}
}
