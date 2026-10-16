// conductor-mesh-dns: the DNS answer table and hand-rolled wire format.
//
// Kept in the style of the original out-of-repo mesh-dns: no DNS library,
// qnameString/question/answers as plain byte slices, NXDOMAIN for anything
// the table does not know. The only generalization over the original is that
// the table (service name -> SRV ports) and the SRV target are data, not
// constants, and A queries for the target itself are answered with the node's
// tailscale IP so SRV target resolution completes in one server.
package main

import (
	"encoding/binary"
	"net"
	"sort"
	"strings"
)

// dnsServer answers queries from one immutable directory.
type dnsServer struct {
	// hostname is the SRV target and the only name with an A record,
	// wire-encodable FQDN form, lowercased, trailing dot ("host.tail.ts.net.").
	hostname string
	// ip is the node's tailscale IP; nil disables A answers.
	ip net.IP
	// services maps SRV name -> ports, one SRV record per port.
	services map[string][]uint16
}

// newDNSServer validates and lowercases its inputs.
func newDNSServer(hostname string, ip net.IP, services map[string][]uint16) *dnsServer {
	norm := make(map[string][]uint16, len(services))
	for name, ports := range services {
		norm[strings.ToLower(name)] = ports
	}
	return &dnsServer{
		hostname: strings.ToLower(hostname),
		ip:       ip,
		services: norm,
	}
}

// qnameString renders the question name in wire order, lowercased, dotted.
func qnameString(buf []byte, off int) (string, int) {
	var parts []string
	for {
		if off >= len(buf) {
			return "", -1
		}
		l := int(buf[off])
		off++
		if l == 0 {
			break
		}
		if l&0xC0 != 0 { // compression pointer in a query: just skip it
			off++
			break
		}
		if off+l > len(buf) {
			return "", -1
		}
		parts = append(parts, strings.ToLower(string(buf[off:off+l])))
		off += l
	}
	return strings.Join(parts, ".") + ".", off
}

// encodeName renders a dotted, lowercased FQDN in wire format.
func encodeName(name string) []byte {
	out := make([]byte, 0, len(name)+1)
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return append(out, 0)
}

// serviceNames lists the SRV names in a stable order, for logging and tests.
func (s *dnsServer) serviceNames() []string {
	names := make([]string, 0, len(s.services))
	for name := range s.services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// answer builds the response to one query, or nil if the packet is unusable.
func (s *dnsServer) answer(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	id := query[0:2]
	qdcount := int(binary.BigEndian.Uint16(query[4:6]))
	off := 12
	name := ""
	qtype := 0
	if qdcount >= 1 {
		var end int
		name, end = qnameString(query, off)
		if end > 0 && end+4 <= len(query) {
			qtype = int(binary.BigEndian.Uint16(query[end : end+2]))
			off = end + 4
		}
	}
	question := query[12:off]

	// meshTarget keeps the loopback-peering record on localhost even when the
	// tailnet target is a MagicDNS name: a remote peer dialing the mesh ports
	// through serve would hit whatever serve publishes there (8443 is cairn's
	// tailnet HTTPS in the standard layout), not the loopback daemon.
	meshName := "_conductor-mesh._tcp.mesh.internal."
	resp := make([]byte, 0, 512)
	resp = append(resp, id[0], id[1])
	rcode := byte(0)
	ancount := 0
	var answers []byte
	if qdcount == 1 {
		switch {
		case qtype == 33: // SRV
			if ports, ok := s.services[name]; ok {
				target := encodeName(s.hostname)
				if name == meshName {
					target = encodeName("localhost.")
				}
				for _, port := range ports {
					rdata := make([]byte, 0, 6+len(target))
					var tmp [2]byte
					binary.BigEndian.PutUint16(tmp[:], 0) // priority
					rdata = append(rdata, tmp[:]...)
					rdata = append(rdata, tmp[:]...) // weight
					binary.BigEndian.PutUint16(tmp[:], port)
					rdata = append(rdata, tmp[:]...)
					rdata = append(rdata, target...)
					answers = append(answers, 0xC0, 0x0C) // name pointer -> question
					answers = append(answers, 0, 33)      // SRV
					answers = append(answers, 0, 1)       // IN
					answers = append(answers, 0, 0, 0, 60)
					binary.BigEndian.PutUint16(tmp[:], uint16(len(rdata)))
					answers = append(answers, tmp[:]...)
					answers = append(answers, rdata...)
					ancount++
				}
			} else {
				rcode = 3 // NXDOMAIN
			}
		case qtype == 1: // A
			if name == s.hostname && s.ip != nil {
				answers = append(answers, 0xC0, 0x0C) // name pointer -> question
				answers = append(answers, 0, 1)       // A
				answers = append(answers, 0, 1)       // IN
				answers = append(answers, 0, 0, 0, 60)
				answers = append(answers, 0, 4)
				answers = append(answers, s.ip.To4()...)
				ancount++
			} else {
				rcode = 3 // NXDOMAIN
			}
		default:
			rcode = 3 // NXDOMAIN
		}
	} else {
		rcode = 3 // NXDOMAIN
	}
	resp = append(resp, 0x81, 0x80|rcode)
	resp = append(resp, 0, byte(qdcount))
	resp = append(resp, 0, byte(ancount))
	resp = append(resp, 0, 0, 0, 0)
	resp = append(resp, question...)
	resp = append(resp, answers...)
	return resp
}
