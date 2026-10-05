package harness

import (
	"slices"
	"strings"
	"testing"
)

func names(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		out = append(out, name)
	}
	return out
}

// A harness keeps what a coding agent needs and loses Conductor's credentials; a check — which
// runs whatever the agent left in the worktree — loses every credential-looking variable.
func TestSanitizeEnvLevels(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin", "HOME=/home/op", "GOFLAGS=-mod=mod", "LANG=C.UTF-8",
		"ANTHROPIC_API_KEY=sk-ant", "OPENAI_API_KEY=sk-oai",
		"DATABASE_URL=postgres://x", "CONDUCTOR_TOKEN=cdt_x", "PGPASSWORD=pw",
		"CONDUCTOR_CHECKPOINT_KEY=pass", "CONDUCTOR_BACKUP_S3_SECRET_KEY=s3",
		"CONDUCTOR_DEDUPE_SECRET=d", "CONDUCTOR_TERMINAL=kitty",
		"AWS_SECRET_ACCESS_KEY=aws", "AWS_REGION=us-east-1", "GITHUB_TOKEN=ghp",
		"SSH_AUTH_SOCK=/tmp/agent", "NPM_TOKEN=npm",
	}

	harness := names(SanitizeEnv(environ, false))
	for _, keep := range []string{"PATH", "HOME", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CONDUCTOR_TERMINAL", "AWS_REGION"} {
		if !slices.Contains(harness, keep) {
			t.Errorf("harness env lost %s, which an agent needs", keep)
		}
	}
	for _, drop := range []string{"DATABASE_URL", "CONDUCTOR_TOKEN", "PGPASSWORD",
		"CONDUCTOR_CHECKPOINT_KEY", "CONDUCTOR_BACKUP_S3_SECRET_KEY", "CONDUCTOR_DEDUPE_SECRET"} {
		if slices.Contains(harness, drop) {
			t.Errorf("harness env kept %s", drop)
		}
	}

	check := names(SanitizeEnv(environ, true))
	for _, keep := range []string{"PATH", "HOME", "GOFLAGS", "LANG"} {
		if !slices.Contains(check, keep) {
			t.Errorf("check env lost %s, which a toolchain needs", keep)
		}
	}
	for _, drop := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "DATABASE_URL",
		"CONDUCTOR_TOKEN", "CONDUCTOR_TERMINAL", "AWS_SECRET_ACCESS_KEY", "AWS_REGION",
		"GITHUB_TOKEN", "SSH_AUTH_SOCK", "NPM_TOKEN", "PGPASSWORD"} {
		if slices.Contains(check, drop) {
			t.Errorf("check env kept %s", drop)
		}
	}
}
