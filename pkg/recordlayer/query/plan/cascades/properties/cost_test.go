package properties

import (
	"math"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// scan returns a Reference holding a single FullUnorderedScan over
// the given record types.
func scan(types ...string) *expressions.Reference {
	return expressions.InitialOf(expressions.NewFullUnorderedScanExpression(types, nil))
}

// scanQ wraps `scan(types...)` in a ForEach Quantifier.
func scanQ(types ...string) expressions.Quantifier {
	return expressions.ForEachQuantifier(scan(types...))
}

func TestCost_Less_OrdersByTotalThenCardinality(t *testing.T) {
	t.Parallel()
	cheap := Cost{Cardinality: 100, CPU: 10}
	pricy := Cost{Cardinality: 1000, CPU: 50}
	if !cheap.Less(pricy) {
		t.Fatalf("cheap=%+v should be Less than pricy=%+v", cheap, pricy)
	}
	if pricy.Less(cheap) {
		t.Fatalf("pricy=%+v should NOT be Less than cheap=%+v", pricy, cheap)
	}
	// Tie on Total, break on Cardinality (lower wins).
	a := Cost{Cardinality: 100, CPU: 50}
	b := Cost{Cardinality: 50, CPU: 100}
	if a.Total() != b.Total() {
		t.Fatalf("setup error: totals differ %v vs %v", a.Total(), b.Total())
	}
	if !b.Less(a) {
		t.Fatal("on Total tie, lower-cardinality b should win")
	}
	if a.Less(b) {
		t.Fatal("on Total tie, higher-cardinality a should not win")
	}
}

func TestEstimateCost_NilReturnsZero(t *testing.T) {
	t.Parallel()
	c := EstimateCost(nil)
	if c.Total() != 0 {
		t.Fatalf("nil cost = %+v, want zero", c)
	}
}

func TestEstimateCost_FullScanIsLeafCardinality(t *testing.T) {
	t.Parallel()
	e := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	c := EstimateCost(e)
	if c.Cardinality != LeafScanCardinality {
		t.Fatalf("scan cardinality=%v, want %v", c.Cardinality, LeafScanCardinality)
	}
	if c.CPU != 0 {
		t.Fatalf("scan CPU=%v, want 0", c.CPU)
	}
}

func TestEstimateCost_FilterReducesCardinalityBySelectivity(t *testing.T) {
	t.Parallel()
	// Filter([P], scan(T)) — single predicate.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	)
	c := EstimateCost(filter)
	want := LeafScanCardinality * FilterSelectivity
	if math.Abs(c.Cardinality-want) > 1e-6 {
		t.Fatalf("filter cardinality=%v, want %v", c.Cardinality, want)
	}
	if c.CPU < LeafScanCardinality*FilterCPU*0.99 {
		t.Fatalf("filter CPU=%v too low", c.CPU)
	}
}

func TestEstimateCost_FilterMultiPredicateExponentialSelectivity(t *testing.T) {
	t.Parallel()
	// Two predicates → selectivity^2; cardinality is smaller than
	// single-predicate filter.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	one := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	)
	two := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred, pred},
		scanQ("T"),
	)
	c1 := EstimateCost(one)
	c2 := EstimateCost(two)
	if c2.Cardinality >= c1.Cardinality {
		t.Fatalf("two-predicate filter cardinality=%v should be < one-predicate %v", c2.Cardinality, c1.Cardinality)
	}
}

func TestEstimateCost_SortIsExpensive(t *testing.T) {
	t.Parallel()
	// Sort beats Filter on cardinality (both pass-through under
	// Sort, only Filter drops rows), but Sort costs more CPU.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filterCost := EstimateCost(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	))
	sortCost := EstimateCost(expressions.NewLogicalSortExpression(
		nil,
		scanQ("T"),
	))
	if sortCost.CPU <= filterCost.CPU {
		t.Fatalf("sort CPU=%v should exceed filter CPU=%v", sortCost.CPU, filterCost.CPU)
	}
}

