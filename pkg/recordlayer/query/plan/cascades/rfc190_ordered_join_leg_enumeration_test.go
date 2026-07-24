package cascades

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestRFC190OrderedJoinLegPairsCaseOneExhaustiveRetainsSourcePartitions(t *testing.T) {
	t.Parallel()

	requested := rfc190EnumerationRequest(
		properties.DistinctnessNotDistinct,
		true,
		"a",
	)
	outerExpensive := rfc190EnumerationExpression("outer-expensive")
	outerCheap := rfc190EnumerationExpression("outer-cheap")
	innerAExpensive := rfc190EnumerationExpression("inner-a-expensive")
	innerACheap := rfc190EnumerationExpression("inner-a-cheap")
	innerB := rfc190EnumerationExpression("inner-b")
	innerUnsatisfying := rfc190EnumerationExpression("inner-unsatisfying")

	rawOuters := []joinLegOrderingVariant{
		{expr: outerExpensive, maxCardinalityOne: true},
		{expr: outerCheap, maxCardinalityOne: true},
	}
	orderedInners := []joinLegOrderingVariant{
		{
			expr:           innerAExpensive,
			sourceOrdering: rfc190EnumerationOrdering(false, "source-a"),
			pulledOrdering: rfc190EnumerationOrdering(false, "a"),
		},
		{
			expr:           innerACheap,
			sourceOrdering: rfc190EnumerationOrdering(false, "source-a"),
			pulledOrdering: rfc190EnumerationOrdering(false, "a"),
		},
		{
			expr:           innerB,
			sourceOrdering: rfc190EnumerationOrdering(false, "source-b"),
			pulledOrdering: rfc190EnumerationOrdering(false, "a"),
		},
		{
			expr:           innerUnsatisfying,
			sourceOrdering: rfc190EnumerationOrdering(false, "source-c"),
			pulledOrdering: rfc190EnumerationOrdering(false, "not-a"),
		},
	}
	less := rfc190EnumerationLess(map[string]int{
		"outer-expensive":    20,
		"outer-cheap":        1,
		"inner-a-expensive":  30,
		"inner-a-cheap":      2,
		"inner-b":            3,
		"inner-unsatisfying": 0,
	})

	pairs := orderedJoinLegPairs(
		rawOuters,
		nil,
		nil,
		orderedInners,
		requested,
		less,
	)
	rfc190AssertEnumerationPairs(t, pairs, []string{
		"outer-cheap/inner-a-cheap",
		"outer-cheap/inner-b",
	})
}

func TestRFC190OrderedJoinLegPairsCaseOneNonExhaustiveRollsUpPartitions(t *testing.T) {
	t.Parallel()

	outerExpensive := rfc190EnumerationExpression("outer-expensive")
	outerCheap := rfc190EnumerationExpression("outer-cheap")
	innerAExpensive := rfc190EnumerationExpression("inner-a-expensive")
	innerACheap := rfc190EnumerationExpression("inner-a-cheap")
	innerB := rfc190EnumerationExpression("inner-b")

	pairs := orderedJoinLegPairs(
		[]joinLegOrderingVariant{
			{expr: outerExpensive, maxCardinalityOne: true},
			{expr: outerCheap, maxCardinalityOne: true},
		},
		nil,
		nil,
		[]joinLegOrderingVariant{
			{
				expr:           innerAExpensive,
				sourceOrdering: rfc190EnumerationOrdering(false, "source-a"),
				pulledOrdering: rfc190EnumerationOrdering(false, "a"),
			},
			{
				expr:           innerACheap,
				sourceOrdering: rfc190EnumerationOrdering(false, "source-a"),
				pulledOrdering: rfc190EnumerationOrdering(false, "a"),
			},
			{
				expr:           innerB,
				sourceOrdering: rfc190EnumerationOrdering(false, "source-b"),
				pulledOrdering: rfc190EnumerationOrdering(false, "a"),
			},
		},
		rfc190EnumerationRequest(
			properties.DistinctnessNotDistinct,
			false,
			"a",
		),
		rfc190EnumerationLess(map[string]int{
			"outer-expensive":   20,
			"outer-cheap":       1,
			"inner-a-expensive": 30,
			"inner-a-cheap":     2,
			"inner-b":           3,
		}),
	)
	rfc190AssertEnumerationPairs(t, pairs, []string{
		"outer-cheap/inner-a-cheap",
	})
}

