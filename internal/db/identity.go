package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
)

// ---------------------------------------------------------------------------
// Organizations
// ---------------------------------------------------------------------------

// CreateOrganization registers a tenant and mints its dedupe key. The key never leaves the
// control plane (DESIGN.md §25.6).
func (s *Store) CreateOrganization(ctx context.Context, slug, name string) (domain.Organization, error) {
	key, err := privacy.NewDedupeKey()
	if err != nil {
		return domain.Organization{}, err
	}
	var org domain.Organization
	err = s.pool.QueryRow(ctx, `
		INSERT INTO organizations (slug, name, dedupe_key)
		VALUES ($1, $2, $3)
		RETURNING id::text, slug, name, dedupe_key, created_at`,
		slug, name, key,
	).Scan(&org.ID, &org.Slug, &org.Name, &org.DedupeKey, &org.CreatedAt)
	return org, err
}

func (s *Store) GetOrganization(ctx context.Context, id domain.ID) (domain.Organization, error) {
	var org domain.Organization
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, slug, name, dedupe_key, created_at
		  FROM organizations WHERE id = $1::uuid`, id,
	).Scan(&org.ID, &org.Slug, &org.Name, &org.DedupeKey, &org.CreatedAt)
	return org, noRows(err)
}

func (s *Store) GetOrganizationBySlug(ctx context.Context, slug string) (domain.Organization, error) {
	var org domain.Organization
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, slug, name, dedupe_key, created_at
		  FROM organizations WHERE slug = $1`, slug,
	).Scan(&org.ID, &org.Slug, &org.Name, &org.DedupeKey, &org.CreatedAt)
	return org, noRows(err)
}

// DedupeKeyForProject fetches the tenant key that fingerprints in a project are computed
// under.
func (s *Store) DedupeKeyForProject(ctx context.Context, projectID domain.ID) ([]byte, error) {
	var key []byte
	err := s.pool.QueryRow(ctx, `
		SELECT o.dedupe_key
		  FROM organizations o
		  JOIN projects p ON p.organization_id = o.id
		 WHERE p.id = $1::uuid`, projectID,
	).Scan(&key)
	return key, noRows(err)
}

// ---------------------------------------------------------------------------
// Principals
// ---------------------------------------------------------------------------

func (s *Store) CreatePrincipal(ctx context.Context, orgID domain.ID, kind domain.PrincipalKind, handle, displayName, email string) (domain.Principal, error) {
	if displayName == "" {
		displayName = handle
	}
	var p domain.Principal
	err := s.pool.QueryRow(ctx, `
		INSERT INTO principals (organization_id, kind, handle, display_name, email)
		VALUES ($1::uuid, $2, $3, $4, $5)
		RETURNING id::text, organization_id::text, kind, handle, display_name,
		          COALESCE(email, ''), created_at`,
		orgID, kind, handle, displayName, nullableText(email),
	).Scan(&p.ID, &p.OrganizationID, &p.Kind, &p.Handle, &p.DisplayName, &p.Email, &p.CreatedAt)
	return p, err
}

func (s *Store) GetPrincipal(ctx context.Context, id domain.ID) (domain.Principal, error) {
	var p domain.Principal
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, organization_id::text, kind, handle, display_name,
		       COALESCE(email, ''), created_at
		  FROM principals WHERE id = $1::uuid`, id,
	).Scan(&p.ID, &p.OrganizationID, &p.Kind, &p.Handle, &p.DisplayName, &p.Email, &p.CreatedAt)
	return p, noRows(err)
}

func (s *Store) GetPrincipalByHandle(ctx context.Context, orgID domain.ID, handle string) (domain.Principal, error) {
	var p domain.Principal
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, organization_id::text, kind, handle, display_name,
		       COALESCE(email, ''), created_at
		  FROM principals WHERE organization_id = $1::uuid AND handle = $2`, orgID, handle,
	).Scan(&p.ID, &p.OrganizationID, &p.Kind, &p.Handle, &p.DisplayName, &p.Email, &p.CreatedAt)
	return p, noRows(err)
}

