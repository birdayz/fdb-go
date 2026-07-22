package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestBindOrientedComparison_NestedFieldShadowsTopLevelIndex pins RFC-187 S1:
// a predicate on a NESTED field `addr.city` must NOT bind (SARG) a top-level
// index column that merely shares the leaf name `city`. Before the name-path
// fix, valuesMatchColumn compared leaf names (EqualFold("CITY","CITY")) and
// bound the nested predicate to the wrong column — a sargable seek marked
// matched with no residual re-check → wrong rows.
func TestBindOrientedComparison_NestedFieldShadowsTopLevelIndex(t *testing.T) {
	t.Parallel()

	source := values.NamedCorrelationIdentifier("T")
	cand := values.NamedCorrelationIdentifier("CAND")
	param := values.NamedCorrelationIdentifier("p0")

	// Candidate placeholder for a TOP-LEVEL `city` index column, exactly the
	// shape ValueIndexScanMatchCandidate.ColumnValue builds: FieldValue(QOV,"CITY").
	topLevelCity := values.NewFieldValue(values.NewQuantifiedObjectValue(cand), "CITY", values.UnknownType)
	ph := predicates.NewPlaceholder(param, topLevelCity)

	// A constant comparand — independently evaluable w.r.t. the matched source.
	lit := &values.ConstantValue{Value: "NYC", Typ: values.UnknownType}
	eq := func(col values.Value) *predicates.ComparisonPredicate {
		return predicates.NewComparisonPredicate(col, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: lit})
	}

	// NESTED `addr.city` (form a — nested Child chain, leaf Field="CITY"),
	// correlated to the matched source T.
	nestedAddrCity := values.NewFieldValue(
		values.NewFieldValue(values.NewQuantifiedObjectValue(source), "ADDR", values.UnknownType),
		"CITY", values.UnknownType)
	if got := bindOrientedComparison(eq(nestedAddrCity), ph, source); got != nil {
		t.Fatalf("nested addr.city bound the top-level `city` index (wrong column): got %v, want nil", got)
	}

	// POSITIVE: a real top-level `city` predicate MUST still bind — the fix must
	// not regress the flat-column match.
	flatCity := values.NewFieldValue(values.NewQuantifiedObjectValue(source), "CITY", values.UnknownType)
	if got := bindOrientedComparison(eq(flatCity), ph, source); got == nil {
		t.Fatalf("top-level `city = 'NYC'` failed to bind the `city` index (regressed the positive match)")
	}

	// POSITIVE (bake-tolerance): a resolver-BAKED top-level `city` ref (the shape
	// resolveQualifiedBaked yields for a qualified predicate) must bind the LAZY
	// candidate — the mixed baked/lazy representation the leaf-name compare bridged.
	bakedCity := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(source), "CITY", 0, values.UnknownType)
	if got := bindOrientedComparison(eq(bakedCity), ph, source); got == nil {
		t.Fatalf("baked top-level `city` ref failed to bind the lazy candidate (baked/lazy bridge regressed)")
	}
}
