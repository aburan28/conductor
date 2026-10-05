package db

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/domain"
)

// SCIM provisioning state (migration 0013): the tokens an identity provider's SCIM client
// presents, the users it manages, and its groups. The protocol is internal/admin/scim.go
// and internal/api/scim.go.

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

// SCIMTokenPrefix marks SCIM tokens so secret scanners, and people, can tell them from
// ordinary bearer tokens: a SCIM token works on /scim/v2 and nowhere else.
const SCIMTokenPrefix = "cdscim_"

// SCIMToken describes a SCIM token without disclosing it.
type SCIMToken struct {
	ID         domain.ID  `json:"id"`
	Name       string     `json:"name"`
	CreatedBy  string     `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// CreateSCIMToken mints an organization's SCIM token and returns the plaintext once.
func (s *Store) CreateSCIMToken(ctx context.Context, orgID, by domain.ID, name string) (SCIMToken, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return SCIMToken{}, "", err
	}
	token := SCIMTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	var t SCIMToken
	err := s.pool.QueryRow(ctx, `
		INSERT INTO scim_tokens (organization_id, name, token_hash, created_by)
		VALUES ($1::uuid, $2, $3, $4::uuid)
		RETURNING id::text, name, created_at`, orgID, name, hashToken(token), nullable(by),
	).Scan(&t.ID, &t.Name, &t.CreatedAt)
	return t, token, err
}

// ListSCIMTokens lists an organization's SCIM tokens, newest first.
func (s *Store) ListSCIMTokens(ctx context.Context, orgID domain.ID) ([]SCIMToken, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text, t.name, COALESCE(p.handle, ''), t.created_at, t.last_used_at, t.revoked_at
		  FROM scim_tokens t LEFT JOIN principals p ON p.id = t.created_by
		 WHERE t.organization_id = $1::uuid ORDER BY t.created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SCIMToken{}
	for rows.Next() {
		var t SCIMToken
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedBy, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeSCIMToken revokes one of an organization's SCIM tokens.
func (s *Store) RevokeSCIMToken(ctx context.Context, orgID, id domain.ID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE scim_tokens SET revoked_at = now()
		 WHERE id = $1::uuid AND organization_id = $2::uuid AND revoked_at IS NULL`, id, orgID)
	if isBadUUIDErr(err) || (err == nil && tag.RowsAffected() == 0) {
		return domain.ErrNotFound
	}
	return err
}

// AuthenticateSCIMToken resolves a SCIM token to its organization.
func (s *Store) AuthenticateSCIMToken(ctx context.Context, token string) (domain.ID, error) {
	if !strings.HasPrefix(token, SCIMTokenPrefix) {
		return "", domain.ErrUnauthenticated
	}
	var org domain.ID
	err := s.pool.QueryRow(ctx, `
		UPDATE scim_tokens SET last_used_at = now()
		 WHERE token_hash = $1 AND revoked_at IS NULL
		RETURNING organization_id::text`, hashToken(token)).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrUnauthenticated
	}
	return org, err
}

func isBadUUIDErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// SCIMUser is a principal as SCIM sees it. Every human principal of the organization is a
// User — so a provider can find, by userName, the accounts that existed before it was
// connected — and its userName is the one the provider set, else its registered address,
// else its handle.
type SCIMUser struct {
	ID          domain.ID
	Handle      string
	UserName    string
	ExternalID  string
	DisplayName string
	Email       string
	Active      bool
	Managed     bool
	CreatedAt   time.Time
}

const scimUserColumns = `p.id::text, p.handle, COALESCE(p.scim_user_name, p.email, p.handle), COALESCE(p.scim_external_id, ''),
	p.display_name, COALESCE(p.email, ''), p.deactivated_at IS NULL, p.scim_user_name IS NOT NULL, p.created_at`

func scanSCIMUser(row pgx.Row) (SCIMUser, error) {
	var u SCIMUser
	err := row.Scan(&u.ID, &u.Handle, &u.UserName, &u.ExternalID, &u.DisplayName, &u.Email, &u.Active, &u.Managed, &u.CreatedAt)
	return u, err
}

// scimUserScope selects an organization's SCIM users: its humans the provider has not
// deleted.
const scimUserScope = `p.organization_id = $1::uuid AND p.kind = 'human' AND p.scim_deleted_at IS NULL`

// SCIMUserFilter narrows a listing to one attribute value; empty Attribute lists all.
type SCIMUserFilter struct {
	Attribute string // "username", "externalid", "id", "emails.value"
	Value     string
}

