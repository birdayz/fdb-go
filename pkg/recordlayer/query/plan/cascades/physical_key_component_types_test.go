package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func candidatePhysicalTypeEquality(value any) *predicates.ComparisonRange {
	comparison := predicates.NewLiteralComparison(predicates.ComparisonEquals, value)
	return predicates.EmptyComparisonRange().Merge(&comparison).Range
}

func candidatePhysicalTypeInequality(value any) *predicates.ComparisonRange {
	comparison := predicates.NewLiteralComparison(predicates.ComparisonLessThan, value)
	return predicates.EmptyComparisonRange().Merge(&comparison).Range
}

func candidatePhysicalTypeStartsWith(value any) *predicates.ComparisonRange {
	comparison := predicates.NewLiteralComparison(predicates.ComparisonStartsWith, value)
	return predicates.EmptyComparisonRange().Merge(&comparison).Range
}

func candidatePhysicalIsNotNull() *predicates.ComparisonRange {
	comparison := predicates.Comparison{Type: predicates.ComparisonIsNotNull}
	return predicates.EmptyComparisonRange().Merge(&comparison).Range
}

func TestAggregateCandidateStopsAtPhysicalGroupingBoundary(t *testing.T) {
	t.Parallel()

	candidate := NewAggregateIndexMatchCandidate(
		"max_idx", []string{"T"}, []string{"A", "B", "C"}, expressions.AggMax, "V", values.UnknownType,
		[]values.Type{values.NullableFloat, values.NullableDouble, values.NullableLong},
		2,
	)
	aliases := candidate.GetSargableAliases()
	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		aliases[0]: candidatePhysicalTypeEquality(float64(0)),
		aliases[1]: candidatePhysicalTypeEquality(float64(1)),
		aliases[2]: candidatePhysicalTypeEquality(int64(2)),
	}
	prefix := candidate.ComputeBoundParameterPrefixMap(bindings)
	if len(prefix) != 2 {
		t.Fatalf("bound prefix size = %d, want physical prefix size 2", len(prefix))
	}
	if _, leaked := prefix[aliases[2]]; leaked {
		t.Fatal("permuted suffix grouping column crossed the physical SARG boundary")
	}

	indexPlan, ok := candidate.ToScanPlan(prefix, false).(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("scan plan type = %T, want *RecordQueryIndexPlan", candidate.ToScanPlan(prefix, false))
	}
	if got := len(indexPlan.GetScanComparisons()); got != 2 {
		t.Fatalf("scan comparison count = %d, want 2", got)
	}
	types := indexPlan.GetKeyComponentTypes()
	if len(types) != 3 ||
		types[0].Code() != values.TypeCodeFloat ||
		types[1].Code() != values.TypeCodeDouble ||
		types[2].Code() != values.TypeCodeLong {
		t.Fatalf("scan physical types = %v, want [FLOAT DOUBLE LONG]", types)
	}
	aggregate := plans.NewRecordQueryAggregateIndexPlan(indexPlan, "T", values.UnknownType, "MAX").
		WithGroupColumns([]string{"A", "B", "C"}, "V")
	if got := aggregate.GetPhysicalGroupingPrefixCount(); got != 2 {
		t.Fatalf("aggregate physical grouping prefix = %d, want 2", got)
	}
}

func TestCandidatePhysicalTypeFallbackUsesCardinalityInt(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "TAGS", FieldType: values.UnknownType},
	})
	known := false
	candidate := NewValueIndexScanMatchCandidateWithFunctions(
		"card_idx", []string{"T"}, []string{"TAGS"}, []string{FunctionKindCardinality},
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()}, rowType, false, nil, &known,
	)
	types := candidate.GetKeyComponentTypes()
	if len(types) != 1 || types[0].Code() != values.TypeCodeInt {
		t.Fatalf("CARDINALITY physical type = %v, want INT", types)
	}
}

