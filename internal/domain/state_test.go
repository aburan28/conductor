package domain

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// DESIGN.md §32.1 requires unit tests for the transition guards. These are the cheapest tests
// in the system and they protect the invariant that everything else assumes: that a task
// cannot reach a state it has no legal path to.

func TestTaskTransitionsFollowDesign(t *testing.T) {
	legal := [][2]TaskStatus{
		{TaskProposed, TaskReady},
		{TaskProposed, TaskCancelled},
		{TaskReady, TaskClaimed},
		{TaskClaimed, TaskRunning},
		{TaskClaimed, TaskReady}, // lease expired before the attempt started
		{TaskRunning, TaskBlockedConflict},
		{TaskBlockedConflict, TaskRunning}, // blocker cleared, same lease still valid
		{TaskBlockedConflict, TaskReady},   // blocker cleared after the lease was released
		{TaskRunning, TaskVerifying},
		{TaskVerifying, TaskRunning}, // fixable validation failure
		{TaskVerifying, TaskReviewRequired},
		{TaskVerifying, TaskDone},
		{TaskReviewRequired, TaskRunning}, // changes requested
		{TaskReviewRequired, TaskMerging},
		{TaskMerging, TaskDone},
		{TaskRunning, TaskFailed},
		{TaskFailed, TaskReady}, // operator retry
	}
	for _, tc := range legal {
		if !CanTransitionTask(tc[0], tc[1]) {
			t.Errorf("%s -> %s should be legal", tc[0], tc[1])
		}
	}

	illegal := [][2]TaskStatus{
		{TaskReady, TaskDone},       // no work can be done without being claimed
		{TaskReady, TaskRunning},    // must pass through claimed
		{TaskProposed, TaskClaimed}, // must be validated first
		{TaskDone, TaskRunning},     // terminal
		{TaskCancelled, TaskReady},  // terminal
		{TaskSuperseded, TaskReady}, // terminal
		{TaskClaimed, TaskDone},     // skips verification entirely
		{TaskFailed, TaskDone},      // a failure cannot become a success without a retry
	}
	for _, tc := range illegal {
		if CanTransitionTask(tc[0], tc[1]) {
			t.Errorf("%s -> %s should be illegal", tc[0], tc[1])
		}
	}
}

func TestTerminalStatesHaveNoExits(t *testing.T) {
	for _, terminal := range []TaskStatus{TaskDone, TaskCancelled, TaskSuperseded} {
		for _, to := range AllTaskStatuses {
			if to == terminal {
				continue // self-transition is an idempotent re-assertion
			}
			if CanTransitionTask(terminal, to) {
				t.Errorf("terminal %s should not transition to %s", terminal, to)
			}
		}
	}
}

func TestSelfTransitionIsIdempotent(t *testing.T) {
	for _, s := range AllTaskStatuses {
		if !CanTransitionTask(s, s) {
			t.Errorf("%s -> %s (no-op) should be permitted", s, s)
		}
	}
	for _, s := range AllAttemptStates {
		if !CanTransitionAttempt(s, s) {
			t.Errorf("attempt %s -> %s (no-op) should be permitted", s, s)
		}
	}
}

func TestAssertTransitionReturnsSentinel(t *testing.T) {
	err := AssertTaskTransition(TaskReady, TaskDone)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("err = %v, want ErrIllegalTransition", err)
	}
	if !strings.Contains(err.Error(), "ready") || !strings.Contains(err.Error(), "done") {
		t.Errorf("error should name both states, got %q", err)
	}
	if err := AssertTaskTransition(TaskReady, TaskClaimed); err != nil {
		t.Errorf("legal transition returned %v", err)
	}
}

