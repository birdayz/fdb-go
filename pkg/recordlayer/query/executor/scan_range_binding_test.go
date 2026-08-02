package executor

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func scanRangeTestComparison(
	t *testing.T,
	typ predicates.ComparisonType,
	operand values.Value,
) *predicates.ComparisonRange {
	t.Helper()
	merged := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type: typ, Operand: operand,
	})
	if !merged.Ok {
		t.Fatalf("merge comparison %v failed", typ)
	}
	return merged.Range
}

func scanRangeTestEq(t *testing.T, value any) *predicates.ComparisonRange {
	t.Helper()
	return scanRangeTestComparison(
		t,
		predicates.ComparisonEquals,
		values.LiteralValue(value),
	)
}

func TestBindScanComparisonsToRangeSet_NonTerminalFloatingZero(t *testing.T) {
	t.Parallel()
	carriers := []struct {
		name         string
		physicalType values.Type
		negativeZero any
		positiveZero any
	}{
		{name: "DOUBLE", physicalType: values.NotNullDouble, negativeZero: math.Copysign(0, -1), positiveZero: float64(0)},
		{name: "FLOAT", physicalType: values.NotNullFloat, negativeZero: math.Float32frombits(1 << 31), positiveZero: float32(0)},
	}
	for _, carrier := range carriers {
		carrier := carrier
		for _, comparisonType := range []predicates.ComparisonType{
			predicates.ComparisonEquals,
			predicates.ComparisonNotDistinctFrom,
		} {
			comparisonType := comparisonType
			for _, input := range []struct {
				name string
				zero any
			}{
				{name: "negative", zero: carrier.negativeZero},
				{name: "positive", zero: carrier.positiveZero},
			} {
				input := input
				name := fmt.Sprintf("%s/%v/%s", carrier.name, comparisonType, input.name)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					comparisons := []*predicates.ComparisonRange{
						scanRangeTestComparison(t, comparisonType, values.LiteralValue(input.zero)),
						scanRangeTestEq(t, int64(5)),
					}
					spec, err := bindScanComparisonsToRangeSet(
						comparisons,
						[]values.Type{carrier.physicalType, values.NotNullLong},
						nil,
						false,
						"idx:T_VW",
					)
					if err != nil {
						t.Fatal(err)
					}
					if spec.empty {
						t.Fatal("zero equality unexpectedly produced an empty range set")
					}
					if got, want := spec.alternativeCounts, []uint32{2, 1}; !equalUint32s(got, want) {
						t.Fatalf("alternative counts = %v, want %v", got, want)
					}

					negative, err := spec.materialize([]uint32{0, 0})
					if err != nil {
						t.Fatal(err)
					}
					positive, err := spec.materialize([]uint32{1, 0})
					if err != nil {
						t.Fatal(err)
					}
					assertExactZeroRange(t, negative, carrier.negativeZero, int64(5))
					assertExactZeroRange(t, positive, carrier.positiveZero, int64(5))
				})
			}
		}
	}
}

func TestBindScanComparisonsToRangeSet_ReverseZeroOrder(t *testing.T) {
	t.Parallel()
	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{
			scanRangeTestEq(t, math.Copysign(0, -1)),
			scanRangeTestEq(t, int64(5)),
		},
		[]values.Type{values.NotNullDouble, values.NotNullLong},
		nil,
		true,
		"idx:T_VW",
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := spec.materialize([]uint32{0, 0})
	if err != nil {
		t.Fatal(err)
	}
	second, err := spec.materialize([]uint32{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	assertExactZeroRange(t, first, float64(0), int64(5))
	assertExactZeroRange(t, second, math.Copysign(0, -1), int64(5))
}

func TestBindScanComparisonsToRangeSet_TwoZerosCartesianWithoutMaterialization(t *testing.T) {
	t.Parallel()
	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{
			scanRangeTestEq(t, float64(0)),
			scanRangeTestEq(t, float64(0)),
			scanRangeTestEq(t, int64(5)),
		},
		[]values.Type{values.NotNullDouble, values.NotNullDouble, values.NotNullLong},
		nil,
		false,
		"idx:T_VVW",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := spec.alternativeCounts, []uint32{2, 2, 1}; !equalUint32s(got, want) {
		t.Fatalf("alternative counts = %v, want %v", got, want)
	}
	for first := uint32(0); first < 2; first++ {
		for second := uint32(0); second < 2; second++ {
			rng, materializeErr := spec.materialize([]uint32{first, second, 0})
			if materializeErr != nil {
				t.Fatal(materializeErr)
			}
			if len(rng.Low) != 3 || len(rng.High) != 3 {
				t.Fatalf("choices [%d,%d]: range = %+v, want exact 3-component range", first, second, rng)
			}
			if math.Signbit(rng.Low[0].(float64)) != (first == 0) ||
				math.Signbit(rng.Low[1].(float64)) != (second == 0) {
				t.Fatalf("choices [%d,%d] materialized signs %v", first, second, rng.Low)
			}
			if rng.Low[2] != int64(5) || rng.LowEndpoint != recordlayer.EndpointTypeRangeInclusive ||
				rng.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
				t.Fatalf("choices [%d,%d] materialized %+v", first, second, rng)
			}
		}
	}
}

func TestBindScanComparisonsToRangeSet_TerminalZeroUsesOneInclusiveRange(t *testing.T) {
	t.Parallel()
	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, float64(0))},
		[]values.Type{values.NotNullDouble},
		nil,
		false,
		"idx:T_V",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := spec.alternativeCounts, []uint32{1}; !equalUint32s(got, want) {
		t.Fatalf("alternative counts = %v, want %v", got, want)
	}
	rng, err := spec.materialize([]uint32{0})
	if err != nil {
		t.Fatal(err)
	}
	if len(rng.Low) != 1 || len(rng.High) != 1 ||
		!math.Signbit(rng.Low[0].(float64)) || math.Signbit(rng.High[0].(float64)) ||
		rng.LowEndpoint != recordlayer.EndpointTypeRangeInclusive ||
		rng.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("terminal zero range = %+v, want [-0,+0] inclusive", rng)
	}
}

func TestBindScanComparisonsToRangeSet_VectorTerminalZeroStaysTwoExactPartitions(t *testing.T) {
	t.Parallel()
	spec, err := bindScanComparisonsToRangeSetWithTerminalWidening(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, float64(0))},
		[]values.Type{values.NotNullDouble},
		nil,
		false,
		"vector:V",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := spec.alternativeCounts, []uint32{2}; !equalUint32s(got, want) {
		t.Fatalf("alternative counts = %v, want %v", got, want)
	}
	for choice, wantNegative := range []bool{true, false} {
		rng, materializeErr := spec.materialize([]uint32{uint32(choice)})
		if materializeErr != nil {
			t.Fatal(materializeErr)
		}
		if len(rng.Low) != 1 || len(rng.High) != 1 ||
			!bytes.Equal(rng.Low.Pack(), rng.High.Pack()) ||
			math.Signbit(rng.Low[0].(float64)) != wantNegative {
			t.Fatalf("choice %d range = %+v, want exact negative=%v partition", choice, rng, wantNegative)
		}
	}
}

func TestBindScanComparisonsToRangeSet_ExactAlgebraExhaustiveSmallUniverse(t *testing.T) {
	t.Parallel()

	negativeZero := math.Copysign(0, -1)
	zeroValues := []float64{-1, negativeZero, 0, 1}
	suffixValues := []int64{1, 5, 9}

	t.Run("one nonterminal zero and exact suffix", func(t *testing.T) {
		t.Parallel()
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{
				scanRangeTestEq(t, float64(0)),
				scanRangeTestEq(t, int64(5)),
			},
			[]values.Type{values.NotNullDouble, values.NotNullLong}, nil, false, "algebra:one",
		)
		if err != nil {
			t.Fatal(err)
		}
		ranges := materializeEveryRange(t, spec)
		for _, first := range zeroValues {
			for _, suffix := range suffixValues {
				candidate := tuple.Tuple{first, suffix}
				got := rangeMembershipCount(ranges, candidate)
				want := 0
				if first == 0 && suffix == 5 {
					want = 1
				}
				if got != want {
					t.Errorf("candidate %v belongs to %d ranges, want %d", candidate, got, want)
				}
			}
		}
	})

	t.Run("two zeros form four disjoint exact branches", func(t *testing.T) {
		t.Parallel()
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{
				scanRangeTestEq(t, float64(0)),
				scanRangeTestEq(t, float64(0)),
				scanRangeTestEq(t, int64(5)),
			},
			[]values.Type{values.NotNullDouble, values.NotNullDouble, values.NotNullLong},
			nil, false, "algebra:two",
		)
		if err != nil {
			t.Fatal(err)
		}
		ranges := materializeEveryRange(t, spec)
		if len(ranges) != 4 {
			t.Fatalf("range count = %d, want 4", len(ranges))
		}
		for _, first := range zeroValues {
			for _, second := range zeroValues {
				for _, suffix := range suffixValues {
					candidate := tuple.Tuple{first, second, suffix}
					got := rangeMembershipCount(ranges, candidate)
					want := 0
					if first == 0 && second == 0 && suffix == 5 {
						want = 1
					}
					if got != want {
						t.Errorf("candidate %v belongs to %d ranges, want %d", candidate, got, want)
					}
				}
			}
		}
	})

	t.Run("zero prefix with trailing inequality", func(t *testing.T) {
		t.Parallel()
		greaterOrEqual := scanRangeTestComparison(
			t, predicates.ComparisonGreaterThanEq, values.LiteralValue(int64(5)),
		)
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{scanRangeTestEq(t, float64(0)), greaterOrEqual},
			[]values.Type{values.NotNullDouble, values.NotNullLong}, nil, false, "algebra:ineq",
		)
		if err != nil {
			t.Fatal(err)
		}
		ranges := materializeEveryRange(t, spec)
		for _, first := range zeroValues {
			for _, suffix := range suffixValues {
				candidate := tuple.Tuple{first, suffix}
				got := rangeMembershipCount(ranges, candidate)
				want := 0
				if first == 0 && suffix >= 5 {
					want = 1
				}
				if got != want {
					t.Errorf("candidate %v belongs to %d ranges, want %d", candidate, got, want)
				}
			}
		}
	})

	t.Run("terminal zero range covers every suffix exactly once", func(t *testing.T) {
		t.Parallel()
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{scanRangeTestEq(t, float64(0))},
			[]values.Type{values.NotNullDouble}, nil, false, "algebra:terminal",
		)
		if err != nil {
			t.Fatal(err)
		}
		ranges := materializeEveryRange(t, spec)
		for _, first := range zeroValues {
			for _, suffix := range suffixValues {
				candidate := tuple.Tuple{first, suffix}
				got := rangeMembershipCount(ranges, candidate)
				want := 0
				if first == 0 {
					want = 1
				}
				if got != want {
					t.Errorf("candidate %v belongs to %d ranges, want %d", candidate, got, want)
				}
			}
		}
	})
}

