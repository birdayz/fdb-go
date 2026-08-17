package plans

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func plannerDynamicEquality(t *testing.T, typ values.Type) *predicates.ComparisonRange {
	t.Helper()
	comparison := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: &values.ParameterValue{Ordinal: 1, Typ: typ},
	}
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to build dynamic equality")
	}
	return merged.Range
}

func TestPhysicalEqualityOrdering_KnownIntegerAuthorityKeepsUnknownRHSFixed(t *testing.T) {
	t.Parallel()
	plan := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan(
			"IDX",
			[]*predicates.ComparisonRange{plannerDynamicEquality(t, values.UnknownType)},
			[]string{"T"}, exactTestRecordType(), false,
		)
	}).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithIndexMetadata([]string{"V"}, []string{"ID"}, false).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong})
	plain := plan.HintOrdering()
	if !plain.IsKnown || len(plain.Keys) != 1 {
		t.Fatalf("HintOrdering = %#v, want only PK suffix: authoritative LONG has no signed-zero fanout", plain)
	}
	field, ok := values.AsFieldValue(plain.Keys[0])
	if !ok || field.DisplayName() != "ID" {
		t.Fatalf("HintOrdering key = %#v, want ID", plain.Keys[0])
	}
	rich := plan.HintRichOrdering()
	bindings := rich.GetBindingMap()
	first := rich.GetKeys()[0]
	if got := bindings[first]; len(got) != 1 || !got[0].IsFixed() {
		t.Fatalf("LONG dynamic binding = %#v, want Fixed", got)
	}
}

func TestIndexPrimaryKeySuffixTypesParticipateInIdentityAndCopies(t *testing.T) {
	t.Parallel()

	base := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("IDX", nil, []string{"T"}, exactTestRecordType(), false)
	}).
		WithKeyComponentTypes([]values.Type{values.NotNullString}).
		WithIndexMetadata([]string{"STATUS"}, []string{"ID"}, false)
	longPlan := base.WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong})
	doublePlan := base.WithPrimaryKeyComponentTypes([]values.Type{values.NotNullDouble})
	if longPlan.EqualsPlanWithoutChildren(doublePlan) ||
		longPlan.HashCodeWithoutChildren() == doublePlan.HashCodeWithoutChildren() {
		t.Fatal("plans with LONG versus DOUBLE PK suffix metadata coalesced")
	}
	copied := doublePlan.WithScanComparisons(nil).WithStrictlySorted()
	got := copied.GetPrimaryKeyComponentTypes()
	if len(got) != 1 || got[0].Code() != values.TypeCodeDouble {
		t.Fatalf("copy lost DOUBLE PK suffix metadata: %v", got)
	}
	exact := base.WithPrimaryKeyComponentTypes([]values.Type{
		values.NotNullLong, values.NotNullDouble,
	}).GetPrimaryKeyComponentTypes()
	if len(exact) != 1 || exact[0].Code() != values.TypeCodeLong {
		t.Fatalf("PK metadata was not normalized to the exact name width: %v", exact)
	}
	replaced := longPlan.WithIndexMetadata(
		[]string{"STATUS"}, []string{"OTHER_ID"}, false,
	).GetPrimaryKeyComponentTypes()
	if len(replaced) != 1 || replaced[0].Code() != values.TypeCodeUnknown {
		t.Fatalf("replacement PK names retained stale positional types: %v", replaced)
	}
}

func TestPhysicalEqualityCardinality_PrimaryAndUniqueCartesianMultiplicity(t *testing.T) {
	t.Parallel()
	comps := []*predicates.ComparisonRange{pkOrderingEq(t, float64(0)), pkOrderingEq(t, float32(0))}
	types := []values.Type{values.NotNullDouble, values.NotNullFloat}
	pk := []values.Value{testField(t, "V1", values.NotNullDouble), testField(t, "V2", values.NotNullFloat)}
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	}).
		WithPrimaryKey(pk).
		WithScanComparisons(comps).
		WithKeyComponentTypes(types)
	index := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("U", comps, []string{"T"}, exactTestRecordType(), false)
	}).
		WithKeyComponentTypes(types).
		WithIndexMetadata([]string{"V1", "V2"}, []string{"ID"}, true).
		WithPrimaryKeyComponentTypes([]values.Type{values.NotNullLong})

	for name, plan := range map[string]CostedPlan{"primary": scan, "unique": index} {
		name, plan := name, plan
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bounds := plan.ProvenCardinalities(nil)
			if bounds.Max.IsUnknown() || bounds.Max.Value() != 4 {
				t.Fatalf("max cardinality = %+v, want exact physical multiplicity 4", bounds.Max)
			}
			cost := plan.HintCost(nil, properties.FixedStatistics{Cardinality: 1000})
			if cost.Cardinality != 4 {
				t.Fatalf("cost cardinality = %v, want 4", cost.Cardinality)
			}
			wantCPU := properties.FetchCPU + 3*properties.ScanCPU + properties.PhysicalRangeSeekCost
			if cost.CPU != wantCPU {
				t.Fatalf("cost CPU = %v, want %v (four rows, two disjoint seeks; terminal zero widened)", cost.CPU, wantCPU)
			}
		})
	}
}