func TestValueAndPrimaryCandidatesKeepSuffixAfterSignedZero(t *testing.T) {
	t.Parallel()

	aliases := []values.CorrelationIdentifier{
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
	}
	knownDistinct := false
	valueCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"float_suffix", []string{"T"}, []string{"V", "W"}, nil, aliases,
		values.UnknownType, false, nil, &knownDistinct,
	).WithKeyComponentTypes([]values.Type{values.NullableDouble, values.NullableLong})
	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		aliases[0]: candidatePhysicalTypeEquality(math.Copysign(0, -1)),
		aliases[1]: candidatePhysicalTypeEquality(int64(5)),
	}
	if prefix := valueCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 2 {
		t.Fatalf("value-index signed-zero prefix size = %d, want 2", len(prefix))
	}

	primaryCandidate := NewPrimaryScanMatchCandidate(
		nil, aliases, []string{"T"}, []string{"T"}, []string{"V", "W"}, true, values.UnknownType,
	).WithKeyComponentTypes([]values.Type{values.NullableDouble, values.NullableLong})
	if prefix := primaryCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 2 {
		t.Fatalf("primary signed-zero prefix size = %d, want 2", len(prefix))
	}
}

func TestVisibleConstantNaNEligibility(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		rangeFn func(any) *predicates.ComparisonRange
	}{
		{name: "equality", rangeFn: candidatePhysicalTypeEquality},
		{name: "ordered", rangeFn: candidatePhysicalTypeInequality},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			aliases := []values.CorrelationIdentifier{
				values.UniqueCorrelationIdentifier(),
				values.UniqueCorrelationIdentifier(),
				values.UniqueCorrelationIdentifier(),
			}
			bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				aliases[0]: candidatePhysicalTypeEquality(int64(1)),
				aliases[1]: test.rangeFn(math.NaN()),
				aliases[2]: candidatePhysicalTypeEquality(int64(3)),
			}
			knownDistinct := false
			valueCandidate := NewValueIndexScanMatchCandidateWithFunctions(
				"nan_idx", []string{"T"}, []string{"A", "B", "C"}, nil, aliases,
				values.UnknownType, false, nil, &knownDistinct,
			).WithKeyComponentTypes([]values.Type{values.NullableLong, values.NullableDouble, values.NullableLong})
			valuePrefix := valueCandidate.ComputeBoundParameterPrefixMap(bindings)
			if len(valuePrefix) != 1 {
				t.Fatalf("value NaN prefix size = %d, want preceding equality only", len(valuePrefix))
			}
			if _, sarged := valuePrefix[aliases[1]]; sarged {
				t.Fatal("visible constant NaN must remain residual on an ordinary value scan")
			}

			primaryCandidate := NewPrimaryScanMatchCandidate(
				nil, aliases, []string{"T"}, []string{"T"}, []string{"A", "B", "C"}, true, values.UnknownType,
			).WithKeyComponentTypes([]values.Type{values.NullableLong, values.NullableDouble, values.NullableLong})
			if prefix := primaryCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 1 {
				t.Fatalf("primary NaN prefix size = %d, want preceding equality only", len(prefix))
			}

			aggregate := NewAggregateIndexMatchCandidate(
				"agg_nan", []string{"T"}, []string{"A", "B", "C"}, expressions.AggMax, "V", values.UnknownType,
				[]values.Type{values.NullableDouble, values.NullableLong, values.NullableLong}, 3,
			)
			aggAliases := aggregate.GetSargableAliases()
			if candidateBindingRangesEligible(aggregate, map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				aggAliases[0]: test.rangeFn(math.NaN()),
			}) {
				t.Fatal("aggregate candidate accepted a visible constant NaN comparison")
			}

			vector := NewVectorIndexScanMatchCandidate(
				"vector_nan", []string{"T"}, []string{"PART", "EMBEDDING"}, 1,
				values.DistanceEuclidean, values.UnknownType, false, nil,
			).WithPartitionKeyComponentTypes([]values.Type{values.NullableDouble})
			partitionAlias := vector.GetSargableAliases()[0]
			if candidateBindingRangesEligible(vector, map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				partitionAlias: test.rangeFn(math.NaN()),
			}) {
				t.Fatal("self-limiting partitioned vector candidate accepted a visible constant NaN comparison")
			}
		})
	}
}

