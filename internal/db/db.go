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
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/adamburan/conductor/internal/domain"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store is the handle every service takes. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	// now is injectable so time-dependent behaviour (lease expiry, stall detection) can be
	// driven deterministically in tests instead of with sleeps.
	now func() time.Time
}

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	// The claim path takes short row locks and advisory locks; a starved pool turns those
	// into queueing rather than errors, so keep some headroom.
	if cfg.MaxConns < 8 {
		cfg.MaxConns = 8
	}
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

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

// Migrate applies every embedded migration that has not been applied yet, in filename order.
//
// It takes a session-level advisory lock first, so two conductord processes starting at the
// same moment cannot both try to create the same table.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	const migrationLock = 8712340001
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock)
	}()

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	applied := map[string]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		// The very first run has no schema_migrations table yet; any other error is real.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42P01" {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
	} else {
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			applied[v] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		// Each migration runs in its own transaction: a failure leaves the database at the
		// last complete version rather than half-migrated.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
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
