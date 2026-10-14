package policy

// BoundsFacts is the position of a measured-bound frontier and of one challenge against it,
// read by internal/bounds from the sealed documents the ecbench bounds protocol writes
// (`ecbench.frontier/v1` and `ecbench.verdict/v1`; aburan28/crypto docs/bounds/README.md).
//
// They belong here, beside the cairn facts, because they are the same kind of thing. A
// frontier is built from committed bound records and never edited; a verdict is one audited
// paired session judged against a frozen challenge, and judging it again yields the same id.
// Both are content-addressed, so anyone holding the records can rebuild them and check the
// seal. A routing rule that reads them is reading a fact, not an opinion -- which is the
// property that lets it spend real money on the answer.
//
// What they make answerable: "this task is trying to beat a bound; has the challenge gone
// several epochs without an advance?" An epoch without an advance at the cheap end of the
// ladder is the signal that the cheap end has found what it will find.
type BoundsFacts struct {
	// Known reports that a frontier document was read. It is a statement about the project's
	// configuration, not about the task: when `bounds.frontier` in project.yaml names a
	// document that exists and seals, every task sees the same frontier, so a rule meant for
	// bounds work only guards itself with `task.labels has "bounds"`. When false, Env renders
	// the frontier names as nil so that no threshold against them can be met.
	Known      bool
	FrontierID string
	// DomainCount is the number of domains the frontier compares within. Bounds compare only
	// inside one domain (problem, curve family, unit, tier, resource envelope).
	DomainCount int
	// FrontierEntries counts the bounds on the frontier across every domain -- those with
	// `is_frontier`, which no other bound dominates -- not every bound the document lists.
	FrontierEntries int

	// ChallengeKnown reports that the task names a challenge (its `ECCH1h…` id in
	// task.external_ref, the slot a cairn task uses for its objective id) and that at least
	// one verdict against it was read. When false the challenge names render as nil: a
	// challenge nobody has run yet has no epochs, not zero epochs, and must not look stalled.
	ChallengeKnown bool
	// ChallengeEpochs is the number of distinct epochs holding a verdict.
	ChallengeEpochs int
	// EpochsWithoutAdvance is the number of distinct epochs later than the last `advances`
	// verdict -- every epoch seen, when nothing has advanced yet. Epochs are counted, not
	// subtracted: a challenge's epochs are whatever integers nobody has used for it, so the
	// gap between two epoch numbers means nothing.
	EpochsWithoutAdvance int
	// LastOutcome is the outcome of the latest verdict: advances, trade, matches, regresses
	// or inadmissible.
	LastOutcome string
}

// addTo renders the facts under their dotted names. The two presence flags are plain bools so
// that `!bounds.known` and `!bounds.challenge_known` read naturally; every other name is nil
// when its flag is false, so that no threshold comparison against it can be true. See
// CairnFacts.Known for why absence must never render as zero.
func (b BoundsFacts) addTo(env MapEnv) {
	var frontierID, domainCount, frontierEntries any
	if b.Known {
		frontierID, domainCount, frontierEntries = b.FrontierID, b.DomainCount, b.FrontierEntries
	}
	var epochs, withoutAdvance, lastOutcome any
	if b.ChallengeKnown {
		epochs, withoutAdvance, lastOutcome = b.ChallengeEpochs, b.EpochsWithoutAdvance, b.LastOutcome
	}
	env["bounds.known"] = b.Known
	env["bounds.frontier_id"] = frontierID
	env["bounds.domain_count"] = domainCount
	env["bounds.frontier_entries"] = frontierEntries
	env["bounds.challenge_known"] = b.ChallengeKnown
	env["bounds.challenge_epochs"] = epochs
	env["bounds.epochs_without_advance"] = withoutAdvance
	env["bounds.last_outcome"] = lastOutcome
}
