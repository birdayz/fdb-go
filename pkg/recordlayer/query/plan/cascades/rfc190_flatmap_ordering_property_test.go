package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// These tests pin the three branches of Java 4.12.11
// OrderingProperty.visitFlatMapPlan. They deliberately use finals-only
// singleton child references: only that private-reference shape prevents a
// later memo winner change from invalidating a sort-elision proof.

func TestRFC190FlatMapOrderingCase1MaxOneOuterUsesInner(t *testing.T) {
	t.Parallel()

	outerKey := rfc190Field("outer_pk")
	innerKey := rfc190Field("inner_key")
	outer := plans.NewRecordQueryScanPlan(
		[]string{"OUTER"}, values.UnknownType, false,
	).WithPrimaryKey([]values.Value{outerKey}).
		WithScanComparisons([]*predicates.ComparisonRange{rfc190EqualityRange(t, int64(7))})
	inner := rfc190Index("INNER_IDX", "inner_key", true, true)

	outerMax := computeCardinalities(outer, outer).GetMaxCardinality()
	if outerMax.IsUnknown() || outerMax.Value() != 1 {
		t.Fatalf("case 1 precondition: outer max cardinality = %v, want 1", outerMax)
	}

	flatMap, _, innerAlias, _ := rfc190FlatMap(
		"case1", expressions.FinalOf(outer), expressions.FinalOf(inner),
		outerKey, innerKey,
	)
	got := computeWrapperRichOrdering(flatMap)
	rfc190AssertOrdering(t, got, true, []rfc190ExpectedOrderingPart{
		{
			value:     rfc190OutputField(innerAlias, "inner_out"),
			sortOrder: properties.ProvidedSortOrderDescending,
		},
	})
}

func TestRFC190FlatMapOrderingCase2aNonDistinctOuterUsesOuterOnly(t *testing.T) {
	t.Parallel()

	outerKey := rfc190Field("outer_key")
	innerKey := rfc190Field("inner_key")
	outer := plans.NewRecordQueryScanPlan(
		[]string{"OUTER"}, values.UnknownType, true,
	).WithPrimaryKey([]values.Value{outerKey})
	// Make the ignored inner suffix visibly different and strict. Case 2a must
	// still report only the non-distinct outer ordering.
	inner := rfc190Index("INNER_IDX", "inner_key", false, true)

	outerMax := computeCardinalities(outer, outer).GetMaxCardinality()
	if !outerMax.IsUnknown() {
		t.Fatalf("case 2a precondition: outer max cardinality = %v, want unknown", outerMax)
	}
	if computeWrapperRichOrdering(outer).IsDistinct() {
		t.Fatal("case 2a precondition: outer ordering must be non-distinct")
	}

	flatMap, outerAlias, _, _ := rfc190FlatMap(
		"case2a", expressions.FinalOf(outer), expressions.FinalOf(inner),
		outerKey, innerKey,
	)
	got := computeWrapperRichOrdering(flatMap)
	rfc190AssertOrdering(t, got, false, []rfc190ExpectedOrderingPart{
		{
			value:     rfc190OutputField(outerAlias, "outer_out"),
			sortOrder: properties.ProvidedSortOrderDescending,
		},
	})
}

