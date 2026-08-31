package api

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
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/peer"
)

// The peer surface is the one part of the API whose authn is the TLS layer itself, so it
// is tested over a real TLS listener with client-certificate verification, not httptest.

type testCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func genTestCA(t *testing.T) testCA {
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
	return testCA{cert: cert, key: key}
}

func genTestCert(t *testing.T, ca testCA, name string) tls.Certificate {
	t.Helper()
	certPEM, keyPEM := genTestCertPEM(t, ca, name)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// genTestCertPEM is genTestCert in the on-disk shape peer.Options expects.
func genTestCertPEM(t *testing.T, ca testCA, name string) (certPEM, keyPEM []byte) {
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

// newPeerTLSServer serves the api handler behind mutual-TLS verification.
func newPeerTLSServer(t *testing.T, handler http.Handler, ca testCA, name string) (url string, pool *x509.CertPool) {
	t.Helper()
	pair := genTestCert(t, ca, name)
	pool = x509.NewCertPool()
	pool.AddCert(ca.cert)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: handler,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{pair},
			ClientCAs:    pool,
			ClientAuth:   tls.VerifyClientCertIfGiven,
		},
	}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	return "https://" + ln.Addr().String(), pool
}

func TestPeerInfoRequiresMeshCertificate(t *testing.T) {
	newHarness(t) // brings up the store the server is built on
	ca := genTestCA(t)

	status := func() []peer.LinkStatus {
		return []peer.LinkStatus{{Name: "beta", URL: "https://127.0.0.1:18444", State: peer.StateUp}}
	}
	srv := New(sharedStore, coord.New(sharedStore), Options{
		PeerName:     "alpha",
		PeerStatus:   status,
		SelfEndpoint: "https://127.0.0.1:1",
	})
	url, pool := newPeerTLSServer(t, srv.Handler(), ca, "alpha")

	// A plain TLS client with no client certificate: it may hold a perfectly good bearer
	// token, but /v1/peer/* is not a bearer surface — it must be refused.
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, MinVersion: tls.VersionTLS12}}}).Get(url + "/v1/peer/info")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a client certificate, got %d", resp.StatusCode)
	}

	// The same client presenting a mesh certificate identifies the caller, and the answer
	// is the daemon's own name — not an echo of the caller's.
	clientCert := genTestCert(t, ca, "beta")
	resp, err = (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
		MinVersion:   tls.VersionTLS12}}}).Get(url + "/v1/peer/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with a mesh certificate, got %d", resp.StatusCode)
	}
	var info peer.Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Name != "alpha" {
		t.Fatalf("expected the answering daemon's name (alpha), got %q", info.Name)
	}

	// The same answer carries the daemon's mesh advertisement: itself and everyone it
	// knows how to dial, so a discovery-enabled peer can learn members from it.
	if len(info.Mesh) != 2 {
		t.Fatalf("expected the advertisement to carry self + beta, got %+v", info.Mesh)
	}
	byName := map[string]string{}
	for _, p := range info.Mesh {
		byName[p.Name] = p.URL
	}
	if byName["alpha"] != "https://127.0.0.1:1" || byName["beta"] != "https://127.0.0.1:18444" {
		t.Fatalf("advertisement should name self and beta with their URLs, got %+v", info.Mesh)
	}
}