// ListSCIMUsers returns a page of users (startIndex is 1-based, as in SCIM) and the total.
func (s *Store) ListSCIMUsers(ctx context.Context, orgID domain.ID, f SCIMUserFilter, startIndex, count int) ([]SCIMUser, int, error) {
	where := scimUserScope
	args := []any{orgID}
	switch f.Attribute {
	case "":
	case "username":
		where += ` AND lower(COALESCE(p.scim_user_name, p.email, p.handle)) = lower($2)`
		args = append(args, f.Value)
	case "externalid":
		where += ` AND p.scim_external_id = $2`
		args = append(args, f.Value)
	case "id":
		where += ` AND p.id::text = $2`
		args = append(args, f.Value)
	case "emails.value":
		where += ` AND lower(p.email) = lower($2)`
		args = append(args, f.Value)
	default:
		return nil, 0, fmt.Errorf("%w: filtering on %s", domain.ErrInvalidArgument, f.Attribute)
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM principals p WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	startIndex = max(startIndex, 1)
	rows, err := s.pool.Query(ctx, `SELECT `+scimUserColumns+` FROM principals p WHERE `+where+
		` ORDER BY p.created_at, p.id LIMIT `+strconv.Itoa(count)+` OFFSET `+strconv.Itoa(startIndex-1), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []SCIMUser{}
	for rows.Next() {
		u, err := scanSCIMUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// GetSCIMUser returns one user of an organization.
func (s *Store) GetSCIMUser(ctx context.Context, orgID, id domain.ID) (SCIMUser, error) {
	u, err := scanSCIMUser(s.pool.QueryRow(ctx, `SELECT `+scimUserColumns+` FROM principals p WHERE `+scimUserScope+
		` AND p.id::text = $2`, orgID, id))
	return u, noRows(err)
}

// SCIMUserParams is what a provider sets on a user.
type SCIMUserParams struct {
	UserName, ExternalID, DisplayName, Email string
	Active                                   bool
	// Project and Role, when set, are the membership a new account starts with. Role is
	// bounded by the caller to contributor, reviewer or observer.
	ProjectID domain.ID
	Role      domain.Role
}

// ErrSCIMUserExists is a create for a userName another account already answers to.
var ErrSCIMUserExists = fmt.Errorf("%w: a user with this userName already exists", domain.ErrDuplicate)

// CreateSCIMUser creates a provider-managed human principal. The handle is the userName's
// local part (or the whole of it), made unique in the organization.
func (s *Store) CreateSCIMUser(ctx context.Context, orgID domain.ID, p SCIMUserParams) (SCIMUser, error) {
	var id domain.ID
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals p WHERE `+scimUserScope+
			` AND lower(COALESCE(p.scim_user_name, p.email, p.handle)) = lower($2))`, orgID, p.UserName).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return ErrSCIMUserExists
		}
		base, _, _ := strings.Cut(p.UserName, "@")
		handle, err := freeHandle(ctx, tx, orgID, SanitizeHandle(base))
		if err != nil {
			return err
		}
		display := p.DisplayName
		if display == "" {
			display = handle
		}
		var deactivated any
		if !p.Active {
			deactivated = time.Now()
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO principals (organization_id, kind, handle, display_name, email, scim_user_name, scim_external_id, deactivated_at)
			VALUES ($1::uuid, 'human', $2, $3, $4, $5, $6, $7)
			RETURNING id::text`, orgID, handle, display, nullableText(p.Email), p.UserName, nullableText(p.ExternalID), deactivated,
		).Scan(&id); err != nil {
			if isUniqueViolation(err, "principals_scim_user_name") {
				return ErrSCIMUserExists
			}
			return err
		}
		if p.ProjectID != "" && p.Role != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO project_memberships (project_id, principal_id, role)
				SELECT $1::uuid, $2::uuid, $3 FROM projects WHERE id = $1::uuid AND organization_id = $4::uuid`,
				p.ProjectID, id, p.Role, orgID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return SCIMUser{}, err
	}
	return s.GetSCIMUser(ctx, orgID, id)
}

// freeHandle returns the first of base, base-2, base-3, … unused in the organization.
func freeHandle(ctx context.Context, tx pgx.Tx, orgID domain.ID, base string) (string, error) {
	for i := 1; i <= 100; i++ {
		candidate := base
		if i > 1 {
			candidate = base + "-" + strconv.Itoa(i)
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals WHERE organization_id = $1::uuid AND handle = $2)`,
			orgID, candidate).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: no free handle near %q", domain.ErrConflict, base)
}

// SCIMUserUpdate changes a user's provider-managed attributes; nil leaves one alone.
// Setting any of them makes the account provider-managed (its userName is recorded).
type SCIMUserUpdate struct {
	UserName, ExternalID, DisplayName, Email *string
}

// UpdateSCIMUser applies an update.
func (s *Store) UpdateSCIMUser(ctx context.Context, orgID, id domain.ID, u SCIMUserUpdate) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE principals p SET
		       scim_user_name   = COALESCE($3, p.scim_user_name, p.email, p.handle),
		       scim_external_id = CASE WHEN $4::text IS NULL THEN p.scim_external_id ELSE NULLIF($4, '') END,
		       display_name     = CASE WHEN COALESCE($5, '') = '' THEN p.display_name ELSE $5 END,
		       email            = CASE WHEN $6::text IS NULL THEN p.email ELSE NULLIF($6, '') END
		 WHERE `+scimUserScope+` AND p.id::text = $2`,
		orgID, id, u.UserName, u.ExternalID, u.DisplayName, u.Email)
	if isUniqueViolation(err, "principals_scim_user_name") {
		return ErrSCIMUserExists
	}
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return err
}

// DeleteSCIMUser is a provider deleting a user. The principal row stays — the audit log,
// tasks and history refer to it — but it is deactivated, its tokens revoked, its
// memberships, linked identities and group memberships removed, and it disappears from
// SCIM. Deleting the last administrator of a project is refused.
func (s *Store) DeleteSCIMUser(ctx context.Context, orgID, id domain.ID) (int64, error) {
	var revoked int64
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE principals p SET deactivated_at = COALESCE(p.deactivated_at, now()), scim_deleted_at = now()
			 WHERE `+scimUserScope+` AND p.id::text = $2`, orgID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		if err := lastAdminCheck(ctx, tx, id); err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE principal_id = $1::uuid AND revoked_at IS NULL`, id)
		if err != nil {
			return err
		}
		revoked = tag.RowsAffected()
		for _, stmt := range []string{
			`DELETE FROM project_memberships WHERE principal_id = $1::uuid`,
			`DELETE FROM external_identities WHERE principal_id = $1::uuid`,
			`DELETE FROM scim_group_members WHERE principal_id = $1::uuid`,
			`DELETE FROM sso_logins WHERE principal_id = $1::uuid OR link_principal = $1::uuid`,
		} {
			if _, err := tx.Exec(ctx, stmt, id); err != nil {
				return err
			}
		}
		return nil
	})
	return revoked, err
}

// ---------------------------------------------------------------------------
// Groups
// ---------------------------------------------------------------------------

// SCIMGroupMember is a member of a group.
type SCIMGroupMember struct {
	ID      domain.ID `json:"value"`
	Display string    `json:"display"`
}

// SCIMGroup is a provider's group.
type SCIMGroup struct {
	ID          domain.ID
	DisplayName string
	ExternalID  string
	Members     []SCIMGroupMember
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (s *Store) scimGroupMembers(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, groupID domain.ID) ([]SCIMGroupMember, error) {
	rows, err := q.Query(ctx, `
		SELECT p.id::text, p.handle FROM scim_group_members m JOIN principals p ON p.id = m.principal_id
		 WHERE m.group_id = $1::uuid ORDER BY p.handle`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SCIMGroupMember{}
	for rows.Next() {
		var m SCIMGroupMember
		if err := rows.Scan(&m.ID, &m.Display); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListSCIMGroups lists an organization's groups, optionally filtered on displayName,
// externalId or id.
func (s *Store) ListSCIMGroups(ctx context.Context, orgID domain.ID, attr, value string, startIndex, count int) ([]SCIMGroup, int, error) {
	where := `organization_id = $1::uuid`
	args := []any{orgID}
	switch attr {
	case "":
	case "displayname":
		where += ` AND lower(display_name) = lower($2)`
		args = append(args, value)
	case "externalid":
		where += ` AND external_id = $2`
		args = append(args, value)
	case "id":
		where += ` AND id::text = $2`
		args = append(args, value)
	default:
		return nil, 0, fmt.Errorf("%w: filtering on %s", domain.ErrInvalidArgument, attr)
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM scim_groups WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text, display_name, COALESCE(external_id, ''), created_at, updated_at
		  FROM scim_groups WHERE `+where+` ORDER BY created_at, id LIMIT `+strconv.Itoa(count)+
		` OFFSET `+strconv.Itoa(max(startIndex, 1)-1), args...)
	if err != nil {
		return nil, 0, err
	}
	var out []SCIMGroup
	for rows.Next() {
		var g SCIMGroup
		if err := rows.Scan(&g.ID, &g.DisplayName, &g.ExternalID, &g.CreatedAt, &g.UpdatedAt); err != nil {
			rows.Close()
			return nil, 0, err
		}
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for i := range out {
		if out[i].Members, err = s.scimGroupMembers(ctx, s.pool, out[i].ID); err != nil {
			return nil, 0, err
		}
	}
	if out == nil {
		out = []SCIMGroup{}
	}
	return out, total, nil
}