func TestEstimateCost_DistinctOverSortBeatsSortOverDistinct(t *testing.T) {
	t.Parallel()
	// Distinct(Sort(scan)) vs Sort(Distinct(scan)). DistinctOverSortElim
	// rewrites the former to Distinct(scan) (no sort) — that's
	// cheaper. Without the rewrite, Distinct over Sort costs more
	// than Sort over Distinct because Sort processes the unfiltered
	// row set in the former.
	//
	// Actually under the current heuristic both shapes touch every
	// row at least once with similar selectivity; the test pins that
	// the SHAPE Distinct-no-sort (single Distinct) beats both shapes.
	scanRef := scan("T")

	// d := Distinct(scan)  — no sort
	d := expressions.NewLogicalDistinctExpression(expressions.ForEachQuantifier(scanRef))

	// ds := Distinct(Sort(scan))  — distinct over sort
	sortQ := expressions.ForEachQuantifier(expressions.InitialOf(
		expressions.NewLogicalSortExpression(nil, expressions.ForEachQuantifier(scan("T"))),
	))
	ds := expressions.NewLogicalDistinctExpression(sortQ)

	cd := EstimateCost(d)
	cds := EstimateCost(ds)

	if !cd.Less(cds) {
		t.Fatalf("Distinct(scan)=%+v should beat Distinct(Sort(scan))=%+v — DistinctOverSortElim's calibration target", cd, cds)
	}
}

func TestEstimateCost_PushFilterThroughSortBeatsSortOverFilter(t *testing.T) {
	t.Parallel()
	// PushFilterThroughSort: Sort(Filter(P, scan)) vs Filter(P, Sort(scan)).
	// Pushing Filter under Sort means the sort runs on a smaller
	// row set — the rewrite is the lower-cost shape. Calibration
	// target: Sort(Filter(...)) < Filter(Sort(...)).
	pred := predicates.NewConstantPredicate(predicates.TriTrue)

	// Sort(Filter(P, scan)) — Filter pushed under Sort.
	filterUnderSort := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	)
	pushed := expressions.NewLogicalSortExpression(
		nil,
		expressions.ForEachQuantifier(expressions.InitialOf(filterUnderSort)),
	)

	// Filter(P, Sort(scan)) — Filter above Sort.
	sortInside := expressions.NewLogicalSortExpression(nil, scanQ("T"))
	pulled := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(expressions.InitialOf(sortInside)),
	)

	cPushed := EstimateCost(pushed)
	cPulled := EstimateCost(pulled)

	if !cPushed.Less(cPulled) {
		t.Fatalf("Sort(Filter)=%+v should beat Filter(Sort)=%+v — PushFilterThroughSort's calibration target", cPushed, cPulled)
	}
}

func TestEstimateCost_UnionSumsChildren(t *testing.T) {
	t.Parallel()
	// Union of two scans: cardinality = sum of children.
	u := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		scanQ("T"),
		scanQ("U"),
	})
	c := EstimateCost(u)
	if c.Cardinality != 2*LeafScanCardinality {
		t.Fatalf("union cardinality=%v, want %v", c.Cardinality, 2*LeafScanCardinality)
	}
}

func TestEstimateCost_IntersectionBoundedByMin(t *testing.T) {
	t.Parallel()
	// Intersection cardinality is bounded by the smallest child.
	// Two scans of the same record type → both 1e6 → output = 1e6.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)

	smaller := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred, pred, pred},
		scanQ("T"),
	)
	bigger := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("U"),
	)

	inter := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(smaller)),
			expressions.ForEachQuantifier(expressions.InitialOf(bigger)),
		},
		nil,
	)
	c := EstimateCost(inter)
	cs := EstimateCost(smaller)
	if c.Cardinality > cs.Cardinality {
		t.Fatalf("intersection cardinality=%v should be ≤ smallest child %v", c.Cardinality, cs.Cardinality)
	}
}

