package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// freshSchemaStore opens a store whose search_path is a new, empty schema, so a test can
// migrate from nothing without touching the shared database the rest of the suite uses.
func freshSchemaStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration tests")
	}
	ctx := context.Background()
	admin := testStore(t)
	schema := fmt.Sprintf("migtest_%d", time.Now().UnixNano())
	if _, err := admin.Pool().Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Pool().Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, err := Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func appliedVersions(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.Pool().Query(context.Background(), `SELECT version FROM schema_migrations ORDER BY applied_at, version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// From an empty database, every migration applies once, in filename order, and a second run
// is a no-op.
func TestMigrateFromEmptyIsIdempotent(t *testing.T) {
	s := freshSchemaStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	known := KnownMigrations()
	if got := appliedVersions(t, s); strings.Join(got, ",") != strings.Join(known, ",") {
		t.Fatalf("applied %v, want %v", got, known)
	}
	// The two legacy 0002 files are distinct versions in a fixed order.
	if i, j := indexOf(known, "0002_budget_grants"), indexOf(known, "0002_session_capabilities"); i < 0 || j < 0 || i > j {
		t.Errorf("0002 ordering = %v", known)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if got := appliedVersions(t, s); len(got) != len(known) {
		t.Errorf("second run applied again: %v", got)
	}
	st, err := s.SchemaStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Current() || st.Applied != known[len(known)-1] {
		t.Errorf("status = %+v", st)
	}
	// The connection Migrate used went back to the pool with the pool's bounds restored.
	var timeout string
	if err := s.Pool().QueryRow(ctx, `SHOW statement_timeout`).Scan(&timeout); err != nil || timeout == "0" {
		t.Errorf("statement_timeout after migrate = %q %v", timeout, err)
	}
}

// A database a newer binary migrated is refused before anything changes.
func TestMigrateRefusesNewerSchema(t *testing.T) {
	s := freshSchemaStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ('9999_from_the_future')`); err != nil {
		t.Fatal(err)
	}
	err := s.Migrate(ctx)
	if !errors.Is(err, ErrSchemaTooNew) || !strings.Contains(err.Error(), "9999_from_the_future") {
		t.Fatalf("migrate against a newer schema = %v, want ErrSchemaTooNew naming the version", err)
	}
	st, err := s.SchemaStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Unknown != 1 || st.Current() {
		t.Errorf("status = %+v", st)
	}
}

func TestOpenAppliesStatementBounds(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration tests")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn, Options{StatementTimeout: 1500 * time.Millisecond, LockTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var st, lt string
	if err := s.Pool().QueryRow(ctx, `SELECT current_setting('statement_timeout'), current_setting('lock_timeout')`).Scan(&st, &lt); err != nil {
		t.Fatal(err)
	}
	if st != "1500ms" || lt != "0" {
		t.Errorf("statement_timeout=%q lock_timeout=%q", st, lt)
	}
	// A statement past the bound fails rather than hanging the caller.
	if _, err := s.Pool().Exec(ctx, `SELECT pg_sleep(5)`); err == nil || !strings.Contains(err.Error(), "statement timeout") {
		t.Errorf("long statement = %v, want a statement timeout", err)
	}
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}
