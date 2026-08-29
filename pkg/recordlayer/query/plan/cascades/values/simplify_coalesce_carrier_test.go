package values

import "testing"

// coalesceCarrierColumn returns a BAKED FieldValue reading a single
// LONG-carried column off a one-column row, plus the row that binds it. A
// baked reference is the shape that resolves against an OrdinalRow, and a LONG
// carrier is what makes the declared-type conversion observable: a
// DOUBLE-declared COALESCE over it must hand back a float64.
func coalesceCarrierColumn(t *testing.T) (Value, OrdinalRow) {
	t.Helper()
	rowType := NewRecordType("CoalesceCarrierRow", false, []Field{
		{Name: "N", FieldType: NullableLong, Ordinal: 0},
	})
	root, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("coalesce_carrier"), rowType)
	if err != nil {
		t.Fatalf("construct QOV: %v", err)
	}
	column, err := ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("resolve column: %v", err)
	}
	return column, &fakeOrdinalRow{names: []string{"N"}, slots: []any{int64(3)}}
}

// TestSimplifyCoalesce_DegenerateExitKeepsDeclaredCarrier pins the exact shape
// that broke: `COALESCE(<long column>, <typed NULL>)`, whose declared result
// type is the maximum of its arguments and so carries a FLOATING code, reduced
// by the redundant-null removal to its single surviving argument.
//
// The survivor is a RUNTIME value, which is what makes this exit different from
// the winning-constant exit beside it — the conversion cannot be arithmetic on
// a literal, it has to be a node. Before the fix the exit returned the bare
// column and the DOUBLE-declared expression evaluated int64.
//
// Both floating codes are driven, not just DOUBLE: coerceNumericResult treats
// FLOAT differently (it rounds through float32), so a fix that restored only
// the DOUBLE arm would pass a DOUBLE-only test with the FLOAT arm still wrong.
func TestSimplifyCoalesce_DegenerateExitKeepsDeclaredCarrier(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		declared Type
	}{
		{name: "double", declared: NullableDouble},
		{name: "float", declared: NullableFloat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			column, row := coalesceCarrierColumn(t)
			coalesce := NewScalarFunctionValue("COALESCE", tc.declared,
				column, &NullValue{Typ: tc.declared})

			want, err := coalesce.Evaluate(row)
			if err != nil {
				t.Fatalf("evaluate un-simplified: %v", err)
			}
			if _, isFloat := want.(float64); !isFloat {
				t.Fatalf("fixture is inert: a %v-declared COALESCE evaluated %#v (%T), "+
					"so there is no carrier for the simplifier to drop", tc.declared, want, want)
			}

			simplified := SimplifyValue(coalesce)
			if _, stillCoalesce := simplified.(*ScalarFunctionValue); stillCoalesce {
				t.Fatalf("fixture is inert: the redundant-null removal did not fire, "+
					"so the node-removing exit under test was never reached (got %s)",
					ExplainValue(simplified))
			}

			got, err := simplified.Evaluate(row)
			if err != nil {
				t.Fatalf("evaluate simplified %s: %v", ExplainValue(simplified), err)
			}
			if got != want {
				t.Fatalf("simplification changed the answer: want %#v (%T), got %#v (%T) from %s",
					want, want, got, got, ExplainValue(simplified))
			}
		})
	}
}

