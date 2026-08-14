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
	topLevelCity := mustMatchField(t, mustMatchQOV(t, cand, matchRuleRowType()), "CITY")
	ph := predicates.NewPlaceholder(param, topLevelCity)

	// A constant comparand — independently evaluable w.r.t. the matched source.
	lit := &values.ConstantValue{Value: "NYC", Typ: values.NullableString}
	eq := func(col values.Value) *predicates.ComparisonPredicate {
		return predicates.NewComparisonPredicate(col, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: lit})
	}

	// NESTED `addr.city` (form a — nested Child chain, leaf Field="CITY"),
	// correlated to the matched source T.
	nestedAddrCity := mustMatchField(t,
		mustMatchField(t, mustMatchQOV(t, source, matchRuleRowType()), "ADDR"),
		"CITY")
	if got := bindOrientedComparison(eq(nestedAddrCity), ph, source); got != nil {
		t.Fatalf("nested addr.city bound the top-level `city` index (wrong column): got %v, want nil", got)
	}

	// POSITIVE: a real top-level `city` predicate MUST still bind — the fix must
	// not regress the flat-column match.
	flatCity := mustMatchField(t, mustMatchQOV(t, source, matchRuleRowType()), "CITY")
	if got := bindOrientedComparison(eq(flatCity), ph, source); got == nil {
		t.Fatalf("top-level `city = 'NYC'` failed to bind the `city` index (regressed the positive match)")
	}

	// POSITIVE (bake-tolerance): a resolver-BAKED top-level `city` ref (the shape
	// resolveQualifiedBaked yields for a qualified predicate) must bind the LAZY
	// candidate — the mixed baked/lazy representation the leaf-name compare bridged.
	bakedCityValue, err := values.ResolveOrdinalSeedField(
		mustMatchQOV(t, source, matchRuleRowType()), 5)
	bakedCity := mustConstruct(t, bakedCityValue, err)
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
		mustMatchQOV(t, candidateElement, values.NotNullLong),
	)
	cp := predicates.NewComparisonPredicate(
		mustMatchQOV(t, queryElement, values.NotNullLong),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(9), Typ: values.NullableLong},
		},
	)
	if got := bindOrientedComparison(cp, ph, queryElement); got == nil {
		t.Fatal("primitive fanout element QOV failed to bind its candidate placeholder")
	}

	// A field below a primitive element is not merely a different semantic
	// slot: it is an impossible Value.  RFC-232 rejects that former lazy fixture
	// before the matcher can observe it.
	fieldRequestValue, err := values.FieldByName("X")
	fieldRequest := mustConstruct(t, fieldRequestValue, err)
	if _, err := values.ResolveFieldAccess(
		mustMatchQOV(t, queryElement, values.NotNullLong),
		[]values.FieldRequest{fieldRequest},
	); err == nil {
		t.Fatal("field-below-primitive fixture was admitted")
	}

	recordType := values.NewRecordType("ELEMENT", false, []values.Field{{
		Name:      "X",
		FieldType: values.NullableLong,
		Ordinal:   0,
	}})
	structuredQuery := mustMatchQOV(t, queryElement, recordType)
	structuredPlaceholder := predicates.NewPlaceholder(
		param,
		mustMatchQOV(t, candidateElement, recordType),
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

	if _, err := values.NewQuantifiedObjectValue(queryElement, values.UnknownType); err == nil {
		t.Fatal("UNKNOWN query element QOV was admitted")
	}
	if _, err := values.NewQuantifiedObjectValue(candidateElement, values.UnknownType); err == nil {
		t.Fatal("UNKNOWN candidate element QOV was admitted")
	}
}
