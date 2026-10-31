package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/capability"
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