// GetSCIMGroup returns one group with its members.
func (s *Store) GetSCIMGroup(ctx context.Context, orgID, id domain.ID) (SCIMGroup, error) {
	var g SCIMGroup
	err := s.pool.QueryRow(ctx, `SELECT id::text, display_name, COALESCE(external_id, ''), created_at, updated_at
		  FROM scim_groups WHERE organization_id = $1::uuid AND id::text = $2`, orgID, id,
	).Scan(&g.ID, &g.DisplayName, &g.ExternalID, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return g, noRows(err)
	}
	g.Members, err = s.scimGroupMembers(ctx, s.pool, g.ID)
	return g, err
}

// ErrSCIMGroupExists is a group name already taken in the organization.
var ErrSCIMGroupExists = fmt.Errorf("%w: a group with this displayName already exists", domain.ErrDuplicate)

// SCIMGroupChange is a change to a group; nil fields are left alone.
type SCIMGroupChange struct {
	DisplayName, ExternalID *string
	Add, Remove             []domain.ID
	// Replace, when set, is the whole member list.
	Replace *[]domain.ID
}

// CreateSCIMGroup creates a group with its members.
func (s *Store) CreateSCIMGroup(ctx context.Context, orgID domain.ID, name, externalID string, members []domain.ID) (SCIMGroup, error) {
	var id domain.ID
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO scim_groups (organization_id, display_name, external_id)
			VALUES ($1::uuid, $2, $3) RETURNING id::text`, orgID, name, nullableText(externalID)).Scan(&id)
		if isUniqueViolation(err, "scim_groups_name") {
			return ErrSCIMGroupExists
		}
		if err != nil {
			return err
		}
		return addGroupMembers(ctx, tx, orgID, id, members)
	})
	if err != nil {
		return SCIMGroup{}, err
	}
	return s.GetSCIMGroup(ctx, orgID, id)
}

// ChangeSCIMGroup applies a change in one transaction.
func (s *Store) ChangeSCIMGroup(ctx context.Context, orgID, id domain.ID, c SCIMGroupChange) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE scim_groups SET display_name = COALESCE($3, display_name),
			       external_id = CASE WHEN $4::text IS NULL THEN external_id ELSE NULLIF($4, '') END,
			       updated_at = now()
			 WHERE organization_id = $1::uuid AND id::text = $2`, orgID, id, c.DisplayName, c.ExternalID)
		if isUniqueViolation(err, "scim_groups_name") {
			return ErrSCIMGroupExists
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		if c.Replace != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM scim_group_members WHERE group_id = $1::uuid`, id); err != nil {
				return err
			}
			if err := addGroupMembers(ctx, tx, orgID, id, *c.Replace); err != nil {
				return err
			}
		}
		if err := addGroupMembers(ctx, tx, orgID, id, c.Add); err != nil {
			return err
		}
		if len(c.Remove) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM scim_group_members WHERE group_id = $1::uuid AND principal_id::text = ANY($2)`,
				id, idStrings(c.Remove)); err != nil {
				return err
			}
		}
		return nil
	})
}

// addGroupMembers adds principals of the organization to a group; ids that name no such
// principal are ignored, as a provider may push a member Conductor never provisioned.
func addGroupMembers(ctx context.Context, tx pgx.Tx, orgID, groupID domain.ID, ids []domain.ID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO scim_group_members (group_id, principal_id)
		SELECT $1::uuid, p.id FROM principals p
		 WHERE p.organization_id = $2::uuid AND p.id::text = ANY($3) AND p.scim_deleted_at IS NULL
		ON CONFLICT DO NOTHING`, groupID, orgID, idStrings(ids))
	return err
}

func idStrings(ids []domain.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

// DeleteSCIMGroup deletes a group and its memberships.
func (s *Store) DeleteSCIMGroup(ctx context.Context, orgID, id domain.ID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM scim_groups WHERE organization_id = $1::uuid AND id::text = $2`, orgID, id)
	if err == nil && tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return err
}

// GroupsOf returns the names of the SCIM groups a principal belongs to.
func (s *Store) GroupsOf(ctx context.Context, principalID domain.ID) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.display_name FROM scim_group_members m JOIN scim_groups g ON g.id = m.group_id
		 WHERE m.principal_id = $1::uuid ORDER BY g.display_name`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
