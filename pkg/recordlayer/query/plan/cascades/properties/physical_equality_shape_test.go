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

func TestProvenFullEqualityMultiplicity_UniqueNullSemantics(t *testing.T) {
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
	nullSafeDynamic := func(typ values.Type) *predicates.ComparisonRange {
		return rangeOf(predicates.Comparison{
			Type:    predicates.ComparisonNotDistinctFrom,
			Operand: &values.ParameterValue{Ordinal: 1, Typ: typ},
		})
	}

	tests := []struct {
		name      string
		comps     []*predicates.ComparisonRange
		types     []values.Type
		semantics KeyUniquenessSemantics
		want      int64
		known     bool
	}{
		{name: "tuple uniqueness includes nullable NULL key", comps: []*predicates.ComparisonRange{isNull}, types: []values.Type{values.NullableLong}, semantics: TupleKeyUnique, want: 1, known: true},
		{name: "secondary nullable IS NULL has duplicate prefixes", comps: []*predicates.ComparisonRange{isNull}, types: []values.Type{values.NullableLong}, semantics: SecondaryUniqueNullsDistinct},
		{name: "secondary NOT NULL IS NULL is empty", comps: []*predicates.ComparisonRange{isNull}, types: []values.Type{values.NotNullLong}, semantics: SecondaryUniqueNullsDistinct, known: true},
		{name: "secondary nullable null-safe NULL has duplicate prefixes", comps: []*predicates.ComparisonRange{nullSafeNull}, types: []values.Type{values.NullableLong}, semantics: SecondaryUniqueNullsDistinct},
		{name: "secondary NOT NULL null-safe NULL is empty", comps: []*predicates.ComparisonRange{nullSafeNull}, types: []values.Type{values.NotNullLong}, semantics: SecondaryUniqueNullsDistinct, known: true},
		{name: "secondary nullable dynamic null-safe equality can bind NULL", comps: []*predicates.ComparisonRange{nullSafeDynamic(values.NullableLong)}, types: []values.Type{values.NullableLong}, semantics: SecondaryUniqueNullsDistinct},
		{name: "secondary nonnullable dynamic null-safe equality is unique", comps: []*predicates.ComparisonRange{rangeOf(predicates.Comparison{Type: predicates.ComparisonNotDistinctFrom, Operand: propertyField(t, "X", values.NotNullLong)})}, types: []values.Type{values.NullableLong}, semantics: SecondaryUniqueNullsDistinct, want: 1, known: true},
		{name: "secondary nullable ordinary dynamic equality is empty-or-unique", comps: []*predicates.ComparisonRange{physicalShapeEquality(t, &values.ParameterValue{Ordinal: 1, Typ: values.NullableLong})}, types: []values.Type{values.NullableLong}, semantics: SecondaryUniqueNullsDistinct, want: 1, known: true},
		{name: "secondary ordinary equals NULL is empty", comps: []*predicates.ComparisonRange{physicalShapeLiteral(t, nil)}, types: []values.Type{values.NullableLong}, semantics: SecondaryUniqueNullsDistinct, known: true},
		{name: "secondary signed zero still has two raw unique prefixes", comps: []*predicates.ComparisonRange{physicalShapeLiteral(t, float64(0))}, types: []values.Type{values.NullableDouble}, semantics: SecondaryUniqueNullsDistinct, want: 2, known: true},
		{name: "missing physical nullability declines", comps: []*predicates.ComparisonRange{isNull}, types: []values.Type{values.UnknownType}, semantics: SecondaryUniqueNullsDistinct},
		{name: "composite nullable component poisons uniqueness", comps: []*predicates.ComparisonRange{physicalShapeLiteral(t, int64(7)), isNull}, types: []values.Type{values.NotNullLong, values.NullableLong}, semantics: SecondaryUniqueNullsDistinct},
		{name: "non-leading equality gap is not full coverage", comps: []*predicates.ComparisonRange{predicates.EmptyComparisonRange(), physicalShapeLiteral(t, int64(7))}, types: []values.Type{values.NotNullLong, values.NotNullLong}, semantics: SecondaryUniqueNullsDistinct},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := ProvenFullEqualityMultiplicity(test.comps, test.types, len(test.comps), test.semantics)
			if got.IsUnknown() == test.known {
				t.Fatalf("known = %v, want %v", !got.IsUnknown(), test.known)
			}
			if test.known && got.Value() != test.want {
				t.Fatalf("multiplicity = %d, want %d", got.Value(), test.want)
			}
		})
	}
}