func TestPhysicalEqualityCardinality_DynamicFloatUnknownButDynamicIntegerOne(t *testing.T) {
	t.Parallel()
	mk := func(physicalType values.Type) *RecordQueryIndexPlan {
		return mustChecked(t, func() (*RecordQueryIndexPlan, error) {
			return NewRecordQueryIndexPlan(
				"U", []*predicates.ComparisonRange{plannerDynamicEquality(t, values.UnknownType)},
				[]string{"T"}, exactTestRecordType(), false,
			)
		}).
			WithKeyComponentTypes([]values.Type{physicalType}).
			WithIndexMetadata([]string{"V"}, []string{"ID"}, true)
	}
	if max := mk(values.NotNullDouble).ProvenCardinalities(nil).Max; !max.IsUnknown() {
		t.Fatalf("dynamic DOUBLE max = %+v, want unknown because runtime NaN is unsupported", max)
	}
	integerMax := mk(values.NotNullLong).ProvenCardinalities(nil).Max
	if integerMax.IsUnknown() || integerMax.Value() != 1 {
		t.Fatalf("dynamic LONG max = %+v, want 1", integerMax)
	}
}

func TestUniqueIndexCardinality_NullableKeysUseNullsDistinctSemantics(t *testing.T) {
	t.Parallel()
	rangeOf := func(comparison predicates.Comparison) *predicates.ComparisonRange {
		t.Helper()
		merged := predicates.EmptyComparisonRange().Merge(&comparison)
		if !merged.Ok {
			t.Fatalf("failed to build %s range", comparison.Type.Symbol())
		}
		return merged.Range
	}
	isNull := rangeOf(predicates.Comparison{Type: predicates.ComparisonIsNull})
	nullSafeNull := rangeOf(predicates.Comparison{
		Type: predicates.ComparisonNotDistinctFrom, Operand: values.LiteralValue(nil),
	})
	ordinaryParameter := plannerDynamicEquality(t, values.NullableLong)

	for _, test := range []struct {
		name        string
		comparison  *predicates.ComparisonRange
		physical    values.Type
		wantKnown   bool
		wantMaximum int64
	}{
		{name: "nullable IS NULL is not unique", comparison: isNull, physical: values.NullableLong},
		{name: "nullable null-safe NULL is not unique", comparison: nullSafeNull, physical: values.NullableLong},
		{name: "NOT NULL IS NULL is empty", comparison: isNull, physical: values.NotNullLong, wantKnown: true},
		{name: "NOT NULL null-safe NULL is empty", comparison: nullSafeNull, physical: values.NotNullLong, wantKnown: true},
		{name: "ordinary nullable parameter is empty-or-unique", comparison: ordinaryParameter, physical: values.NullableLong, wantKnown: true, wantMaximum: 1},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
				return NewRecordQueryIndexPlan(
					"U", []*predicates.ComparisonRange{test.comparison},
					[]string{"T"}, exactTestRecordType(), false,
				)
			}).WithKeyComponentTypes([]values.Type{test.physical}).
				WithIndexMetadata([]string{"V"}, []string{"ID"}, true)
			bound := plan.ProvenCardinalities(nil).Max
			if bound.IsUnknown() == test.wantKnown {
				t.Fatalf("maximum known = %v, want %v", !bound.IsUnknown(), test.wantKnown)
			}
			if test.wantKnown && bound.Value() != test.wantMaximum {
				t.Fatalf("maximum = %d, want %d", bound.Value(), test.wantMaximum)
			}
			if got := isProvablePointProbe(plan); got != (test.wantKnown && test.wantMaximum == 1) {
				t.Fatalf("isProvablePointProbe = %v", got)
			}
			cost := plan.HintCost(nil, properties.FixedStatistics{Cardinality: 1000})
			if test.wantKnown && cost.Cardinality != float64(test.wantMaximum) {
				t.Fatalf("proven cost cardinality = %v, want %d", cost.Cardinality, test.wantMaximum)
			}
			if !test.wantKnown && cost.Cardinality <= 1 {
				t.Fatalf("nullable NULL UNIQUE probe cost = %+v, must remain an unbounded bucket estimate", cost)
			}
		})
	}
}

