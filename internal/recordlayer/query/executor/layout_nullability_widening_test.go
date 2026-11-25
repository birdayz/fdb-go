package executor

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestAttachOrdinalLayout_NotNullRowTakesNullableCarrier pins the one type
// disagreement the layout attach admits: a NOT NULL row under a carrier that is
// the same record made nullable. A plan's result value can keep a leg's
// nullable row over a quantifier that flows the NOT NULL one (the null-on-empty
// flag eliminated under a null-rejecting predicate, factory scenarios
// fc_0000000534_q3 and fc_0000000556_q5); the row is a value of the carrier and
// takes its type. Every other disagreement is still refused.
func TestAttachOrdinalLayout_NotNullRowTakesNullableCarrier(t *testing.T) {
	t.Parallel()

	notNull := values.NewRecordType("leg", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
	})
	nullable, ok := values.WithNullability(notNull, true).(*values.RecordType)
	if !ok || !nullable.IsNullable() {
		t.Fatal("fixture: the nullable carrier is not a nullable record")
	}
	_, nullableLayout := scanPlanWithLayout(t, nullable)
	_, notNullLayout := scanPlanWithLayout(t, notNull)

	row := NewPositionalRow(notNull)
	row.Slots[0] = int64(7)
	attached, err := row.AttachOrdinalLayout(nullableLayout, nullableLayout.Carrier().FlowedType())
	if err != nil {
		t.Fatalf("a NOT NULL row under its nullable carrier was refused: %v", err)
	}
	if !attached.Type.Equals(nullable) {
		t.Errorf("attached row type %v, want the carrier's %v", attached.Type, nullable)
	}
	if attached == row || !row.Type.Equals(notNull) {
		t.Error("the input row was mutated instead of copied")
	}
	if attached.Slots[0] != int64(7) {
		t.Errorf("slot 0 = %v, want 7", attached.Slots[0])
	}

	nullableRow := NewPositionalRow(nullable)
	if _, err := nullableRow.AttachOrdinalLayout(notNullLayout, notNullLayout.Carrier().FlowedType()); err == nil {
		t.Error("a nullable row under a NOT NULL carrier was admitted")
	}

	other := values.NewRecordType("leg", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
	})
	otherRow := NewPositionalRow(other)
	if _, err := otherRow.AttachOrdinalLayout(nullableLayout, nullableLayout.Carrier().FlowedType()); err == nil {
		t.Error("a row whose FIELD types differ from the carrier's was admitted")
	}
}
