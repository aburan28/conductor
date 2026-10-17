package secrets

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/secretbox"
)

func TestVaultRoundTripAndCrossProjectSeal(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	store, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	org, err := store.CreateOrganization(ctx, "sec-org-"+suffix, "sec")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, db.CreateProjectParams{
		OrganizationID: org.ID, Slug: "app-" + suffix, DisplayName: "app", RepoPath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	alice, err := store.CreatePrincipal(ctx, org.ID, domain.PrincipalHuman, "alice-"+suffix, "alice", "")
	if err != nil {
		t.Fatal(err)
	}

	raw, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	v := New(store, &secretbox.Source{Env: raw})

	view, err := v.Set(ctx, project, alice.ID, SetRequest{
		Name: "OPENAI_API_KEY", Value: "sk-test-secret-value", Description: "model key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Hint != "…alue" {
		t.Fatalf("hint = %q", view.Hint)
	}
	list, err := v.List(ctx, project.ID)
	if err != nil || len(list) != 1 || list[0].Name != "OPENAI_API_KEY" {
		t.Fatalf("list = %+v, err=%v", list, err)
	}
	opened, err := v.Open(ctx, project, "OPENAI_API_KEY")
	if err != nil || opened.Value != "sk-test-secret-value" {
		t.Fatalf("open = %+v, err=%v", opened, err)
	}
	env, err := v.Export(ctx, project)
	if err != nil || env["OPENAI_API_KEY"] != "sk-test-secret-value" {
		t.Fatalf("export = %+v, err=%v", env, err)
	}

	other, err := store.CreateProject(ctx, db.CreateProjectParams{
		OrganizationID: org.ID, Slug: "other-" + suffix, DisplayName: "other", RepoPath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := store.GetProjectSecret(ctx, project.ID, "OPENAI_API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProjectSecret(ctx, db.ProjectSecret{
		OrganizationID: org.ID, ProjectID: other.ID, Name: "OPENAI_API_KEY",
		ValueSealed: sec.ValueSealed, Hint: sec.Hint, UpdatedBy: alice.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Open(ctx, other, "OPENAI_API_KEY"); err == nil {
		t.Fatal("opened a blob sealed for another project")
	}
}

func TestValidateName(t *testing.T) {
	if err := ValidateName("DATABASE_URL"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"1bad", "MY-API-KEY", "my.key", "export"} {
		if err := ValidateName(name); err == nil {
			t.Fatalf("expected error for %q", name)
		}
	}
	if !ShellName("export") || !ShellName("DATABASE_URL") || ShellName("MY-API-KEY") || ShellName("my.key") {
		t.Fatal("ShellName does not match portable identifiers")
	}
}

// A secret is exported into contributors' shells and wrap's child environment, so a name a
// loader, shell, git or runtime acts on would be code execution, not a credential.
func TestReservedNamesAreRefused(t *testing.T) {
	for _, name := range []string{"PATH", "path", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES",
		"PROMPT_COMMAND", "BASH_ENV", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_SSH_COMMAND",
		"NODE_OPTIONS", "BASH_FUNC_x%%", "npm_config_script_shell", "CONDUCTOR_ENDPOINT", "HTTPS_PROXY"} {
		if !ReservedName(name) {
			t.Errorf("ReservedName(%q) = false", name)
		}
	}
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GITHUB_TOKEN", "NPM_TOKEN",
		"PATHFINDER_KEY", "GIT_TOKEN", "DATABASE_URL"} {
		if ReservedName(name) {
			t.Errorf("ReservedName(%q) = true", name)
		}
	}

	raw, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	v := New(nil, &secretbox.Source{Env: raw}) // refused before the store is touched
	if _, err := v.Set(context.Background(), domain.Project{ID: "p"}, "", SetRequest{Name: "LD_PRELOAD", Value: "/tmp/x.so"}); err == nil {
		t.Error("the vault stored a secret named LD_PRELOAD")
	}
}
