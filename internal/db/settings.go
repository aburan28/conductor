package db

import (
	"context"
	"errors"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

// Security modes for a control plane (see 0005_server_settings.sql).
const (
	// SecurityLocal lets a client on the same machine sign in as the machine's owner without
	// a token. The right default for a laptop: nobody should have to find a token to open
	// their own dashboard.
	SecurityLocal = "local"
	// SecurityEnhanced requires a token for everything, everywhere, and revokes the tokens
	// local sign-in handed out. The right setting for a shared machine or a server.
	SecurityEnhanced = "enhanced"
)

// LocalTokenPrefix names tokens minted by local sign-in, so switching to enhanced mode can
// revoke exactly those and nothing a person minted on purpose.
const LocalTokenPrefix = "local:"

// ServerSettings is the single settings row. Zero values mean "not set".
type ServerSettings struct {
	LocalOwnerID domain.ID `json:"local_owner_id,omitempty"`
	SecurityMode string    `json:"security_mode,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitzero"`
}

// GetServerSettings returns the settings row, or zero values when none has been written.
func (s *Store) GetServerSettings(ctx context.Context) (ServerSettings, error) {
	var out ServerSettings
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(local_owner_id::text, ''), COALESCE(security_mode, ''), updated_at
		  FROM server_settings WHERE singleton`).Scan(&out.LocalOwnerID, &out.SecurityMode, &out.UpdatedAt)
	if errors.Is(noRows(err), domain.ErrNotFound) {
		return ServerSettings{}, nil
	}
	return out, err
}

// SetLocalOwner records the machine's owner. With onlyIfUnset an existing owner is kept,
// which is what bootstrap wants: the first person to set the machine up owns it, and a
// later bootstrap for a teammate does not quietly take it over. It reports whether the
// owner is now principalID.
func (s *Store) SetLocalOwner(ctx context.Context, principalID domain.ID, onlyIfUnset bool) (bool, error) {
	var owner string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO server_settings (singleton, local_owner_id, updated_at)
		VALUES (true, $1::uuid, now())
		ON CONFLICT (singleton) DO UPDATE
		   SET local_owner_id = CASE
		           WHEN $2 AND server_settings.local_owner_id IS NOT NULL THEN server_settings.local_owner_id
		           ELSE EXCLUDED.local_owner_id END,
		       updated_at = now()
		RETURNING COALESCE(local_owner_id::text, '')`, principalID, onlyIfUnset).Scan(&owner)
	if err != nil {
		return false, err
	}
	return owner == principalID, nil
}

// SetSecurityMode persists the chosen mode. An empty mode clears the choice, returning the
// daemon to its default.
func (s *Store) SetSecurityMode(ctx context.Context, mode string) error {
	if mode != "" && mode != SecurityLocal && mode != SecurityEnhanced {
		return domain.ErrInvalidArgument
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO server_settings (singleton, security_mode, updated_at)
		VALUES (true, NULLIF($1, ''), now())
		ON CONFLICT (singleton) DO UPDATE SET security_mode = NULLIF($1, ''), updated_at = now()`, mode)
	return err
}

// RevokeLocalTokens revokes every token local sign-in minted, for every principal. It is
// what switching to enhanced mode does, so a session opened under the looser mode does not
// outlive the decision to tighten it.
func (s *Store) RevokeLocalTokens(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_tokens SET revoked_at = now()
		 WHERE revoked_at IS NULL AND name LIKE $1`, LocalTokenPrefix+"%")
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
