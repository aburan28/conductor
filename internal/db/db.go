// Package db is the persistence layer. It owns every SQL statement in the system.
//
// Two conventions run throughout, both chosen to keep the rest of the codebase free of
// database-specific types:
//
//   - UUIDs cross this boundary as plain strings. Selects cast with `::text`, parameters
//     cast with `$n::uuid`.
//   - Nullable columns are coalesced to zero values in SQL (`COALESCE(x::text, ”)`) so Go
//     structs can use plain fields rather than pointers for everything optional.
package db

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aburan28/conductor/internal/domain"
)

// Store is the handle every service takes. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	// now is injectable so time-dependent behaviour (lease expiry, stall detection) can be
	// driven deterministically in tests instead of with sleeps.
	now func() time.Time
	// auditHook, when set, sees every audit record after it is written (audit.go).
	auditHook atomic.Pointer[AuditHook]
}

// Default server-side bounds on a single statement and on a single lock wait. Every request
// handler and scheduler step runs short statements, so a statement that takes this long is
// hung — on a lock held by a stuck transaction, say — and failing it is what lets the
// scheduler's next tick and the next request proceed.
const (
	DefaultStatementTimeout = 30 * time.Second
	DefaultLockTimeout      = 10 * time.Second
)

// Options bounds what one database call may cost. The zero value applies the defaults above;
// a negative duration disables that bound. A bound set in the DSN itself
// (`?statement_timeout=...`) wins over both.
type Options struct {
	StatementTimeout time.Duration
	LockTimeout      time.Duration
}

// ValidateDSN checks a deployment before opening a connection. RDS uses the same
// PostgreSQL driver, with certificate chain and hostname verification and an explicit
// root certificate pool. Custom DNS and China-region endpoints are supported. Errors
// deliberately omit the connection string, which can contain credentials.
func ValidateDSN(mode, dsn string) error {
	if mode == "" {
		mode = "external"
	}
	switch mode {
	case "local", "external", "rds":
	default:
		return errors.New("database mode must be local, external or rds")
	}
	if strings.TrimSpace(dsn) == "" {
		return errors.New("a PostgreSQL connection string is required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errors.New("could not parse the PostgreSQL connection string or read its TLS files")
	}
	if mode != "rds" {
		return nil
	}
	verified := func(host string, c *tls.Config) bool {
		return host != "" && !strings.HasPrefix(host, "/") && c != nil &&
			!c.InsecureSkipVerify && c.RootCAs != nil && c.ServerName == host
	}
	if !verified(cfg.ConnConfig.Host, cfg.ConnConfig.TLSConfig) {
		return errors.New("RDS requires sslmode=verify-full and sslrootcert pointing to a trusted CA bundle")
	}
	for _, fallback := range cfg.ConnConfig.Fallbacks {
		if !verified(fallback.Host, fallback.TLSConfig) {
			return errors.New("every RDS fallback must verify TLS with a configured CA bundle")
		}
	}
	return nil
}

func (o Options) withDefaults() Options {
	if o.StatementTimeout == 0 {
		o.StatementTimeout = DefaultStatementTimeout
	}
	if o.LockTimeout == 0 {
		o.LockTimeout = DefaultLockTimeout
	}
	return o
}

// Open connects to Postgres and verifies the connection. opts is optional; at most one is
// used.
func Open(ctx context.Context, dsn string, opts ...Options) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	o = o.withDefaults()
	// The claim path takes short row locks and advisory locks; a starved pool turns those
	// into queueing rather than errors, so keep some headroom. Note the floor is per
	// process: N replicas hold up to N times this many connections.
	if cfg.MaxConns < 8 {
		cfg.MaxConns = 8
	}
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	// Startup parameters rather than a SET per checkout: they apply to every connection the
	// pool ever opens and cost no round trip. Migrate lifts them for its own connection.
	setTimeout := func(name string, d time.Duration) {
		if _, set := cfg.ConnConfig.RuntimeParams[name]; set || d < 0 {
			return
		}
		cfg.ConnConfig.RuntimeParams[name] = strconv.FormatInt(d.Milliseconds(), 10)
	}
	setTimeout("statement_timeout", o.StatementTimeout)
	setTimeout("lock_timeout", o.LockTimeout)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool, now: time.Now}, nil
}

// NewWithPool wraps an existing pool, for tests and embedding.
func NewWithPool(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

// SetClock overrides the store's notion of now. Tests use it to advance past a lease TTL
// without sleeping.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Now returns the store's current time.
func (s *Store) Now() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now().UTC()
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Tx runs fn inside a transaction, rolling back on error or panic.
//
// Every multi-statement invariant in Conductor — claim, reserve, release, reconcile — goes
// through here. Nothing that must be atomic is allowed to be a sequence of Store calls.
func (s *Store) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			// Use a detached context so cleanup still runs if ctx was cancelled mid-flight.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// lockProject takes a transaction-scoped advisory lock keyed on the project.
//
// This is what makes the reservation check-then-insert safe. Without it, two overlapping
// reservations can each observe no conflict and then both insert, because row locks do not
// protect against rows that do not exist yet (DESIGN.md §11, §32.2).
func lockProject(ctx context.Context, tx pgx.Tx, projectID domain.ID) error {
	_, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "conductor/project/"+projectID)
	return err
}

// isUniqueViolation reports whether err is a Postgres unique-constraint violation, optionally
// on a specific constraint.
//
// The claim path relies on this: `one_active_lease_per_task` is the last line of defence
// against two concurrent claims, and hitting it means "someone beat me to it", not "the
// database is broken".
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// noRows normalizes pgx.ErrNoRows into the domain's not-found error.
func noRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}

// nullable renders an empty string as SQL NULL, for optional foreign keys.
func nullable(id domain.ID) any {
	if id == "" {
		return nil
	}
	return id
}

// nullableText renders an empty string as SQL NULL for optional text columns that carry a
// uniqueness constraint (external_ref), where ” and NULL differ.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}
