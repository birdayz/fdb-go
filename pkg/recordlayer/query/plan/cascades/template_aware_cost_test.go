package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/properties"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// buildFlatMapOverNilInnerFetch constructs a FlatMap whose inner is a push-through
// Fetch template with a NIL inner plan (FlatMap(outer=Scan, inner=Fetch(<nil>))), with
// the Fetch's real inner (a secondary index scan over a large record type) reachable
// only through the expression's quantifier graph — exactly the shape the data-access
// path produces for a correlated index-probe join inner (RFC-076 step 3b).
func buildFlatMapOverNilInnerFetch(t *testing.T) *physicalFlatMapWrapper {
	t.Helper()

	// Inner: a real index scan over a large record type (the buried data access).
	innerIndexPlan := plans.NewRecordQueryIndexPlan(
		"idx_customer", nil, []string{"Customers"}, values.UnknownType, false,
	)
	innerIndexWrapper := &physicalIndexScanWrapper{plan: innerIndexPlan, columnNames: []string{"CUSTOMER_ID"}}
	innerIndexRef := expressions.InitialOf(innerIndexWrapper)

	// Fetch template over the index scan, with a NIL inner plan (the push-through shell).
	nilInnerFetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(
		nil, nil, values.UnknownType, plans.FetchIndexRecordsPrimaryKey,
	)
	fetchQ := expressions.ForEachQuantifier(innerIndexRef)
	fetchWrapper := NewPhysicalFetchFromPartialRecordWrapper(nilInnerFetchPlan, fetchQ)
	if !isNilInnerFetch(fetchWrapper) {
		t.Fatal("setup: fetchWrapper should be a nil-inner Fetch shell")
	}
	fetchRef := expressions.InitialOf(fetchWrapper)

	// Outer: a full primary scan.
	outerScanPlan := plans.NewRecordQueryScanPlan([]string{"Orders"}, values.UnknownType, false)
	outerScanRef := expressions.InitialOf(&physicalScanWrapper{plan: outerScanPlan})

	// FlatMap join: outer = scan, inner = nil-inner Fetch shell.
	flatMapPlan := plans.NewRecordQueryFlatMapPlan(
		outerScanPlan, nilInnerFetchPlan,
		values.NamedCorrelationIdentifier("o"), values.NamedCorrelationIdentifier("i"),
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()), false,
	)
	outerQ := expressions.ForEachQuantifier(outerScanRef)
	innerQ := expressions.ForEachQuantifier(fetchRef)
	return newPhysicalFlatMapWrapper(flatMapPlan, outerQ, innerQ)
}

// TestExprConcreteCost_ResolvesNilInnerFetchTemplate pins RFC-076 step 3b: a join whose
// inner is a nil-inner Fetch template must be costed as its REAL ref-resolved inner, not
// as the free empty shell the plan tree shows. exprConcreteCost resolves the inner via
// the expression's quantifier graph; the plain concretePlanCost (which the buggy pre-3b
// path fed to compareJoinOrdering) sees the nil inner as ~free. The resolved cost MUST be
// strictly higher — else the cost model under-costs a full-scan-driven join order and
// picks it over a selective one (TestFDB_JoinSelPred_Repro, once the ordering-constraint
// pass (3a) makes the ordered template variant reachable).
func TestExprConcreteCost_ResolvesNilInnerFetchTemplate(t *testing.T) {
	t.Parallel()
	flatMapWrapper := buildFlatMapOverNilInnerFetch(t)

	stats := properties.DefaultStatistics{}
	// concretePlanCost walks the plan tree, where the Fetch inner is nil → ~free.
	shellCost := concretePlanCost(flatMapWrapper.GetRecordQueryPlan(), stats, nil)
	// exprConcreteCost resolves the Fetch's real inner (the index scan) via the ref graph.
	resolvedCost := exprConcreteCost(flatMapWrapper, stats, nil)

	if !(resolvedCost.CPU > shellCost.CPU) {
		t.Fatalf("expected template-resolved cost to EXCEED the free-shell cost; "+
			"resolved.CPU=%g shell.CPU=%g (3b did not resolve the nil-inner Fetch inner)",
			resolvedCost.CPU, shellCost.CPU)
	}
}

// TestExprConcreteCounts_SeesBuriedIndexScanUnderNilInnerFetch pins the criterion-#2 side
// of RFC-076 step 3b: the buried index scan under a nil-inner Fetch shell must be visible
// to the cost-model operator counts. The plain concretePlanCounts cannot see it (the
// shell's plan tree has no children); exprConcreteCounts descends the Fetch wrapper's
// quantifier to the real index scan and counts it.
func TestExprConcreteCounts_SeesBuriedIndexScanUnderNilInnerFetch(t *testing.T) {
	t.Parallel()
	flatMapWrapper := buildFlatMapOverNilInnerFetch(t)

	shellCounts := concretePlanCounts(flatMapWrapper.GetRecordQueryPlan(), nil)
	if shellCounts.indexScanCount != 0 {
		t.Fatalf("setup: concretePlanCounts should NOT see the buried index scan (nil-inner Fetch hides it), got indexScanCount=%d", shellCounts.indexScanCount)
	}

	resolvedCounts := exprConcreteCounts(flatMapWrapper, nil, nil)
	if resolvedCounts.indexScanCount < 1 {
		t.Fatalf("expected exprConcreteCounts to count the resolved inner index scan, got indexScanCount=%d", resolvedCounts.indexScanCount)
	}
	if resolvedCounts.flatMapCount != 1 {
		t.Fatalf("expected exprConcreteCounts to count the FlatMap, got flatMapCount=%d", resolvedCounts.flatMapCount)
	}
	if resolvedCounts.fetchCount < 1 {
		t.Fatalf("expected exprConcreteCounts to count the Fetch shell, got fetchCount=%d", resolvedCounts.fetchCount)
	}
}
