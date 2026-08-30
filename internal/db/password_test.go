package db

import (
	"strings"
	"testing"
)

// Pure unit tests: hashing lives entirely in Go, so it needs no database. The SQL side is
// covered by the API integration tests.

func TestHashPasswordRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	stored, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(stored, "pbkdf2-sha256$") {
		t.Errorf("stored hash does not name its scheme: %q", stored)
	}
	// Two hashes of the same password must differ: a per-hash salt is what makes the
	// stored rows useless for building a rainbow table.
	again, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash again: %v", err)
	}
	if again == stored {
		t.Error("two hashes of one password were identical; the salt is not random")
	}
	if !verifyPassword(stored, pw) {
		t.Error("the right password did not verify")
	}
	if verifyPassword(stored, pw+" ") {
		t.Error("a wrong password verified")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"",
		"not-a-hash",
		"scrypt$600000$abc$def",
		"pbkdf2-sha256$notanumber$abc$def",
		"pbkdf2-sha256$600000$!!!not-base64!!!$def",
	} {
		if verifyPassword(bad, "whatever") {
			t.Errorf("malformed hash %q verified; it must fail closed", bad)
		}
	}
}

func TestHashPasswordEnforcesMinimumLength(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Error("a 5-character password was accepted")
	}
}
