package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/capability"
	"github.com/aburan28/conductor/internal/delphi"
)

func seedRegistry(t *testing.T, dir string, cals ...capability.Calibration) {
	t.Helper()
	r, err := capability.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cals {
		if _, err := r.AddCalibration(c); err != nil {
			t.Fatal(err)
		}
	}
}

func cardCal(provider, model, effort string, passed int, cost float64, at string) capability.Calibration {
	return capability.Calibration{Suite: "card-compile-v1", SuiteSHA256: "s1", Family: "card-compile",
		Target: capability.Target{Provider: provider, Model: model, Effort: effort},
		Passed: passed, Graded: 200, CostUSD: &cost, FinishedAt: at}
}

func TestRouteSendsAFamilyToTheCheapestEquivalentModel(t *testing.T) {
	fa, _ := servers(t)
	reg := filepath.Join(t.TempDir(), "registry")
	t.Setenv("DELPHI_REGISTRY", reg)
	seedRegistry(t, reg,
		cardCal("anthropic", "claude-sonnet-5-5", "high", 190, 4, "2026-10-31T01:00:00Z"),
		cardCal("anthropic", "claude-haiku-5-5", "", 188, 1, "2026-10-31T02:00:00Z"))

	code, stdout, stderr := cli(t, "", "route", "-family", "card-compile")
	if code != 0 || !strings.Contains(stdout, "card-compile -> anthropic:claude-haiku-5-5") ||
		!strings.Contains(stdout, "escalates to: anthropic:claude-sonnet-5-5@high") {
		t.Fatalf("route: exit %d\n%s\n%s", code, stdout, stderr)
	}

	out := filepath.Join(t.TempDir(), "run")
	code, stdout, stderr = cli(t, "", "run", "-route", "card-compile", "-prompt", "compile this", "-out", out, "-every", "1ms")
	if code != 0 {
		t.Fatalf("run -route: exit %d\n%s\n%s", code, stdout, stderr)
	}
	fa.mu.Lock()
	body, _ := json.Marshal(fa.bodies)
	fa.mu.Unlock()
	if !strings.Contains(string(body), `"model":"claude-haiku-5-5"`) {
		t.Errorf("the routed request did not go to haiku: %s", body)
	}
	rows := lines(t, filepath.Join(out, "results.jsonl"))
	meta := rows[0]["meta"].(map[string]any)
	if meta["route_family"] != "card-compile" || !strings.Contains(meta["route_basis"].(string), "188/200") {
		t.Errorf("result meta = %v", meta)
	}

	if code, _, stderr := cli(t, "", "run", "-route", "card-compile", "-model", "x", "-prompt", "p",
		"-out", filepath.Join(t.TempDir(), "r2")); code == 0 || !strings.Contains(stderr, "drop -model") {
		t.Errorf("-route with -model: exit %d %s", code, stderr)
	}
}