// TestSimplifyCoalesce_WinningConstantKeepsDeclaredCarrier is the sibling exit,
// pinned so the two cannot drift apart again. This one was already correct; it
// is here because a later edit that unifies the exits must keep BOTH answers,
// and because a regression here is otherwise invisible (the constant path folds
// silently).
func TestSimplifyCoalesce_WinningConstantKeepsDeclaredCarrier(t *testing.T) {
	t.Parallel()
	column, row := coalesceCarrierColumn(t)
	// NULL, then a non-null integral constant, then a runtime value: the
	// constant wins while only constants have been seen.
	coalesce := NewScalarFunctionValue("COALESCE", NullableDouble,
		&NullValue{Typ: NullableDouble},
		&ConstantValue{Value: int64(5), Typ: NullableLong},
		column)

	want, err := coalesce.Evaluate(row)
	if err != nil {
		t.Fatalf("evaluate un-simplified: %v", err)
	}
	if want != float64(5) {
		t.Fatalf("fixture is inert: expected the DOUBLE carrier to show, got %#v (%T)", want, want)
	}

	simplified := SimplifyValue(coalesce)
	got, err := simplified.Evaluate(row)
	if err != nil {
		t.Fatalf("evaluate simplified %s: %v", ExplainValue(simplified), err)
	}
	if got != want {
		t.Fatalf("simplification changed the answer: want %#v (%T), got %#v (%T) from %s",
			want, want, got, got, ExplainValue(simplified))
	}
}

// TestSimplifyCoalesce_NonConvertingTypeIsLeftBare pins the other direction of
// carrierConvertingType: where coerceNumericResult is the identity, the exit
// must hand back the survivor UNWRAPPED. A Promote added there would be a
// no-op at runtime but a real change to the tree — Explain output, memo
// identity and every rule matching on the node's shape — bought for nothing.
func TestSimplifyCoalesce_NonConvertingTypeIsLeftBare(t *testing.T) {
	t.Parallel()
	column, _ := coalesceCarrierColumn(t)
	coalesce := NewScalarFunctionValue("COALESCE", NullableLong,
		column, &NullValue{Typ: NullableLong})

	simplified := SimplifyValue(coalesce)
	if _, wrapped := simplified.(*PromoteValue); wrapped {
		t.Fatalf("LONG-declared COALESCE wrapped its survivor in a Promote: %s", ExplainValue(simplified))
	}
	if simplified != column {
		t.Fatalf("expected the bare survivor, got %s", ExplainValue(simplified))
	}
}

