package main

import (
	"fmt"
	"net"
	"strings"
)

type directoryIdentity struct {
	hostname       string
	ip             net.IP
	meshTarget     string
	detectionError error
}

// Explicit identity never consults Tailscale, DNS, or an external service.
// The default retains the existing Tailscale/localhost startup behavior.
func resolveDirectoryIdentity(hostname, address, meshTarget string, detect func() (string, net.IP, error)) (directoryIdentity, error) {
	var out directoryIdentity
	var err error
	if out.meshTarget, err = directoryHostname(meshTarget); err != nil {
		return out, fmt.Errorf("--mesh-target: %w", err)
	}
	if address != "" {
		if hostname == "" {
			return out, fmt.Errorf("--address requires --hostname")
		}
		out.ip = net.ParseIP(address)
		if out.ip == nil || !(out.ip.IsGlobalUnicast() || out.ip.IsLoopback() || out.ip.IsLinkLocalUnicast()) {
			return out, fmt.Errorf("--address must be a literal unicast IPv4 or IPv6 address")
		}
	}
	if hostname == "" {
		hostname, out.ip, out.detectionError = detect()
		if out.detectionError != nil {
			hostname, out.ip = "localhost", nil
		}
	}
	if out.hostname, err = directoryHostname(hostname); err != nil {
		return out, fmt.Errorf("--hostname: %w", err)
	}
	return out, nil
}

func directoryHostname(value string) (string, error) {
	name := strings.TrimSuffix(strings.ToLower(value), ".")
	if name == "" || len(name) > 253 || net.ParseIP(name) != nil {
		return "", fmt.Errorf("expected a DNS hostname")
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid DNS hostname %q", value)
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '-' {
				return "", fmt.Errorf("invalid DNS hostname %q", value)
			}
		}
	}
	return name + ".", nil
}
