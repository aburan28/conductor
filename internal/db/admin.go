package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adamburan/conductor/internal/admin"
	"github.com/adamburan/conductor/internal/domain"
)

// Organization-level administration (migration 0013): the policy document, the logo, who
// administers an organization, and deactivated accounts. The rules themselves live in
// internal/admin; this file stores them and keeps the multi-row changes atomic.

// OrgPolicy is a stored policy and when it last changed.
type OrgPolicy struct {
	Policy    admin.Policy `json:"policy"`
	UpdatedAt *time.Time   `json:"updated_at,omitempty"`
	UpdatedBy domain.ID    `json:"updated_by,omitempty"`
}

// GetOrgPolicy returns an organization's stored policy, normalized; the defaults when none
// has been stored.
func (s *Store) GetOrgPolicy(ctx context.Context, orgID domain.ID) (OrgPolicy, error) {
	var raw []byte
	var out OrgPolicy
	var by *string
	err := s.pool.QueryRow(ctx, `
		SELECT policy, updated_at, updated_by::text FROM org_policies WHERE organization_id = $1::uuid`, orgID,
	).Scan(&raw, &out.UpdatedAt, &by)
	out.Policy = admin.Defaults()
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if by != nil {
		out.UpdatedBy = domain.ID(*by)
	}
	if err := json.Unmarshal(raw, &out.Policy); err != nil {
		// A document this binary cannot read is a defect, not a reason to lock everyone out
		// or to silently apply defaults: say which organization it is.
		return out, fmt.Errorf("organization %s policy: %w", orgID, err)
	}
	out.Policy.Normalize()
	return out, nil
}

// PutOrgPolicy stores an organization's policy.
func (s *Store) PutOrgPolicy(ctx context.Context, orgID domain.ID, p admin.Policy, by domain.ID) error {
	p.Normalize()
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO org_policies (organization_id, policy, updated_at, updated_by)
		VALUES ($1::uuid, $2, now(), $3::uuid)
		ON CONFLICT (organization_id) DO UPDATE
		   SET policy = EXCLUDED.policy, updated_at = now(), updated_by = EXCLUDED.updated_by`,
		orgID, body, nullable(by))
	return err
}

// GetOrgLogo returns an organization's logo; found is false when it has none.
func (s *Store) GetOrgLogo(ctx context.Context, orgID domain.ID) (admin.Logo, bool, error) {
	var l admin.Logo
	err := s.pool.QueryRow(ctx, `SELECT content_type, data, sha256 FROM org_logos WHERE organization_id = $1::uuid`,
		orgID).Scan(&l.ContentType, &l.Data, &l.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, false, nil
	}
	return l, err == nil, err
}

// PutOrgLogo stores a logo admin.CheckLogo accepted.
func (s *Store) PutOrgLogo(ctx context.Context, orgID domain.ID, l admin.Logo) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO org_logos (organization_id, content_type, data, sha256, updated_at)
		VALUES ($1::uuid, $2, $3, $4, now())
		ON CONFLICT (organization_id) DO UPDATE
		   SET content_type = EXCLUDED.content_type, data = EXCLUDED.data, sha256 = EXCLUDED.sha256, updated_at = now()`,
		orgID, l.ContentType, l.Data, l.SHA256)
	return err
}

// DeleteOrgLogo removes an organization's logo.
func (s *Store) DeleteOrgLogo(ctx context.Context, orgID domain.ID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM org_logos WHERE organization_id = $1::uuid`, orgID)
	return err
}

// PrimaryOrganization is the organization a sign-in page brands itself for before anyone
// has signed in: the machine owner's, or the only one there is. found is false on a server
// hosting several organizations with no owner recorded.
func (s *Store) PrimaryOrganization(ctx context.Context) (domain.Organization, bool, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(
		         (SELECT p.organization_id::text FROM server_settings ss
		            JOIN principals p ON p.id = ss.local_owner_id WHERE ss.singleton),
		         (SELECT min(id::text) FROM organizations HAVING count(*) = 1),
		         '')`).Scan(&id)
	if err != nil || id == "" {
		return domain.Organization{}, false, err
	}
	org, err := s.GetOrganization(ctx, domain.ID(id))
	return org, err == nil, err
}

