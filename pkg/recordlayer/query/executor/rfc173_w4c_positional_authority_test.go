package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestRFC173W4c_PositionalAuthority_ScalarElement is the MANDATORY
// positional-authority pin (RFC-173 W4c design condition 4): the no-AT mixed
// seed references the bare-scalar unnest element as a DIRECT
// QuantifiedObjectValue (Java's isPrimitive() branch). The ordinal-birth binder
// must RAW-bind that leg (ordinalJoinBirth.RawLegs) — never route it through
// adaptLegPositional, which would synthesize an EMPTY positional row for a
// non-record Datum and birth the element NULL. The coexistence Datum MASKS that
// (it carries the raw scalar), so this pin asserts the element's POSITIONAL SLOT
// VALUE directly — the S4-surviving authority. Without the raw-bind it is an
// empty *PositionalRow, not the scalar; the pin then fails.
func TestRFC173W4c_PositionalAuthority_ScalarElement(t *testing.T) {
	// The mixed seed: a baked outer leg run (ofOrdinal over a record QOV) + the
	// element as a DIRECT bare QOV over a SCALAR type.
	outerType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "ARR", FieldType: values.NotNullLong, Ordinal: 1},
	})
	outerCorr := values.NamedCorrelationIdentifier("T")
	innerCorr := values.NamedCorrelationIdentifier("X")
	outerQOV := values.NewQuantifiedObjectValueOfType(outerCorr, outerType)
	o0, err := values.NewFieldValueOfOrdinal(outerQOV, 0)
	if err != nil {
		t.Fatalf("bake o0: %v", err)
	}
	o1, err := values.NewFieldValueOfOrdinal(outerQOV, 1)
	if err != nil {
		t.Fatalf("bake o1: %v", err)
	}
	elementQOV := values.NewQuantifiedObjectValueOfType(innerCorr, values.NotNullLong)
	mixed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: o0},
		values.RecordConstructorField{Name: "ARR", Value: o1},
		values.RecordConstructorField{Name: "X", Value: elementQOV},
	)

	c, err := newFlatMapCursor(
		recordlayer.FromList([]QueryResult{}), nil, nil, nil, EmptyEvaluationContext(),
		outerCorr, innerCorr, mixed, false, recordlayer.ExecuteProperties{},
	)
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	defer c.Close()
	if !c.birth.enabled() {
		t.Fatal("the mixed seed (baked outer) must enable ordinal birth")
	}
	if _, raw := c.birth.RawLegs[innerCorr]; !raw {
		t.Fatalf("the bare-scalar element leg %s must be a RawLeg (bind its whole Datum raw)", innerCorr)
	}

	outerPos := NewPositionalRow(outerType)
	outerPos.Set(0, int64(7))
	outerPos.Set(1, int64(0))
	outerRow := QueryResult{Datum: map[string]any{"ID": int64(7), "ARR": int64(0)}, Positional: outerPos}
	innerRow := QueryResult{Datum: int64(101)} // the bare-scalar unnest element

	got, err := c.computeResult(outerRow, innerRow)
	if err != nil {
		t.Fatalf("computeResult: %v", err)
	}
	if got.Positional == nil || len(got.Positional.Slots) != 3 {
		t.Fatalf("birthed positional row = %v, want 3 slots [ID ARR element]", got.Positional)
	}
	// The S4-authority: slot 2 (the element) MUST be the scalar 101 — NOT an
	// empty/all-nil positional row (the pre-raw-bind bug the spike caught).
	if got.Positional.Slots[2] != int64(101) {
		t.Fatalf("element positional slot = %#v (%T), want int64(101) — the raw-bind regressed (an empty OrdinalRow is the pre-fix bug)", got.Positional.Slots[2], got.Positional.Slots[2])
	}
	// Outer slots stay correct (baked ofOrdinal over the adapted outer leg).
	if got.Positional.Slots[0] != int64(7) {
		t.Errorf("outer ID slot = %#v, want int64(7)", got.Positional.Slots[0])
	}
	// The coexistence Datum carries the same element (dual-emission invariant).
	if m, ok := got.Datum.(map[string]any); !ok || m["X"] != int64(101) {
		t.Errorf("coexistence Datum X = %#v, want int64(101)", got.Datum)
	}
}

