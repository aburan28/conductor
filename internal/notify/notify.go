// Package notify sends a project's domain events to the places a team already looks: generic
// webhooks (signed JSON), Slack incoming webhooks, and Discord webhooks.
//
// Events reach it through the transactional outbox (DESIGN.md §23.3): every event is written
// with an outbox row in the same transaction as the state change it describes, and the relay
// here claims undelivered rows, sends each to the project's channels that subscribe to it, and
// marks it delivered. Delivery is at least once; a receiver deduplicates by event id.
//
// Two properties matter more than the rest:
//
//   - Privacy. A channel is project-wide and its receiver is outside Conductor's access
//     checks, so every event leaves through the same projection the API applies for an
//     ordinary project member (coord.ProjectEvents), narrowed further for private tasks
//     (render.go). A private task's title, intent, and paths never reach a channel.
//   - Credentials. A Slack or Discord URL is a bearer credential, and a webhook's signing
//     secret is what its receiver trusts; both are sealed at rest under conductord's secret
//     key and are never returned by the API after creation.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/secretbox"
)

// Channel kinds.
const (
	KindWebhook = "webhook"
	KindSlack   = "slack"
	KindDiscord = "discord"
)

// Seal purposes, bound into each sealed value so a URL cannot be passed off as a secret.
const (
	sealURL    = "conductor/notify/url"
	sealSecret = "conductor/notify/secret"
)

// maxChannelsPerProject bounds what one maintainer can make the relay fan out to.
const maxChannelsPerProject = 20

// Options configures a Notifier.
type Options struct {
	// SecretKey seals channel URLs and secrets. Without one, channels cannot be created.
	SecretKey *secretbox.Source
	// Network says which destinations may be reached.
	Network NetworkPolicy
	// DashboardURL, when set, is linked from each message.
	DashboardURL string
	Logger       *slog.Logger

	// Poll is how often the relay looks for undelivered events.
	Poll time.Duration
	// Batch is how many outbox rows one pass claims.
	Batch int
	// Concurrency bounds how many channels are sent to at once. Each channel's events go
	// one at a time, in order.
	Concurrency int
	// RequestTimeout bounds one HTTP request, including reading the response.
	RequestTimeout time.Duration
	// PassTimeout bounds one relay pass. Rows not reached in time are released for the next.
	PassTimeout time.Duration
	// MaxAttempts is how many failed sends of one event to one channel are made before it is
	// given up; MaxAge is how old an event may be and still be sent at all — a stall alert
	// from yesterday is noise, not news.
	MaxAttempts int
	MaxAge      time.Duration
	// BackoffBase doubles with each consecutive failure of a channel, up to BackoffMax.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// Projects, when not empty, limits the relay to these projects. conductord leaves it
	// empty; test suites sharing one database use it to stay out of each other's outbox.
	Projects []domain.ID
}

func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Poll <= 0 {
		o.Poll = 3 * time.Second
	}
	if o.Batch <= 0 {
		o.Batch = 100
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 10 * time.Second
	}
	if o.PassTimeout <= 0 {
		o.PassTimeout = 60 * time.Second
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 8
	}
	if o.MaxAge <= 0 {
		o.MaxAge = 24 * time.Hour
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = 15 * time.Second
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = time.Hour
	}
	return o
}

// Notifier manages channels and runs the relay.
type Notifier struct {
	store  *db.Store
	svc    *coord.Service
	opts   Options
	client *http.Client
}

// New returns a Notifier.
func New(store *db.Store, svc *coord.Service, opts Options) *Notifier {
	opts = opts.withDefaults()
	return &Notifier{store: store, svc: svc, opts: opts, client: opts.Network.newClient(opts.RequestTimeout)}
}

