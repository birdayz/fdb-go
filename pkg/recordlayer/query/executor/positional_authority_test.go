package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestPositionalAuthority_ScalarElement is the MANDATORY
// positional-authority pin: the no-AT mixed seed references the bare-scalar
// unnest element as a DIRECT QuantifiedObjectValue (Java's isPrimitive()
// branch). The ordinal-build binder must RAW-bind that leg
// (ordinalJoinBuild.RawLegs) — never route it through adaptLegPositional,
// which would synthesize an EMPTY positional row for a non-record row and
// build the element NULL. This pin asserts the element's POSITIONAL SLOT
// VALUE directly. Without the raw-bind it is an empty *PositionalRow, not the
// scalar; the pin then fails.
func TestPositionalAuthority_ScalarElement(t *testing.T) {
	// The mixed seed: a baked outer leg run (ofOrdinal over a record QOV) + the
	// element as a DIRECT bare QOV over a SCALAR type.
	outerType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ARR", FieldType: values.NotNullLong, Ordinal: 1},
	})
	outerCorr := values.NamedCorrelationIdentifier("T")
	innerCorr := values.NamedCorrelationIdentifier("X")
	outerQOV := mustTestQOV(t, outerCorr, outerType)
	o0 := mustExecutorConstruct(values.ResolveOrdinalSeedField(outerQOV, 0))
	o1 := mustExecutorConstruct(values.ResolveOrdinalSeedField(outerQOV, 1))
	elementQOV := mustTestQOV(t, innerCorr, values.NotNullLong)
	mixed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: o0},
		values.RecordConstructorField{Name: "ARR", Value: o1},
		values.RecordConstructorField{Name: "X", Value: elementQOV},
	)

	c, err := newFlatMapCursorForPlan(
		recordlayer.FromList([]QueryResult{}), nil, nil, nil, EmptyEvaluationContext(),
		outerCorr, innerCorr, mixed, recordlayer.ExecuteProperties{}, false,
		false,
	)
	if err != nil {
		t.Fatalf("newFlatMapCursorForPlan: %v", err)
	}
	defer c.Close()
	if !c.build.enabled() {
		t.Fatal("the mixed seed (baked outer) must enable ordinal build")
	}
	if _, raw := c.build.RawLegs[innerCorr]; !raw {
		t.Fatalf("the bare-scalar element leg %s must be a RawLeg (bind its whole row raw)", innerCorr)
	}

	outerPos := NewPositionalRow(outerType)
	outerPos.Set(0, int64(7))
	outerPos.Set(1, int64(0))
	outerRow := QueryResult{Positional: outerPos}
	innerRow := dscalar(int64(101)) // the bare-scalar unnest element

	got, err := c.computeResult(outerRow, innerRow)
	if err != nil {
		t.Fatalf("computeResult: %v", err)
	}
	if got.Positional == nil || len(got.Positional.Slots) != 3 {
		t.Fatalf("built positional row = %v, want 3 slots [ID ARR element]", got.Positional)
	}
	// Slot 2 (the element) MUST be the scalar 101 — NOT an empty/all-nil
	// positional row (the raw-bind is what prevents that).
	if got.Positional.Slots[2] != int64(101) {
		t.Fatalf("element positional slot = %#v (%T), want int64(101) — the raw-bind regressed (an empty OrdinalRow is the bug it guards against)", got.Positional.Slots[2], got.Positional.Slots[2])
	}
	// Outer slots stay correct (baked ofOrdinal over the adapted outer leg).
	if got.Positional.Slots[0] != int64(7) {
		t.Errorf("outer ID slot = %#v, want int64(7)", got.Positional.Slots[0])
	}
	// The element resolves by its output name X against the built row.
	if m, ok := rowMapOK(got); !ok || m["X"] != int64(101) {
		t.Errorf("element X by name = %#v, want int64(101)", got.Positional)
	}
}

// TestFlatMapScalarOuterBindsTheDatumAndShadowsEnclosingExactValue pins the
// non-build execution path for a bare scalar OUTER. Go transports an Explode
// element in a one-slot PositionalRow, but the quantifier object is the scalar
// datum itself. The local FlatMap binding must also replace an enclosing exact
// binding for the same alias/type: Java's nearest correlation wins.
func TestFlatMapScalarOuterBindsTheDatumAndShadowsEnclosingExactValue(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("SCALAR_OUTER")
	innerAlias := values.NamedCorrelationIdentifier("SCALAR_INNER")
	outer := mustExecutorConstruct(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
		Value: []any{int64(42)},
		Typ:   values.NewArrayType(false, values.NotNullLong),
	}))
	outerObject := mustTestQOV(t, outerAlias, values.NotNullLong)
	inner := mustExecutorConstruct(plans.NewRecordQueryValuesPlan([]values.Value{outerObject}))
	innerObject := mustTestQOV(t, innerAlias, inner.GetResultType())
	plan := mustExecutorConstruct(plans.NewRecordQueryFlatMapPlan(
		outer, inner, outerAlias, innerAlias, innerObject, false,
	))

	// If computeResult/inner execution consults the enclosing exact map instead
	// of installing the nearer FlatMap leg, the emitted value is 999. If the
	// scalar transport is not unwrapped, it is a *PositionalRow instead of 42.
	evalCtx, err := EmptyEvaluationContext().withQuantifiedBinding(outerObject, int64(999), false)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := ExecutePlan(
		context.Background(), plan, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cursor.Close() }()
	result, err := cursor.OnNext(context.Background())
	if err != nil || !result.HasNext() {
		t.Fatalf("FlatMap scalar row = (%v, %v), want one row", result, err)
	}
	row := result.GetValue().Positional
	if row == nil || len(row.Slots) != 1 || row.Slots[0] != int64(42) {
		t.Fatalf("FlatMap scalar row = %#v, want one slot containing local datum 42", row)
	}
	end, err := cursor.OnNext(context.Background())
	if err != nil || end.HasNext() {
		t.Fatalf("FlatMap scalar tail = (%v, %v), want exhausted", end, err)
	}
}

