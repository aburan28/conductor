// conductor-mesh-dns: tailscale CLI auto-detection of this node's SRV target.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"time"
)

// tailscaleCLI locates the Tailscale binary: PATH first, then the standard
// macOS app location. Empty string means "not found".
func tailscaleCLI() string {
	if path, err := exec.LookPath("tailscale"); err == nil {
		return path
	}
	if path, err := exec.LookPath("/Applications/Tailscale.app/Contents/MacOS/Tailscale"); err == nil {
		return path
	}
	return ""
}

// detectSelf runs `tailscale status --json` once and returns this node's
// MagicDNS FQDN (trailing dot kept, lowercased) and its tailscale IP.
// Errors are returned to the caller, which decides whether to fall back.
func detectSelf() (string, net.IP, error) {
	cli := tailscaleCLI()
	if cli == "" {
		return "", nil, fmt.Errorf("tailscale CLI not found in PATH or /Applications")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, cli, "status", "--json").Output()
	if err != nil {
		return "", nil, fmt.Errorf("tailscale status --json: %w", err)
	}
	var state struct {
		Self *struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(output, &state); err != nil {
		return "", nil, fmt.Errorf("parse tailscale status: %w", err)
	}
	if state.Self == nil || state.Self.DNSName == "" {
		return "", nil, fmt.Errorf("tailscale status has no Self.DNSName (logged in? MagicDNS enabled?)")
	}
	name := state.Self.DNSName
	if name[len(name)-1] != '.' {
		name += "."
	}
	var ip net.IP
	for _, s := range state.Self.TailscaleIPs {
		if v := net.ParseIP(s); v != nil {
			ip = v
			break
		}
	}
	return name, ip, nil
}
