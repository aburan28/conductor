package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

func TestSignatureVerifies(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "whsec_") || len(secret) < 40 {
		t.Fatalf("secret %q does not look like a signing secret", secret)
	}
	body := []byte(`{"id":"e1","type":"scope.released"}`)
	now := time.Unix(1_700_000_000, 0)
	h := http.Header{}
	h.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	h.Set(HeaderSignature, Sign(secret, now.Unix(), body))

	if err := Verify(secret, h, body, now.Add(30*time.Second), 0); err != nil {
		t.Fatalf("a fresh, correctly signed request was refused: %v", err)
	}
	if err := Verify(secret, h, append(body, ' '), now, 0); !errors.Is(err, ErrBadSignature) {
		t.Errorf("an altered body verified: %v", err)
	}
	if err := Verify("whsec_other", h, body, now, 0); !errors.Is(err, ErrBadSignature) {
		t.Errorf("another secret verified: %v", err)
	}
	// A captured request replayed later is refused even though its signature is intact.
	if err := Verify(secret, h, body, now.Add(10*time.Minute), 0); !errors.Is(err, ErrStale) {
		t.Errorf("a replay ten minutes later verified: %v", err)
	}
	// Moving the timestamp forward to dodge that breaks the signature.
	moved := h.Clone()
	moved.Set(HeaderTimestamp, strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10))
	if err := Verify(secret, moved, body, now.Add(10*time.Minute), 0); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a re-stamped replay verified: %v", err)
	}
	if err := Verify(secret, http.Header{}, body, now, 0); !errors.Is(err, ErrNoSignature) {
		t.Errorf("an unsigned request: %v", err)
	}
}

func TestValidateURL(t *testing.T) {
	strict := NetworkPolicy{}
	for _, raw := range []string{
		"http://hooks.example.com/x",   // plaintext
		"ftp://example.com/x",          // not http
		"https://127.0.0.1/hook",       // loopback
		"https://localhost:8080/hook",  // loopback by name
		"https://10.1.2.3/hook",        // private
		"https://169.254.169.254/meta", // link-local: cloud metadata
		"https://[::1]/hook",           // IPv6 loopback
		"https://[fd00::1]/hook",       // IPv6 unique local
		"https://100.64.0.1/hook",      // carrier-grade NAT
		"https://0.0.0.0/hook",         // unspecified
		"not a url",
		"https:///nohost",
	} {
		if _, err := strict.ValidateURL(raw); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s was accepted (%v)", raw, err)
		}
	}
	for _, raw := range []string{"https://hooks.slack.com/services/T0/B0/xyz", "https://8.8.8.8/hook"} {
		if _, err := strict.ValidateURL(raw); err != nil {
			t.Errorf("%s was refused: %v", raw, err)
		}
	}
	lan := NetworkPolicy{AllowPrivate: true, AllowHTTP: true}
	for _, raw := range []string{"http://10.1.2.3/hook", "http://localhost:9000/x"} {
		if _, err := lan.ValidateURL(raw); err != nil {
			t.Errorf("with the operator flags, %s was refused: %v", raw, err)
		}
	}
	// The error names the host and nothing of the path, where a webhook's token lives.
	_, err := strict.ValidateURL("http://hooks.example.com/services/SECRETTOKEN")
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("error leaks the URL path: %v", err)
	}
}

