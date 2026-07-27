package plans

import (
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// nConjuncts builds n distinct ConstantPredicate(TriTrue) leaves — their
// truth value is irrelevant to these cost tests, only their COUNT and
// packaging matter.
func nConjuncts(n int) []predicates.QueryPredicate {
	out := make([]predicates.QueryPredicate, n)
	for i := range out {
		out[i] = predicates.NewConstantPredicate(predicates.TriTrue)
	}
	return out
}

// asAndPredicate packages n conjuncts as ONE top-level AndPredicate — the
// shape a constructing rule produces when it combines a residual into a
// single conjunction — instead of n separate top-level list entries.
func asAndPredicate(n int) []predicates.QueryPredicate {
	return []predicates.QueryPredicate{predicates.NewAnd(nConjuncts(n)...)}
}

// TestFilterCostFormulas_PackagingInvariant pins the fix for a real defect:
// RecordQueryFilterPlan.HintCost and RecordQueryPredicatesFilterPlan.HintCost
// used to count selectivity factors via len(predicates), which sees
// `[And(a, b)]` as ONE predicate but `[a, b]` as two — so the SAME logical
// residual was costed with a different number of selectivity factors purely
// because of how a rule happened to package it. Both HintCost methods now
// count via predicates.CountConjuncts, which flattens AndPredicate nesting,
// so packaging never changes the cost.
//
// This is a property, not a single example: for every conjunct count 1..4,
// list-form and AndPredicate-form must cost IDENTICALLY.
func TestFilterCostFormulas_PackagingInvariant(t *testing.T) {
	t.Parallel()

	child := []properties.Cost{{Cardinality: 1000, CPU: 100}}

	for n := 1; n <= 4; n++ {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()

			list := nConjuncts(n)
			anded := asAndPredicate(n)

			t.Run("RecordQueryFilterPlan", func(t *testing.T) {
				t.Parallel()
				listCost := NewRecordQueryFilterPlan(list, nil).HintCost(child, nil)
				andedCost := NewRecordQueryFilterPlan(anded, nil).HintCost(child, nil)
				if listCost != andedCost {
					t.Fatalf("n=%d: list-packaged cost %+v != AndPredicate-packaged cost %+v", n, listCost, andedCost)
				}
				wantSel := math.Pow(properties.FilterSelectivity, float64(n))
				wantCard := child[0].Cardinality * wantSel
				if math.Abs(listCost.Cardinality-wantCard) > 1e-9 {
					t.Fatalf("n=%d: cardinality=%v, want %v (%d selectivity factors)", n, listCost.Cardinality, wantCard, n)
				}
			})

			t.Run("RecordQueryPredicatesFilterPlan", func(t *testing.T) {
				t.Parallel()
				listCost := NewRecordQueryPredicatesFilterPlan(nil, list).HintCost(child, nil)
				andedCost := NewRecordQueryPredicatesFilterPlan(nil, anded).HintCost(child, nil)
				if listCost != andedCost {
					t.Fatalf("n=%d: list-packaged cost %+v != AndPredicate-packaged cost %+v", n, listCost, andedCost)
				}
				wantSel := math.Pow(properties.FilterSelectivity, float64(n))
				wantCard := child[0].Cardinality * wantSel
				if math.Abs(listCost.Cardinality-wantCard) > 1e-9 {
					t.Fatalf("n=%d: cardinality=%v, want %v (%d selectivity factors)", n, listCost.Cardinality, wantCard, n)
				}
			})
		})
	}
}

// TestNestedLoopJoinAndPredicatesFilterCost_AgreeOnSameLogicalResidual pins
// the cross-SHAPE half of the same invariant: a residual reaching the memo as
// a materialized RecordQueryNestedLoopJoinPlan's join predicates must apply
// the SAME number of selectivity factors as the identical residual reaching a
// RecordQueryPredicatesFilterPlan wrapped around a FlatMap's inner — the two
// physical realizations of one logical join. Checked for both packagings
// (list and single AndPredicate) and several conjunct counts.
func TestNestedLoopJoinAndPredicatesFilterCost_AgreeOnSameLogicalResidual(t *testing.T) {
	t.Parallel()

	outer := properties.Cost{Cardinality: 10, CPU: 1}
	inner := properties.Cost{Cardinality: 20, CPU: 2}

	for n := 1; n <= 4; n++ {
		n := n
		for _, tc := range []struct {
			name  string
			preds []predicates.QueryPredicate
		}{
			{"list", nConjuncts(n)},
			{"and-wrapped", asAndPredicate(n)},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				nlj := NewRecordQueryNestedLoopJoinPlan(nil, nil, tc.preds, JoinInner, "O", "I", nil)
				nljCost := nlj.HintCost([]properties.Cost{outer, inner}, nil)

				// The FlatMap's own inner already contains the join predicate
				// applied via a PredicatesFilter (as FlatMapCost's doc comment
				// establishes) — so the residual filter's selectivity, applied
				// to the SAME raw inner cardinality, must shrink innerCard by
				// exactly the factor NestedLoopJoinCost applies internally.
				filterCost := NewRecordQueryPredicatesFilterPlan(nil, tc.preds).HintCost([]properties.Cost{inner}, nil)

				wantSel := math.Pow(properties.FilterSelectivity, float64(n))
				wantNLJCard := outer.Cardinality * inner.Cardinality * wantSel
				if math.Abs(nljCost.Cardinality-wantNLJCard) > 1e-9 {
					t.Fatalf("n=%d %s: NLJ cardinality=%v, want %v", n, tc.name, nljCost.Cardinality, wantNLJCard)
				}
				wantFilterCard := inner.Cardinality * wantSel
				if math.Abs(filterCost.Cardinality-wantFilterCard) > 1e-9 {
					t.Fatalf("n=%d %s: PredicatesFilter cardinality=%v, want %v", n, tc.name, filterCost.Cardinality, wantFilterCard)
				}

				// Cross-shape agreement: outer*innerFiltered == the NLJ's own
				// outer*inner*sel computation, for the SAME residual.
				flatMapEquivalentCard := outer.Cardinality * filterCost.Cardinality
				if math.Abs(flatMapEquivalentCard-nljCost.Cardinality) > 1e-9 {
					t.Fatalf("n=%d %s: FlatMap-shape cardinality (outer*filteredInner)=%v != NLJ-shape cardinality=%v — same logical join, different selectivity factor count",
						n, tc.name, flatMapEquivalentCard, nljCost.Cardinality)
				}
			})
		}
	}
}
