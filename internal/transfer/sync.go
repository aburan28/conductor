package transfer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// LAN sync is the one part of transfer that sends session data off the machine, so it is
// opt-in, explicitly acknowledged, and local-network only. It speaks TLS with an ephemeral
// self-signed certificate, and authenticates the peer with a one-time pairing code bound to
// both certificates' fingerprints: a device on the network without the code cannot
// interpose, even though the certificate itself is untrusted. Session bytes are then sealed
// with a key derived from that code.
//
// The exchange is symmetric: each side sends the sessions its filters select and imports
// the other's through the same append-only merge path as a file bundle. Nothing is
// overwritten: a diverged session is reported as a conflict.

// Direction limits a sync to one way. The zero value syncs both ways.
type Direction string

const (
	SyncBoth Direction = "both"
	SyncPull Direction = "pull"
	SyncPush Direction = "push"
)

// SyncOptions selects what a peer sends and how the receiving side applies it.
type SyncOptions struct {
	Tool        string
	Project     string
	Since       time.Time
	ClaudeHome  string
	CodexHome   string
	DryRun      bool
	Direction   Direction
	IncludeArch bool

	// Code lets a caller fix the pairing code (tests and scripted pairings); empty means a
	// fresh random code is generated and printed by ServeSync.
	Code string
	// Ready is called with the listening address once ServeSync is accepting, so a test or
	// a wrapper can connect without scraping stdout.
	Ready func(addr string)
}

// SyncSummary is what one side did.
type SyncSummary struct {
	Sent     int          `json:"sent"`
	Received int          `json:"received"`
	Import   ImportResult `json:"import"`
	Warnings []string     `json:"warnings,omitempty"`
}

const syncALPN = "conductor-session-transfer/1"

// ServeSync listens for one peer, prints the pairing code, and applies what arrives. It
// returns after the exchange (or when ctx is cancelled).
func ServeSync(ctx context.Context, addr string, opts SyncOptions, out io.Writer) (SyncSummary, error) {
	code := opts.Code
	if code == "" {
		var err error
		code, err = newPairingCode()
		if err != nil {
			return SyncSummary{}, err
		}
	}
	cert, err := ephemeralCert()
	if err != nil {
		return SyncSummary{}, err
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{syncALPN},
		MinVersion:   tls.VersionTLS13,
		// The client certificate is not trusted by a CA; it exists only so the pairing proof
		// can bind both ends of the connection.
		ClientAuth: tls.RequestClientCert,
	})
	if err != nil {
		return SyncSummary{}, err
	}
	defer ln.Close()

	fmt.Fprintf(out, "Listening on %s\n", ln.Addr())
	fmt.Fprintf(out, "On the other device run:\n  conductor sessions transfer connect %s --i-understand\n", advertiseAddr(ln.Addr()))
	fmt.Fprintf(out, "When it asks, enter this pairing code:  %s\n\n", formatCode(code))
	if opts.Ready != nil {
		opts.Ready(ln.Addr().String())
	}

	type accepted struct {
		c   net.Conn
		err error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		ch <- accepted{c, err}
	}()
	var conn net.Conn
	select {
	case <-ctx.Done():
		return SyncSummary{}, ctx.Err()
	case a := <-ch:
		if a.err != nil {
			return SyncSummary{}, a.err
		}
		conn = a.c
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Minute))

	key, err := serverHandshake(conn, code, cert.Certificate[0])
	if err != nil {
		return SyncSummary{}, err
	}
	return exchange(conn, key, opts, true)
}

// ConnectSync connects to a peer, authenticates with the pairing code, and exchanges
// sessions. The code is read from the terminal unless it is passed in.
func ConnectSync(ctx context.Context, address, code string, opts SyncOptions) (SyncSummary, error) {
	if err := ensurePrivateAddress(address); err != nil {
		return SyncSummary{}, err
	}
	if code == "" {
		var err error
		code, err = promptCode()
		if err != nil {
			return SyncSummary{}, err
		}
	}
	cert, err := ephemeralCert()
	if err != nil {
		return SyncSummary{}, err
	}
	d := &net.Dialer{}
	raw, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return SyncSummary{}, err
	}
	conn := tls.Client(raw, &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // identity comes from the pairing proof, not a CA
		NextProtos:         []string{syncALPN},
		MinVersion:         tls.VersionTLS13,
	})
	defer conn.Close()
	if err := conn.HandshakeContext(ctx); err != nil {
		return SyncSummary{}, err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Minute))
	key, err := clientHandshake(conn, code, cert.Certificate[0])
	if err != nil {
		return SyncSummary{}, err
	}
	return exchange(conn, key, opts, false)
}

