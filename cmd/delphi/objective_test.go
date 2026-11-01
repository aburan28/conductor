package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/capability"
	"github.com/aburan28/conductor/internal/objective"
)

func objCal(family, provider, model, effort string, passed int, cost float64) capability.Calibration {
	return capability.Calibration{Suite: family + "-v1", SuiteSHA256: "s-" + family, Family: family,
		Target: capability.Target{Provider: provider, Model: model, Effort: effort},
		Passed: passed, Graded: 200, CostUSD: &cost, FinishedAt: "2026-11-01T00:00:00Z"}
}

func objectiveRegistry(t *testing.T) string {
	reg := filepath.Join(t.TempDir(), "registry")
	t.Setenv("DELPHI_REGISTRY", reg)
	seedRegistry(t, reg,
		objCal("plan", "anthropic", "claude-sonnet-5-5", "high", 190, 4),
		objCal("summarize", "anthropic", "claude-sonnet-5-5", "high", 190, 4),
		objCal("summarize", "anthropic", "claude-haiku-5-5", "", 188, 1),
		objCal("review", "anthropic", "claude-opus-5-5", "high", 196, 9),
		objCal("review", "openai", "gpt-test", "", 195, 3))
	return reg
}

const twoWorkItems = `{"decomposable": true, "children": [
 {"kind": "work", "statement": "Summarize option A", "criteria": ["names the cost"], "family": "summarize", "budget_usd": 0.4},
 {"kind": "work", "statement": "Summarize option B", "criteria": ["names the risk"], "family": "summarize", "budget_usd": 0.4}]}`