func TestBindScanComparisonsToRangeSet_FloatingBoundaryValues(t *testing.T) {
	t.Parallel()
	valuesToProbe := []float64{
		-3.5,
		-math.SmallestNonzeroFloat64,
		math.SmallestNonzeroFloat64,
		3.5,
		math.Inf(-1),
		math.Inf(1),
	}
	for _, value := range valuesToProbe {
		value := value
		t.Run(tuple.Tuple{value}.String(), func(t *testing.T) {
			t.Parallel()
			spec, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{
					scanRangeTestEq(t, value),
					scanRangeTestEq(t, int64(5)),
				},
				[]values.Type{values.NotNullDouble, values.NotNullLong}, nil, false, "edge",
			)
			if err != nil {
				t.Fatal(err)
			}
			if spec.alternativeCounts[0] != 1 {
				t.Fatalf("value %v alternatives = %d, want 1", value, spec.alternativeCounts[0])
			}
		})
	}
}

func TestBindScanComparisonsToRangeSet_NullSafeEqualityAndStartsWith(t *testing.T) {
	t.Parallel()

	nullSafe := scanRangeTestComparison(
		t, predicates.ComparisonNotDistinctFrom, values.LiteralValue(nil),
	)
	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{nullSafe},
		[]values.Type{values.NullableDouble}, nil, false, "null-safe",
	)
	if err != nil {
		t.Fatal(err)
	}
	if spec.empty {
		t.Fatal("IS NOT DISTINCT FROM NULL must seek the physical NULL key")
	}
	rng, err := spec.materialize([]uint32{0})
	if err != nil {
		t.Fatal(err)
	}
	if len(rng.Low) != 1 || rng.Low[0] != nil {
		t.Fatalf("null-safe equality range = %+v, want allOf(NULL)", rng)
	}

	startsWith := scanRangeTestComparison(
		t, predicates.ComparisonStartsWith, values.LiteralValue("ab"),
	)
	prefixSpec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, int64(7)), startsWith},
		[]values.Type{values.NotNullLong, values.NullableString}, nil, false, "starts-with",
	)
	if err != nil {
		t.Fatal(err)
	}
	prefixRange, err := prefixSpec.materialize([]uint32{0})
	if err != nil {
		t.Fatal(err)
	}
	if prefixRange.LowEndpoint != recordlayer.EndpointTypePrefixString ||
		prefixRange.HighEndpoint != recordlayer.EndpointTypePrefixString ||
		len(prefixRange.Low) != 2 || prefixRange.Low[0] != int64(7) || prefixRange.Low[1] != "ab" {
		t.Fatalf("STARTS_WITH range = %+v", prefixRange)
	}
}

func TestBindScanComparisonsToRangeSet_StartsWithRequiresPhysicalString(t *testing.T) {
	t.Parallel()

	valid := scanRangeTestComparison(
		t, predicates.ComparisonStartsWith, values.LiteralValue("ab"),
	)
	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{valid},
		[]values.Type{values.NullableString}, nil, false, "starts-with-string",
	)
	if err != nil {
		t.Fatalf("STRING STARTS_WITH: %v", err)
	}
	rng, err := spec.materialize(nil)
	if err != nil {
		t.Fatal(err)
	}
	if rng.LowEndpoint != recordlayer.EndpointTypePrefixString ||
		rng.HighEndpoint != recordlayer.EndpointTypePrefixString ||
		len(rng.Low) != 1 || rng.Low[0] != "ab" {
		t.Fatalf("STRING STARTS_WITH range = %+v, want PREFIX_STRING(ab)", rng)
	}

	for _, physicalType := range []values.Type{
		values.NullableLong,
		values.NullableDate,
		values.NullableTimestamp,
		values.NullableBytes,
		values.NullableDouble,
		values.NullableBoolean,
		values.NullableUuid,
	} {
		physicalType := physicalType
		t.Run(physicalType.Code().String(), func(t *testing.T) {
			t.Parallel()
			operand := any("ab")
			switch physicalType.Code() {
			case values.TypeCodeLong:
				operand = int64(1)
			case values.TypeCodeBytes:
				operand = []byte("ab")
			case values.TypeCodeDouble:
				operand = float64(1)
			case values.TypeCodeBoolean:
				operand = true
			case values.TypeCodeUuid:
				operand = [16]byte{1}
			}
			comparison := scanRangeTestComparison(
				t, predicates.ComparisonStartsWith,
				&values.ConstantValue{Value: operand, Typ: values.UnknownType},
			)
			_, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{comparison},
				[]values.Type{physicalType}, nil, false, "unsupported-starts-with",
			)
			var unsupported *UnsupportedPhysicalStartsWithError
			if !errors.As(err, &unsupported) {
				t.Fatalf("error = %T(%v), want UnsupportedPhysicalStartsWithError", err, err)
			}
			if unsupported.Component != 0 || unsupported.PhysicalType.Code() != physicalType.Code() {
				t.Fatalf("unsupported STARTS_WITH error = %+v", unsupported)
			}
		})
	}

	t.Run("NULL comparand is type-independent empty", func(t *testing.T) {
		t.Parallel()
		nullComparison := scanRangeTestComparison(
			t, predicates.ComparisonStartsWith, values.LiteralValue(nil),
		)
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{nullComparison},
			[]values.Type{values.NullableLong}, nil, false, "null-starts-with",
		)
		if err != nil {
			t.Fatal(err)
		}
		if !spec.empty {
			t.Fatal("STARTS_WITH NULL must be empty independently of physical carrier")
		}
	})

	t.Run("missing operand fails loud", func(t *testing.T) {
		t.Parallel()
		comparison := scanRangeTestComparison(t, predicates.ComparisonStartsWith, nil)
		_, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{comparison},
			[]values.Type{values.NullableString}, nil, false, "missing-starts-with",
		)
		if err == nil {
			t.Fatal("STARTS_WITH without an operand must fail")
		}
	})
}

func TestBindScanComparisonsToRangeSet_UnknownPhysicalTypeNeverInfersNonNullCarrier(t *testing.T) {
	t.Parallel()

	physicalVariants := []struct {
		name     string
		keyTypes []values.Type
	}{
		{name: "missing metadata", keyTypes: nil},
		{name: "explicit Unknown metadata", keyTypes: []values.Type{values.UnknownType}},
	}
	comparisons := []struct {
		name       string
		comparison *predicates.ComparisonRange
		typ        predicates.ComparisonType
	}{
		{
			name:       "known DOUBLE operand declaration",
			comparison: scanRangeTestEq(t, float64(1)),
			typ:        predicates.ComparisonEquals,
		},
		{
			name: "Unknown runtime integer inequality",
			comparison: scanRangeTestComparison(t, predicates.ComparisonGreaterThan,
				&values.ConstantValue{Value: int64(1), Typ: values.UnknownType}),
			typ: predicates.ComparisonGreaterThan,
		},
		{
			name:       "known STRING STARTS_WITH declaration",
			comparison: scanRangeTestComparison(t, predicates.ComparisonStartsWith, values.LiteralValue("a")),
			typ:        predicates.ComparisonStartsWith,
		},
	}

	for _, physical := range physicalVariants {
		physical := physical
		for _, test := range comparisons {
			test := test
			t.Run(physical.name+"/"+test.name, func(t *testing.T) {
				t.Parallel()
				_, err := bindScanComparisonsToRangeSet(
					[]*predicates.ComparisonRange{test.comparison},
					physical.keyTypes, nil, false, "unknown-physical-type",
				)
				var unknown *UnknownPhysicalKeyTypeError
				if !errors.As(err, &unknown) {
					t.Fatalf("error = %T(%v), want UnknownPhysicalKeyTypeError", err, err)
				}
				if unknown.Component != 0 || unknown.Comparison != test.typ {
					t.Fatalf("unknown physical error = %+v", unknown)
				}
			})
		}
	}
}

