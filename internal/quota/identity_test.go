package quota

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeAccountKeyChangesWhenDefaultLoginChanges(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(id, org string) {
		t.Helper()
		body := `{"oauthAccount":{"accountUuid":"` + id + `","organizationUuid":"` + org + `","emailAddress":"person@example.test"}}`
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("one", "org-a")
	first := AccountKey("claude", dir, home)
	if !strings.HasPrefix(first, "default-id-") || Identity("claude", dir, home).Email != "person@example.test" {
		t.Fatalf("first profile identity missing: %s", first)
	}
	write("two", "org-a")
	second := AccountKey("claude", dir, home)
	if first == second {
		t.Fatal("switching credentials in default reused the old quota key")
	}
	write("one", "org-b")
	if third := AccountKey("claude", dir, home); third == first || third == second {
		t.Fatalf("organization change reused an old quota key: %s", third)
	}
	if got := AccountKey("claude", filepath.Join(home, ".claude-empty"), home); got != "empty" {
		t.Fatalf("missing metadata should keep an unlabeled profile distinct: %s", got)
	}
}

func TestCurrentIdentityOnlyDecoratesMatchingLocalAccount(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"oauthAccount":{"accountUuid":"owner","organizationUuid":"org","emailAddress":"owner@example.test"}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	key := AccountKey("claude", dir, home)
	email, ok := CurrentIdentity("claude", key, host, home, func(string) string { return "" })
	if !ok || email != "owner@example.test" {
		t.Fatalf("owner identity missing: %q, %v", email, ok)
	}
	for _, c := range []struct{ key, host string }{{"default", host}, {key, "another-machine"}} {
		if email, ok := CurrentIdentity("claude", c.key, c.host, home, func(string) string { return "" }); ok || email != "" {
			t.Fatalf("identity crossed account or machine boundary: %q, %v", email, ok)
		}
	}
}

func TestCodexAccountKeyIgnoresTokenRefreshAndSeparatesAccounts(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(subject, account, signature string) {
		t.Helper()
		claims, _ := json.Marshal(map[string]any{"sub": subject, "email": "person@example.test",
			"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}})
		token := "header." + base64.RawURLEncoding.EncodeToString(claims) + "." + signature
		body, _ := json.Marshal(map[string]any{"tokens": map[string]string{"id_token": token}})
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("user-one", "account-a", "old")
	first := AccountKey("codex", dir, home)
	write("user-one", "account-a", "refreshed")
	if got := AccountKey("codex", dir, home); got != first {
		t.Fatalf("token refresh changed account key: %s -> %s", first, got)
	}
	write("user-one", "account-b", "new-account")
	if got := AccountKey("codex", dir, home); got == first {
		t.Fatal("second ChatGPT account reused quota key")
	}
}

func TestCodexRolloutIgnoresPreviousAccountInSameStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"sub": "current", "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "current-account"}})
	auth, _ := json.Marshal(map[string]any{"tokens": map[string]string{
		"id_token": "head." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"}})
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 19, 0, 0, 0, time.UTC)
	day := filepath.Join(dir, "sessions", "2026", "10", "16")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	for account, percent := range map[string]string{"current-account": "25", "old-account": "100"} {
		body := `{"type":"session_meta","payload":{"creator_account_id":"` + account + `"}}` + "\n" +
			`{"type":"event_msg","timestamp":"2026-10-16T18:00:00Z","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":` + percent + `,"window_minutes":10080,"resets_at":1792785600}}}}` + "\n"
		path := filepath.Join(day, "rollout-"+account+".jsonl")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatal(err)
		}
	}
	snaps := ReadCodex(dir, AccountKey("codex", dir, home), now)
	if len(snaps) != 1 || snaps[0].UsedPercent == nil || *snaps[0].UsedPercent != 25 {
		t.Fatalf("previous account's limit affected current account: %+v", snaps)
	}
}

func TestClaudeTranscriptLimitIgnoresPreviousAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude")
	project := filepath.Join(dir, "projects", "-work-repo")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := `{"oauthAccount":{"accountUuid":"current","organizationUuid":"org"}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"current", "previous"} {
		bridge := `{"type":"bridge-session","ownerAccountUuid":"` + id + `","ownerOrganizationUuid":"org"}` + "\n"
		body := []byte(bridge)
		if id == "previous" {
			body = append(body, fixtureBytes(t, "claude-transcript.jsonl")...)
		}
		if err := os.WriteFile(filepath.Join(project, id+".jsonl"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if id, _ := claudeTranscriptAccount(filepath.Join(project, "previous.jsonl")); id != "previous" {
		t.Fatalf("bridge account = %s", id)
	}
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	snaps := ReadClaudeTranscripts(dir, AccountKey("claude", dir, home), now)
	if len(snaps) != 0 {
		t.Fatalf("previous account's exhausted windows reached current account: %+v", snaps)
	}
}
