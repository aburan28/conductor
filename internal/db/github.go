package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

// GitHub App state that every replica must share (migration 0007). The credentials are
// stored as the JSON githubapp.Credentials marshals to; this package does not interpret them.

// GitHubApp returns the stored app credentials; found is false when no app is set up.
func (s *Store) GitHubApp(ctx context.Context) (credentials []byte, found bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT credentials FROM github_app WHERE singleton`).Scan(&credentials)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return credentials, err == nil, err
}

// SaveGitHubApp stores the app's credentials, replacing any app already stored.
func (s *Store) SaveGitHubApp(ctx context.Context, credentials []byte) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO github_app (singleton, credentials, source) VALUES (true, $1, 'setup')
		ON CONFLICT (singleton) DO UPDATE
		   SET credentials = EXCLUDED.credentials, source = 'setup', updated_at = now()`,
		credentials)
	return err
}

// ImportGitHubApp stores credentials only if none are stored yet, and reports whether it did.
// It moves an app set up before credentials lived in the database (a file on one host) into
// it, once: an app stored since, by setup on any replica, is never overwritten by a file.
func (s *Store) ImportGitHubApp(ctx context.Context, credentials []byte) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO github_app (singleton, credentials, source) VALUES (true, $1, 'imported')
		ON CONFLICT (singleton) DO NOTHING`, credentials)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GitHubSetup is a pending manifest-flow setup.
type GitHubSetup struct {
	Org, Name string
	By        domain.ID
	ExpiresAt time.Time
}

// CreateGitHubSetup records a setup under the hash of its state.
func (s *Store) CreateGitHubSetup(ctx context.Context, stateHash string, st GitHubSetup) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO github_setup_states (state_hash, org, name, created_by, expires_at)
		VALUES ($1, $2, $3, $4::uuid, $5)`,
		stateHash, st.Org, st.Name, st.By, st.ExpiresAt)
	return err
}

// GitHubSetupByState returns an unused, unexpired setup. With consume, it is marked used in
// the same statement, so two callbacks racing on two replicas cannot both complete it.
func (s *Store) GitHubSetupByState(ctx context.Context, stateHash string, consume bool) (GitHubSetup, bool, error) {
	q := `SELECT org, name, created_by::text, expires_at FROM github_setup_states
	       WHERE state_hash = $1 AND used_at IS NULL AND expires_at > now()`
	if consume {
		q = `UPDATE github_setup_states SET used_at = now()
		      WHERE state_hash = $1 AND used_at IS NULL AND expires_at > now()
		  RETURNING org, name, created_by::text, expires_at`
	}
	var st GitHubSetup
	err := s.pool.QueryRow(ctx, q, stateHash).Scan(&st.Org, &st.Name, &st.By, &st.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, false, nil
	}
	return st, err == nil, err
}

// CheckRunRecord is the check run Conductor last posted on a commit.
type CheckRunRecord struct {
	ID          int64
	Fingerprint string
}

// GitHubCheckRun returns the record for repository@headSHA; found is false when Conductor
// has posted nothing on that commit.
func (s *Store) GitHubCheckRun(ctx context.Context, repository, headSHA string) (CheckRunRecord, bool, error) {
	var rec CheckRunRecord
	err := s.pool.QueryRow(ctx, `
		SELECT check_run_id, fingerprint FROM github_check_runs
		 WHERE repository = $1 AND head_sha = $2`, repository, headSHA).Scan(&rec.ID, &rec.Fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, false, nil
	}
	return rec, err == nil, err
}

// SaveGitHubCheckRun records what was posted on repository@headSHA.
func (s *Store) SaveGitHubCheckRun(ctx context.Context, repository, headSHA string, rec CheckRunRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO github_check_runs (repository, head_sha, check_run_id, fingerprint)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (repository, head_sha) DO UPDATE
		   SET check_run_id = EXCLUDED.check_run_id, fingerprint = EXCLUDED.fingerprint,
		       updated_at = now()`,
		repository, headSHA, rec.ID, rec.Fingerprint)
	return err
}

// TryExclusive runs fn only if no other process holds the named lock, and reports whether
// it ran. The lock is a session-level advisory lock on a dedicated connection, released when
// fn returns — or by Postgres, if this process dies holding it — so leadership needs no
// lease bookkeeping and fails over on its own.
func (s *Store) TryExclusive(ctx context.Context, name string, fn func(ctx context.Context) error) (bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, name).Scan(&got); err != nil {
		return false, err
	}
	if !got {
		return false, nil
	}
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		if _, err := conn.Exec(cleanup, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, name); err != nil {
			// A connection that may still hold the lock must not go back to the pool, where
			// it would keep every other replica out for as long as it lived.
			_ = conn.Conn().Close(cleanup)
		}
	}()
	return true, fn(ctx)
}