func TestSecondaryUniqueKeyGloballyEnforced(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		types []values.Type
		arity int
		want  bool
	}{
		{name: "all authoritative NOT NULL", types: []values.Type{values.NotNullLong, values.NotNullString}, arity: 2, want: true},
		{name: "nullable component", types: []values.Type{values.NotNullLong, values.NullableString}, arity: 2},
		{name: "unknown component", types: []values.Type{values.NotNullLong, values.UnknownType}, arity: 2},
		{name: "FLOAT raw NaN encodings", types: []values.Type{values.NotNullFloat}, arity: 1},
		{name: "DOUBLE raw NaN encodings", types: []values.Type{values.NotNullDouble}, arity: 1},
		{name: "missing component", types: []values.Type{values.NotNullLong}, arity: 2},
		{name: "extra component is ambiguous", types: []values.Type{values.NotNullLong, values.NullableString}, arity: 1},
		{name: "zero arity", types: []values.Type{values.NotNullLong}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := SecondaryUniqueKeyGloballyEnforced(test.types, test.arity); got != test.want {
				t.Fatalf("SecondaryUniqueKeyGloballyEnforced = %v, want %v", got, test.want)
			}
		})
	}
}

func TestTupleKeyUniquenessMatchesLogicalEquality(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		types []values.Type
		arity int
		want  bool
	}{
		{name: "authoritative NOT NULL", types: []values.Type{values.NotNullLong, values.NotNullString}, arity: 2, want: true},
		{name: "nullable primary component", types: []values.Type{values.NullableLong, values.NullableString}, arity: 2, want: true},
		{name: "unknown component", types: []values.Type{values.NotNullLong, values.UnknownType}, arity: 2},
		{name: "nil component", types: []values.Type{values.NotNullLong, nil}, arity: 2},
		{name: "FLOAT raw NaN encodings", types: []values.Type{values.NotNullFloat}, arity: 1},
		{name: "nullable FLOAT raw NaN encodings", types: []values.Type{values.NullableFloat}, arity: 1},
		{name: "DOUBLE raw NaN encodings", types: []values.Type{values.NotNullDouble}, arity: 1},
		{name: "missing component", types: []values.Type{values.NotNullLong}, arity: 2},
		{name: "extra component", types: []values.Type{values.NotNullLong, values.NullableString}, arity: 1},
		{name: "zero arity", types: []values.Type{values.NotNullLong}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := TupleKeyUniquenessMatchesLogicalEquality(test.types, test.arity); got != test.want {
				t.Fatalf("TupleKeyUniquenessMatchesLogicalEquality = %v, want %v", got, test.want)
			}
		})
	}
}

