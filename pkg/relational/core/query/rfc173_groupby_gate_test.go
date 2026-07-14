package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestSeedElementSlots_WindowsExplodeSeed pins seedElementSlots' positive
// contract: an Explode-quantifier select whose RC references the inner element
// windows, with the element field's slot resolved by RC index. (The companion
// negative — the `!rc.AnchoredJoin` groupBy gate rejecting a name-model seed —
// was deleted with the AnchoredJoin producer and marker, RFC-173 S4 item B:
// the rejected shape is no longer constructible.)
func TestSeedElementSlots_WindowsExplodeSeed(t *testing.T) {
	t.Parallel()
	inner := values.NamedCorrelationIdentifier("EL")
	// An Explode quantifier over A.ARR, aliased EL — the seed's element leg.
	explode := expressions.NewExplodeExpression(
		values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A")), "ARR", values.UnknownType),
	)
	quant := expressions.NamedForEachQuantifier(inner, expressions.InitialOf(explode))
	// One field that references the inner element (element slot at rc-index 0).
	elemField := values.RecordConstructorField{Name: "X", Value: values.NewQuantifiedObjectValue(inner)}
	rc := &values.RecordConstructorValue{Fields: []values.RecordConstructorField{elemField}}
	sel := expressions.NewSelectExpression(rc, []expressions.Quantifier{quant}, nil)

	if got, slots, ok := seedElementSlots(sel); !ok || got == nil || slots["X"] != 0 {
		t.Fatalf("Explode seed must window: ok=%v rc=%v slots=%v, want ok + X@0", ok, got, slots)
	}
}
