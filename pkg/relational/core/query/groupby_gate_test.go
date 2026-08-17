package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestSeedElementSlots_WindowsExplodeSeed pins seedElementSlots' positive
// contract: an Explode-quantifier select whose RC references the inner element
// windows, with the element field's slot resolved by RC index. (The companion
// negative test — rejecting a name-model seed lacking an AnchoredJoin marker —
// is gone along with the AnchoredJoin producer: that shape is no longer
// constructible, so there is nothing left to reject.)
func TestSeedElementSlots_WindowsExplodeSeed(t *testing.T) {
	t.Parallel()
	inner := values.NamedCorrelationIdentifier("EL")
	// An Explode quantifier over A.ARR, aliased EL — the seed's element leg.
	arrayType := values.NewArrayType(false, values.NotNullString)
	ownerType := &values.RecordType{Fields: []values.Field{{Name: "ARR", Ordinal: 0, FieldType: arrayType}}}
	collection := exactTestField(t, exactTestQOV(t, "A", ownerType), 0)
	explode, err := expressions.NewExplodeExpression(collection)
	if err != nil {
		t.Fatalf("NewExplodeExpression: %v", err)
	}
	quant := expressions.NamedForEachQuantifier(inner, expressions.InitialOf(explode))
	// One field that references the inner element (element slot at rc-index 0).
	elemQOV, err := values.NewQuantifiedObjectValue(inner, values.NotNullString)
	if err != nil {
		t.Fatalf("element QOV: %v", err)
	}
	elemField := values.RecordConstructorField{Name: "X", Value: elemQOV}
	rc := &values.RecordConstructorValue{Fields: []values.RecordConstructorField{elemField}}
	sel, err := expressions.NewSelectExpression(rc, []expressions.Quantifier{quant}, nil)
	if err != nil {
		t.Fatalf("NewSelectExpression: %v", err)
	}

	if got, slots, ok := seedElementSlots(sel); !ok || got == nil || slots["X"] != 0 {
		t.Fatalf("Explode seed must window: ok=%v rc=%v slots=%v, want ok + X@0", ok, got, slots)
	}
}
