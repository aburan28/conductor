package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These exist because the first implementation had the sense of `allow()` inverted — fresh
// clients were throttled and exhausted ones were let through. It read correctly and behaved
// backwards, which is precisely the class of bug a unit test catches for free.

func TestLimiterAllowsUntilBudgetIsSpent(t *testing.T) {
	l := newAuthLimiter()

	for i := 0; i < authFailureBudget; i++ {
		if ok, _ := l.allow("client"); !ok {
			t.Fatalf("client was throttled after %d failures, budget is %d", i, authFailureBudget)
		}
		l.fail("client")
	}

	ok, retryAfter := l.allow("client")
	if ok {
		t.Errorf("client should be throttled after %d failures", authFailureBudget)
	}
	if retryAfter <= 0 || retryAfter > authFailureWindow {
		t.Errorf("retryAfter = %s, want a positive duration within the window", retryAfter)
	}
}

func TestLimiterIsPerClient(t *testing.T) {
	l := newAuthLimiter()
	for i := 0; i < authFailureBudget+5; i++ {
		l.fail("noisy")
	}
	if ok, _ := l.allow("noisy"); ok {
		t.Error("the noisy client should be throttled")
	}
	if ok, _ := l.allow("quiet"); !ok {
		t.Error("one client's failures must not throttle another")
	}
}

func TestLimiterForgivesOnSuccess(t *testing.T) {
	l := newAuthLimiter()
	for i := 0; i < authFailureBudget+3; i++ {
		l.fail("client")
	}
	l.succeed("client")
	if ok, _ := l.allow("client"); !ok {
		t.Error("a successful authentication should clear the failure record")
	}
}

func TestLimiterWindowExpires(t *testing.T) {
	l := newAuthLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < authFailureBudget+1; i++ {
		l.fail("client")
	}
	if ok, _ := l.allow("client"); ok {
		t.Fatal("should be throttled immediately after exhausting the budget")
	}

	now = now.Add(authFailureWindow + time.Second)
	if ok, _ := l.allow("client"); !ok {
		t.Error("the throttle should lift once the window passes")
	}
}

func TestLimiterBoundsMemory(t *testing.T) {
	l := newAuthLimiter()
	for i := 0; i < authLimiterMaxEntries+500; i++ {
		l.fail(string(rune(i%1114111)) + "-client")
	}
	l.mu.Lock()
	size := len(l.entries)
	l.mu.Unlock()
	if size > authLimiterMaxEntries {
		t.Errorf("limiter holds %d entries, cap is %d", size, authLimiterMaxEntries)
	}
}

// X-Forwarded-For is attacker-controlled unless a proxy rewrites it. Honouring it by default
// would let any client pick its own throttle bucket and bypass the limit entirely.
func TestClientKeyIgnoresForwardedHeaderUnlessBehindProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	r.RemoteAddr = "10.0.0.5:44321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := clientKey(r, false); got != "10.0.0.5" {
		t.Errorf("clientKey(behindProxy=false) = %q, want the socket address 10.0.0.5", got)
	}
	if got := clientKey(r, true); got != "1.2.3.4" {
		t.Errorf("clientKey(behindProxy=true) = %q, want the forwarded client 1.2.3.4", got)
	}
}

func TestClientKeyTakesLeftmostForwardedEntry(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	r.RemoteAddr = "10.0.0.5:44321"
	// A proxy appends as it forwards, so the original client is left-most.
	r.Header.Set("X-Forwarded-For", " 1.2.3.4 , 10.0.0.1")
	if got := clientKey(r, true); got != "1.2.3.4" {
		t.Errorf("clientKey = %q, want 1.2.3.4", got)
	}
}

func TestClientKeyFallsBackWhenRemoteAddrHasNoPort(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	r.RemoteAddr = "not-an-address"
	if got := clientKey(r, false); got != "not-an-address" {
		t.Errorf("clientKey = %q, want the raw value as a fallback", got)
	}
}