func TestRFC190OrderedJoinLegPairsCaseTwoADistinctRetainsOuterPartitions(t *testing.T) {
	t.Parallel()

	outerAExpensive := rfc190EnumerationExpression("outer-a-expensive")
	outerACheap := rfc190EnumerationExpression("outer-a-cheap")
	outerB := rfc190EnumerationExpression("outer-b")
	outerNonDistinct := rfc190EnumerationExpression("outer-nondistinct")
	innerNonDistinct := rfc190EnumerationExpression("inner-nondistinct")
	innerDistinct := rfc190EnumerationExpression("inner-distinct")

	orderedOuters := []joinLegOrderingVariant{
		{
			expr:           outerAExpensive,
			sourceOrdering: rfc190EnumerationOrdering(true, "source-a"),
			pulledOrdering: rfc190EnumerationOrdering(true, "a"),
		},
		{
			expr:           outerACheap,
			sourceOrdering: rfc190EnumerationOrdering(true, "source-a"),
			pulledOrdering: rfc190EnumerationOrdering(true, "a"),
		},
		{
			expr:           outerB,
			sourceOrdering: rfc190EnumerationOrdering(true, "source-b"),
			pulledOrdering: rfc190EnumerationOrdering(true, "a"),
		},
		{
			expr:           outerNonDistinct,
			sourceOrdering: rfc190EnumerationOrdering(false, "source-c"),
			pulledOrdering: rfc190EnumerationOrdering(false, "a"),
		},
	}
	rawInners := []joinLegOrderingVariant{
		{
			expr:           innerNonDistinct,
			sourceOrdering: rfc190EnumerationOrdering(false, "inner"),
			pulledOrdering: rfc190EnumerationOrdering(false, "inner"),
		},
		{
			expr:           innerDistinct,
			sourceOrdering: rfc190EnumerationOrdering(true, "inner"),
			pulledOrdering: rfc190EnumerationOrdering(true, "inner"),
		},
	}
	less := rfc190EnumerationLess(map[string]int{
		"outer-a-expensive": 20,
		"outer-a-cheap":     1,
		"outer-b":           2,
		"outer-nondistinct": 0,
		"inner-nondistinct": 0,
		"inner-distinct":    10,
	})

	for _, exhaustive := range []bool{false, true} {
		requested := rfc190EnumerationRequest(
			properties.DistinctnessDistinct,
			exhaustive,
			"a",
		)
		pairs := orderedJoinLegPairs(
			nil,
			rawInners,
			orderedOuters,
			nil,
			requested,
			less,
		)
		rfc190AssertEnumerationPairs(t, pairs, []string{
			"outer-a-cheap/inner-distinct",
			"outer-b/inner-distinct",
		})
	}
}

func TestRFC190OrderedJoinLegPairsCaseTwoANonDistinctRollsUpPartitions(t *testing.T) {
	t.Parallel()

	outerAExpensive := rfc190EnumerationExpression("outer-a-expensive")
	outerACheap := rfc190EnumerationExpression("outer-a-cheap")
	outerB := rfc190EnumerationExpression("outer-b")
	inner := rfc190EnumerationExpression("inner")
	orderedOuters := []joinLegOrderingVariant{
		{
			expr:           outerAExpensive,
			sourceOrdering: rfc190EnumerationOrdering(false, "source-a"),
			pulledOrdering: rfc190EnumerationOrdering(false, "a"),
		},
		{
			expr:           outerACheap,
			sourceOrdering: rfc190EnumerationOrdering(false, "source-a"),
			pulledOrdering: rfc190EnumerationOrdering(false, "a"),
		},
		{
			expr:           outerB,
			sourceOrdering: rfc190EnumerationOrdering(false, "source-b"),
			pulledOrdering: rfc190EnumerationOrdering(false, "a"),
		},
	}
	less := rfc190EnumerationLess(map[string]int{
		"outer-a-expensive": 20,
		"outer-a-cheap":     1,
		"outer-b":           2,
		"inner":             0,
	})

	for _, exhaustive := range []bool{false, true} {
		pairs := orderedJoinLegPairs(
			nil,
			[]joinLegOrderingVariant{{expr: inner}},
			orderedOuters,
			nil,
			rfc190EnumerationRequest(
				properties.DistinctnessNotDistinct,
				exhaustive,
				"a",
			),
			less,
		)
		rfc190AssertEnumerationPairs(t, pairs, []string{
			"outer-a-cheap/inner",
		})
	}
}