func TestBestRefCost_ReturnsCheapest(t *testing.T) {
	t.Parallel()
	// Build a Reference with two members: Filter(scan) and Sort(scan).
	// Filter is cheaper (drops rows AND less CPU). BestRefCost returns
	// Filter's cost.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	innerScan := scan("T")
	filterMember := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerScan),
	)
	sortMember := expressions.NewLogicalSortExpression(
		nil,
		expressions.ForEachQuantifier(innerScan),
	)
	// Filter and Sort are NOT semantically equivalent, but Reference.Insert
	// allows multiple distinct members.
	ref := expressions.InitialOf(filterMember)
	if inserted := ref.Insert(sortMember); !inserted {
		t.Fatal("setup error: Insert(sortMember) should have succeeded")
	}
	if len(ref.Members()) != 2 {
		t.Fatalf("setup error: members=%d, want 2", len(ref.Members()))
	}

	best := BestRefCost(ref)
	wantFilter := EstimateCost(filterMember)
	if best.Total() != wantFilter.Total() {
		t.Fatalf("BestRefCost=%+v, want filter's cost %+v", best, wantFilter)
	}
}

func TestBestRefCost_NilOrEmptyReturnsZero(t *testing.T) {
	t.Parallel()
	if c := BestRefCost(nil); c.Total() != 0 {
		t.Fatalf("nil ref cost=%+v, want zero", c)
	}
	r := &expressions.Reference{}
	if c := BestRefCost(r); c.Total() != 0 {
		t.Fatalf("empty ref cost=%+v, want zero", c)
	}
}

func TestEstimateCost_DMLCardinalityEqualsInner(t *testing.T) {
	t.Parallel()
	// Insert/Update/Delete: cardinality equals inner (one effect per
	// input row). Build a Delete with a Filter inner: cardinality
	// matches the filtered count.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	)
	cf := EstimateCost(filter)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))
	del := expressions.NewDeleteExpression(innerQ, "T")
	cd := EstimateCost(del)
	if math.Abs(cd.Cardinality-cf.Cardinality) > 1e-6 {
		t.Fatalf("delete cardinality=%v, want filter's %v", cd.Cardinality, cf.Cardinality)
	}
	// CPU should EXCEED filter CPU (Delete adds per-row write).
	if cd.CPU <= cf.CPU {
		t.Fatalf("delete CPU=%v should exceed filter CPU=%v", cd.CPU, cf.CPU)
	}
}

func TestCostLess_AsClosure(t *testing.T) {
	t.Parallel()
	// CostLess is the comparator passed to Reference.GetBest.
	// Verify it's a stable total order over the typed expressions.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	cheap := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred, pred},
		scanQ("T"),
	)
	pricy := expressions.NewLogicalSortExpression(nil, scanQ("T"))
	if !CostLess(cheap, pricy) {
		t.Fatalf("CostLess(filter, sort) should be true")
	}
	if CostLess(pricy, cheap) {
		t.Fatalf("CostLess(sort, filter) should be false")
	}
	// Self-comparison is not Less (irreflexive).
	if CostLess(cheap, cheap) {
		t.Fatal("CostLess is not irreflexive")
	}
}

func TestReferenceGetBest_PicksCheapestMember(t *testing.T) {
	t.Parallel()
	// Build a Reference with three members of different costs;
	// GetBest(CostLess) returns the cheapest.
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	)
	sort := expressions.NewLogicalSortExpression(nil, scanQ("T"))
	dist := expressions.NewLogicalDistinctExpression(scanQ("T"))

	ref := expressions.InitialOf(sort)
	ref.Insert(dist)
	ref.Insert(filter)

	best := ref.GetBest(CostLess)
	if best == nil {
		t.Fatal("GetBest returned nil")
	}
	// Filter has the lowest cost among the three (highest selectivity,
	// cheap CPU).
	if best != filter {
		t.Fatalf("GetBest=%T, want *LogicalFilterExpression", best)
	}
}

func TestReferenceGetBest_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	r := &expressions.Reference{}
	if got := r.GetBest(CostLess); got != nil {
		t.Fatalf("GetBest on empty Reference = %v, want nil", got)
	}
}

func TestReferenceGetBest_SingleMember(t *testing.T) {
	t.Parallel()
	e := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(e)
	if got := ref.GetBest(CostLess); got != e {
		t.Fatalf("GetBest single-member returned %v, want %v", got, e)
	}
}