func TestRFC190FlatMapOrderingCase2bDistinctOuterConcatenatesInner(t *testing.T) {
	t.Parallel()

	outerKey := rfc190Field("outer_key")
	innerKey := rfc190Field("inner_key")
	outer := rfc190Index("OUTER_IDX", "outer_key", false, true)
	// The inner is intentionally non-distinct. Java concatenation requires the
	// left/outer ordering to be distinct, but takes RESULT distinctness from
	// the right/inner ordering.
	inner := plans.NewRecordQueryScanPlan(
		[]string{"INNER"}, values.UnknownType, true,
	).WithPrimaryKey([]values.Value{innerKey})

	if !computeWrapperRichOrdering(outer).IsDistinct() {
		t.Fatal("case 2b precondition: outer ordering must be distinct")
	}
	if computeWrapperRichOrdering(inner).IsDistinct() {
		t.Fatal("case 2b precondition: inner ordering must be non-distinct")
	}

	flatMap, outerAlias, innerAlias, resultValue := rfc190FlatMap(
		"case2b", expressions.FinalOf(outer), expressions.FinalOf(inner),
		outerKey, innerKey,
	)
	outerOutput := rfc190OutputField(outerAlias, "outer_out")
	innerOutput := rfc190OutputField(innerAlias, "inner_out")
	got := computeWrapperRichOrdering(flatMap)
	rfc190AssertOrdering(t, got, false, []rfc190ExpectedOrderingPart{
		{value: outerOutput, sortOrder: properties.ProvidedSortOrderAscending},
		{value: innerOutput, sortOrder: properties.ProvidedSortOrderDescending},
	})

	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: outerOutput, SortOrder: properties.RequestedSortOrderAscending},
			{Value: innerOutput, SortOrder: properties.RequestedSortOrderDescending},
		},
		properties.DistinctnessNotDistinct,
		false,
	)
	pulledOuter := computeWrapperRichOrdering(outer).
		PullUpThroughValue(resultValue, outerAlias)
	if pulledOuter.Satisfies(requested) {
		t.Fatal("case 2b precondition: outer alone unexpectedly satisfies the combined request")
	}
	if !got.Satisfies(requested) {
		t.Fatal("concatenated FlatMap ordering does not satisfy outer-prefix + inner-suffix request")
	}

	outerKeyString := values.ExplainValue(got.GetKeys()[0])
	innerKeyString := values.ExplainValue(got.GetKeys()[1])
	if !got.OrderingSet().DependencyMap().Contains(innerKeyString, outerKeyString) {
		t.Fatalf("concatenated ordering lacks dependency %s -> %s",
			innerKeyString, outerKeyString)
	}
}

func TestRFC190FlatMapOrderingRequiresExactFinalChildReferences(t *testing.T) {
	t.Parallel()

	t.Run("exploratory_child_is_not_safe", func(t *testing.T) {
		t.Parallel()

		outerKey := rfc190Field("outer_key")
		innerKey := rfc190Field("inner_key")
		outer := plans.NewRecordQueryScanPlan(
			[]string{"OUTER"}, values.UnknownType, false,
		).WithPrimaryKey([]values.Value{outerKey})
		inner := rfc190Index("INNER_IDX", "inner_key", false, true)

		flatMap, _, _, _ := rfc190FlatMap(
			"unsafe_exploratory",
			expressions.InitialOf(outer),
			expressions.FinalOf(inner),
			outerKey,
			innerKey,
		)
		rfc190AssertUnsafeJoinOrdering(t, flatMap)
	})

	t.Run("multi_final_child_is_not_safe", func(t *testing.T) {
		t.Parallel()

		outerKey := rfc190Field("outer_key")
		innerKey := rfc190Field("inner_key")
		outer := plans.NewRecordQueryScanPlan(
			[]string{"OUTER"}, values.UnknownType, false,
		).WithPrimaryKey([]values.Value{outerKey})
		inner := rfc190Index("INNER_ASC", "inner_key", false, true)
		innerAlternative := rfc190Index("INNER_DESC", "inner_key", true, true)
		innerRef := expressions.FinalOf(inner)
		if !innerRef.InsertFinal(innerAlternative) {
			t.Fatal("failed to construct multi-final inner reference")
		}

		flatMap, _, _, _ := rfc190FlatMap(
			"unsafe_multi_final",
			expressions.FinalOf(outer),
			innerRef,
			outerKey,
			innerKey,
		)
		rfc190AssertUnsafeJoinOrdering(t, flatMap)
	})
}

type rfc190ExpectedOrderingPart struct {
	value     values.Value
	sortOrder properties.ProvidedSortOrder
}

func rfc190Field(name string) values.Value {
	return &values.FieldValue{Field: name, Typ: values.UnknownType}
}

