package notify

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/metrics"
)

var (
	notificationsSent = metrics.Default.NewCounter("conductor_notifications_total",
		"Notification sends, by channel kind and outcome (delivered, retry, failed, expired).", "kind", "outcome")
	relayErrors = metrics.Default.NewCounter("conductor_notify_relay_errors_total",
		"Notification relay passes that failed.")
)

// observer is the viewer every outbound event is projected for: a project member with no
// relationship to the work. Its empty principal id matches no task's owner and no event's
// actor, so no owner's view of their own work ever leaves through a channel.
var observer = coord.Caller{Role: domain.RoleObserver}

// Run relays until ctx ends. A pass already under way when ctx ends finishes the request it
// is making (bounded by RequestTimeout) and records what happened, then Run returns; nothing
// is cut off between sending an event and recording that it was sent.
func (n *Notifier) Run(ctx context.Context) error {
	ticker := time.NewTicker(n.opts.Poll)
	defer ticker.Stop()
	n.opts.Logger.Info("notification relay started", "poll", n.opts.Poll.String())
	for {
		select {
		case <-ctx.Done():
			n.opts.Logger.Info("notification relay stopped")
			return ctx.Err()
		case <-ticker.C:
			// Keep passing while there is a backlog, so a burst drains at the speed of the
			// receivers rather than one batch per poll.
			for ctx.Err() == nil {
				report, err := n.Pass(ctx)
				if err != nil {
					relayErrors.Inc()
					n.opts.Logger.Error("notification relay pass failed", "error", err)
					break
				}
				if report.Claimed < n.opts.Batch {
					break
				}
			}
		}
	}
}

// PassReport summarizes one relay pass.
type PassReport struct {
	Claimed   int
	Delivered int
	Failed    int
	Retrying  int
	Finished  int
}

// job is one event bound for one channel.
type job struct {
	item    *db.OutboxItem
	message Message
}

// outcome is what happened to one job.
type outcome struct {
	state    string // db.Delivery*, or "" when the job was not attempted
	attempts int
	after    *time.Time // when the job may be tried again, if it is still pending
}

// Pass claims a batch of undelivered events, sends each to the channels that want it, and
// records the result. ctx ending stops the pass from starting new sends; the ones in flight
// and the bookkeeping complete under a context of their own, bounded by PassTimeout.
func (n *Notifier) Pass(ctx context.Context) (PassReport, error) {
	var report PassReport
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), n.opts.PassTimeout)
	defer cancel()

	items, err := n.store.ClaimOutbox(work, n.opts.Batch, 2*n.opts.PassTimeout, n.opts.Projects)
	if err != nil || len(items) == 0 {
		return report, err
	}
	report.Claimed = len(items)

	byProject := map[domain.ID][]*db.OutboxItem{}
	var projectIDs []domain.ID
	for i := range items {
		it := &items[i]
		if _, ok := byProject[it.ProjectID]; !ok {
			projectIDs = append(projectIDs, it.ProjectID)
		}
		byProject[it.ProjectID] = append(byProject[it.ProjectID], it)
	}
	channels, err := n.store.NotificationChannelsFor(work, projectIDs)
	if err != nil {
		n.release(items)
		return report, err
	}
	chansByProject := map[domain.ID][]db.NotificationChannel{}
	for _, c := range channels {
		chansByProject[c.ProjectID] = append(chansByProject[c.ProjectID], c)
	}

	// Plan: which channel gets which event, rendered once per event.
	jobs := map[domain.ID][]job{}
	chanByID := map[domain.ID]db.NotificationChannel{}
	expected := map[int64][]domain.ID{} // outbox id -> channels it is waiting on
	for _, pid := range projectIDs {
		project, err := n.store.GetProject(work, pid)
		if err != nil {
			n.release(items)
			return report, err
		}
		pending := byProject[pid]
		events := make([]domain.Event, len(pending))
		for i, it := range pending {
			events[i] = it.Event
		}
		// The same projection the API applies to a project member's event feed.
		projected, err := n.svc.ProjectEvents(work, observer, pid, events)
		if err != nil {
			n.release(items)
			return report, err
		}
		for i, it := range pending {
			if it.Event.Type == "" {
				continue // the event itself is gone (retention); nothing to send
			}
			var msg *Message
			for _, c := range chansByProject[pid] {
				// A channel hears about what happened after it was added, not the backlog.
				if it.Event.OccurredAt.Before(c.CreatedAt) || !wants(c.Events, it.Event) {
					continue
				}
				if d, ok := it.Deliveries[c.ID]; ok && d.State != db.DeliveryPending {
					continue
				}
				if msg == nil {
					m := buildMessage(project.Slug, projected[i], n.opts.DashboardURL)
					msg = &m
				}
				chanByID[c.ID] = c
				jobs[c.ID] = append(jobs[c.ID], job{item: it, message: *msg})
				expected[it.ID] = append(expected[it.ID], c.ID)
			}
		}
	}

	// Send: channels in parallel up to Concurrency, each channel's events in order.
	type key struct {
		outbox  int64
		channel domain.ID
	}
	var mu sync.Mutex
	results := map[key]outcome{}
	sem := make(chan struct{}, n.opts.Concurrency)
	var wg sync.WaitGroup
	for cid, list := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(c db.NotificationChannel, list []job) {
			defer wg.Done()
			defer func() { <-sem }()
			for _, out := range n.deliverChannel(ctx, work, c, list) {
				mu.Lock()
				results[key{out.outbox, c.ID}] = out.outcome
				switch out.outcome.state {
				case db.DeliveryDelivered:
					report.Delivered++
				case db.DeliveryFailed:
					report.Failed++
				case db.DeliveryPending:
					report.Retrying++
				}
				mu.Unlock()
			}
		}(chanByID[cid], list)
	}
	wg.Wait()

	// Settle each row: done when no channel is still waiting for it, otherwise due again
	// when the soonest waiting channel may be tried.
	var finished []int64
	var firstErr error
	for i := range items {
		it := &items[i]
		attempts := it.Attempts
		var next *time.Time
		waiting, dueNow := false, false
		for _, cid := range expected[it.ID] {
			out := results[key{it.ID, cid}]
			if out.state == db.DeliveryDelivered || out.state == db.DeliveryFailed {
				continue
			}
			waiting = true
			attempts = max(attempts, out.attempts)
			if out.after == nil {
				dueNow = true // this one was simply not reached
			} else if next == nil || out.after.Before(*next) {
				t := *out.after
				next = &t
			}
		}
		if dueNow {
			next = nil
		}
		if !waiting {
			finished = append(finished, it.ID)
			continue
		}
		if err := n.store.DeferOutbox(work, it.ID, attempts, next); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := n.store.FinishOutbox(work, finished); err != nil && firstErr == nil {
		firstErr = err
	}
	report.Finished = len(finished)
	return report, firstErr
}

