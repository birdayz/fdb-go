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

// A primitive Explode flows its element as a bare QOV. Query and candidate
// correlations differ, but once bindOrientedComparison has established the
// query operand belongs to the matched source, two bare QOVs denote the same
// fanout element slot and must bind the candidate placeholder.
func TestBindOrientedComparison_PrimitiveFanOutElementQOV(t *testing.T) {
	t.Parallel()

	queryElement := values.NamedCorrelationIdentifier("E")
	candidateElement := values.NamedCorrelationIdentifier("CANDIDATE_E")
	param := values.NamedCorrelationIdentifier("p0")
	ph := predicates.NewPlaceholder(
		param,
		values.NewQuantifiedObjectValueOfType(candidateElement, values.NotNullLong),
	)
	cp := predicates.NewComparisonPredicate(
		values.NewQuantifiedObjectValueOfType(queryElement, values.NotNullLong),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(9), Typ: values.NullableLong},
		},
	)
	if got := bindOrientedComparison(cp, ph, queryElement); got == nil {
		t.Fatal("primitive fanout element QOV failed to bind its candidate placeholder")
	}

	// A field below the element is a different semantic slot and must not be
	// collapsed into the whole element merely because the other side is a QOV.
	fieldBelowElement := values.NewFieldValue(
		values.NewQuantifiedObjectValue(queryElement),
		"X",
		values.NullableLong,
	)
	cp = predicates.NewComparisonPredicate(
		fieldBelowElement,
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(9), Typ: values.NullableLong},
		},
	)
	if got := bindOrientedComparison(cp, ph, queryElement); got != nil {
		t.Fatalf("field-below-element matched whole primitive element: %v", got)
	}

	recordType := values.NewRecordType("ELEMENT", false, []values.Field{{
		Name:      "X",
		FieldType: values.NullableLong,
		Ordinal:   0,
	}})
	structuredQuery := values.NewQuantifiedObjectValueOfType(queryElement, recordType)
	structuredPlaceholder := predicates.NewPlaceholder(
		param,
		values.NewQuantifiedObjectValueOfType(candidateElement, recordType),
	)
	cp = predicates.NewComparisonPredicate(
		structuredQuery,
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(9), Typ: values.NullableLong},
		},
	)
	if got := bindOrientedComparison(cp, structuredPlaceholder, queryElement); got != nil {
		t.Fatalf("structured whole-object QOV matched as a primitive fanout element: %v", got)
	}

	unknownQuery := values.NewQuantifiedObjectValue(queryElement)
	unknownPlaceholder := predicates.NewPlaceholder(
		param,
		values.NewQuantifiedObjectValue(candidateElement),
	)
	typedQuery := values.NewQuantifiedObjectValueOfType(queryElement, values.NotNullLong)
	cp = predicates.NewComparisonPredicate(
		typedQuery,
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(9), Typ: values.NullableLong},
		},
	)
	if got := bindOrientedComparison(cp, unknownPlaceholder, queryElement); got == nil {
		t.Fatal("typed primitive query did not match the descriptor-UNKNOWN fanout candidate element")
	}

	cp = predicates.NewComparisonPredicate(
		unknownQuery,
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(9), Typ: values.NullableLong},
		},
	)
	if got := bindOrientedComparison(cp, unknownPlaceholder, queryElement); got != nil {
		t.Fatalf("unresolved whole-object QOV matched as a proven primitive fanout element: %v", got)
	}
}
