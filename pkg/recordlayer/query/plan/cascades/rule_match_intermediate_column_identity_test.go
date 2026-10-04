package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPromotedFloatScanBound(t *testing.T) {
	t.Parallel()
	column, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("FLOAT_BOUND"), values.NullableFloat)
	if err != nil {
		t.Fatal(err)
	}
	promotion := values.NewPromoteValue(column, values.NullableDouble)
	for _, tc := range []struct {
		name       string
		input      float64
		op, wantOp predicates.ComparisonType
		want       any
	}{
		{"exact", 3, predicates.ComparisonEquals, predicates.ComparisonEquals, float32(3)},
		{"inexact equality", 0.1, predicates.ComparisonEquals, predicates.ComparisonEquals, float64(0.1)},
		{"upper floor", 0.1, predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq, math.Nextafter32(float32(0.1), float32(math.Inf(-1)))},
		{"lower ceiling", 0.1, predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq, float32(0.1)},
		{"upper overflow", 1e40, predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq, float32(math.MaxFloat32)},
		{"lower overflow", -1e40, predicates.ComparisonGreaterThanEq, predicates.ComparisonGreaterThanEq, -float32(math.MaxFloat32)},
		{"tiny positive", math.SmallestNonzeroFloat64, predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq, float32(math.SmallestNonzeroFloat32)},
		{"tiny negative", -math.SmallestNonzeroFloat64, predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq, -float32(math.SmallestNonzeroFloat32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			operand := &values.ConstantValue{Value: tc.input, Typ: values.NotNullDouble}
			original := comparisonOrientation{column: promotion, comparison: predicates.Comparison{Type: tc.op, Operand: operand}}
			got := narrowPromotedFloatScanBound(original)
			bound, ok := values.EvaluateConstant(got.comparison.Operand)
			if !ok || got.column != column || got.comparison.Type != tc.wantOp || bound != tc.want {
				t.Fatalf("scan bound = %v %v (%T), want operator %v bound %v (%T)", got.comparison.Type, bound, bound, tc.wantOp, tc.want, tc.want)
			}
			if original.column != promotion || original.comparison.Type != tc.op || original.comparison.Operand != operand || operand.Value != tc.input {
				t.Fatal("scan inversion mutated the semantic predicate")
			}
		})
	}
}

// A block Select carries its WHERE as a PredicateWithValueAndRanges, which
// binds through the subsumption binder: a promoted FLOAT column still narrows
// its constant bound onto the column, as the comparison binder does.
func TestPromotedFloatBoundNarrowsInBothBinders(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("FLOAT_ROW", false, []values.Field{{Name: "F", FieldType: values.NullableFloat, Ordinal: 0}})
	scan, err := expressions.NewFullUnorderedScanExpression([]string{"FLOAT_ROW"}, rowType)
	if err != nil {
		t.Fatal(err)
	}
	candidate := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	column := mustMatchField(t, mustMatchQOV(t, candidate.GetAlias(), rowType), "F")
	ph := predicates.NewPlaceholder(values.NamedCorrelationIdentifier("p0"), column)
	cp := predicates.NewComparisonPredicate(values.NewPromoteValue(column, values.NullableDouble),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: 1.5, Typ: values.NotNullDouble}})
	for name, got := range map[string]*predicates.ComparisonRange{
		"comparison": bindOrientedComparison(cp, ph, candidate.GetAlias()),
		"subsumption": func() *predicates.ComparisonRange {
			r, _ := bindSelectSubsumptionComparisonToPlaceholder(cp, ph, []expressions.Quantifier{candidate})
			return r
		}(),
	} {
		if got == nil || len(got.GetComparisons()) != 1 {
			t.Fatalf("%s binder: promoted FLOAT equality did not bind the FLOAT column", name)
		}
		if bound, ok := values.EvaluateConstant(got.GetComparisons()[0].Operand); !ok || bound != float32(1.5) {
			t.Fatalf("%s binder: bound %v (%T), want float32 1.5", name, bound, bound)
		}
	}
}

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
