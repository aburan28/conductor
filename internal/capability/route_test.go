package capability_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/capability"
)

func routeRegistry(t *testing.T, families string, cals ...capability.Calibration) *capability.Registry {
	t.Helper()
	dir := t.TempDir()
	if families != "" {
		if err := os.WriteFile(filepath.Join(dir, "families.json"), []byte(families), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := capability.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cals {
		if _, err := r.AddCalibration(c); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func cal(target capability.Target, passed, graded int, cost *float64, at string) capability.Calibration {
	return capability.Calibration{Suite: "cards-a", SuiteSHA256: "sha-a", Family: "card-compile", Target: target,
		Passed: passed, Graded: graded, CostUSD: cost, FinishedAt: at}
}

func usd(v float64) *float64 { return &v }

var (
	sonnet = capability.Target{Provider: "anthropic", Model: "claude-sonnet-5-5", Effort: "high"}
	haiku  = capability.Target{Provider: "anthropic", Model: "claude-haiku-5-5"}
	glm    = capability.Target{Provider: "vllm", Model: "glm-5.3"}
)

func allowAll(string) (bool, string) { return true, "" }

func TestRouteTakesTheCheapestModelEquivalentToTheBest(t *testing.T) {
	r := routeRegistry(t, "",
		cal(sonnet, 190, 200, usd(4), "2026-10-31T01:00:00Z"),
		cal(haiku, 188, 200, usd(1), "2026-10-31T02:00:00Z"), // equivalent within 0.05, cheaper
		cal(glm, 120, 200, usd(0.1), "2026-10-31T03:00:00Z"), // cheapest, but worse
	)
	if _, err := r.ImportEvidence([]capability.Evidence{evidence("deepseek-v4", "swe", 90, "2026-10-20")}, now); err != nil {
		t.Fatal(err)
	}
	route, err := r.Route("card-compile", allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if route.Choice != haiku.String() || route.Reference != sonnet.String() ||
		len(route.Escalation) != 1 || route.Escalation[0] != sonnet.String() {
		t.Fatalf("route = %+v", route)
	}
	if !strings.Contains(route.Basis, "188/200") || !strings.Contains(route.Basis, "equivalent to the reference") {
		t.Errorf("basis = %q", route.Basis)
	}
	why := map[string]capability.Considered{}
	for _, c := range route.Considered {
		why[c.Target] = c
	}
	if c := why[glm.String()]; c.Eligible || !strings.Contains(c.Why, "worse") {
		t.Errorf("glm = %+v", c)
	}
}

func TestRouteWithALevelUsesTheLowBoundInsteadOfEquivalence(t *testing.T) {
	r := routeRegistry(t, `{"families":{"card-compile":{"level":0.85}}}`,
		cal(sonnet, 190, 200, usd(4), "2026-10-31T01:00:00Z"),
		cal(haiku, 188, 200, usd(1), "2026-10-31T02:00:00Z"),
		cal(glm, 120, 200, usd(0.1), "2026-10-31T03:00:00Z"),
	)
	route, err := r.Route("card-compile", allowAll)
	if err != nil || route.Choice != haiku.String() || !strings.Contains(route.Basis, "meets level 0.85") {
		t.Fatalf("route = %+v, %v", route, err)
	}
	// Free mode (or a provider without credentials) can rule a model out.
	route, err = r.Route("card-compile", func(target string) (bool, string) {
		if strings.HasPrefix(target, "anthropic:claude-haiku") {
			return false, "free mode: a paid API"
		}
		return true, ""
	})
	if err != nil || route.Choice != sonnet.String() {
		t.Fatalf("with haiku ruled out: %+v, %v", route, err)
	}
}

func TestRouteRefusesWhenNothingQualifies(t *testing.T) {
	r := routeRegistry(t, `{"families":{"card-compile":{"level":0.99}}}`, cal(haiku, 150, 200, usd(1), "2026-10-31T02:00:00Z"))
	if _, err := r.Route("card-compile", allowAll); err == nil || !strings.Contains(err.Error(), "below the level") {
		t.Fatalf("err = %v", err)
	}
	empty := routeRegistry(t, "")
	if _, err := empty.Route("card-compile", allowAll); err == nil || !strings.Contains(err.Error(), "no calibrations") {
		t.Fatalf("empty registry: %v", err)
	}
}

func TestAnUnknownCostIsNeverTakenToBeCheaper(t *testing.T) {
	r := routeRegistry(t, `{"families":{"card-compile":{"level":0.5}}}`,
		cal(glm, 190, 200, nil, "2026-10-31T01:00:00Z"), // unpriced
		cal(haiku, 180, 200, usd(3), "2026-10-31T02:00:00Z"),
	)
	route, err := r.Route("card-compile", allowAll)
	if err != nil || route.Choice != haiku.String() || !strings.Contains(route.Basis, "$") {
		t.Fatalf("route = %+v, %v", route, err)
	}
}

func TestEscalationIsTheNextBetterModelThenTheBest(t *testing.T) {
	tg := func(m string) capability.Target { return capability.Target{Provider: "p", Model: m} }
	r := routeRegistry(t, `{"families":{"card-compile":{"level":0.5}}}`,
		cal(tg("a"), 150, 200, usd(1), "2026-10-31T01:00:00Z"),
		cal(tg("b"), 165, 200, usd(2), "2026-10-31T01:00:01Z"),
		cal(tg("c"), 180, 200, usd(3), "2026-10-31T01:00:02Z"),
		cal(tg("d"), 195, 200, usd(4), "2026-10-31T01:00:03Z"),
	)
	route, err := r.Route("card-compile", allowAll)
	if err != nil || route.Choice != "p:a" || strings.Join(route.Escalation, ",") != "p:b,p:d" {
		t.Fatalf("route = %+v, %v", route, err)
	}
}

func TestParseTargetReadsWhatStringWrites(t *testing.T) {
	for _, in := range []capability.Target{sonnet, haiku, {Provider: "openrouter", Model: "poolside/laguna-xs-2.1:free"}} {
		got, err := capability.ParseTarget(in.String())
		if err != nil || got != in {
			t.Errorf("ParseTarget(%q) = %+v, %v", in.String(), got, err)
		}
	}
	for _, bad := range []string{"", "nocolon", ":m", "p:"} {
		if _, err := capability.ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) accepted", bad)
		}
	}
}
