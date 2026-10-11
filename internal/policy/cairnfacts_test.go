package policy

import "testing"

func evalBool(t *testing.T, expr string, f Facts) bool {
	t.Helper()
	e, err := Compile(expr)
	if err != nil {
		t.Fatalf("compile %q: %v", expr, err)
	}
	got, err := e.Bool(f.Env())
	if err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	return got
}

// The rule this guards is the one an operator would actually write, and it is the one that
// would have been catastrophic: "the pool is spent, stop routing work at it". If an absent
// cairn fact rendered as zero, this would be true for every repository task in the project
// and the whole lane would go quiet for a reason nobody could see.
func TestAbsentCairnFactsSatisfyNoThreshold(t *testing.T) {
	repoTask := Facts{} // no Cairn block at all: an ordinary task

	for _, expr := range []string{
		"cairn.reward_remaining <= 0",
		"cairn.frontier_score >= 0",
		"cairn.frontier_score < 100",
		"cairn.settled",
	} {
		if evalBool(t, expr, repoTask) {
			t.Errorf("%q was true for a task with no cairn objective", expr)
		}
	}

	// The presence flag is the one fact that is meaningful when absent, so that a policy can
	// say "this is not cairn work" without comparing anything.
	if evalBool(t, "cairn.known", repoTask) {
		t.Error("cairn.known was true for a task with no cairn objective")
	}
	if !evalBool(t, "!cairn.known", repoTask) {
		t.Error("!cairn.known should read naturally for an ordinary task")
	}
}

func TestKnownCairnFactsCompareNormally(t *testing.T) {
	stalled := Facts{Cairn: CairnFacts{
		Known:           true,
		ObjectiveID:     "sha256:abc",
		FrontierScore:   12,
		RewardRemaining: 1100000,
	}}

	// A funded objective whose frontier is beatable: worth spending a better model on.
	if !evalBool(t, "cairn.known && cairn.reward_remaining > 0 && !cairn.settled", stalled) {
		t.Error("a funded, open objective should match the escalation rule")
	}
	if evalBool(t, "cairn.reward_remaining <= 0", stalled) {
		t.Error("a funded objective must not look exhausted")
	}
	if !evalBool(t, `cairn.objective_id == "sha256:abc"`, stalled) {
		t.Error("objective id should compare as a string")
	}

	// And an exhausted pool does stop it.
	spent := Facts{Cairn: CairnFacts{Known: true, RewardRemaining: 0, FrontierScore: 20}}
	if !evalBool(t, "cairn.reward_remaining <= 0", spent) {
		t.Error("an exhausted pool should match the stop rule")
	}
}

// Every name Env produces must be in KnownFacts, or `conductor policy lint` will tell an
// operator their working rule reads an unknown fact -- and every name in KnownFacts must be
// producible, or the linter will bless a rule that is always null.
func TestCairnFactsAreAllDeclared(t *testing.T) {
	env := Facts{Cairn: CairnFacts{Known: true}}.Env()
	declared := make(map[string]bool, len(KnownFacts))
	for _, name := range KnownFacts {
		declared[name] = true
	}
	for name := range env {
		if !declared[name] {
			t.Errorf("Env produces %q, which KnownFacts does not declare", name)
		}
	}
	for _, name := range KnownFacts {
		if _, ok := env[name]; !ok {
			t.Errorf("KnownFacts declares %q, which Env never produces", name)
		}
	}
}