func TestBindScanComparisonsToRangeSet_NonAuthoritativePhysicalTypeAllowsTypeIndependentNullSemantics(t *testing.T) {
	t.Parallel()

	physicalVariants := []struct {
		name     string
		keyTypes []values.Type
	}{
		{name: "missing metadata", keyTypes: nil},
		{name: "explicit Unknown metadata", keyTypes: []values.Type{values.UnknownType}},
		{name: "NULL metadata", keyTypes: []values.Type{values.NullType}},
		{name: "ANY metadata", keyTypes: []values.Type{values.AnyType}},
		{name: "NONE metadata", keyTypes: []values.Type{values.NoneType}},
		{name: "RECORD metadata", keyTypes: []values.Type{values.NewRecordType("R", false, nil)}},
		{name: "ARRAY metadata", keyTypes: []values.Type{values.NewArrayType(false, values.NotNullLong)}},
		{name: "RELATION metadata", keyTypes: []values.Type{values.NewRelationType(values.NotNullLong)}},
		{name: "ENUM metadata", keyTypes: []values.Type{values.NewEnumType("E", false, nil)}},
	}
	for _, physical := range physicalVariants {
		physical := physical
		t.Run(physical.name, func(t *testing.T) {
			t.Parallel()

			isNull, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{
					scanRangeTestComparison(t, predicates.ComparisonIsNull, nil),
				},
				physical.keyTypes, nil, false, "unknown-is-null",
			)
			if err != nil {
				t.Fatal(err)
			}
			isNullRange, err := isNull.materialize([]uint32{0})
			if err != nil {
				t.Fatal(err)
			}
			if len(isNullRange.Low) != 1 || isNullRange.Low[0] != nil {
				t.Fatalf("IS NULL range = %+v, want exact NULL key", isNullRange)
			}

			equalsNull, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{
					scanRangeTestComparison(t, predicates.ComparisonEquals, values.LiteralValue(nil)),
				},
				physical.keyTypes, nil, false, "unknown-equals-null",
			)
			if err != nil || !equalsNull.empty {
				t.Fatalf("= NULL = empty %v, error %v; want true, nil", equalsNull.empty, err)
			}

			nullSafe, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{
					scanRangeTestComparison(t, predicates.ComparisonNotDistinctFrom, values.LiteralValue(nil)),
				},
				physical.keyTypes, nil, false, "unknown-null-safe",
			)
			if err != nil {
				t.Fatal(err)
			}
			nullSafeRange, err := nullSafe.materialize([]uint32{0})
			if err != nil {
				t.Fatal(err)
			}
			if len(nullSafeRange.Low) != 1 || nullSafeRange.Low[0] != nil {
				t.Fatalf("IS NOT DISTINCT FROM NULL range = %+v, want exact NULL key", nullSafeRange)
			}

			for _, comparisonType := range []predicates.ComparisonType{
				predicates.ComparisonGreaterThan,
				predicates.ComparisonStartsWith,
			} {
				nullRange, bindErr := bindScanComparisonsToRangeSet(
					[]*predicates.ComparisonRange{
						scanRangeTestComparison(t, comparisonType, values.LiteralValue(nil)),
					},
					physical.keyTypes, nil, false, "unknown-null-binary",
				)
				if bindErr != nil || !nullRange.empty {
					t.Fatalf("%v NULL = empty %v, error %v; want true, nil",
						comparisonType, nullRange.empty, bindErr)
				}
			}

			isNotNull, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{
					scanRangeTestComparison(t, predicates.ComparisonIsNotNull, nil),
				},
				physical.keyTypes, nil, false, "unknown-is-not-null",
			)
			if err != nil {
				t.Fatal(err)
			}
			isNotNullRange, err := isNotNull.materialize(nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(isNotNullRange.Low) != 1 || isNotNullRange.Low[0] != nil ||
				isNotNullRange.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
				t.Fatalf("IS NOT NULL range = %+v, want (NULL, tree-end]", isNotNullRange)
			}
		})
	}
}

func TestBindScanComparisonsToRangeSet_NonScalarPhysicalTypeFailsClosedBeforePacking(t *testing.T) {
	t.Parallel()

	physicalTypes := []values.Type{
		values.NullType,
		values.AnyType,
		values.NoneType,
		values.NewRecordType("R", false, nil),
		values.NewArrayType(false, values.NotNullLong),
		values.NewRelationType(values.NotNullLong),
		values.NewEnumType("E", false, nil),
	}
	for _, physicalType := range physicalTypes {
		physicalType := physicalType
		t.Run(physicalType.Code().String(), func(t *testing.T) {
			t.Parallel()
			for _, comparisonType := range []predicates.ComparisonType{
				predicates.ComparisonEquals,
				predicates.ComparisonGreaterThan,
			} {
				comparisonType := comparisonType
				t.Run(fmt.Sprint(comparisonType), func(t *testing.T) {
					t.Parallel()
					// []any is deliberately not an FDB tuple carrier. Before the
					// fail-closed type gate it reached range-set fingerprinting for
					// structured/placeholder metadata and Tuple.Pack panicked.
					operand := &values.ConstantValue{
						Value: []any{int64(1)},
						Typ:   values.UnknownType,
					}
					_, err := bindScanComparisonsToRangeSet(
						[]*predicates.ComparisonRange{
							scanRangeTestComparison(t, comparisonType, operand),
						},
						[]values.Type{physicalType}, nil, false, "unsupported-physical-type",
					)
					var unsupported *UnsupportedPhysicalKeyTypeError
					if !errors.As(err, &unsupported) {
						t.Fatalf("error = %T(%v), want UnsupportedPhysicalKeyTypeError", err, err)
					}
					if unsupported.Component != 0 || unsupported.Comparison != comparisonType ||
						unsupported.PhysicalType.Code() != physicalType.Code() {
						t.Fatalf("unsupported physical error = %+v", unsupported)
					}
				})
			}
		})
	}
}

func TestExactVectorPartitionPrefix(t *testing.T) {
	t.Parallel()

	all, err := exactVectorPartitionPrefix(recordlayer.TupleRangeAll)
	if err != nil || all != nil {
		t.Fatalf("all prefix = %v, %v; want nil, nil", all, err)
	}
	exactInput := tuple.Tuple{math.Copysign(0, -1), int64(7)}
	exact, err := exactVectorPartitionPrefix(recordlayer.TupleRangeAllOf(exactInput))
	if err != nil || !bytes.Equal(exact.Pack(), exactInput.Pack()) {
		t.Fatalf("exact prefix = %v, %v; want %v", exact, err, exactInput)
	}
	exact[1] = int64(99)
	if exactInput[1] != int64(7) {
		t.Fatal("exactVectorPartitionPrefix aliased the materialized range tuple")
	}

	_, err = exactVectorPartitionPrefix(recordlayer.TupleRangeBetween(
		tuple.Tuple{int64(1)}, tuple.Tuple{int64(2)},
	))
	if err == nil {
		t.Fatal("vector partition accepted a non-exact range")
	}
}

func TestBindScanComparisonsToRangeSet_PhysicalTypeIsAuthoritative(t *testing.T) {
	t.Parallel()

	t.Run("FLOAT narrows unknown integer zero to float32 alternatives", func(t *testing.T) {
		t.Parallel()
		unknownZero := &values.ConstantValue{Value: int64(0), Typ: values.UnknownType}
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{
				scanRangeTestComparison(t, predicates.ComparisonEquals, unknownZero),
				scanRangeTestEq(t, int64(5)),
			},
			[]values.Type{values.NotNullFloat, values.NotNullLong},
			nil,
			false,
			"idx:T_FW",
		)
		if err != nil {
			t.Fatal(err)
		}
		if spec.alternativeCounts[0] != 2 {
			t.Fatalf("FLOAT zero alternatives = %d, want 2", spec.alternativeCounts[0])
		}
		rng, err := spec.materialize([]uint32{0, 0})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := rng.Low[0].(float32); !ok {
			t.Fatalf("FLOAT key packed as %T, want float32", rng.Low[0])
		}
	})

	t.Run("INT overrides unknown floating runtime zero", func(t *testing.T) {
		t.Parallel()
		unknownZero := &values.ConstantValue{Value: float64(0), Typ: values.UnknownType}
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{
				scanRangeTestComparison(t, predicates.ComparisonEquals, unknownZero),
				scanRangeTestEq(t, int64(5)),
			},
			[]values.Type{values.NotNullInt, values.NotNullLong},
			nil,
			false,
			"idx:T_IW",
		)
		if err != nil {
			t.Fatal(err)
		}
		if spec.alternativeCounts[0] != 1 {
			t.Fatalf("INT zero alternatives = %d, want 1", spec.alternativeCounts[0])
		}
		rng, err := spec.materialize([]uint32{0, 0})
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := rng.Low[0].(int64); !ok || value != 0 {
			t.Fatalf("INT key packed as %T(%v), want int64(0)", rng.Low[0], rng.Low[0])
		}
	})
}

func TestBindScanComparisonsToRangeSet_IncompatiblePhysicalComparandIsLoud(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		physicalType values.Type
		runtimeValue any
	}{
		{name: "FLOAT string", physicalType: values.NotNullFloat, runtimeValue: "1.5"},
		{name: "DOUBLE string", physicalType: values.NotNullDouble, runtimeValue: "1.5"},
		{name: "INT string", physicalType: values.NotNullInt, runtimeValue: "1"},
		{name: "LONG string", physicalType: values.NotNullLong, runtimeValue: "1"},
		{name: "BOOLEAN integer", physicalType: values.NotNullBoolean, runtimeValue: int64(1)},
		{name: "STRING bytes", physicalType: values.NotNullString, runtimeValue: []byte("a")},
		{
			name:         "STRING time is logically comparable but not wire-compatible",
			physicalType: values.NotNullString,
			runtimeValue: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		{name: "BYTES string", physicalType: values.NotNullBytes, runtimeValue: "a"},
		{name: "UUID unpromoted string", physicalType: values.NotNullUuid, runtimeValue: "550e8400-e29b-41d4-a716-446655440000"},
		{name: "VERSION string", physicalType: values.NotNullVersion, runtimeValue: "version"},
		{name: "VERSION incomplete placeholder", physicalType: values.NotNullVersion, runtimeValue: tuple.IncompleteVersionstamp(7)},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, comparisonType := range []predicates.ComparisonType{
				predicates.ComparisonEquals,
				predicates.ComparisonGreaterThan,
			} {
				operand := &values.ConstantValue{Value: test.runtimeValue, Typ: values.UnknownType}
				_, err := bindScanComparisonsToRangeSet(
					[]*predicates.ComparisonRange{
						scanRangeTestComparison(t, comparisonType, operand),
					},
					[]values.Type{test.physicalType}, nil, false, "incompatible-physical-comparand",
				)
				var incompatible *IncompatiblePhysicalComparandError
				if !errors.As(err, &incompatible) {
					t.Fatalf("%v error = %T(%v), want IncompatiblePhysicalComparandError",
						comparisonType, err, err)
				}
				if incompatible.Component != 0 || incompatible.Comparison != comparisonType ||
					incompatible.PhysicalType.Code() != test.physicalType.Code() {
					t.Fatalf("%v incompatible error = %+v", comparisonType, incompatible)
				}
			}
		})
	}

	t.Run("STARTS_WITH validates its carrier", func(t *testing.T) {
		t.Parallel()
		_, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{
				scanRangeTestComparison(
					t, predicates.ComparisonStartsWith,
					&values.ConstantValue{Value: int64(1), Typ: values.UnknownType},
				),
			},
			[]values.Type{values.NotNullString}, nil, false, "incompatible-starts-with",
		)
		var incompatible *IncompatiblePhysicalComparandError
		if !errors.As(err, &incompatible) || incompatible.Comparison != predicates.ComparisonStartsWith {
			t.Fatalf("STARTS_WITH error = %T(%v), want IncompatiblePhysicalComparandError", err, err)
		}
	})
}

