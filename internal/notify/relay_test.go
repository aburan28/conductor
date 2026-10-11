package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/secretbox"
)

// Relay tests run against a schema of their own, like the scheduler's: the relay claims
// across every project with a channel, and the shared test database is other packages' too.

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixture struct {
	t       *testing.T
	ctx     context.Context
	dsn     string
	store   *db.Store
	project domain.Project
	alice   domain.Principal
	key     *secretbox.Source
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping notification relay integration tests")
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("notifytest_%d", time.Now().UnixNano())
	if _, err := admin.Pool().Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Pool().Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	f := &fixture{t: t, ctx: ctx, dsn: u.String()}
	f.store = f.open()
	if err := f.store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	org, err := f.store.CreateOrganization(ctx, "org", "Org")
	if err != nil {
		t.Fatal(err)
	}
	f.alice, err = f.store.CreatePrincipal(ctx, org.ID, domain.PrincipalHuman, "alice", "Alice", "")
	if err != nil {
		t.Fatal(err)
	}
	f.project, err = f.store.CreateProject(ctx, db.CreateProjectParams{
		OrganizationID: org.ID, Slug: "proj", DisplayName: "Proj", Config: domain.DefaultProjectConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	f.key = &secretbox.Source{Env: raw}
	return f
}

// open returns another connection pool on the fixture's schema: a second replica.
func (f *fixture) open() *db.Store {
	f.t.Helper()
	s, err := db.Open(f.ctx, f.dsn)
	if err != nil {
		f.t.Fatalf("open: %v", err)
	}
	f.t.Cleanup(s.Close)
	return s
}

func (f *fixture) notifier(store *db.Store, tune func(*Options)) *Notifier {
	opts := Options{
		SecretKey: f.key, Network: NetworkPolicy{AllowPrivate: true, AllowHTTP: true},
		Logger: quiet, BackoffBase: time.Second, RequestTimeout: 5 * time.Second,
	}
	if tune != nil {
		tune(&opts)
	}
	return New(store, coord.New(store), opts)
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.store.Pool().Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.store.Pool().QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// event appends a project-level event of a subscribed type.
func (f *fixture) event(typ string, payload map[string]any) {
	f.t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	if err := f.store.AppendEvent(f.ctx, f.project.OrganizationID, f.project.ID, "", "project", f.project.ID,
		typ, domain.VisibilityTeamSummary, payload); err != nil {
		f.t.Fatal(err)
	}
}

// due makes every waiting row and channel ready now, standing in for the backoff elapsing.
func (f *fixture) due() {
	f.exec(`UPDATE outbox_events SET next_attempt_at = NULL WHERE delivered_at IS NULL`)
	f.exec(`UPDATE notification_channels SET retry_after = NULL`)
}

// receiver records what it is sent and answers with the status code reply returns.
type receiver struct {
	mu       sync.Mutex
	server   *httptest.Server
	requests []received
	reply    func(n int) int
	delay    time.Duration
}

type received struct {
	header http.Header
	body   []byte
}

func newReceiver(t *testing.T, reply func(n int) int) *receiver {
	r := &receiver{reply: reply}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, received{req.Header.Clone(), body})
		n := len(r.requests)
		r.mu.Unlock()
		if r.delay > 0 {
			time.Sleep(r.delay)
		}
		code := http.StatusOK
		if r.reply != nil {
			code = r.reply(n)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte("answer\nwith a newline"))
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *receiver) got() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.requests...)
}

