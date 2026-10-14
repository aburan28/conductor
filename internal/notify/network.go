package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

// Outbound requests go to URLs that project maintainers type in, from inside the network the
// control plane runs in. Unchecked, "notify this URL" is a way to make conductord POST to its
// own admin ports, the cloud metadata service, or anything else on the LAN (server-side
// request forgery). So by default only public addresses are reachable, and the check is made
// on the address actually dialed — after DNS resolution, on every connection — so a name that
// resolves to a public address at creation and to 10.0.0.5 later is still refused.

// NetworkPolicy says which outbound destinations are allowed.
type NetworkPolicy struct {
	// AllowPrivate permits loopback, private, link-local, and other non-public addresses:
	// a self-hosted chat server on the LAN, or a receiver in local testing.
	AllowPrivate bool
	// AllowHTTP permits plain http:// URLs. The payload and its signature would cross the
	// network in the clear, and a Slack-style URL is itself the credential, so this is for
	// local testing only.
	AllowHTTP bool
	// Proxy, when set, is the forward proxy every notification goes through (http, https,
	// or socks5). The destination is still checked: resolved here and refused if any of its
	// addresses is not public, before the proxy is asked to connect to it.
	Proxy *url.URL

	// resolver looks destination names up when a proxy is in use; tests replace it.
	resolver func(ctx context.Context, host string) ([]netip.Addr, error)
}

// ParseProxy validates a --notify-proxy value.
func ParseProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("notify proxy %q is not a URL like http://proxy.internal:3128", redactURL(raw))
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("notify proxy must be http, https or socks5, not %q", u.Scheme)
	}
	if u.Scheme == "socks5h" {
		// socks5h asks the proxy to resolve names, which is the default for socks5 in Go
		// anyway; the destination check below resolves them here as well.
		u.Scheme = "socks5"
	}
	return u, nil
}

// errBlockedAddress is returned when a destination is not a public address.
var errBlockedAddress = errors.New("refusing to connect to a non-public address")

// nonPublic lists ranges that are not reachable public unicast space beyond what netip's
// predicates cover.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT, often a cloud's internal network
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and the broadcast address
	netip.MustParsePrefix("fec0::/10"),       // deprecated site-local
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
}

// publicAddress reports whether ip is ordinary public unicast space.
func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// ValidateURL checks a channel URL when it is added: scheme, host, and — for a literal
// address or localhost — whether it is reachable under the policy. Names are checked again,
// resolved, on every connection.
func (p NetworkPolicy) ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Opaque != "" {
		return nil, fmt.Errorf("%w: %q is not an absolute URL", domain.ErrInvalidArgument, redactURL(raw))
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowHTTP {
			return nil, fmt.Errorf("%w: notification URLs must use https (conductord --notify-allow-http permits http for local testing)",
				domain.ErrInvalidArgument)
		}
	default:
		return nil, fmt.Errorf("%w: notification URLs must use https, not %q", domain.ErrInvalidArgument, u.Scheme)
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("%w: a notification URL has no #fragment", domain.ErrInvalidArgument)
	}
	host := u.Hostname()
	if !p.AllowPrivate {
		if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
			return nil, fmt.Errorf("%w: %s is this machine; %s", domain.ErrInvalidArgument, host, allowPrivateHint)
		}
		if ip, err := netip.ParseAddr(host); err == nil && !publicAddress(ip) {
			return nil, fmt.Errorf("%w: %s is not a public address; %s", domain.ErrInvalidArgument, ip, allowPrivateHint)
		}
	}
	return u, nil
}

const allowPrivateHint = "conductord --notify-allow-private-networks permits destinations on a private network"

// guardDial refuses connections to non-public addresses. It runs after DNS resolution, on
// the address the socket is about to connect to.
func (p NetworkPolicy) guardDial(_, address string, _ syscall.RawConn) error {
	if p.AllowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %s", errBlockedAddress, host)
	}
	if !publicAddress(ip) {
		return fmt.Errorf("%w %s (%s)", errBlockedAddress, ip, allowPrivateHint)
	}
	return nil
}

// checkDestination resolves a destination host and refuses it unless every address it has is
// public. It is the check a proxy cannot make for us: once a request is handed to a proxy,
// the address conductord would dial is the proxy's, and the guard on the dialer sees only
// that.
//
// The proxy resolves the name again for itself, and its answer can differ from this one
// (a different resolver, split-horizon DNS, or a record that changes in between — DNS
// rebinding). Refusing names that resolve privately here closes the plain case; a proxy that
// itself refuses private destinations closes the rest (docs/OPERATIONS.md).
func (p NetworkPolicy) checkDestination(ctx context.Context, host string) error {
	if p.AllowPrivate {
		return nil
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return fmt.Errorf("%w %s (%s)", errBlockedAddress, host, allowPrivateHint)
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		resolve := p.resolver
		if resolve == nil {
			resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			}
		}
		if addrs, err = resolve(ctx, host); err != nil {
			return fmt.Errorf("resolving %s: %w", host, err)
		}
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%s has no addresses", host)
	}
	for _, ip := range addrs {
		if !publicAddress(ip) {
			return fmt.Errorf("%w %s: %s resolves to %s (%s)", errBlockedAddress, host, host, ip, allowPrivateHint)
		}
	}
	return nil
}

// newClient builds the HTTP client every notification is sent with.
//
// Without a configured proxy it connects directly, never through HTTP(S)_PROXY from the
// environment, and checks the address on every connection after DNS (guardDial). With one,
// it connects only to the proxy, and the proxy hook refuses a destination that resolves to a
// non-public address before the proxy is asked for it (checkDestination) — so a CONNECT to a
// private target is never sent. Either way it follows no redirects, which would be a second,
// unchecked destination for the same credentials. The timeout bounds the whole request,
// including reading the response.
func (p NetworkPolicy) newClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: min(timeout, 10*time.Second), Control: p.guardDial}
	var proxy func(*http.Request) (*url.URL, error)
	if p.Proxy != nil {
		// The proxy is the operator's and may well be on a private network; the dialer only
		// ever connects to it, so its own address is not checked.
		dialer.Control = nil
		proxyURL := p.Proxy
		proxy = func(r *http.Request) (*url.URL, error) {
			if err := p.checkDestination(r.Context(), r.URL.Hostname()); err != nil {
				return nil, err
			}
			return proxyURL, nil
		}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 proxy,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   min(timeout, 10*time.Second),
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// sendError describes a failed request without the URL. Go's *url.Error quotes the full URL,
// and for Slack and Discord the URL is the credential; the error is stored and shown to
// project maintainers, so it must not carry it.
func sendError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return err.Error()
}

// redactURL shows enough of a URL to recognise it — the scheme and host — and nothing of
// its path, query, or user info, which is where webhook credentials live.
func redactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "…"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// urlHint is what a channel shows of its URL after creation: the host and the last four
// characters, so two channels to the same service can be told apart.
func urlHint(u *url.URL) string {
	full := u.String()
	tail := full
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return u.Host + "/…" + tail
}
