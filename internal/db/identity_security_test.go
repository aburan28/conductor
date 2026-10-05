package db

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

// AddMember used to be an upsert, so adding an existing member silently rewrote their role.
func TestAddMemberNeverChangesAnExistingRole(t *testing.T) {
	f := newFixture(t)
	err := f.store.AddMember(f.ctx, f.project.ID, f.bob.ID, domain.RoleProjectAdmin)
	if !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("AddMember of an existing member = %v, want ErrDuplicate", err)
	}
	if role, _ := f.store.RoleIn(f.ctx, f.project.ID, f.bob.ID); role != domain.RoleContributor {
		t.Errorf("bob's role became %s", role)
	}
	if err := f.store.SetMemberRole(f.ctx, f.project.ID, f.bob.ID, domain.RoleMaintainer); err != nil {
		t.Fatal(err)
	}
	if role, _ := f.store.RoleIn(f.ctx, f.project.ID, f.bob.ID); role != domain.RoleMaintainer {
		t.Errorf("SetMemberRole left bob as %s", role)
	}
	stranger, err := f.store.CreatePrincipal(f.ctx, f.org.ID, domain.PrincipalHuman, "stranger", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMemberRole(f.ctx, f.project.ID, stranger.ID, domain.RoleObserver); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("SetMemberRole for a non-member = %v, want ErrNotFound (it must not create a membership)", err)
	}
}

// Re-running bootstrap retires the bootstrap token it printed before.
func TestReplaceTokenRevokesThePreviousOne(t *testing.T) {
	f := newFixture(t)
	first, _, err := f.store.ReplaceToken(f.ctx, f.alice.ID, "bootstrap", tokenTestTTL)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.store.CreateToken(f.ctx, f.alice.ID, "laptop", tokenTestTTL)
	if err != nil {
		t.Fatal(err)
	}
	second, revoked, err := f.store.ReplaceToken(f.ctx, f.alice.ID, "bootstrap", tokenTestTTL)
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Errorf("revoked %d, want the one earlier bootstrap token", revoked)
	}
	if _, err := f.store.AuthenticateToken(f.ctx, first); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("the earlier bootstrap token still works: %v", err)
	}
	for _, tok := range []string{second, other} {
		if _, err := f.store.AuthenticateToken(f.ctx, tok); err != nil {
			t.Errorf("a token that should survive does not: %v", err)
		}
	}
	tokens, _ := f.store.ListTokens(f.ctx, f.alice.ID)
	for _, tk := range tokens {
		if tk.Name == "bootstrap" && tk.RevokedAt == nil && tk.ExpiresAt == nil {
			t.Error("the bootstrap token never expires")
		}
	}
}

// A scoped token reports its project, so the API can confine it.
func TestScopedTokenCarriesItsProject(t *testing.T) {
	f := newFixture(t)
	tok, err := f.store.CreateScopedToken(f.ctx, f.alice.ID, f.project.ID, "attempt:x", tokenTestTTL)
	if err != nil {
		t.Fatal(err)
	}
	p, info, err := f.store.AuthenticateTokenInfo(f.ctx, tok)
	if err != nil || p.ID != f.alice.ID || info.ProjectID != f.project.ID || info.ExpiresAt == nil {
		t.Errorf("AuthenticateTokenInfo = %+v, %+v, %v", p, info, err)
	}
}

// With CONDUCTOR_DEDUPE_SECRET set, the key in the database alone no longer reproduces a
// fingerprint.
func TestDedupeSecretChangesTheEffectiveKey(t *testing.T) {
	f := newFixture(t)
	t.Setenv(DedupeSecretEnv, "")
	stored, err := f.store.DedupeKeyForProject(f.ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, f.org.DedupeKey) {
		t.Fatal("without a secret the stored key must be used unchanged")
	}
	t.Setenv(DedupeSecretEnv, "held-outside-the-database")
	mixed, err := f.store.DedupeKeyForProject(f.ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(mixed, stored) || len(mixed) != 32 {
		t.Errorf("the secret did not change the key")
	}
}

// Only the principal that registered a runner can heartbeat it or re-register its name.
func TestRunnerBelongsToItsPrincipal(t *testing.T) {
	f := newFixture(t)
	r, err := f.store.RegisterRunner(f.ctx, domain.Runner{OrganizationID: f.org.ID,
		ProjectID: f.project.ID, PrincipalID: f.alice.ID, Name: "box-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.HeartbeatRunner(f.ctx, r.ID, f.bob.ID, 5); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("heartbeat by another principal = %v, want ErrNotFound", err)
	}
	if err := f.store.HeartbeatRunner(f.ctx, "not-a-uuid", f.alice.ID, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("heartbeat of a malformed id = %v, want ErrNotFound", err)
	}
	if err := f.store.HeartbeatRunner(f.ctx, r.ID, f.alice.ID, 1); err != nil {
		t.Errorf("owner heartbeat: %v", err)
	}
	if _, err := f.store.RegisterRunner(f.ctx, domain.Runner{OrganizationID: f.org.ID,
		ProjectID: f.project.ID, PrincipalID: f.bob.ID, Name: "box-1"}); !errors.Is(err, domain.ErrDuplicate) {
		t.Errorf("registering another principal's runner name = %v, want ErrDuplicate", err)
	}
	again, err := f.store.RegisterRunner(f.ctx, domain.Runner{OrganizationID: f.org.ID,
		ProjectID: f.project.ID, PrincipalID: f.alice.ID, Name: "box-1", MaxConcurrency: 3})
	if err != nil || again.ID != r.ID || again.MaxConcurrency != 3 {
		t.Errorf("owner re-registration = %+v, %v", again, err)
	}
}

const tokenTestTTL = 24 * time.Hour
