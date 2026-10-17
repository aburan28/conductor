package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"reflect"
	"testing"
	"time"
)

// testServer is the directory under test: MagicDNS target, this node's IP,
// and the built-in services.
func testServer() *dnsServer {
	return newDNSServer("host-a.tail1234.ts.net.", net.ParseIP("100.64.0.1"), builtinServices)
}

func TestDNSQuestionTruncationAndCountBoundaries(t *testing.T) {
	server := testServer()
	query := buildQuery(0x1234, server.hostname, 1)
	for size := 0; size < len(query); size++ {
		response := server.answer(query[:size])
		if size < 12 {
			if response != nil {
				t.Fatalf("%d-byte prefix answered: %x", size, response)
			}
			continue
		}
		if rcode, count := parseHeader(t, response); rcode != 3 || count != 0 {
			t.Fatalf("%d-byte partial question = %d/%d; want no answer", size, rcode, count)
		}
		if !bytes.Equal(response[:2], query[:2]) {
			t.Fatalf("%d-byte prefix lost transaction ID", size)
		}
	}
	for _, count := range []uint16{0, 2, 65535} {
		packet := append([]byte(nil), query...)
		binary.BigEndian.PutUint16(packet[4:6], count)
		if rcode, answers := parseHeader(t, server.answer(packet)); rcode != 3 || answers != 0 {
			t.Fatalf("unsupported %d questions = %d/%d", count, rcode, answers)
		}
	}
	for _, tail := range [][]byte{{0xc0}, {0xc0, 0x0c}, {3, 'x'}, {0x40, 0, 0, 1, 0, 1}} {
		packet := append(append([]byte(nil), query[:12]...), tail...)
		if _, count := parseHeader(t, server.answer(packet)); count != 0 {
			t.Fatalf("malformed name %x answered", tail)
		}
	}
}

func TestDNSMappedIPv4AndUnknownAddressNames(t *testing.T) {
	server := newDNSServer("leader.internal.", net.ParseIP("::ffff:10.88.0.1"), builtinServices)
	for _, qtype := range []uint16{1, 28} {
		response := server.answer(buildQuery(0x1234, "LEADER.INTERNAL.", qtype))
		rcode, count := parseHeader(t, response)
		wantCount := 0
		if qtype == 1 {
			wantCount = 1
		}
		if rcode != 0 || count != wantCount {
			t.Fatalf("mapped IPv4 type %d = %d/%d", qtype, rcode, count)
		}
		if qtype == 1 && !net.IP(response[len(response)-4:]).Equal(net.ParseIP("10.88.0.1")) {
			t.Fatalf("mapped A data = %x", response)
		}
		if rcode, count := parseHeader(t, server.answer(buildQuery(1, "unknown.internal.", qtype))); rcode != 3 || count != 0 {
			t.Fatalf("unknown address name type %d = %d/%d", qtype, rcode, count)
		}
	}
}

func TestServiceConfigurationPortTypesAndStableListing(t *testing.T) {
	for _, value := range []string{`{"_custom._tcp.mesh.internal.":[-1]}`, `{"_custom._tcp.mesh.internal.":[65536]}`, `{"_custom._tcp.mesh.internal.":[1.5]}`, `{"_custom._tcp.mesh.internal.":["443"]}`, `[]`, `{} {}`} {
		if _, err := servicesFromEnv(value); err == nil {
			t.Errorf("invalid port configuration accepted: %s", value)
		}
	}
	services, err := servicesFromEnv(`{"_Z._tcp.mesh.internal.":[65535],"_a._tcp.mesh.internal.":[1]}`)
	if err != nil {
		t.Fatal(err)
	}
	server := newDNSServer("leader.internal.", nil, services)
	names := server.serviceNames()
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("unstable service order: %v", names)
		}
	}
	if !reflect.DeepEqual(names, server.serviceNames()) {
		t.Fatal("service listing changed between calls")
	}
	for name, port := range map[string]uint16{"_z._tcp.mesh.internal.": 65535, "_a._tcp.mesh.internal.": 1} {
		records := parseSRVAnswers(t, server.answer(buildQuery(1, name, 33)))
		if len(records) != 1 || records[0].port != port {
			t.Fatalf("service %s boundary port = %+v", name, records)
		}
	}
}

