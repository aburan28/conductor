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
