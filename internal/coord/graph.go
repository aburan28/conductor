package coord

import (
	"context"
	"sort"

	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
	"github.com/adamburan/conductor/internal/resource"
)

// DetectConflicts rebuilds the merge-risk graph for a project (DESIGN.md §11.6).
//
// Three edge kinds come out of one pass over open tasks:
//
//   - scope_overlap  — what people *said* they would touch, via the reservation matrix.
//   - merge_risk     — what agents are *actually* touching, from git-observed changed paths.
//   - duplicate_intent — HMAC'd signature similarity, with no plaintext involved.
//
// The second is the one that earns its keep. Worktree isolation means two branches never
// corrupt each other on disk, so nothing surfaces the problem until merge time — by which
// point both sides have finished work that has to be thrown away. Comparing observed
// footprints turns that into a warning at minute five instead of a conflict at hour three.
func (s *Service) DetectConflicts(ctx context.Context, projectID domain.ID) (int, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return 0, err
	}
	footprints, err := s.Store.OpenFootprints(ctx, projectID)
	if err != nil {
		return 0, err
	}
	if len(footprints) < 2 {
		// Nothing can conflict with itself; still clear stale edges from a previous pass.
		for _, kind := range []domain.ConflictKind{
			domain.ConflictScopeOverlap, domain.ConflictMergeRisk, domain.ConflictDuplicateIntent,
		} {
			if err := s.Store.ReplaceConflictsOfKind(ctx, projectID, kind, nil); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}

	policy := config.ScopePolicyFrom(project.Config)
	threshold := project.Config.DuplicateThreshold
	if threshold <= 0 {
		threshold = 0.55
	}

	// Pre-parse declared scopes once; the pairwise loop is O(n²) in tasks and would
	// otherwise re-parse every resource string n times.
	parsed := make([][]parsedScope, len(footprints))
	for i, f := range footprints {
		for _, r := range f.Declared {
			res, err := resource.New(r.ResourceType, r.ResourceKey)
			if err != nil {
				continue
			}
			parsed[i] = append(parsed[i], parsedScope{res: res, mode: r.Mode, group: r.AlternativeGroup})
		}
	}

	var scopeEdges, mergeEdges, dupEdges []domain.ConflictEdge

	for i := 0; i < len(footprints); i++ {
		for j := i + 1; j < len(footprints); j++ {
			a, b := footprints[i], footprints[j]

			if e, ok := scopeOverlapEdge(projectID, a, b, parsed[i], parsed[j], policy); ok {
				scopeEdges = append(scopeEdges, e)
			}
			if e, ok := mergeRiskEdge(projectID, a, b); ok {
				mergeEdges = append(mergeEdges, e)
			}
			if e, ok := duplicateEdge(projectID, a, b, threshold); ok {
				dupEdges = append(dupEdges, e)
			}
		}
	}

	for kind, edges := range map[domain.ConflictKind][]domain.ConflictEdge{
		domain.ConflictScopeOverlap:    scopeEdges,
		domain.ConflictMergeRisk:       mergeEdges,
		domain.ConflictDuplicateIntent: dupEdges,
	} {
		if err := s.Store.ReplaceConflictsOfKind(ctx, projectID, kind, edges); err != nil {
			return 0, err
		}
	}
	return len(scopeEdges) + len(mergeEdges) + len(dupEdges), nil
}

type parsedScope struct {
	res   resource.Resource
	mode  domain.ReservationMode
	group string
}

// scopeOverlapEdge finds the most severe declared-scope collision between two tasks.
func scopeOverlapEdge(projectID domain.ID, a, b db.TaskFootprint, as, bs []parsedScope, policy resource.Policy) (domain.ConflictEdge, bool) {
	worst := domain.OutcomeAllow
	worstSeverity := domain.SeverityInfo
	kind := domain.ConflictScopeOverlap
	var contested []string
	overlapping := 0

	for _, x := range as {
		for _, y := range bs {
			if !resource.Overlaps(x.res, y.res) {
				continue
			}
			outcome := resource.Decide(
				resource.Holder{Mode: x.mode, AlternativeGroup: x.group},
				resource.Holder{Mode: y.mode, AlternativeGroup: y.group},
				policy)
			if outcome == domain.OutcomeAllow {
				continue
			}
			overlapping++
			contested = appendUnique(contested, x.res.String())
			sev := resource.SeverityFor(outcome, x.mode, y.mode)
			if sev.AtLeast(worstSeverity) {
				worstSeverity = sev
				worst = outcome
				kind = resource.ConflictKindFor(x.mode, y.mode)
			}
		}
	}
	if overlapping == 0 {
		return domain.ConflictEdge{}, false
	}

	denom := len(as)
	if len(bs) < denom {
		denom = len(bs)
	}
	ratio := 0.0
	if denom > 0 {
		ratio = float64(len(contested)) / float64(denom)
		if ratio > 1 {
			ratio = 1
		}
	}

	sort.Strings(contested)
	return domain.ConflictEdge{
		ProjectID:  projectID,
		TaskA:      a.TaskID,
		TaskB:      b.TaskID,
		Kind:       kind,
		Severity:   worstSeverity,
		Weight:     ratio,
		Suggestion: resource.Suggestion(worst, ratio, 0),
		Detail: domain.ConflictDetail{
			Resources:  contested,
			Reason:     "declared scopes overlap",
			TaskARef:   a.TaskRef,
			TaskBRef:   b.TaskRef,
			TaskAOwner: a.Owner,
			TaskBOwner: b.Owner,
		},
	}, true
}

