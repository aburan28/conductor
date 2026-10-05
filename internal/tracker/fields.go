package tracker

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
)

// Field rules: what an issue's title and body become on a task.
//
// The title is the issue's title. The objective is the body with any acceptance section taken
// out, and the acceptance criteria are that section's list items. Both are bounded the way a
// task filed through the API is (privacy.MaxSummaryLength): a task is a coordination record,
// not a copy of the issue, and the whole text stays one click away on the issue itself.

// Bounds on what an issue contributes to a task.
const (
	maxObjective     = privacy.MaxSummaryLength
	maxTitle         = privacy.MaxSummaryLength
	maxCriteria      = 20
	maxCriterionText = 300
)

// Fields are a task's issue-derived fields.
type Fields struct {
	Title     string
	Objective string
	Criteria  []domain.AcceptanceCriterion
}

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	heading     = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	acceptance  = regexp.MustCompile(`(?i)^acceptance(\s+criteria)?\s*:?$`)
	listItem    = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?(.+?)\s*$`)
	blankRuns   = regexp.MustCompile(`\n{3,}`)
	spaceRuns   = regexp.MustCompile(`\s+`)
)

// MapFields maps an issue's title and body to task fields. key names the issue for a title
// that is empty.
func MapFields(key, title, body string) Fields {
	f := Fields{Title: clamp(spaceRuns.ReplaceAllString(strings.TrimSpace(title), " "), maxTitle)}
	if f.Title == "" {
		f.Title = "Issue " + key
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = htmlComment.ReplaceAllString(body, "")

	// Split out the acceptance section: from its heading to the next heading of the same
	// level or higher.
	var kept, section []string
	level := 0
	in := false
	for _, line := range strings.Split(body, "\n") {
		if m := heading.FindStringSubmatch(line); m != nil {
			switch {
			case !in && acceptance.MatchString(m[2]) && level == 0:
				in, level = true, len(m[1])
				continue
			case in && len(m[1]) <= level:
				in = false
			}
		}
		if in {
			section = append(section, line)
		} else {
			kept = append(kept, line)
		}
	}
	f.Objective = truncateText(blankRuns.ReplaceAllString(strings.TrimSpace(strings.Join(kept, "\n")), "\n\n"), maxObjective)
	f.Criteria = criteria(section)
	return f
}

// criteria reads an acceptance section: each list item is a criterion; a section with no list
// is one criterion, its text.
func criteria(section []string) []domain.AcceptanceCriterion {
	var out []domain.AcceptanceCriterion
	var prose []string
	for _, line := range section {
		if m := listItem.FindStringSubmatch(line); m != nil {
			if len(out) < maxCriteria {
				out = append(out, domain.AcceptanceCriterion{Text: clamp(m[1], maxCriterionText)})
			}
			continue
		}
		if t := strings.TrimSpace(line); t != "" {
			prose = append(prose, t)
		}
	}
	if len(out) == 0 && len(prose) > 0 {
		out = append(out, domain.AcceptanceCriterion{Text: clamp(strings.Join(prose, " "), maxCriterionText)})
	}
	return out
}

// TitleHash and BodyHash fingerprint the fields an issue contributed, so the sync can tell
// later whether the issue or the task changed them.
func (f Fields) TitleHash() string { return HashTitle(f.Title) }

func (f Fields) BodyHash() string { return HashBody(f.Objective, f.Criteria) }

// HashTitle fingerprints a title.
func HashTitle(title string) string { return digest(title) }

// HashBody fingerprints an objective and its criteria. A criterion's status and evidence are
// left out: verification changes those, not the text anyone wrote.
func HashBody(objective string, criteria []domain.AcceptanceCriterion) string {
	parts := []string{objective}
	for _, c := range criteria {
		parts = append(parts, c.Text)
	}
	return digest(strings.Join(parts, "\x00"))
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func clamp(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// truncateText bounds text to n runes, preferring to end at a paragraph, then a line, then a
// sentence, as long as that keeps at least half the room.
func truncateText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n-2])
	for _, sep := range []string{"\n\n", "\n", ". "} {
		if i := strings.LastIndex(cut, sep); i >= len(cut)/2 {
			if sep == ". " {
				i++ // keep the full stop
			}
			return strings.TrimSpace(cut[:i]) + " …"
		}
	}
	return strings.TrimSpace(cut) + "…"
}
