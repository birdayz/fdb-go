package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
)

// AddPartialMatchForCandidate stores a PartialMatch for the given
// MatchCandidate on the Reference. Typed wrapper around
// Reference.AddPartialMatch. Mirrors Java's
// Reference.addPartialMatchForCandidate.
func AddPartialMatchForCandidate(
	ref *expressions.Reference,
	candidate MatchCandidate,
	match PartialMatch,
) bool {
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