// TestRFC173W4c_OrdinalAliasCollision pins the ordinal-alias-collision handling
// via PRODUCER CONTEXT: a WITH-ORDINALITY Explode leg (marked in OrdinalityLegs
// by newFlatMapCursor) binds STRICTLY POSITIONALLY (slot i = Datum[_i]) — so a
// user AS/AT alias that SPELLS an internal OrdinalFieldName (`FROM t, t.arr AS
// "_1" AT "_0"`) can't route the wrong internal key — while a SHAPE-IDENTICAL
// name-model leg whose OWN columns are aliased "_0"/"_1" (NOT an ordinality
// Explode) binds by NAME through adaptLegPositional. The two are indistinguishable
// by Datum shape; only the producer signal disambiguates them.
func TestRFC173W4c_OrdinalAliasCollision(t *testing.T) {
	innerCorr := values.NamedCorrelationIdentifier("X")
	// The internal-keyed ordinality Datum: _0 = element, _1 = 1-based ordinal.
	datum := map[string]any{
		values.OrdinalFieldName(0): int64(101),
		values.OrdinalFieldName(1): int64(1),
	}

	// bindOrdinality binds a WITH-ORDINALITY leg (marked OrdinalityLegs) whose
	// type is named by the given AS/AT aliases, over the internal-keyed Datum.
	bindOrdinality := func(t *testing.T, asName, atName string) values.OrdinalRow {
		t.Helper()
		legType := values.NewRecordType("", true, []values.Field{
			{Name: asName, FieldType: values.NotNullLong, Ordinal: 0},
			{Name: atName, FieldType: values.NotNullInt, Ordinal: 1},
		})
		b := &ordinalJoinBirth{
			OrdinalityLegs: map[values.CorrelationIdentifier]struct{}{innerCorr: {}},
			LegTypes:       map[values.CorrelationIdentifier]*values.RecordType{innerCorr: legType},
		}
		legs := map[values.CorrelationIdentifier]values.OrdinalRow{}
		raw := map[values.CorrelationIdentifier]any{}
		if err := b.bindLeg(legs, raw, innerCorr.Name(), &QueryResult{Datum: datum}); err != nil {
			t.Fatalf("bindLeg: %v", err)
		}
		return legs[innerCorr]
	}

	// AS "_1" collides with the internal ordinal key; the element (slot 0) must
	// still be m["_0"], the ordinal (slot 1) m["_1"].
	row := bindOrdinality(t, "_1", "O")
	if got, _ := row.Get(0); got != int64(101) {
		t.Fatalf("AS \"_1\" element slot = %#v, want 101 — a user alias spelling _1 must not read the internal ordinal key", got)
	}
	if got, _ := row.Get(1); got != int64(1) {
		t.Fatalf("AT O ordinal slot = %#v, want 1", got)
	}

	// The FULLY-colliding case — BOTH aliases spell ordinal keys, swapped:
	// `AS "_1" AT "_0"`. No shape discriminator can handle this (leg type {_1,_0}
	// over Datum {_0,_1} is identical to the name-model union case below);
	// producer-context positional binding still puts element at slot 0, ordinal 1.
	rowBoth := bindOrdinality(t, "_1", "_0")
	if got, _ := rowBoth.Get(0); got != int64(101) {
		t.Fatalf("AS \"_1\" AT \"_0\" element slot = %#v, want 101", got)
	}
	if got, _ := rowBoth.Get(1); got != int64(1) {
		t.Fatalf("AS \"_1\" AT \"_0\" ordinal slot = %#v, want 1", got)
	}

	// The producer-context DISTINCTION: a NAME-MODEL leg (NOT an ordinality
	// Explode — no OrdinalityLegs mark) whose own columns are aliased "_1"/"_0"
	// is SHAPE-IDENTICAL to rowBoth but must bind BY NAME via adaptLegPositional:
	// column "_1" reads m["_1"]=1, column "_0" reads m["_0"]=101 (NOT swapped to
	// positional). This is the case a Datum-shape discriminator would break.
	nameLegType := values.NewRecordType("", true, []values.Field{
		{Name: "_1", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "_0", FieldType: values.NotNullLong, Ordinal: 1},
	})
	nameRow, err := adaptLegPositional(QueryResult{Datum: datum}, nameLegType)
	if err != nil {
		t.Fatalf("adaptLegPositional (name-model _1/_0 columns): %v", err)
	}
	if got, _ := nameRow.Get(0); got != int64(1) {
		t.Fatalf("name-model column \"_1\" = %#v, want m[\"_1\"]=1 (NAME binding, not positional)", got)
	}
	if got, _ := nameRow.Get(1); got != int64(101) {
		t.Fatalf("name-model column \"_0\" = %#v, want m[\"_0\"]=101", got)
	}
}