func TestValueIdentityMatchesLogicalEqualityIsRecursive(t *testing.T) {
	t.Parallel()
	safeRecord := values.NewRecordType("", true, []values.Field{
		{Name: "A", FieldType: values.NullableLong},
		{Name: "B", FieldType: values.NewArrayType(true, values.NullableString)},
	})
	unsafeRecord := values.NewRecordType("", true, []values.Field{
		{Name: "A", FieldType: values.NullableLong},
		{Name: "B", FieldType: values.NewArrayType(true, values.NullableDouble)},
	})
	for _, test := range []struct {
		name string
		typ  values.Type
		want bool
	}{
		{name: "safe nested record", typ: safeRecord, want: true},
		{name: "nested DOUBLE", typ: unsafeRecord},
		{name: "erased array", typ: values.NewArrayType(true, nil)},
		{name: "unknown", typ: values.UnknownType},
		{name: "enum-like scalar", typ: values.NotNullString, want: true},
	} {
		if got := ValueIdentityMatchesLogicalEquality(test.typ); got != test.want {
			t.Errorf("%s: ValueIdentityMatchesLogicalEquality = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestLogicalEqualityAtMostOnePhysicalKey(t *testing.T) {
	t.Parallel()
	dynamic := func(typ values.Type) values.Value {
		return &values.ParameterValue{Ordinal: 1, Typ: typ}
	}
	tests := []struct {
		name         string
		physicalType values.Type
		operand      values.Value
		want         bool
	}{
		{name: "DOUBLE positive zero spans both signs", physicalType: values.NotNullDouble, operand: values.LiteralValue(float64(0))},
		{name: "DOUBLE negative zero spans both signs", physicalType: values.NotNullDouble, operand: values.LiteralValue(math.Copysign(0, -1))},
		{name: "FLOAT integer zero spans both signs", physicalType: values.NotNullFloat, operand: values.LiteralValue(int64(0))},
		{name: "DOUBLE NaN spans raw payloads", physicalType: values.NotNullDouble, operand: values.LiteralValue(math.NaN())},
		{name: "DOUBLE finite nonzero is singleton", physicalType: values.NotNullDouble, operand: values.LiteralValue(1.25), want: true},
		{name: "FLOAT tiny DOUBLE is empty not narrowed", physicalType: values.NotNullFloat, operand: values.LiteralValue(math.SmallestNonzeroFloat64), want: true},
		{name: "dynamic DOUBLE can become zero or NaN", physicalType: values.NotNullDouble, operand: dynamic(values.NotNullDouble)},
		{name: "dynamic integer can become floating-key zero", physicalType: values.NotNullDouble, operand: dynamic(values.NotNullLong)},
		{name: "LONG integer literal is singleton", physicalType: values.NotNullLong, operand: values.LiteralValue(int64(7)), want: true},
		{name: "LONG exactly represented float is singleton", physicalType: values.NotNullLong, operand: values.LiteralValue(float64(7)), want: true},
		{name: "LONG fractional float is empty", physicalType: values.NotNullLong, operand: values.LiteralValue(1.5), want: true},
		{name: "LONG positive precision cliff is plural", physicalType: values.NotNullLong, operand: values.LiteralValue(float64(1 << 53))},
		{name: "LONG negative precision cliff is plural", physicalType: values.NotNullLong, operand: values.LiteralValue(-float64(1 << 53))},
		{name: "LONG dynamic float can reach precision cliff", physicalType: values.NotNullLong, operand: dynamic(values.NotNullDouble)},
		{name: "LONG dynamic integer stays exact", physicalType: values.NotNullLong, operand: dynamic(values.NotNullLong), want: true},
		{name: "integer key never equals NaN", physicalType: values.NotNullLong, operand: values.LiteralValue(math.NaN()), want: true},
		{name: "STRING dynamic equality is exact", physicalType: values.NotNullString, operand: dynamic(values.NotNullString), want: true},
		{name: "Unknown physical type declines", physicalType: values.UnknownType, operand: values.LiteralValue(int64(7))},
		{name: "NULL equality is empty without physical metadata", physicalType: values.UnknownType, operand: values.LiteralValue(nil), want: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := LogicalEqualityAtMostOnePhysicalKey(
				physicalShapeEquality(t, test.operand), test.physicalType,
			)
			if got != test.want {
				t.Fatalf("LogicalEqualityAtMostOnePhysicalKey = %v, want %v", got, test.want)
			}
		})
	}
}

func TestLogicalEqualityProjectionInjective(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source values.Type
		target values.Type
		want   bool
	}{
		{name: "LONG to LONG", source: values.NotNullLong, target: values.NullableLong, want: true},
		{name: "INT to LONG", source: values.NotNullInt, target: values.NotNullLong, want: true},
		{name: "LONG to INT tuple integer carrier", source: values.NotNullLong, target: values.NotNullInt, want: true},
		{name: "INT to DOUBLE is exact", source: values.NotNullInt, target: values.NotNullDouble, want: true},
		{name: "INT to FLOAT rounds", source: values.NotNullInt, target: values.NotNullFloat},
		{name: "LONG to DOUBLE rounds", source: values.NotNullLong, target: values.NotNullDouble},
		{name: "FLOAT source has signed-zero aliases", source: values.NotNullFloat, target: values.NotNullFloat},
		{name: "DOUBLE source has signed-zero and NaN aliases", source: values.NotNullDouble, target: values.NotNullLong},
		{name: "STRING identity", source: values.NotNullString, target: values.NullableString, want: true},
		{name: "cross-domain nonnumeric declines", source: values.NotNullString, target: values.NotNullLong},
		{name: "Unknown source declines", source: values.UnknownType, target: values.NotNullLong},
		{name: "Unknown target declines", source: values.NotNullLong, target: values.UnknownType},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := LogicalEqualityProjectionInjective(test.source, test.target); got != test.want {
				t.Fatalf("LogicalEqualityProjectionInjective = %v, want %v", got, test.want)
			}
		})
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

// TestPhysicalOrderingPrefixLength_FloatRelaxationIsTerminal pins the COUNT,
// not merely the non-emptiness, of the ordering prefix across a float
// coordinate — which is the dimension a len(Keys) != 0 assertion cannot see.
//
// A float coordinate that is congruent only through the NaN tie class orders
// ITSELF and scrambles everything after it: all NaN payloads are one logical
// value spread over many distinct physical keys, so within that tie the next
// coordinate is emitted in payload order rather than its own. Claiming the
// second column here returns rows in the wrong order for
// ORDER BY v, w — a silent wrong answer, not a missed optimisation.
//
// The two-sided arm is the control: a finite interval contains no NaN, so
// there is no tie class and the prefix legitimately continues.
func TestPhysicalOrderingPrefixLength_FloatRelaxationIsTerminal(t *testing.T) {
	t.Parallel()
	floatThenLong := []values.Type{values.NullableDouble, values.NullableLong}
	oneSided := func(t *testing.T) *predicates.ComparisonRange {
		t.Helper()
		comparison := predicates.NewLiteralComparison(predicates.ComparisonLessThan, float64(0))
		merged := predicates.EmptyComparisonRange().Merge(&comparison)
		if !merged.Ok {
			t.Fatal("failed to build a one-sided float inequality")
		}
		return merged.Range
	}
	twoSided := func(t *testing.T) *predicates.ComparisonRange {
		t.Helper()
		low := predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, float64(-8))
		merged := predicates.EmptyComparisonRange().Merge(&low)
		if !merged.Ok {
			t.Fatal("failed to build the lower bound")
		}
		high := predicates.NewLiteralComparison(predicates.ComparisonLessThan, float64(8))
		merged = merged.Range.Merge(&high)
		if !merged.Ok {
			t.Fatal("failed to build the upper bound")
		}
		return merged.Range
	}

	for _, test := range []struct {
		name  string
		comps func(*testing.T) *predicates.ComparisonRange
		want  int
		why   string
	}{
		{
			name:  "one-sided inequality stops after the float",
			comps: oneSided,
			want:  1,
			why:   "the negative-NaN block is a tie class; the next column is in payload order inside it",
		},
		{
			name:  "two-sided finite range continues past the float",
			comps: twoSided,
			want:  2,
			why:   "a finite interval holds no NaN, so there is no tie class to scramble the next column",
		},
		{
			name:  "unbound float stops after the float, and only for a range-set leaf",
			comps: nil,
			want:  0,
			why:   "only a range-set leaf splits the blocks at all, and even then the claim ends there",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var comps []*predicates.ComparisonRange
			if test.comps != nil {
				comps = []*predicates.ComparisonRange{test.comps(t)}
			}
			if got := PhysicalOrderingPrefixLength(comps, floatThenLong, 2); got != test.want {
				t.Fatalf("PhysicalOrderingPrefixLength = %d, want %d — %s", got, test.want, test.why)
			}
		})
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
		// An UNBOUND float coordinate is congruent because the scan splits it
		// into NULL, the numbers, and the two NaN blocks and returns them in
		// logical order. Before that split it was NOT congruent, and the cost
		// of saying so was a blocking in-memory sort over the whole table.
		{name: "unbound DOUBLE stops for a leaf that cannot split blocks", types: []values.Type{values.NullableDouble, values.NullableLong}, want: 0},
		// An ordered FLOAT coordinate is congruent for ITSELF and TERMINAL: the
		// scan emits its ranges in logical order, but the NaN block it appends
		// is a tie class of many distinct physical keys, so the coordinate
		// AFTER it comes back in NaN-payload order. Hence 1, not 2.
		//
		// The LONG control is the contrast that keeps this honest: an ordered
		// integer coordinate has no tie class, so its prefix genuinely runs to
		// the end. If these two ever agree again, the terminality rule has been
		// lost.
		{name: "ordered FLOAT is congruent but terminal", comps: []*predicates.ComparisonRange{inequality(float64(0))}, types: []values.Type{values.NullableFloat, values.NullableLong}, want: 1},
		{name: "ordered LONG control runs to the end", comps: []*predicates.ComparisonRange{inequality(int64(0))}, types: []values.Type{values.NullableLong, values.NullableLong}, want: 2},
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