// TestCarrierConvertingTypeMatchesCoerceNumericResult drives EVERY TypeCode
// through both carrierConvertingType and coerceNumericResult and fails if they
// disagree. carrierConvertingType is a claim about coerceNumericResult's switch;
// without this the claim is a copy of that switch that nothing keeps in step,
// and the failure mode is silent — a new converting code would be left
// un-preserved by the COALESCE exits with every existing test still green.
//
// The code list is checked for COMPLETENESS first, against the contiguous
// TypeCode range, so adding a code to the enum fails here rather than passing
// on a population that quietly stopped covering it.
func TestCarrierConvertingTypeMatchesCoerceNumericResult(t *testing.T) {
	t.Parallel()
	byCode := map[TypeCode]Type{
		TypeCodeUnknown:   UnknownType,
		TypeCodeNull:      NullType,
		TypeCodeBoolean:   NullableBoolean,
		TypeCodeInt:       NullableInt,
		TypeCodeLong:      NullableLong,
		TypeCodeFloat:     NullableFloat,
		TypeCodeDouble:    NullableDouble,
		TypeCodeString:    NullableString,
		TypeCodeBytes:     NullableBytes,
		TypeCodeVersion:   NullableVersion,
		TypeCodeEnum:      &EnumType{EnumName: "E", Nullable: true},
		TypeCodeRecord:    NewRecordType("R", true, []Field{{Name: "F", FieldType: NullableLong, Ordinal: 0}}),
		TypeCodeArray:     &ArrayType{Nullable: true, ElementType: NullableLong},
		TypeCodeRelation:  &RelationType{InnerType: NullableLong},
		TypeCodeNone:      NoneType,
		TypeCodeAny:       AnyType,
		TypeCodeUuid:      NullableUuid,
		TypeCodeDate:      NullableDate,
		TypeCodeTimestamp: NullableTimestamp,
	}

	// Completeness over the contiguous enum range. TypeCodeTimestamp is the
	// last declared code; a code added after it lands outside this range and
	// is caught by the count check below instead.
	for code := TypeCodeUnknown; code <= TypeCodeTimestamp; code++ {
		typ, ok := byCode[code]
		if !ok {
			t.Fatalf("TypeCode %v (%d) has no entry — this test's population stopped covering the enum", code, int(code))
		}
		if typ.Code() != code {
			t.Fatalf("TypeCode %v maps to a type whose Code() is %v", code, typ.Code())
		}
	}
	if len(byCode) != int(TypeCodeTimestamp)+1 {
		t.Fatalf("population is %d entries for %d codes — a code was added to the enum without an entry here",
			len(byCode), int(TypeCodeTimestamp)+1)
	}

	// int64 is the probe because it is the carrier the two converting codes
	// actually move: DOUBLE widens it and FLOAT rounds it through float32.
	const probe = int64(3)
	converting := 0
	for code := TypeCodeUnknown; code <= TypeCodeTimestamp; code++ {
		typ := byCode[code]
		coerced := coerceNumericResult(probe, typ)
		changed := coerced != any(probe)
		if got := carrierConvertingType(typ); got != changed {
			t.Fatalf("TypeCode %v: carrierConvertingType=%v but coerceNumericResult(%v) returned %#v (%T) — changed=%v",
				code, got, probe, coerced, coerced, changed)
		}
		if changed {
			converting++
		}
	}
	// A non-vacuity floor AND a ceiling: zero would mean the probe never
	// converts and every arm agreed trivially; more than two would mean a new
	// converting code appeared, which is the event the COALESCE exits need to
	// hear about.
	if converting != 2 {
		t.Fatalf("expected exactly 2 converting codes (DOUBLE, FLOAT), saw %d", converting)
	}

	// nil is its own arm in carrierConvertingType and must answer false.
	if carrierConvertingType(nil) {
		t.Fatal("carrierConvertingType(nil) must be false")
	}
}

// TestSimplifyCoalesce_DegenerateExitKeepsDeclaredType pins the second thing a
// removed COALESCE node owes its parent: the declared TYPE.
//
// The fixture is `CAST(COALESCE(<int column>, CAST(NULL AS BIGINT)) AS BOOLEAN)`
// and it is WELL TYPED — LONG really is MaximumType(INT, LONG), so this is a
// shape a producer builds, not one only a test can construct. CastValue.Evaluate
// dispatches on `c.Child.Type()`, and Java's cast table admits INT->BOOLEAN
// while rejecting LONG->BOOLEAN, so the un-simplified expression correctly
// refuses. Reduced to the bare INT column it began answering — Go accepting a
// cast Java rejects, from a simplification pass.
//
// The assertion is on the ERROR, not on the tree shape: what matters is that
// the parent still sees LONG, and a later change that preserves the type by
// some other node than a Promote should keep passing.
func TestSimplifyCoalesce_DegenerateExitKeepsDeclaredType(t *testing.T) {
	t.Parallel()
	rowType := NewRecordType("CoalesceTypeRow", false, []Field{
		{Name: "I", FieldType: NullableInt, Ordinal: 0},
	})
	root, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("coalesce_type"), rowType)
	if err != nil {
		t.Fatalf("construct QOV: %v", err)
	}
	intColumn, err := ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("resolve column: %v", err)
	}

	declared := MaximumType(NullableInt, NullableLong)
	if declared == nil || declared.Code() != TypeCodeLong {
		t.Fatalf("fixture is inert: MaximumType(INT, LONG) = %v, want LONG — the cast under test "+
			"only refuses because the COALESCE is LONG", declared)
	}
	coalesce := NewScalarFunctionValue("COALESCE", declared,
		intColumn, &NullValue{Typ: NullableLong})
	cast := NewCastValue(coalesce, NullableBoolean)
	row := &fakeOrdinalRow{names: []string{"I"}, slots: []any{int64(5)}}

	if _, err := cast.Evaluate(row); err == nil {
		t.Fatal("fixture is inert: the un-simplified CAST(<LONG> AS BOOLEAN) did not refuse, " +
			"so there is no refusal for the simplifier to lose")
	}

	simplified := SimplifyValue(cast)
	simplifiedCast, isCast := simplified.(*CastValue)
	if !isCast {
		t.Fatalf("expected the CAST to survive simplification, got %s", ExplainValue(simplified))
	}
	if _, stillCoalesce := simplifiedCast.Child.(*ScalarFunctionValue); stillCoalesce {
		t.Fatalf("fixture is inert: the redundant-null removal did not fire, so the "+
			"node-removing exit under test was never reached (got %s)", ExplainValue(simplified))
	}
	if _, err := simplified.Evaluate(row); err == nil {
		t.Fatalf("simplification turned a refused cast into an answer: %s", ExplainValue(simplified))
	}
}

