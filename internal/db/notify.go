package db

import (
	"context"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// Notification channels and the outbox relay's bookkeeping (internal/notify). The store holds
// sealed credentials and delivery state; sealing, rendering, and sending are the notify
// package's.

// NotificationChannel is one project's outbound destination.
type NotificationChannel struct {
	ID             domain.ID `json:"id"`
	OrganizationID domain.ID `json:"-"`
	ProjectID      domain.ID `json:"project_id"`
	Kind           string    `json:"kind"`
	Name           string    `json:"name"`
	URLHint        string    `json:"url_hint"`
	Events         []string  `json:"events"`
	CreatedBy      domain.ID `json:"created_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`

	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	LastErrorAt   *time.Time `json:"last_error_at,omitempty"`
	Failures      int        `json:"failures"`
	RetryAfter    *time.Time `json:"retry_after,omitempty"`
	// Ready is false while the channel is backing off (RetryAfter is in the future), as
	// judged by the database's clock.
	Ready bool `json:"-"`

	// The sealed URL and signing secret. They never leave the server: the json tags keep
	// them out of any response that serializes a channel by mistake.
	URLSealed    string `json:"-"`
	SecretSealed string `json:"-"`
}

const channelColumns = `
	id::text, organization_id::text, project_id::text, kind, name, url_hint, events,
	COALESCE(created_by::text, ''), created_at, last_success_at, last_error, last_error_at,
	failures, retry_after, (retry_after IS NULL OR retry_after <= now()), url_sealed, secret_sealed`

func scanChannel(scan func(...any) error) (NotificationChannel, error) {
	var c NotificationChannel
	err := scan(&c.ID, &c.OrganizationID, &c.ProjectID, &c.Kind, &c.Name, &c.URLHint, &c.Events,
		&c.CreatedBy, &c.CreatedAt, &c.LastSuccessAt, &c.LastError, &c.LastErrorAt,
		&c.Failures, &c.RetryAfter, &c.Ready, &c.URLSealed, &c.SecretSealed)
	return c, err
}

// CreateNotificationChannel stores a channel whose URL and secret the caller has sealed.
func (s *Store) CreateNotificationChannel(ctx context.Context, c NotificationChannel) (NotificationChannel, error) {
	return scanChannel(s.pool.QueryRow(ctx, `
		INSERT INTO notification_channels (organization_id, project_id, kind, name, url_sealed,
		        url_hint, secret_sealed, events, created_by)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9::uuid)
		RETURNING `+channelColumns,
		c.OrganizationID, c.ProjectID, c.Kind, c.Name, c.URLSealed, c.URLHint, c.SecretSealed,
		c.Events, nullable(c.CreatedBy)).Scan)
}

// ListNotificationChannels returns a project's channels, oldest first.
func (s *Store) ListNotificationChannels(ctx context.Context, projectID domain.ID) ([]NotificationChannel, error) {
	return s.queryChannels(ctx, `
		SELECT `+channelColumns+` FROM notification_channels
		 WHERE project_id = $1::uuid ORDER BY created_at, id`, projectID)
}

// NotificationChannelsFor returns the channels of several projects at once, for the relay.
func (s *Store) NotificationChannelsFor(ctx context.Context, projectIDs []domain.ID) ([]NotificationChannel, error) {
	return s.queryChannels(ctx, `
		SELECT `+channelColumns+` FROM notification_channels
		 WHERE project_id = ANY($1::uuid[]) ORDER BY created_at, id`, projectIDs)
}

func (s *Store) queryChannels(ctx context.Context, sql string, args ...any) ([]NotificationChannel, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotificationChannel{}
	for rows.Next() {
		c, err := scanChannel(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetNotificationChannel finds a channel within a project. The project is part of the key on
// purpose (DESIGN.md §25.6): a channel id from another project is simply not found.
func (s *Store) GetNotificationChannel(ctx context.Context, projectID, id domain.ID) (NotificationChannel, error) {
	c, err := scanChannel(s.pool.QueryRow(ctx, `
		SELECT `+channelColumns+` FROM notification_channels
		 WHERE project_id = $1::uuid AND id = $2::uuid`, projectID, id).Scan)
	return c, noRows(err)
}

// DeleteNotificationChannel removes a channel and its delivery records.
func (s *Store) DeleteNotificationChannel(ctx context.Context, projectID, id domain.ID) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM notification_channels WHERE project_id = $1::uuid AND id = $2::uuid`, projectID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// RecordChannelSuccess clears a channel's failure streak and backoff.
func (s *Store) RecordChannelSuccess(ctx context.Context, id domain.ID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE notification_channels
		   SET last_success_at = now(), failures = 0, retry_after = NULL
		 WHERE id = $1::uuid`, id)
	return err
}

// RecordChannelFailure notes a failed send and backs the channel off: base doubled for each
// consecutive failure before this one, capped. It returns when the channel may be tried again.
//
// The backoff belongs to the channel rather than to each event, because what fails is almost
// always the endpoint — down, rate limiting, or revoked — and every event queued for it would
// otherwise be tried against it once per relay pass.
func (s *Store) RecordChannelFailure(ctx context.Context, id domain.ID, errText string, base, maxBackoff time.Duration) (time.Time, error) {
	var retryAfter time.Time
	err := s.pool.QueryRow(ctx, `
		UPDATE notification_channels
		   SET failures = failures + 1, last_error = $2, last_error_at = now(),
		       retry_after = now() + make_interval(secs =>
		           LEAST($4::float8, $3::float8 * power(2, LEAST(failures, 30))))
		 WHERE id = $1::uuid
		RETURNING retry_after`,
		id, errText, base.Seconds(), maxBackoff.Seconds()).Scan(&retryAfter)
	return retryAfter, noRows(err)
}

// Delivery states of one event to one channel.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
)

// OutboxItem is one claimed outbox row with its event and what has been delivered so far.
type OutboxItem struct {
	ID        int64
	ProjectID domain.ID
	Attempts  int
	Event     domain.Event
	// Deliveries is the per-channel state recorded on earlier passes, by channel id.
	Deliveries map[domain.ID]Delivery
}

// Delivery is the recorded state of one event to one channel.
type Delivery struct {
	State    string
	Attempts int
}

// ClaimOutbox claims up to limit undelivered outbox rows that are due, in projects that have
// at least one notification channel, and hides them from every other relay for hold.
//
// The claim is a committed update, not a held lock: the rows are selected FOR UPDATE SKIP
// LOCKED, so two relays (replicas) racing for them split them rather than both taking them,
// and their next_attempt_at moves past the hold, so a relay that starts after the claim
// commits does not see them as due. No transaction stays open across the HTTP calls that
// follow. If the claiming process dies, the rows become due again once the hold lapses.
//
// projects, when not empty, limits the claim to those projects.
func (s *Store) ClaimOutbox(ctx context.Context, limit int, hold time.Duration, projects []domain.ID) ([]OutboxItem, error) {
	if limit <= 0 {
		limit = 100
	}
	if len(projects) == 0 {
		projects = nil
	}
	// Candidates are read per project with a channel, through the (project_id, id) index of
	// pending rows, so the pass never walks the pending rows of the projects without one —
	// which retention keeps for weeks. The lock is then taken on the candidates by id.
	rows, err := s.pool.Query(ctx, `
		WITH candidates AS (
		    SELECT p.id
		      FROM (SELECT DISTINCT project_id FROM notification_channels
		             WHERE $3::uuid[] IS NULL OR project_id = ANY($3::uuid[])) c
		     CROSS JOIN LATERAL (
		           SELECT o.id FROM outbox_events o
		            WHERE o.project_id = c.project_id
		              AND o.delivered_at IS NULL
		              AND (o.next_attempt_at IS NULL OR o.next_attempt_at <= now())
		            ORDER BY o.id
		            LIMIT $1) p),
		picked AS (
		    SELECT o.id FROM outbox_events o
		     WHERE o.id IN (SELECT id FROM candidates)
		       AND o.delivered_at IS NULL
		       AND (o.next_attempt_at IS NULL OR o.next_attempt_at <= now())
		     ORDER BY o.id
		     LIMIT $1
		     FOR UPDATE SKIP LOCKED)
		UPDATE outbox_events o
		   SET next_attempt_at = now() + make_interval(secs => $2)
		  FROM picked
		 WHERE o.id = picked.id
		RETURNING o.id, o.project_id::text, o.attempts, o.event_id::text`,
		limit, hold.Seconds(), projects)
	if err != nil {
		return nil, err
	}
	var items []OutboxItem
	byEvent := map[domain.ID]int{}
	var eventIDs []domain.ID
	var outboxIDs []int64
	for rows.Next() {
		var it OutboxItem
		var eventID domain.ID
		if err := rows.Scan(&it.ID, &it.ProjectID, &it.Attempts, &eventID); err != nil {
			rows.Close()
			return nil, err
		}
		it.Event.ID = eventID
		it.Deliveries = map[domain.ID]Delivery{}
		byEvent[eventID] = len(items)
		items = append(items, it)
		eventIDs = append(eventIDs, eventID)
		outboxIDs = append(outboxIDs, it.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	evRows, err := s.pool.Query(ctx,
		`SELECT `+eventColumns+` FROM domain_events WHERE id = ANY($1::uuid[])`, eventIDs)
	if err != nil {
		return nil, err
	}
	for evRows.Next() {
		e, err := scanEvent(evRows.Scan)
		if err != nil {
			evRows.Close()
			return nil, err
		}
		if i, ok := byEvent[e.ID]; ok {
			items[i].Event = e
		}
	}
	evRows.Close()
	if err := evRows.Err(); err != nil {
		return nil, err
	}

	byOutbox := make(map[int64]int, len(items))
	for i, it := range items {
		byOutbox[it.ID] = i
	}
	dRows, err := s.pool.Query(ctx, `
		SELECT outbox_id, channel_id::text, state, attempts FROM notification_deliveries
		 WHERE outbox_id = ANY($1::bigint[])`, outboxIDs)
	if err != nil {
		return nil, err
	}
	defer dRows.Close()
	for dRows.Next() {
		var outboxID int64
		var channelID domain.ID
		var d Delivery
		if err := dRows.Scan(&outboxID, &channelID, &d.State, &d.Attempts); err != nil {
			return nil, err
		}
		if i, ok := byOutbox[outboxID]; ok {
			items[i].Deliveries[channelID] = d
		}
	}
	return items, dRows.Err()
}

// RecordDelivery stores the state of one event to one channel.
func (s *Store) RecordDelivery(ctx context.Context, outboxID int64, channelID domain.ID, state string, attempts int, errText string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification_deliveries (outbox_id, channel_id, state, attempts, last_error)
		VALUES ($1, $2::uuid, $3, $4, $5)
		ON CONFLICT (outbox_id, channel_id) DO UPDATE
		   SET state = EXCLUDED.state, attempts = EXCLUDED.attempts,
		       last_error = EXCLUDED.last_error, updated_at = now()`,
		outboxID, channelID, state, attempts, errText)
	return err
}

// FinishOutbox marks rows as done with: every channel that wanted them has them, or has
// given up. Retention then treats them as delivered.
func (s *Store) FinishOutbox(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox_events SET delivered_at = now(), next_attempt_at = NULL
		 WHERE id = ANY($1::bigint[]) AND delivered_at IS NULL`, ids)
	return err
}

// DeferOutbox makes a claimed row due again at a time (nil: now), recording how many
// attempts its most-tried channel has had.
func (s *Store) DeferOutbox(ctx context.Context, id int64, attempts int, at *time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox_events SET attempts = $2, next_attempt_at = COALESCE($3, now())
		 WHERE id = $1 AND delivered_at IS NULL`, id, attempts, at)
	return err
}