func TestRouteInFreeModeOnlyChoosesFreeModels(t *testing.T) {
	fa, _ := servers(t)
	local, served := localModel(t)
	home := os.Getenv("DELPHI_HOME")
	os.WriteFile(filepath.Join(home, "endpoints.json"), []byte(`{"vllm":{"base_url":"`+local.URL+`/v1"}}`), 0o600)
	reg := filepath.Join(t.TempDir(), "registry")
	t.Setenv("DELPHI_REGISTRY", reg)
	t.Setenv("DELPHI_FREE", "local")
	seedRegistry(t, reg, cardCal("anthropic", "claude-sonnet-5-5", "high", 190, 4, "2026-10-31T01:00:00Z"))

	// Only a paid model is calibrated: nothing qualifies, and the reason says so.
	code, _, stderr := cli(t, "", "route", "-family", "card-compile")
	if code == 0 || !strings.Contains(stderr, "free mode") {
		t.Fatalf("free route with only paid models: exit %d %s", code, stderr)
	}

	// A local model measured equivalent is chosen, and answers.
	seedRegistry(t, reg, cardCal("vllm", "glm-5.3", "", 189, 0, "2026-10-31T03:00:00Z"))
	out := filepath.Join(t.TempDir(), "run")
	if code, stdout, stderr := cli(t, "", "run", "-route", "card-compile", "-prompt", "p", "-out", out, "-every", "1ms"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if len(*served) != 1 || (*served)[0] != "glm-5.3" {
		t.Errorf("local endpoint served %v", *served)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.bodies) != 0 {
		t.Errorf("free routing sent %d request(s) to the paid API", len(fa.bodies))
	}
}

func famCal(family, model, effort string, passed int, cost float64, at string) capability.Calibration {
	c := cardCal("anthropic", model, effort, passed, cost, at)
	c.Suite, c.Family = family+"-v1", family
	return c
}

func TestGoalRouteSendsEachSideToItsFamilyAndEscalatesAfterARejectedRound(t *testing.T) {
	fa, _ := servers(t)
	reg := filepath.Join(t.TempDir(), "registry")
	t.Setenv("DELPHI_REGISTRY", reg)
	seedRegistry(t, reg,
		famCal("plan", "claude-sonnet-5-5", "high", 190, 4, "2026-10-31T01:00:00Z"),
		famCal("plan", "claude-haiku-5-5", "", 188, 1, "2026-10-31T02:00:00Z"),
		famCal("review", "claude-opus-5-5", "high", 196, 9, "2026-10-31T03:00:00Z"))
	fa.mu.Lock()
	fa.outcome = func(id string) string {
		switch id {
		case "goal-r1-review":
			return claudeMsg(`{"verdict":"revise","feedback":"add the missing check"}`, "end_turn", "")
		case "goal-r2-review":
			return claudeMsg(`{"verdict":"accept","feedback":"done"}`, "end_turn", "")
		}
		return claudeMsg("text for "+id, "end_turn", "")
	}
	fa.mu.Unlock()

	out := filepath.Join(t.TempDir(), "goal")
	code, stdout, se := cli(t, "", "goal", "init", "-out", out, "-goal", "Assess an option", "-rounds", "3",
		"-route", "plan", "-reviewer-route", "review")
	if code != 0 || !strings.Contains(stdout, "plan -> anthropic:claude-haiku-5-5") ||
		!strings.Contains(stdout, "review -> anthropic:claude-opus-5-5@high") {
		t.Fatalf("init: %d\n%s\n%s", code, stdout, se)
	}
	if code, _, se := cli(t, "", "goal", "run", "-out", out, "-every", "1ms"); code != 0 {
		t.Fatalf("run: %d %s", code, se)
	}
	fa.mu.Lock()
	var got []string
	for _, b := range fa.bodies {
		p := b["params"].(map[string]any)
		got = append(got, p["model"].(string))
	}
	fa.mu.Unlock()
	want := []string{"claude-haiku-5-5", "claude-haiku-5-5", "claude-opus-5-5", // round 1
		"claude-sonnet-5-5", "claude-sonnet-5-5", "claude-opus-5-5"} // round 2, escalated
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %v, want %v", got, want)
	}
	s, err := loadGoal(out)
	if err != nil || s.Status != "self_reviewed" || s.Route.Step != 1 ||
		s.History[0].EscalatedTo != "anthropic:claude-sonnet-5-5@high" || s.Effort != "high" {
		t.Fatalf("state: %+v %v", s, err)
	}
	st, err := delphi.OpenFileStore(out)
	if err != nil {
		t.Fatal(err)
	}
	for id, family := range map[string]string{"goal-r1-plan": "plan", "goal-r2-review": "review"} {
		r, found, err := st.Result(id)
		if err != nil || !found || r.Meta["route_family"] != family || r.Meta["route_basis"] == "" {
			t.Errorf("%s meta = %v (found %t, %v)", id, r.Meta, found, err)
		}
	}
}

func TestGoalRouteStaysAtTheTopOfItsChainAndRefusesExplicitModels(t *testing.T) {
	s := goalState{Stage: "review", Round: 1, MaxRounds: 3, Provider: "anthropic", Model: "m1",
		Route: &goalRoute{Family: "plan", Chain: []string{"anthropic:m1"}}}
	r := delphi.Result{Status: delphi.Succeeded, Text: `{"verdict":"revise","feedback":"again"}`}
	if err := nextGoal(&s, r); err != nil || s.Round != 2 || s.Model != "m1" || s.History[0].EscalatedTo != "" {
		t.Fatalf("top of chain: %+v %v", s, err)
	}
	reg := filepath.Join(t.TempDir(), "registry")
	t.Setenv("DELPHI_REGISTRY", reg)
	seedRegistry(t, reg, famCal("plan", "claude-haiku-5-5", "", 188, 1, "2026-10-31T02:00:00Z"))
	code, _, se := cli(t, "", "goal", "init", "-out", filepath.Join(t.TempDir(), "g"), "-goal", "x",
		"-route", "plan", "-model", "claude-opus-5-5")
	if code != exitUsage || !strings.Contains(se, "drop -model") {
		t.Fatalf("explicit model with -route: %d %s", code, se)
	}
	code, _, se = cli(t, "", "goal", "init", "-out", filepath.Join(t.TempDir(), "h"), "-goal", "x", "-route", "nosuch")
	if code == 0 || !strings.Contains(se, "no calibrations") {
		t.Fatalf("unroutable family: %d %s", code, se)
	}
}
