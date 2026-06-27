package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// buildMatchMaxMatchMap computes the MaxMatchMap for a query↔candidate
// match, porting Java's SelectExpression.subsumedBy and
// RelationalExpression.exactlySubsumedBy. Both Java call sites compute a
// MaxMatchMap before constructing the RegularMatchInfo — it is NOT
// optional: the MaxMatchMap is the structural record of which query
// result sub-expressions the candidate covers, and PartialMatch.PullUp
// (and hence compensation) reads it. Without it, PullUp returns nil and
// CompensateCompleteMatch yields ImpossibleCompensation, so the
// data-access path produces no scan.
//
// Recipe (Java RelationalExpression.exactlySubsumedBy /
// SelectExpression.subsumedBy):
//
//	translatedResultValue = queryResultValue.translateCorrelations(bindingAliasMap)
//	MaxMatchMap.compute(translatedResultValue,
//	    candidateResultValue,
//	    aliases(candidateQuantifiers)            // = bindingAliasMap targets
//	    ValueEquivalence.fromAliasMap(bindingAliasMap))
//
// The ranged-over aliases are the candidate-side aliases (the binding
// alias map's targets) — the same set PartialMatch.PullUp re-derives
// from the binding map.
func buildMatchMaxMatchMap(
	queryResultValue values.Value,
	candidateResultValue values.Value,
	boundAliasMap *AliasMap,
) *MaxMatchMap {
	rebase := make(values.AliasMap)
	rangedOver := make(map[values.CorrelationIdentifier]struct{})
	for _, src := range boundAliasMap.Sources() {
		tgt := boundAliasMap.GetTarget(src)
		rebase[src] = tgt
		rangedOver[tgt] = struct{}{}
	}
	translated := values.RebaseValue(queryResultValue, rebase)
	return ComputeMaxMatchMapWithEquivalence(
		translated,
		candidateResultValue,
		rangedOver,
		NewAliasMapValueEquivalence(boundAliasMap),
	)
}

// isSargableComparisonForMatch reports whether a comparison type can bind
// to an index candidate's placeholder as a scan constraint. It is the
// value-index range set (ImplementIndexScanRule's isScanRangeCompatible:
// =, <, <=, >, >=, STARTS_WITH) PLUS the NULL comparisons (IS NULL is a
// [null] EQUALITY range, IS NOT NULL is the (null, +inf) INEQUALITY range —
// Java's ScanComparisons.getComparisonType maps them to EQUALITY/INEQUALITY)
// PLUS the vector DISTANCE_RANK bounds (for a vector candidate's distance
// placeholder). Everything else (IN, NOT EQUALS, LIKE, full-text, …) is
// non-sargable and stays a residual filter.
//
// Only this index-match gate admits the NULL comparisons (not the base
// isScanRangeCompatible, which the NLJ path also consults): the index-match
// path runs them through ComparisonRange.Merge (which classifies IS NULL as
// equality, IS NOT NULL as inequality) and the executor's
// scanComparisonsToTupleRange (which builds the [null]/(null,+inf) ranges),
// both of which handle the null cases correctly.
func isSargableComparisonForMatch(t predicates.ComparisonType) bool {
	if isScanRangeCompatible(t) {
		return true
	}
	switch t {
	case predicates.ComparisonIsNull,
		predicates.ComparisonIsNotNull:
		return true
	case predicates.ComparisonDistanceRankEquals,
		predicates.ComparisonDistanceRankLessThan,
		predicates.ComparisonDistanceRankLessThanOrEq:
		return true
	}
	return false
}

// reapplyResidualCompensation builds a PredicateCompensation that
// re-applies a query predicate as a residual filter over the matched
// index scan. Ports Java's residual path in
// QueryPredicate.findImpliedMappings: a query predicate with no
// candidate placeholder maps to a tautology candidate predicate whose
// compensation is PredicateCompensationFunction.ofPredicate(predicate)
// — the predicate is deferred to a filter above the scan rather than
// silently dropped.
func reapplyResidualCompensation(pred predicates.QueryPredicate) PredicateCompensation {
	return func(_ PartialMatch, _ map[values.CorrelationIdentifier]*predicates.ComparisonRange, _ *PullUp) PredicateCompensationFunc {
		return OfPredicateCompensation(pred, true)
	}
}
