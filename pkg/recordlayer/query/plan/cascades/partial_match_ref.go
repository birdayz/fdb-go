package cascades

import "fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"

// AddPartialMatchForCandidate stores a PartialMatch for the given
// MatchCandidate on the Reference. Typed wrapper around
// Reference.AddPartialMatch. Mirrors Java's
// Reference.addPartialMatchForCandidate.
//
// Semantic dedup: Reference.AddPartialMatch dedups only by pointer
// identity, but matching rules re-fire and reconstruct the SAME logical
// match. Endpoint-only dedup is too coarse, however: partial matching can
// produce several valid alias maps, child selections, and bindings for one
// (queryExpression, candidateRef) pair. Drop only an exact semantic re-add;
// retain alternatives whose downstream matching or compensation metadata
// differs.
func AddPartialMatchForCandidate(
	ref *expressions.Reference,
	candidate MatchCandidate,
	match PartialMatch,
) bool {
	pmi, ok := match.(*PartialMatchImpl)
	if ok {
		for _, existing := range GetPartialMatchesForCandidate(ref, candidate) {
			epm, ok := existing.(*PartialMatchImpl)
			if !ok {
				continue
			}
			if partialMatchesSemanticallyEqual(epm, pmi) {
				return false // exact semantic match already present
			}
		}
	}
	return ref.AddPartialMatch(candidate, match)
}

// GetPartialMatchesForCandidate returns all PartialMatches stored on
// the Reference for the given MatchCandidate. Typed wrapper around
// Reference.GetPartialMatchesFor. Mirrors Java's
// Reference.getPartialMatchesForCandidate.
func GetPartialMatchesForCandidate(
	ref *expressions.Reference,
	candidate MatchCandidate,
) []PartialMatch {
	raw := ref.GetPartialMatchesFor(candidate)
	if len(raw) == 0 {
		return nil
	}
	result := make([]PartialMatch, len(raw))
	for i, r := range raw {
		result[i] = r.(PartialMatch)
	}
	return result
}

// forEachPartialMatchForCandidate visits stored matches in deterministic
// insertion order without allocating a typed copy of the entire slice. It is
// used by budgeted matcher searches, where even inspecting a candidate match
// consumes work and an adversarially large reference must be stoppable.
func forEachPartialMatchForCandidate(
	ref *expressions.Reference,
	candidate MatchCandidate,
	visit func(PartialMatch) bool,
) bool {
	for _, raw := range ref.GetPartialMatchesFor(candidate) {
		match, ok := raw.(PartialMatch)
		if !ok {
			continue
		}
		if !visit(match) {
			return false
		}
	}
	return true
}

// GetPartialMatchesForExpression returns all PartialMatches stored on
// the Reference whose query expression matches the given expression
// (identity comparison, matching Java's == check in
// Reference.getPartialMatchesForExpression).
func GetPartialMatchesForExpression(
	ref *expressions.Reference,
	expr expressions.RelationalExpression,
) []PartialMatch {
	raw := ref.GetAllPartialMatches()
	if len(raw) == 0 {
		return nil
	}
	var result []PartialMatch
	for _, r := range raw {
		pm := r.(PartialMatch)
		if pmi, ok := pm.(*PartialMatchImpl); ok {
			if pmi.GetQueryExpression() == expr {
				result = append(result, pm)
			}
		}
	}
	return result
}

// GetPartialMatchCandidatesTyped returns all MatchCandidates that have
// partial matches on the Reference. Typed wrapper around
// Reference.GetPartialMatchCandidates.
func GetPartialMatchCandidatesTyped(
	ref *expressions.Reference,
) []MatchCandidate {
	raw := ref.GetPartialMatchCandidates()
	if len(raw) == 0 {
		return nil
	}
	result := make([]MatchCandidate, len(raw))
	for i, r := range raw {
		result[i] = r.(MatchCandidate)
	}
	return result
}