func TestAttemptTerminalStates(t *testing.T) {
	terminal := []AttemptState{
		AttemptSucceeded, AttemptFailedRetryable, AttemptFailedTerminal,
		AttemptCancelled, AttemptStale,
	}
	for _, s := range terminal {
		if s.IsLive() {
			t.Errorf("%s should not be live", s)
		}
		for _, to := range AllAttemptStates {
			if to != s && CanTransitionAttempt(s, to) {
				t.Errorf("terminal attempt %s should not transition to %s", s, to)
			}
		}
	}

	// A runner can fail between claiming and preparing a workspace; that must be retryable.
	if !CanTransitionAttempt(AttemptQueued, AttemptFailedRetryable) {
		t.Error("queued -> failed_retryable must be legal so an early runner failure requeues")
	}
}

// TaskStatusAfterAttempt owns the retry budget, so both the scheduler and the API agree on
// when a task is exhausted.
func TestTaskStatusAfterAttempt(t *testing.T) {
	cases := []struct {
		attempt   AttemptState
		used, max int
		want      TaskStatus
	}{
		{AttemptSucceeded, 1, 4, TaskVerifying},
		{AttemptFailedRetryable, 1, 4, TaskReady},
		{AttemptFailedRetryable, 4, 4, TaskFailed}, // budget exhausted
		{AttemptStale, 1, 4, TaskReady},
		{AttemptStale, 9, 4, TaskFailed},
		{AttemptFailedTerminal, 1, 4, TaskFailed}, // terminal ignores the budget
		{AttemptCancelled, 1, 4, TaskCancelled},
	}
	for _, tc := range cases {
		if got := TaskStatusAfterAttempt(tc.attempt, tc.used, tc.max); got != tc.want {
			t.Errorf("TaskStatusAfterAttempt(%s, %d/%d) = %s, want %s",
				tc.attempt, tc.used, tc.max, got, tc.want)
		}
	}
}

func TestVisibilityLadderOrdering(t *testing.T) {
	if !VisibilityTeamArtifacts.AtLeast(VisibilityTeamSummary) {
		t.Error("team_artifacts should include team_summary")
	}
	if VisibilityPrivate.AtLeast(VisibilityTeamSummary) {
		t.Error("private must not satisfy team_summary")
	}
	// An unrecognized value must be treated as maximally restrictive, not permissive: a typo
	// in a config file should hide data, never expose it.
	if Visibility("nonsense").AtLeast(VisibilityTeamSummary) {
		t.Error("an unknown visibility must fail closed")
	}
}

func TestRoleCapabilities(t *testing.T) {
	if !RoleMaintainer.Can(RoleContributor) {
		t.Error("maintainer should satisfy contributor")
	}
	if RoleObserver.Can(RoleContributor) {
		t.Error("observer must not satisfy contributor")
	}
	// Runner is a capability, not a seniority level; it must never imply human permissions.
	if RoleRunner.Can(RoleMaintainer) || RoleRunner.Can(RoleContributor) {
		t.Error("runner must not imply human roles")
	}
	if RoleOrgAdmin.Can(RoleRunner) {
		t.Error("a human role must not imply the runner capability")
	}
}

func TestEffortAndTierClamping(t *testing.T) {
	if got := EffortHigh.AtMost(EffortLow); got != EffortLow {
		t.Errorf("AtMost = %s, want low", got)
	}
	if got := EffortHigh.Up(); got != EffortHigh {
		t.Errorf("escalation should saturate below max, got %s", got)
	}
	if got := TierT4.Up(); got != TierT4 {
		t.Errorf("tier escalation should saturate at T4, got %s", got)
	}
	if got := TierT0.Down(); got != TierT0 {
		t.Errorf("tier de-escalation should saturate at T0, got %s", got)
	}
	if !TierT3.AtLeast(TierT2) || TierT1.AtLeast(TierT2) {
		t.Error("tier ordering is wrong")
	}
}

