package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// twoPhysicalScanWinners builds two distinct physical scan wrappers (over
// different record types, so their structural plan-hash differs) for use as
// stamped winners.
func twoPhysicalScanWinners(t *testing.T) (expressions.RelationalExpression, expressions.RelationalExpression) {
	t.Helper()
	ref1 := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"Table1"}, values.UnknownType))
	FireExpressionRule(NewPrimaryScanRule(), ref1)
	w1 := findPhysicalExpr(ref1)

	ref2 := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"Table2"}, values.UnknownType))
	FireExpressionRule(NewPrimaryScanRule(), ref2)
	w2 := findPhysicalExpr(ref2)

	if w1 == nil || w2 == nil {
		t.Fatal("PrimaryScanRule did not yield physical scan winners")
	}
	return w1, w2
}

// TestGetWinnerForOrdering_DeterministicCheapestAmongSatisfyingWinners (F20)
// pins that when no exact-key winner exists, getWinnerForOrdering returns the
// `less`-CHEAPEST stamped winner whose ordering satisfies the request — the
// same cost-aware, deterministic selection as the un-stamped fallback — instead
// of the FIRST satisfying winner in (randomized) Go map-iteration order. The
// old code returned the first satisfying winner in map order: cost-blind AND
// nondeterministic (flips across plannings). Java prunes each Reference to ONE
// winner via a total-order cost model whose compare ends in a deterministic
// planHash tie-break.
func TestGetWinnerForOrdering_DeterministicCheapestAmongSatisfyingWinners(t *testing.T) {
	t.Parallel()

	w1, w2 := twoPhysicalScanWinners(t)

	// Stamp two winners under superset orderings [A,B] and [A,C]. Neither is the
	// exact-key winner for the request [A asc], so lookup enters the winner scan;
	// both orderings Satisfy [A asc], so BOTH are candidates.
	ref := expressions.InitialOf(w1)
	keyAB := expressions.OrderingFromNameDir([]string{"A", "B"}, []bool{false, false})
	keyAC := expressions.OrderingFromNameDir([]string{"A", "C"}, []bool{false, false})
	ref.SetWinner(keyAB, w1)
	ref.SetWinner(keyAC, w2)

	reqOrd := NewRequestedOrdering(
		[]RequestedOrderingPart{{Value: &values.FieldValue{Field: "A"}, SortOrder: RequestedSortOrderAscending}},
		DistinctnessPreserveDistinctness, false)

	// A rank map gives an explicit, controllable total order over the two winners.
	rankOf := func(cheapest expressions.RelationalExpression) func(a, b expressions.RelationalExpression) bool {
		return func(a, b expressions.RelationalExpression) bool {
			// The `cheapest` argument is strictly less than anything else.
			return a == cheapest && b != cheapest
		}
	}

	const iters = 500

	// (a) cost-awareness: the returned winner is the `less`-cheapest candidate.
	//     Prove it is genuinely cost-driven by flipping which winner is cheapest.
	for _, cheapest := range []expressions.RelationalExpression{w1, w2} {
		less := rankOf(cheapest)
		for i := 0; i < iters; i++ {
			got := getWinnerForOrdering(ref, reqOrd, less)
			if got != cheapest {
				t.Fatalf("iter %d: cost-aware winner = %p, want cheapest %p", i, got, cheapest)
			}
		}
	}

	// (b) even a comparator that TIES the two winners (returns false both ways,
	//     i.e. carries no cost tie-break of its own) yields a DETERMINISTIC
	//     winner — the wrapper adds the structural plan-hash tie-break so the
	//     min is unique regardless of map iteration order. Requires the two
	//     winners to hash apart.
	if costExprHash(w1) == costExprHash(w2) {
		t.Fatal("precondition: the two winners must have distinct costExprHash for the tie-break to be observable")
	}
	tyingLess := func(a, b expressions.RelationalExpression) bool { return false }
	first := getWinnerForOrdering(ref, reqOrd, tyingLess)
	for i := 0; i < iters; i++ {
		if got := getWinnerForOrdering(ref, reqOrd, tyingLess); got != first {
			t.Fatalf("iter %d: tying-comparator winner flipped to %p, want stable %p", i, got, first)
		}
	}

	// (c) determinism with the DEFAULT cost model (nil less): the SAME winner
	//     every iteration. This is the core revert-proof — the old first-in-map
	//     scan returns 2 distinct winners across 500 calls.
	distinct := map[expressions.RelationalExpression]struct{}{}
	for i := 0; i < iters; i++ {
		distinct[getWinnerForOrdering(ref, reqOrd, nil)] = struct{}{}
	}
	if len(distinct) != 1 {
		t.Fatalf("default-less winner is nondeterministic: %d distinct winners across %d calls", len(distinct), iters)
	}
}

// eqIndexScan builds an index scan on `idx` with a single equality comparand.
func eqIndexScan(t *testing.T, lit any) *plans.RecordQueryIndexPlan {
	t.Helper()
	res := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: values.LiteralValue(lit),
	})
	if !res.Ok {
		t.Fatalf("merge failed for = %v", lit)
	}
	return plans.NewRecordQueryIndexPlan("idx",
		[]*predicates.ComparisonRange{res.Range}, []string{"T"}, values.UnknownType, false)
}

// TestMemoizeLeaf_IndexScanComparandNotCollapsed (F21 memo-level) pins that two
// index scans differing only in their equality comparand intern into DISTINCT
// References (the memo leaf dedup keys on HashCodeWithoutChildren +
// EqualsWithoutChildren). Before the F21 fix these collapsed into one Reference,
// so extraction could materialize the wrong-comparand scan.
func TestMemoizeLeaf_IndexScanComparandNotCollapsed(t *testing.T) {
	t.Parallel()

	m := NewMemo(nil)
	w5 := &physicalIndexScanWrapper{plan: eqIndexScan(t, int64(5)), columnNames: []string{"c"}}
	w7 := &physicalIndexScanWrapper{plan: eqIndexScan(t, int64(7)), columnNames: []string{"c"}}

	ref5 := m.MemoizeExpression(w5)
	ref7 := m.MemoizeExpression(w7)
	if ref5 == ref7 {
		t.Error("IndexScan([= 5]) and IndexScan([= 7]) collapsed into ONE Reference — F21 memo collapse")
	}

	// An identical-comparand scan MUST still intern into the SAME Reference
	// (dedup preserved for genuine twins).
	w5b := &physicalIndexScanWrapper{plan: eqIndexScan(t, int64(5)), columnNames: []string{"c"}}
	ref5b := m.MemoizeExpression(w5b)
	if ref5 != ref5b {
		t.Error("identical IndexScan([= 5]) must intern into the SAME Reference")
	}
}