// ---------------------------------------------------------------------------
// handshake
// ---------------------------------------------------------------------------

func serverHandshake(conn net.Conn, code string, selfDER []byte) ([]byte, error) {
	hello, err := readLine(conn)
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(hello)
	if len(parts) != 2 || parts[0] != "HELLO" {
		return nil, errors.New("peer did not send a transfer hello")
	}
	wantClientFP := fingerprint(conn)
	if wantClientFP == "" || !hmac.Equal([]byte(parts[1]), []byte(wantClientFP)) {
		return nil, errors.New("peer certificate does not match the fingerprint it claimed")
	}
	clientFP := parts[1]
	serverFP := selfFingerprint(selfDER)
	nonce, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CHALLENGE %s %s\n", serverFP, nonce); err != nil {
		return nil, err
	}
	line, err := readLine(conn)
	if err != nil {
		return nil, err
	}
	p := strings.Fields(line)
	if len(p) != 2 || p[0] != "PROOF" {
		return nil, errors.New("peer did not answer the pairing challenge")
	}
	want := proof(code, clientFP, serverFP, nonce)
	got, err := hex.DecodeString(p[1])
	if err != nil || !hmac.Equal(got, want) {
		return nil, errors.New("pairing code did not match")
	}
	ok, err := hex.DecodeString(p[1])
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "OK %s\n", hex.EncodeToString(ok)); err != nil {
		return nil, err
	}
	return sessionKey(code, clientFP, serverFP, nonce), nil
}

func clientHandshake(conn net.Conn, code string, selfDER []byte) ([]byte, error) {
	clientFP := selfFingerprint(selfDER)
	if _, err := fmt.Fprintf(conn, "HELLO %s\n", clientFP); err != nil {
		return nil, err
	}
	line, err := readLine(conn)
	if err != nil {
		return nil, err
	}
	p := strings.Fields(line)
	if len(p) != 3 || p[0] != "CHALLENGE" {
		return nil, errors.New("peer did not answer the transfer hello")
	}
	serverFP, nonce := p[1], p[2]
	if !hmac.Equal([]byte(serverFP), []byte(fingerprint(conn))) {
		return nil, errors.New("peer certificate does not match the fingerprint it claimed")
	}
	pr := proof(code, clientFP, serverFP, nonce)
	if _, err := fmt.Fprintf(conn, "PROOF %s\n", hex.EncodeToString(pr)); err != nil {
		return nil, err
	}
	line, err = readLine(conn)
	if err != nil {
		return nil, err
	}
	ok := strings.Fields(line)
	if len(ok) != 2 || ok[0] != "OK" {
		return nil, errors.New("pairing failed")
	}
	got, err := hex.DecodeString(ok[1])
	if err != nil || !hmac.Equal(got, pr) {
		return nil, errors.New("pairing response did not match")
	}
	return sessionKey(code, clientFP, serverFP, nonce), nil
}

func proof(code, clientFP, serverFP, nonce string) []byte {
	m := hmac.New(sha256.New, []byte(normalizeCode(code)))
	m.Write([]byte("conductor-transfer/1|" + clientFP + "|" + serverFP + "|" + nonce))
	return m.Sum(nil)
}

func sessionKey(code, clientFP, serverFP, nonce string) []byte {
	m := hmac.New(sha256.New, []byte(normalizeCode(code)))
	m.Write([]byte("session|" + clientFP + "|" + serverFP + "|" + nonce))
	return m.Sum(nil)
}

func selfFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func fingerprint(conn net.Conn) string {
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}
	st := tc.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return ""
	}
	sum := sha256.Sum256(st.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// exchange
// ---------------------------------------------------------------------------

type wireMessage struct {
	Type     string       `json:"type"` // bundle, empty, pull, result
	Bundle   []byte       `json:"bundle,omitempty"`
	WantBack bool         `json:"want_back,omitempty"`
	Result   *SyncSummary `json:"result,omitempty"`
}

// exchange runs one symmetric session: the initiator offers what it has (unless it is
// pull-only), the listener applies it and answers, and then the listener offers its own
// sessions back when the initiator asked for them. Import always goes through Import with
// Merge set, so a session that only grew is appended, never overwritten.
func exchange(conn net.Conn, key []byte, opts SyncOptions, server bool) (SyncSummary, error) {
	var summary SyncSummary
	send := func(m wireMessage) error {
		payload, err := json.Marshal(m)
		if err != nil {
			return err
		}
		return writeFrame(conn, key, payload)
	}
	recv := func() (wireMessage, error) {
		payload, err := readFrame(conn, key)
		if err != nil {
			return wireMessage{}, err
		}
		var m wireMessage
		return m, json.Unmarshal(payload, &m)
	}

	if !server {
		// Initiator: offer, or ask for the peer's sessions in pull mode.
		if opts.Direction == SyncPull {
			if err := send(wireMessage{Type: "pull", WantBack: true}); err != nil {
				return summary, err
			}
		} else {
			b, count, warns, err := buildSyncBundle(opts)
			if err != nil {
				return summary, err
			}
			summary.Sent = count
			summary.Warnings = append(summary.Warnings, warns...)
			msg := wireMessage{Type: "empty", WantBack: opts.Direction != SyncPush}
			msg.Bundle = b
			if len(b) > 0 {
				msg.Type = "bundle"
			}
			if err := send(msg); err != nil {
				return summary, err
			}
		}
		wantBack := opts.Direction != SyncPush
		if opts.Direction == SyncPush {
			m, err := recv()
			if err != nil {
				return summary, err
			}
			if m.Result != nil {
				summary.Import = m.Result.Import
			}
			return summary, nil
		}
		// Listen for either the answer to our offer or, in pull mode, the bundle.
		m, err := recv()
		if err != nil {
			return summary, err
		}
		switch m.Type {
		case "result":
			if m.Result != nil {
				summary.Import = m.Result.Import
			}
			if !wantBack {
				return summary, nil
			}
			m, err = recv()
			if err != nil {
				return summary, err
			}
		case "bundle", "empty":
			// pull mode: the peer sent its sessions directly
		default:
			return summary, fmt.Errorf("unexpected sync message %q", m.Type)
		}
		got, err := applyWire(m, opts)
		if err != nil {
			return summary, err
		}
		summary.Received = got.Received
		summary.Warnings = append(summary.Warnings, got.Warnings...)
		return summary, send(wireMessage{Type: "result", Result: &SyncSummary{Import: got.Import}})
	}

	// Listener: read the initiator's first message.
	m, err := recv()
	if err != nil {
		return summary, err
	}
	if m.Type != "pull" {
		got, err := applyWire(m, opts)
		if err != nil {
			return summary, err
		}
		summary.Received = got.Received
		summary.Warnings = append(summary.Warnings, got.Warnings...)
		summary.Import = got.Import
		if err := send(wireMessage{Type: "result", Result: &SyncSummary{Import: got.Import}}); err != nil {
			return summary, err
		}
	}
	if !m.WantBack {
		return summary, nil
	}
	b, count, warns, err := buildSyncBundle(opts)
	if err != nil {
		return summary, err
	}
	summary.Sent = count
	summary.Warnings = append(summary.Warnings, warns...)
	msg := wireMessage{Type: "empty"}
	msg.Bundle = b
	if len(b) > 0 {
		msg.Type = "bundle"
	}
	if err := send(msg); err != nil {
		return summary, err
	}
	reply, err := recv()
	if err != nil {
		return summary, err
	}
	if reply.Result != nil {
		summary.Import = reply.Result.Import
	}
	return summary, nil
}

func applyWire(m wireMessage, opts SyncOptions) (SyncSummary, error) {
	var got SyncSummary
	switch m.Type {
	case "empty":
		return got, nil
	case "pull":
		return got, nil
	case "bundle":
		b, err := OpenBytes(m.Bundle)
		if err != nil {
			return got, err
		}
		imp, err := Import(b, ImportOptions{
			ClaudeHome: opts.ClaudeHome,
			CodexHome:  opts.CodexHome,
			Merge:      true,
			DryRun:     opts.DryRun,
		})
		if err != nil {
			return got, err
		}
		got.Import = imp
		got.Received = len(imp.Items)
		return got, nil
	default:
		return got, fmt.Errorf("unexpected sync message %q", m.Type)
	}
}

// buildSyncBundle exports the selected sessions into memory. No sessions is not an error in
// a sync: it means there is nothing new to offer, and the peer is told so.
func buildSyncBundle(opts SyncOptions) ([]byte, int, []string, error) {
	var buf bytes.Buffer
	rep, err := Export(&buf, ExportOptions{
		Tool:            opts.Tool,
		Project:         opts.Project,
		ClaudeHome:      opts.ClaudeHome,
		CodexHome:       opts.CodexHome,
		Since:           opts.Since,
		IncludeArchived: opts.IncludeArch,
		Source:          "sync",
	})
	if errors.Is(err, ErrNothingSelected) {
		return nil, 0, nil, nil
	}
	if err != nil {
		return nil, 0, nil, err
	}
	return buf.Bytes(), rep.Sessions, rep.Warnings, nil
}

// ---------------------------------------------------------------------------
// framing and crypto
// ---------------------------------------------------------------------------

const maxFrame = 1 << 30

func writeFrame(w io.Writer, key, payload []byte) error {
	block, err := newGCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, block.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := block.Seal(nil, nonce, payload, nil)
	frame := make([]byte, 4+len(nonce)+len(sealed))
	binary.BigEndian.PutUint32(frame, uint32(len(nonce)+len(sealed)))
	copy(frame[4:], nonce)
	copy(frame[4+len(nonce):], sealed)
	_, err = w.Write(frame)
	return err
}

func readFrame(r io.Reader, key []byte) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return nil, fmt.Errorf("sync frame too large (%d bytes)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	block, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(buf) < block.NonceSize() {
		return nil, errors.New("short sync frame")
	}
	nonce := buf[:block.NonceSize()]
	return block.Open(nil, nonce, buf[block.NonceSize():], nil)
}

