package accounts

import (
	"os"
	"path/filepath"
	"testing"
)

// withEnv points both the profile root and the live credential file at a temp
// directory, so tests never touch a real harness login.
func withEnv(t *testing.T) (stateDir string, live string) {
	t.Helper()
	stateDir = t.TempDir()
	live = filepath.Join(stateDir, "live-credentials.json")
	if err := os.WriteFile(live, []byte(`{"token":"one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONDUCTOR_STATE_DIR", stateDir)
	t.Setenv("CONDUCTOR_LIVE_CLAUDE", live)
	return stateDir, live
}

func TestAddListSwitchActive(t *testing.T) {
	_, live := withEnv(t)

	if err := Add("claude", "main"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// A second account, snapshotted from a changed live file.
	if err := os.WriteFile(live, []byte(`{"token":"two"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Add("claude", "backup"); err != nil {
		t.Fatalf("Add backup: %v", err)
	}

	profiles, err := List("claude")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(profiles) != 2 || profiles[0].Name != "backup" || profiles[1].Name != "main" {
		t.Fatalf("profiles = %+v", profiles)
	}
	if got := Active("claude"); got != "backup" {
		t.Errorf("Active = %q, want backup (the live file matches it)", got)
	}

	if err := Switch("claude", "main"); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	body, err := os.ReadFile(live)
	if err != nil || string(body) != `{"token":"one"}` {
		t.Fatalf("live file after switch = %q, %v", body, err)
	}
	if got := Active("claude"); got != "main" {
		t.Errorf("Active after switch = %q, want main", got)
	}
}

func TestEnsureUsableSwitchesOffExhaustedAccount(t *testing.T) {
	_, live := withEnv(t)
	if err := Add("claude", "main"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(live, []byte(`{"token":"two"}`), 0o600)
	if err := Add("claude", "backup"); err != nil {
		t.Fatal(err)
	}
	if err := Switch("claude", "main"); err != nil {
		t.Fatal(err)
	}

	// Active account usable: no switch.
	if got, err := EnsureUsable("claude"); err != nil || got != "" {
		t.Fatalf("EnsureUsable = %q, %v; want no switch", got, err)
	}

	if err := MarkExhausted("claude", "main"); err != nil {
		t.Fatalf("MarkExhausted: %v", err)
	}
	got, err := EnsureUsable("claude")
	if err != nil {
		t.Fatalf("EnsureUsable: %v", err)
	}
	if got != "backup" {
		t.Fatalf("EnsureUsable switched to %q, want backup", got)
	}
	if a := Active("claude"); a != "backup" {
		t.Errorf("Active = %q, want backup", a)
	}

	// Exhaustion markers are visible in List.
	profiles, _ := List("claude")
	if profiles[1].Name != "main" || profiles[1].ExhaustedAt == nil {
		t.Errorf("main should carry an exhaustion marker: %+v", profiles)
	}

	// Both exhausted: nothing to do, stay put.
	if err := MarkExhausted("claude", "backup"); err != nil {
		t.Fatal(err)
	}
	if got, err := EnsureUsable("claude"); err != nil || got != "" {
		t.Fatalf("EnsureUsable with all exhausted = %q, %v; want no switch", got, err)
	}

	// Window rolls over: the marker clears and the account is usable again.
	if err := ClearExhausted("claude", "main"); err != nil {
		t.Fatal(err)
	}
	if err := Switch("claude", "main"); err != nil {
		t.Fatal(err)
	}
	if got, err := EnsureUsable("claude"); err != nil || got != "" {
		t.Fatalf("EnsureUsable after clear = %q, %v; want no switch", got, err)
	}
}

func TestAddWithoutLiveCredentials(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", stateDir)
	t.Setenv("CONDUCTOR_LIVE_CODEX", filepath.Join(stateDir, "missing.json"))
	if err := Add("codex", "main"); err == nil {
		t.Fatal("Add should fail when the harness is not logged in")
	}
}