func TestBindScanComparisonsToRangeSet_CompatiblePhysicalCarriersRemainAccepted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		physicalType values.Type
		runtimeValue any
	}{
		{name: "FLOAT numeric coercion", physicalType: values.NotNullFloat, runtimeValue: int64(1)},
		{name: "DOUBLE numeric coercion", physicalType: values.NotNullDouble, runtimeValue: int64(1)},
		{name: "INT integer", physicalType: values.NotNullInt, runtimeValue: int32(1)},
		{name: "LONG integer", physicalType: values.NotNullLong, runtimeValue: int64(1)},
		{name: "BOOLEAN", physicalType: values.NotNullBoolean, runtimeValue: true},
		{name: "STRING", physicalType: values.NotNullString, runtimeValue: "a"},
		{name: "BYTES", physicalType: values.NotNullBytes, runtimeValue: []byte("a")},
		{name: "UUID packing coercion", physicalType: values.NotNullUuid, runtimeValue: [16]byte{1}},
		{name: "VERSION", physicalType: values.NotNullVersion, runtimeValue: tuple.Versionstamp{}},
		{name: "DATE string carrier", physicalType: values.NotNullDate, runtimeValue: "2024-01-02"},
		{name: "TIMESTAMP string carrier", physicalType: values.NotNullTimestamp, runtimeValue: "2024-01-02 03:04:05"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			operand := &values.ConstantValue{Value: test.runtimeValue, Typ: values.UnknownType}
			spec, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{
					scanRangeTestComparison(t, predicates.ComparisonEquals, operand),
				},
				[]values.Type{test.physicalType}, nil, false, "compatible-physical-comparand",
			)
			if err != nil {
				t.Fatal(err)
			}
			if spec.empty {
				t.Fatal("compatible carrier unexpectedly produced an empty range")
			}
			if _, err = spec.materialize(make([]uint32, len(spec.alternativeCounts))); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBindScanComparisonsToRangeSet_InexactFloatProjectionIsUnsatisfiable(t *testing.T) {
	t.Parallel()

	for _, comparisonType := range []predicates.ComparisonType{
		predicates.ComparisonEquals,
		predicates.ComparisonNotDistinctFrom,
	} {
		comparisonType := comparisonType
		t.Run(fmt.Sprint(comparisonType), func(t *testing.T) {
			t.Parallel()
			for _, runtimeValue := range []any{
				int64(16_777_217),
				float64(16_777_217),
				math.MaxFloat64,
				-math.MaxFloat64,
			} {
				runtimeValue := runtimeValue
				t.Run(fmt.Sprintf("%T_%v", runtimeValue, runtimeValue), func(t *testing.T) {
					t.Parallel()
					operand := &values.ConstantValue{Value: runtimeValue, Typ: values.UnknownType}
					spec, err := bindScanComparisonsToRangeSet(
						[]*predicates.ComparisonRange{
							scanRangeTestComparison(t, comparisonType, operand),
						},
						[]values.Type{values.NotNullFloat},
						nil,
						false,
						"float-inexact-equality",
					)
					if err != nil {
						t.Fatal(err)
					}
					if !spec.empty || spec.materialize != nil {
						t.Fatalf("inexact FLOAT projection %v produced a physical probe", runtimeValue)
					}
				})
			}
		})
	}
}

func TestBindScanComparisonsToRangeSet_ExactFloatProjectionStillSeeks(t *testing.T) {
	t.Parallel()

	for _, runtimeValue := range []any{
		int64(16_777_216),
		float64(16_777_216),
		math.Inf(-1),
		math.Inf(1),
	} {
		runtimeValue := runtimeValue
		t.Run(fmt.Sprintf("%T_%v", runtimeValue, runtimeValue), func(t *testing.T) {
			t.Parallel()
			operand := &values.ConstantValue{Value: runtimeValue, Typ: values.UnknownType}
			spec, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{
					scanRangeTestComparison(t, predicates.ComparisonEquals, operand),
				},
				[]values.Type{values.NotNullFloat},
				nil,
				false,
				"float-exact-equality",
			)
			if err != nil {
				t.Fatal(err)
			}
			if spec.empty {
				t.Fatalf("exact FLOAT projection %v was marked empty", runtimeValue)
			}
			rng, materializeErr := spec.materialize(make([]uint32, len(spec.alternativeCounts)))
			if materializeErr != nil {
				t.Fatal(materializeErr)
			}
			got, ok := rng.Low[0].(float32)
			if !ok || float64(got) != float64(float32(mustNumericFloat64(t, runtimeValue))) {
				t.Fatalf("wire bound = %T(%v), want exact float32 projection of %v", rng.Low[0], rng.Low[0], runtimeValue)
			}
		})
	}
}

// TestBindScanComparisonsToRangeSet_FloatOrderingBindsARangeSet pins that an
// ordered FLOAT/DOUBLE comparison BINDS, rather than refusing and sending the
// query to a full scan. Exactness of what it binds is proved separately in
// float_ordered_range_exactness_test.go against the engine's own predicate
// evaluator; this test guards the ACCESS PATH, which is the property a refusal
// silently destroys.
//
// A NaN threshold still refuses: no single raw payload is an exact logical NaN
// bound, and that case is unchanged.
func TestBindScanComparisonsToRangeSet_FloatOrderingBindsARangeSet(t *testing.T) {
	t.Parallel()

	comparisonTypes := []predicates.ComparisonType{
		predicates.ComparisonLessThan,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonGreaterThanEq,
	}
	for _, physicalType := range []values.Type{values.NotNullFloat, values.NotNullDouble} {
		for _, comparisonType := range comparisonTypes {
			for _, threshold := range []float64{-1, math.Copysign(0, -1), 0, 1, math.Inf(1)} {
				operand := &values.ConstantValue{Value: threshold, Typ: values.UnknownType}
				spec, err := bindScanComparisonsToRangeSet(
					[]*predicates.ComparisonRange{
						scanRangeTestComparison(t, comparisonType, operand),
					},
					[]values.Type{physicalType}, nil, false, "float-ordering-range-set",
				)
				if err != nil {
					t.Fatalf("physical %v, %v %v did not bind: %v — an ordered float predicate must keep its index access path",
						physicalType, comparisonType, threshold, err)
				}
				if spec.empty {
					t.Fatalf("physical %v, %v %v bound an EMPTY range set", physicalType, comparisonType, threshold)
				}
			}
			// A NaN threshold has no exact raw-payload endpoint and still refuses.
			nanOperand := &values.ConstantValue{Value: math.NaN(), Typ: values.UnknownType}
			_, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{
					scanRangeTestComparison(t, comparisonType, nanOperand),
				},
				[]values.Type{physicalType}, nil, false, "float-ordering-nan",
			)
			var unsupported *UnsupportedPhysicalFloatEquivalenceError
			if !errors.As(err, &unsupported) {
				t.Fatalf("physical %v, %v NaN error = %T(%v), want UnsupportedPhysicalFloatEquivalenceError",
					physicalType, comparisonType, err, err)
			}
		}
	}

	// A NULL binary comparand is empty without consulting the non-null float
	// order, and IS NOT NULL is the type-independent exclusive NULL boundary.
	for _, physicalType := range []values.Type{values.NotNullFloat, values.NotNullDouble} {
		nullSpec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{
				scanRangeTestComparison(t, predicates.ComparisonGreaterThan, values.LiteralValue(nil)),
			},
			[]values.Type{physicalType}, nil, false, "null-float-ordering",
		)
		if err != nil || !nullSpec.empty {
			t.Fatalf("physical %v > NULL = empty %v, error %v", physicalType, nullSpec.empty, err)
		}
		isNotNull, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{
				scanRangeTestComparison(t, predicates.ComparisonIsNotNull, nil),
			},
			[]values.Type{physicalType}, nil, false, "float-is-not-null",
		)
		if err != nil {
			t.Fatal(err)
		}
		rng, err := isNotNull.materialize(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rng.Low) != 1 || rng.Low[0] != nil ||
			rng.LowEndpoint != recordlayer.EndpointTypeRangeExclusive {
			t.Fatalf("physical %v IS NOT NULL range = %+v", physicalType, rng)
		}
	}
}

