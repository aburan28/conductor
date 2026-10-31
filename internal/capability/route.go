package capability

import (
	"fmt"
	"sort"
	"strings"
)

// Considered is one model the router looked at and why it was or was not eligible.
type Considered struct {
	Target   string `json:"target"`
	Eligible bool   `json:"eligible"`
	Why      string `json:"why"`
}

// Route is a delegation decision for one task family (docs/DELPHI_OBJECTIVES.md §4): the
// cheapest model measured good enough, and at most two stronger ones to escalate to.
type Route struct {
	Family     string       `json:"family"`
	Delta      float64      `json:"delta"`
	Level      float64      `json:"level,omitempty"`
	Reference  string       `json:"reference"`
	Choice     string       `json:"choice"`
	Escalation []string     `json:"escalation,omitempty"`
	Basis      string       `json:"basis"`
	Considered []Considered `json:"considered"`
}

// Route chooses a model for family from its calibrations. A model is eligible when it is
// calibrated (prior-only models never are), allow accepts it (free mode, a configured
// provider), and it is good enough: its Wilson low bound reaches the family's level, or,
// with no level, it is the reference or measured equivalent to it within delta. Among
// eligible models the cheapest per accepted answer wins; a model with no known cost is never
// taken to be cheaper, and ties go to the higher low bound. Escalation is the next better
// eligible model and the best one.
func (r *Registry) Route(family string, allow func(target string) (bool, string)) (Route, error) {
	t := r.Table(family)
	out := Route{Family: family, Delta: t.Delta, Level: t.Level, Reference: t.Reference}
	var eligible []Row
	for _, row := range t.Rows {
		c := Considered{Target: row.Target}
		switch {
		case row.Label != "calibrated" || row.Graded == 0:
			c.Why = "not calibrated on this family"
		case t.Level > 0 && row.Low < t.Level:
			c.Why = fmt.Sprintf("Wilson low %.3f is below the level %.2f", row.Low, t.Level)
		case t.Level == 0 && row.Target != t.Reference && row.Verdict != Equivalent:
			c.Why = fmt.Sprintf("%s against the reference %s (needs equivalent)", orDash(string(row.Verdict)), t.Reference)
		default:
			if ok, why := allow(row.Target); !ok {
				c.Why = why
			} else {
				c.Eligible, c.Why = true, "eligible"
				eligible = append(eligible, row)
			}
		}
		out.Considered = append(out.Considered, c)
	}
	if len(eligible) == 0 {
		return out, fmt.Errorf("no model qualifies for %s: calibrate one (delphi calibrate) or review the family's level; considered: %s",
			family, summarize(out.Considered))
	}
	cheapest := append([]Row(nil), eligible...)
	sort.SliceStable(cheapest, func(i, j int) bool {
		a, b := cheapest[i], cheapest[j]
		switch {
		case a.CostPer != nil && b.CostPer != nil && *a.CostPer != *b.CostPer:
			return *a.CostPer < *b.CostPer
		case (a.CostPer == nil) != (b.CostPer == nil):
			return a.CostPer != nil // a known cost beats an unknown one
		}
		return a.Low > b.Low
	})
	choice := cheapest[0]
	out.Choice = choice.Target
	var stronger []Row
	for _, row := range eligible {
		if row.Low > choice.Low {
			stronger = append(stronger, row)
		}
	}
	sort.Slice(stronger, func(i, j int) bool { return stronger[i].Low < stronger[j].Low })
	if len(stronger) > 2 {
		stronger = []Row{stronger[0], stronger[len(stronger)-1]}
	}
	for _, row := range stronger {
		out.Escalation = append(out.Escalation, row.Target)
	}
	out.Basis = basis(choice, t)
	return out, nil
}

func basis(row Row, t Table) string {
	b := fmt.Sprintf("%s passed %d/%d on %s (95%% %.3f..%.3f)", row.Target, row.Passed, row.Graded, t.Family, row.Low, row.High)
	if row.CostPer != nil {
		b += fmt.Sprintf(", $%.4f per accepted answer", *row.CostPer)
	} else {
		b += ", cost unknown"
	}
	switch {
	case t.Level > 0:
		b += fmt.Sprintf("; meets level %.2f", t.Level)
	case row.Target == t.Reference:
		b += "; the reference (no cheaper model is measured equivalent)"
	default:
		b += fmt.Sprintf("; equivalent to the reference %s within %.2f", t.Reference, t.Delta)
	}
	return b
}

func summarize(cs []Considered) string {
	if len(cs) == 0 {
		return "no calibrations"
	}
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.Target + " (" + c.Why + ")"
	}
	return strings.Join(parts, "; ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ParseTarget reads "provider:model[@effort]" as Target.String writes it.
func ParseTarget(s string) (Target, error) {
	provider, rest, ok := strings.Cut(s, ":")
	if !ok || provider == "" || rest == "" {
		return Target{}, fmt.Errorf("%q is not provider:model[@effort]", s)
	}
	t := Target{Provider: provider, Model: rest}
	if i := strings.LastIndex(rest, "@"); i > 0 {
		t.Model, t.Effort = rest[:i], rest[i+1:]
	}
	return t, nil
}