func TestPhysicalEqualityCost_EveryPluralLeafPaysSeekSetup(t *testing.T) {
	t.Parallel()
	stats := properties.FixedStatistics{Cardinality: 1000}
	zeroComps := []*predicates.ComparisonRange{pkOrderingEq(t, float64(0)), pkOrderingEq(t, int64(5))}
	nonzeroComps := []*predicates.ComparisonRange{pkOrderingEq(t, float64(1)), pkOrderingEq(t, int64(5))}
	types := []values.Type{values.NotNullDouble, values.NotNullLong}
	mkIndex := func(comps []*predicates.ComparisonRange) *RecordQueryIndexPlan {
		return mustChecked(t, func() (*RecordQueryIndexPlan, error) {
			return NewRecordQueryIndexPlan("I", comps, []string{"T"}, exactTestRecordType(), false)
		}).
			WithKeyComponentTypes(types).
			WithIndexMetadata([]string{"V", "W"}, []string{"ID"}, false)
	}
	zeroIndex, nonzeroIndex := mkIndex(zeroComps).HintCost(nil, stats), mkIndex(nonzeroComps).HintCost(nil, stats)
	if zeroIndex.Cardinality != nonzeroIndex.Cardinality {
		t.Fatalf("non-unique estimates differ in cardinality: zero=%+v nonzero=%+v", zeroIndex, nonzeroIndex)
	}
	if zeroIndex.CPU-nonzeroIndex.CPU != properties.PhysicalRangeSeekCost {
		t.Fatalf("non-unique zero extra CPU = %v, want one seek setup %v", zeroIndex.CPU-nonzeroIndex.CPU, properties.PhysicalRangeSeekCost)
	}

	mkAggregate := func(comps []*predicates.ComparisonRange) *RecordQueryAggregateIndexPlan {
		index := mkIndex(comps).WithPhysicalGroupingPrefixCount(2)
		return mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
			return NewRecordQueryAggregateIndexPlan(index, "T", exactTestRecordType(), "SUM")
		}).
			WithGroupColumns([]string{"V", "W"}, "A")
	}
	zeroAggregate, nonzeroAggregate := mkAggregate(zeroComps).HintCost(nil, stats), mkAggregate(nonzeroComps).HintCost(nil, stats)
	if zeroAggregate.CPU-nonzeroAggregate.CPU != properties.PhysicalRangeSeekCost {
		t.Fatalf("aggregate zero extra CPU = %v, want one seek setup %v", zeroAggregate.CPU-nonzeroAggregate.CPU, properties.PhysicalRangeSeekCost)
	}
}

func TestPhysicalEqualityCost_EmptyNonUniqueProbeIsNotFree(t *testing.T) {
	t.Parallel()
	plan := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan(
			"I", []*predicates.ComparisonRange{pkOrderingEq(t, nil)},
			[]string{"T"}, exactTestRecordType(), false,
		)
	}).
		WithKeyComponentTypes([]values.Type{values.NullableDouble}).
		WithIndexMetadata([]string{"V"}, []string{"ID"}, false)
	cost := plan.HintCost(nil, properties.FixedStatistics{Cardinality: 1000})
	if cost.Cardinality != 0 || cost.CPU != properties.PhysicalRangeSeekCost {
		t.Fatalf("empty probe cost = %+v, want zero rows plus one seek setup", cost)
	}
}

