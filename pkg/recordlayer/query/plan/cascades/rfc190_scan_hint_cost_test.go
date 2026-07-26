package cascades

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestScanPlanExpressionHintCost_StampedPartialPKPrefixNotPointProbe exercises
// the production primary-scan match-candidate path. In a multi-type store this
// cost is normally reached through scanPlanExpression (often around a
// TypeFilter), not by calling RecordQueryScanPlan.HintCost directly.
func TestScanPlanExpressionHintCost_StampedPartialPKPrefixNotPointProbe(t *testing.T) {
	t.Parallel()

	tenantAlias := values.NamedCorrelationIdentifier("tenant")
	orderAlias := values.NamedCorrelationIdentifier("order")
	candidate := NewPrimaryScanMatchCandidate(
		nil,
		[]values.CorrelationIdentifier{tenantAlias, orderAlias},
		[]string{"T", "U"},
		[]string{"T"},
		[]string{"TENANT", "ORDER"},
		false, // genuine shared-primary-key-range multi-type scan: no record-type-key prefix
		values.UnknownType,
	)

	eqTenant := pkGateEq(t, int64(7))
	partialPlan := candidate.ToScanPlan(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			tenantAlias: eqTenant,
		},
		false,
	)
	partialScan := pkScanFromDataAccessPlan(partialPlan)
	if partialScan == nil {
		t.Fatalf("candidate plan = %T, want a primary scan under at most a type filter", partialPlan)
	}
	if got := len(partialScan.GetPrimaryKeyValues()); got != 2 {
		t.Fatalf("production candidate stamped %d primary-key values, want 2", got)
	}
	if got := len(partialScan.GetScanComparisons()); got != 1 {
		t.Fatalf("production candidate emitted %d scan comparisons, want one-column prefix", got)
	}

	stats := properties.FixedStatistics{Cardinality: 1000}
	got := (&scanPlanExpression{plan: partialPlan}).HintCost(nil, stats)
	// NO PhysicalWrapperCostMultiplier on Cardinality (CPU-only, cost_formulas.go):
	// Cardinality is a logical-group property, so stacking a TypeFilter on top of
	// the scan must not shrink the row count beyond the two selectivity factors.
	want := stats.Cardinality *
		properties.EqualityBoundSelectivity *
		properties.TypeFilterSelectivity
	if math.Abs(got.Cardinality-want) > 1e-12 {
		t.Fatalf("partial-PK adapter cardinality = %v, want selectivity-priced %v", got.Cardinality, want)
	}

	fullPlan := candidate.ToScanPlan(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			tenantAlias: eqTenant,
			orderAlias:  pkGateEq(t, int64(9)),
		},
		false,
	)
	fullScan := pkScanFromDataAccessPlan(fullPlan)
	if fullScan == nil {
		t.Fatalf("full candidate plan = %T, want a primary scan under at most a type filter", fullPlan)
	}
	fullLeafCost := fullScan.HintCost(nil, stats)
	if fullLeafCost.Cardinality != 1 {
		t.Fatalf("fully equality-bound stamped scan cardinality = %v, want 1", fullLeafCost.Cardinality)
	}
	fullAdapterCost := (&scanPlanExpression{plan: fullPlan}).HintCost(nil, stats)
	// NO PhysicalWrapperCostMultiplier on Cardinality — see the partial-PK case above.
	wantFullAdapter := properties.TypeFilterSelectivity
	if math.Abs(fullAdapterCost.Cardinality-wantFullAdapter) > 1e-12 {
		t.Fatalf("full-PK adapter cardinality = %v, want point-priced %v", fullAdapterCost.Cardinality, wantFullAdapter)
	}
}

// TestScanProvableMaxCard_UsesStampedPKArity pins the logical memo-descent
// cardinality path: a composite-key prefix is unbounded even when every
// comparison present is an equality.
func TestScanProvableMaxCard_UsesStampedPKArity(t *testing.T) {
	t.Parallel()

	pk2 := []values.Value{
		&values.FieldValue{Field: "TENANT", Typ: values.UnknownType},
		&values.FieldValue{Field: "ORDER", Typ: values.UnknownType},
	}
	prefix := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey(pk2).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))})
	if card, known := scanProvableMaxCard(prefix); known {
		t.Fatalf("composite-PK prefix has provable max cardinality %v, want unknown", card)
	}

	full := prefix.WithScanComparisons([]*predicates.ComparisonRange{
		pkGateEq(t, int64(7)),
		pkGateEq(t, int64(9)),
	})
	if card, known := scanProvableMaxCard(full); !known || card != 1 {
		t.Fatalf("fully equality-bound stamped scan = (%v,%v), want (1,true)", card, known)
	}
}

// TestLogicalCounts_PrimaryKeyContextFallback exercises the logical-parent
// memo walk, which has a PlanContext available even though its physical child
// may be an older/hand-built unstamped scan.
func TestLogicalCounts_PrimaryKeyContextFallback(t *testing.T) {
	t.Parallel()

	ctx := &pkGateTestCtx{pk: []string{"TENANT", "ORDER"}}
	logicalParent := func(scan *plans.RecordQueryScanPlan) expressions.RelationalExpression {
		return expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
			expressions.ForEachQuantifier(expressions.FinalOf(scan)),
		)
	}

	partial := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithScanComparisons([]*predicates.ComparisonRange{pkGateEq(t, int64(7))})
	partialCounts := findExpressionsByType(logicalParent(partial), nil, ctx)
	if !partialCounts.unboundedDataAccess || partialCounts.maxDataAccessCardinality != -1 {
		t.Fatalf("ctx-proven composite prefix counts = {unbounded:%v max:%v}, want {true -1}",
			partialCounts.unboundedDataAccess, partialCounts.maxDataAccessCardinality)
	}

	full := partial.WithScanComparisons([]*predicates.ComparisonRange{
		pkGateEq(t, int64(7)),
		pkGateEq(t, int64(9)),
	})
	fullCounts := findExpressionsByType(logicalParent(full), nil, ctx)
	if fullCounts.unboundedDataAccess || fullCounts.maxDataAccessCardinality != 1 {
		t.Fatalf("ctx-proven full-PK counts = {unbounded:%v max:%v}, want {false 1}",
			fullCounts.unboundedDataAccess, fullCounts.maxDataAccessCardinality)
	}
}
