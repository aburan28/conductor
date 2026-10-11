package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrations are forward-only SQL files under migrations/, applied in byte-wise filename
// order and recorded in schema_migrations by filename without the extension.
//
// Numbering is strict from 0003 on: every new file takes the next unused four-digit prefix.
// The two 0002 files predate that rule. They cannot be renamed — the name is the version a
// deployed database has recorded — and they do not need to be: the version is the whole
// name, so they are distinct, and sort.Strings orders "0002_budget_grants" before
// "0002_session_capabilities" on every machine. Neither depends on the other.

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLock serializes Migrate across processes.
const migrationLock = 8712340001

// ErrSchemaTooNew means the database carries migrations this binary does not know: a newer
// conductord migrated it. Running an older binary against it would read and write a schema
// it was not written for, so startup refuses instead.
var ErrSchemaTooNew = errors.New("database schema is newer than this binary")

// KnownMigrations lists the versions embedded in this binary, in the order they apply.
func KnownMigrations() []string {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		// The directory is embedded at build time; failing to read it is a build defect.
		panic(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			out = append(out, strings.TrimSuffix(e.Name(), ".sql"))
		}
	}
	sort.Strings(out)
	return out
}

// Migrate applies every embedded migration that has not been applied yet, in filename order.
//
// It takes a session-level advisory lock first, so two conductord processes starting at the
// same moment cannot both try to create the same table. It refuses, before changing
// anything, a database that already has a migration this binary does not know.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	// A migration may legitimately run for longer than the pool's per-statement bound (an
	// index build on a large table), and waiting on the migration lock is the point of the
	// lock. Lift both bounds on this connection only, and put them back before it returns
	// to the pool: RESET restores the connection's startup value.
	if _, err := conn.Exec(ctx, `SET statement_timeout = 0; SET lock_timeout = 0`); err != nil {
		return fmt.Errorf("lift timeouts for migration: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `RESET statement_timeout; RESET lock_timeout`)
	}()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock)
	}()

	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	known := KnownMigrations()
	if err := checkNotNewer(applied, known); err != nil {
		return err
	}

	for _, version := range known {
		if applied[version] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + version + ".sql")
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
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}
	return nil
}

// appliedMigrations reads schema_migrations; a database with no such table has applied none.
func appliedMigrations(ctx context.Context, conn *pgxpool.Conn) (map[string]bool, error) {
	applied := map[string]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		// The very first run has no schema_migrations table yet; any other error is real.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42P01" {
			return nil, fmt.Errorf("read schema_migrations: %w", err)
		}
		return applied, nil
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// checkNotNewer fails when applied holds a version known does not.
func checkNotNewer(applied map[string]bool, known []string) error {
	knownSet := make(map[string]bool, len(known))
	for _, v := range known {
		knownSet[v] = true
	}
	var unknown []string
	for v := range applied {
		if !knownSet[v] {
			unknown = append(unknown, v)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	latest := ""
	if len(known) > 0 {
		latest = known[len(known)-1]
	}
	return fmt.Errorf("%w: it has migration(s) %s, and this binary knows migrations only up to %s. "+
		"A newer conductord has run against this database; run that version (or newer) instead. "+
		"Migrations are forward-only, so an older binary cannot be pointed at a newer schema "+
		"(docs/OPERATIONS.md, \"Upgrades and rollback\")",
		ErrSchemaTooNew, strings.Join(unknown, ", "), latest)
}

// SchemaStatus describes the database schema against this binary.
type SchemaStatus struct {
	// Applied is the newest version recorded in the database ("" when none).
	Applied string `json:"applied"`
	// Known is the newest version this binary embeds.
	Known string `json:"known"`
	// Pending counts known migrations not yet applied; Unknown counts applied migrations
	// this binary does not know. Both are zero on a healthy server.
	Pending int `json:"pending"`
	Unknown int `json:"unknown"`
}

// Current reports whether the schema is exactly what this binary expects.
func (s SchemaStatus) Current() bool { return s.Pending == 0 && s.Unknown == 0 }

// SchemaStatus compares the database's applied migrations with this binary's.
func (s *Store) SchemaStatus(ctx context.Context) (SchemaStatus, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return SchemaStatus{}, err
	}
	defer conn.Release()
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return SchemaStatus{}, err
	}
	known := KnownMigrations()
	st := SchemaStatus{}
	if len(known) > 0 {
		st.Known = known[len(known)-1]
	}
	knownSet := map[string]bool{}
	for _, v := range known {
		knownSet[v] = true
		if !applied[v] {
			st.Pending++
		}
	}
	for v := range applied {
		if !knownSet[v] {
			st.Unknown++
		}
		if v > st.Applied {
			st.Applied = v
		}
	}
	return st, nil
}
