// Package quality implements advisory quality.lint: structural checks on a public
// task envelope. Findings never block merge; they are hints for authors and agents.
//
// Heuristics always run offline. When an OpenAI-compatible endpoint is configured,
// Lint may ask a cheap T1 model for extra findings and merge them (enhance.go).
package quality

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/privacy"
)

// Envelope is the public coordination surface a lint may see. It deliberately
// excludes chats, transcripts, secrets, and model output.
type Envelope struct {
	Summary            string                       `json:"summary"`
	Objective          string                       `json:"objective,omitempty"`
	AcceptanceCriteria []domain.AcceptanceCriterion `json:"acceptance_criteria,omitempty"`
	Scopes             []domain.ScopeRequest        `json:"scopes,omitempty"`
	Labels             []string                     `json:"labels,omitempty"`
	// Visibility, when private and RedactPrivate is set, clears intent fields before lint.
	Visibility domain.Visibility `json:"visibility,omitempty"`
	// RedactPrivate asks Lint to treat the envelope as a private task the caller may
	// not read: only structural scope/label signals remain.
	RedactPrivate bool `json:"-"`
}

// Finding is one advisory note. Severity is informational; nothing here is a gate.
type Finding struct {
	Severity string `json:"severity"` // error | warning | info
	Code     string `json:"code"`
	Where    string `json:"where"`
	Message  string `json:"message"`
}

// Result is always advisory. Clients must not treat it as a merge check.
type Result struct {
	Findings []Finding `json:"findings"`
	Advisory bool      `json:"advisory"`
	Engine   string    `json:"engine"` // heuristic | heuristic+llm
	Redacted bool      `json:"redacted,omitempty"`
	Enhanced bool      `json:"enhanced,omitempty"`
}

// Enhancer optionally adds LLM findings. Implementations must not persist prompts.
type Enhancer interface {
	Enhance(ctx context.Context, env Envelope, base []Finding) ([]Finding, error)
}

// Options controls Lint. Nil Enhancer means heuristic only.
type Options struct {
	Enhancer Enhancer
}

// Lint runs the heuristic baseline, then optional enhancement. It never returns an
// error for "bad quality" — only for unexpected enhancer failures that already
// fell back are swallowed so the advisory path stays available offline.
func Lint(ctx context.Context, env Envelope, opts Options) Result {
	redacted := false
	if env.RedactPrivate {
		env = redactPrivateEnvelope(env)
		redacted = true
	}
	// Normalize: ClampSummary keeps agent text from smuggling a chat into metadata.
	env.Summary = privacy.ClampSummary(strings.TrimSpace(env.Summary))
	env.Objective = privacy.ClampSummary(strings.TrimSpace(env.Objective))
	env = stripSecretLike(env)

	findings := heuristic(env)
	engine := "heuristic"
	enhanced := false

	if opts.Enhancer != nil && !redacted {
		extra, err := opts.Enhancer.Enhance(ctx, env, findings)
		if err == nil && len(extra) > 0 {
			findings = mergeFindings(findings, extra)
			engine = "heuristic+llm"
			enhanced = true
		}
	}

	if findings == nil {
		findings = []Finding{}
	}
	return Result{
		Findings: findings,
		Advisory: true,
		Engine:   engine,
		Redacted: redacted,
		Enhanced: enhanced,
	}
}

func redactPrivateEnvelope(env Envelope) Envelope {
	return Envelope{
		Scopes:        env.Scopes,
		Labels:        nil, // labels can leak intent on private work
		Visibility:    domain.VisibilityPrivate,
		RedactPrivate: true,
	}
}

