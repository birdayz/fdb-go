package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestPositionalAuthority_ScalarElement is the MANDATORY
// positional-authority pin: the no-AT mixed seed references the bare-scalar
// unnest element as a DIRECT QuantifiedObjectValue (Java's isPrimitive()
// branch). The ordinal-birth binder must RAW-bind that leg
// (ordinalJoinBirth.RawLegs) — never route it through adaptLegPositional,
// which would synthesize an EMPTY positional row for a non-record row and
// birth the element NULL. This pin asserts the element's POSITIONAL SLOT
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
		t.Fatalf("birthed positional row = %v, want 3 slots [ID ARR element]", got.Positional)
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
	// The element resolves by its output name X against the birthed row.
	if m, ok := rowMapOK(got); !ok || m["X"] != int64(101) {
		t.Errorf("element X by name = %#v, want int64(101)", got.Positional)
	}
}

// TestOrdinalAliasCollision pins the ordinal-alias-collision handling via
// PRODUCER CONTEXT: a WITH-ORDINALITY Explode leg (marked in OrdinalityLegs by
// newFlatMapCursor) binds STRICTLY POSITIONALLY (slot i = row[_i]) — so a user
// AS/AT alias that SPELLS an internal OrdinalFieldName (`FROM t, t.arr AS "_1"
// AT "_0"`) can't route the wrong internal key — while a SHAPE-IDENTICAL
// name-model leg whose OWN columns are aliased "_0"/"_1" (NOT an ordinality
// Explode) binds by NAME through adaptLegPositional. The two rows are
// indistinguishable by shape alone; only the producer signal disambiguates
// them.
func TestOrdinalAliasCollision(t *testing.T) {
	innerCorr := values.NamedCorrelationIdentifier("X")
	// The internal-keyed ordinality row: _0 = element, _1 = 1-based ordinal.
	datum := map[string]any{
		values.OrdinalFieldName(0): int64(101),
		values.OrdinalFieldName(1): int64(1),
	}
	ordRow := dmap(datum)

	// bindOrdinality binds a WITH-ORDINALITY leg (marked OrdinalityLegs) whose
	// type is named by the given AS/AT aliases, over the internal-keyed row.
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
		row := ordRow
		if err := b.bindLeg(legs, raw, innerCorr.Name(), &row); err != nil {
			t.Fatalf("bindLeg: %v", err)
		}
		return legs[innerCorr]
	}

	// AS "_1" collides with the internal ordinal key; the element (slot 0) must
	// still be the row's _0 slot, the ordinal (slot 1) the row's _1 slot.
	row := bindOrdinality(t, "_1", "O")
	if got, _ := row.Get(0); got != int64(101) {
		t.Fatalf("AS \"_1\" element slot = %#v, want 101 — a user alias spelling _1 must not read the internal ordinal key", got)
	}
	if got, _ := row.Get(1); got != int64(1) {
		t.Fatalf("AT O ordinal slot = %#v, want 1", got)
	}

	// The FULLY-colliding case — BOTH aliases spell ordinal keys, swapped:
	// `AS "_1" AT "_0"`. No shape discriminator can tell this apart from the
	// name-model union case below (leg type {_1,_0} over a row keyed {_0,_1}
	// is identical either way); producer-context positional binding still
	// puts the element at slot 0, the ordinal at slot 1.
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
	// column "_1" reads the row's "_1" value (1), column "_0" reads the row's
	// "_0" value (101) — NOT swapped to positional. This is the case a
	// shape-only discriminator would get wrong.
	nameLegType := values.NewRecordType("", true, []values.Field{
		{Name: "_1", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "_0", FieldType: values.NotNullLong, Ordinal: 1},
	})
	nameRow, err := adaptLegPositional(dmap(datum), nameLegType)
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
	outerRow := QueryResult{Positional: outerPos}
	got, err := c.computeResult(outerRow, QueryResult{})
	if err != nil {
		t.Fatalf("computeResult: %v", err)
	}
	if got.Positional.Slots[1] != nil {
		t.Fatalf("null element slot = %#v, want nil (NULL)", got.Positional.Slots[1])
	}
}
