package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/capability"
)

func TestCalibrateRecordsGradedRunsAndTheTableComparesThem(t *testing.T) {
	fa, fo := servers(t)
	answers := map[string]string{"q1": "42", "q2": "41", "q3": "7"} // q2 is wrong
	fa.mu.Lock()
	fa.outcome = func(id string) string { return claudeMsg(answers[id], "end_turn", "") }
	fa.mu.Unlock()
	fo.mu.Lock()
	fo.outcome = func(id string, _ map[string]any) (string, string) {
		right := map[string]string{"q1": "42", "q2": "42", "q3": "7"}
		return oaOK(id, right[id], "completed"), ""
	}
	fo.mu.Unlock()
	dir := t.TempDir()
	suite := filepath.Join(dir, "arith.json")
	os.WriteFile(suite, []byte(`{"schema":"conductor.capability.suite.v1","id":"arith","family":"math",
		"system":"Answer with the number only.","max_tokens":64,"grader":{"kind":"exact"},
		"items":[{"id":"q1","prompt":"6*7","expect":"42"},{"id":"q2","prompt":"40+2","expect":"42"},{"id":"q3","prompt":"3+4","expect":"7"}]}`), 0o644)
	reg := filepath.Join(dir, "registry")

	code, stdout, stderr := cli(t, "", "calibrate", "-suite", suite, "-registry", reg, "-provider", "anthropic",
		"-model", "claude-haiku-5-5", "-every", "1ms")
	if code != 0 || !strings.Contains(stdout, "2/3 passed") {
		t.Fatalf("anthropic calibrate: exit %d\n%s\n%s", code, stdout, stderr)
	}
	// The suite's system prompt and token limit reached the provider.
	fa.mu.Lock()
	body := fa.bodies[0]
	fa.mu.Unlock()
	if b, _ := json.Marshal(body); !strings.Contains(string(b), "Answer with the number only.") || !strings.Contains(string(b), `"max_tokens":64`) {
		t.Errorf("anthropic request body: %s", b)
	}
	code, stdout, stderr = cli(t, "", "calibrate", "-suite", suite, "-registry", reg, "-provider", "openai",
		"-model", "gpt-test", "-every", "1ms")
	if code != 0 || !strings.Contains(stdout, "3/3 passed") {
		t.Fatalf("openai calibrate: exit %d\n%s\n%s", code, stdout, stderr)
	}

	r, err := capability.Open(reg)
	if err != nil || len(r.Calibrations) != 2 {
		t.Fatalf("registry: %d calibrations, %v", len(r.Calibrations), err)
	}
	for _, c := range r.Calibrations {
		if c.Family != "math" || c.Graded != 3 || len(c.Grades) != 3 || len(c.SuiteSHA256) != 64 || c.RunDir == "" {
			t.Errorf("record = %+v", c)
		}
		for _, g := range c.Grades {
			if len(g.AnswerSHA256) != 64 {
				t.Errorf("grade without an answer digest: %+v", g)
			}
		}
	}

	evidence := filepath.Join(dir, "evidence.json")
	os.WriteFile(evidence, []byte(`[{"schema":"conductor.capability.evidence.v1","model":"glm-5.3","benchmark":"gsm-like",
		"version":"2026-10","score":81,"scale":"percent","source_url":"https://example.org/board","retrieved":"2026-10-01","self_reported":true}]`), 0o644)
	os.WriteFile(filepath.Join(reg, "families.json"), []byte(`{"benchmarks":{"gsm-like":{"math":1}}}`), 0o644)
	if code, stdout, stderr := cli(t, "", "registry", "import", "-registry", reg, "-file", evidence); code != 0 ||
		!strings.Contains(stdout, "imported 1") {
		t.Fatalf("import: exit %d\n%s\n%s", code, stdout, stderr)
	}

	code, stdout, stderr = cli(t, "", "registry", "table", "-registry", reg, "-family", "math", "-json")
	if code != 0 {
		t.Fatalf("table: exit %d\n%s", code, stderr)
	}
	var tables []capability.Table
	if err := json.Unmarshal([]byte(stdout), &tables); err != nil || len(tables) != 1 {
		t.Fatalf("table json: %v\n%s", err, stdout)
	}
	tab := tables[0]
	if tab.Reference != "openai:gpt-test" || len(tab.Rows) != 3 {
		t.Fatalf("table = %+v", tab)
	}
	// Three items cannot separate 3/3 from 2/3: the verdict must say so.
	if v := tab.Rows[1]; v.Target != "anthropic:claude-haiku-5-5" || v.Verdict != capability.Inconclusive {
		t.Errorf("second row = %+v", v)
	}
	if ref := tab.Rows[0]; ref.CostPer == nil || math.Abs(*ref.CostPer-0.0009) > 1e-9 {
		t.Errorf("openai cost per accepted answer = %v, want 0.0009 (100 in at $1, 200 out at $4 per MTok)", ref.CostPer)
	}
	// Haiku is priced as requested ($0.05 / $0.25 per MTok batch), though the fake answers as
	// another model: 3 answers of 1000 in + 2000 out tokens over 2 accepted.
	if h := tab.Rows[1]; h.CostPer == nil || math.Abs(*h.CostPer-3*(1000*0.05+2000*0.25)/1e6/2) > 1e-12 {
		t.Errorf("haiku cost per accepted answer = %v", h.CostPer)
	}
	if p := tab.Rows[2]; p.Label != "prior-only" || p.Model != "glm-5.3" {
		t.Errorf("prior-only row = %+v", p)
	}
	// The text table prints every row.
	code, stdout, _ = cli(t, "", "registry", "table", "-registry", reg)
	t.Log(stdout)
	if code != 0 ||
		!strings.Contains(stdout, "reference") || !strings.Contains(stdout, "inconclusive") || !strings.Contains(stdout, "prior-only") {
		t.Errorf("text table:\n%s", stdout)
	}
}

func TestCalibrateRefusesAnUnusableSuiteBeforeSpending(t *testing.T) {
	fa, _ := servers(t)
	suite := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(suite, []byte(`{"schema":"conductor.capability.suite.v1","id":"bad","family":"math","grader":{"kind":"exact"},"items":[{"id":"q1","prompt":"6*7"}]}`), 0o644)
	if code, _, stderr := cli(t, "", "calibrate", "-suite", suite, "-provider", "anthropic", "-model", "m"); code == 0 ||
		!strings.Contains(stderr, "needs expect") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.bodies) != 0 {
		t.Errorf("%d requests were sent for an invalid suite", len(fa.bodies))
	}
}
