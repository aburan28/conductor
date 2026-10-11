package policy

import (
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/domain"
)

// The rules this guards are the ones an operator would write first, and the ones that would
// have been catastrophic had absence rendered as zero. "Three epochs without an advance,
// escalate" would have stayed quiet -- zero is not three -- but "a fresh advance, stay cheap"
// (`bounds.epochs_without_advance <= 0`) would have matched every task in the project, and
// "nothing left to beat" (`bounds.frontier_entries == 0`) every task in a project that reads
// no frontier. Absent facts are nil, and nil satisfies nothing.
func TestAbsentBoundsFactsSatisfyNoThreshold(t *testing.T) {
	repoTask := Facts{} // no Bounds block at all: an ordinary task

	for _, expr := range []string{
		"bounds.epochs_without_advance >= 3",
		"bounds.epochs_without_advance <= 0",
		"bounds.epochs_without_advance >= 0",
		"bounds.challenge_epochs < 100",
		"bounds.challenge_epochs == 0",
		"bounds.frontier_entries == 0",
		"bounds.frontier_entries >= 0",
		"bounds.domain_count > 0",
		`bounds.last_outcome == "regresses"`,
		`bounds.last_outcome == "advances"`,
		`bounds.frontier_id == "ECFR1h8b45b672c283"`,
	} {
		if evalBool(t, expr, repoTask) {
			t.Errorf("%q was true for a task with no bounds facts", expr)
		}
	}

	// The two presence flags are the facts that mean something when absent, so that a policy
	// can say "this is not bounds work" or "nobody has run this challenge" without comparing
	// anything.
	for _, flag := range []string{"bounds.known", "bounds.challenge_known"} {
		if evalBool(t, flag, repoTask) {
			t.Errorf("%s was true for a task with no bounds facts", flag)
		}
		if !evalBool(t, "!"+flag, repoTask) {
			t.Errorf("!%s should read naturally for an ordinary task", flag)
		}
	}
}

// A frontier the project reads is known to every task; the challenge names stay absent for a
// task that works no challenge, or one nobody has run yet. Neither may look stalled: a
// challenge with no verdicts has no epochs, not zero epochs.
func TestFrontierWithoutChallengeIsNotStalled(t *testing.T) {
	frontier := BoundsFacts{Known: true, FrontierID: "ECFR1h8b45b672c283", DomainCount: 3, FrontierEntries: 13}
	fresh := Facts{Bounds: frontier}

	if !evalBool(t, "bounds.known && !bounds.challenge_known", fresh) {
		t.Error("a frontier with no challenge should read as known, challenge unknown")
	}
	if !evalBool(t, `bounds.frontier_id == "ECFR1h8b45b672c283" && bounds.domain_count == 3 && bounds.frontier_entries >= 13`, fresh) {
		t.Error("frontier facts should compare normally once a frontier is read")
	}
	for _, expr := range []string{
		"bounds.challenge_known && bounds.epochs_without_advance >= 3",
		"bounds.epochs_without_advance >= 0",
		"bounds.challenge_epochs >= 0",
		`bounds.last_outcome == "inadmissible"`,
	} {
		if evalBool(t, expr, fresh) {
			t.Errorf("%q was true for a challenge nobody has run", expr)
		}
	}

	// The hold rule is for bounds work in a project that reads no frontier. A project that
	// reads one must not hold, and -- the label guard being load-bearing, since bounds.known
	// is false for every task in a project without a frontier -- an unlabelled task must not
	// be held either.
	hold := `task.labels has "bounds" && !bounds.known`
	labelled := Facts{Task: domain.Task{Labels: []string{"bounds"}, ExternalRef: "ECCH1h91fd655dfb54"}, Bounds: frontier}
	if evalBool(t, hold, labelled) {
		t.Error("bounds work in a project with a frontier must not be held")
	}
	if !evalBool(t, hold, Facts{Task: domain.Task{Labels: []string{"bounds"}}}) {
		t.Error("bounds work in a project with no frontier should be held")
	}
	if evalBool(t, hold, Facts{}) {
		t.Error("an ordinary task must not be held for lacking a frontier it never asked about")
	}
}

func TestKnownBoundsFactsCompareNormally(t *testing.T) {
	stalled := Facts{Bounds: BoundsFacts{
		Known: true, FrontierID: "ECFR1h8b45b672c283", DomainCount: 3, FrontierEntries: 13,
		ChallengeKnown: true, ChallengeEpochs: 4, EpochsWithoutAdvance: 3, LastOutcome: "regresses",
	}}

	// Three epochs past its last advance: worth spending a better model on.
	if !evalBool(t, "bounds.challenge_known && bounds.epochs_without_advance >= 3", stalled) {
		t.Error("a challenge three epochs past its last advance should match the escalation rule")
	}
	if !evalBool(t, `bounds.last_outcome == "regresses" && bounds.challenge_epochs == 4`, stalled) {
		t.Error("outcome should compare as a string and epochs as a number")
	}
	if evalBool(t, `bounds.last_outcome == "advances"`, stalled) {
		t.Error("a regressing challenge must not look like it just advanced")
	}

	// A fresh advance resets the count and stops the escalation; zero is a value, not an
	// absence, once the challenge is known.
	advanced := Facts{Bounds: BoundsFacts{Known: true, ChallengeKnown: true, ChallengeEpochs: 2, LastOutcome: "advances"}}
	if evalBool(t, "bounds.epochs_without_advance >= 3", advanced) {
		t.Error("a challenge that just advanced must not match the escalation rule")
	}
	if !evalBool(t, `bounds.last_outcome == "advances" && bounds.epochs_without_advance == 0`, advanced) {
		t.Error("a known challenge with no epochs since its advance should read as zero")
	}
}

// Every name Env produces must be in KnownFacts, or `conductor policy lint` will tell an
// operator their working rule reads an unknown fact -- and every name in KnownFacts must be
// producible, or the linter will bless a rule that is always null.
func TestBoundsFactsAreAllDeclared(t *testing.T) {
	env := Facts{Bounds: BoundsFacts{Known: true, ChallengeKnown: true}}.Env()
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
	// And the eight names this file is about are among them, so that a rename in one place
	// is caught by the other.
	for _, name := range []string{
		"bounds.known", "bounds.frontier_id", "bounds.domain_count", "bounds.frontier_entries",
		"bounds.challenge_known", "bounds.challenge_epochs", "bounds.epochs_without_advance",
		"bounds.last_outcome",
	} {
		if !declared[name] {
			t.Errorf("KnownFacts does not declare %q", name)
		}
		if _, ok := env[name]; !ok {
			t.Errorf("Env does not produce %q", name)
		}
	}
}

// The two rules .conductor/dispatch.yaml ships for bounds, as written there, lint clean. If
// the expressions move there, move them here.
func TestShippedBoundsRulesLintClean(t *testing.T) {
	p := samplePolicy()
	p.Rules = append(p.Rules,
		domain.DispatchRule{ID: "bounds-stalled-escalate",
			When:    "bounds.challenge_known && bounds.epochs_without_advance >= 3",
			Require: &domain.CapabilityRequirement{Tier: domain.TierT3}},
		domain.DispatchRule{ID: "bounds-unknown-frontier-hold",
			When:   `task.labels has "bounds" && !bounds.known`,
			Prefer: &domain.DispatchSelector{Tag: "local"}},
	)
	_, issues := CompileDispatch(p)
	for _, i := range issues {
		if strings.HasPrefix(i.Where, "rule bounds-") {
			t.Errorf("shipped bounds rule does not lint clean: %s", i)
		}
	}
}
