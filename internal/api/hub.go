package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/metrics"
)

// eventHub fans one read of a project's event log out to every stream open on it.
//
// Each stream used to poll the database every second on its own, so database load grew with
// the number of open dashboards rather than with the number of projects being watched. The
// hub runs one poller per project with at least one subscriber, and stops it when the last
// one leaves. It also enforces the stream caps, so a client cannot open connections until
// the server runs out of goroutines or file descriptors.
//
// The hub only moves events; it does not decide who may see them. Every event goes to every
// subscriber of its project, and the stream handler applies the caller's view.
type eventHub struct {
	store           *db.Store
	logger          *slog.Logger
	baseCtx         context.Context
	poll            time.Duration
	maxTotal        int
	maxPerPrincipal int

	mu           sync.Mutex
	total        int
	perPrincipal map[domain.ID]int
	feeds        map[domain.ID]*projectFeed
}

type projectFeed struct {
	subs   map[*subscription]struct{}
	cancel context.CancelFunc
}

// subscription is one stream's view of a feed. events is closed when the subscriber is too
// slow to keep up (it should reconnect and re-read), or when the server shuts down.
type subscription struct {
	hub       *eventHub
	project   domain.ID
	principal domain.ID
	events    chan domain.Event
	closed    bool
}

// subscriberBuffer is how far a stream may fall behind its feed before it is dropped.
const subscriberBuffer = 512

var streamsOpen = metrics.Default.NewGauge("conductor_event_streams_open",
	"Open event-stream connections.")
var streamFeeds = metrics.Default.NewGauge("conductor_event_stream_feeds",
	"Projects with at least one open event stream, each polled once per interval.")
var streamsRejected = metrics.Default.NewCounter("conductor_event_streams_rejected_total",
	"Event-stream connections refused by the caps.")

func newEventHub(store *db.Store, logger *slog.Logger, baseCtx context.Context, poll time.Duration, maxTotal, maxPerPrincipal int) *eventHub {
	return &eventHub{
		store: store, logger: logger, baseCtx: baseCtx, poll: poll,
		maxTotal: maxTotal, maxPerPrincipal: maxPerPrincipal,
		perPrincipal: map[domain.ID]int{}, feeds: map[domain.ID]*projectFeed{},
	}
}

// subscribe opens a stream on a project for a principal, or returns errTooManyStreams.
func (h *eventHub) subscribe(project, principal domain.ID) (*subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.baseCtx.Err() != nil {
		return nil, h.baseCtx.Err()
	}
	if h.total >= h.maxTotal || h.perPrincipal[principal] >= h.maxPerPrincipal {
		streamsRejected.Inc()
		return nil, errTooManyStreams
	}
	sub := &subscription{hub: h, project: project, principal: principal,
		events: make(chan domain.Event, subscriberBuffer)}
	feed, ok := h.feeds[project]
	if !ok {
		ctx, cancel := context.WithCancel(h.baseCtx)
		feed = &projectFeed{subs: map[*subscription]struct{}{}, cancel: cancel}
		h.feeds[project] = feed
		streamFeeds.Add(1)
		go h.run(ctx, project, feed)
	}
	feed.subs[sub] = struct{}{}
	h.total++
	h.perPrincipal[principal]++
	streamsOpen.Add(1)
	return sub, nil
}

// close ends the subscription. It is safe to call more than once.
func (sub *subscription) close() {
	h := sub.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(sub)
}

func (h *eventHub) removeLocked(sub *subscription) {
	if sub.closed {
		return
	}
	sub.closed = true
	close(sub.events)
	h.total--
	if h.perPrincipal[sub.principal]--; h.perPrincipal[sub.principal] <= 0 {
		delete(h.perPrincipal, sub.principal)
	}
	streamsOpen.Add(-1)
	if feed, ok := h.feeds[sub.project]; ok {
		delete(feed.subs, sub)
		if len(feed.subs) == 0 {
			feed.cancel()
			delete(h.feeds, sub.project)
			streamFeeds.Add(-1)
		}
	}
}

// run polls one project's events and hands each to every subscriber. It starts a little in
// the past, matching the backlog a new stream reads, so nothing that committed between a
// subscriber's backlog read and the feed's first poll is missed; subscribers drop what they
// have already seen.
func (h *eventHub) run(ctx context.Context, project domain.ID, feed *projectFeed) {
	cursor := time.Now().Add(-streamBacklog)
	var lastID domain.ID
	ticker := time.NewTicker(h.poll)
	defer ticker.Stop()
	defer func() {
		// The server is shutting down (or the last subscriber left): end every stream.
		h.mu.Lock()
		for sub := range feed.subs {
			h.removeLocked(sub)
		}
		h.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for {
			events, err := h.store.EventsSince(ctx, project, cursor, lastID, 500)
			if err != nil {
				if ctx.Err() == nil {
					// A database blip: keep the streams open and try again next interval.
					h.logger.Debug("event feed poll failed", "project", project, "error", err)
				}
				break
			}
			if len(events) == 0 {
				break
			}
			h.mu.Lock()
			for _, e := range events {
				for sub := range feed.subs {
					select {
					case sub.events <- e:
					default:
						// Too slow to keep up. Dropping it bounds memory; the client
						// reconnects and re-reads its backlog.
						h.removeLocked(sub)
					}
				}
			}
			h.mu.Unlock()
			last := events[len(events)-1]
			cursor, lastID = last.OccurredAt, last.ID
			if len(events) < 500 {
				break
			}
		}
	}
}

// streamBacklog is how far back a new stream starts.
const streamBacklog = 2 * time.Minute

// after reports whether e sorts after the (at, id) cursor, in EventsSince's order.
func after(e domain.Event, at time.Time, id domain.ID) bool {
	return e.OccurredAt.After(at) || (e.OccurredAt.Equal(at) && string(e.ID) > string(id))
}
