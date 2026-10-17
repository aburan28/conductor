// Command conductor-mesh-dns serves the mesh's service directory over UDP DNS.
//
// It grew out of a tiny out-of-repo server that answered one static SRV record
// (`_conductor-mesh._tcp.mesh.internal.` -> localhost:8443/8444) on
// 127.0.0.1:15353 for conductor's --peer-discover-dns. The tailnet now
// publishes several services (conductor over `tailscale serve`, cairn's HTTP
// API, cairn p2p), so the one static record became a small registry — but the
// server is still static, hand-rolled, and tiny on purpose: every name in the
// registry resolves, everything else gets NXDOMAIN.
//
// The SRV target is explicit (--hostname and --address for private networks),
// or auto-detected from `tailscale status --json`, falling back to localhost.
// A/AAAA records can resolve the target without an external DNS dependency.
//
//	Listen on loopback only:      conductor-mesh-dns            (default)
//	Expose to the whole tailnet:  conductor-mesh-dns --listen 0.0.0.0:15353
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
)

// builtinServices is the directory when MESH_DNS_SERVICES is unset: every
// service this machine publishes on the tailnet today.
var builtinServices = map[string][]uint16{
	// conductor daemon mesh links (back-compat with --peer-discover-dns):
	// 8443 the daemon's own HTTPS listener, 8444 the tailscale TCP forward.
	"_conductor-mesh._tcp.mesh.internal.": {8443, 8444},
	// conductor's public surface behind `tailscale serve` (HTTPS on 443).
	"_conductor._tcp.mesh.internal.": {443},
	// cairn's HTTP API/UI, served/funneled on the tailnet HTTPS port.
	"_cairn-http._tcp.mesh.internal.": {8443},
	// cairn p2p, bound directly on the tailnet interface.
	"_cairn-p2p._tcp.mesh.internal.": {9000},
}

// servicesFromEnv layers MESH_DNS_SERVICES (JSON: name -> [ports]) over the
// built-ins: the env map replaces the built-in ports for a known name and
// adds new names verbatim. An empty or invalid value falls back to built-ins.
func servicesFromEnv(value string) (map[string][]uint16, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return builtinServices, nil
	}
	var custom map[string][]uint16
	if err := json.Unmarshal([]byte(value), &custom); err != nil {
		return nil, fmt.Errorf("parse MESH_DNS_SERVICES: %w", err)
	}
	services := make(map[string][]uint16, len(builtinServices)+len(custom))
	for name, ports := range builtinServices {
		services[name] = ports
	}
	for name, ports := range custom {
		services[name] = ports
	}
	return services, nil
}

func main() {
	fs := flag.NewFlagSet("conductor-mesh-dns", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:15353", "UDP listen address; bind a private interface to expose the directory to your network")
	hostname := fs.String("hostname", "", "SRV target DNS name; bypasses Tailscale detection when set")
	address := fs.String("address", "", "literal IPv4 or IPv6 address for the SRV target (requires --hostname)")
	meshTarget := fs.String("mesh-target", "localhost", "DNS target for _conductor-mesh; set to --hostname for remote private-network peers")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `conductor-mesh-dns — service discovery over DNS for a private Conductor network

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}

	services, err := servicesFromEnv(os.Getenv("MESH_DNS_SERVICES"))
	if err != nil {
		log.Fatal(err)
	}

	identity, err := resolveDirectoryIdentity(*hostname, *address, *meshTarget, detectSelf)
	if err != nil {
		log.Fatal(err)
	}
	if identity.detectionError != nil {
		log.Printf("tailscale auto-detect failed (%v); falling back to localhost", identity.detectionError)
	}
	server := newDNSServer(identity.hostname, identity.ip, services)
	server.meshTarget = identity.meshTarget
	addr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	for _, name := range server.serviceNames() {
		target := server.hostname
		if name == "_conductor-mesh._tcp.mesh.internal." {
			target = server.meshTarget
		}
		log.Printf("serving %s -> %s:%v", name, target, server.services[name])
	}
	if os.Getenv("MESH_DNS_ONESHOT") != "" {
		buf := make([]byte, 512)
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := conn.WriteToUDP(server.answer(buf[:n]), peer); err != nil {
			log.Fatal(err)
		}
		return
	}
	buf := make([]byte, 512)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Print(err)
			continue
		}
		if _, err := conn.WriteToUDP(server.answer(buf[:n]), peer); err != nil {
			log.Print(err)
		}
	}
}
