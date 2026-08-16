package resource

import "github.com/adamburan/conductor/internal/domain"

// Policy carries the configurable cells of the conflict matrix. DESIGN.md §11.3 marks
// several cells "block by policy" or "warn or block"; those are the knobs here, and they
// come from .conductor/policies.yaml rather than being compiled in.
type Policy struct {
	// WriteWrite governs two overlapping write_exclusive reservations.
	WriteWrite domain.Outcome
	// ReadWrite governs a read_shared reservation meeting a write in either direction.
	ReadWrite domain.Outcome
	// ProtectedVsNonWrite governs the "block by policy" cells where a protected_exclusive
	// reservation meets a read or review.
	ProtectedVsNonWrite domain.Outcome
	// SpeculativeVsWrite governs the "warn or block" cells between a speculative write and
	// a real write.
	SpeculativeVsWrite domain.Outcome
}

// DefaultPolicy matches .conductor/policies.yaml defaults and DESIGN.md §20.2.
func DefaultPolicy() Policy {
	return Policy{
		WriteWrite:          domain.OutcomeBlockConflict,
		ReadWrite:           domain.OutcomeAllowWithWarning,
		ProtectedVsNonWrite: domain.OutcomeBlockConflict,
		SpeculativeVsWrite:  domain.OutcomeAllowWithWarning,
	}
}

// Holder describes one side of a potential conflict.
type Holder struct {
	Mode             domain.ReservationMode
	AlternativeGroup string
}

// Decide applies the DESIGN.md §11.3 matrix to two *already known to overlap* reservations.
// Callers must establish overlap with Overlaps first; this function answers only "given
// that these two touch the same thing, what happens?".
//
// The matrix rows are the existing reservation, the columns the requested one.
func Decide(existing, requested Holder, p Policy) domain.Outcome {
	p = p.withDefaults()

	// Protected is absolute in every cell except the ones the project marks otherwise, and
	// it is checked first so no later rule can weaken it.
	if existing.Mode == domain.ModeProtectedExclusive || requested.Mode == domain.ModeProtectedExclusive {
		other := requested.Mode
		if requested.Mode == domain.ModeProtectedExclusive {
			other = existing.Mode
		}
		if other == domain.ModeProtectedExclusive {
			return domain.OutcomeBlockConflict
		}
		if other.Writes() {
			return domain.OutcomeBlockConflict
		}
		// read_shared or review_shared against a protected resource: "block by policy".
		return p.ProtectedVsNonWrite
	}

	// Review never contends for ownership; it is compatible with everything non-protected.
	if existing.Mode == domain.ModeReviewShared || requested.Mode == domain.ModeReviewShared {
		return domain.OutcomeAllow
	}

	// Two reads never conflict.
	if !existing.Mode.Writes() && !requested.Mode.Writes() {
		return domain.OutcomeAllow
	}

	// Exactly one side writes: the read/write cell.
	if existing.Mode.Writes() != requested.Mode.Writes() {
		return p.ReadWrite
	}

	// Both write. Speculative alternatives are the one case where two writers may coexist,
	// because they are deliberately competing implementations (DESIGN.md §11.2) and are not
	// mergeable without an explicit resolution.
	bothSpeculative := existing.Mode == domain.ModeSpeculativeWrite && requested.Mode == domain.ModeSpeculativeWrite
	if bothSpeculative {
		if existing.AlternativeGroup != "" && requested.AlternativeGroup != "" {
			return domain.OutcomeAllow
		}
		// Without a declared alternative group there is nothing marking these as
		// alternatives rather than an accidental collision.
		return domain.OutcomeAllowWithWarning
	}

	if existing.Mode == domain.ModeSpeculativeWrite || requested.Mode == domain.ModeSpeculativeWrite {
		return p.SpeculativeVsWrite
	}

	return p.WriteWrite
}

func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	if p.WriteWrite == "" {
		p.WriteWrite = d.WriteWrite
	}
	if p.ReadWrite == "" {
		p.ReadWrite = d.ReadWrite
	}
	if p.ProtectedVsNonWrite == "" {
		p.ProtectedVsNonWrite = d.ProtectedVsNonWrite
	}
	if p.SpeculativeVsWrite == "" {
		p.SpeculativeVsWrite = d.SpeculativeVsWrite
	}
	return p
}

// SeverityFor maps an outcome and the modes that produced it onto a conflict severity, used
// for ranking the conflict radar and deciding what blocks a claim.
func SeverityFor(outcome domain.Outcome, existing, requested domain.ReservationMode) domain.Severity {
	switch outcome {
	case domain.OutcomeAllow:
		return domain.SeverityInfo
	case domain.OutcomeAllowWithWarning:
		return domain.SeverityLow
	case domain.OutcomeBlockDuplicate:
		return domain.SeverityMedium
	case domain.OutcomeBlockConflict:
		if existing == domain.ModeProtectedExclusive || requested == domain.ModeProtectedExclusive {
			return domain.SeverityCritical
		}
		return domain.SeverityHigh
	default:
		return domain.SeverityMedium
	}
}

// ConflictKindFor classifies an overlap for the conflict graph.
func ConflictKindFor(existing, requested domain.ReservationMode) domain.ConflictKind {
	if existing == domain.ModeProtectedExclusive || requested == domain.ModeProtectedExclusive {
		return domain.ConflictProtectedResource
	}
	return domain.ConflictScopeOverlap
}

// Suggestion turns a blocking outcome into the actionable advice the CLI, dashboard, and MCP
// tools present. DESIGN.md §6.6 is explicit that the engine returns an action, not a score:
// telling someone "0.82 similarity" is not useful, telling them "join T-42" is.
//
// sharedRatio is the fraction of the requester's resources that collide, which is what
// distinguishes "you are doing the same work" from "you clipped the edge of my directory".
func Suggestion(outcome domain.Outcome, sharedRatio float64, dependencyDirection int) domain.Outcome {
	if !outcome.Blocks() {
		return outcome
	}
	switch {
	case dependencyDirection != 0:
		// One task must land before the other; serializing is the only correct answer.
		return domain.OutcomeSuggestWait
	case sharedRatio >= 0.8:
		return domain.OutcomeSuggestJoin
	case sharedRatio > 0:
		return domain.OutcomeSuggestSplit
	default:
		return outcome
	}
}
