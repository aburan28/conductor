package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/adamburan/conductor/internal/domain"
)

// Single sign-on state (migration 0012): external identities linked to principals, and the
// sign-ins in flight. The protocol lives in internal/sso and the policy in internal/api; this
// file only keeps the invariants the policy relies on atomic.

// ExternalIdentity is an account at an identity provider linked to a principal.
type ExternalIdentity struct {
	PrincipalID domain.ID  `json:"principal_id"`
	Provider    string     `json:"provider"`
	Issuer      string     `json:"issuer"`
	Subject     string     `json:"subject"`
	Email       string     `json:"email,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// ExternalAccount names the account a provider vouched for, with the address it verified.
type ExternalAccount struct {
	Provider, Issuer, Subject, Email string
}

const identityColumns = `principal_id::text, provider, issuer, subject, email, created_at, last_login_at`

func scanIdentity(row pgx.Row) (ExternalIdentity, error) {
	var id ExternalIdentity
	err := row.Scan(&id.PrincipalID, &id.Provider, &id.Issuer, &id.Subject, &id.Email, &id.CreatedAt, &id.LastLoginAt)
	return id, err
}

// IdentityByAccount returns the identity linked to an external account; found is false when
// the account is linked to no one.
func (s *Store) IdentityByAccount(ctx context.Context, issuer, subject string) (ExternalIdentity, bool, error) {
	id, err := scanIdentity(s.pool.QueryRow(ctx,
		`SELECT `+identityColumns+` FROM external_identities WHERE issuer = $1 AND subject = $2`, issuer, subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return id, false, nil
	}
	return id, err == nil, err
}

// TouchIdentity records a sign-in through a linked identity, with the address the provider
// verified this time.
func (s *Store) TouchIdentity(ctx context.Context, issuer, subject, email string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE external_identities SET last_login_at = now(), email = $3
		 WHERE issuer = $1 AND subject = $2`, issuer, subject, email)
	return err
}