func TestExactPlanObjectBindingRejectsNullForNonNullableScalar(t *testing.T) {
	t.Parallel()

	object := mustTestQOV(t, values.NamedCorrelationIdentifier("SCALAR"), values.NotNullLong)
	if got, absent, err := exactPlanObjectBinding(
		object, values.OrdinalCarrierScalar,
		scalarPositionalRowOfType(nil, values.NotNullLong), false,
	); got != nil || absent || err == nil {
		t.Fatalf("non-nullable scalar binding = (%v, %t, %v), want loud nullability rejection", got, absent, err)
	}
	if got, absent, err := exactPlanObjectBinding(
		object, values.OrdinalCarrierScalar,
		scalarPositionalRowOfType(nil, values.NotNullLong), true,
	); got != nil || !absent || err != nil {
		t.Fatalf("explicit missing scalar binding = (%v, %t, %v), want (nil, true, nil)", got, absent, err)
	}
}

func TestExactPlanObjectBindingRejectsWrongRecordTransport(t *testing.T) {
	t.Parallel()

	declared := exactTestRowType(values.Field{Name: "ID", FieldType: values.NotNullLong})
	// WRONG means a different row SHAPE. A RecordName is provenance and
	// compares equal (Java's Type.Record.equals), so "FOREIGN" over the
	// declared fields IS the declared row and there would be nothing to reject.
	wrong := values.NewRecordType("FOREIGN", false, []values.Field{{
		Name: "FOREIGN_ID", Ordinal: 0, FieldType: values.NotNullLong,
	}})
	object := mustTestQOV(t, values.NamedCorrelationIdentifier("ROW"), declared)
	if got, absent, err := exactPlanObjectBinding(
		object, values.OrdinalCarrierRecord,
		&PositionalRow{Type: wrong, Slots: []any{int64(1)}}, false,
	); got != nil || absent || err == nil {
		t.Fatalf("wrong record binding = (%v, %t, %v), want loud exact-type rejection", got, absent, err)
	}
}

// TestOrdinalAliasCollision pins that a leg whose own columns are named "_1"/"_0"
// binds BY NAME through adaptLegPositional: column "_1" reads the row's "_1"
// value, never slot 1. (A WITH ORDINALITY Explode names its slots after the
// unnest's AS/AT aliases, so its rows bind by those names the same way;
// TestFDB_ArrayUnnestOrdinality pins `AS "_1" AT "_0"` end to end.)
func TestOrdinalAliasCollision(t *testing.T) {
	datum := map[string]any{
		values.OrdinalFieldName(0): int64(101),
		values.OrdinalFieldName(1): int64(1),
	}
	nameLegType := values.NewRecordType("", true, []values.Field{
		{Name: "_1", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "_0", FieldType: values.NotNullLong, Ordinal: 1},
	})
	nameRow, err := adaptLegPositional(dmap(datum), nameLegType, values.CorrelationIdentifier{})
	if err != nil {
		t.Fatalf("adaptLegPositional (name-model _1/_0 columns): %v", err)
	}
	if got, _ := nameRow.Get(0); got != int64(1) {
		t.Fatalf("name-model column \"_1\" = %#v, want row[\"_1\"]=1 (NAME binding, not positional)", got)
	}
	if got, _ := nameRow.Get(1); got != int64(101) {
		t.Fatalf("name-model column \"_0\" = %#v, want row[\"_0\"]=101", got)
	}
}

// TestPositionalAuthority_NullElement pins the NULL element (an array
// containing a NULL, or the null-leg): a nil row raw-binds to nil, so the
// element slot is NULL — not an empty row, not a panic.
func TestPositionalAuthority_NullElement(t *testing.T) {
	outerType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	outerCorr := values.NamedCorrelationIdentifier("T")
	innerCorr := values.NamedCorrelationIdentifier("X")
	outerQOV := mustTestQOV(t, outerCorr, outerType)
	o0 := mustExecutorConstruct(values.ResolveOrdinalSeedField(outerQOV, 0))
	mixed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: o0},
		values.RecordConstructorField{Name: "X", Value: mustTestQOV(t, innerCorr, values.NotNullLong)},
	)
	c, err := newFlatMapCursorForPlan(
		recordlayer.FromList([]QueryResult{}), nil, nil, nil, EmptyEvaluationContext(),
		outerCorr, innerCorr, mixed, recordlayer.ExecuteProperties{}, false,
		false,
	)
	if err != nil {
		t.Fatalf("newFlatMapCursorForPlan: %v", err)
	}
	defer c.Close()

	outerPos := NewPositionalRow(outerType)
	outerPos.Set(0, int64(1))
	outerRow := QueryResult{Positional: outerPos}
	got, err := c.computeResult(outerRow, QueryResult{})
	if err != nil {
		t.Fatalf("computeResult: %v", err)
	}
	if got.Positional.Slots[1] != nil {
		t.Fatalf("null element slot = %#v, want nil (NULL)", got.Positional.Slots[1])
	}
}