// buildQuery encodes a one-question DNS query for name/qtype.
func buildQuery(id uint16, name string, qtype uint16) []byte {
	var q []byte
	q = append(q, byte(id>>8), byte(id))
	q = append(q, 0x01, 0x00) // recursion desired
	q = append(q, 0, 1, 0, 0, 0, 0, 0, 0)
	q = append(q, encodeName(name)...)
	q = append(q, byte(qtype>>8), byte(qtype), 0, 1) // IN
	return q
}

// parsedSRV is one answer record, decoded.
type parsedSRV struct {
	priority uint16
	weight   uint16
	port     uint16
	target   string
}

// parseHeader pulls rcode and ancount off a response.
func parseHeader(t *testing.T, resp []byte) (rcode, ancount int) {
	t.Helper()
	if len(resp) < 12 {
		t.Fatalf("response too short: %d bytes", len(resp))
	}
	return int(resp[3] & 0x0F), int(binary.BigEndian.Uint16(resp[6:8]))
}

// parseSRVAnswers walks the answer section of an SRV response and returns one
// parsedSRV per record. It follows the name pointer at the start of each
// record (always 0xC0 0x0C in this server's answers) but not nested pointers.
func parseSRVAnswers(t *testing.T, resp []byte) []parsedSRV {
	t.Helper()
	_, ancount := parseHeader(t, resp)
	// Skip the question section: 12 header bytes + encoded name + type/class.
	off := 12
	for resp[off] != 0 {
		off += 1 + int(resp[off])
	}
	off += 5 // root label + qtype + qclass
	var out []parsedSRV
	for i := 0; i < ancount; i++ {
		if off+12 > len(resp) {
			t.Fatalf("answer %d overruns response at offset %d", i, off)
		}
		if resp[off] != 0xC0 || resp[off+1] != 0x0C {
			t.Fatalf("answer %d does not start with a question pointer", i)
		}
		off += 2
		if at := binary.BigEndian.Uint16(resp[off : off+2]); at != 33 {
			t.Fatalf("answer %d type = %d, want SRV(33)", i, at)
		}
		off += 8 // type + class + ttl
		rdlen := int(binary.BigEndian.Uint16(resp[off : off+2]))
		off += 2
		if off+rdlen > len(resp) {
			t.Fatalf("answer %d rdata overruns response", i)
		}
		rdata := resp[off : off+rdlen]
		off += rdlen
		rec := parsedSRV{
			priority: binary.BigEndian.Uint16(rdata[0:2]),
			weight:   binary.BigEndian.Uint16(rdata[2:4]),
			port:     binary.BigEndian.Uint16(rdata[4:6]),
		}
		name, _ := qnameString(rdata, 6)
		rec.target = name
		out = append(out, rec)
	}
	return out
}

func TestSRVForEachService(t *testing.T) {
	s := testServer()
	for name, wantPorts := range s.services {
		resp := s.answer(buildQuery(0x4242, name, 33))
		rcode, ancount := parseHeader(t, resp)
		if rcode != 0 {
			t.Fatalf("%s: rcode = %d, want 0", name, rcode)
		}
		if ancount != len(wantPorts) {
			t.Fatalf("%s: ancount = %d, want %d", name, ancount, len(wantPorts))
		}
		records := parseSRVAnswers(t, resp)
		var gotPorts []uint16
		wantTarget := s.hostname
		if name == "_conductor-mesh._tcp.mesh.internal." {
			wantTarget = "localhost."
		}
		for _, rec := range records {
			if rec.target != wantTarget {
				t.Fatalf("%s: SRV target = %q, want %q", name, rec.target, wantTarget)
			}
			gotPorts = append(gotPorts, rec.port)
		}
		if len(gotPorts) != len(wantPorts) {
			t.Fatalf("%s: got %d records, want %d", name, len(gotPorts), len(wantPorts))
		}
		for i, port := range wantPorts {
			if gotPorts[i] != port {
				t.Fatalf("%s: record %d port = %d, want %d", name, i, gotPorts[i], port)
			}
		}
	}
}

// TestConductorMeshCompat pins the record shape internal/peer's discovery
// consumes: _conductor-mesh answers with two SRV records targeting localhost,
// the loopback daemon the local resolver (127.0.0.1:15353) fronts.
func TestConductorMeshCompat(t *testing.T) {
	s := testServer()
	resp := s.answer(buildQuery(1, "_conductor-mesh._tcp.mesh.internal.", 33))
	records := parseSRVAnswers(t, resp)
	if len(records) != 2 {
		t.Fatalf("got %d SRV records, want 2 (ports 8443 and 8444)", len(records))
	}
	if records[0].port != 8443 || records[1].port != 8444 {
		t.Fatalf("ports = %d/%d, want 8443/8444", records[0].port, records[1].port)
	}
	if records[0].target != "localhost." {
		t.Fatalf("target = %q", records[0].target)
	}
}