func TestPublicAddress(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.0.0.1": false, "172.16.5.4": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.100.1.1": false, "::1": false, "fe80::1": false,
		"::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "fd12::1": false, "224.0.0.1": false,
		"255.255.255.255": false, "0.0.0.0": false,
	} {
		if got := publicAddress(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddress(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The guard is applied at connect time, to the resolved address: a name that resolves to
// loopback is refused even though the URL contains no address at all.
func TestClientRefusesPrivateDestinations(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	strict := NetworkPolicy{AllowHTTP: true}.newClient(2 * time.Second)
	for _, target := range []string{srv.URL, "http://localhost:" + port + "/"} {
		resp, err := strict.Post(target, "application/json", strings.NewReader("{}"))
		if err == nil {
			resp.Body.Close()
			t.Fatalf("POST %s reached a loopback server", target)
		}
		if !errors.Is(err, errBlockedAddress) {
			t.Errorf("POST %s: %v, want the address guard's refusal", target, err)
		}
		if msg := sendError(err); strings.Contains(msg, port) && strings.Contains(msg, "http://") {
			t.Errorf("stored error quotes the URL: %s", msg)
		}
	}
	if hit {
		t.Fatal("the loopback server received a request")
	}

	lan := NetworkPolicy{AllowHTTP: true, AllowPrivate: true}.newClient(2 * time.Second)
	resp, err := lan.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("with private networks allowed: %v", err)
	}
	resp.Body.Close()
	if !hit {
		t.Fatal("with private networks allowed, the request did not arrive")
	}
}

// Redirects are not followed: a redirect is a second destination the address check and the
// operator never saw.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c := NetworkPolicy{AllowHTTP: true, AllowPrivate: true}.newClient(2 * time.Second)
	resp, err := c.Post(redirect.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if followed || resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect followed=%v status=%d", followed, resp.StatusCode)
	}
}

func TestNormalizeEvents(t *testing.T) {
	got, err := NormalizeEvents(nil)
	if err != nil || len(got) != len(DefaultEvents) {
		t.Fatalf("no events = %v, %v; want the defaults", got, err)
	}
	got, err = NormalizeEvents([]string{"scope.released, github.pr_merged", "scope.released", "task.status_changed:done"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "scope.released github.pr_merged task.status_changed:done" {
		t.Errorf("got %v", got)
	}
	for _, bad := range []string{"conflict.opened", "task.status_changed:nope", "scope.released:done", "attempt.progress"} {
		if _, err := NormalizeEvents([]string{bad}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	got, _ = NormalizeEvents([]string{"default", "task.claimed"})
	if len(got) != len(DefaultEvents)+1 {
		t.Errorf("default plus one = %v", got)
	}
}

// Every default subscription is in the catalog, so none of them is a promise that never fires.
func TestDefaultsAreInTheCatalog(t *testing.T) {
	if _, err := NormalizeEvents(DefaultEvents); err != nil {
		t.Fatal(err)
	}
}

func TestWants(t *testing.T) {
	done := domain.Event{Type: "task.status_changed", Payload: map[string]any{"to": "done"}}
	running := domain.Event{Type: "task.status_changed", Payload: map[string]any{"to": "running"}}
	if !wants([]string{"task.status_changed:done"}, done) || wants([]string{"task.status_changed:done"}, running) {
		t.Error("a status qualifier does not narrow task.status_changed")
	}
	if !wants([]string{"task.status_changed"}, running) {
		t.Error("an unqualified entry does not match every status")
	}
	if !wants([]string{AllEvents}, running) || wants([]string{AllEvents}, domain.Event{Type: "attempt.progress"}) {
		t.Error("* should be the catalog, not every event")
	}
}

// The message for an event about a private task says that something happened to "a private
// task" and nothing about which, where, or why — even where the projection would let a
// member see the task's ref and territory.
func TestPrivateMessageCarriesNoSpecifics(t *testing.T) {
	e := domain.Event{
		ID: "e1", Type: "scope.released", Visibility: domain.VisibilityPrivate, OccurredAt: time.Now(),
		Payload: map[string]any{
			"task_ref": "T-9", "resources": []any{"dir:internal/billing"}, "principal": "bob",
			"reason": "territory you were blocked on was released; check again",
		},
	}
	m := buildMessage("proj", e, "https://conductor.example.com")
	raw, _ := json.Marshal(m)
	slack, _ := slackBody(m)
	discord, _ := discordBody(m)
	for name, body := range map[string][]byte{"webhook": raw, "slack": slack, "discord": discord} {
		for _, leak := range []string{"T-9", "internal/billing", "/tasks/"} {
			if strings.Contains(string(body), leak) {
				t.Errorf("%s body carries %q:\n%s", name, leak, body)
			}
		}
		if !strings.Contains(string(body), "a private task") {
			t.Errorf("%s body does not say a private task:\n%s", name, body)
		}
	}

	e.Visibility = domain.VisibilityTeamSummary
	m = buildMessage("proj", e, "")
	if !strings.Contains(m.Text, "T-9") || !strings.Contains(m.Text, "dir:internal/billing") || !strings.Contains(m.Text, "@bob") {
		t.Errorf("a team-visible release should name the task, territory, and waiter: %s", m.Text)
	}
}

// Text from an event cannot become Slack markup: no links, no @channel.
func TestSlackBodyEscapesMarkup(t *testing.T) {
	m := Message{Project: "p", Type: "task.status_changed", Text: "<!channel> <https://evil|click> & co", OccurredAt: time.Now()}
	body, err := slackBody(m)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Blocks []struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	section := decoded.Blocks[0].Text.Text
	if strings.ContainsAny(section, "<>") || !strings.Contains(section, "&lt;!channel&gt;") {
		t.Errorf("section text is not escaped: %q", section)
	}
	d, _ := discordBody(m)
	if !strings.Contains(string(d), `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("discord body permits mentions: %s", d)
	}
}

// Through a configured proxy the address guard on the dialer sees only the proxy, so the
// destination is resolved and checked before the proxy is asked for it: a private target is
// refused — plain HTTP or CONNECT — and the proxy never hears of it.
func TestProxyStillRefusesPrivateDestinations(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	proxyURL, err := ParseProxy(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	dns := map[string][]netip.Addr{
		"public.test":   {netip.MustParseAddr("93.184.216.34")},
		"rebind.test":   {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.7")},
		"metadata.test": {netip.MustParseAddr("169.254.169.254")},
	}
	policy := NetworkPolicy{AllowHTTP: true, Proxy: proxyURL,
		resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
			if a, ok := dns[host]; ok {
				return a, nil
			}
			return nil, errors.New("no such host")
		}}
	client := policy.newClient(2 * time.Second)

	for _, target := range []string{
		"http://127.0.0.1:9/hook", "http://localhost:9/hook", "http://rebind.test/hook",
		"http://metadata.test/latest", "https://10.0.0.5/hook", "https://[::1]/hook",
	} {
		resp, err := client.Post(target, "application/json", strings.NewReader("{}"))
		if err == nil {
			resp.Body.Close()
			t.Errorf("POST %s through the proxy was allowed", target)
			continue
		}
		if !errors.Is(err, errBlockedAddress) {
			t.Errorf("POST %s: %v, want the destination check's refusal", target, err)
		}
	}
	mu.Lock()
	if len(seen) != 0 {
		t.Errorf("the proxy was asked for private destinations: %v", seen)
	}
	mu.Unlock()

	// A public destination goes through the proxy, which may itself be on a private address.
	resp, err := client.Post("http://public.test/hook", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("public destination through the proxy: %v", err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "POST public.test" {
		t.Errorf("proxy saw %v, want the one public request", seen)
	}
}

func TestParseProxy(t *testing.T) {
	for _, ok := range []string{"http://proxy:3128", "https://user:pw@proxy.internal", "socks5://127.0.0.1:1080"} {
		if _, err := ParseProxy(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"proxy:3128", "ftp://proxy", ""} {
		if _, err := ParseProxy(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ParseProxy("ftp://user:hunter2@proxy"); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error quotes the proxy's credentials: %v", err)
	}
}

// The conflict events render as a sentence a team can act on, and say nothing specific about
// a private side.
func TestConflictMessages(t *testing.T) {
	detected := domain.Event{ID: "e", Type: "conflict.detected", Visibility: domain.VisibilityTeamSummary,
		Payload: map[string]any{"task_ref": "T-1", "with_task_ref": "T-2", "kind": "merge_risk",
			"severity": "high", "suggestion": "suggest_split", "changed_paths": []any{"internal/x.go"}}}
	if got := buildMessage("p", detected, "").Text; got !=
		"Conflict detected between T-1 and T-2 (merge_risk, high); both changed internal/x.go; suggestion: suggest split" {
		t.Errorf("detected: %q", got)
	}
	blocked := domain.Event{ID: "e", Type: "conflict.blocked", Visibility: domain.VisibilityTeamSummary,
		Payload: map[string]any{"task_ref": "T-3", "principal": "bob", "resources": []any{"dir:internal/x"}}}
	if got := buildMessage("p", blocked, "").Text; got !=
		"@bob was blocked by T-3's territory (dir:internal/x). They are told when it is released." {
		t.Errorf("blocked: %q", got)
	}
	for _, e := range []domain.Event{detected, blocked} {
		e.Visibility = domain.VisibilityPrivate
		raw, _ := json.Marshal(buildMessage("p", e, ""))
		for _, leak := range []string{"T-1", "T-2", "T-3", "internal/x"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("private %s carries %q: %s", e.Type, leak, raw)
			}
		}
	}
}
