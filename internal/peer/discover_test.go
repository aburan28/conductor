package peer_test

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/adamburan/conductor/internal/peer"
)

// fakeSRVResolver answers LookupSRV from a fixed table instead of touching real DNS.
type fakeSRVResolver struct {
	records map[string][]*net.SRV
	err     error
}

func (f *fakeSRVResolver) LookupSRV(_ context.Context, service, proto, name string) (string, []*net.SRV, error) {
	if f.err != nil {
		return "", nil, f.err
	}
	return "", f.records[name], nil
}

func TestNewAllowsDiscoveryWithNoStaticPeers(t *testing.T) {
	ca := genCA(t)
	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	m, err := peer.New(peer.Options{
		DiscoverDNS: "_conductor-mesh._tcp.mesh.internal",
		Resolver:    &fakeSRVResolver{},
		CAPath:      caPath, CertPath: certPath, KeyPath: keyPath,
	})
	if err != nil {
		t.Fatalf("expected DiscoverDNS alone to be enough to start a Manager, got %v", err)
	}
	if len(m.Snapshot()) != 0 {
		t.Fatalf("expected no links before the first Discover, got %d", len(m.Snapshot()))
	}
}

func TestNewStillRejectsNoPeersAndNoDiscovery(t *testing.T) {
	ca := genCA(t)
	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	_, err := peer.New(peer.Options{CAPath: caPath, CertPath: certPath, KeyPath: keyPath})
	if err == nil {
		t.Fatal("expected an error: no --peer and no --peer-discover-dns leaves nothing to join")
	}
}

func TestDiscoverAddsPeersFromSRV(t *testing.T) {
	ca := genCA(t)
	betaURL := newMeshServer(t, ca, "beta")
	betaHost := betaURL[len("https://"):] // host:port, as newMeshServer returns it

	host, portStr, err := net.SplitHostPort(betaHost)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}

	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	resolver := &fakeSRVResolver{records: map[string][]*net.SRV{
		"_conductor-mesh._tcp.mesh.internal": {{Target: host + ".", Port: uint16(port)}},
	}}
	m, err := peer.New(peer.Options{
		DiscoverDNS: "_conductor-mesh._tcp.mesh.internal",
		Resolver:    resolver,
		CAPath:      caPath, CertPath: certPath, KeyPath: keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	m.Discover(context.Background())
	links := m.Snapshot()
	if len(links) != 1 {
		t.Fatalf("expected 1 discovered link, got %d", len(links))
	}
	if !links[0].Discovered {
		t.Fatal("expected the link to be flagged as discovered")
	}
	if links[0].State != peer.StateDown {
		t.Fatalf("expected a freshly discovered peer to start down (not yet probed), got %q", links[0].State)
	}

	// A probe should bring it up and adopt the peer's own reported identity — "beta" —
	// over the provisional DNS-target name, without flagging it as a mismatch the way an
	// operator-configured peer under the wrong name would be.
	m.Probe(context.Background())
	links = m.Snapshot()
	if links[0].State != peer.StateUp {
		t.Fatalf("expected the discovered peer to come up, got %q (%s)", links[0].State, links[0].LastError)
	}
	if links[0].Name != "beta" {
		t.Fatalf("expected the discovered peer to adopt its reported name, got %q", links[0].Name)
	}
	if links[0].LastError != "" {
		t.Fatalf("expected no mismatch error for a discovered peer, got %q", links[0].LastError)
	}

	// A second discovery round must not duplicate the link.
	m.Discover(context.Background())
	if len(m.Snapshot()) != 1 {
		t.Fatalf("expected discovery to be idempotent, got %d links", len(m.Snapshot()))
	}
}

func TestDiscoverSkipsSelf(t *testing.T) {
	ca := genCA(t)
	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	resolver := &fakeSRVResolver{records: map[string][]*net.SRV{
		"_conductor-mesh._tcp.mesh.internal": {{Target: "127.0.0.1.", Port: 8443}},
	}}
	m, err := peer.New(peer.Options{
		DiscoverDNS: "_conductor-mesh._tcp.mesh.internal",
		Resolver:    resolver,
		SelfURL:     "https://127.0.0.1:8443",
		CAPath:      caPath, CertPath: certPath, KeyPath: keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Discover(context.Background())
	if len(m.Snapshot()) != 0 {
		t.Fatalf("expected a discovered address matching SelfURL to be skipped, got %d links", len(m.Snapshot()))
	}
}

func TestDiscoverDoesNotDuplicateStaticPeer(t *testing.T) {
	ca := genCA(t)
	betaURL := newMeshServer(t, ca, "beta")
	betaHost := betaURL[len("https://"):]
	host, portStr, err := net.SplitHostPort(betaHost)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}

	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	resolver := &fakeSRVResolver{records: map[string][]*net.SRV{
		"_conductor-mesh._tcp.mesh.internal": {{Target: host + ".", Port: uint16(port)}},
	}}
	m, err := peer.New(peer.Options{
		Peers:       []peer.Peer{{Name: "beta", URL: betaURL}},
		DiscoverDNS: "_conductor-mesh._tcp.mesh.internal",
		Resolver:    resolver,
		CAPath:      caPath, CertPath: certPath, KeyPath: keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Discover(context.Background())
	links := m.Snapshot()
	if len(links) != 1 {
		t.Fatalf("expected the discovered address to merge into the existing static peer, got %d links", len(links))
	}
	if links[0].Discovered {
		t.Fatal("expected the statically configured peer to keep its non-discovered identity")
	}
}

func TestDiscoverFailureLeavesExistingLinksAlone(t *testing.T) {
	ca := genCA(t)
	betaURL := newMeshServer(t, ca, "beta")

	caPath, certPath, keyPath := writeMeshFiles(t, ca, "alpha")
	m, err := peer.New(peer.Options{
		Peers:       []peer.Peer{{Name: "beta", URL: betaURL}},
		DiscoverDNS: "_conductor-mesh._tcp.mesh.internal",
		Resolver:    &fakeSRVResolver{err: fmt.Errorf("no such host")},
		CAPath:      caPath, CertPath: certPath, KeyPath: keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Probe(context.Background())
	before := m.Snapshot()

	m.Discover(context.Background()) // resolver errors; must not touch existing links
	after := m.Snapshot()
	if len(after) != len(before) || after[0].State != before[0].State {
		t.Fatalf("expected a failed discovery round to leave existing links untouched, got %+v", after)
	}
}