func TestRawNegativeNaNTupleOrderDivergesFromLogicalOrder(t *testing.T) {
	t.Parallel()

	negativeNaN32 := math.Float32frombits(0xffc00001)
	if compareTupleRangeItems(negativeNaN32, float32(math.Inf(-1))) >= 0 {
		t.Fatal("negative float32 NaN does not sort below -Inf in tuple order")
	}
	if values.CompareFloat64(float64(negativeNaN32), math.Inf(-1)) <= 0 {
		t.Fatal("logical comparison did not canonicalize negative float32 NaN above -Inf")
	}

	negativeNaN64 := math.Float64frombits(0xfff8000000000001)
	if compareTupleRangeItems(negativeNaN64, math.Inf(-1)) >= 0 {
		t.Fatal("negative float64 NaN does not sort below -Inf in tuple order")
	}
	if values.CompareFloat64(negativeNaN64, math.Inf(-1)) <= 0 {
		t.Fatal("logical comparison did not canonicalize negative float64 NaN above -Inf")
	}
}

func TestProjectFloatComparisonToIntegerDomain_ExhaustiveSmallUniverse(t *testing.T) {
	t.Parallel()

	const domainLow, domainHigh = int64(-32), int64(32)
	thresholds := make([]float64, 0, 330)
	for quarter := -160; quarter <= 160; quarter++ {
		thresholds = append(thresholds, float64(quarter)/4)
	}
	thresholds = append(thresholds,
		math.Copysign(0, -1),
		math.Inf(-1),
		math.Inf(1),
		math.NaN(),
	)
	comparisonTypes := []predicates.ComparisonType{
		predicates.ComparisonEquals,
		predicates.ComparisonNotDistinctFrom,
		predicates.ComparisonLessThan,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonGreaterThanEq,
	}

	for _, comparisonType := range comparisonTypes {
		for _, threshold := range thresholds {
			projection := projectFloatComparisonToIntegerDomain(
				threshold, domainLow, domainHigh, comparisonType,
			)
			if !projection.empty &&
				(projection.low < domainLow || projection.high > domainHigh || projection.low > projection.high) {
				t.Fatalf("%v %v projection = %+v outside [%d,%d]",
					comparisonType, threshold, projection, domainLow, domainHigh)
			}
			for candidate := domainLow; candidate <= domainHigh; candidate++ {
				got := !projection.empty && candidate >= projection.low && candidate <= projection.high
				want := integerFloatComparisonHolds(candidate, threshold, comparisonType)
				if got != want {
					t.Fatalf("candidate %d %v %v: projected=%v predicate=%v interval=%+v",
						candidate, comparisonType, threshold, got, want, projection)
				}
			}
		}
	}
}

func TestProjectFloatComparisonToIntegerDomain_LongPrecisionCliffs(t *testing.T) {
	t.Parallel()

	const twoTo53 = int64(1) << 53
	projection := projectFloatComparisonToIntegerDomain(
		float64(twoTo53), math.MinInt64, math.MaxInt64, predicates.ComparisonEquals,
	)
	if projection.empty || projection.low != twoTo53 || projection.high != twoTo53+1 {
		t.Fatalf("2^53 equality inverse = %+v, want [%d,%d]",
			projection, twoTo53, twoTo53+1)
	}

	// float64(MaxInt64) rounds to 2^63, yet the top of the signed integer
	// domain still contains integers that promote to that value. A naive
	// nominal-range check would incorrectly call this empty.
	top := projectFloatComparisonToIntegerDomain(
		float64(math.MaxInt64), math.MinInt64, math.MaxInt64, predicates.ComparisonEquals,
	)
	if top.empty || top.high != math.MaxInt64 || top.low == top.high {
		t.Fatalf("float64(MaxInt64) equality inverse = %+v, want plural class ending at MaxInt64", top)
	}

	bottom := projectFloatComparisonToIntegerDomain(
		float64(math.MinInt64), math.MinInt64, math.MaxInt64, predicates.ComparisonEquals,
	)
	if bottom.empty || bottom.low != math.MinInt64 || bottom.low == bottom.high {
		t.Fatalf("float64(MinInt64) equality inverse = %+v, want plural class starting at MinInt64", bottom)
	}
}

func TestPhysicalIntegerDomain_UsesCompleteTupleCarrier(t *testing.T) {
	t.Parallel()

	// Physical TypeCodeInt is also assigned to protobuf uint32/fixed32,
	// including values such as 4_000_000_000 that are packed as int64.
	// Physical TypeCodeLong is assigned to uint64/fixed64; extraction casts
	// their raw bits to int64, so MaxUint64 is represented by the -1 key. The
	// erased TypeCode therefore authorizes the complete signed tuple carrier,
	// not merely the nominal SQL/protobuf signed-width range.
	for _, physicalType := range []values.Type{values.NotNullInt, values.NotNullLong} {
		low, high, ok := physicalIntegerDomain(physicalType)
		if !ok || low != math.MinInt64 || high != math.MaxInt64 {
			t.Fatalf("physical %v integer domain = [%d,%d], %v; want full int64",
				physicalType, low, high, ok)
		}
	}
}

func TestBindScanComparisonsToRangeSet_IntegerInverseEqualityCorrectOrLoud(t *testing.T) {
	t.Parallel()

	const twoTo53 = int64(1) << 53
	tests := []struct {
		name         string
		physicalType values.Type
		runtimeValue any
		wantEmpty    bool
		wantKey      *int64
		wantPlural   *integerDomainProjection
	}{
		{name: "ordinary integer parameter remains exact", physicalType: values.NotNullLong, runtimeValue: twoTo53 + 1, wantKey: int64Pointer(twoTo53 + 1)},
		{name: "exact float singleton", physicalType: values.NotNullLong, runtimeValue: float64(42), wantKey: int64Pointer(42)},
		{name: "negative zero singleton", physicalType: values.NotNullLong, runtimeValue: math.Copysign(0, -1), wantKey: int64Pointer(0)},
		{name: "fractional float has empty inverse", physicalType: values.NotNullLong, runtimeValue: 1.5, wantEmpty: true},
		{name: "positive infinity has empty inverse", physicalType: values.NotNullLong, runtimeValue: math.Inf(1), wantEmpty: true},
		{name: "negative infinity has empty inverse", physicalType: values.NotNullLong, runtimeValue: math.Inf(-1), wantEmpty: true},
		{name: "NaN has empty equality inverse", physicalType: values.NotNullLong, runtimeValue: math.NaN(), wantEmpty: true},
		{name: "INT signed maximum singleton", physicalType: values.NotNullInt, runtimeValue: float64(math.MaxInt32), wantKey: int64Pointer(math.MaxInt32)},
		{name: "INT uint32 carrier above MaxInt32 singleton", physicalType: values.NotNullInt, runtimeValue: float64(4_000_000_000), wantKey: int64Pointer(4_000_000_000)},
		{name: "INT carrier precision cliff is plural", physicalType: values.NotNullInt, runtimeValue: float64(twoTo53), wantPlural: &integerDomainProjection{low: twoTo53, high: twoTo53 + 1}},
		{name: "LONG precision cliff is plural", physicalType: values.NotNullLong, runtimeValue: float64(twoTo53), wantPlural: &integerDomainProjection{low: twoTo53, high: twoTo53 + 1}},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, comparisonType := range []predicates.ComparisonType{
				predicates.ComparisonEquals,
				predicates.ComparisonNotDistinctFrom,
			} {
				operand := &values.ConstantValue{Value: test.runtimeValue, Typ: values.UnknownType}
				spec, err := bindScanComparisonsToRangeSet(
					[]*predicates.ComparisonRange{
						scanRangeTestComparison(t, comparisonType, operand),
					},
					[]values.Type{test.physicalType}, nil, false, "integer-inverse-equality",
				)
				if test.wantPlural != nil {
					var unsupported *UnsupportedPhysicalNumericProjectionError
					if !errors.As(err, &unsupported) {
						t.Fatalf("%v error = %T(%v), want UnsupportedPhysicalNumericProjectionError",
							comparisonType, err, err)
					}
					if unsupported.Component != 0 || unsupported.Comparison != comparisonType ||
						unsupported.EquivalenceClassLow != test.wantPlural.low ||
						unsupported.EquivalenceClassHigh != test.wantPlural.high {
						t.Fatalf("%v unsupported projection = %+v, want class %+v",
							comparisonType, unsupported, *test.wantPlural)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%v: %v", comparisonType, err)
				}
				if test.wantEmpty {
					if !spec.empty || spec.materialize != nil {
						t.Fatalf("%v produced non-empty spec for %T(%v)",
							comparisonType, test.runtimeValue, test.runtimeValue)
					}
					continue
				}
				rng, materializeErr := spec.materialize(make([]uint32, len(spec.alternativeCounts)))
				if materializeErr != nil {
					t.Fatal(materializeErr)
				}
				if len(rng.Low) != 1 || len(rng.High) != 1 ||
					rng.Low[0] != *test.wantKey || rng.High[0] != *test.wantKey {
					t.Fatalf("%v range = %+v, want exact integer key %d",
						comparisonType, rng, *test.wantKey)
				}
			}
		})
	}
}

