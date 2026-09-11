package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// placeholderBinding is one query comparison that binds a candidate
// placeholder: the query predicate it came from (the identity the predicate
// map and the residual bookkeeping use) and the ORIENTED comparison that
// binds — `column OP comparand`, commuted where the column sat on the right.
type placeholderBinding struct {
	pred       predicates.QueryPredicate
	cp         *predicates.ComparisonPredicate
	comparison *predicates.Comparison
}

// foldPlaceholderBindings folds every comparison bound to ONE placeholder
// into a single scan range and says which bindings it carries.
//
// Java never sees several query predicates on one column at match time: its
// SelectExpression constructor folds them into one sargable
// (SelectExpression.simplifyConjunction) and mapPredicateToPlaceholder merges
// that sargable's comparisons with ComparisonRange.mergeAll, pushing the range
// and re-applying the residuals. Go keeps one comparison per query predicate,
// so the same fold runs here, at binding time, with the same total merge:
// walked in predicate order, an equality takes the range and every other
// comparison is a residual (a duplicate of that equality is carried, not
// re-applied); otherwise every distinct inequality accumulates. The members
// returned are the bindings whose comparison the range carries; every other
// binding is a residual for the caller to re-apply as a filter, exactly the
// list Java's MergeResult.getResidualComparisons would hold.
//
// The fold never consults the candidate. Whether the folded range can be
// EXECUTED at the placeholder's coordinate — physical key type, a STARTS_WITH
// standing alone, NaN — is the candidate's decision over the whole range,
// made once by ComputeBoundParameterPrefixMap / candidateBindingRangesEligible
// after the fold, and a range the candidate declines demotes every member.
// Deterministic in predicate order: with two different equalities the first
// wins, as it does in Java (RangeConstraints keeps insertion order).
func foldPlaceholderBindings(bound []placeholderBinding) (*predicates.ComparisonRange, []placeholderBinding) {
	merged := predicates.EmptyComparisonRange()
	var members []placeholderBinding
	for _, b := range bound {
		if b.comparison == nil {
			continue
		}
		res := merged.Merge(b.comparison)
		if res.Complete() {
			// Carried: either appended, or a duplicate of one already carried.
			merged = res.Range
			members = append(members, b)
			continue
		}
		// Which side the residuals are on is read off the residual list
		// itself, never off range identity: the incoming comparison is either
		// the sole residual (the range did not move) or an equality that
		// displaced every accumulated inequality (the residuals are those, and
		// the range is the equality alone). Merge's arm table admits no third
		// outcome; the Merge tests pin that the residual list is exactly one
		// of these two.
		if len(res.Residuals) == 1 && res.Residuals[0] == b.comparison {
			continue
		}
		merged = res.Range
		members = []placeholderBinding{b}
	}
	return merged, members
}