// PrincipalsByID batch-loads principals, which presence and conflict projection need in
// order to render handles without an N+1 query per row.
func (s *Store) PrincipalsByID(ctx context.Context, ids []domain.ID) (map[domain.ID]domain.Principal, error) {
	out := map[domain.ID]domain.Principal{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, organization_id::text, kind, handle, display_name,
		       COALESCE(email, ''), created_at
		  FROM principals WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p domain.Principal
		if err := rows.Scan(&p.ID, &p.OrganizationID, &p.Kind, &p.Handle,
			&p.DisplayName, &p.Email, &p.CreatedAt); err != nil {
			return nil, err
		}
		out[p.ID] = p
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Memberships
// ---------------------------------------------------------------------------

func (s *Store) AddMember(ctx context.Context, projectID, principalID domain.ID, role domain.Role) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO project_memberships (project_id, principal_id, role)
		VALUES ($1::uuid, $2::uuid, $3)
		ON CONFLICT (project_id, principal_id) DO UPDATE SET role = EXCLUDED.role`,
		projectID, principalID, role)
	return err
}

// RoleIn returns the caller's role in a project, or ErrNotFound when they are not a member.
//
// Every project-scoped handler calls this before reading anything. Membership is checked in
// Go rather than assumed from the URL, because DESIGN.md §25.6 requires that no query path
// reaches a row by id alone.
func (s *Store) RoleIn(ctx context.Context, projectID, principalID domain.ID) (domain.Role, error) {
	var role domain.Role
	err := s.pool.QueryRow(ctx, `
		SELECT role FROM project_memberships
		 WHERE project_id = $1::uuid AND principal_id = $2::uuid`,
		projectID, principalID).Scan(&role)
	return role, noRows(err)
}

func (s *Store) ListMembers(ctx context.Context, projectID domain.ID) ([]domain.Membership, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT project_id::text, principal_id::text, role, created_at
		  FROM project_memberships WHERE project_id = $1::uuid
		 ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Membership
	for rows.Next() {
		var m domain.Membership
		if err := rows.Scan(&m.ProjectID, &m.PrincipalID, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// API tokens
// ---------------------------------------------------------------------------

// TokenPrefix marks Conductor bearer tokens so secret scanners can recognize them.
const TokenPrefix = "cdt_"

// hashToken is the only transformation applied to a bearer token before storage. The
// plaintext is returned to the caller once, at mint time, and never persisted or logged
// (DESIGN.md §25.1).
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// CreateToken mints a bearer token for a principal and returns the plaintext exactly once.
func (s *Store) CreateToken(ctx context.Context, principalID domain.ID, name string, ttl time.Duration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	var expires any
	if ttl > 0 {
		expires = s.Now().Add(ttl)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO api_tokens (principal_id, name, token_hash, expires_at)
		VALUES ($1::uuid, $2, $3, $4)`,
		principalID, name, hashToken(token), expires)
	if err != nil {
		return "", err
	}
	return token, nil
}

// AuthenticateToken resolves a bearer token to its principal.
//
// Lookup is by hash, so a database read never exposes a usable credential. Expired and
// revoked tokens are filtered in SQL rather than checked afterwards, which keeps the
// "is this still valid" logic in one place.
func (s *Store) AuthenticateToken(ctx context.Context, token string) (domain.Principal, error) {
	var p domain.Principal
	err := s.pool.QueryRow(ctx, `
		UPDATE api_tokens
		   SET last_used_at = now()
		 WHERE token_hash = $1
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
		RETURNING principal_id::text`, hashToken(token),
	).Scan(&p.ID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return domain.Principal{}, domain.ErrUnauthenticated
		}
		return domain.Principal{}, err
	}
	return s.GetPrincipal(ctx, p.ID)
}

func (s *Store) RevokeToken(ctx context.Context, principalID domain.ID, name string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_tokens SET revoked_at = now()
		 WHERE principal_id = $1::uuid AND name = $2 AND revoked_at IS NULL`,
		principalID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// RevokeAllTokens cuts off a principal entirely. Used when a member is removed from a
// project: dropping the membership alone would leave a valid credential in someone's hands.
func (s *Store) RevokeAllTokens(ctx context.Context, principalID domain.ID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_tokens SET revoked_at = now()
		 WHERE principal_id = $1::uuid AND revoked_at IS NULL`, principalID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// TokenInfo describes a token without disclosing it. There is no field here that could carry
// the secret, because the secret is not stored — only its hash.
type TokenInfo struct {
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// ListTokens returns a principal's tokens, newest first.
func (s *Store) ListTokens(ctx context.Context, principalID domain.ID) ([]TokenInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, created_at, last_used_at, expires_at, revoked_at
		  FROM api_tokens WHERE principal_id = $1::uuid
		 ORDER BY created_at DESC`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.Name, &t.CreatedAt, &t.LastUsedAt,
			&t.ExpiresAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RemoveMember drops a project membership and revokes the principal's tokens in one
// transaction, so access cannot survive the removal.
func (s *Store) RemoveMember(ctx context.Context, projectID, principalID domain.ID) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM project_memberships
			 WHERE project_id = $1::uuid AND principal_id = $2::uuid`, projectID, principalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}

		// Tokens are principal-scoped, not project-scoped, so only revoke them when this was
		// the principal's last membership. Otherwise removing someone from one project would
		// lock them out of every other one.
		var remaining int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM project_memberships WHERE principal_id = $1::uuid`,
			principalID).Scan(&remaining); err != nil {
			return err
		}
		if remaining > 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `
			UPDATE api_tokens SET revoked_at = now()
			 WHERE principal_id = $1::uuid AND revoked_at IS NULL`, principalID)
		return err
	})
}

// CountProjectAdmins reports how many principals can administer a project, so the last one
// cannot remove themselves and strand it.
func (s *Store) CountProjectAdmins(ctx context.Context, projectID domain.ID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM project_memberships
		 WHERE project_id = $1::uuid AND role IN ('project_admin','org_admin')`,
		projectID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// Audit records an administrative or cross-principal read (DESIGN.md §25.6). Detail carries
// identifiers only; it is never given free-form content.
func (s *Store) Audit(ctx context.Context, orgID, projectID, actor domain.ID, action, targetType, targetID string, detail map[string]any) {
	payload, dropped := privacy.SanitizeEventPayload(detail)
	if len(dropped) > 0 {
		payload["dropped_keys"] = len(dropped)
	}
	body, err := marshalJSON(payload)
	if err != nil {
		return
	}
	// Auditing must never fail the operation it is recording; a lost audit line is logged
	// upstream by the caller's error handling, not by breaking the request.
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO audit_log (organization_id, project_id, actor_principal,
		                       action, target_type, target_id, detail)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7)`,
		orgID, nullable(projectID), nullable(actor), action, targetType, targetID, body)
}