var (
	vagueAC = regexp.MustCompile(`(?i)^(works?|done|fixed|ok|passes?|lgtm|good|fine|complete[sd]?)\.?$`)
	chatCue = regexp.MustCompile(`(?i)\b(Human:|Assistant:|\[ ass(istan)?t \]|<\/?thinking>)`)
	// Common secret shapes we refuse to forward to an enhancer or echo in findings.
	secretLike = regexp.MustCompile(`(?i)\b(sk-[a-z0-9]{20,}|ghp_[a-z0-9]{20,}|xox[baprs]-[a-z0-9-]{10,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)
)

func stripSecretLike(env Envelope) Envelope {
	env.Summary = secretLike.ReplaceAllString(env.Summary, "[redacted-secret]")
	env.Objective = secretLike.ReplaceAllString(env.Objective, "[redacted-secret]")
	for i := range env.AcceptanceCriteria {
		env.AcceptanceCriteria[i].Text = secretLike.ReplaceAllString(env.AcceptanceCriteria[i].Text, "[redacted-secret]")
	}
	return env
}

func heuristic(env Envelope) []Finding {
	var out []Finding

	sum := strings.TrimSpace(env.Summary)
	obj := strings.TrimSpace(env.Objective)

	if env.RedactPrivate {
		if len(env.Scopes) == 0 {
			out = append(out, Finding{
				Severity: "info", Code: "private_scopes_only", Where: "scopes",
				Message: "private task: only reserved scopes are visible; intent fields were redacted",
			})
		}
		for i, sc := range env.Scopes {
			out = append(out, checkScope(i, sc)...)
		}
		return out
	}

	if sum == "" {
		out = append(out, Finding{
			Severity: "warning", Code: "missing_summary", Where: "summary",
			Message: "summary is empty; a one-sentence intent helps duplicate detection and routing",
		})
	} else if utf8.RuneCountInString(sum) < 12 {
		out = append(out, Finding{
			Severity: "info", Code: "short_summary", Where: "summary",
			Message: "summary is very short; say what changes and why in one sentence",
		})
	}
	if chatCue.MatchString(sum) || chatCue.MatchString(obj) {
		out = append(out, Finding{
			Severity: "warning", Code: "chat_leak", Where: "summary",
			Message: "text looks like a chat transcript; keep task metadata free of conversations",
		})
	}
	if strings.Contains(sum, "[redacted-secret]") || strings.Contains(obj, "[redacted-secret]") {
		out = append(out, Finding{
			Severity: "warning", Code: "secret_like", Where: "summary",
			Message: "secret-like material was redacted; never put credentials in task fields",
		})
	}

	if obj == "" {
		out = append(out, Finding{
			Severity: "info", Code: "missing_objective", Where: "objective",
			Message: "objective is empty; a short success condition helps verifiers",
		})
	} else if looksLikeChatDump(obj) {
		out = append(out, Finding{
			Severity: "warning", Code: "objective_too_long", Where: "objective",
			Message: "objective is very long; keep the public objective short and put detail in the worktree",
		})
	}

	if len(env.AcceptanceCriteria) == 0 {
		out = append(out, Finding{
			Severity: "warning", Code: "missing_acceptance_criteria", Where: "acceptance_criteria",
			Message: "no acceptance criteria; add at least one checkable outcome",
		})
	} else {
		for i, ac := range env.AcceptanceCriteria {
			text := strings.TrimSpace(ac.Text)
			where := "acceptance_criteria[" + strconv.Itoa(i) + "]"
			if text == "" {
				out = append(out, Finding{
					Severity: "warning", Code: "empty_criterion", Where: where,
					Message: "acceptance criterion text is empty",
				})
				continue
			}
			if vagueAC.MatchString(text) {
				out = append(out, Finding{
					Severity: "info", Code: "vague_criterion", Where: where,
					Message: "criterion is vague; name a command, behaviour, or observable result",
				})
			}
			if secretLike.MatchString(text) || strings.Contains(text, "[redacted-secret]") {
				out = append(out, Finding{
					Severity: "warning", Code: "secret_like", Where: where,
					Message: "criterion contained secret-like material that was redacted",
				})
			}
		}
	}

	if len(env.Scopes) == 0 {
		out = append(out, Finding{
			Severity: "warning", Code: "missing_scopes", Where: "scopes",
			Message: "no scopes reserved; declare path: or dir: resources before editing",
		})
	} else {
		for i, sc := range env.Scopes {
			out = append(out, checkScope(i, sc)...)
		}
	}

	if len(env.Labels) == 0 && (sum != "" || obj != "") {
		out = append(out, Finding{
			Severity: "info", Code: "no_labels", Where: "labels",
			Message: "no labels; tags like docs/backend help dispatch filters",
		})
	}

	return out
}

func checkScope(i int, sc domain.ScopeRequest) []Finding {
	var out []Finding
	where := "scopes[" + strconv.Itoa(i) + "]"
	res := strings.TrimSpace(sc.Resource)
	if res == "" {
		return []Finding{{
			Severity: "warning", Code: "empty_scope", Where: where,
			Message: "scope resource is empty",
		}}
	}
	typ, key, ok := strings.Cut(res, ":")
	if !ok || typ == "" || key == "" {
		out = append(out, Finding{
			Severity: "warning", Code: "malformed_scope", Where: where,
			Message: "scope should look like path:FILE, dir:DIR, or migration:NAME",
		})
		return out
	}
	switch typ {
	case "path", "dir", "migration":
	default:
		out = append(out, Finding{
			Severity: "info", Code: "unusual_scope_type", Where: where,
			Message: "unusual scope type " + typ + "; prefer path:, dir:, or migration:",
		})
	}
	if typ == "dir" && (key == "." || key == "/" || key == "") {
		out = append(out, Finding{
			Severity: "warning", Code: "overbroad_scope", Where: where,
			Message: "directory scope is over-broad; reserve a tighter path when possible",
		})
	}
	if typ == "path" && strings.HasSuffix(key, "/") {
		out = append(out, Finding{
			Severity: "info", Code: "path_looks_like_dir", Where: where,
			Message: "path: ends with /; did you mean dir:?",
		})
	}
	return out
}

func looksLikeChatDump(s string) bool {
	if utf8.RuneCountInString(s) > 2000 {
		return true
	}
	lines := strings.Count(s, "\n")
	return lines > 40
}

func mergeFindings(base, extra []Finding) []Finding {
	seen := map[string]bool{}
	out := make([]Finding, 0, len(base)+len(extra))
	key := func(f Finding) string {
		return f.Code + "|" + f.Where + "|" + f.Message
	}
	for _, f := range base {
		seen[key(f)] = true
		out = append(out, f)
	}
	for _, f := range extra {
		f.Severity = normalizeSeverity(f.Severity)
		f.Code = sanitizeToken(f.Code)
		f.Where = sanitizeWhere(f.Where)
		f.Message = privacy.ClampSummary(strings.TrimSpace(f.Message))
		if f.Message == "" || seen[key(f)] {
			continue
		}
		if secretLike.MatchString(f.Message) {
			continue
		}
		seen[key(f)] = true
		out = append(out, f)
	}
	return out
}

func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error", "warning", "info":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return "info"
	}
}

func sanitizeToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "llm_finding"
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	if b.Len() == 0 {
		return "llm_finding"
	}
	return b.String()
}

func sanitizeWhere(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "envelope"
	}
	if utf8.RuneCountInString(s) > 80 {
		r := []rune(s)
		return string(r[:80])
	}
	return s
}