func readLine(r io.Reader) (string, error) {
	var b strings.Builder
	var one [1]byte
	for b.Len() <= 512 {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return strings.TrimSpace(b.String()), nil
		}
		b.WriteByte(one[0])
	}
	return "", errors.New("handshake line too long")
}

// ---------------------------------------------------------------------------
// certificates, codes, addresses
// ---------------------------------------------------------------------------

func ephemeralCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "conductor-session-transfer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func newPairingCode() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

func normalizeCode(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	c = strings.ReplaceAll(c, "-", "")
	c = strings.ReplaceAll(c, " ", "")
	return c
}

func formatCode(c string) string {
	var out strings.Builder
	for i, r := range c {
		if i > 0 && i%4 == 0 {
			out.WriteByte('-')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func ensurePrivateAddress(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("%s resolves to no address", host)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
			return fmt.Errorf("%s is not a private address; LAN sync stays on your local network", ip)
		}
	}
	return nil
}

// promptCode reads the pairing code from the terminal, so it never lands in shell history
// or a process list.
func promptCode() (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("no terminal to read the pairing code from; pass --code (it will be visible in shell history)")
	}
	defer tty.Close()
	fmt.Fprint(tty, "Pairing code: ")
	var line string
	if _, err := fmt.Fscanln(tty, &line); err != nil {
		return "", err
	}
	return line, nil
}

func advertiseAddr(a net.Addr) string {
	if ta, ok := a.(*net.TCPAddr); ok {
		if ta.IP.IsUnspecified() {
			if ip := firstPrivateIP(); ip != "" {
				return net.JoinHostPort(ip, fmt.Sprint(ta.Port))
			}
		}
	}
	return a.String()
}

func firstPrivateIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.IsPrivate() {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}