type channelOutcome struct {
	outbox  int64
	outcome outcome
}

// deliverChannel sends a channel its events in order. After the first failure the channel is
// backing off, and the rest wait for it rather than each being tried against an endpoint that
// just failed. stop ending (shutdown) or the pass running out of time leaves the rest unsent
// and due immediately.
func (n *Notifier) deliverChannel(stop, work context.Context, c db.NotificationChannel, list []job) []channelOutcome {
	out := make([]channelOutcome, 0, len(list))
	var backoff *time.Time
	if !c.Ready && c.RetryAfter != nil {
		t := *c.RetryAfter
		backoff = &t
	}
	for _, j := range list {
		prior := j.item.Deliveries[c.ID].Attempts
		if backoff != nil {
			out = append(out, channelOutcome{j.item.ID, outcome{state: db.DeliveryPending, attempts: prior, after: backoff}})
			continue
		}
		if stop.Err() != nil || work.Err() != nil {
			out = append(out, channelOutcome{j.item.ID, outcome{attempts: prior}})
			continue
		}
		if time.Since(j.item.Event.OccurredAt) > n.opts.MaxAge {
			n.record(work, j.item.ID, c.ID, db.DeliveryFailed, prior, "expired: the event is older than the notification window")
			notificationsSent.Inc(c.Kind, "expired")
			out = append(out, channelOutcome{j.item.ID, outcome{state: db.DeliveryFailed, attempts: prior}})
			continue
		}

		res := n.send(work, c, j.message)
		if res.err == "" {
			n.record(work, j.item.ID, c.ID, db.DeliveryDelivered, prior+1, "")
			if err := n.store.RecordChannelSuccess(work, c.ID); err != nil {
				n.opts.Logger.Warn("record notification success failed", "channel", c.ID, "error", err)
			}
			notificationsSent.Inc(c.Kind, "delivered")
			out = append(out, channelOutcome{j.item.ID, outcome{state: db.DeliveryDelivered, attempts: prior + 1}})
			continue
		}
		if errors.Is(work.Err(), context.DeadlineExceeded) && !res.permanent && stop.Err() == nil {
			// The pass ran out of time mid-request; that says nothing about the channel.
			out = append(out, channelOutcome{j.item.ID, outcome{attempts: prior}})
			continue
		}

		attempts := prior + 1
		retryAt, err := n.store.RecordChannelFailure(work, c.ID, res.err, n.opts.BackoffBase, n.opts.BackoffMax)
		if err != nil {
			n.opts.Logger.Warn("record notification failure failed", "channel", c.ID, "error", err)
			retryAt = time.Now().Add(n.opts.BackoffBase)
		}
		n.opts.Logger.Warn("notification not delivered", "channel", c.ID, "kind", c.Kind,
			"event", j.message.Type, "attempt", attempts, "error", res.err)
		if res.permanent || attempts >= n.opts.MaxAttempts {
			n.record(work, j.item.ID, c.ID, db.DeliveryFailed, attempts, res.err)
			notificationsSent.Inc(c.Kind, "failed")
			out = append(out, channelOutcome{j.item.ID, outcome{state: db.DeliveryFailed, attempts: attempts}})
		} else {
			n.record(work, j.item.ID, c.ID, db.DeliveryPending, attempts, res.err)
			notificationsSent.Inc(c.Kind, "retry")
			out = append(out, channelOutcome{j.item.ID, outcome{state: db.DeliveryPending, attempts: attempts, after: &retryAt}})
		}
		backoff = &retryAt
	}
	return out
}

func (n *Notifier) record(ctx context.Context, outboxID int64, channelID domain.ID, state string, attempts int, errText string) {
	if err := n.store.RecordDelivery(ctx, outboxID, channelID, state, attempts, errText); err != nil {
		n.opts.Logger.Warn("record notification delivery failed", "channel", channelID, "error", err)
	}
}

// release makes claimed rows due again after a pass failed before sending anything.
func (n *Notifier) release(items []db.OutboxItem) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, it := range items {
		_ = n.store.DeferOutbox(ctx, it.ID, it.Attempts, nil)
	}
}
