package executor

// RFC-173 S4 commit 4 — the identity-FlatMap pass-through widening. A gated
// minted-dup outer with a LEG-INDEPENDENT existential (the exists inner has no
// baked outer refs, so the item-2 inner probe outerBakedType is nil) must still
// flow its ordinal positional row through the identity pass-through, keyed on
// the OUTER's own gated seed (outerMergedType from downstreamLegWindows) — else
// the minted-dup upper serves NULLs off a name Datum that never had the
// binding-keyed column. This pin discriminates the ORDINAL outer (propagates)
// from the NAME-MODEL outer (does not) — the load-bearing invariant that keeps
// the widening from silently swallowing the name-model existential class. Proven
// discriminating: neuter the `adaptType = c.outerMergedType` fallback and the
// gated case regresses (Positional not propagated) while the name-model case is
// unchanged.

import (
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestRFC173Commit4_IdentityPassThroughDiscriminates(t *testing.T) {
	t.Parallel()
	legA, legB, _, _, seed := ojWiringLegs(t) // A(ID,V), B(ID,W); seed = [A.ID,A.V,B.ID,B.W]
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, legA, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, legB, false)
	mergedCorr := values.NamedCorrelationIdentifier("M")
	existCorr := values.NamedCorrelationIdentifier("EX")

	// The identity pass-through RV: the whole outer object (QOV(outer)) — the
	// WHERE-EXISTS shape. A leg-independent exists inner (a plain scan, no baked
	// outer refs) leaves outerBakedType nil, so the item-2 probe can't recognise
	// the ordinal outer.
	identityRV := values.NewQuantifiedObjectValue(mergedCorr)
	legIndependentInner := plans.NewRecordQueryScanPlan([]string{"C"}, values.UnknownType, false)

	innerRow := QueryResult{Datum: map[string]any{}}

	// GATED ORDINAL OUTER: the step-1 NLJ carries the ordinal seed → outerMergedType
	// set (from downstreamLegWindowsTyped), outerBakedType nil (leg-independent inner).
	gatedOuter := plans.NewRecordQueryNestedLoopJoinPlan(scanA, scanB, nil, plans.JoinInner, "A", "B", seed)
	cGated, err := newFlatMapCursor(recordlayer.FromList([]QueryResult{}), gatedOuter, legIndependentInner, nil,
		EmptyEvaluationContext(), mergedCorr, existCorr, identityRV, false, recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("newFlatMapCursor (gated): %v", err)
	}
	defer cGated.Close()
	if cGated.outerBakedType != nil {
		t.Fatal("a leg-independent exists inner must leave the item-2 probe (outerBakedType) NIL")
	}
	if cGated.outerMergedType == nil {
		t.Fatal("commit 4: a gated ordinal join OUTER must be recognised (outerMergedType != nil) so its positional row can flow")
	}
	// A SEED-LAYOUT outer positional row (the merged type the birth produces):
	// adaptLegPositional against outerMergedType is then a no-op and the slots
	// flow verbatim — proving the propagation, not a realignment artifact.
	pos := NewPositionalRow(cGated.outerMergedType)
	pos.Set(0, int64(1))
	pos.Set(1, int64(10))
	pos.Set(2, int64(2))
	pos.Set(3, int64(20))
	outerRow := QueryResult{Datum: map[string]any{}, Positional: pos}
	outGated, err := cGated.computeResultLegs(outerRow, &innerRow)
	if err != nil {
		t.Fatalf("computeResultLegs (gated): %v", err)
	}
	if outGated.Positional == nil {
		t.Fatal("commit 4: the gated outer's positional row must PROPAGATE through the identity pass-through (a revert of the widening drops it)")
	}
	if v, ok := outGated.Positional.Get(2); !ok || v != int64(2) {
		t.Fatalf("propagated positional slot 2 (B.ID) = (%v,%v), want (2,true) — the seed-layout row flows verbatim (adapt no-op)", v, ok)
	}

	// NAME-MODEL OUTER: the outer NLJ carries a NAME-MODEL RC (lazy dotted refs,
	// not FrontierPinned) → ordinalJoinSpansOf rejects it → outerMergedType NIL.
	// The widening must NOT recognise it — this is the discriminator that keeps
	// the name-model existential class (CorrelatedExistsCrossJoin) untouched.
	nameModelRV := values.NewRawRecordConstructorValue(
		values.RecordConstructorField{Name: "A.ID", Value: &values.FieldValue{Field: "A.ID", Typ: values.NotNullLong}},
		values.RecordConstructorField{Name: "B.ID", Value: &values.FieldValue{Field: "B.ID", Typ: values.NotNullLong}},
	)
	nameModelOuter := plans.NewRecordQueryNestedLoopJoinPlan(scanA, scanB, nil, plans.JoinInner, "A", "B", nameModelRV)
	cName, err := newFlatMapCursor(recordlayer.FromList([]QueryResult{}), nameModelOuter, legIndependentInner, nil,
		EmptyEvaluationContext(), mergedCorr, existCorr, identityRV, false, recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("newFlatMapCursor (name-model): %v", err)
	}
	defer cName.Close()
	if cName.outerMergedType != nil {
		t.Fatal("a NAME-MODEL outer (lazy anchored RC, not a FrontierPinned seed) must NOT be recognised — the widening must not swallow the name-model existential class")
	}
}
