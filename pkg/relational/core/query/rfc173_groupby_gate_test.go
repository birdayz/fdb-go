package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestSeedElementSlots_AnchoredJoinGate is the DIRECT white-box pin for the
// `!rc.AnchoredJoin` groupBy gate in seedElementSlots (cascades_translator.go): a
// name-model AnchoredJoin result value must NOT be windowed for a positional
// GROUP-BY / element bake — baking an ordinal over a name-model row evaluates the
// ordinal against the Datum map and errors.
//
// This is a white-box pin BY NECESSITY: no runnable SQL shape isolates the gate
// anymore. RFC-173 B ordinalized the last row-reachable declining box shape (the
// scalar-subquery conjunct → grouped_subquery_conjunct_gathers now gathers), and the
// only remaining name-model box shape (multi-esq under EXISTS) strands at
// physicalization for an UNRELATED reason, which MASKS the gate — flipping the gate
// leaves the multi-esq+GROUP BY census pin green (verified). So the discrimination is
// pinned here directly: build one Explode-quantifier seed with a single element field,
// once with a plain (ordinal) RC and once with an AnchoredJoin RC, and assert the gate
// admits the former and rejects the latter. Drop `|| rc.AnchoredJoin` from the guard
// and the anchored case returns ok=true → this goes red.
func TestSeedElementSlots_AnchoredJoinGate(t *testing.T) {
	t.Parallel()
	inner := values.NamedCorrelationIdentifier("EL")
	// An Explode quantifier over A.ARR, aliased EL — the seed's element leg.
	explode := expressions.NewExplodeExpression(
		values.NewFieldValue(values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("A")), "ARR", values.UnknownType),
	)
	quant := expressions.NamedForEachQuantifier(inner, expressions.InitialOf(explode))
	// One field that references the inner element (element slot at rc-index 0).
	elemField := values.RecordConstructorField{Name: "X", Value: values.NewQuantifiedObjectValue(inner)}
	mkSel := func(anchored bool) *expressions.SelectExpression {
		rc := &values.RecordConstructorValue{Fields: []values.RecordConstructorField{elemField}, AnchoredJoin: anchored}
		return expressions.NewSelectExpression(rc, []expressions.Quantifier{quant}, nil)
	}

	// Non-anchored (ordinal) seed: the gate ADMITS → windowable, element X at slot 0.
	// (Discriminating baseline: proves the rejection below is the AnchoredJoin bit and
	// not some other decline, so an always-reject seedElementSlots can't pass this.)
	if rc, slots, ok := seedElementSlots(mkSel(false)); !ok || rc == nil || slots["X"] != 0 {
		t.Fatalf("non-anchored seed must window: ok=%v rc=%v slots=%v, want ok + X@0", ok, rc, slots)
	}
	// Anchored (name-model) seed: the gate REJECTS → not windowable. THE PIN.
	if rc, slots, ok := seedElementSlots(mkSel(true)); ok || rc != nil || slots != nil {
		t.Fatalf("anchored (name-model) seed must be REJECTED by the !rc.AnchoredJoin groupBy gate: ok=%v rc=%v slots=%v", ok, rc, slots)
	}
}
