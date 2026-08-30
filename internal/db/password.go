package db

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

// Password credentials for dashboard sign-in.
//
// A password is a second credential type alongside bearer tokens, and it is the one humans
// can remember. It is stored as a PBKDF2 string with its parameters inline —
// `pbkdf2-sha256$<iterations>$<salt>$<hash>` — so the cost factor can be raised later
// without a schema change and every stored hash names its own scheme. The plaintext exists
// only inside the hashing call, never in a log or a query (DESIGN.md §25.1, same rule as
// tokens).
//
// Null means "no password": service identities should never have one, and a human keeps
// working on tokens until someone sets a password.

const (
	// passwordIterations is the PBKDF2-SHA256 cost. OWASP's current guidance for this
	// primitive is 600,000 rounds; at that cost a wrong guess is expensive but a correct
	// sign-in still feels instant.
	passwordIterations = 600000
	// passwordSaltLen and passwordKeyLen in bytes.
	passwordSaltLen = 16
	passwordKeyLen  = 32
	// PasswordMinLength is the floor enforced at every entry point. Longer is always
	// better; this only excludes passwords that are pure noise even for a throwaway.
	PasswordMinLength = 8
)

var errPasswordFormat = errors.New("stored password hash has an unrecognized format")

// HashPassword derives the stored form of a password. Exported for the CLI bootstrap path,
// which has the plaintext and needs the same encoding the store verifies against.
func HashPassword(password string) (string, error) {
	if len(password) < PasswordMinLength {
		return "", fmt.Errorf("%w: password must be at least %d characters", domain.ErrInvalidArgument, PasswordMinLength)
	}
	salt := make([]byte, passwordSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		passwordIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// verifyPassword checks a plaintext against a stored hash. Unknown formats fail closed.
func verifyPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// SetPassword stores (or replaces) a principal's password. Clearing is not offered here on
// purpose: revocation is what RevokeAllTokens is for, and an admin who wants someone locked
// out should not have to remember whether that person also had a password.
func (s *Store) SetPassword(ctx context.Context, principalID domain.ID, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE principals SET password_hash = $2 WHERE id = $1::uuid`,
		principalID, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// AuthenticatePassword resolves a handle within one organization to its principal, if the
// password matches. Unknown user, unknown org, missing password, and wrong password are all
// the same error, so a probe learns nothing about which part failed.
func (s *Store) AuthenticatePassword(ctx context.Context, orgID domain.ID, handle, password string) (domain.Principal, error) {
	var stored *string
	var p domain.Principal
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, organization_id::text, kind, handle, display_name,
		       COALESCE(email, ''), created_at, password_hash
		  FROM principals
		 WHERE organization_id = $1::uuid AND handle = $2`, orgID, handle,
	).Scan(&p.ID, &p.OrganizationID, &p.Kind, &p.Handle, &p.DisplayName, &p.Email, &p.CreatedAt, &stored)
	if err != nil {
		if err == pgx.ErrNoRows {
			return domain.Principal{}, domain.ErrUnauthenticated
		}
		return domain.Principal{}, err
	}
	if stored == nil || !verifyPassword(*stored, password) {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	return p, nil
}

// PrincipalsByHandle returns every principal with a handle across all organizations. Handles
// are unique per org, not globally, so an org-less sign-in is only unambiguous when this
// returns one row; the API layer turns "more than one" into a request for an org name.
func (s *Store) PrincipalsByHandle(ctx context.Context, handle string) ([]domain.Principal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, organization_id::text, kind, handle, display_name,
		       COALESCE(email, ''), created_at
		  FROM principals WHERE handle = $1`, handle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Principal
	for rows.Next() {
		var p domain.Principal
		if err := rows.Scan(&p.ID, &p.OrganizationID, &p.Kind, &p.Handle,
			&p.DisplayName, &p.Email, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