// The Go enums and the database CHECK constraints must not drift. A value legal in one and
// not the other produces a runtime constraint violation on a path that unit tests pass.
func TestEnumsMatchSchema(t *testing.T) {
	schema := readSchema(t)

	// Several tables have a `state` or `visibility` column with different value sets, so a
	// constraint is identified by a value only it contains rather than by column name alone.
	check := func(label, column, sentinel string, values []string) {
		t.Helper()
		re := regexp.MustCompile(`(?s)CHECK \(` + regexp.QuoteMeta(column) + ` IN \((.*?)\)\)`)

		for _, m := range re.FindAllStringSubmatch(schema, -1) {
			declared := map[string]bool{}
			for _, raw := range strings.Split(m[1], ",") {
				if v := strings.Trim(strings.TrimSpace(raw), "'\n\t "); v != "" {
					declared[v] = true
				}
			}
			if !declared[sentinel] {
				continue // a different table's constraint on the same column name
			}
			for _, v := range values {
				if !declared[v] {
					t.Errorf("%s: Go value %q is missing from the schema CHECK constraint", label, v)
				}
				delete(declared, v)
			}
			for v := range declared {
				t.Errorf("%s: schema allows %q, which no Go constant defines", label, v)
			}
			return
		}
		t.Errorf("%s: no CHECK constraint on %q containing %q was found", label, column, sentinel)
	}

	check("task status", "status", string(TaskVerifying), asStrings(AllTaskStatuses))
	check("attempt state", "state", string(AttemptPreparingWorkspace), asStrings(AllAttemptStates))
	check("session state", "state", string(SessionOnlineIdle), asStrings(AllSessionStates))
	check("visibility", "visibility", string(VisibilityTeamArtifacts), asStrings(AllVisibilities))
	check("reservation mode", "mode", string(ModeProtectedExclusive), asStrings(AllReservationModes))
	check("resource type", "resource_type", string(ResourceMigration), asStrings(AllResourceTypes))
	check("principal kind", "kind", string(PrincipalRunner), asStrings([]PrincipalKind{
		PrincipalHuman, PrincipalSession, PrincipalAgentAttempt,
		PrincipalRunner, PrincipalControlPlane, PrincipalReviewBot,
	}))
	check("role", "role", string(RoleProjectAdmin), asStrings([]Role{
		RoleOrgAdmin, RoleProjectAdmin, RoleMaintainer, RoleContributor,
		RoleReviewer, RoleObserver, RoleRunner,
	}))
	check("conflict kind", "kind", string(ConflictMergeRisk), asStrings([]ConflictKind{
		ConflictScopeOverlap, ConflictDuplicateIntent, ConflictMergeRisk,
		ConflictDependencyCycle, ConflictProtectedResource,
	}))
	check("outcome", "suggestion", string(OutcomeSuggestSupersede), asStrings([]Outcome{
		OutcomeAllow, OutcomeAllowWithWarning, OutcomeBlockDuplicate, OutcomeBlockConflict,
		OutcomeSuggestJoin, OutcomeSuggestSplit, OutcomeSuggestWait, OutcomeSuggestSupersede,
	}))
	check("agent role", "role", string(RoleImplementer), asStrings([]AgentRole{
		RolePlanner, RoleImplementer, RoleVerifier, RoleCodeReviewer, RoleResearcher,
	}))
	check("effort", "reasoning_effort", string(EffortMax), asStrings([]Effort{
		EffortNone, EffortLow, EffortMedium, EffortHigh, EffortMax,
	}))
	check("severity", "severity", string(SeverityCritical), asStrings([]Severity{
		SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical,
	}))
	check("risk level", "risk_level", string(RiskCritical), asStrings([]RiskLevel{
		RiskUnknown, RiskLow, RiskMedium, RiskHigh, RiskCritical,
	}))
}

func asStrings[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

func readSchema(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		candidate := filepath.Join(dir, "internal", "db", "migrations", "0001_init.sql")
		if body, err := os.ReadFile(candidate); err == nil {
			return string(body)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the initial migration")
	return ""
}