func TestPhysicalEqualityVectorCost_FullFanoutAndPartialUnknownPartitions(t *testing.T) {
	t.Parallel()
	k := values.LiteralValue(int64(7))
	query := values.LiteralValue([]float64{1, 2})
	full := mustChecked(t, func() (*RecordQueryVectorIndexPlan, error) {
		return NewRecordQueryVectorIndexPlan(
			"V", []*predicates.ComparisonRange{pkOrderingEq(t, float64(0)), pkOrderingEq(t, int64(5))},
			query, k, predicates.ComparisonDistanceRankLessThanOrEq, nil, nil,
			[]string{"T"}, exactTestRecordType(),
		)
	}).
		WithPartitionColumns([]string{"P1", "P2"}).
		WithPartitionKeyComponentTypes([]values.Type{values.NotNullDouble, values.NotNullLong})
	fullCost := full.HintCost(nil, nil)
	if fullCost.Cardinality != 14 {
		t.Fatalf("fully bound vector cardinality = %v, want 2*k=14", fullCost.Cardinality)
	}
	wantFullCPU := 14*properties.ScanCPU*properties.PhysicalWrapperCostMultiplier + properties.PhysicalRangeSeekCost
	if fullCost.CPU != wantFullCPU {
		t.Fatalf("fully bound vector CPU = %v, want %v", fullCost.CPU, wantFullCPU)
	}

	partial := mustChecked(t, func() (*RecordQueryVectorIndexPlan, error) {
		return NewRecordQueryVectorIndexPlan(
			"V", []*predicates.ComparisonRange{pkOrderingEq(t, float64(0))},
			query, k, predicates.ComparisonDistanceRankLessThanOrEq, nil, nil,
			[]string{"T"}, exactTestRecordType(),
		)
	}).
		WithPartitionColumns([]string{"P1", "P2"}).
		WithPartitionKeyComponentTypes([]values.Type{values.NotNullDouble})
	partialCost := partial.HintCost(nil, nil)
	if partialCost.Cardinality != properties.LeafScanCardinality {
		t.Fatalf("partial vector cardinality = %v, want conservative unknown-partition estimate %v", partialCost.Cardinality, properties.LeafScanCardinality)
	}
	if partialCost.Cardinality == 14 {
		t.Fatal("partial vector prefix must not advertise signedFanout*k")
	}
}

func TestPhysicalEqualityCost_OverflowAndIllegalPermutationStayFinite(t *testing.T) {
	t.Parallel()
	const count = 64
	comps := make([]*predicates.ComparisonRange, count)
	types := make([]values.Type, count)
	columns := make([]string, count)
	for i := range comps {
		comps[i] = pkOrderingEq(t, float64(0))
		types[i] = values.NotNullDouble
		columns[i] = "P"
	}
	vector := mustChecked(t, func() (*RecordQueryVectorIndexPlan, error) {
		return NewRecordQueryVectorIndexPlan(
			"V", comps, values.LiteralValue([]float64{1}), values.LiteralValue(int64(10)),
			predicates.ComparisonDistanceRankLessThanOrEq, nil, nil, []string{"T"}, exactTestRecordType(),
		)
	}).
		WithPartitionColumns(columns).
		WithPartitionKeyComponentTypes(types)
	vectorCost := vector.HintCost(nil, nil)
	if math.IsNaN(vectorCost.Cardinality) || math.IsInf(vectorCost.Cardinality, 0) ||
		math.IsNaN(vectorCost.CPU) || math.IsInf(vectorCost.CPU, 0) {
		t.Fatalf("overflow vector cost is non-finite: %+v", vectorCost)
	}
	if vectorCost.Cardinality != properties.MaxFiniteHeuristic || vectorCost.CPU != properties.MaxFiniteHeuristic {
		t.Fatalf("overflow vector cost = %+v, want finite saturation", vectorCost)
	}

	index := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("P", nil, []string{"T"}, exactTestRecordType(), false)
	}).
		WithPhysicalGroupingPrefixCount(1)
	aggregate := mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
		return NewRecordQueryAggregateIndexPlan(index, "T", exactTestRecordType(), "MAX")
	}).
		WithGroupColumns([]string{"G1", "G2"}, "V")
	illegalCost := aggregate.HintCost(nil, nil)
	if illegalCost.Cardinality != properties.MaxFiniteHeuristic || illegalCost.CPU != properties.MaxFiniteHeuristic {
		t.Fatalf("positive-permutation cost = %+v, want finite saturation", illegalCost)
	}
}