func TestCandidatesNeverInferUnknownPhysicalKeyTypeFromComparand(t *testing.T) {
	t.Parallel()

	aliases := []values.CorrelationIdentifier{
		values.UniqueCorrelationIdentifier(),
		values.UniqueCorrelationIdentifier(),
	}
	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		aliases[0]: candidatePhysicalTypeEquality(int64(7)),
		aliases[1]: candidatePhysicalTypeEquality(float64(0)),
	}
	knownDistinct := false
	valueCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"heterogeneous", []string{"FloatRecord", "DoubleRecord"}, []string{"A", "B"}, nil, aliases,
		values.UnknownType, false, nil, &knownDistinct,
	).WithKeyComponentTypes([]values.Type{values.NullableLong, values.UnknownType})
	if prefix := valueCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 1 {
		t.Fatalf("value-index prefix with conflicting second component = %d, want 1", len(prefix))
	}

	primaryCandidate := NewPrimaryScanMatchCandidate(
		nil, aliases, []string{"FloatRecord", "DoubleRecord"}, []string{"FloatRecord", "DoubleRecord"},
		[]string{"A", "B"}, true, values.UnknownType,
	).WithKeyComponentTypes([]values.Type{values.NullableLong, values.UnknownType})
	if prefix := primaryCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 1 {
		t.Fatalf("primary prefix with conflicting second component = %d, want 1", len(prefix))
	}

	aggregate := NewAggregateIndexMatchCandidate(
		"heterogeneous_aggregate", []string{"FloatRecord", "DoubleRecord"}, []string{"A"},
		expressions.AggMax, "V", values.UnknownType,
		[]values.Type{values.UnknownType}, 1,
	)
	if candidateBindingRangesEligible(aggregate, map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		aggregate.GetSargableAliases()[0]: candidatePhysicalTypeEquality(float64(1)),
	}) {
		t.Fatal("aggregate candidate inferred an Unknown physical type from a DOUBLE literal")
	}

	vector := NewVectorIndexScanMatchCandidate(
		"heterogeneous_vector", []string{"FloatRecord", "DoubleRecord"}, []string{"PART", "EMBEDDING"}, 1,
		values.DistanceEuclidean, values.UnknownType, false, nil,
	).WithPartitionKeyComponentTypes([]values.Type{values.UnknownType})
	if candidateBindingRangesEligible(vector, map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		vector.GetSargableAliases()[0]: candidatePhysicalTypeEquality(float64(1)),
	}) {
		t.Fatal("vector candidate inferred an Unknown physical type from a DOUBLE literal")
	}
}