// The full loop, one machine: a discovery-enabled Manager (beta) knocks on a real api
// server (alpha) announcing its own endpoint. Alpha's wired receiver records beta as a
// discovered link — the inbound half of discovery that makes a new daemon visible to the
// mesh it points at.
func TestKnockTeachesTheReceiver(t *testing.T) {
	newHarness(t) // brings up the store the server is built on
	ca := genTestCA(t)

	// Alpha: a real Manager wired to the server both as link table and knock observer.
	// Its one configured peer is a dead address so it must learn beta purely inbound.
	dir := t.TempDir()
	alphaCert, alphaKey := genTestCertPEM(t, ca, "alpha")
	caPath := filepath.Join(dir, "ca.pem")
	alphaCertPath := filepath.Join(dir, "alpha-cert.pem")
	alphaKeyPath := filepath.Join(dir, "alpha-key.pem")
	for path, body := range map[string][]byte{
		caPath:        pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}),
		alphaCertPath: alphaCert,
		alphaKeyPath:  alphaKey,
	} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	receiver, err := peer.New(peer.Options{
		Peers:  []peer.Peer{{Name: "dead", URL: "https://127.0.0.1:1"}},
		CAPath: caPath, CertPath: alphaCertPath, KeyPath: alphaKeyPath,
		Discovery: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := New(sharedStore, coord.New(sharedStore), Options{
		PeerName:     "alpha",
		PeerStatus:   receiver.Snapshot,
		PeerObserve:  receiver.ObserveFrom,
		SelfEndpoint: "https://127.0.0.1:1",
	})
	alphaURL, _ := newPeerTLSServer(t, srv.Handler(), ca, "alpha")

	// Beta: a discovery-enabled Manager whose configured roster is alpha alone and whose
	// self URL points at a live stand-in of its own control plane.
	betaSrv := New(sharedStore, coord.New(sharedStore), Options{PeerName: "beta"})
	betaURL, _ := newPeerTLSServer(t, betaSrv.Handler(), ca, "beta")
	betaCert, betaKey := genTestCertPEM(t, ca, "beta")
	betaCertPath := filepath.Join(dir, "beta-cert.pem")
	betaKeyPath := filepath.Join(dir, "beta-key.pem")
	if err := os.WriteFile(betaCertPath, betaCert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(betaKeyPath, betaKey, 0o600); err != nil {
		t.Fatal(err)
	}
	knocker, err := peer.New(peer.Options{
		Peers:   []peer.Peer{{Name: "alpha", URL: alphaURL}},
		SelfURL: betaURL,
		CAPath:  caPath, CertPath: betaCertPath, KeyPath: betaKeyPath,
		Discovery: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	knocker.Probe(context.Background())

	beta := findLinkByURL(t, receiver.Snapshot(), betaURL)
	if beta.Name != "beta" || beta.Source != peer.SourceDiscovered || beta.Via != "beta" {
		t.Fatalf("expected beta recorded as discovered from its knock, got %+v", beta)
	}
}

func findLinkByURL(t *testing.T, links []peer.LinkStatus, url string) peer.LinkStatus {
	t.Helper()
	for _, l := range links {
		if l.URL == url {
			return l
		}
	}
	t.Fatalf("no link for %q in %+v", url, links)
	return peer.LinkStatus{}
}

func TestListPeersReportsLinkTable(t *testing.T) {
	h := newHarness(t)
	ca := genTestCA(t)

	status := func() []peer.LinkStatus {
		return []peer.LinkStatus{{Name: "beta", URL: "https://127.0.0.1:18444", State: peer.StateUp, RTTMillis: 2}}
	}
	srv := New(sharedStore, coord.New(sharedStore), Options{
		PeerName:     "alpha",
		PeerStatus:   status,
		SelfEndpoint: "https://127.0.0.1:1",
	})
	url, pool := newPeerTLSServer(t, srv.Handler(), ca, "alpha")

	// A member with a bearer token reads the mesh link table over ordinary TLS.
	req, _ := http.NewRequestWithContext(context.Background(), "GET", url+"/v1/peers", nil)
	req.Header.Set("Authorization", "Bearer "+h.aliceTok)
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, MinVersion: tls.VersionTLS12}}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var got struct {
		Peers []peer.LinkStatus `json:"peers"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Peers) != 1 || got.Peers[0].Name != "beta" || got.Peers[0].State != peer.StateUp {
		t.Fatalf("expected the link table, got %+v", got.Peers)
	}
}

func TestListPeersWithoutMesh(t *testing.T) {
	// A daemon that never joined a mesh still answers /v1/peers — with an empty table.
	h := newHarness(t)
	status, body := h.do(h.aliceTok, "GET", "/v1/peers", nil)
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var got struct {
		Peers []peer.LinkStatus `json:"peers"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Peers) != 0 {
		t.Fatalf("expected no peers, got %+v", got.Peers)
	}
}
