package modelsrc

import (
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

func ids(cs ...string) []Candidate {
	out := make([]Candidate, len(cs))
	for i, c := range cs {
		out[i] = Candidate{ID: c}
	}
	return out
}

func TestChoosePrefersHigherVersionsNumerically(t *testing.T) {
	sel := domain.ModelSelector{Endpoint: "e", Match: []string{"claude-sonnet-*"}}
	got, ok := Choose(sel, ids("claude-sonnet-5-9", "claude-sonnet-5-10", "claude-sonnet-4-5", "claude-opus-9"))
	if !ok || got.ID != "claude-sonnet-5-10" {
		t.Fatalf("picked %+v ok=%v, want claude-sonnet-5-10", got, ok)
	}
}

func TestChooseUsesReleaseDateWhenBothHaveOne(t *testing.T) {
	sel := domain.ModelSelector{Endpoint: "e", Match: []string{"*"}}
	older := Candidate{ID: "zzz-model", Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	newer := Candidate{ID: "aaa-model", Created: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	got, _ := Choose(sel, []Candidate{older, newer})
	if got.ID != "aaa-model" {
		t.Fatalf("picked %q, want the later release", got.ID)
	}
	// One side lacking a date falls back to the ids rather than guessing.
	got, _ = Choose(sel, []Candidate{{ID: "m-2"}, {ID: "m-10", Created: time.Now()}})
	if got.ID != "m-10" {
		t.Fatalf("picked %q", got.ID)
	}
}

func TestChooseMatchAndExclude(t *testing.T) {
	sel := domain.ModelSelector{
		Endpoint: "e", Match: []string{"gpt-*", "o?"}, Exclude: []string{"*-mini", "*-preview"},
	}
	if !Matches(sel, "o4") || Matches(sel, "o40") || Matches(sel, "gpt-8-preview") {
		t.Fatal("glob match or exclude misbehaved")
	}
	// A selector spanning families has no version order between them without release dates,
	// so the check of "newest" stays within one family.
	sel.Match = []string{"gpt-*"}
	got, ok := Choose(sel, ids("gpt-7", "gpt-7-mini", "gpt-8-preview", "o4", "text-embedding-9", "GPT-6"))
	if !ok || got.ID != "gpt-7" {
		t.Fatalf("picked %+v ok=%v, want gpt-7", got, ok)
	}
	if _, ok := Choose(sel, ids("text-embedding-9", "whisper-2")); ok {
		t.Fatal("chose a model that matches nothing")
	}
	if !Matches(sel, "GPT-6") {
		t.Fatal("matching should ignore case")
	}
}

func TestChooseIsStableAcrossInputOrder(t *testing.T) {
	sel := domain.ModelSelector{Endpoint: "e", Match: []string{"*"}}
	a, _ := Choose(sel, ids("m-1", "m-1a", "m-1b"))
	b, _ := Choose(sel, ids("m-1b", "m-1", "m-1a"))
	if a.ID != b.ID {
		t.Fatalf("order changed the pick: %q vs %q", a.ID, b.ID)
	}
}

func TestCompareNatural(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"a-9", "a-10", -1}, {"a-10", "a-9", 1}, {"a-007", "a-7", 0}, {"x", "x", 0},
		{"claude-5", "claude-5-5", -1}, {"v99999999999999999999", "v100000000000000000000", -1},
	}
	for _, c := range cases {
		if got := compareNatural(c.a, c.b); got != c.want {
			t.Errorf("compareNatural(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestValidateSelector(t *testing.T) {
	good := domain.ModelSelector{Endpoint: "anthropic", Match: []string{"claude-*"}, Pick: "newest"}
	if err := ValidateSelector(good); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]domain.ModelSelector{
		"no endpoint": {Match: []string{"x"}},
		"no match":    {Endpoint: "e"},
		"empty glob":  {Endpoint: "e", Match: []string{" "}},
		"bad pick":    {Endpoint: "e", Match: []string{"x"}, Pick: "cheapest"},
	} {
		if err := ValidateSelector(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGlobStarCrossesSlashes(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "moonshotai/kimi-k3", true},
		{"*kimi*", "accounts/fireworks/models/kimi-k3", true},
		{"accounts/*/models/kimi-*", "accounts/fireworks/models/kimi-k3", true},
		{"claude-sonnet-*", "claude-sonnet-5-5", true},
		{"claude-sonnet-*", "claude-opus-5", false},
		{"gpt-?", "gpt-7", true},
		{"gpt-?", "gpt-77", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"*-preview", "x-preview", true},
		{"", "", true},
		{"", "x", false},
		{"**", "", true},
	}
	for _, c := range cases {
		if got := glob(c.pattern, c.s); got != c.want {
			t.Errorf("glob(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}