func TestRFC190OrderedJoinLegPairsCaseTwoBRetainsEveryOuterPartition(t *testing.T) {
	t.Parallel()

	requested := rfc190EnumerationRequest(
		properties.DistinctnessNotDistinct,
		false,
		"c",
		"b",
	)
	outerAExpensive := rfc190EnumerationExpression("outer-a-expensive")
	outerACheap := rfc190EnumerationExpression("outer-a-cheap")
	outerB := rfc190EnumerationExpression("outer-b")
	innerExpensive := rfc190EnumerationExpression("inner-expensive")
	innerCheap := rfc190EnumerationExpression("inner-cheap")

	orderedOuters := []joinLegOrderingVariant{
		{
			expr:           outerAExpensive,
			sourceOrdering: rfc190EnumerationOrdering(true, "source-a"),
			pulledOrdering: properties.NewRichOrdering(nil, nil, true),
		},
		{
			expr:           outerACheap,
			sourceOrdering: rfc190EnumerationOrdering(true, "source-a"),
			pulledOrdering: properties.NewRichOrdering(nil, nil, true),
		},
		{
			expr:           outerB,
			sourceOrdering: rfc190EnumerationOrdering(true, "source-b"),
			pulledOrdering: rfc190EnumerationFixedPrefixOrdering(true, "a", "c"),
		},
	}
	orderedInners := []joinLegOrderingVariant{
		{
			expr:           innerExpensive,
			sourceOrdering: rfc190EnumerationOrdering(false, "inner-a"),
			pulledOrdering: rfc190EnumerationFixedPrefixOrdering(false, "c", "b"),
		},
		{
			expr:           innerCheap,
			sourceOrdering: rfc190EnumerationOrdering(false, "inner-b"),
			pulledOrdering: rfc190EnumerationOrdering(false, "b"),
		},
	}
	less := rfc190EnumerationLess(map[string]int{
		"outer-a-expensive": 20,
		"outer-a-cheap":     1,
		"outer-b":           2,
		"inner-expensive":   30,
		"inner-cheap":       3,
	})

	pairs := orderedJoinLegPairs(
		nil,
		nil,
		orderedOuters,
		orderedInners,
		requested,
		less,
	)
	rfc190AssertEnumerationPairs(t, pairs, []string{
		"outer-a-cheap/inner-expensive",
		"outer-b/inner-cheap",
	})
}

func TestRFC190OrderedJoinLegPairsCaseTwoBExhaustiveRetainsInnerPartitions(t *testing.T) {
	t.Parallel()

	outerAExpensive := rfc190EnumerationExpression("outer-a-expensive")
	outerACheap := rfc190EnumerationExpression("outer-a-cheap")
	outerB := rfc190EnumerationExpression("outer-b")
	innerAExpensive := rfc190EnumerationExpression("inner-a-expensive")
	innerACheap := rfc190EnumerationExpression("inner-a-cheap")
	innerB := rfc190EnumerationExpression("inner-b")

	pairs := orderedJoinLegPairs(
		nil,
		nil,
		[]joinLegOrderingVariant{
			{
				expr:           outerAExpensive,
				sourceOrdering: rfc190EnumerationOrdering(true, "outer-source-a"),
				pulledOrdering: properties.NewRichOrdering(nil, nil, true),
			},
			{
				expr:           outerACheap,
				sourceOrdering: rfc190EnumerationOrdering(true, "outer-source-a"),
				pulledOrdering: properties.NewRichOrdering(nil, nil, true),
			},
			{
				expr:           outerB,
				sourceOrdering: rfc190EnumerationOrdering(true, "outer-source-b"),
				pulledOrdering: properties.NewRichOrdering(nil, nil, true),
			},
		},
		[]joinLegOrderingVariant{
			{
				expr:           innerAExpensive,
				sourceOrdering: rfc190EnumerationOrdering(false, "inner-source-a"),
				pulledOrdering: rfc190EnumerationOrdering(false, "b"),
			},
			{
				expr:           innerACheap,
				sourceOrdering: rfc190EnumerationOrdering(false, "inner-source-a"),
				pulledOrdering: rfc190EnumerationOrdering(false, "b"),
			},
			{
				expr:           innerB,
				sourceOrdering: rfc190EnumerationOrdering(false, "inner-source-b"),
				pulledOrdering: rfc190EnumerationOrdering(false, "b"),
			},
		},
		rfc190EnumerationRequest(
			properties.DistinctnessNotDistinct,
			true,
			"b",
		),
		rfc190EnumerationLess(map[string]int{
			"outer-a-expensive": 20,
			"outer-a-cheap":     1,
			"outer-b":           2,
			"inner-a-expensive": 30,
			"inner-a-cheap":     3,
			"inner-b":           4,
		}),
	)
	rfc190AssertEnumerationPairs(t, pairs, []string{
		"outer-a-cheap/inner-a-cheap",
		"outer-a-cheap/inner-b",
		"outer-b/inner-a-cheap",
		"outer-b/inner-b",
	})
}

