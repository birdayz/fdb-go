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

func pkGateComparison(t *testing.T, comparisonType predicates.ComparisonType, v any) *predicates.ComparisonRange {
	t.Helper()
	cmp := predicates.NewLiteralComparison(comparisonType, v)
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatalf("failed to build %v range", comparisonType)
	}
	return res.Range
}

func pkGateEq(t *testing.T, v any) *predicates.ComparisonRange {
	t.Helper()
	return pkGateComparison(t, predicates.ComparisonEquals, v)
}

// TestPKFullyEqualityBound pins RFC-186 §2B's shared predicate arms.
func TestPKFullyEqualityBound(t *testing.T) {
	t.Parallel()
	pk2 := []values.Value{
		&values.FieldValue{Field: "TENANT", Typ: values.UnknownType},
		&values.FieldValue{Field: "ORDER", Typ: values.UnknownType},
	}
	scan := func(recordTypes []string, pk []values.Value, comps ...*predicates.ComparisonRange) *plans.RecordQueryScanPlan {
		plan := plans.NewRecordQueryScanPlan(recordTypes, values.UnknownType, false)
		if pk != nil {
			plan = plan.WithPrimaryKey(pk)
		}
		return plan.WithScanComparisons(comps)
	}
	compositeCtx := &pkGateTestCtx{pk: []string{"TENANT", "ORDER"}}
	singleCtx := &pkGateTestCtx{pk: []string{"ID"}}

	t.Run("unknown arity and no comparisons", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(scan([]string{"T"}, nil), nil)
		if fullBind || provable {
			t.Fatalf("empty comparisons = (%v,%v), want (false,false)", fullBind, provable)
		}
	})
	t.Run("stamped arity and no comparisons", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(scan([]string{"T"}, pk2), nil)
		if fullBind || !provable {
			t.Fatalf("stamped empty comparisons = (%v,%v), want (false,true)", fullBind, provable)
		}
	})
	t.Run("nil ctx and unstamped equality is unprovable", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(
			scan([]string{"T"}, nil, pkGateEq(t, int64(1))),
			nil,
		)
		if fullBind || provable {
			t.Fatalf("unstamped nil-ctx equality = (%v,%v), want (false,false)", fullBind, provable)
		}
	})
	t.Run("stamped composite PK partial prefix bind is not full", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(
			scan([]string{"T"}, pk2, pkGateEq(t, int64(7))),
			nil,
		)
		if fullBind || !provable {
			t.Fatalf("stamped partial prefix = (%v,%v), want (false,true)", fullBind, provable)
		}
	})
	t.Run("stamped composite PK fully bound with nil ctx", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(
			scan([]string{"T"}, pk2, pkGateEq(t, int64(7)), pkGateEq(t, int64(9))),
			nil,
		)
		if !fullBind || !provable {
			t.Fatalf("stamped full bind = (%v,%v), want (true,true)", fullBind, provable)
		}
	})
	t.Run("context fallback rejects composite PK partial prefix", func(t *testing.T) {
		t.Parallel()
		// One comparison present (tenant only) against PK (TENANT, ORDER):
		// a prefix scan, never a point probe.
		fullBind, provable := pkFullyEqualityBound(
			scan([]string{"T"}, nil, pkGateEq(t, int64(7))),
			compositeCtx,
		)
		if fullBind || !provable {
			t.Fatalf("partial prefix = (%v,%v), want (false,true)", fullBind, provable)
		}
	})
	t.Run("context fallback accepts composite PK fully bound", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(
			scan([]string{"T"}, nil, pkGateEq(t, int64(7)), pkGateEq(t, int64(9))),
			compositeCtx,
		)
		if !fullBind || !provable {
			t.Fatalf("full bind = (%v,%v), want (true,true)", fullBind, provable)
		}
	})
	t.Run("stamped arity wins over conflicting context", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(
			scan([]string{"T"}, pk2, pkGateEq(t, int64(7))),
			singleCtx,
		)
		if fullBind || !provable {
			t.Fatalf("stamped composite prefix with single-column ctx = (%v,%v), want (false,true)", fullBind, provable)
		}
	})
	t.Run("range is never a full equality bind", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(
			scan(
				[]string{"T"},
				pk2,
				pkGateEq(t, int64(7)),
				pkGateComparison(t, predicates.ComparisonGreaterThan, int64(9)),
			),
			nil,
		)
		if fullBind || !provable {
			t.Fatalf("stamped range = (%v,%v), want (false,true)", fullBind, provable)
		}
	})
	t.Run("unstamped multi-type scan does not use first type context", func(t *testing.T) {
		t.Parallel()
		fullBind, provable := pkFullyEqualityBound(
			scan([]string{"T", "U"}, nil, pkGateEq(t, int64(7))),
			singleCtx,
		)
		if fullBind || provable {
			t.Fatalf("unstamped multi-type equality = (%v,%v), want (false,false)", fullBind, provable)
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
	pk2 := []values.Value{
		&values.FieldValue{Field: "TENANT", Typ: values.UnknownType},
		&values.FieldValue{Field: "ORDER", Typ: values.UnknownType},
	}

	prefix := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey(pk2).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))})
	if cost := concretePlanCost(prefix, stats, compositeCtx); cost.Cardinality == 1 {
		t.Fatalf("composite-PK prefix bind priced as point probe (cardinality=1); want selectivity estimate")
	}

	full := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey(pk2).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))})
	if cost := concretePlanCost(full, stats, compositeCtx); cost.Cardinality != 1 {
		t.Fatalf("provable full-PK bind must stay a point probe, got cardinality=%v", cost.Cardinality)
	}

	// The plan stamp proves coverage even without PlanContext.
	if cost := concretePlanCost(full, stats, nil); cost.Cardinality != 1 {
		t.Fatalf("stamped full-PK bind with nil ctx must stay a point probe, got cardinality=%v", cost.Cardinality)
	}

	// Without either a plan stamp or context, equality coverage is unproven
	// and must fall back to selectivity pricing.
	unstampedFull := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))})
	if cost := concretePlanCost(unstampedFull, stats, nil); cost.Cardinality == 1 {
		t.Fatalf("unstamped nil-ctx equality must not price as a point probe")
	}

	// A single-record-type context remains a fallback for unstamped plans.
	if cost := concretePlanCost(unstampedFull, stats, compositeCtx); cost.Cardinality != 1 {
		t.Fatalf("context-proven full-PK bind must be a point probe, got cardinality=%v", cost.Cardinality)
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