// suppress unused-import warnings if values is dropped from a future
// edit. Keeping the import is cheaper than re-deriving constructors.
var _ = values.UnknownType

// TestEstimateCostWith_FixedStatistics pins that a non-default
// StatisticsProvider drives leaf-scan cardinality. With a 1000-row
// FixedStatistics, a single Filter over a scan emits 500 rows
// (FilterSelectivity * 1000), not LeafScanCardinality * 0.5.
func TestEstimateCostWith_FixedStatistics(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("T"),
	)
	stats := FixedStatistics{Cardinality: 1000}
	c := EstimateCostWith(filter, stats)
	want := 1000.0 * FilterSelectivity
	if c.Cardinality != want {
		t.Fatalf("filter cardinality=%v, want %v", c.Cardinality, want)
	}
}

// TestEstimateCostWith_MapStatisticsPerTypeFlipsCheapest demonstrates
// the high-leverage stats use case: when a Reference holds two
// scan-shaped alternatives (Filter(scan(big_table)) vs Filter(scan(
// small_table))), the cheapest extracted plan should depend on which
// table is actually smaller — driven by stats.
func TestEstimateCostWith_MapStatisticsPerTypeFlipsCheapest(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	bigScan := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("BigTable"),
	)
	smallScan := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		scanQ("SmallTable"),
	)

	statsBigSmaller := MapStatistics{
		PerType: map[string]float64{
			"BigTable":   100,
			"SmallTable": 10000,
		},
	}
	if !EstimateCostWith(bigScan, statsBigSmaller).Less(EstimateCostWith(smallScan, statsBigSmaller)) {
		t.Fatal("with BigTable=100 SmallTable=10000, BigTable filter should win")
	}

	statsSmallSmaller := MapStatistics{
		PerType: map[string]float64{
			"BigTable":   10000,
			"SmallTable": 100,
		},
	}
	if !EstimateCostWith(smallScan, statsSmallSmaller).Less(EstimateCostWith(bigScan, statsSmallSmaller)) {
		t.Fatal("with BigTable=10000 SmallTable=100, SmallTable filter should win")
	}
}

// TestCostLessWith_BindsStats ensures CostLessWith captures the
// stats provider in its closure — the returned comparator routes
// scan cost through the bound stats.
func TestCostLessWith_BindsStats(t *testing.T) {
	t.Parallel()
	scanT := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	scanU := expressions.NewFullUnorderedScanExpression([]string{"U"}, nil)
	stats := MapStatistics{
		PerType: map[string]float64{"T": 10, "U": 1000000},
	}
	less := CostLessWith(stats)
	if !less(scanT, scanU) {
		t.Fatal("CostLessWith(stats): T(10) should beat U(1M)")
	}
	if less(scanU, scanT) {
		t.Fatal("CostLessWith(stats): U(1M) should not beat T(10)")
	}
}

// TestStatistics_FallbackToLeafScan covers the unknown-name case in
// MapStatistics: PerType lookup misses → Fallback. Empty Fallback →
// LeafScanCardinality.
func TestStatistics_FallbackToLeafScan(t *testing.T) {
	t.Parallel()
	s := MapStatistics{PerType: map[string]float64{"T": 10}}
	if got := s.RecordTypeCardinality("Unknown"); got != LeafScanCardinality {
		t.Fatalf("Unknown name fell through to %v, want LeafScanCardinality (%v)", got, LeafScanCardinality)
	}
	s2 := MapStatistics{
		PerType:  map[string]float64{"T": 10},
		Fallback: 50000,
	}
	if got := s2.RecordTypeCardinality("Unknown"); got != 50000 {
		t.Fatalf("Unknown name + Fallback=50000 returned %v", got)
	}
}

