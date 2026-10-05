package db

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// The two unique keys and the first-link rule are what keep one external account from
// reaching two principals, and a second account from attaching to a principal that already
// signs in with one. They hold in SQL, whatever the API does.
func TestLinkIdentityInvariants(t *testing.T) {
	f := newFixture(t)
	tag := fmt.Sprintf("%x", time.Now().UnixNano())
	issuer := "https://idp-" + tag + ".example"
	acct := func(sub string) ExternalAccount {
		return ExternalAccount{Provider: "test", Issuer: issuer, Subject: sub, Email: sub + "@example.com"}
	}

	if err := f.store.LinkIdentity(f.ctx, f.alice.ID, acct("a"), true); err != nil {
		t.Fatal(err)
	}
	if err := f.store.LinkIdentity(f.ctx, f.bob.ID, acct("a"), false); !errors.Is(err, ErrIdentityTaken) {
		t.Errorf("one account linked to two principals: %v", err)
	}
	if err := f.store.LinkIdentity(f.ctx, f.alice.ID, acct("a2"), false); !errors.Is(err, ErrIssuerTaken) {
		t.Errorf("a second account at one issuer linked: %v", err)
	}
	other := ExternalAccount{Provider: "gh", Issuer: "https://gh-" + tag + ".example", Subject: "1"}
	if err := f.store.LinkIdentity(f.ctx, f.alice.ID, other, true); !errors.Is(err, ErrAlreadyLinked) {
		t.Errorf("a first-sign-in link attached to a principal that already has an identity: %v", err)
	}
	// An explicit link by the signed-in principal may add another issuer.
	if err := f.store.LinkIdentity(f.ctx, f.alice.ID, other, false); err != nil {
		t.Errorf("linking a second issuer: %v", err)
	}

	got, found, err := f.store.IdentityByAccount(f.ctx, issuer, "a")
	if err != nil || !found || got.PrincipalID != f.alice.ID {
		t.Fatalf("IdentityByAccount = %+v %v %v", got, found, err)
	}
	tok, err := f.store.CreateToken(f.ctx, f.alice.ID, SSOTokenPrefix+"test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := f.store.UnlinkIdentity(f.ctx, f.alice.ID, "test")
	if err != nil || revoked != 1 {
		t.Fatalf("unlink = %d, %v", revoked, err)
	}
	if _, err := f.store.AuthenticateToken(f.ctx, tok); err == nil {
		t.Error("the unlinked identity's token still authenticates")
	}
}

func TestSanitizeHandle(t *testing.T) {
	for in, want := range map[string]string{
		"Jane.Doe":       "jane.doe",
		"jane+conductor": "jane",
		"--x--":          "x",
		"日本":             "user",
		"octo-cat_9":     "octo-cat_9",
		"averyveryveryveryverylonglocalpartindeed": "averyveryveryveryverylonglocalpa",
	} {
		if got := SanitizeHandle(in); got != want {
			t.Errorf("SanitizeHandle(%q) = %q, want %q", in, got, want)
		}
	}
}