func rfc190OutputField(
	alias values.CorrelationIdentifier,
	name string,
) values.Value {
	return values.NewFieldValue(
		values.NewQuantifiedObjectValue(alias),
		name,
		values.UnknownType,
	)
}

func rfc190EqualityRange(t *testing.T, literal any) *predicates.ComparisonRange {
	t.Helper()
	comparison := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to build equality comparison range")
	}
	return merged.Range
}

func rfc190Index(
	indexName string,
	columnName string,
	reverse bool,
	distinct bool,
) *plans.RecordQueryIndexPlan {
	index := plans.NewRecordQueryIndexPlan(
		indexName, nil, []string{"T"}, values.UnknownType, reverse,
	).WithIndexMetadata([]string{columnName}, nil, false)
	if distinct {
		index = index.WithStrictlySorted()
	}
	return index
}

func rfc190FlatMap(
	name string,
	outerRef *expressions.Reference,
	innerRef *expressions.Reference,
	outerKey values.Value,
	innerKey values.Value,
) (*plans.RecordQueryFlatMapPlan, values.CorrelationIdentifier, values.CorrelationIdentifier, values.Value) {
	outerAlias := values.NamedCorrelationIdentifier("rfc190_" + name + "_outer")
	innerAlias := values.NamedCorrelationIdentifier("rfc190_" + name + "_inner")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "outer_out", Value: outerKey},
		values.RecordConstructorField{Name: "inner_out", Value: innerKey},
	)
	flatMap := plans.NewRecordQueryFlatMapPlanFromQuantifiers(
		expressions.NamedPhysicalQuantifier(outerAlias, outerRef),
		expressions.NamedPhysicalQuantifier(innerAlias, innerRef),
		outerAlias,
		innerAlias,
		resultValue,
		false,
	)
	return flatMap, outerAlias, innerAlias, resultValue
}

func rfc190AssertOrdering(
	t *testing.T,
	got *properties.RichOrdering,
	wantDistinct bool,
	want []rfc190ExpectedOrderingPart,
) {
	t.Helper()
	if got == nil {
		t.Fatal("computed nil rich ordering")
	}
	if got.IsDistinct() != wantDistinct {
		t.Fatalf("distinctness = %v, want %v", got.IsDistinct(), wantDistinct)
	}
	keys := got.GetKeys()
	if len(keys) != len(want) {
		t.Fatalf("ordering keys = %v, want %d keys", rfc190ExplainValues(keys), len(want))
	}
	for i := range want {
		if !values.ValuesStructurallyEqual(keys[i], want[i].value) {
			t.Fatalf("ordering key %d = %s, want %s",
				i, values.ExplainValue(keys[i]), values.ExplainValue(want[i].value))
		}
		gotSortOrder := properties.SortOrderOf(got.GetBindingMap()[keys[i]])
		if gotSortOrder != want[i].sortOrder {
			t.Fatalf("ordering key %d (%s) sort order = %v, want %v",
				i, values.ExplainValue(keys[i]), gotSortOrder, want[i].sortOrder)
		}
	}
}

func rfc190AssertUnsafeJoinOrdering(
	t *testing.T,
	flatMap *plans.RecordQueryFlatMapPlan,
) {
	t.Helper()
	got := computeWrapperRichOrdering(flatMap)
	if got == nil {
		t.Fatal("unsafe FlatMap returned nil instead of conservative empty ordering")
	}
	if len(got.GetKeys()) != 0 {
		t.Fatalf("unsafe FlatMap advertised ordering keys %v", rfc190ExplainValues(got.GetKeys()))
	}
	if computeWrapperOrdering(flatMap).IsKnown {
		t.Fatal("unsafe FlatMap advertised a known plain ordering")
	}
}

func rfc190ExplainValues(items []values.Value) []string {
	explained := make([]string, len(items))
	for i, item := range items {
		explained[i] = values.ExplainValue(item)
	}
	return explained
}