func TestObjectiveDecomposesDelegatesReviewsIndependentlyAndEscalates(t *testing.T) {
	fa, fo := servers(t)
	objectiveRegistry(t)
	fa.mu.Lock()
	fa.outcome = func(id string) string {
		switch id {
		case "n0-plan-a0":
			return claudeMsg(twoWorkItems, "end_turn", "")
		case "n0_2-work-a0":
			return claudeMsg("B is fine", "end_turn", "")
		}
		return claudeMsg("answer "+id, "end_turn", "")
	}
	fa.mu.Unlock()
	fo.mu.Lock()
	fo.outcome = func(id string, _ map[string]any) (string, string) {
		verdict := `{"verdict":"accept","feedback":"meets the criteria"}`
		if id == "n0_2-review-a0" {
			verdict = `{"verdict":"reject","feedback":"does not name the risk"}`
		}
		return oaOK(id, verdict, "completed"), ""
	}
	fo.mu.Unlock()

	out := filepath.Join(t.TempDir(), "obj")
	code, stdout, se := cli(t, "", "objective", "init", "-out", out, "-objective", "Compare options A and B",
		"-criterion", "both options are covered", "-budget-usd", "1")
	if code != 0 || !strings.Contains(stdout, "plan -> anthropic:claude-sonnet-5-5@high") {
		t.Fatalf("init: %d\n%s\n%s", code, stdout, se)
	}
	if code, stdout, se := cli(t, "", "objective", "run", "-out", out, "-every", "1ms"); code != 0 {
		t.Fatalf("run: %d\n%s\n%s", code, stdout, se)
	}
	l, err := objective.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	tr := l.Tree()
	root, a, b := tr.Get("n0"), tr.Get("n0.1"), tr.Get("n0.2")
	if root.State != objective.Accepted || a.State != objective.Accepted || b.State != objective.Accepted {
		t.Fatalf("states: root %s (%s), a %s, b %s (%s)", root.State, root.Reason, a.State, b.State, b.Reason)
	}
	// The work went to the cheapest equivalent model; the rejected one climbed its route.
	if a.Attempts[0].Target != "anthropic:claude-haiku-5-5" || len(b.Attempts) != 2 ||
		b.Attempts[1].Target != "anthropic:claude-sonnet-5-5@high" || !strings.Contains(b.Attempts[0].Outcome, "does not name the risk") {
		t.Fatalf("attempts: a %+v b %+v", a.Attempts, b.Attempts)
	}
	// Reviews are independent: Claude-written answers go to the non-Anthropic reviewer.
	for _, n := range []*objective.Node{root, a, b} {
		if r := n.Current().Reviewer; r != "openai:gpt-test" {
			t.Errorf("%s reviewed by %q", n.ID, r)
		}
	}
	// Review costs are priced and rolled up to the root.
	if !(root.SpentUSD > 0) || root.SpentTokens == 0 || root.SpentUSD < a.SpentUSD+b.SpentUSD {
		t.Errorf("spend: root $%v (%d tokens), a $%v, b $%v", root.SpentUSD, root.SpentTokens, a.SpentUSD, b.SpentUSD)
	}
	if !strings.Contains(root.Answer, "## n0.1: Summarize option A") {
		t.Errorf("root synthesis = %q", root.Answer)
	}
	report, err := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil || !strings.Contains(string(report), "# Objective: accepted") {
		t.Fatalf("report: %v\n%s", err, report)
	}
	// Resume asks nobody anything again.
	fa.mu.Lock()
	before := len(fa.bodies)
	fa.mu.Unlock()
	if code, _, se := cli(t, "", "objective", "run", "-out", out, "-every", "1ms"); code != 0 {
		t.Fatalf("resume: %d %s", code, se)
	}
	fa.mu.Lock()
	after := len(fa.bodies)
	fa.mu.Unlock()
	if after != before {
		t.Errorf("resume submitted %d new requests", after-before)
	}
	code, stdout, _ = cli(t, "", "objective", "status", "-out", out)
	if code != 0 || !strings.Contains(stdout, "objective accepted") || !strings.Contains(stdout, "n0.2") {
		t.Errorf("status: %d\n%s", code, stdout)
	}
	code, stdout, _ = cli(t, "", "objective", "status", "-out", out, "-json")
	var st struct {
		Summary map[string]int `json:"summary"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &st) != nil || st.Summary["accepted"] != 3 {
		t.Errorf("status -json: %d %s", code, stdout)
	}
}

func TestObjectiveFailsWhenEveryPlannerReturnsAnInvalidDecomposition(t *testing.T) {
	fa, _ := servers(t)
	objectiveRegistry(t)
	fa.mu.Lock()
	fa.outcome = func(id string) string {
		// Over budget, an unroutable family, and a child with no criteria.
		return claudeMsg(`{"decomposable": true, "children": [
 {"kind": "work", "statement": "x", "criteria": [], "family": "summarize", "budget_usd": 5},
 {"kind": "work", "statement": "y", "criteria": ["z"], "family": "nosuch", "budget_usd": 0.1}]}`, "end_turn", "")
	}
	fa.mu.Unlock()
	out := filepath.Join(t.TempDir(), "obj")
	if code, _, se := cli(t, "", "objective", "init", "-out", out, "-objective", "Do it", "-criterion", "done", "-budget-usd", "1"); code != 0 {
		t.Fatalf("init: %d %s", code, se)
	}
	if code, _, _ := cli(t, "", "objective", "run", "-out", out, "-every", "1ms"); code != exitFailures {
		t.Fatalf("run exit %d, want failures", code)
	}
	l, _ := objective.Load(out)
	root := l.Tree().Get("n0")
	if root.State != objective.Failed || len(root.Attempts) != 1 || len(root.Children) != 0 {
		t.Fatalf("root: %+v", root)
	}
	for _, want := range []string{"no acceptance criteria", `family "nosuch" has no model`, "more than the $1.0000 left"} {
		if !strings.Contains(root.Reason, want) {
			t.Errorf("reason %q lacks %q", root.Reason, want)
		}
	}
}

func TestObjectiveInitRefusesWhatCannotBeChecked(t *testing.T) {
	servers(t)
	objectiveRegistry(t)
	dir := t.TempDir()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-objective", "x", "-budget-usd", "1"}, "at least one -criterion"},
		{[]string{"-objective", "x", "-criterion", "y"}, "-budget-usd must be positive"},
		{[]string{"-objective", "x", "-criterion", "y", "-budget-usd", "1", "-review-family", ""}, "needs a check"},
		{[]string{"-objective", "x", "-criterion", "y", "-budget-usd", "1", "-plan-family", "nosuch"}, "family nosuch"},
	} {
		args := append([]string{"objective", "init", "-out", filepath.Join(dir, c.want)}, c.args...)
		if code, _, se := cli(t, "", args...); code == 0 || !strings.Contains(se, c.want) {
			t.Errorf("%v: exit %d, %s", c.args, code, se)
		}
	}
}

func TestObjectiveFailureAbandonsSiblingsAndFailsTheParent(t *testing.T) {
	fa, fo := servers(t)
	objectiveRegistry(t)
	fa.mu.Lock()
	fa.outcome = func(id string) string {
		if id == "n0-plan-a0" {
			return claudeMsg(twoWorkItems, "end_turn", "")
		}
		return claudeMsg("answer "+id, "end_turn", "")
	}
	fa.mu.Unlock()
	fo.mu.Lock()
	fo.outcome = func(id string, _ map[string]any) (string, string) {
		if strings.HasPrefix(id, "n0_2-") {
			return oaOK(id, `{"verdict":"reject","feedback":"still wrong"}`, "completed"), ""
		}
		return oaOK(id, `{"verdict":"accept","feedback":"ok"}`, "completed"), ""
	}
	fo.mu.Unlock()
	out := filepath.Join(t.TempDir(), "obj")
	if code, _, se := cli(t, "", "objective", "init", "-out", out, "-objective", "Compare", "-criterion", "c", "-budget-usd", "1"); code != 0 {
		t.Fatalf("init: %d %s", code, se)
	}
	if code, _, _ := cli(t, "", "objective", "run", "-out", out, "-every", "1ms"); code != exitFailures {
		t.Fatalf("run exit %d", code)
	}
	l, _ := objective.Load(out)
	tr := l.Tree()
	b, root := tr.Get("n0.2"), tr.Get("n0")
	// Both models in n0.2's route were tried and rejected, then it failed, and so did the root.
	if b.State != objective.Failed || len(b.Attempts) != 2 || !strings.Contains(b.Reason, "all 2 attempt(s) rejected") {
		t.Fatalf("n0.2: %s %q %+v", b.State, b.Reason, b.Attempts)
	}
	if root.State != objective.Failed || !strings.Contains(root.Reason, "n0.2 failed") {
		t.Fatalf("root: %s %q", root.State, root.Reason)
	}
	if a := tr.Get("n0.1"); a.State != objective.Accepted && a.State != objective.Abandoned {
		t.Fatalf("n0.1 left %s", a.State)
	}
}

func TestObjectiveNeverLetsAVendorReviewItsOwnAnswer(t *testing.T) {
	fa, _ := servers(t)
	reg := filepath.Join(t.TempDir(), "registry")
	t.Setenv("DELPHI_REGISTRY", reg)
	seedRegistry(t, reg,
		objCal("plan", "anthropic", "claude-sonnet-5-5", "high", 190, 4),
		objCal("review", "anthropic", "claude-opus-5-5", "high", 196, 9)) // the only reviewer is Claude
	fa.mu.Lock()
	fa.outcome = func(id string) string {
		return claudeMsg(`{"decomposable": true, "children": [{"kind": "work", "statement": "s", "criteria": ["c"], "family": "plan", "budget_usd": 0.5}]}`, "end_turn", "")
	}
	fa.mu.Unlock()
	out := filepath.Join(t.TempDir(), "obj")
	if code, _, se := cli(t, "", "objective", "init", "-out", out, "-objective", "x", "-criterion", "c", "-budget-usd", "1"); code != 0 {
		t.Fatalf("init: %d %s", code, se)
	}
	cli(t, "", "objective", "run", "-out", out, "-every", "1ms")
	l, _ := objective.Load(out)
	n := l.Tree().Get("n0.1")
	if n.State != objective.Failed || !strings.Contains(n.Reason, "no independent reviewer") || !strings.Contains(n.Reason, "same vendor") {
		t.Fatalf("n0.1: %s %q", n.State, n.Reason)
	}
}