// TestCandidatesRetainPhysicalFloatInequalities pins WHICH leaves keep an
// ordered FLOAT/DOUBLE bound and which decline it, and the split is by
// CAPABILITY, not by type.
//
// A value index and a primary scan bind through the physical range set, which
// represents an ordered float comparison exactly as one or two ranges, so they
// retain the bound and keep their index. An aggregate index reads a
// pre-aggregated stream and a partitioned vector index is self-limiting per
// partition; neither can carry a residual or enumerate a second range, so both
// still decline.
//
// Declining everywhere was the earlier behaviour and it was a strict
// downgrade: it traded every float index range for a full scan without fixing
// any wrong answer.
func TestCandidatesRetainPhysicalFloatInequalities(t *testing.T) {
	t.Parallel()

	alias := values.UniqueCorrelationIdentifier()
	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		alias: candidatePhysicalTypeInequality(float64(0)),
	}
	knownDistinct := false
	valueCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"float_order", []string{"T"}, []string{"V"}, nil, []values.CorrelationIdentifier{alias},
		values.UnknownType, false, nil, &knownDistinct,
	).WithKeyComponentTypes([]values.Type{values.NullableDouble})
	if prefix := valueCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 1 {
		t.Fatalf("value candidate prefix = %v, want the FLOAT inequality retained; "+
			"declining it withdraws the index access path and fixes nothing", prefix)
	}
	primaryCandidate := NewPrimaryScanMatchCandidate(
		nil, []values.CorrelationIdentifier{alias}, []string{"T"}, []string{"T"},
		[]string{"V"}, true, values.UnknownType,
	).WithKeyComponentTypes([]values.Type{values.NullableFloat})
	if prefix := primaryCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 1 {
		t.Fatalf("primary candidate prefix = %v, want the FLOAT inequality retained", prefix)
	}

	aggregate := NewAggregateIndexMatchCandidate(
		"float_order_aggregate", []string{"T"}, []string{"V"}, expressions.AggMax, "A", values.UnknownType,
		[]values.Type{values.NullableDouble}, 1,
	)
	aggAlias := aggregate.GetSargableAliases()[0]
	if candidateBindingRangesEligible(aggregate, map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		aggAlias: candidatePhysicalTypeInequality(float64(0)),
	}) {
		t.Fatal("aggregate candidate accepted a FLOAT inequality")
	}

	integerCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"integer_order", []string{"T"}, []string{"V"}, nil, []values.CorrelationIdentifier{alias},
		values.UnknownType, false, nil, &knownDistinct,
	).WithKeyComponentTypes([]values.Type{values.NullableLong})
	if prefix := integerCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 1 {
		t.Fatalf("integer inequality prefix = %v, want retained bound", prefix)
	}
}

func TestCandidatesRetainPhysicalFloatIsNotNull(t *testing.T) {
	t.Parallel()

	alias := values.UniqueCorrelationIdentifier()
	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		alias: candidatePhysicalIsNotNull(),
	}
	knownDistinct := false
	valueCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"float_not_null", []string{"T"}, []string{"V"}, nil,
		[]values.CorrelationIdentifier{alias}, values.UnknownType, false, nil, &knownDistinct,
	).WithKeyComponentTypes([]values.Type{values.NullableDouble})
	if prefix := valueCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 1 {
		t.Fatalf("value FLOAT IS NOT NULL prefix = %v, want retained bound", prefix)
	}

	primaryCandidate := NewPrimaryScanMatchCandidate(
		nil, []values.CorrelationIdentifier{alias}, []string{"T"}, []string{"T"},
		[]string{"V"}, true, values.UnknownType,
	).WithKeyComponentTypes([]values.Type{values.NullableFloat})
	if prefix := primaryCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 1 {
		t.Fatalf("primary FLOAT IS NOT NULL prefix = %v, want retained bound", prefix)
	}

	aggregate := NewAggregateIndexMatchCandidate(
		"float_not_null_aggregate", []string{"T"}, []string{"V"}, expressions.AggMax, "A", values.UnknownType,
		[]values.Type{values.NullableDouble}, 1,
	)
	aggAlias := aggregate.GetSargableAliases()[0]
	if !candidateBindingRangesEligible(aggregate, map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		aggAlias: candidatePhysicalIsNotNull(),
	}) {
		t.Fatal("aggregate candidate declined exact FLOAT IS NOT NULL")
	}

	vector := NewVectorIndexScanMatchCandidate(
		"float_not_null_vector", []string{"T"}, []string{"PART", "EMBEDDING"}, 1,
		values.DistanceEuclidean, values.UnknownType, false, nil,
	).WithPartitionKeyComponentTypes([]values.Type{values.NullableFloat})
	partitionAlias := vector.GetSargableAliases()[0]
	if !candidateBindingRangesEligible(vector, map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		partitionAlias: candidatePhysicalIsNotNull(),
	}) {
		t.Fatal("vector candidate declined exact FLOAT IS NOT NULL")
	}
}

