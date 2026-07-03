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

// TestRFC173W4c_OrdinalAliasCollision pins the ordinal-alias-collision bug: a WITH-ORDINALITY
// unnest whose AS/AT alias SPELLS an internal OrdinalFieldName (`FROM t, t.arr
// AS "_1" AT "O"`) must still bind the element to slot 0 and the ordinal to slot
// 1 — the ordinality Datum ({_0:element, _1:ordinal}) is bound STRICTLY
// POSITIONALLY, so the user alias "_1" naming slot 0 does NOT read the internal
// `_1` (the ordinal). Before the fix, adaptLegPositional's name-match consumed
// m["_1"] for the "_1"-named slot 0 and `SELECT "_1"` returned the ordinal.
func TestRFC173W4c_OrdinalAliasCollision(t *testing.T) {
	// The inner leg type is named by the user aliases: slot 0 = AS alias "_1"
	// (the element), slot 1 = AT alias "O" (the ordinal). The Explode Datum keys
	// are the INTERNAL _0/_1, which must map positionally.
	legType := values.NewRecordType("", true, []values.Field{
		{Name: "_1", FieldType: values.NotNullLong, Ordinal: 0}, // AS alias collides with internal _1
		{Name: "O", FieldType: values.NotNullInt, Ordinal: 1},
	})
	datum := map[string]any{
		values.OrdinalFieldName(0): int64(101), // element
		values.OrdinalFieldName(1): int64(1),   // 1-based ordinal
	}
	row, err := adaptLegPositional(QueryResult{Datum: datum}, legType)
	if err != nil {
		t.Fatalf("adaptLegPositional: %v", err)
	}
	if got, _ := row.Get(0); got != int64(101) {
		t.Fatalf("slot 0 (AS alias \"_1\", the element) = %#v, want int64(101) — a user alias spelling _1 must not read the internal ordinal key", got)
	}
	if got, _ := row.Get(1); got != int64(1) {
		t.Fatalf("slot 1 (AT alias O, the ordinal) = %#v, want int64(1)", got)
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