// ChannelView is a channel as the API returns it: everything but its credentials.
type ChannelView struct {
	ID            domain.ID  `json:"id"`
	Kind          string     `json:"kind"`
	Name          string     `json:"name,omitempty"`
	URLHint       string     `json:"url_hint"`
	Signed        bool       `json:"signed"`
	Events        []string   `json:"events"`
	CreatedBy     domain.ID  `json:"created_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	LastErrorAt   *time.Time `json:"last_error_at,omitempty"`
	Failures      int        `json:"failures"`
	RetryAfter    *time.Time `json:"retry_after,omitempty"`
}

func view(c db.NotificationChannel) ChannelView {
	return ChannelView{
		ID: c.ID, Kind: c.Kind, Name: c.Name, URLHint: c.URLHint, Signed: c.SecretSealed != "",
		Events: c.Events, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt,
		LastSuccessAt: c.LastSuccessAt, LastError: c.LastError, LastErrorAt: c.LastErrorAt,
		Failures: c.Failures, RetryAfter: c.RetryAfter,
	}
}

// CreateRequest describes a new channel.
type CreateRequest struct {
	Kind   string   `json:"kind"`
	URL    string   `json:"url"`
	Name   string   `json:"name,omitempty"`
	Events []string `json:"events,omitempty"`
}

// Created is a new channel, with the webhook signing secret — shown this once.
type Created struct {
	ChannelView
	Secret string `json:"secret,omitempty"`
}

// Create validates, seals, and stores a channel.
func (n *Notifier) Create(ctx context.Context, project domain.Project, by domain.ID, req CreateRequest) (Created, error) {
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	switch kind {
	case KindWebhook, KindSlack, KindDiscord:
	default:
		return Created{}, fmt.Errorf("%w: kind must be webhook, slack, or discord", domain.ErrInvalidArgument)
	}
	u, err := n.opts.Network.ValidateURL(req.URL)
	if err != nil {
		return Created{}, err
	}
	events, err := NormalizeEvents(req.Events)
	if err != nil {
		return Created{}, err
	}
	name := strings.TrimSpace(req.Name)
	if len(name) > 80 {
		return Created{}, fmt.Errorf("%w: name is at most 80 characters", domain.ErrInvalidArgument)
	}
	existing, err := n.store.ListNotificationChannels(ctx, project.ID)
	if err != nil {
		return Created{}, err
	}
	if len(existing) >= maxChannelsPerProject {
		return Created{}, fmt.Errorf("%w: a project has at most %d notification channels", domain.ErrInvalidArgument, maxChannelsPerProject)
	}
	if n.opts.SecretKey == nil {
		return Created{}, errors.New("no secret key is configured to seal notification credentials")
	}
	key, err := n.opts.SecretKey.Get(true)
	if err != nil {
		return Created{}, err
	}
	sealedURL, err := key.Seal([]byte(u.String()), sealURL)
	if err != nil {
		return Created{}, err
	}
	var secret, sealedSecret string
	if kind == KindWebhook {
		if secret, err = NewSecret(); err != nil {
			return Created{}, err
		}
		if sealedSecret, err = key.Seal([]byte(secret), sealSecret); err != nil {
			return Created{}, err
		}
	}
	ch, err := n.store.CreateNotificationChannel(ctx, db.NotificationChannel{
		OrganizationID: project.OrganizationID, ProjectID: project.ID, Kind: kind, Name: name,
		URLSealed: sealedURL, URLHint: urlHint(u), SecretSealed: sealedSecret, Events: events,
		CreatedBy: by,
	})
	if err != nil {
		return Created{}, err
	}
	return Created{ChannelView: view(ch), Secret: secret}, nil
}

// List returns a project's channels.
func (n *Notifier) List(ctx context.Context, projectID domain.ID) ([]ChannelView, error) {
	chans, err := n.store.ListNotificationChannels(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelView, 0, len(chans))
	for _, c := range chans {
		out = append(out, view(c))
	}
	return out, nil
}

// Get returns one of a project's channels.
func (n *Notifier) Get(ctx context.Context, projectID, id domain.ID) (ChannelView, error) {
	c, err := n.store.GetNotificationChannel(ctx, projectID, id)
	if err != nil {
		return ChannelView{}, err
	}
	return view(c), nil
}

// Delete removes a channel.
func (n *Notifier) Delete(ctx context.Context, projectID, id domain.ID) error {
	return n.store.DeleteNotificationChannel(ctx, projectID, id)
}

// TestResult reports a test send.
type TestResult struct {
	OK     bool   `json:"ok"`
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Test sends a test message to a channel now, ignoring its backoff, and reports what the
// receiver answered.
func (n *Notifier) Test(ctx context.Context, project domain.Project, id domain.ID) (TestResult, error) {
	c, err := n.store.GetNotificationChannel(ctx, project.ID, id)
	if err != nil {
		return TestResult{}, err
	}
	res := n.send(ctx, c, testMessage(project.Slug, n.opts.DashboardURL, time.Now()))
	return TestResult{OK: res.err == "", Status: res.status, Error: res.err}, nil
}

// sendResult is one HTTP attempt's outcome.
type sendResult struct {
	status int
	err    string
	// permanent means sending the same message again cannot succeed (the receiver refused
	// it), so it is not retried.
	permanent bool
}

// send delivers one message to one channel.
func (n *Notifier) send(ctx context.Context, c db.NotificationChannel, m Message) sendResult {
	if n.opts.SecretKey == nil {
		return sendResult{err: "no secret key is configured to unseal the channel"}
	}
	key, err := n.opts.SecretKey.Get(false)
	if err != nil {
		return sendResult{err: "cannot unseal the channel: " + err.Error()}
	}
	rawURL, err := key.Open(c.URLSealed, sealURL)
	if err != nil {
		return sendResult{err: "cannot unseal the channel: " + err.Error()}
	}
	var body []byte
	switch c.Kind {
	case KindSlack:
		body, err = slackBody(m)
	case KindDiscord:
		body, err = discordBody(m)
	default:
		body, err = jsonBody(m)
	}
	if err != nil {
		return sendResult{err: err.Error(), permanent: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, string(rawURL), bytes.NewReader(body))
	if err != nil {
		return sendResult{err: sendError(err), permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Conductor-Notify/1")
	if c.Kind == KindWebhook {
		secret, err := key.Open(c.SecretSealed, sealSecret)
		if err != nil {
			return sendResult{err: "cannot unseal the signing secret: " + err.Error()}
		}
		ts := time.Now().Unix()
		req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
		req.Header.Set(HeaderSignature, Sign(string(secret), ts, body))
		req.Header.Set(HeaderEvent, m.Type)
		req.Header.Set(HeaderDelivery, m.ID)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return sendResult{err: sendError(err)}
	}
	defer resp.Body.Close()
	// A short read of the answer, for the error message; the rest is drained so the
	// connection can be reused.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return sendResult{status: resp.StatusCode}
	}
	msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if text := oneLine(string(snippet)); text != "" {
		msg += ": " + text
	}
	// 4xx means the receiver will not take this request as it is; 408 and 429 are the
	// exceptions that mean "later". 3xx is a redirect, which is not followed.
	permanent := resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout &&
		resp.StatusCode != http.StatusTooManyRequests
	return sendResult{status: resp.StatusCode, err: msg, permanent: permanent}
}

func jsonBody(m Message) ([]byte, error) { return json.Marshal(m) }

// oneLine makes a receiver's answer safe to store and show: one line, printable, short.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return truncate(strings.TrimSpace(s), 160)
}

// truncate shortens s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