// mergeRiskEdge compares the paths two attempts have actually changed.
func mergeRiskEdge(projectID domain.ID, a, b db.TaskFootprint) (domain.ConflictEdge, bool) {
	if len(a.ChangedPaths) == 0 || len(b.ChangedPaths) == 0 {
		return domain.ConflictEdge{}, false
	}
	inA := make(map[string]bool, len(a.ChangedPaths))
	for _, p := range a.ChangedPaths {
		inA[p] = true
	}
	var shared []string
	for _, p := range b.ChangedPaths {
		if inA[p] {
			shared = append(shared, p)
		}
	}
	if len(shared) == 0 {
		return domain.ConflictEdge{}, false
	}

	denom := len(a.ChangedPaths)
	if len(b.ChangedPaths) < denom {
		denom = len(b.ChangedPaths)
	}
	ratio := float64(len(shared)) / float64(denom)

	// Severity scales with how much of the smaller change is contested. One shared file in a
	// forty-file change is a note; ten out of twelve means one of these branches is going to
	// be rewritten.
	severity := domain.SeverityLow
	switch {
	case ratio >= 0.5:
		severity = domain.SeverityHigh
	case ratio >= 0.2:
		severity = domain.SeverityMedium
	}

	suggestion := domain.OutcomeSuggestSplit
	if ratio >= 0.8 {
		suggestion = domain.OutcomeSuggestJoin
	} else if severity == domain.SeverityLow {
		suggestion = domain.OutcomeAllowWithWarning
	}

	sort.Strings(shared)
	return domain.ConflictEdge{
		ProjectID:  projectID,
		TaskA:      a.TaskID,
		TaskB:      b.TaskID,
		Kind:       domain.ConflictMergeRisk,
		Severity:   severity,
		Weight:     ratio,
		Suggestion: suggestion,
		Detail: domain.ConflictDetail{
			SharedPaths: shared,
			Reason:      "both branches have modified the same files",
			TaskARef:    a.TaskRef,
			TaskBRef:    b.TaskRef,
			TaskAOwner:  a.Owner,
			TaskBOwner:  b.Owner,
		},
	}, true
}

// duplicateEdge compares HMAC'd intent signatures.
func duplicateEdge(projectID domain.ID, a, b db.TaskFootprint, threshold float64) (domain.ConflictEdge, bool) {
	exact := a.Fingerprint != "" && a.Fingerprint == b.Fingerprint
	similarity := privacy.Similarity(a.MinHash, b.MinHash)
	if !exact && similarity < threshold {
		return domain.ConflictEdge{}, false
	}

	severity := domain.SeverityMedium
	suggestion := domain.OutcomeSuggestJoin
	if exact {
		severity = domain.SeverityHigh
		similarity = 1
		suggestion = domain.OutcomeSuggestSupersede
	}

	return domain.ConflictEdge{
		ProjectID:  projectID,
		TaskA:      a.TaskID,
		TaskB:      b.TaskID,
		Kind:       domain.ConflictDuplicateIntent,
		Severity:   severity,
		Weight:     similarity,
		Suggestion: suggestion,
		Detail: domain.ConflictDetail{
			// Similarity is stored for operators but stripped by the privacy projection
			// before it reaches another principal, so it cannot be probed as an oracle
			// against someone's private intent.
			Similarity: similarity,
			Reason:     "intent fingerprints indicate the same work",
			TaskARef:   a.TaskRef,
			TaskBRef:   b.TaskRef,
			TaskAOwner: a.Owner,
			TaskBOwner: b.Owner,
		},
	}, true
}

func appendUnique(in []string, s string) []string {
	for _, existing := range in {
		if existing == s {
			return in
		}
	}
	return append(in, s)
}

// ConflictViews projects the conflict graph for a caller, orienting each edge so "mine" and
// "other" are meaningful and redacting private counterparty titles.
func (s *Service) ConflictViews(ctx context.Context, c Caller, projectID domain.ID, openOnly bool) ([]privacy.ConflictView, error) {
	edges, err := s.Store.ListConflicts(ctx, projectID, openOnly)
	if err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, nil
	}

	ids := map[domain.ID]bool{}
	for _, e := range edges {
		ids[e.TaskA], ids[e.TaskB] = true, true
	}
	tasks := map[domain.ID]domain.Task{}
	ownerIDs := make([]domain.ID, 0, len(ids))
	for id := range ids {
		t, err := s.Store.GetTask(ctx, id)
		if err != nil {
			continue
		}
		tasks[id] = t
		ownerIDs = append(ownerIDs, t.CreatedBy)
	}
	principals, err := s.Store.PrincipalsByID(ctx, ownerIDs)
	if err != nil {
		return nil, err
	}
	ownerOf := func(t domain.Task) privacy.Owner {
		p := principals[t.CreatedBy]
		return privacy.Owner{PrincipalID: p.ID, Handle: p.Handle}
	}

	out := make([]privacy.ConflictView, 0, len(edges))
	for _, e := range edges {
		a, okA := tasks[e.TaskA]
		b, okB := tasks[e.TaskB]
		if !okA || !okB {
			continue
		}
		// Orient the edge so the caller's own task is "mine" when one of them is.
		mine, other := a, b
		if b.CreatedBy == c.Principal.ID && a.CreatedBy != c.Principal.ID {
			mine, other = b, a
		}
		out = append(out, privacy.ProjectConflict(c.Viewer(), e, mine, other, ownerOf(mine), ownerOf(other)))
	}
	return out, nil
}