func (f *fixture) channel(n *Notifier, kind, target string, events ...string) Created {
	f.t.Helper()
	c, err := n.Create(f.ctx, f.project, f.alice.ID, CreateRequest{Kind: kind, URL: target, Events: events})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// Two relays (two replicas, separate pools) racing over one backlog deliver every event
// exactly once: FOR UPDATE SKIP LOCKED splits the claim between them.
func TestRelayDeliversOnceAcrossReplicas(t *testing.T) {
	f := newFixture(t)
	recv := newReceiver(t, nil)
	recv.delay = 2 * time.Millisecond
	tune := func(o *Options) { o.Batch = 4 }
	a, b := f.notifier(f.store, tune), f.notifier(f.open(), tune)
	ch := f.channel(a, KindWebhook, recv.server.URL, "attempt.stalled")
	const events = 40
	for i := 0; i < events; i++ {
		f.event("attempt.stalled", map[string]any{"task_ref": fmt.Sprintf("T-%d", i), "harness": "claude"})
	}

	var wg sync.WaitGroup
	claimed := make([]int, 2)
	for i, n := range []*Notifier{a, b} {
		wg.Add(1)
		go func(i int, n *Notifier) {
			defer wg.Done()
			for {
				r, err := n.Pass(f.ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if r.Claimed == 0 {
					return
				}
				claimed[i] += r.Claimed
			}
		}(i, n)
	}
	wg.Wait()

	got := recv.got()
	seen := map[string]int{}
	for _, r := range got {
		if err := Verify(ch.Secret, r.header, r.body, time.Now(), 0); err != nil {
			t.Errorf("delivery does not verify with the channel's secret: %v", err)
		}
		var m Message
		if err := json.Unmarshal(r.body, &m); err != nil {
			t.Fatal(err)
		}
		if r.header.Get(HeaderDelivery) != m.ID || r.header.Get(HeaderEvent) != "attempt.stalled" {
			t.Errorf("headers %v do not describe %s", r.header, m.ID)
		}
		seen[m.ID]++
	}
	if len(seen) != events {
		t.Errorf("%d distinct events delivered, want %d", len(seen), events)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("event %s delivered %d times", id, n)
		}
	}
	if claimed[0] == 0 || claimed[1] == 0 {
		t.Logf("one relay claimed everything (%v); the split is timing-dependent, the exactly-once check is not", claimed)
	}
	if n := f.count(`SELECT count(*) FROM outbox_events WHERE delivered_at IS NULL`); n != 0 {
		t.Errorf("%d outbox rows still undelivered", n)
	}
}

// A failing receiver is retried with exponential backoff per channel, and the event is
// delivered once it recovers.
func TestRelayRetriesWithBackoff(t *testing.T) {
	f := newFixture(t)
	recv := newReceiver(t, func(n int) int {
		if n <= 2 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	n := f.notifier(f.store, func(o *Options) { o.BackoffBase = 10 * time.Second })
	ch := f.channel(n, KindWebhook, recv.server.URL)
	f.event("budget.downshift", map[string]any{"cost_usd": 12.5, "reason": "downshift threshold reached"})

	backoff := func() time.Duration {
		var secs float64
		if err := f.store.Pool().QueryRow(f.ctx, `
			SELECT EXTRACT(EPOCH FROM retry_after - now()) FROM notification_channels WHERE id = $1::uuid`,
			ch.ID).Scan(&secs); err != nil {
			t.Fatal(err)
		}
		return time.Duration(secs * float64(time.Second))
	}

	r, err := n.Pass(f.ctx)
	if err != nil || r.Retrying != 1 {
		t.Fatalf("first pass = %+v, %v; want one retry", r, err)
	}
	if d := backoff(); d < 8*time.Second || d > 11*time.Second {
		t.Errorf("first backoff %v, want about the 10s base", d)
	}
	// The row waits for the channel: an immediate pass claims nothing.
	if r, _ := n.Pass(f.ctx); r.Claimed != 0 {
		t.Errorf("a backing-off event was claimed again at once: %+v", r)
	}
	view, err := n.Get(f.ctx, f.project.ID, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Failures != 1 || !strings.HasPrefix(view.LastError, "HTTP 503") || strings.Contains(view.LastError, "\n") {
		t.Errorf("channel health = %+v", view)
	}

	f.due()
	if r, err := n.Pass(f.ctx); err != nil || r.Retrying != 1 {
		t.Fatalf("second pass = %+v, %v", r, err)
	}
	// Doubled: the second consecutive failure waits twice as long.
	if d := backoff(); d < 18*time.Second || d > 21*time.Second {
		t.Errorf("second backoff %v, want about 20s", d)
	}

	f.due()
	if r, err := n.Pass(f.ctx); err != nil || r.Delivered != 1 || r.Finished != 1 {
		t.Fatalf("third pass = %+v, %v; want the delivery", r, err)
	}
	if got := len(recv.got()); got != 3 {
		t.Errorf("receiver saw %d requests, want 3", got)
	}
	view, _ = n.Get(f.ctx, f.project.ID, ch.ID)
	if view.Failures != 0 || view.LastSuccessAt == nil || view.RetryAfter != nil {
		t.Errorf("after recovery the channel is %+v", view)
	}
	if n := f.count(`SELECT count(*) FROM outbox_events WHERE delivered_at IS NULL`); n != 0 {
		t.Errorf("%d rows undelivered after success", n)
	}
	if n := f.count(`SELECT attempts FROM notification_deliveries WHERE channel_id = $1::uuid AND state = 'delivered'`, ch.ID); n != 3 {
		t.Errorf("delivery recorded %d attempts, want 3", n)
	}
}

// The backoff is capped, and an event is given up after MaxAttempts: the row is then done
// with, so retention can prune it.
func TestRelayGivesUpAfterMaxAttempts(t *testing.T) {
	f := newFixture(t)
	recv := newReceiver(t, func(int) int { return http.StatusInternalServerError })
	n := f.notifier(f.store, func(o *Options) {
		o.MaxAttempts = 3
		o.BackoffBase = 10 * time.Second
		o.BackoffMax = 25 * time.Second
	})
	ch := f.channel(n, KindWebhook, recv.server.URL)
	f.event("budget.exhausted", nil)
	for i := 0; i < 3; i++ {
		if _, err := n.Pass(f.ctx); err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			f.due()
		}
	}
	var secs float64
	_ = f.store.Pool().QueryRow(f.ctx, `SELECT EXTRACT(EPOCH FROM retry_after - now()) FROM notification_channels WHERE id = $1::uuid`, ch.ID).Scan(&secs)
	if secs > 26 {
		t.Errorf("backoff %vs exceeds the 25s cap", secs)
	}
	if got := len(recv.got()); got != 3 {
		t.Errorf("%d sends, want 3", got)
	}
	if n := f.count(`SELECT count(*) FROM notification_deliveries WHERE state = 'failed' AND attempts = 3`); n != 1 {
		t.Error("the delivery was not given up after three attempts")
	}
	if n := f.count(`SELECT count(*) FROM outbox_events WHERE delivered_at IS NULL`); n != 0 {
		t.Error("a given-up row is still pending")
	}
}

// A receiver that refuses the request (4xx) is not retried for that event, and while a
// channel is backing off, the rest of its queue waits instead of hammering it.
func TestRelayPermanentFailureAndQueuedEventsWait(t *testing.T) {
	f := newFixture(t)
	recv := newReceiver(t, func(int) int { return http.StatusBadRequest })
	n := f.notifier(f.store, nil)
	f.channel(n, KindWebhook, recv.server.URL)
	f.event("quota.warning", map[string]any{"harness": "claude", "kind": "5h", "percent_hint": 85})
	f.event("quota.exhausted", map[string]any{"harness": "claude", "kind": "5h"})

	r, err := n.Pass(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Failed != 1 || r.Retrying != 1 || len(recv.got()) != 1 {
		t.Fatalf("pass = %+v with %d sends; want the first refused and the second held back", r, len(recv.got()))
	}
	if n := f.count(`SELECT count(*) FROM outbox_events WHERE delivered_at IS NULL AND next_attempt_at > now()`); n != 1 {
		t.Errorf("%d rows waiting for the channel's backoff, want 1", n)
	}
}

// A channel hears about what happens after it is added and what it subscribed to; anything
// else in the outbox is settled without a send.
func TestRelaySendsOnlySubscribedEventsAfterCreation(t *testing.T) {
	f := newFixture(t)
	recv := newReceiver(t, nil)
	n := f.notifier(f.store, nil)
	f.event("attempt.stalled", nil) // before the channel
	f.exec(`UPDATE domain_events SET occurred_at = now() - interval '1 minute'`)
	f.channel(n, KindWebhook, recv.server.URL, "task.status_changed:done")
	f.event("task.status_changed", map[string]any{"from": "merging", "to": "done", "task_ref": "T-1"})
	f.event("task.status_changed", map[string]any{"from": "ready", "to": "claimed", "task_ref": "T-2"})
	f.event("attempt.progress", nil)

	r, err := n.Pass(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Claimed != 4 || r.Finished != 4 || r.Delivered != 1 {
		t.Fatalf("pass = %+v; want four rows settled and one sent", r)
	}
	got := recv.got()
	if len(got) != 1 || !strings.Contains(string(got[0].body), `"T-1 is now done (was merging)"`) {
		t.Fatalf("sent %d: %s", len(got), got)
	}
}

// Slack and Discord get their own body shapes, unsigned.
func TestRelaySlackAndDiscordBodies(t *testing.T) {
	f := newFixture(t)
	slack, discord := newReceiver(t, nil), newReceiver(t, nil)
	n := f.notifier(f.store, nil)
	if c := f.channel(n, KindSlack, slack.server.URL); c.Secret != "" || c.Signed {
		t.Errorf("a Slack channel got a signing secret: %+v", c)
	}
	f.channel(n, KindDiscord, discord.server.URL)
	f.event("github.pr_merged", map[string]any{"task_ref": "T-7", "branch": "feat/x"})
	if _, err := n.Pass(f.ctx); err != nil {
		t.Fatal(err)
	}
	s, d := slack.got(), discord.got()
	if len(s) != 1 || len(d) != 1 {
		t.Fatalf("slack %d, discord %d sends", len(s), len(d))
	}
	var sb struct {
		Text   string           `json:"text"`
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(s[0].body, &sb); err != nil || len(sb.Blocks) != 2 ||
		!strings.Contains(sb.Text, "Pull request for T-7 merged (feat/x)") {
		t.Errorf("slack body: %s (%v)", s[0].body, err)
	}
	if s[0].header.Get(HeaderSignature) != "" {
		t.Error("a Slack request carries a webhook signature")
	}
	if !strings.Contains(string(d[0].body), `"content"`) {
		t.Errorf("discord body: %s", d[0].body)
	}
}

// The URL and secret are sealed in the database: a dump of the row carries neither.
func TestChannelCredentialsAreSealedAtRest(t *testing.T) {
	f := newFixture(t)
	n := f.notifier(f.store, nil)
	const token = "Q7xT9mZpL2vR8sK4"
	c := f.channel(n, KindWebhook, "http://127.0.0.1:9/services/"+token)
	var row string
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT row_to_json(c)::text FROM notification_channels c WHERE id = $1::uuid`, c.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	secretBody := strings.TrimPrefix(c.Secret, "whsec_")
	for _, plain := range []string{token, "/services/", secretBody} {
		if strings.Contains(row, plain) {
			t.Errorf("the stored row contains %q in the clear:\n%s", plain, row)
		}
	}
	if !strings.HasSuffix(c.URLHint, token[len(token)-4:]) || strings.Contains(c.URLHint, token) {
		t.Errorf("hint %q should show the last four characters and no more", c.URLHint)
	}
	// And the server cannot use them without its key.
	other, _ := secretbox.GenerateKey()
	wrong := New(f.store, coord.New(f.store), Options{SecretKey: &secretbox.Source{Env: other},
		Network: NetworkPolicy{AllowPrivate: true, AllowHTTP: true}, Logger: quiet})
	res, err := wrong.Test(f.ctx, f.project, c.ID)
	if err != nil || res.OK || !strings.Contains(res.Error, "unseal") {
		t.Errorf("test send under another key = %+v, %v", res, err)
	}
}

// A pass started as the server shuts down sends nothing new and hands its rows back, due now.
func TestPassAfterShutdownSendsNothing(t *testing.T) {
	f := newFixture(t)
	recv := newReceiver(t, nil)
	n := f.notifier(f.store, nil)
	f.channel(n, KindWebhook, recv.server.URL)
	f.event("attempt.stalled", nil)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	r, err := n.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Claimed != 1 || r.Delivered != 0 || len(recv.got()) != 0 {
		t.Fatalf("pass after shutdown = %+v, %d sends", r, len(recv.got()))
	}
	if n := f.count(`SELECT count(*) FROM outbox_events WHERE delivered_at IS NULL AND next_attempt_at <= now()`); n != 1 {
		t.Error("the unsent row is not due again")
	}
	if n := f.count(`SELECT count(*) FROM notification_channels WHERE failures > 0`); n != 0 {
		t.Error("shutdown was counted as a channel failure")
	}
}
