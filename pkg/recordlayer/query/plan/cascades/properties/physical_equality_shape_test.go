package properties

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func physicalShapeEquality(t *testing.T, operand values.Value) *predicates.ComparisonRange {
	t.Helper()
	comparison := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: operand}
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to build equality range")
	}
	return merged.Range
}

func physicalShapeLiteral(t *testing.T, literal any) *predicates.ComparisonRange {
	t.Helper()
	return physicalShapeEquality(t, values.LiteralValue(literal))
}

func TestPhysicalEqualityShape_AuthorityAndOperandMatrix(t *testing.T) {
	t.Parallel()

	typedDynamic := func(typ values.Type) values.Value {
		return &values.ParameterValue{Ordinal: 1, Typ: typ}
	}
	tests := []struct {
		name              string
		physicalType      values.Type
		operand           values.Value
		fixed             bool
		fanout            bool
		seeks             int64
		multiplicity      int64
		multiplicityKnown bool
		knownNaN          bool
		unsatisfiable     bool
	}{
		{name: "FLOAT authority coerces integer zero", physicalType: values.NotNullFloat, operand: values.LiteralValue(int64(0)), fanout: true, seeks: 2, multiplicity: 2, multiplicityKnown: true},
		{name: "DOUBLE authority coerces integer zero", physicalType: values.NotNullDouble, operand: values.LiteralValue(int64(0)), fanout: true, seeks: 2, multiplicity: 2, multiplicityKnown: true},
		{name: "known INT overrides floating zero RHS", physicalType: values.NotNullLong, operand: values.LiteralValue(float64(0)), fixed: true, seeks: 1, multiplicity: 1, multiplicityKnown: true},
		{name: "known INT overrides NaN RHS", physicalType: values.NotNullLong, operand: values.LiteralValue(math.NaN()), fixed: true, seeks: 1, multiplicity: 1, multiplicityKnown: true},
		{name: "Unknown physical rejects floating-literal inference", physicalType: values.UnknownType, operand: values.LiteralValue(float64(0)), fanout: false, seeks: 2},
		{name: "Unknown physical rejects integer-literal inference", physicalType: values.UnknownType, operand: values.LiteralValue(int64(0)), fanout: false, seeks: 2},
		{name: "nonzero float is fixed", physicalType: values.NotNullDouble, operand: values.LiteralValue(-1.25), fixed: true, seeks: 1, multiplicity: 1, multiplicityKnown: true},
		{name: "positive infinity is fixed", physicalType: values.NotNullDouble, operand: values.LiteralValue(math.Inf(1)), fixed: true, seeks: 1, multiplicity: 1, multiplicityKnown: true},
		{name: "FLOAT coercion can round tiny DOUBLE to zero", physicalType: values.NotNullFloat, operand: values.LiteralValue(math.SmallestNonzeroFloat64), fanout: true, seeks: 2, multiplicity: 2, multiplicityKnown: true},
		{name: "DOUBLE retains tiny nonzero", physicalType: values.NotNullDouble, operand: values.LiteralValue(math.SmallestNonzeroFloat64), fixed: true, seeks: 1, multiplicity: 1, multiplicityKnown: true},
		{name: "known float NaN is unsupported", physicalType: values.NotNullDouble, operand: values.LiteralValue(math.NaN()), fanout: true, seeks: 2, knownNaN: true},
		{name: "typed dynamic cannot resolve Unknown physical type", physicalType: values.UnknownType, operand: typedDynamic(values.NullableDouble), seeks: 2},
		{name: "physical FLOAT overrides Unknown dynamic", physicalType: values.NotNullFloat, operand: typedDynamic(values.UnknownType), fanout: true, seeks: 2},
		{name: "physical INT overrides Unknown dynamic", physicalType: values.NotNullLong, operand: typedDynamic(values.UnknownType), fixed: true, seeks: 1, multiplicity: 1, multiplicityKnown: true},
		{name: "genuinely Unknown dynamic is conservative", physicalType: values.UnknownType, operand: typedDynamic(values.UnknownType), seeks: 2},
		{name: "ordinary equals NULL is empty", physicalType: values.NullableDouble, operand: values.LiteralValue(nil), fixed: true, seeks: 1, multiplicity: 0, multiplicityKnown: true, unsatisfiable: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			shape := PhysicalEqualityShapeForComparisons(
				[]*predicates.ComparisonRange{physicalShapeEquality(t, test.operand)},
				[]values.Type{test.physicalType},
				false,
			)
			if len(shape.Components) != 1 {
				t.Fatalf("components = %d, want 1", len(shape.Components))
			}
			component := shape.Components[0]
			if component.PhysicallyFixed != test.fixed || component.MayFanOut != test.fanout {
				t.Fatalf("component = %+v, want fixed=%v fanout=%v", component, test.fixed, test.fanout)
			}
			if shape.SuccessfulSeekUpperBound != test.seeks {
				t.Fatalf("seek upper bound = %d, want %d", shape.SuccessfulSeekUpperBound, test.seeks)
			}
			if shape.UnsupportedKnownNaN != test.knownNaN || shape.Unsatisfiable != test.unsatisfiable {
				t.Fatalf("shape = %+v, want knownNaN=%v unsatisfiable=%v", shape, test.knownNaN, test.unsatisfiable)
			}
			if shape.ProvenRowMultiplicity.IsUnknown() == test.multiplicityKnown {
				t.Fatalf("multiplicity known = %v, want %v", !shape.ProvenRowMultiplicity.IsUnknown(), test.multiplicityKnown)
			}
			if test.multiplicityKnown && shape.ProvenRowMultiplicity.Value() != test.multiplicity {
				t.Fatalf("multiplicity = %d, want %d", shape.ProvenRowMultiplicity.Value(), test.multiplicity)
			}
		})
	}
}