// TestSimplifyCoalesce_DeclinesWhereAPromoteWouldNotReproduceTheNode pins the
// decline, and pins it on a REACHABLE shape: MaximumType(STRING, UUID) is UUID,
// so `COALESCE(<string column>, <uuid>)` really does declare UUID over a STRING
// survivor.
//
// There, PromoteValue is not the node the COALESCE was — it parses the string
// into a neutral [16]byte where the COALESCE's own post-processing
// (coerceNumericResult) passes it through. The rule must leave the COALESCE
// standing rather than substitute a conversion it was not asked for.
func TestSimplifyCoalesce_DeclinesWhereAPromoteWouldNotReproduceTheNode(t *testing.T) {
	t.Parallel()
	if got := MaximumType(NullableString, NullableUuid); got == nil || got.Code() != TypeCodeUuid {
		t.Fatalf("fixture is inert: MaximumType(STRING, UUID) = %v, want UUID — this shape is "+
			"only worth declining because a producer can build it", got)
	}
	const uuidText = "0f0f0f0f-0f0f-0f0f-0f0f-0f0f0f0f0f0f"
	rowType := NewRecordType("CoalesceUuidRow", false, []Field{
		{Name: "S", FieldType: NullableString, Ordinal: 0},
	})
	root, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("coalesce_uuid"), rowType)
	if err != nil {
		t.Fatalf("construct QOV: %v", err)
	}
	stringColumn, err := ResolveFieldOrdinals(root, []int{0})
	if err != nil {
		t.Fatalf("resolve column: %v", err)
	}

	// A Promote here would parse; the COALESCE does not. Measure that the two
	// really disagree, so the decline is guarding something.
	promoted, err := NewPromoteValue(
		&ConstantValue{Value: uuidText, Typ: NullableString}, NullableUuid).Evaluate(nil)
	if err != nil {
		t.Fatalf("promote probe: %v", err)
	}
	if promoted == any(uuidText) {
		t.Fatal("fixture is inert: Promote(STRING -> UUID) passed the string through, " +
			"so it would have reproduced the node and there is nothing to decline")
	}

	coalesce := NewScalarFunctionValue("COALESCE", NullableUuid,
		stringColumn, &NullValue{Typ: NullableUuid})
	row := &fakeOrdinalRow{names: []string{"S"}, slots: []any{uuidText}}

	want, err := coalesce.Evaluate(row)
	if err != nil {
		t.Fatalf("evaluate un-simplified: %v", err)
	}
	simplified := SimplifyValue(coalesce)
	if simplified != Value(coalesce) {
		t.Fatalf("expected the COALESCE to stand, got %s", ExplainValue(simplified))
	}
	got, err := simplified.Evaluate(row)
	if err != nil {
		t.Fatalf("evaluate simplified: %v", err)
	}
	if got != want {
		t.Fatalf("declined simplification still changed the answer: want %#v, got %#v", want, got)
	}
}