// IsOrgAdmin reports whether a principal holds org_admin in any project of its
// organization. Roles are per project in this model; an org_admin membership anywhere in
// the organization is what makes someone its administrator.
func (s *Store) IsOrgAdmin(ctx context.Context, principalID, orgID domain.ID) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM project_memberships m
		    JOIN projects pr ON pr.id = m.project_id
		    JOIN principals p ON p.id = m.principal_id
		   WHERE m.principal_id = $1::uuid AND pr.organization_id = $2::uuid
		     AND m.role = 'org_admin' AND p.deactivated_at IS NULL)`, principalID, orgID).Scan(&ok)
	return ok, err
}

// OrgMembership is one project membership of an organization member.
type OrgMembership struct {
	Project   string      `json:"project"`
	ProjectID domain.ID   `json:"project_id"`
	Role      domain.Role `json:"role"`
}

// OrgMember is a principal of an organization with everything the admin area shows.
type OrgMember struct {
	ID            domain.ID            `json:"id"`
	Handle        string               `json:"handle"`
	DisplayName   string               `json:"display_name"`
	Kind          domain.PrincipalKind `json:"kind"`
	Email         string               `json:"email,omitempty"`
	DeactivatedAt *time.Time           `json:"deactivated_at,omitempty"`
	SCIMManaged   bool                 `json:"scim_managed"`
	CreatedAt     time.Time            `json:"created_at"`
	LastSeenAt    *time.Time           `json:"last_seen_at,omitempty"`
	Memberships   []OrgMembership      `json:"memberships"`
	Identities    []ExternalIdentity   `json:"identities"`
}

// ListOrgMembers returns every principal of an organization that is not a per-attempt
// identity, with its memberships and linked identities.
func (s *Store) ListOrgMembers(ctx context.Context, orgID domain.ID) ([]OrgMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id::text, p.handle, p.display_name, p.kind, COALESCE(p.email, ''), p.deactivated_at,
		       p.scim_user_name IS NOT NULL AND p.scim_deleted_at IS NULL, p.created_at,
		       (SELECT max(t.last_used_at) FROM api_tokens t WHERE t.principal_id = p.id),
		       COALESCE((SELECT json_agg(json_build_object('project', pr.slug, 'project_id', pr.id, 'role', m.role) ORDER BY pr.slug)
		                   FROM project_memberships m JOIN projects pr ON pr.id = m.project_id
		                  WHERE m.principal_id = p.id), '[]')
		  FROM principals p
		 WHERE p.organization_id = $1::uuid AND p.kind NOT IN ('agent_attempt', 'interactive_session')
		 ORDER BY p.deactivated_at IS NOT NULL, lower(p.handle)`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrgMember{}
	for rows.Next() {
		var m OrgMember
		var memberships []byte
		if err := rows.Scan(&m.ID, &m.Handle, &m.DisplayName, &m.Kind, &m.Email, &m.DeactivatedAt,
			&m.SCIMManaged, &m.CreatedAt, &m.LastSeenAt, &memberships); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(memberships, &m.Memberships); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Identities, err = s.ListIdentities(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PrincipalDeactivated reports whether a principal is deactivated.
func (s *Store) PrincipalDeactivated(ctx context.Context, principalID domain.ID) (bool, error) {
	var at *time.Time
	err := s.pool.QueryRow(ctx, `SELECT deactivated_at FROM principals WHERE id = $1::uuid`, principalID).Scan(&at)
	return at != nil, noRows(err)
}

// ErrLastAdmin refuses a change that would leave a project with no administrator.
var ErrLastAdmin = fmt.Errorf("%w: this is the last administrator of a project; promote someone else first", domain.ErrNotPermitted)

// SetPrincipalActive deactivates or reactivates a principal. Deactivating revokes every
// token it holds and every sign-in it has in flight, in one transaction, so access ends
// with the statement rather than with the next token expiry; its memberships stay, inert,
// so reactivating restores them, and its audit history is untouched. Deactivating the last
// administrator of any project is refused. It reports how many tokens were revoked.
func (s *Store) SetPrincipalActive(ctx context.Context, orgID, principalID domain.ID, active bool) (int64, error) {
	var revoked int64
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if active {
			tag, err := tx.Exec(ctx, `UPDATE principals SET deactivated_at = NULL
				 WHERE id = $1::uuid AND organization_id = $2::uuid`, principalID, orgID)
			if err == nil && tag.RowsAffected() == 0 {
				err = domain.ErrNotFound
			}
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE principals SET deactivated_at = COALESCE(deactivated_at, now())
			 WHERE id = $1::uuid AND organization_id = $2::uuid`, principalID, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		if err := lastAdminCheck(ctx, tx, principalID); err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = now()
			 WHERE principal_id = $1::uuid AND revoked_at IS NULL`, principalID)
		if err != nil {
			return err
		}
		revoked = tag.RowsAffected()
		_, err = tx.Exec(ctx, `DELETE FROM sso_logins WHERE principal_id = $1::uuid OR link_principal = $1::uuid`, principalID)
		return err
	})
	return revoked, err
}

// lastAdminCheck refuses when principalID is the only active administrator of some project
// it administers. It locks those projects' memberships first, so two deactivations racing
// on two administrators cannot both pass.
func lastAdminCheck(ctx context.Context, tx pgx.Tx, principalID domain.ID) error {
	if _, err := tx.Exec(ctx, `
		SELECT 1 FROM project_memberships
		 WHERE project_id IN (SELECT project_id FROM project_memberships WHERE principal_id = $1::uuid)
		   FOR UPDATE`, principalID); err != nil {
		return err
	}
	var stranded bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM project_memberships m
		   WHERE m.principal_id = $1::uuid AND m.role IN ('project_admin', 'org_admin')
		     AND NOT EXISTS (
		       SELECT 1 FROM project_memberships o JOIN principals op ON op.id = o.principal_id
		        WHERE o.project_id = m.project_id AND o.principal_id <> $1::uuid
		          AND o.role IN ('project_admin', 'org_admin') AND op.deactivated_at IS NULL))`,
		principalID).Scan(&stranded)
	if err != nil {
		return err
	}
	if stranded {
		return ErrLastAdmin
	}
	return nil
}

// OrgsAutoProvisioning lists the organizations whose stored policy turns auto-provisioning
// on. The policy is applied (and checked) by the caller; this only narrows the search.
func (s *Store) OrgsAutoProvisioning(ctx context.Context) ([]domain.ID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT organization_id::text FROM org_policies WHERE (policy->>'auto_provision')::boolean`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListOrganizationIDs lists every organization, for policies a config file applies to all.
func (s *Store) ListOrganizationIDs(ctx context.Context) ([]domain.ID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM organizations ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ID
	for rows.Next() {
		var id domain.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