func TestBindScanComparisonsToRangeSet_IntegerInverseInequalityExactAlgebra(t *testing.T) {
	t.Parallel()

	type physicalDomain struct {
		name       string
		typ        values.Type
		candidates []int64
	}
	domains := []physicalDomain{
		{
			name: "INT",
			typ:  values.NotNullInt,
			candidates: []int64{
				math.MinInt64, math.MinInt32, math.MinInt32 + 1, -2, -1, 0, 1, 2,
				math.MaxInt32 - 1, math.MaxInt32, math.MaxInt32 + 1,
				3_999_999_999, 4_000_000_000, 4_000_000_001, math.MaxInt64,
			},
		},
		{
			name: "LONG",
			typ:  values.NotNullLong,
			candidates: []int64{
				math.MinInt64, math.MinInt64 + 1,
				-(int64(1) << 53) - 1, -(int64(1) << 53), -2, -1, 0, 1, 2,
				(int64(1) << 53), (int64(1) << 53) + 1, (int64(1) << 53) + 2,
				math.MaxInt64 - 1, math.MaxInt64,
			},
		},
	}
	thresholds := []float64{
		math.Inf(-1),
		-math.MaxFloat64,
		float64(math.MinInt64),
		-1.5,
		math.Copysign(0, -1),
		0,
		0.5,
		1.5,
		float64(4_000_000_000),
		float64(4_000_000_000) + 0.5,
		float64(int64(1) << 53),
		float64(int64(1)<<53) + 2,
		float64(math.MaxInt64),
		math.MaxFloat64,
		math.Inf(1),
		math.NaN(),
	}
	comparisonTypes := []predicates.ComparisonType{
		predicates.ComparisonLessThan,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonGreaterThanEq,
	}

	for _, domain := range domains {
		domain := domain
		t.Run(domain.name, func(t *testing.T) {
			t.Parallel()
			for _, comparisonType := range comparisonTypes {
				for _, threshold := range thresholds {
					operand := &values.ConstantValue{Value: threshold, Typ: values.UnknownType}
					spec, err := bindScanComparisonsToRangeSet(
						[]*predicates.ComparisonRange{
							scanRangeTestComparison(t, comparisonType, operand),
						},
						[]values.Type{domain.typ}, nil, false, "integer-inverse-inequality",
					)
					if err != nil {
						t.Fatalf("%s %v %v: %v", domain.name, comparisonType, threshold, err)
					}
					ranges := materializeEveryRange(t, spec)
					for _, candidate := range domain.candidates {
						got := rangeMembershipCount(ranges, tuple.Tuple{candidate}) == 1
						want := integerFloatComparisonHolds(candidate, threshold, comparisonType)
						if got != want {
							t.Fatalf("%s candidate %d %v %v: range=%v predicate=%v",
								domain.name, candidate, comparisonType, threshold, got, want)
						}
					}
				}
			}
		})
	}
}

func TestBindScanComparisonsToRangeSet_IntegerProjectedBoundsIntersectOrderIndependently(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T, comparisons ...*predicates.Comparison) *predicates.ComparisonRange {
		t.Helper()
		rangeValue := predicates.EmptyComparisonRange()
		for _, comparison := range comparisons {
			merged := rangeValue.Merge(comparison)
			if !merged.Ok {
				t.Fatalf("merge %v failed", comparison.Type)
			}
			rangeValue = merged.Range
		}
		return rangeValue
	}
	comparison := func(comparisonType predicates.ComparisonType, value any) *predicates.Comparison {
		return &predicates.Comparison{
			Type: comparisonType,
			Operand: &values.ConstantValue{
				Value: value,
				Typ:   values.UnknownType,
			},
		}
	}

	for _, inequalities := range [][]*predicates.Comparison{
		{comparison(predicates.ComparisonGreaterThan, 1.1), comparison(predicates.ComparisonLessThan, 1.9)},
		{comparison(predicates.ComparisonLessThan, 1.9), comparison(predicates.ComparisonGreaterThan, 1.1)},
	} {
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{build(t, inequalities...)},
			[]values.Type{values.NotNullLong}, nil, false, "empty-integer-intersection",
		)
		if err != nil {
			t.Fatal(err)
		}
		if !spec.empty {
			t.Fatalf("integer interval (1.1,1.9) = %+v, want empty", spec)
		}
	}

	// Mix an exact integer bound with a projected floating bound in both
	// insertion orders. The tighter high endpoint (<=1) must survive.
	for _, inequalities := range [][]*predicates.Comparison{
		{comparison(predicates.ComparisonLessThan, int64(10)), comparison(predicates.ComparisonLessThan, 1.5)},
		{comparison(predicates.ComparisonLessThan, 1.5), comparison(predicates.ComparisonLessThan, int64(10))},
	} {
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{build(t, inequalities...)},
			[]values.Type{values.NotNullLong}, nil, false, "tight-integer-intersection",
		)
		if err != nil {
			t.Fatal(err)
		}
		ranges := materializeEveryRange(t, spec)
		for _, candidate := range []int64{0, 1, 2, 9, 10} {
			got := rangeMembershipCount(ranges, tuple.Tuple{candidate}) == 1
			want := float64(candidate) < 1.5
			if got != want {
				t.Fatalf("candidate %d membership=%v, want %v", candidate, got, want)
			}
		}
	}

	for _, inequalities := range [][]*predicates.Comparison{
		{comparison(predicates.ComparisonGreaterThan, int64(-10)), comparison(predicates.ComparisonGreaterThan, -1.5)},
		{comparison(predicates.ComparisonGreaterThan, -1.5), comparison(predicates.ComparisonGreaterThan, int64(-10))},
	} {
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{build(t, inequalities...)},
			[]values.Type{values.NotNullLong}, nil, false, "tight-integer-low-intersection",
		)
		if err != nil {
			t.Fatal(err)
		}
		ranges := materializeEveryRange(t, spec)
		for _, candidate := range []int64{-10, -9, -2, -1, 0} {
			got := rangeMembershipCount(ranges, tuple.Tuple{candidate}) == 1
			want := float64(candidate) > -1.5
			if got != want {
				t.Fatalf("candidate %d membership=%v, want %v", candidate, got, want)
			}
		}
	}
}

func TestBindScanComparisonsToRangeSet_FloatingBoundsBindInAllInsertionOrders(t *testing.T) {
	t.Parallel()

	comparison := func(comparisonType predicates.ComparisonType, value float64) *predicates.Comparison {
		return &predicates.Comparison{
			Type: comparisonType,
			Operand: &values.ConstantValue{
				Value: value,
				Typ:   values.UnknownType,
			},
		}
	}
	build := func(t *testing.T, comparisons ...*predicates.Comparison) *predicates.ComparisonRange {
		t.Helper()
		result := predicates.EmptyComparisonRange()
		for _, candidate := range comparisons {
			merged := result.Merge(candidate)
			if !merged.Ok {
				t.Fatalf("merge %v failed", candidate.Type)
			}
			result = merged.Range
		}
		return result
	}
	type physicalFloat struct {
		name string
		typ  values.Type
	}
	physicalTypes := []physicalFloat{
		{
			name: "FLOAT",
			typ:  values.NotNullFloat,
		},
		{
			name: "DOUBLE",
			typ:  values.NotNullDouble,
		},
	}
	tests := []struct {
		name        string
		comparisons []*predicates.Comparison
	}{
		{
			name: "greater bounds strictest first",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonGreaterThan, 10),
				comparison(predicates.ComparisonGreaterThan, 0),
			},
		},
		{
			name: "greater bounds strictest last",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonGreaterThan, 0),
				comparison(predicates.ComparisonGreaterThan, 10),
			},
		},
		{
			name: "less bounds strictest first",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonLessThan, 0),
				comparison(predicates.ComparisonLessThan, 10),
			},
		},
		{
			name: "less bounds strictest last",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonLessThan, 10),
				comparison(predicates.ComparisonLessThan, 0),
			},
		},
		{
			name: "equal lower endpoint chooses exclusive",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonGreaterThanEq, 1),
				comparison(predicates.ComparisonGreaterThan, 1),
			},
		},
		{
			name: "equal upper endpoint chooses exclusive",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonLessThanOrEq, 1),
				comparison(predicates.ComparisonLessThan, 1),
			},
		},
		{
			name: "zero closed interval contains both signs",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonGreaterThanEq, math.Copysign(0, -1)),
				comparison(predicates.ComparisonLessThanOrEq, 0),
			},
		},
	}

	for _, physical := range physicalTypes {
		physical := physical
		t.Run(physical.name, func(t *testing.T) {
			t.Parallel()
			for _, test := range tests {
				test := test
				t.Run(test.name, func(t *testing.T) {
					t.Parallel()
					// Combined float bounds bind rather than refuse. A two-sided
					// range needs only ONE physical range (no NaN is logically
					// inside a finite interval); a lower-bounded-only one needs
					// two, because the negative NaNs qualify logically and sit
					// at the bottom of the key space physically.
					spec, err := bindScanComparisonsToRangeSet(
						[]*predicates.ComparisonRange{build(t, test.comparisons...)},
						[]values.Type{physical.typ}, nil, false, "floating-bound-intersection",
					)
					if err != nil {
						t.Fatalf("two-sided float bounds did not bind: %v", err)
					}
					// The exact ranges are proved in
					// float_ordered_range_exactness_test.go; what matters here
					// is that combined bounds still reach an index at all.
					if spec.empty {
						t.Fatalf("combined float bounds bound an EMPTY range set")
					}
				})
			}

			for _, constraints := range [][]*predicates.Comparison{
				{
					comparison(predicates.ComparisonGreaterThan, 10),
					comparison(predicates.ComparisonLessThan, 0),
				},
				{
					comparison(predicates.ComparisonLessThan, 0),
					comparison(predicates.ComparisonGreaterThan, 10),
				},
				{
					comparison(predicates.ComparisonGreaterThan, 0),
					comparison(predicates.ComparisonLessThanOrEq, 0),
				},
			} {
				// Contradictory bounds are EMPTY, not an error: the binder now
				// intersects them and proves no key can qualify.
				spec, err := bindScanComparisonsToRangeSet(
					[]*predicates.ComparisonRange{build(t, constraints...)},
					[]values.Type{physical.typ}, nil, false, "contradictory-floating-bounds",
				)
				if err != nil {
					t.Fatalf("contradictory %s bounds errored instead of proving emptiness: %v",
						physical.name, err)
				}
				if !spec.empty {
					t.Fatalf("contradictory %s bounds bound a NON-empty range set", physical.name)
				}
			}
		})
	}
}

