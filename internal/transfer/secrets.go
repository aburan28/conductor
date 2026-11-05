package transfer

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// This file is the pre-egress gate: a bundle can contain prompts, command output, file
// paths, and accidentally printed credentials. Export refuses to write one that contains a
// likely secret unless the caller opts in (--allow-secrets) or masks it (--redact). The
// same scan runs over an incoming bundle for `inspect`, so a file received from someone
// else can be checked before it is trusted.
//
// Detection is best-effort and pattern-based, favouring high-signal shapes over recall. It
// never phones home.

// Finding is one detected secret. Value is masked, so a report is safe to print.
type Finding struct {
	Type   string `json:"type"`
	Line   int    `json:"line"`
	Masked string `json:"masked"`
}

// SecretsError reports that a session cannot leave the machine as-is.
type SecretsError struct {
	File     string
	Findings []Finding
}

func (e *SecretsError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s contains %d likely secret(s)", e.File, len(e.Findings))
	for i, f := range e.Findings {
		if i == 5 {
			b.WriteString(", …")
			break
		}
		fmt.Fprintf(&b, "; line %d: %s %s", f.Line, f.Type, f.Masked)
	}
	b.WriteString(" — re-run with --redact to mask them or --allow-secrets to export anyway")
	return b.String()
}

type secretPattern struct {
	name string
	re   *regexp.Regexp
}

// Ordered most-specific first; each is anchored on a distinctive prefix or shape.
var secretPatterns = []secretPattern{
	{"private key block", regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`)},
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b`)},
	{"GitHub fine-grained token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{82}\b`)},
	{"Anthropic API key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"OpenAI API key", regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9]{20,}`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z\-_]{35}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"Bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{20,}`)},
	{"generic secret assignment", regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|passwd|password)\b\s*[:=]\s*['"]?[A-Za-z0-9._/+\-]{16,}['"]?`)},
}

// ScanSecrets returns the distinct secrets found in content, with line numbers. A masked
// value and its type are deduplicated, so repeating one key in a transcript reports once.
func ScanSecrets(content []byte) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, p := range secretPatterns {
		for _, loc := range p.re.FindAllIndex(content, -1) {
			masked := maskSecret(string(content[loc[0]:loc[1]]))
			key := p.name + "|" + masked
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Finding{
				Type:   p.name,
				Line:   bytes.Count(content[:loc[0]], []byte("\n")) + 1,
				Masked: masked,
			})
		}
	}
	return out
}

// RedactSecrets replaces every detected secret with a typed placeholder and reports how
// many replacements were made. It is lossy and opt-in. Placeholders contain no characters
// that need JSON escaping, so a transcript stays parseable.
func RedactSecrets(content []byte) ([]byte, int) {
	out := content
	total := 0
	for _, p := range secretPatterns {
		out = p.re.ReplaceAllFunc(out, func([]byte) []byte {
			total++
			return []byte("[REDACTED:" + strings.ReplaceAll(p.name, " ", "-") + "]")
		})
	}
	return out, total
}

// maskSecret shows the first four and last two characters, hiding the middle.
func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", 6) + s[len(s)-2:]
}