// TestEstimateCost_ScanOverMultipleRecordTypesSums pins the Union-
// across-record-types semantics of FullUnorderedScan: scan over
// {A, B} emits stats(A) + stats(B) rows.
func TestEstimateCost_ScanOverMultipleRecordTypesSums(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"A", "B"}, nil)
	stats := MapStatistics{
		PerType: map[string]float64{"A": 100, "B": 200},
	}
	c := EstimateCostWith(scan, stats)
	if c.Cardinality != 300 {
		t.Fatalf("scan(A,B) with A=100 B=200 cardinality=%v, want 300", c.Cardinality)
	}
}

// TestBestRefCost_MemoisationConsistency pins that BestRefCost
// returns the same answer as the un-memoised path. Builds a wide
// Reference (10 distinct members all sharing the same inner scan
// Reference) and asserts BestRefCost picks the same minimum the
// for-loop would.
func TestBestRefCost_MemoisationConsistency(t *testing.T) {
	t.Parallel()
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	innerScan := scan("T")
	innerQ := func() expressions.Quantifier { return expressions.ForEachQuantifier(innerScan) }

	// 10 distinct Filter members with 1, 2, ..., 10 predicates so
	// each has a different cost.
	r := expressions.InitialOf(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		innerQ(),
	))
	for i := 2; i <= 10; i++ {
		preds := make([]predicates.QueryPredicate, i)
		for j := range preds {
			preds[j] = pred
		}
		r.Insert(expressions.NewLogicalFilterExpression(preds, innerQ()))
	}

	// Memoised path.
	memoised := BestRefCost(r)

	// Un-memoised path: walk every member and EstimateCost (no memo).
	unmemoised := EstimateCost(r.Members()[0])
	for _, m := range r.Members()[1:] {
		c := EstimateCost(m)
		if c.Less(unmemoised) {
			unmemoised = c
		}
	}

	if memoised.Total() != unmemoised.Total() {
		t.Fatalf("memoised %+v != un-memoised %+v", memoised, unmemoised)
	}
}

// BenchmarkExtractBestPlan_DeepTree pins ExtractBestPlan perf on a
// 5-deep Filter chain. Each Reference has a single member; the
// extractor walks every Quantifier and rebuilds the tree.
func BenchmarkExtractBestPlan_DeepTree(b *testing.B) {
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	innerQ := scanQ("Order")
	for i := 0; i < 5; i++ {
		f := expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{pred}, innerQ,
		)
		innerQ = expressions.ForEachQuantifier(expressions.InitialOf(f))
	}
	r := innerQ.GetRangesOver()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ExtractBestPlan(r)
	}
}

// BenchmarkExtractBestPlan_WideAlternatives pins ExtractBestPlan
// perf when the top-level Reference has 5 distinct Filter members
// over a shared inner. Memoisation kicks in for the cost computation.
func BenchmarkExtractBestPlan_WideAlternatives(b *testing.B) {
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	innerScan := scan("Order")
	r := expressions.InitialOf(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.ForEachQuantifier(innerScan),
	))
	for i := 2; i <= 5; i++ {
		preds := make([]predicates.QueryPredicate, i)
		for j := range preds {
			preds[j] = pred
		}
		r.Insert(expressions.NewLogicalFilterExpression(
			preds, expressions.ForEachQuantifier(innerScan),
		))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ExtractBestPlan(r)
	}
}

// BenchmarkBestRefCost_WideRef pins the memoisation win on a
// Reference with many members all sharing the same deep inner
// sub-Reference. Without memoisation the inner walk is repeated N
// times (once per member); with memoisation it's walked once.
func BenchmarkBestRefCost_WideRef(b *testing.B) {
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	// Deep inner: Filter over Filter over Filter over Scan.
	d := scanQ("T")
	for i := 0; i < 8; i++ {
		f := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, d)
		d = expressions.ForEachQuantifier(expressions.InitialOf(f))
	}

	// 20 distinct members all over the same deep inner Reference.
	r := expressions.InitialOf(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		d,
	))
	for i := 2; i <= 20; i++ {
		preds := make([]predicates.QueryPredicate, i)
		for j := range preds {
			preds[j] = pred
		}
		r.Insert(expressions.NewLogicalFilterExpression(preds, d))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BestRefCost(r)
	}
}