// ListIdentities returns a principal's linked identities, oldest first.
func (s *Store) ListIdentities(ctx context.Context, principalID domain.ID) ([]ExternalIdentity, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+identityColumns+` FROM external_identities WHERE principal_id = $1::uuid ORDER BY created_at`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExternalIdentity{}
	for rows.Next() {
		id, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ErrIdentityTaken means the external account is already linked to a principal.
var ErrIdentityTaken = fmt.Errorf("%w: this account is already linked to a principal", domain.ErrDuplicate)

// ErrIssuerTaken means the principal already has an account at this issuer linked.
var ErrIssuerTaken = fmt.Errorf("%w: an account at this provider is already linked", domain.ErrDuplicate)

// ErrAlreadyLinked means a first-sign-in link was refused because the principal already
// signs in through some external identity.
var ErrAlreadyLinked = fmt.Errorf("%w: the principal already has a linked identity", domain.ErrDuplicate)

// LinkIdentity links an external account to a principal. With onlyFirst, it links only when
// the principal has no linked identity at all, decided in the same statement as the insert,
// so two first sign-ins racing on two replicas cannot both attach an account.
func (s *Store) LinkIdentity(ctx context.Context, principalID domain.ID, acct ExternalAccount, onlyFirst bool) error {
	return s.linkIdentity(ctx, s.pool, principalID, acct, onlyFirst)
}

// execer is a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (s *Store) linkIdentity(ctx context.Context, q execer, principalID domain.ID, acct ExternalAccount, onlyFirst bool) error {
	tag, err := q.Exec(ctx, `
		INSERT INTO external_identities (principal_id, provider, issuer, subject, email, last_login_at)
		SELECT $1::uuid, $2, $3, $4, $5, now()
		 WHERE NOT $6 OR NOT EXISTS (SELECT 1 FROM external_identities WHERE principal_id = $1::uuid)`,
		principalID, acct.Provider, acct.Issuer, acct.Subject, acct.Email, onlyFirst)
	switch {
	case isUniqueViolation(err, "external_identities_account"):
		return ErrIdentityTaken
	case isUniqueViolation(err, "external_identities_one_per_issuer"):
		return ErrIssuerTaken
	case err != nil:
		return err
	case tag.RowsAffected() == 0:
		return ErrAlreadyLinked
	}
	return nil
}

// UnlinkIdentity removes a principal's identity from one provider and revokes the tokens
// signing in through it issued, in one transaction: an unlinked account must not keep a live
// session. It returns ErrNotFound when nothing was linked.
func (s *Store) UnlinkIdentity(ctx context.Context, principalID domain.ID, provider string) (int64, error) {
	var revoked int64
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM external_identities WHERE principal_id = $1::uuid AND provider = $2`,
			principalID, provider)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		tag, err = tx.Exec(ctx, `
			UPDATE api_tokens SET revoked_at = now()
			 WHERE principal_id = $1::uuid AND (name = $2 OR starts_with(name, $2 || '/')) AND revoked_at IS NULL`,
			principalID, SSOTokenPrefix+provider)
		revoked = tag.RowsAffected()
		return err
	})
	return revoked, err
}

// SSOTokenPrefix names every token a single sign-on issues: sso:<provider>. A token minted
// with one of those (a runner's per-attempt token, a second machine's) is named
// sso:<provider>/<name>: it belongs to the same sign-in, is capped at its expiry, and is
// revoked with it.
const SSOTokenPrefix = "sso:"

// HumansByEmail returns the human principals an administrator registered this address on,
// compared case-insensitively, across every organization.
func (s *Store) HumansByEmail(ctx context.Context, email string) ([]domain.Principal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, organization_id::text, kind, handle, display_name,
		       COALESCE(email, ''), created_at
		  FROM principals
		 WHERE email IS NOT NULL AND lower(email) = lower($1) AND kind = 'human'
		 ORDER BY created_at`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Principal
	for rows.Next() {
		var p domain.Principal
		if err := rows.Scan(&p.ID, &p.OrganizationID, &p.Kind, &p.Handle, &p.DisplayName, &p.Email, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPrincipalEmail sets (or, with "", clears) the address single sign-on links by.
func (s *Store) SetPrincipalEmail(ctx context.Context, principalID domain.ID, email string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE principals SET email = $2 WHERE id = $1::uuid`,
		principalID, nullableText(email))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ProvisionSSOPrincipal creates a human principal for a first sign-in, makes it a member of
// one project, and links the external account, in one transaction. The handle is the first
// free one of base, base-2, base-3, … in the project's organization.
func (s *Store) ProvisionSSOPrincipal(ctx context.Context, project domain.Project, base, displayName string,
	role domain.Role, acct ExternalAccount) (domain.Principal, error) {
	var p domain.Principal
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		handle := ""
		for i := 1; i <= 100 && handle == ""; i++ {
			candidate := base
			if i > 1 {
				candidate = base + "-" + strconv.Itoa(i)
			}
			var taken bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM principals WHERE organization_id = $1::uuid AND handle = $2)`,
				project.OrganizationID, candidate).Scan(&taken); err != nil {
				return err
			}
			if !taken {
				handle = candidate
			}
		}
		if handle == "" {
			return fmt.Errorf("%w: no free handle near %q", domain.ErrConflict, base)
		}
		if displayName == "" {
			displayName = handle
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO principals (organization_id, kind, handle, display_name, email)
			VALUES ($1::uuid, 'human', $2, $3, $4)
			RETURNING id::text, organization_id::text, kind, handle, display_name, COALESCE(email, ''), created_at`,
			project.OrganizationID, handle, displayName, nullableText(acct.Email),
		).Scan(&p.ID, &p.OrganizationID, &p.Kind, &p.Handle, &p.DisplayName, &p.Email, &p.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_memberships (project_id, principal_id, role) VALUES ($1::uuid, $2::uuid, $3)`,
			project.ID, p.ID, role); err != nil {
			return err
		}
		return s.linkIdentity(ctx, tx, p.ID, acct, false)
	})
	return p, err
}

// SanitizeHandle turns an email local part or a provider username into a handle: lowercase
// letters, digits, dots, dashes and underscores, at most 32 characters.
func SanitizeHandle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == '+':
			// Plus-addressing is a mailbox detail, not part of the name.
			return finishHandle(b.String())
		}
		if b.Len() >= 32 {
			break
		}
	}
	return finishHandle(b.String())
}

func finishHandle(h string) string {
	h = strings.Trim(h, ".-_")
	if h == "" {
		return "user"
	}
	return h
}

// ---------------------------------------------------------------------------
// Sign-ins in flight
// ---------------------------------------------------------------------------

// SSOLogin is a sign-in in flight.
type SSOLogin struct {
	StateHash       string
	Provider        string
	Client          string // "dashboard" or "cli"
	Nonce           string
	Verifier        string
	BinderHash      string
	ClientChallenge string
	ReturnTo        string
	LinkPrincipal   domain.ID
	ExpiresAt       time.Time
	// Set once the callback has issued a ticket.
	PrincipalID domain.ID
}