// TestRFC173W4c_PositionalAuthority_NullElement pins the NULL element (an array
// containing a NULL, or the null-leg): a nil Datum raw-binds to nil, so the
// element slot is NULL — not an empty row, not a panic.
func TestRFC173W4c_PositionalAuthority_NullElement(t *testing.T) {
	outerType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	outerCorr := values.NamedCorrelationIdentifier("T")
	innerCorr := values.NamedCorrelationIdentifier("X")
	outerQOV := values.NewQuantifiedObjectValueOfType(outerCorr, outerType)
	o0, err := values.NewFieldValueOfOrdinal(outerQOV, 0)
	if err != nil {
		t.Fatalf("bake o0: %v", err)
	}
	mixed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: o0},
		values.RecordConstructorField{Name: "X", Value: values.NewQuantifiedObjectValueOfType(innerCorr, values.NotNullLong)},
	)
	c, err := newFlatMapCursor(
		recordlayer.FromList([]QueryResult{}), nil, nil, nil, EmptyEvaluationContext(),
		outerCorr, innerCorr, mixed, false, recordlayer.ExecuteProperties{},
	)
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	defer c.Close()

	outerPos := NewPositionalRow(outerType)
	outerPos.Set(0, int64(1))
	outerRow := QueryResult{Datum: map[string]any{"ID": int64(1)}, Positional: outerPos}
	got, err := c.computeResult(outerRow, QueryResult{Datum: nil})
	if err != nil {
		t.Fatalf("computeResult: %v", err)
	}
	if got.Positional.Slots[1] != nil {
		t.Fatalf("null element slot = %#v, want nil (NULL)", got.Positional.Slots[1])
	}
}

// TestRFC173W4c_OracleOrdinality pins the §5 NAME-MODEL ORACLE (dualwindow dual)
// for a WITH-ORDINALITY unnest: the ordinal seed's baked element/ordinal fields
// are named by the AS/AT aliases (V/O), but the oracle reads them BY NAME against
// the Explode's internal `_0`/`_1` Datum. The oracle path must rebind the
// ordinality inner under the AS/AT names (producer context) so the reads resolve
// — matching the pre-W4c name model. Without the rebind, V/O read missing and the
// oracle returned wrong rows (a latent dualwindow gap). NOT parallel: it flips the
// global oracle (Go runs non-parallel tests before parallel ones — isolated).
func TestRFC173W4c_OracleOrdinality(t *testing.T) {
	outerType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	outerCorr := values.NamedCorrelationIdentifier("T")
	innerCorr := values.NamedCorrelationIdentifier("X")
	outerQOV := values.NewQuantifiedObjectValueOfType(outerCorr, outerType)
	o0, err := values.NewFieldValueOfOrdinal(outerQOV, 0)
	if err != nil {
		t.Fatalf("bake ID: %v", err)
	}
	// The WITH-ORDINALITY inner leg type is named by the AS/AT aliases.
	innerType := values.NewRecordType("", true, []values.Field{
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "O", FieldType: values.NotNullInt, Ordinal: 1},
	})
	innerQOV := values.NewQuantifiedObjectValueOfType(innerCorr, innerType)
	elemFV, err := values.NewFieldValueOfOrdinal(innerQOV, 0) // Field = "V"
	if err != nil {
		t.Fatalf("bake V: %v", err)
	}
	ordFV, err := values.NewFieldValueOfOrdinal(innerQOV, 1) // Field = "O"
	if err != nil {
		t.Fatalf("bake O: %v", err)
	}
	seed := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: o0},
		values.RecordConstructorField{Name: "V", Value: elemFV},
		values.RecordConstructorField{Name: "O", Value: ordFV},
	)
	c, err := newFlatMapCursor(
		recordlayer.FromList([]QueryResult{}), nil, nil, nil, EmptyEvaluationContext(),
		outerCorr, innerCorr, seed, false, recordlayer.ExecuteProperties{},
	)
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	defer c.Close()
	// Production newFlatMapCursor marks this from the inner Explode plan; here the
	// inner plan is nil, so mark the ordinality leg directly.
	if c.birth.OrdinalityLegs == nil {
		c.birth.OrdinalityLegs = map[values.CorrelationIdentifier]struct{}{}
	}
	c.birth.OrdinalityLegs[innerCorr] = struct{}{}

	SetNameModelOracle(true)
	defer SetNameModelOracle(false)

	outerRow := QueryResult{Datum: map[string]any{"ID": int64(7)}}
	innerRow := QueryResult{Datum: map[string]any{
		values.OrdinalFieldName(0): int64(101), // element
		values.OrdinalFieldName(1): int64(1),   // 1-based ordinal
	}}
	got, err := c.computeResult(outerRow, innerRow)
	if err != nil {
		t.Fatalf("computeResult (oracle): %v", err)
	}
	m, ok := got.Datum.(map[string]any)
	if !ok {
		t.Fatalf("oracle Datum = %T, want map", got.Datum)
	}
	if m["V"] != int64(101) {
		t.Fatalf("oracle element V = %#v, want 101 — the oracle must rebind the ordinality inner under the AS/AT names (else V reads missing against the _0/_1 Explode Datum)", m["V"])
	}
	if m["O"] != int64(1) {
		t.Fatalf("oracle ordinal O = %#v, want 1", m["O"])
	}
}
