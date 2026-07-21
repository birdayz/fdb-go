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
	if cost := concretePlanCost(prefix, stats, compositeCtx); cost.Cardinality == 1 {
		t.Fatalf("composite-PK prefix bind priced as point probe (cardinality=1); want selectivity estimate")
	}

	full := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7)), pkGateEq(t, int64(9))})
	if cost := concretePlanCost(full, stats, compositeCtx); cost.Cardinality != 1 {
		t.Fatalf("provable full-PK bind must stay a point probe, got cardinality=%v", cost.Cardinality)
	}

	// nil ctx (unprovable coverage): STRICTER policy at the join leaf —
	// never a point probe, even on an all-equality bind.
	if cost := concretePlanCost(full, stats, nil); cost.Cardinality == 1 {
		t.Fatalf("unprovable coverage must not price as point probe at the join leaf, got cardinality=1")
	}
}
