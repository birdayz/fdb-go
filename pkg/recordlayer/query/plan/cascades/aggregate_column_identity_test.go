package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestAggColumnMatches_NestedShadowsTopLevel pins RFC-187 S4/S5/S8: the single
// chokepoint every aggregate-index match site now routes through
// (MatchesGroupBy, MatchesSingleAggregateOf, groupColEqualityIndex, the
// streaming-agg-from-index eligibility checks) compares a query grouping-key /
// aggregate-operand against a declared aggregate-index column by full accessor
// PATH — so a nested `addr.city` grouping key never matches a same-leaf-named
// top-level `city` aggregate index (which would aggregate the wrong column).
func TestAggColumnMatches_NestedShadowsTopLevel(t *testing.T) {
	t.Parallel()

	src := values.NamedCorrelationIdentifier("T")
	flatCity := values.NewFieldValue(values.NewQuantifiedObjectValue(src), "CITY", values.UnknownType)
	nestedAddrCity := values.NewFieldValue(
		values.NewFieldValue(values.NewQuantifiedObjectValue(src), "ADDR", values.UnknownType),
		"CITY", values.UnknownType)
	bakedCity := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(src), "CITY", 0, values.UnknownType)

	if aggColumnMatches(nestedAddrCity, "CITY") {
		t.Fatal("nested addr.city matched a top-level CITY aggregate column (would aggregate wrong column)")
	}
	if !aggColumnMatches(flatCity, "CITY") {
		t.Fatal("flat city failed to match the CITY aggregate column (regressed the positive match)")
	}
	if !aggColumnMatches(bakedCity, "CITY") {
		t.Fatal("baked city failed to match the CITY aggregate column (baked/lazy bridge regressed)")
	}
	// Case-insensitive, and a different leaf must not match.
	if !aggColumnMatches(flatCity, "city") {
		t.Fatal("aggregate column match is not case-insensitive")
	}
	if aggColumnMatches(values.NewFieldValue(values.NewQuantifiedObjectValue(src), "NAME", values.UnknownType), "CITY") {
		t.Fatal("column NAME matched aggregate column CITY")
	}
}