// maxSSOLoginsInFlight bounds unfinished sign-ins. Starting one needs no credential, so
// without a bound anyone could fill this table; past it, starting a sign-in answers "busy"
// until the rows expire.
const maxSSOLoginsInFlight = 10000

// CreateSSOLogin records a sign-in under the hash of its state. Expired rows are deleted on
// the way, so the table holds only what is in flight.
func (s *Store) CreateSSOLogin(ctx context.Context, l SSOLogin) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM sso_logins WHERE expires_at < now() - interval '1 hour'
		    AND (ticket_expires_at IS NULL OR ticket_expires_at < now() - interval '1 hour')`); err != nil {
		return err
	}
	var inFlight int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sso_logins WHERE expires_at > now()`).Scan(&inFlight); err != nil {
		return err
	}
	if inFlight >= maxSSOLoginsInFlight {
		return fmt.Errorf("%w: too many sign-ins in progress; try again in a few minutes", domain.ErrCapacity)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sso_logins (state_hash, provider, client, nonce, verifier, binder_hash,
		                        client_challenge, return_to, link_principal, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::uuid, $10)`,
		l.StateHash, l.Provider, l.Client, l.Nonce, l.Verifier, l.BinderHash,
		l.ClientChallenge, l.ReturnTo, nullable(l.LinkPrincipal), l.ExpiresAt)
	return err
}

const ssoLoginColumns = `state_hash, provider, client, nonce, verifier, binder_hash, client_challenge,
	return_to, COALESCE(link_principal::text, ''), expires_at, COALESCE(principal_id::text, '')`

func scanSSOLogin(row pgx.Row) (SSOLogin, error) {
	var l SSOLogin
	err := row.Scan(&l.StateHash, &l.Provider, &l.Client, &l.Nonce, &l.Verifier, &l.BinderHash,
		&l.ClientChallenge, &l.ReturnTo, &l.LinkPrincipal, &l.ExpiresAt, &l.PrincipalID)
	return l, err
}

// ConsumeSSOState marks a sign-in's state used and returns it; found is false when the
// state is unknown, expired, or already used. Marking and reading are one statement, so a
// callback replayed on another replica finds nothing.
func (s *Store) ConsumeSSOState(ctx context.Context, stateHash string) (SSOLogin, bool, error) {
	l, err := scanSSOLogin(s.pool.QueryRow(ctx, `
		UPDATE sso_logins SET used_at = now()
		 WHERE state_hash = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING `+ssoLoginColumns, stateHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return l, false, nil
	}
	return l, err == nil, err
}

// IssueSSOTicket attaches a one-time ticket for principalID to a consumed sign-in.
func (s *Store) IssueSSOTicket(ctx context.Context, stateHash, ticketHash string, principalID domain.ID, ttl time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sso_logins SET ticket_hash = $2, principal_id = $3::uuid, ticket_expires_at = now() + make_interval(secs => $4)
		 WHERE state_hash = $1 AND used_at IS NOT NULL AND ticket_hash IS NULL`,
		stateHash, ticketHash, principalID, ttl.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// RedeemSSOTicket consumes a ticket and returns its sign-in. The ticket must be presented
// with the proof the sign-in was bound to — the dashboard's binder cookie (its hash), or the
// CLI's PKCE challenge recomputed from its verifier — and that is checked in the same
// statement that consumes it, so a stolen ticket is neither usable nor able to burn the real
// one. found is false for an unknown, expired, used or unproven ticket alike.
func (s *Store) RedeemSSOTicket(ctx context.Context, ticketHash, binderHash, challenge string) (SSOLogin, bool, error) {
	l, err := scanSSOLogin(s.pool.QueryRow(ctx, `
		UPDATE sso_logins SET ticket_used_at = now()
		 WHERE ticket_hash = $1 AND ticket_used_at IS NULL AND ticket_expires_at > now()
		   AND ((client = 'dashboard' AND binder_hash <> '' AND binder_hash = $2)
		     OR (client = 'cli' AND client_challenge <> '' AND client_challenge = $3))
		RETURNING `+ssoLoginColumns, ticketHash, binderHash, challenge))
	if errors.Is(err, pgx.ErrNoRows) {
		return l, false, nil
	}
	return l, err == nil, err
}

// HasMembership reports whether a principal belongs to at least one project.
func (s *Store) HasMembership(ctx context.Context, principalID domain.ID) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM project_memberships WHERE principal_id = $1::uuid)`, principalID).Scan(&ok)
	return ok, err
}