func TestPhysicalEqualityShape_IsNullIsOnePhysicalKey(t *testing.T) {
	t.Parallel()
	comparison := predicates.Comparison{Type: predicates.ComparisonIsNull}
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to build IS NULL range")
	}
	shape := PhysicalEqualityShapeForComparisons(
		[]*predicates.ComparisonRange{merged.Range}, []values.Type{values.NullableDouble}, false,
	)
	if !shape.PhysicallyFixed || shape.MayFanOut || shape.ProvenRowMultiplicity.IsUnknown() ||
		shape.ProvenRowMultiplicity.Value() != 1 {
		t.Fatalf("IS NULL shape = %+v, want one fixed physical key", shape)
	}
}

func TestPhysicalEqualityShape_NullSafeEqualityWithNullIsOnePhysicalKey(t *testing.T) {
	t.Parallel()
	comparison := predicates.Comparison{
		Type: predicates.ComparisonNotDistinctFrom, Operand: values.LiteralValue(nil),
	}
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to build IS NOT DISTINCT FROM NULL range")
	}
	shape := PhysicalEqualityShapeForComparisons(
		[]*predicates.ComparisonRange{merged.Range}, []values.Type{values.NullableDouble}, false,
	)
	if !shape.PhysicallyFixed || shape.Unsatisfiable || shape.ProvenRowMultiplicity.IsUnknown() ||
		shape.ProvenRowMultiplicity.Value() != 1 {
		t.Fatalf("null-safe NULL shape = %+v, want one satisfiable fixed NULL key", shape)
	}
}

func TestPhysicalEqualityShape_TerminalWideningChangesSeeksNotMultiplicity(t *testing.T) {
	t.Parallel()
	zero := physicalShapeLiteral(t, float64(0))
	shape := PhysicalEqualityShapeForComparisons(
		[]*predicates.ComparisonRange{zero}, []values.Type{values.NotNullDouble}, true,
	)
	if shape.PhysicallyFixed || !shape.MayFanOut {
		t.Fatalf("terminal zero shape = %+v, want expanding/non-fixed", shape)
	}
	if shape.SuccessfulSeekUpperBound != 1 {
		t.Fatalf("terminal zero seeks = %d, want 1 inclusive range", shape.SuccessfulSeekUpperBound)
	}
	if shape.ProvenRowMultiplicity.IsUnknown() || shape.ProvenRowMultiplicity.Value() != 2 {
		t.Fatalf("terminal zero multiplicity = %+v, want 2", shape.ProvenRowMultiplicity)
	}
}