func TestRFC190OrderedJoinLegPairsDeduplicatesPhysicalPairs(t *testing.T) {
	t.Parallel()

	requested := rfc190EnumerationRequest(
		properties.DistinctnessNotDistinct,
		true,
		"a",
	)
	outer := rfc190EnumerationExpression("outer")
	firstInner := rfc190EnumerationExpression("same-inner")
	equalInner := rfc190EnumerationExpression("same-inner")

	pairs := orderedJoinLegPairs(
		[]joinLegOrderingVariant{{
			expr:              outer,
			maxCardinalityOne: true,
		}},
		nil,
		nil,
		[]joinLegOrderingVariant{
			{
				expr:           firstInner,
				sourceOrdering: rfc190EnumerationOrdering(false, "source-a"),
				pulledOrdering: rfc190EnumerationOrdering(false, "a"),
			},
			{
				expr:           equalInner,
				sourceOrdering: rfc190EnumerationOrdering(false, "source-b"),
				pulledOrdering: rfc190EnumerationOrdering(false, "a"),
			},
		},
		requested,
		rfc190EnumerationLess(map[string]int{
			"outer":      1,
			"same-inner": 2,
		}),
	)
	rfc190AssertEnumerationPairs(t, pairs, []string{"outer/same-inner"})
}

func rfc190EnumerationOrdering(
	distinct bool,
	columnNames ...string,
) *properties.RichOrdering {
	bindingMap := make(map[values.Value][]properties.OrderingBinding, len(columnNames))
	keys := make([]values.Value, 0, len(columnNames))
	for _, columnName := range columnNames {
		key := &values.FieldValue{Field: columnName, Typ: values.UnknownType}
		bindingMap[key] = []properties.OrderingBinding{
			properties.SortedBinding(properties.ProvidedSortOrderAscending),
		}
		keys = append(keys, key)
	}
	return properties.NewRichOrdering(bindingMap, keys, distinct)
}

func rfc190EnumerationFixedPrefixOrdering(
	distinct bool,
	fixedColumn string,
	sortedColumn string,
) *properties.RichOrdering {
	fixed := &values.FieldValue{Field: fixedColumn, Typ: values.UnknownType}
	sorted := &values.FieldValue{Field: sortedColumn, Typ: values.UnknownType}
	return properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			fixed:  {properties.FixedBinding("fixed")},
			sorted: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		},
		[]values.Value{fixed, sorted},
		distinct,
	)
}

func rfc190EnumerationRequest(
	distinctness properties.Distinctness,
	exhaustive bool,
	columnNames ...string,
) *properties.RequestedOrdering {
	parts := make([]properties.RequestedOrderingPart, 0, len(columnNames))
	for _, columnName := range columnNames {
		parts = append(parts, properties.RequestedOrderingPart{
			Value: &values.FieldValue{
				Field: columnName,
				Typ:   values.UnknownType,
			},
			SortOrder: properties.RequestedSortOrderAscending,
		})
	}
	return properties.NewRequestedOrdering(parts, distinctness, exhaustive)
}

func rfc190EnumerationExpression(name string) expressions.RelationalExpression {
	return plans.NewRecordQueryScanPlan(
		[]string{name},
		values.UnknownType,
		false,
	)
}

func rfc190EnumerationExpressionName(
	expression expressions.RelationalExpression,
) string {
	scan, ok := expression.(*plans.RecordQueryScanPlan)
	if !ok {
		panic("RFC-190 enumeration test expected a scan expression")
	}
	recordTypes := scan.GetRecordTypes()
	if len(recordTypes) != 1 {
		panic("RFC-190 enumeration test expected one record type")
	}
	return recordTypes[0]
}

func rfc190EnumerationLess(
	ranks map[string]int,
) func(expressions.RelationalExpression, expressions.RelationalExpression) bool {
	return func(
		left expressions.RelationalExpression,
		right expressions.RelationalExpression,
	) bool {
		return ranks[rfc190EnumerationExpressionName(left)] <
			ranks[rfc190EnumerationExpressionName(right)]
	}
}

func rfc190AssertEnumerationPairs(
	t *testing.T,
	pairs []joinLegOrderingPair,
	want []string,
) {
	t.Helper()

	got := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		got = append(got,
			rfc190EnumerationExpressionName(pair.outer)+"/"+
				rfc190EnumerationExpressionName(pair.inner))
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("ordered join-leg pairs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ordered join-leg pairs = %v, want %v", got, want)
		}
	}
}