func TestARecordForHostname(t *testing.T) {
	s := testServer()
	resp := s.answer(buildQuery(7, s.hostname, 1))
	rcode, ancount := parseHeader(t, resp)
	if rcode != 0 || ancount != 1 {
		t.Fatalf("rcode = %d, ancount = %d; want 0/1", rcode, ancount)
	}
	off := len(resp) - 6 // rdata length (2) + IPv4 (4)
	if binary.BigEndian.Uint16(resp[off:off+2]) != 4 {
		t.Fatalf("A rdata length = %d, want 4", binary.BigEndian.Uint16(resp[off:off+2]))
	}
	if got := net.IP(resp[off+2 : off+6]).String(); got != "100.64.0.1" {
		t.Fatalf("A rdata = %s, want 100.64.0.1", got)
	}
}

func TestNXDOMAIN(t *testing.T) {
	s := testServer()
	for _, name := range []string{
		"_unknown._tcp.mesh.internal.", // not in the registry
		"otherhost.ts.net.",            // the hostname but an SRV query
	} {
		resp := s.answer(buildQuery(9, name, 33))
		rcode, ancount := parseHeader(t, resp)
		if rcode != 3 || ancount != 0 {
			t.Fatalf("%s: rcode = %d, ancount = %d; want NXDOMAIN/0", name, rcode, ancount)
		}
	}
	// The hostname with a non-A qtype is also unknown.
	resp := s.answer(buildQuery(9, s.hostname, 16)) // TXT
	if rcode, _ := parseHeader(t, resp); rcode != 3 {
		t.Fatalf("TXT for hostname: rcode = %d, want 3", rcode)
	}
}

func TestMalformedPackets(t *testing.T) {
	s := testServer()
	for _, buf := range [][]byte{
		nil,
		{},
		{0x00},
		{0x12, 0x34}, // header only, no question
		append(buildQuery(1, "_conductor-mesh._tcp.mesh.internal.", 33)[:11], 0xFF), // truncated mid-question
	} {
		resp := s.answer(buf)
		if len(buf) < 12 {
			if resp != nil {
				t.Fatalf("short packet %v got response %v", buf, resp)
			}
			continue
		}
		// A header-only query has qdcount=0: it gets NXDOMAIN, not a crash.
		if rcode, _ := parseHeader(t, resp); rcode != 3 {
			t.Fatalf("packet %v: rcode = %d, want 3", buf, rcode)
		}
	}
}

// Only standard queries are answered. A packet with QR set is itself a response (answering
// it lets a spoofed source loop two servers), and NOTIFY / UPDATE are not questions.
func TestIgnoresResponsesAndOtherOpcodes(t *testing.T) {
	s := testServer()
	for name, flags := range map[string]byte{
		"response": 0x81,            // QR + RD
		"notify":   (4 << 3) | 0x01, // opcode 4
		"update":   5 << 3,          // opcode 5
		"iquery":   1 << 3,          // opcode 1
	} {
		q := buildQuery(1, s.hostname, 1)
		q[2] = flags
		if resp := s.answer(q); resp != nil {
			t.Errorf("%s: answered with %v, want no reply", name, resp)
		}
	}
	if s.answer(buildQuery(1, s.hostname, 1)) == nil {
		t.Error("a standard query went unanswered")
	}
}

func TestTruncatedQuestionStillNXDOMAINs(t *testing.T) {
	s := testServer()
	// Claim one question but cut the packet before the name ends.
	q := buildQuery(3, "_cairn-p2p._tcp.mesh.internal.", 33)
	resp := s.answer(q[:14])
	if len(resp) < 12 {
		t.Fatal("no response")
	}
	if rcode, _ := parseHeader(t, resp); rcode != 3 {
		t.Fatalf("rcode = %d, want NXDOMAIN for an unreadable question", rcode)
	}
}

