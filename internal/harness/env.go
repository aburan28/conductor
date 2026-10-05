package harness

import (
	"os"
	"strings"
)

// Environment hygiene for the processes a runner starts (DESIGN.md §25.2, §25.3).
//
// A runner executes code it did not write: the harness acts on a model's decisions, and the
// project's required checks run whatever the agent just left in the worktree — an edited
// Makefile, a test that reads the environment. Both inherit the runner's environment unless
// it is cleaned, and a runner's environment is where an operator's credentials live.
//
// Two levels:
//
//   - The harness keeps its own credentials (a coding agent needs its model API key, PATH,
//     HOME) but never Conductor's: the database URL, the operator's token, and every
//     CONDUCTOR_* secret are removed.
//   - A check is stricter. It needs a toolchain, not credentials, so anything that looks
//     like one — tokens, keys, passwords, cloud and model-provider settings, the SSH agent —
//     is removed as well.
//
// This is a blocklist, chosen because an allowlist would break checks on every toolchain it
// did not anticipate. It is not a sandbox: the process still runs as the runner's user and
// can read that user's files (DESIGN.md §25.3).

// SanitizeEnv returns environ without the variables a launched process must not inherit.
// strict selects the check level described above.
func SanitizeEnv(environ []string, strict bool) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if conductorSecret(name) || (strict && looksSecret(name)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// CheckEnv is the environment a required check runs with.
func CheckEnv() []string { return SanitizeEnv(os.Environ(), true) }

// conductorSecret reports a variable that carries Conductor's own credentials.
func conductorSecret(name string) bool {
	upper := strings.ToUpper(name)
	switch upper {
	case "DATABASE_URL", "CONDUCTOR_TOKEN", "CONDUCTOR_DB", "POSTGRES_PASSWORD",
		"PGPASSWORD", "PGPASSFILE":
		return true
	}
	return strings.HasPrefix(upper, "CONDUCTOR_") && secretWord(upper)
}

// looksSecret reports a variable that is probably a credential of some kind.
func looksSecret(name string) bool {
	upper := strings.ToUpper(name)
	if secretWord(upper) {
		return true
	}
	for _, prefix := range []string{
		"CONDUCTOR_", "AWS_", "AZURE_", "GOOGLE_APPLICATION_CREDENTIALS", "GCLOUD_",
		"ANTHROPIC_", "OPENAI_", "GEMINI_", "OPENROUTER_", "PG",
	} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	switch upper {
	case "SSH_AUTH_SOCK", "GPG_AGENT_INFO", "KRB5CCNAME", "DOCKER_AUTH_CONFIG", "NETRC":
		return true
	}
	return false
}

// secretWord reports a name containing a word that marks a credential.
func secretWord(upper string) bool {
	for _, word := range []string{
		"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "PRIVATE_KEY",
		"API_KEY", "APIKEY", "ACCESS_KEY", "AUTH_KEY", "SESSION_KEY",
	} {
		if strings.Contains(upper, word) {
			return true
		}
	}
	return strings.HasSuffix(upper, "_KEY") || strings.HasSuffix(upper, "_DSN")
}
