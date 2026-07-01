package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestBoundSelectivity_CostMonotonicity is RFC-164 WS-4's cost-monotonicity
// property test: a metamorphic invariant on the cost model's selectivity, with no
// Java oracle (the scalar cost model is a Go-only path). It pins the ordering that
// the COST-SELECTIVITY bug (#405) inverted — where a generic residual
// FilterSelectivity (0.5) was applied to an equality bound, making a point probe
// look LESS selective than a range and mis-picking the index. The two invariants:
//
//  1. Constant ordering: an equality (point) probe is more selective than an open
//     range, which is more selective than a generic residual filter —
//     EqualityBoundSelectivity < RangeSelectivity < FilterSelectivity.
//  2. boundSelectivity monotonicity: a MORE selective bound set never estimates
//     MORE rows. An equality bound out-selects a range; adding ANY bound only
//     lowers selectivity (each factor is in (0,1)); empty/nil bounds are inert.
//
// Scope (Graefe): this is LAYERED protection, not sole coverage. It pins the
// in-boundSelectivity invariant — the load-bearing `selEq < selRng` catches a
// future edit that re-applies FilterSelectivity (0.5) to an equality branch (the
// exact #405 inversion) even if the constants stay correctly ordered. The actual
// index MIS-PICK it caused is guarded at the plan level by
// TestCostSelectivity_PrefersEqualityIndex (pkg/relational/core/embedded); this
// test does not exercise the three callers (physical{,Index}ScanWrapper.HintCost,
// scanLikeCost) that turn `sel` into a cardinality, nor scanLikeCost's
// `fullBindUnique` 1-row short-circuit (the low-NDV secondary-index hazard) —
// those remain a follow-up (RFC-164 WS-4).
//
// CAVEAT: this pins EqualityBoundSelectivity = 0.1 as a STATLESS high-NDV
// assumption (boundSelectivity's own doc, planning_cost_model.go). The metamorphic
// framing (selEq < selRng, monotonicity) survives a future per-column-NDV fix that
// makes equality selectivity vary — the exact-value sibling TestBoundSelectivity
// would not.
func TestBoundSelectivity_CostMonotonicity(t *testing.T) {
	t.Parallel()

	// (1) Constant ordering — the load-bearing COST-SELECTIVITY invariant.
	if !(properties.EqualityBoundSelectivity < properties.RangeSelectivity) {
		t.Errorf("EqualityBoundSelectivity (%v) must be < RangeSelectivity (%v): a point probe is more selective than an open range",
			properties.EqualityBoundSelectivity, properties.RangeSelectivity)
	}
	if !(properties.RangeSelectivity < properties.FilterSelectivity) {
		t.Errorf("RangeSelectivity (%v) must be < FilterSelectivity (%v): a sargable range beats a generic residual filter",
			properties.RangeSelectivity, properties.FilterSelectivity)
	}

	eq := mkComparisonRange(t, predicates.ComparisonEquals)
	if !eq.IsEquality() {
		t.Fatalf("expected an equality range, got %v", eq.GetRangeType())
	}
	rng := mkComparisonRange(t, predicates.ComparisonLessThan)
	if !rng.IsInequality() {
		t.Fatalf("expected an inequality (range) range, got %v", rng.GetRangeType())
	}

	selEq, nEq, allEq := boundSelectivity([]*predicates.ComparisonRange{eq})
	selRng, nRng, allEqRng := boundSelectivity([]*predicates.ComparisonRange{rng})

	// (2a) An equality bound is strictly more selective (lower sel) than a range.
	if !(selEq < selRng) {
		t.Errorf("boundSelectivity(equality)=%v must be < boundSelectivity(range)=%v — else the planner prefers a range scan over a point probe",
			selEq, selRng)
	}
	if nEq != 1 || !allEq {
		t.Errorf("single equality bound: numBound=%d allEquality=%v, want 1, true", nEq, allEq)
	}
	if nRng != 1 || allEqRng {
		t.Errorf("single range bound: numBound=%d allEquality=%v, want 1, false", nRng, allEqRng)
	}

	// (2b) Monotonicity: adding ANY further bound only LOWERS selectivity
	// (never estimates more rows) — each additional factor is in (0,1).
	sel2Eq, _, _ := boundSelectivity([]*predicates.ComparisonRange{eq, eq})
	if !(sel2Eq < selEq) {
		t.Errorf("adding an equality bound must lower selectivity: sel([eq,eq])=%v not < sel([eq])=%v", sel2Eq, selEq)
	}
	selEqRng, _, allEqBoth := boundSelectivity([]*predicates.ComparisonRange{eq, rng})
	if !(selEqRng < selEq) || !(selEqRng < selRng) {
		t.Errorf("adding a bound must lower selectivity below EITHER single bound: sel([eq,rng])=%v, sel([eq])=%v, sel([rng])=%v",
			selEqRng, selEq, selRng)
	}
	if allEqBoth {
		t.Error("a bound set containing a range must report allEquality=false")
	}

	// (3) Identity: no bounds → no selectivity reduction.
	selNone, nNone, allEqNone := boundSelectivity(nil)
	if selNone != 1.0 || nNone != 0 || !allEqNone {
		t.Errorf("no bounds: sel=%v numBound=%d allEquality=%v, want 1.0, 0, true", selNone, nNone, allEqNone)
	}

	// (4) Empty / nil bounds are inert — they must not change selectivity or count
	// (a dropped/unbound scan column must not spuriously make an access look cheaper).
	selWithInert, nWithInert, allEqInert := boundSelectivity([]*predicates.ComparisonRange{predicates.EmptyComparisonRange(), eq, nil})
	if selWithInert != selEq || nWithInert != 1 || !allEqInert {
		t.Errorf("empty/nil bounds must be skipped: sel=%v numBound=%d allEquality=%v, want %v, 1, true", selWithInert, nWithInert, allEqInert, selEq)
	}
}

// mkComparisonRange builds a single-comparison ComparisonRange of the given type by
// merging it into an empty range (the same construction the planner uses).
func mkComparisonRange(t *testing.T, typ predicates.ComparisonType) *predicates.ComparisonRange {
	t.Helper()
	comp := predicates.Comparison{
		Type:    typ,
		Operand: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("b")),
	}
	mr := predicates.EmptyComparisonRange().Merge(&comp)
	if !mr.Ok {
		t.Fatalf("failed to merge %v comparison into an empty range", typ)
	}
	return mr.Range
}
