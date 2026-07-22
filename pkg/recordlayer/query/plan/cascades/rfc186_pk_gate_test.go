package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// pkGateTestCtx is a PlanContext stub resolving a fixed composite primary
// key, for the RFC-186 §2B point-probe gate pins.
type pkGateTestCtx struct {
	indexTestPlanContext
	pk []string
}

func (c *pkGateTestCtx) GetPrimaryKeyColumns(string) []string { return c.pk }

func pkGateEq(t *testing.T, v any) *predicates.ComparisonRange {
	t.Helper()
	cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, v)
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatal("failed to build equality range")
	}
	return res.Range
}

// TestPKFullyEqualityBound pins RFC-186 §2B's shared predicate arms.
func TestPKFullyEqualityBound(t *testing.T) {
	t.Parallel()
	scan := func(comps ...*predicates.ComparisonRange) *plans.RecordQueryScanPlan {
		return plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).WithScanComparisons(comps)
	}
	compositeCtx := &pkGateTestCtx{pk: []string{"TENANT", "ORDER"}}

	t.Run("no comparisons", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(scan(), nil)
		if fullBind || provable {
			t.Fatalf("empty comparisons = (%v,%v), want (false,false)", fullBind, provable)
		}
	})
	t.Run("nil ctx: full bind unprovable", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(scan(pkGateEq(t, int64(1))), nil)
		if !fullBind || provable {
			t.Fatalf("nil-ctx all-equality = (%v,%v), want (true,false)", fullBind, provable)
		}
	})
	t.Run("composite PK partial prefix bind is NOT full", func(t *testing.T) {
		t.Parallel()
		// One comparison present (tenant only) against PK (TENANT, ORDER):
		// a prefix scan, never a point probe.
		fullBind, provable := pkFullyEqualityBound(scan(pkGateEq(t, int64(7))), compositeCtx)
		if fullBind || !provable {
			t.Fatalf("partial prefix = (%v,%v), want (false,true)", fullBind, provable)
		}
	})
	t.Run("composite PK fully bound", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(scan(pkGateEq(t, int64(7)), pkGateEq(t, int64(9))), compositeCtx)
		if !fullBind || !provable {
			t.Fatalf("full bind = (%v,%v), want (true,true)", fullBind, provable)
		}
	})
}

// TestConcreteJoinCost_CompositePKPrefixNotPointProbe is RFC-186 test 3:
// the join-ordering leaf must NOT price a composite-PK equality PREFIX as
// a 1-row point probe. Pre-§2B the scan arm passed fullBindUnique=true
// unconditionally and scanLikeCost's `numBound == len(comps)` passed for
// the prefix (comps holds only the PRESENT comparison) — a potentially
// million-row prefix scan became a repeatable 1-row inner probe and join
// ordering drove off it.
func TestConcreteJoinCost_CompositePKPrefixNotPointProbe(t *testing.T) {
	t.Parallel()
	stats := properties.DefaultStatistics{}
	compositeCtx := &pkGateTestCtx{pk: []string{"TENANT", "ORDER"}}

	prefix := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))})
	if cost := concretePlanCostStrict(prefix, stats, compositeCtx, true); cost.Cardinality == 1 {
		t.Fatalf("composite-PK prefix bind priced as point probe (cardinality=1); want selectivity estimate")
	}

	full := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))})
	if cost := concretePlanCostStrict(full, stats, compositeCtx, true); cost.Cardinality != 1 {
		t.Fatalf("provable full-PK bind must stay a point probe, got cardinality=%v", cost.Cardinality)
	}

	// nil ctx (unprovable coverage) under the STRICT join-ordering policy:
	// never a point probe, even on an all-equality bind.
	if cost := concretePlanCostStrict(full, stats, nil, true); cost.Cardinality == 1 {
		t.Fatalf("unprovable coverage must not price as point probe at the strict join leaf, got cardinality=1")
	}

	// The ADVISORY policy (the data-access HintCost adapter path — no
	// PlanContext available): a full-equality bind still prices as a point
	// lookup; imposing strictness here re-broke the documented id-IN-(...)
	// full-scan mis-cost the adapter exists to fix.
	if cost := concretePlanCost(full, stats, nil); cost.Cardinality != 1 {
		t.Fatalf("advisory policy must keep the full-equality point lookup, got cardinality=%v", cost.Cardinality)
	}
	// Advisory + PROVABLY partial coverage still declines the shortcut.
	if cost := concretePlanCost(prefix, stats, compositeCtx); cost.Cardinality == 1 {
		t.Fatalf("advisory policy with provably partial coverage must not price as point probe")
	}
}

// TestConcreteJoinCost_DispatchesToHintCost pins RFC-186 §2D: an operator
// without an explicit arm in the join-ordering cost walk is priced by ITS
// OWN HintCost (the single source of truth), never first-child-transparent.
// Pre-§2D a limit under a join exposed its child's full cardinality (no
// cap) and a union exposed only its first branch (no sum, no merge CPU) —
// silently flipping join-order selection.
func TestConcreteJoinCost_DispatchesToHintCost(t *testing.T) {
	t.Parallel()
	stats := properties.DefaultStatistics{}

	t.Run("limit caps cardinality", func(t *testing.T) {
		t.Parallel()
		inner := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
		limit := plans.NewRecordQueryLimitPlan(inner, 7, 0)
		got := concretePlanCost(limit, stats, nil)
		want := limit.HintCost([]properties.Cost{concretePlanCost(inner, stats, nil)}, stats)
		if got != want {
			t.Fatalf("walk cost %+v != HintCost %+v (dispatch must delegate)", got, want)
		}
		// The cap (7, times the physical-wrapper discount) vs the full scan
		// (LeafScanCardinality): orders of magnitude apart — the walk must
		// see the CAPPED side.
		if got.Cardinality > 10 {
			t.Fatalf("LIMIT 7 over a full scan must cap cardinality (~7), got %v", got.Cardinality)
		}
	})

	t.Run("union sums children", func(t *testing.T) {
		t.Parallel()
		a := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
		b := plans.NewRecordQueryScanPlan([]string{"U"}, values.UnknownType, false)
		union := plans.NewRecordQueryUnorderedUnionPlan([]plans.RecordQueryPlan{a, b})
		got := concretePlanCost(union, stats, nil)
		ca := concretePlanCost(a, stats, nil)
		cb := concretePlanCost(b, stats, nil)
		// Union must reflect BOTH branches (modulo the wrapper discount) —
		// strictly above either single branch; first-child transparency
		// would return exactly ca.
		if got.Cardinality <= ca.Cardinality || got.Cardinality <= cb.Cardinality {
			t.Fatalf("union cardinality %v must exceed each branch (%v, %v) — first-child transparency detected",
				got.Cardinality, ca.Cardinality, cb.Cardinality)
		}
	})
}