func TestRangeTailTightening_FloatingBoundsUseTupleOrder(t *testing.T) {
	t.Parallel()

	for _, physical := range []struct {
		name string
		zero func(bool) any
		item func(float64) any
	}{
		{
			name: "FLOAT",
			zero: func(negative bool) any {
				if negative {
					return float32(math.Copysign(0, -1))
				}
				return float32(0)
			},
			item: func(value float64) any { return float32(value) },
		},
		{
			name: "DOUBLE",
			zero: func(negative bool) any {
				if negative {
					return math.Copysign(0, -1)
				}
				return float64(0)
			},
			item: func(value float64) any { return value },
		},
	} {
		physical := physical
		t.Run(physical.name, func(t *testing.T) {
			t.Parallel()

			for _, lowItems := range [][]any{
				{physical.item(10), physical.item(0)},
				{physical.item(0), physical.item(10)},
			} {
				tail := boundRangeTail{kind: boundRangeTailInequality}
				for _, item := range lowItems {
					tightenRangeTailLow(&tail, item, recordlayer.EndpointTypeRangeExclusive)
				}
				if compareTupleRangeItems(tail.lowItem, physical.item(10)) != 0 ||
					tail.lowEndpoint != recordlayer.EndpointTypeRangeExclusive {
					t.Fatalf("low bound = %T(%v), %v; want > 10",
						tail.lowItem, tail.lowItem, tail.lowEndpoint)
				}
			}

			for _, highItems := range [][]any{
				{physical.item(0), physical.item(10)},
				{physical.item(10), physical.item(0)},
			} {
				tail := boundRangeTail{kind: boundRangeTailInequality}
				for _, item := range highItems {
					tightenRangeTailHigh(&tail, item, recordlayer.EndpointTypeRangeExclusive)
				}
				if compareTupleRangeItems(tail.highItem, physical.item(0)) != 0 ||
					tail.highEndpoint != recordlayer.EndpointTypeRangeExclusive {
					t.Fatalf("high bound = %T(%v), %v; want < 0",
						tail.highItem, tail.highItem, tail.highEndpoint)
				}
			}

			tie := boundRangeTail{kind: boundRangeTailInequality}
			tightenRangeTailLow(&tie, physical.item(1), recordlayer.EndpointTypeRangeInclusive)
			tightenRangeTailLow(&tie, physical.item(1), recordlayer.EndpointTypeRangeExclusive)
			if tie.lowEndpoint != recordlayer.EndpointTypeRangeExclusive {
				t.Fatal("equal lower endpoint did not retain exclusive bound")
			}
			tightenRangeTailHigh(&tie, physical.item(1), recordlayer.EndpointTypeRangeInclusive)
			if !rangeTailIsEmpty(tie) {
				t.Fatal("equal exclusive/inclusive endpoints were not contradictory")
			}

			zeros := boundRangeTail{kind: boundRangeTailInequality}
			tightenRangeTailLow(&zeros, physical.zero(true), recordlayer.EndpointTypeRangeInclusive)
			tightenRangeTailHigh(&zeros, physical.zero(false), recordlayer.EndpointTypeRangeInclusive)
			if rangeTailIsEmpty(zeros) || compareTupleRangeItems(zeros.lowItem, zeros.highItem) >= 0 {
				t.Fatalf("closed signed-zero interval = %+v, want ordered [-0,+0]", zeros)
			}

			for _, bounds := range [][2]any{
				{physical.item(10), physical.item(0)},
				{physical.zero(false), physical.zero(true)},
			} {
				contradiction := boundRangeTail{kind: boundRangeTailInequality}
				tightenRangeTailLow(&contradiction, bounds[0], recordlayer.EndpointTypeRangeExclusive)
				tightenRangeTailHigh(&contradiction, bounds[1], recordlayer.EndpointTypeRangeExclusive)
				if !rangeTailIsEmpty(contradiction) {
					t.Fatalf("bounds (%v,%v) were not contradictory", bounds[0], bounds[1])
				}
			}
		})
	}
}

func TestBindScanComparisonsToRangeSet_StringBoundsIntersectOrderIndependently(t *testing.T) {
	t.Parallel()

	comparison := func(comparisonType predicates.ComparisonType, value string) *predicates.Comparison {
		return &predicates.Comparison{
			Type:    comparisonType,
			Operand: values.LiteralValue(value),
		}
	}
	build := func(t *testing.T, comparisons ...*predicates.Comparison) *predicates.ComparisonRange {
		t.Helper()
		result := predicates.EmptyComparisonRange()
		for _, candidate := range comparisons {
			merged := result.Merge(candidate)
			if !merged.Ok {
				t.Fatalf("merge %v failed", candidate.Type)
			}
			result = merged.Range
		}
		return result
	}
	tests := []struct {
		name        string
		comparisons []*predicates.Comparison
		want        func(string) bool
	}{
		{
			name: "greater strictest first",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonGreaterThan, "m"),
				comparison(predicates.ComparisonGreaterThan, "a"),
			},
			want: func(value string) bool { return value > "m" },
		},
		{
			name: "greater strictest last",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonGreaterThan, "a"),
				comparison(predicates.ComparisonGreaterThan, "m"),
			},
			want: func(value string) bool { return value > "m" },
		},
		{
			name: "less strictest first",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonLessThan, "m"),
				comparison(predicates.ComparisonLessThan, "z"),
			},
			want: func(value string) bool { return value < "m" },
		},
		{
			name: "less strictest last",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonLessThan, "z"),
				comparison(predicates.ComparisonLessThan, "m"),
			},
			want: func(value string) bool { return value < "m" },
		},
		{
			name: "equal endpoint keeps exclusive",
			comparisons: []*predicates.Comparison{
				comparison(predicates.ComparisonGreaterThanEq, "m"),
				comparison(predicates.ComparisonGreaterThan, "m"),
			},
			want: func(value string) bool { return value > "m" },
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			spec, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{build(t, test.comparisons...)},
				[]values.Type{values.NotNullString}, nil, false, "string-bound-intersection",
			)
			if err != nil {
				t.Fatal(err)
			}
			ranges := materializeEveryRange(t, spec)
			for _, candidate := range []string{"", "a", "b", "m", "n", "z", "zz"} {
				got := rangeMembershipCount(ranges, tuple.Tuple{candidate}) == 1
				if want := test.want(candidate); got != want {
					t.Fatalf("candidate %q membership=%v, want %v", candidate, got, want)
				}
			}
		})
	}

	for _, constraints := range [][]*predicates.Comparison{
		{
			comparison(predicates.ComparisonGreaterThan, "z"),
			comparison(predicates.ComparisonLessThan, "a"),
		},
		{
			comparison(predicates.ComparisonLessThan, "a"),
			comparison(predicates.ComparisonGreaterThan, "z"),
		},
		{
			comparison(predicates.ComparisonGreaterThan, "m"),
			comparison(predicates.ComparisonLessThanOrEq, "m"),
		},
	} {
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{build(t, constraints...)},
			[]values.Type{values.NotNullString}, nil, false, "contradictory-string-bounds",
		)
		if err != nil {
			t.Fatal(err)
		}
		if !spec.empty || spec.materialize != nil {
			t.Fatal("contradictory string bounds produced a non-empty range")
		}
	}
}

func TestBindScanComparisonsToRangeSet_RejectsNonContiguousComparisonShape(t *testing.T) {
	t.Parallel()

	equality := scanRangeTestEq(t, int64(1))
	inequality := scanRangeTestComparison(t, predicates.ComparisonGreaterThan, values.LiteralValue(int64(0)))
	tests := []struct {
		name        string
		comparisons []*predicates.ComparisonRange
		component   int
	}{
		{
			name:        "equality after gap",
			comparisons: []*predicates.ComparisonRange{equality, predicates.EmptyComparisonRange(), equality},
			component:   2,
		},
		{
			name:        "equality after inequality tail",
			comparisons: []*predicates.ComparisonRange{inequality, equality},
			component:   1,
		},
		{
			name:        "second inequality component",
			comparisons: []*predicates.ComparisonRange{inequality, inequality},
			component:   1,
		},
		{
			name:        "constraint after tail and empty",
			comparisons: []*predicates.ComparisonRange{equality, inequality, predicates.EmptyComparisonRange(), equality},
			component:   3,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			keyTypes := make([]values.Type, len(test.comparisons))
			for i := range keyTypes {
				keyTypes[i] = values.NotNullLong
			}
			_, err := bindScanComparisonsToRangeSet(
				test.comparisons, keyTypes, nil, false, "invalid-comparison-shape",
			)
			var shape *InvalidScanComparisonShapeError
			if !errors.As(err, &shape) || shape.Component != test.component {
				t.Fatalf("error = %T(%v), want InvalidScanComparisonShapeError at %d",
					err, err, test.component)
			}
		})
	}

	if _, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{
			equality, inequality, predicates.EmptyComparisonRange(), nil,
		},
		[]values.Type{values.NotNullLong, values.NotNullLong},
		nil, false, "valid-comparison-shape",
	); err != nil {
		t.Fatalf("valid equality/tail/empty shape: %v", err)
	}
}

