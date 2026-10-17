package main

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestExplicitIdentitySkipsDetection(t *testing.T) {
	for _, address := range []string{"10.88.0.1", "fd88::1"} {
		identity, err := resolveDirectoryIdentity("Leader.INTERNAL", address, "Leader.INTERNAL.", func() (string, net.IP, error) {
			t.Fatal("explicit configuration attempted Tailscale detection")
			return "", nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if identity.hostname != "leader.internal." || identity.meshTarget != identity.hostname || !identity.ip.Equal(net.ParseIP(address)) {
			t.Fatalf("unexpected identity: %+v", identity)
		}
	}
}

func TestDefaultIdentityPreservesDetectionAndFallback(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		identity, err := resolveDirectoryIdentity("", "", "localhost", func() (string, net.IP, error) {
			if unavailable {
				return "", nil, errors.New("CLI unavailable")
			}
			return "node.tail123.ts.net.", net.ParseIP("100.64.0.1"), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if identity.meshTarget != "localhost." {
			t.Fatalf("mesh target changed: %s", identity.meshTarget)
		}
		if unavailable {
			if identity.hostname != "localhost." || identity.ip != nil || identity.detectionError == nil {
				t.Fatalf("unexpected fallback: %+v", identity)
			}
		} else if identity.hostname != "node.tail123.ts.net." || identity.ip == nil {
			t.Fatalf("unexpected detected identity: %+v", identity)
		}
	}
}

func TestInvalidDirectoryIdentityFailsBeforeDetection(t *testing.T) {
	for _, tc := range []struct{ hostname, address, target string }{
		{"", "10.88.0.1", "localhost"},
		{"leader.internal", "leader.internal", "localhost"},
		{"leader.internal", "0.0.0.0", "localhost"},
		{"leader.internal", "::", "localhost"},
		{"leader.internal", "224.0.0.1", "localhost"},
		{"leader.internal", "255.255.255.255", "localhost"},
		{"leader.internal", "ff02::1", "localhost"},
		{"leader.internal", "fd88::1%wg0", "localhost"},
		{"leader.internal", "10.88.0.1", "bad..internal"},
		{"leader.internal", "10.88.0.1", "https://leader.internal"},
		{"", "", "bad..internal"},
		{"bad..internal", "10.88.0.1", "localhost"},
		{"-bad.internal", "", "localhost"},
		{"10.88.0.1", "", "localhost"},
		{strings.Repeat("a", 64) + ".internal", "", "localhost"},
	} {
		_, err := resolveDirectoryIdentity(tc.hostname, tc.address, tc.target, func() (string, net.IP, error) {
			t.Fatal("invalid configuration reached detection")
			return "", nil, nil
		})
		if err == nil {
			t.Errorf("accepted invalid identity: %+v", tc)
		}
	}
}

func TestDirectoryHostnameWireLimits(t *testing.T) {
	// Four labels occupy the maximum 255-byte wire name, including their
	// length bytes and the root terminator.
	maximum := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	for _, input := range []string{maximum, strings.ToUpper(maximum) + ".", "localhost", "worker-1.internal."} {
		name, err := directoryHostname(input)
		if err != nil {
			t.Fatalf("valid hostname %q: %v", input, err)
		}
		if len(encodeName(name)) > 255 || !strings.HasSuffix(name, ".") || name != strings.ToLower(name) {
			t.Fatalf("hostname not normalized or wire encodable: %q", name)
		}
	}
	for _, input := range []string{"", ".", maximum + "d", "worker_.internal", "worker-.internal", "worker.internal..", "wörker.internal", " worker.internal", "::1"} {
		if _, err := directoryHostname(input); err == nil {
			t.Errorf("invalid hostname %q accepted", input)
		}
	}
}

func TestExplicitHostnameWithoutAddressSkipsDetection(t *testing.T) {
	identity, err := resolveDirectoryIdentity("Leader.INTERNAL.", "", "peer.internal", func() (string, net.IP, error) {
		t.Fatal("explicit hostname attempted automatic detection")
		return "", nil, nil
	})
	if err != nil || identity.hostname != "leader.internal." || identity.meshTarget != "peer.internal." || identity.ip != nil || identity.detectionError != nil {
		t.Fatalf("hostname-only identity = %+v, %v", identity, err)
	}
	server := newDNSServer(identity.hostname, identity.ip, builtinServices)
	for _, qtype := range []uint16{1, 28} {
		if rcode, count := parseHeader(t, server.answer(buildQuery(1, identity.hostname, qtype))); rcode != 0 || count != 0 {
			t.Fatalf("hostname-only type %d = %d/%d, want NOERROR without address data", qtype, rcode, count)
		}
	}
}