func TestPhysicalEqualityShape_CartesianProductAndOverflow(t *testing.T) {
	t.Parallel()
	zero := physicalShapeLiteral(t, float64(0))
	nonzero := physicalShapeLiteral(t, int64(5))
	shape := PhysicalEqualityShapeForComparisons(
		[]*predicates.ComparisonRange{zero, zero, nonzero},
		[]values.Type{values.NotNullDouble, values.NotNullFloat, values.NotNullLong},
		true,
	)
	if shape.SuccessfulSeekUpperBound != 4 || shape.ProvenRowMultiplicity.IsUnknown() ||
		shape.ProvenRowMultiplicity.Value() != 4 {
		t.Fatalf("two-zero shape = %+v, want four seeks and four-row multiplicity", shape)
	}

	const count = 64
	comps := make([]*predicates.ComparisonRange, count)
	types := make([]values.Type, count)
	for i := range comps {
		comps[i] = zero
		types[i] = values.NotNullDouble
	}
	overflow := PhysicalEqualityShapeForComparisons(comps, types, false)
	if overflow.SuccessfulSeekUpperBound != math.MaxInt64 || overflow.SuccessfulSeekUpperBoundExact {
		t.Fatalf("overflow seek shape = %+v, want finite saturated inexact max", overflow)
	}
	if !overflow.ProvenRowMultiplicity.IsUnknown() {
		t.Fatalf("overflow row multiplicity = %+v, want unknown", overflow.ProvenRowMultiplicity)
	}
}

func TestPhysicalOrderingPrefixLength_FloatNaNCongruence(t *testing.T) {
	t.Parallel()

	inequality := func(literal any) *predicates.ComparisonRange {
		comparison := predicates.NewLiteralComparison(predicates.ComparisonLessThan, literal)
		merged := predicates.EmptyComparisonRange().Merge(&comparison)
		if !merged.Ok {
			t.Fatal("failed to build inequality")
		}
		return merged.Range
	}
	isNullComparison := predicates.Comparison{Type: predicates.ComparisonIsNull}
	isNull := predicates.EmptyComparisonRange().Merge(&isNullComparison)
	if !isNull.Ok {
		t.Fatal("failed to build IS NULL range")
	}

	tests := []struct {
		name  string
		comps []*predicates.ComparisonRange
		types []values.Type
		want  int
	}{
		{name: "unbound DOUBLE stops", types: []values.Type{values.NullableDouble, values.NullableLong}, want: 0},
		{name: "ordered FLOAT stops", comps: []*predicates.ComparisonRange{inequality(float64(0))}, types: []values.Type{values.NullableFloat, values.NullableLong}, want: 0},
		{name: "nonzero equality excludes NaNs", comps: []*predicates.ComparisonRange{physicalShapeLiteral(t, float64(5))}, types: []values.Type{values.NullableDouble, values.NullableLong}, want: 2},
		{name: "signed-zero equality preserves total ORDER BY", comps: []*predicates.ComparisonRange{physicalShapeLiteral(t, float64(0))}, types: []values.Type{values.NullableDouble, values.NullableLong}, want: 2},
		{name: "known NaN equality stops", comps: []*predicates.ComparisonRange{physicalShapeLiteral(t, math.NaN())}, types: []values.Type{values.NullableDouble, values.NullableLong}, want: 0},
		{name: "Unknown non-null stops", comps: []*predicates.ComparisonRange{physicalShapeLiteral(t, int64(1))}, types: []values.Type{values.UnknownType, values.NullableLong}, want: 0},
		{name: "type-independent IS NULL crosses Unknown", comps: []*predicates.ComparisonRange{isNull.Range}, types: []values.Type{values.UnknownType, values.NullableLong}, want: 2},
		{name: "known scalar prefix stops before unbound DOUBLE", types: []values.Type{values.NullableLong, values.NullableDouble, values.NullableLong}, want: 1},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := PhysicalOrderingPrefixLength(test.comps, test.types, len(test.types)); got != test.want {
				t.Fatalf("PhysicalOrderingPrefixLength = %d, want %d", got, test.want)
			}
		})
	}
}