func TestServicesFromEnv(t *testing.T) {
	base, err := servicesFromEnv("")
	if err != nil || len(base) != 4 {
		t.Fatalf("empty env: got %v (%v), want the 4 built-ins", base, err)
	}
	custom, err := servicesFromEnv(`{"_custom._tcp.mesh.internal.":[5555],"_cairn-p2p._tcp.mesh.internal.":[9001]}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := custom["_custom._tcp.mesh.internal."]; !ok {
		t.Fatal("custom service not added")
	}
	if got := custom["_cairn-p2p._tcp.mesh.internal."]; len(got) != 1 || got[0] != 9001 {
		t.Fatalf("built-in not replaced: %v", got)
	}
	if _, err := servicesFromEnv("{not json"); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestQnameString(t *testing.T) {
	// Lowercased, dotted, trailing dot.
	name, off := qnameString(append([]byte{2, 'A', 'B'}, 0), 0)
	if name != "ab." || off != 4 {
		t.Fatalf("qnameString = %q, %d", name, off)
	}
	// A label that overruns the buffer is an error.
	if name, _ := qnameString([]byte{5, 'a'}, 0); name != "" {
		t.Fatalf("overrun accepted: %q", name)
	}
}

// TestOverTheWireWithNetResolver runs a real dnsServer over UDP and resolves
// _conductor-mesh through Go's net.Resolver — the same code path
// internal/peer.ResolveSRV uses for --peer-discover-dns.
func TestOverTheWireWithNetResolver(t *testing.T) {
	s := testServer()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := conn.WriteTo(s.answer(buf[:n]), peer); err != nil {
				return
			}
		}
	}()
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			return d.DialContext(ctx, "udp", conn.LocalAddr().String())
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, records, err := r.LookupSRV(ctx, "", "", "_conductor-mesh._tcp.mesh.internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("net.Resolver saw %d records, want 2", len(records))
	}
	if records[0].Target != "localhost." {
		t.Fatalf("net.Resolver target = %q", records[0].Target)
	}
	if records[0].Port != 8443 || records[1].Port != 8444 {
		t.Fatalf("net.Resolver ports = %d/%d, want 8443/8444", records[0].Port, records[1].Port)
	}
}

// Real resolver queries verify the complete SRV + address discovery chain on a
// private network, with no Tailscale process or upstream DNS server.
func TestExplicitPrivateNetworkDiscovery(t *testing.T) {
	for _, address := range []string{"10.88.0.1", "fd88::1"} {
		t.Run(address, func(t *testing.T) {
			s := newDNSServer("leader.internal.", net.ParseIP(address), map[string][]uint16{
				"_conductor-mesh._tcp.mesh.internal.": {8443},
			})
			s.meshTarget = s.hostname
			conn, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			go func() {
				buf := make([]byte, 512)
				for {
					n, remote, err := conn.ReadFrom(buf)
					if err != nil {
						return
					}
					if _, err := conn.WriteTo(s.answer(buf[:n]), remote); err != nil {
						return
					}
				}
			}()
			resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "udp", conn.LocalAddr().String())
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, records, err := resolver.LookupSRV(ctx, "", "", "_conductor-mesh._tcp.mesh.internal")
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].Target != s.hostname || records[0].Port != 8443 {
				t.Fatalf("unexpected mesh SRV: %+v", records)
			}
			addresses, err := resolver.LookupIPAddr(ctx, records[0].Target)
			if err != nil {
				t.Fatal(err)
			}
			if len(addresses) != 1 || !addresses[0].IP.Equal(net.ParseIP(address)) {
				t.Fatalf("addresses = %v, want %s", addresses, address)
			}
		})
	}
}

func TestAddressFamilyAnswers(t *testing.T) {
	for _, tc := range []struct {
		address string
		qtype   uint16
		length  int
	}{
		{"10.88.0.1", 1, 4}, {"10.88.0.1", 28, 0},
		{"fd88::1", 1, 0}, {"fd88::1", 28, 16},
		{"", 1, 0}, {"", 28, 0},
	} {
		s := newDNSServer("leader.internal.", net.ParseIP(tc.address), builtinServices)
		response := s.answer(buildQuery(1, s.hostname, tc.qtype))
		rcode, count := parseHeader(t, response)
		wantCount := 0
		if tc.length > 0 {
			wantCount = 1
		}
		if rcode != 0 || count != wantCount {
			t.Fatalf("%s type %d: rcode/count = %d/%d", tc.address, tc.qtype, rcode, count)
		}
		if tc.length > 0 {
			offset := len(response) - tc.length
			if int(binary.BigEndian.Uint16(response[offset-2:offset])) != tc.length || !net.IP(response[offset:]).Equal(net.ParseIP(tc.address)) {
				t.Fatalf("malformed address response: %v", response)
			}
		}
	}
}
