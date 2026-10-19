// conductor-mesh-dns: the DNS answer table and hand-rolled wire format.
//
// Kept in the style of the original out-of-repo mesh-dns: no DNS library,
// qnameString/question/answers as plain byte slices, NXDOMAIN for anything
// the table does not know. The only generalization over the original is that
// the table (service name -> SRV ports) and the SRV target are data, not
// constants. A/AAAA queries for the target itself resolve its configured address.
package main

import (
	"encoding/binary"
	"net"
	"sort"
	"strings"
)

// dnsServer answers queries from one immutable directory.
type dnsServer struct {
	// hostname is the SRV target and the only name with address records,
	// wire-encodable FQDN form, lowercased, trailing dot ("host.tail.ts.net.").
	hostname string
	// ip is the advertised address; nil disables address answers.
	ip net.IP
	// meshTarget preserves local peering by default; explicit deployments can
	// instead advertise a daemon reachable over their private network.
	meshTarget string
	// services maps SRV name -> ports, one SRV record per port.
	services map[string][]uint16
}

// newDNSServer lowercases names; command-line validation happens before construction.
func newDNSServer(hostname string, ip net.IP, services map[string][]uint16) *dnsServer {
	norm := make(map[string][]uint16, len(services))
	for name, ports := range services {
		norm[strings.ToLower(name)] = ports
	}
	return &dnsServer{
		hostname:   strings.ToLower(hostname),
		meshTarget: "localhost.",
		ip:         ip,
		services:   norm,
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
	// Only a standard query (QR clear, opcode 0) gets an answer. Replying to a response would
	// let a spoofed packet bounce two DNS servers off each other indefinitely, and NOTIFY or
	// UPDATE are not something to answer as if they were questions.
	if query[2]&0x80 != 0 || (query[2]>>3)&0x0F != 0 {
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
					target = encodeName(s.meshTarget)
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
		case qtype == 1 || qtype == 28: // A or AAAA
			if name != s.hostname {
				rcode = 3 // NXDOMAIN
				break
			}
			var address net.IP
			if qtype == 1 {
				address = s.ip.To4()
			} else if s.ip.To4() == nil {
				address = s.ip.To16()
			}
			// A known target without this family returns NOERROR with no data.
			if address != nil {
				answers = append(answers, 0xC0, 0x0C)
				answers = append(answers, 0, byte(qtype), 0, 1)
				answers = append(answers, 0, 0, 0, 60)
				answers = append(answers, 0, byte(len(address)))
				answers = append(answers, address...)
				ancount++
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