func TestCandidatesOnlySARGStartsWithOnPhysicalString(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		physicalType values.Type
		comparand    any
	}{
		{name: "LONG", physicalType: values.NullableLong, comparand: int64(1)},
		{name: "DATE", physicalType: values.NullableDate, comparand: "2024-01-02"},
		{name: "TIMESTAMP", physicalType: values.NullableTimestamp, comparand: "2024-01-02 03:04:05"},
		{name: "BYTES", physicalType: values.NullableBytes, comparand: []byte("ab")},
		{name: "DOUBLE", physicalType: values.NullableDouble, comparand: float64(1)},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			alias := values.UniqueCorrelationIdentifier()
			bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				alias: candidatePhysicalTypeStartsWith(test.comparand),
			}
			knownDistinct := false
			valueCandidate := NewValueIndexScanMatchCandidateWithFunctions(
				"unsupported_starts_with", []string{"T"}, []string{"V"}, nil,
				[]values.CorrelationIdentifier{alias}, values.UnknownType, false, nil, &knownDistinct,
			).WithKeyComponentTypes([]values.Type{test.physicalType})
			if prefix := valueCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 0 {
				t.Fatalf("value candidate SARGed %s STARTS_WITH: %v", test.name, prefix)
			}

			primaryCandidate := NewPrimaryScanMatchCandidate(
				nil, []values.CorrelationIdentifier{alias}, []string{"T"}, []string{"T"},
				[]string{"V"}, true, values.UnknownType,
			).WithKeyComponentTypes([]values.Type{test.physicalType})
			if prefix := primaryCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 0 {
				t.Fatalf("primary candidate SARGed %s STARTS_WITH: %v", test.name, prefix)
			}

			aggregate := NewAggregateIndexMatchCandidate(
				"unsupported_starts_with_aggregate", []string{"T"}, []string{"V"},
				expressions.AggMax, "A", values.UnknownType,
				[]values.Type{test.physicalType}, 1,
			)
			aggAlias := aggregate.GetSargableAliases()[0]
			if candidateBindingRangesEligible(aggregate, map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				aggAlias: candidatePhysicalTypeStartsWith(test.comparand),
			}) {
				t.Fatalf("aggregate candidate accepted %s STARTS_WITH", test.name)
			}

			vector := NewVectorIndexScanMatchCandidate(
				"unsupported_starts_with_vector", []string{"T"}, []string{"PART", "EMBEDDING"}, 1,
				values.DistanceEuclidean, values.UnknownType, false, nil,
			).WithPartitionKeyComponentTypes([]values.Type{test.physicalType})
			partitionAlias := vector.GetSargableAliases()[0]
			if candidateBindingRangesEligible(vector, map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				partitionAlias: candidatePhysicalTypeStartsWith(test.comparand),
			}) {
				t.Fatalf("vector candidate accepted %s STARTS_WITH", test.name)
			}
		})
	}

	t.Run("STRING remains SARGable", func(t *testing.T) {
		t.Parallel()
		alias := values.UniqueCorrelationIdentifier()
		bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			alias: candidatePhysicalTypeStartsWith("ab"),
		}
		knownDistinct := false
		valueCandidate := NewValueIndexScanMatchCandidateWithFunctions(
			"string_starts_with", []string{"T"}, []string{"V"}, nil,
			[]values.CorrelationIdentifier{alias}, values.UnknownType, false, nil, &knownDistinct,
		).WithKeyComponentTypes([]values.Type{values.NullableString})
		if prefix := valueCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 1 {
			t.Fatalf("value STRING STARTS_WITH prefix = %v, want retained bound", prefix)
		}

		primaryCandidate := NewPrimaryScanMatchCandidate(
			nil, []values.CorrelationIdentifier{alias}, []string{"T"}, []string{"T"},
			[]string{"V"}, true, values.UnknownType,
		).WithKeyComponentTypes([]values.Type{values.NullableString})
		if prefix := primaryCandidate.ComputeBoundParameterPrefixMap(bindings); len(prefix) != 1 {
			t.Fatalf("primary STRING STARTS_WITH prefix = %v, want retained bound", prefix)
		}
	})
}
