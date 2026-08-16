package resource

import (
	"testing"

	"github.com/adamburan/conductor/internal/domain"
)

// TestMatrixMatchesDesign walks the full 5x5 table from DESIGN.md §11.3. Cells the design
// marks "by policy" are asserted against the default policy.
func TestMatrixMatchesDesign(t *testing.T) {
	read := domain.ModeReadShared
	write := domain.ModeWriteExclusive
	review := domain.ModeReviewShared
	spec := domain.ModeSpeculativeWrite
	prot := domain.ModeProtectedExclusive

	allow := domain.OutcomeAllow
	warn := domain.OutcomeAllowWithWarning
	block := domain.OutcomeBlockConflict

	cases := []struct {
		existing, requested domain.ReservationMode
		want                domain.Outcome
	}{
		// Existing: read
		{read, read, allow}, {read, write, warn}, {read, review, allow},
		{read, spec, warn}, {read, prot, block},
		// Existing: write
		{write, read, warn}, {write, write, block}, {write, review, allow},
		{write, spec, warn}, {write, prot, block},
		// Existing: review
		{review, read, allow}, {review, write, allow}, {review, review, allow},
		{review, spec, allow}, {review, prot, block},
		// Existing: speculative write
		{spec, read, warn}, {spec, write, warn}, {spec, review, allow},
		{spec, prot, block},
		// Existing: protected
		{prot, read, block}, {prot, write, block}, {prot, review, block},
		{prot, spec, block}, {prot, prot, block},
	}

	p := DefaultPolicy()
	for _, tc := range cases {
		got := Decide(Holder{Mode: tc.existing}, Holder{Mode: tc.requested}, p)
		if got != tc.want {
			t.Errorf("Decide(existing=%s, requested=%s) = %s, want %s",
				tc.existing, tc.requested, got, tc.want)
		}
	}
}

// Two speculative writes are the one legal pair of overlapping writers, and only when both
// declare an alternative group (DESIGN.md §11.2, §11.3).
func TestSpeculativeAlternatives(t *testing.T) {
	p := DefaultPolicy()

	got := Decide(
		Holder{Mode: domain.ModeSpeculativeWrite, AlternativeGroup: "alt-a"},
		Holder{Mode: domain.ModeSpeculativeWrite, AlternativeGroup: "alt-b"},
		p,
	)
	if got != domain.OutcomeAllow {
		t.Errorf("declared alternatives = %s, want allow", got)
	}

	got = Decide(
		Holder{Mode: domain.ModeSpeculativeWrite},
		Holder{Mode: domain.ModeSpeculativeWrite},
		p,
	)
	if got != domain.OutcomeAllowWithWarning {
		t.Errorf("undeclared alternatives = %s, want allow_with_warning", got)
	}
}

// A project may loosen "block by policy" cells, but must never be able to loosen a
// protected-vs-write collision, which is unconditional in the design.
func TestProtectedWriteIsNotPolicyConfigurable(t *testing.T) {
	loose := Policy{
		WriteWrite:          domain.OutcomeAllow,
		ReadWrite:           domain.OutcomeAllow,
		ProtectedVsNonWrite: domain.OutcomeAllow,
		SpeculativeVsWrite:  domain.OutcomeAllow,
	}
	got := Decide(
		Holder{Mode: domain.ModeProtectedExclusive},
		Holder{Mode: domain.ModeWriteExclusive},
		loose,
	)
	if got != domain.OutcomeBlockConflict {
		t.Errorf("protected vs write under loose policy = %s, want block_conflict", got)
	}

	// The configurable cell does honour policy.
	got = Decide(
		Holder{Mode: domain.ModeProtectedExclusive},
		Holder{Mode: domain.ModeReadShared},
		loose,
	)
	if got != domain.OutcomeAllow {
		t.Errorf("protected vs read under loose policy = %s, want allow", got)
	}
}

func TestZeroPolicyUsesDefaults(t *testing.T) {
	got := Decide(
		Holder{Mode: domain.ModeWriteExclusive},
		Holder{Mode: domain.ModeWriteExclusive},
		Policy{},
	)
	if got != domain.OutcomeBlockConflict {
		t.Errorf("zero policy write/write = %s, want block_conflict", got)
	}
}

func TestSeverityEscalatesForProtected(t *testing.T) {
	if got := SeverityFor(domain.OutcomeBlockConflict, domain.ModeWriteExclusive, domain.ModeWriteExclusive); got != domain.SeverityHigh {
		t.Errorf("write/write severity = %s, want high", got)
	}
	if got := SeverityFor(domain.OutcomeBlockConflict, domain.ModeProtectedExclusive, domain.ModeWriteExclusive); got != domain.SeverityCritical {
		t.Errorf("protected severity = %s, want critical", got)
	}
}

func TestSuggestion(t *testing.T) {
	cases := []struct {
		outcome domain.Outcome
		ratio   float64
		depDir  int
		want    domain.Outcome
	}{
		{domain.OutcomeAllow, 0.9, 0, domain.OutcomeAllow},               // non-blocking passes through
		{domain.OutcomeBlockConflict, 0.9, 0, domain.OutcomeSuggestJoin}, // same territory: join
		{domain.OutcomeBlockConflict, 0.3, 0, domain.OutcomeSuggestSplit},
		{domain.OutcomeBlockConflict, 0.9, 1, domain.OutcomeSuggestWait}, // ordering wins over ratio
		{domain.OutcomeBlockConflict, 0, 0, domain.OutcomeBlockConflict},
	}
	for _, tc := range cases {
		if got := Suggestion(tc.outcome, tc.ratio, tc.depDir); got != tc.want {
			t.Errorf("Suggestion(%s, %.1f, %d) = %s, want %s",
				tc.outcome, tc.ratio, tc.depDir, got, tc.want)
		}
	}
}
