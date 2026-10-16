package modelsrc

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

// PickNewest is the default, and only, strategy for choosing among qualifying models.
const PickNewest = "newest"

// Candidate is a discovered model as selection sees it.
type Candidate struct {
	ID      string
	Created time.Time
}

// ValidateSelector checks a selector as declared, before it is stored.
func ValidateSelector(s domain.ModelSelector) error {
	if strings.TrimSpace(s.Endpoint) == "" {
		return fmt.Errorf("%w: select.endpoint is required", domain.ErrInvalidArgument)
	}
	if len(s.Match) == 0 {
		return fmt.Errorf("%w: select.match needs at least one pattern", domain.ErrInvalidArgument)
	}
	for _, p := range append(append([]string(nil), s.Match...), s.Exclude...) {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("%w: select patterns must not be empty", domain.ErrInvalidArgument)
		}
	}
	if s.Pick != "" && s.Pick != PickNewest {
		return fmt.Errorf("%w: select.pick %q is not supported (use %q)", domain.ErrInvalidArgument, s.Pick, PickNewest)
	}
	return nil
}

// Matches reports whether id qualifies under the selector's match and exclude globs.
// Matching is case-insensitive: providers disagree about case, and operators should not have
// to.
func Matches(s domain.ModelSelector, id string) bool {
	id = strings.ToLower(id)
	hit := false
	for _, p := range s.Match {
		if glob(strings.ToLower(p), id) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	for _, p := range s.Exclude {
		if glob(strings.ToLower(p), id) {
			return false
		}
	}
	return true
}

// glob matches s against a pattern where `*` is any run of characters, slashes included, and
// `?` is any single character. path.Match would not do: its `*` stops at `/`, and model ids
// such as "moonshotai/kimi-k3" or "accounts/fireworks/models/x" are full of them.
func glob(pattern, s string) bool {
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// Choose returns the model a selector picks from the candidates, and false when none
// qualifies. "Newest" orders by release time when the provider supplies it for every
// qualifying model, and otherwise by version-aware comparison of the ids, so "claude-sonnet-5-10"
// outranks "claude-sonnet-5-9" and a provider that reports no dates still works. The id
// order is meaningful within one model family; a selector that spans families should rely on
// the provider's release dates or be split into one profile per family.
func Choose(s domain.ModelSelector, candidates []Candidate) (Candidate, bool) {
	var ok []Candidate
	for _, c := range candidates {
		if Matches(s, c.ID) {
			ok = append(ok, c)
		}
	}
	if len(ok) == 0 {
		return Candidate{}, false
	}
	// Dates are used only when every qualifying model has one. Mixing dated and undated models
	// would make the order depend on which pairs happen to be compared.
	dated := true
	for _, c := range ok {
		if c.Created.IsZero() {
			dated = false
			break
		}
	}
	sort.SliceStable(ok, func(i, j int) bool { return newer(ok[i], ok[j], dated) })
	return ok[0], true
}

// newer reports whether a should be preferred over b.
func newer(a, b Candidate, dated bool) bool {
	if dated && !a.Created.Equal(b.Created) {
		return a.Created.After(b.Created)
	}
	if c := compareNatural(a.ID, b.ID); c != 0 {
		return c > 0
	}
	return a.ID < b.ID
}

// compareNatural compares strings treating runs of digits as numbers, so 10 > 9.
func compareNatural(a, b string) int {
	a, b = strings.ToLower(a), strings.ToLower(b)
	for a != "" && b != "" {
		if isDigit(a[0]) && isDigit(b[0]) {
			na, ra := digits(a)
			nb, rb := digits(b)
			if c := compareNumeric(na, nb); c != 0 {
				return c
			}
			a, b = ra, rb
			continue
		}
		if a[0] != b[0] {
			if a[0] < b[0] {
				return -1
			}
			return 1
		}
		a, b = a[1:], b[1:]
	}
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	default:
		return 1
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func digits(s string) (run, rest string) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

// compareNumeric compares digit strings of any length without overflow.
func compareNumeric(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	switch {
	case len(a) != len(b):
		if len(a) < len(b) {
			return -1
		}
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