func TestBindScanComparisonsToRangeSet_RejectsMalformedTailBeforeProjection(t *testing.T) {
	t.Parallel()

	for comparisonType := predicates.ComparisonEquals; comparisonType <= predicates.ComparisonDistanceRankLessThanOrEq; comparisonType++ {
		switch comparisonType {
		case predicates.ComparisonEquals, predicates.ComparisonIsNull,
			predicates.ComparisonNotDistinctFrom:
			// ComparisonRange classifies these as equality, not a tail.
			continue
		case predicates.ComparisonLessThan, predicates.ComparisonLessThanOrEq,
			predicates.ComparisonGreaterThan, predicates.ComparisonGreaterThanEq,
			predicates.ComparisonIsNotNull, predicates.ComparisonStartsWith:
			// These are the complete supported tail operator set.
			continue
		}
		comparisonType := comparisonType
		t.Run(fmt.Sprintf("unsupported_%v", comparisonType), func(t *testing.T) {
			t.Parallel()
			rangeWithFloat := scanRangeTestComparison(
				t, comparisonType, values.LiteralValue(float64(1.5)),
			)
			spec, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{rangeWithFloat},
				[]values.Type{values.NotNullLong}, nil, false,
				"malformed-float-tail",
			)
			var shape *InvalidScanComparisonShapeError
			if !errors.As(err, &shape) || shape.Component != 0 {
				t.Fatalf("error = %T(%v), want InvalidScanComparisonShapeError at 0", err, err)
			}
			if spec.materialize != nil {
				t.Fatal("malformed float tail returned a storage materializer")
			}
		})
	}

	for _, comparisonType := range []predicates.ComparisonType{
		predicates.ComparisonLessThan,
		predicates.ComparisonLessThanOrEq,
		predicates.ComparisonGreaterThan,
		predicates.ComparisonGreaterThanEq,
		predicates.ComparisonStartsWith,
	} {
		comparisonType := comparisonType
		t.Run(fmt.Sprintf("missing_operand_%v", comparisonType), func(t *testing.T) {
			t.Parallel()
			missingOperand := scanRangeTestComparison(t, comparisonType, nil)
			_, err := bindScanComparisonsToRangeSet(
				[]*predicates.ComparisonRange{missingOperand},
				[]values.Type{values.NotNullLong}, nil, false,
				"missing-tail-operand",
			)
			var shape *InvalidScanComparisonShapeError
			if !errors.As(err, &shape) || shape.Component != 0 {
				t.Fatalf("error = %T(%v), want InvalidScanComparisonShapeError at 0", err, err)
			}
		})
	}

	malformedIsNotNull := scanRangeTestComparison(
		t, predicates.ComparisonIsNotNull, values.LiteralValue(float64(1)),
	)
	_, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{malformedIsNotNull},
		[]values.Type{values.NotNullLong}, nil, false,
		"malformed-is-not-null",
	)
	var shape *InvalidScanComparisonShapeError
	if !errors.As(err, &shape) {
		t.Fatalf("IS NOT NULL with operand error = %T(%v), want InvalidScanComparisonShapeError", err, err)
	}
}

func integerFloatComparisonHolds(
	integer int64,
	floating float64,
	comparisonType predicates.ComparisonType,
) bool {
	comparison := compareIntegerToFloat(integer, floating)
	switch comparisonType {
	case predicates.ComparisonEquals, predicates.ComparisonNotDistinctFrom:
		return comparison == 0
	case predicates.ComparisonLessThan:
		return comparison < 0
	case predicates.ComparisonLessThanOrEq:
		return comparison <= 0
	case predicates.ComparisonGreaterThan:
		return comparison > 0
	case predicates.ComparisonGreaterThanEq:
		return comparison >= 0
	default:
		panic(fmt.Sprintf("unsupported integer/float comparison %v", comparisonType))
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func numericComparisonHolds(left, right float64, comparisonType predicates.ComparisonType) bool {
	switch comparisonType {
	case predicates.ComparisonLessThan:
		return left < right
	case predicates.ComparisonLessThanOrEq:
		return left <= right
	case predicates.ComparisonGreaterThan:
		return left > right
	case predicates.ComparisonGreaterThanEq:
		return left >= right
	default:
		panic(fmt.Sprintf("unsupported numeric comparison %v", comparisonType))
	}
}

func mustNumericFloat64(t *testing.T, value any) float64 {
	t.Helper()
	result, _, ok := values.ToFloat64(value)
	if !ok {
		t.Fatalf("%T(%v) is not numeric", value, value)
	}
	return result
}

func TestBindScanComparisonsToRangeSet_NaNFailsBeforeMaterialization(t *testing.T) {
	t.Parallel()
	_, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{
			scanRangeTestEq(t, math.NaN()),
			scanRangeTestEq(t, int64(5)),
		},
		[]values.Type{values.NotNullDouble, values.NotNullLong},
		nil,
		false,
		"idx:T_VW",
	)
	var unsupported *UnsupportedPhysicalFloatEquivalenceError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %T(%v), want UnsupportedPhysicalFloatEquivalenceError", err, err)
	}
	if unsupported.Component != 0 {
		t.Fatalf("NaN component = %d, want 0", unsupported.Component)
	}
}

func TestBindScanComparisonsToRangeSet_NullEqualityIsEmpty(t *testing.T) {
	t.Parallel()
	spec, err := bindScanComparisonsToRangeSet(
		[]*predicates.ComparisonRange{scanRangeTestEq(t, nil)},
		[]values.Type{values.NullableDouble},
		nil,
		false,
		"idx:T_V",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.empty || spec.materialize != nil {
		t.Fatalf("NULL equality spec = %+v, want explicit empty without materializer", spec)
	}
}

func TestBindScanComparisonsToRangeSet_FingerprintCanonicalizesZeroSignAndBindsPlan(t *testing.T) {
	t.Parallel()
	bind := func(zero float64, salt string) scanRangeSetSpec {
		spec, err := bindScanComparisonsToRangeSet(
			[]*predicates.ComparisonRange{
				scanRangeTestEq(t, zero),
				scanRangeTestEq(t, int64(5)),
			},
			[]values.Type{values.NotNullDouble, values.NotNullLong},
			nil,
			false,
			salt,
		)
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}
	negative := bind(math.Copysign(0, -1), "idx:A")
	positive := bind(float64(0), "idx:A")
	otherPlan := bind(float64(0), "idx:B")
	if !bytes.Equal(negative.fingerprint, positive.fingerprint) {
		t.Fatal("logically equal zero signs produced different continuation fingerprints")
	}
	if bytes.Equal(positive.fingerprint, otherPlan.fingerprint) {
		t.Fatal("different physical plans produced the same continuation fingerprint")
	}
}

func TestBindScanComparisonsToRangeSet_StateIsLinearInComponents(t *testing.T) {
	t.Parallel()
	const zeroComponents = 64
	comparisons := make([]*predicates.ComparisonRange, 0, zeroComponents+1)
	types := make([]values.Type, 0, zeroComponents+1)
	for range zeroComponents {
		comparisons = append(comparisons, scanRangeTestEq(t, float64(0)))
		types = append(types, values.NotNullDouble)
	}
	comparisons = append(comparisons, scanRangeTestEq(t, int64(1)))
	types = append(types, values.NotNullLong)

	spec, err := bindScanComparisonsToRangeSet(comparisons, types, nil, false, "idx:wide")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.alternativeCounts) != zeroComponents+1 {
		t.Fatalf("state components = %d, want %d", len(spec.alternativeCounts), zeroComponents+1)
	}
	for i := 0; i < zeroComponents; i++ {
		if spec.alternativeCounts[i] != 2 {
			t.Fatalf("component %d alternatives = %d, want 2", i, spec.alternativeCounts[i])
		}
	}
	if len(spec.fingerprint) != 32 {
		t.Fatalf("fingerprint bytes = %d, want fixed SHA-256 length", len(spec.fingerprint))
	}
}

func assertExactZeroRange(
	t *testing.T,
	rng recordlayer.TupleRange,
	wantZero any,
	wantSuffix int64,
) {
	t.Helper()
	if len(rng.Low) != 2 || len(rng.High) != 2 {
		t.Fatalf("range = %+v, want exact two-component tuple", rng)
	}
	wantWidth, wantNegative, ok := floatingZeroIdentity(wantZero)
	if !ok {
		t.Fatalf("test setup: expected zero = %T(%v), want FLOAT or DOUBLE zero", wantZero, wantZero)
	}
	for _, endpoint := range []struct {
		name  string
		value any
	}{
		{name: "low", value: rng.Low[0]},
		{name: "high", value: rng.High[0]},
	} {
		width, negative, isZero := floatingZeroIdentity(endpoint.value)
		if !isZero || width != wantWidth || negative != wantNegative {
			t.Fatalf("%s zero = %T(%v), identity=(width:%d negative:%v zero:%v), want width:%d negative:%v",
				endpoint.name, endpoint.value, endpoint.value, width, negative, isZero, wantWidth, wantNegative)
		}
	}
	if !bytes.Equal(rng.Low.Pack(), rng.High.Pack()) {
		t.Fatalf("range endpoints pack differently: low=%x high=%x", rng.Low.Pack(), rng.High.Pack())
	}
	if rng.Low[1] != wantSuffix || rng.High[1] != wantSuffix ||
		rng.LowEndpoint != recordlayer.EndpointTypeRangeInclusive ||
		rng.HighEndpoint != recordlayer.EndpointTypeRangeInclusive {
		t.Fatalf("range = %+v, want allOf([zero,%d])", rng, wantSuffix)
	}
}

func floatingZeroIdentity(value any) (width int, negative bool, ok bool) {
	switch zero := value.(type) {
	case float32:
		return 32, math.Signbit(float64(zero)), zero == 0
	case float64:
		return 64, math.Signbit(zero), zero == 0
	default:
		return 0, false, false
	}
}

func equalUint32s(left, right []uint32) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func materializeEveryRange(t *testing.T, spec scanRangeSetSpec) []recordlayer.TupleRange {
	t.Helper()
	if spec.empty {
		return nil
	}
	choices := make([]uint32, len(spec.alternativeCounts))
	ranges := make([]recordlayer.TupleRange, 0)
	for {
		rng, err := spec.materialize(choices)
		if err != nil {
			t.Fatalf("materialize choices %v: %v", choices, err)
		}
		ranges = append(ranges, rng)
		if !advanceScanRangeChoices(choices, spec.alternativeCounts) {
			return ranges
		}
	}
}

func rangeMembershipCount(ranges []recordlayer.TupleRange, candidate tuple.Tuple) int {
	ss := subspace.FromBytes([]byte{0x42})
	key := ss.Pack(candidate)
	count := 0
	for _, rng := range ranges {
		fdbRange := rng.ToFDBRange(ss)
		begin, end := fdbRange.FDBRangeKeys()
		if bytes.Compare(key, begin.FDBKey()) >= 0 && bytes.Compare(key, end.FDBKey()) < 0 {
			count++
		}
	}
	return count
}
