package api

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Failed-authentication throttling.
//
// Tokens are 256 bits of entropy, so guessing one is not a realistic attack. The reason this
// exists is cheaper: without it, an unauthenticated client can drive an unbounded number of
// SHA-256 computations and database round trips from a single connection. Rate limiting the
// *failures* costs a legitimate client nothing — they authenticate on the first try.
//
// Deliberately in-process. A shared limiter would need Redis, and DESIGN.md §7.7 is explicit
// about not introducing Redis for something Postgres or memory can do.

const (
	// authFailureBudget is how many failures one client may accumulate before being asked
	// to wait. Generous enough that a misconfigured script gets a clear 401 several times
	// before it starts getting 429s.
	authFailureBudget = 10
	// authFailureWindow is how long a failure counts against the budget.
	authFailureWindow = 1 * time.Minute
	// authLimiterMaxEntries bounds memory. A flood from many source addresses evicts the
	// oldest entries rather than growing without limit.
	authLimiterMaxEntries = 8192
)

type authLimiter struct {
	mu      sync.Mutex
	entries map[string]*authFailureRecord
	now     func() time.Time
}

type authFailureRecord struct {
	count     int
	windowEnd time.Time
	lastSeen  time.Time
}

func newAuthLimiter() *authLimiter {
	return &authLimiter{entries: map[string]*authFailureRecord{}, now: time.Now}
}

// allow reports whether a client may attempt authentication, and how long to wait if not.
func (l *authLimiter) allow(client string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	rec, ok := l.entries[client]
	if !ok {
		return true, 0
	}
	if now.After(rec.windowEnd) {
		delete(l.entries, client)
		return true, 0
	}
	if rec.count < authFailureBudget {
		return true, 0
	}
	return false, rec.windowEnd.Sub(now)
}

// fail records an authentication failure.
func (l *authLimiter) fail(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	rec, ok := l.entries[client]
	if !ok || now.After(rec.windowEnd) {
		if len(l.entries) >= authLimiterMaxEntries {
			l.evictOldestLocked(now)
		}
		l.entries[client] = &authFailureRecord{
			count: 1, windowEnd: now.Add(authFailureWindow), lastSeen: now,
		}
		return
	}
	rec.count++
	rec.lastSeen = now
	// Each additional failure extends the window, so a client that keeps hammering keeps
	// waiting rather than getting a fresh budget every minute.
	rec.windowEnd = now.Add(authFailureWindow)
}

// succeed clears a client's failure record, so one fat-fingered token does not penalize a
// developer who then pastes the right one.
func (l *authLimiter) succeed(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, client)
}

// evictOldestLocked drops expired records, falling back to the least recently seen when
// nothing has expired.
func (l *authLimiter) evictOldestLocked(now time.Time) {
	for k, rec := range l.entries {
		if now.After(rec.windowEnd) {
			delete(l.entries, k)
		}
	}
	if len(l.entries) < authLimiterMaxEntries {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, rec := range l.entries {
		if oldestKey == "" || rec.lastSeen.Before(oldest) {
			oldestKey, oldest = k, rec.lastSeen
		}
	}
	delete(l.entries, oldestKey)
}

// clientKey identifies the caller for throttling.
//
// X-Forwarded-For is honoured only when the server was told it sits behind a trusted proxy.
// Trusting it unconditionally would let any client spoof its identity and bypass the limit
// entirely — the header is attacker-controlled otherwise.
//
// Even behind the proxy, only the right-most entry is used. A proxy appends the address it
// accepted the connection from, so the last hop is the one value the trusted proxy wrote;
// everything to its left arrived from the client and is whatever the client chose to send.
// Taking the left-most entry, as this once did, let a client mint a fresh throttle bucket per
// request by prefixing a random address of its own. With a chain of several trusted proxies
// the right-most entry is the nearest proxy's peer rather than the client, which only makes
// the throttle coarser — failures are shared more widely, never escaped.
func clientKey(r *http.Request, behindProxy bool) string {
	if behindProxy {
		// Several X-Forwarded-For headers are one list in order (RFC 7230 §3.2.2), so the
		// right-most entry is the last element of the last header.
		if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
			fwd := values[len(values)-1]
			if comma := lastIndexByte(fwd, ','); comma >= 0 {
				fwd = fwd[comma+1:]
			}
			if host := trimSpace(fwd); host != "" {
				return host
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
