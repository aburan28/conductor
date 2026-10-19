package accounts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAliasToAccountFollowsRepointing(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", state)
	project := t.TempDir()
	noEnv := func(string) string { return "" }

	if err := SetAlias("claude", "acme", "work"); err != nil {
		t.Fatalf("SetAlias: %v", err)
	}
	if err := Bind(project, "claude", "acme"); err != nil {
		t.Fatal(err)
	}
	iso, ok, err := Resolve("claude", project, noEnv)
	if err != nil || !ok {
		t.Fatalf("Resolve = %v, %v", ok, err)
	}
	if want := filepath.Join(state, "accounts", "claude", "work"); iso.Dir != want || iso.Account != "acme" || iso.AliasOf != "work" {
		t.Fatalf("iso = %+v, want acme → work in %s", iso, want)
	}

	// Re-pointing the alias moves the bound project with it.
	if err := SetAlias("claude", "acme", "client"); err != nil {
		t.Fatal(err)
	}
	if iso, _, _ := Resolve("claude", project, noEnv); filepath.Base(iso.Dir) != "client" {
		t.Errorf("after re-point, dir = %s", iso.Dir)
	}
}

func TestAliasToDirectory(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	legacy := filepath.Join(t.TempDir(), "claude-legacy")
	if err := SetAlias("claude", "old", legacy); err != nil {
		t.Fatal(err)
	}
	iso, err := Prepare("claude", "old")
	if err != nil {
		t.Fatal(err)
	}
	if iso.Dir != legacy || iso.AliasOf != legacy {
		t.Fatalf("iso = %+v, want dir %s", iso, legacy)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("directory not created: %v", err)
	}
	// A directory alias has no credential snapshot to switch to.
	if err := Switch("claude", "old"); err == nil {
		t.Error("Switch to a directory alias succeeded")
	}
}

func TestAliasForSnapshotAccount(t *testing.T) {
	_, live := withEnv(t)
	if err := Add("claude", "main"); err != nil {
		t.Fatal(err)
	}
	if err := SetAlias("claude", "m", "main"); err != nil {
		t.Fatal(err)
	}
	if err := MarkExhausted("claude", "m"); err != nil {
		t.Fatalf("MarkExhausted via alias: %v", err)
	}
	profiles, _ := List("claude")
	if len(profiles) != 1 || profiles[0].ExhaustedAt == nil {
		t.Fatalf("marker not written for main: %+v", profiles)
	}
	if err := ClearExhausted("claude", "m"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(live, []byte(`{"token":"other"}`), 0o600)
	if err := Switch("claude", "m"); err != nil {
		t.Fatalf("Switch via alias: %v", err)
	}
	if Active("claude") != "main" {
		t.Error("alias switch did not make main active")
	}
	// An alias name cannot be taken by a new snapshot.
	if err := Add("claude", "m"); err == nil {
		t.Error("Add over an alias succeeded")
	}
}

func TestAliasRules(t *testing.T) {
	_, _ = withEnv(t)
	if err := Add("claude", "main"); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ alias, target, why string }{
		{"main", "other", "shadows an account"},
		{"x", "x", "points at itself"},
		{"../x", "main", "path-like alias"},
		{"y", ".hidden", "invalid target name"},
	}
	for _, c := range cases {
		if err := SetAlias("claude", c.alias, c.target); err == nil {
			t.Errorf("SetAlias(%q, %q) accepted (%s)", c.alias, c.target, c.why)
		}
	}
	if err := SetAlias("claude", "a", "main"); err != nil {
		t.Fatal(err)
	}
	if err := SetAlias("claude", "b", "a"); err == nil {
		t.Error("alias to an alias accepted")
	}
	if err := SetAlias("claude", "main2", "main"); err != nil {
		t.Fatal(err)
	}
	if err := SetAlias("vim", "a", "main"); err == nil {
		t.Error("unknown harness accepted")
	}
	if err := RemoveAlias("claude", "nope"); err == nil {
		t.Error("removing a missing alias succeeded")
	}
	if err := RemoveAlias("claude", "a"); err != nil {
		t.Fatal(err)
	}
	list, _ := Aliases("")
	if len(list) != 1 || list[0].Alias != "main2" {
		t.Errorf("Aliases = %+v", list)
	}
}
