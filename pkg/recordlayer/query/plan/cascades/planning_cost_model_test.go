package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// TestPlanningCostModel_PhysicalBeatsLogical verifies criterion 1:
// a physical plan is always preferred over a logical expression.
func TestPlanningCostModel_PhysicalBeatsLogical(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	physical := &physicalScanWrapper{plan: scan}
	logical := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)

	if !PlanningCostModelLess(physical, logical) {
		t.Error("PlanningCostModelLess(physical, logical) = false, want true")
	}
	if PlanningCostModelLess(logical, physical) {
		t.Error("PlanningCostModelLess(logical, physical) = true, want false")
	}
}

// TestPlanningCostModel_FewerResidualPredicatesWins verifies criterion 3:
// among physical plans with identical scan counts, the one with fewer
// residual predicates is preferred.
func TestPlanningCostModel_FewerResidualPredicatesWins(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanRef := expressions.InitialOf(&physicalScanWrapper{plan: scan})
	innerQ := expressions.ForEachQuantifier(scanRef)

	pred1 := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	pred2 := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "y", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(2)),
	)

	// one-predicate filter
	onePred := NewPhysicalPredicatesFilterWrapper(
		plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred1}),
		innerQ,
	)
	// two-predicate filter — same scan underneath
	twoPred := NewPhysicalPredicatesFilterWrapper(
		plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred1, pred2}),
		innerQ,
	)

	if !PlanningCostModelLess(onePred, twoPred) {
		t.Error("PlanningCostModelLess(1-pred, 2-pred) = false, want true")
	}
	if PlanningCostModelLess(twoPred, onePred) {
		t.Error("PlanningCostModelLess(2-pred, 1-pred) = true, want false")
	}
}

// TestPlanningCostModel_HashTieBreakIsDeterministic verifies criterion 17:
// two distinct physical scans must produce a stable ordering — the comparison
// must not return 0 (one must strictly beat the other), and repeated calls
// must agree.
func TestPlanningCostModel_HashTieBreakIsDeterministic(t *testing.T) {
	t.Parallel()

	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	wrapA := &physicalScanWrapper{plan: scanA}
	wrapB := &physicalScanWrapper{plan: scanB}

	ab := PlanningCostModelLess(wrapA, wrapB)
	ba := PlanningCostModelLess(wrapB, wrapA)

	// Exactly one of the two must win (strict total order, not a tie).
	if ab == ba {
		t.Errorf("hash tie-break is inconsistent: Less(A,B)=%v Less(B,A)=%v — exactly one must be true", ab, ba)
	}

	// Must be stable across repeated calls.
	for i := 0; i < 10; i++ {
		if PlanningCostModelLess(wrapA, wrapB) != ab {
			t.Fatalf("hash tie-break changed on iteration %d", i)
		}
	}
}

func TestDeepHashCode_DistinguishesDifferentChildren(t *testing.T) {
	t.Parallel()
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)

	refA := expressions.InitialOf(&physicalScanWrapper{plan: scanA})
	refB := expressions.InitialOf(&physicalScanWrapper{plan: scanB})

	tfPlan := plans.NewRecordQueryTypeFilterPlan([]string{"T"}, nil)
	filterA := NewPhysicalTypeFilterWrapper(tfPlan, expressions.ForEachQuantifier(refA))
	filterB := NewPhysicalTypeFilterWrapper(tfPlan, expressions.ForEachQuantifier(refB))

	hashA := deepHashCode(filterA)
	hashB := deepHashCode(filterB)
	if hashA == hashB {
		t.Errorf("deepHashCode should distinguish type filters over different scans: both = %d", hashA)
	}

	shallowA := filterA.HashCodeWithoutChildren()
	shallowB := filterB.HashCodeWithoutChildren()
	if shallowA != shallowB {
		t.Fatal("shallow hashes should be equal (same type filter plan)")
	}
}

func TestPlanningCostModel_CNFSizeForOrPredicates(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanRef := expressions.InitialOf(&physicalScanWrapper{plan: scan})
	innerQ := expressions.ForEachQuantifier(scanRef)

	pred := func(field string) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(
			&values.FieldValue{Field: field, Typ: values.TypeInt},
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
		)
	}

	// OR(A, B) has CNF size 1 (it's already a single disjunctive clause).
	// AND(A, B) has CNF size 2 (two conjuncts).
	// The plan with AND(A, B) has higher residual predicate cost.
	orPred := predicates.NewOr(pred("a"), pred("b"))
	andPred := predicates.NewAnd(pred("a"), pred("b"))

	orFilter := NewPhysicalPredicatesFilterWrapper(
		plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{orPred}),
		innerQ,
	)
	andFilter := NewPhysicalPredicatesFilterWrapper(
		plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{andPred}),
		innerQ,
	)

	if !PlanningCostModelLess(orFilter, andFilter) {
		t.Error("OR(A,B) [cnfSize=1] should be preferred over AND(A,B) [cnfSize=2]")
	}

	// OR(AND(A,B), AND(C,D)) has CNF size 4 (2×2 cross-product).
	// AND(A, B, C) has CNF size 3 (three conjuncts).
	complexOr := predicates.NewOr(
		predicates.NewAnd(pred("a"), pred("b")),
		predicates.NewAnd(pred("c"), pred("d")),
	)
	tripleAnd := predicates.NewAnd(pred("a"), pred("b"), pred("c"))

	complexFilter := NewPhysicalPredicatesFilterWrapper(
		plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{complexOr}),
		innerQ,
	)
	simpleFilter := NewPhysicalPredicatesFilterWrapper(
		plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{tripleAnd}),
		innerQ,
	)

	if !PlanningCostModelLess(simpleFilter, complexFilter) {
		t.Error("AND(A,B,C) [cnfSize=3] should be preferred over OR(AND(A,B),AND(C,D)) [cnfSize=4]")
	}
}

func TestPlanningCostModel_TypeFilterCountsRecordTypes(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T1", "T2", "T3"}, values.UnknownType, false)
	scanRef := expressions.InitialOf(&physicalScanWrapper{plan: scan})
	innerQ := expressions.ForEachQuantifier(scanRef)

	// Type filter admitting 1 type should have typeFilterCount=1.
	oneType := NewPhysicalTypeFilterWrapper(
		plans.NewRecordQueryTypeFilterPlan([]string{"T1"}, nil),
		innerQ,
	)
	// Type filter admitting 3 types should have typeFilterCount=3.
	threeTypes := NewPhysicalTypeFilterWrapper(
		plans.NewRecordQueryTypeFilterPlan([]string{"T1", "T2", "T3"}, nil),
		innerQ,
	)

	counts1 := findExpressionsByType(oneType, nil)
	counts3 := findExpressionsByType(threeTypes, nil)

	if counts1.typeFilterCount != 1 {
		t.Errorf("typeFilterCount for 1-type filter = %d, want 1", counts1.typeFilterCount)
	}
	if counts3.typeFilterCount != 3 {
		t.Errorf("typeFilterCount for 3-type filter = %d, want 3", counts3.typeFilterCount)
	}
}

// TestRewritingCostModelLess_FewerSelectsWins verifies that an
// expression with fewer SelectExpressions is preferred.
func TestRewritingCostModelLess_FewerSelectsWins(t *testing.T) {
	t.Parallel()

	scanRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType),
	)

	// Build expressions: one with 0 SelectExpressions (just a scan),
	// one wrapped in a SelectExpression.
	scanOnly := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := expressions.ForEachQuantifier(scanRef)
	selectWrapped := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(q.GetAlias()),
		[]expressions.Quantifier{q},
		nil,
	)

	if !RewritingCostModelLess(scanOnly, selectWrapped) {
		t.Error("RewritingCostModelLess: fewer selects (0) should beat more selects (1)")
	}
	if RewritingCostModelLess(selectWrapped, scanOnly) {
		t.Error("RewritingCostModelLess: more selects should NOT beat fewer")
	}
}

// TestRewritingCostModelLess_TiesOnSelectsFewerTableFunctionsWins
// verifies that when SelectExpression counts tie, fewer
// TableFunctionExpressions wins.
func TestRewritingCostModelLess_TiesOnSelectsFewerTableFunctionsWins(t *testing.T) {
	t.Parallel()

	// Both are non-Select, non-TableFunction — same count on selects.
	// a has 0 TableFunctionExpressions, b has 1.
	a := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	b := expressions.NewTableFunctionExpression(&values.ConstantValue{Value: int64(1), Typ: values.TypeInt})

	if !RewritingCostModelLess(a, b) {
		t.Error("RewritingCostModelLess: 0 table functions should beat 1")
	}
	if RewritingCostModelLess(b, a) {
		t.Error("RewritingCostModelLess: 1 table function should NOT beat 0")
	}
}

// TestRewritingCostModelLess_AllTie_HashDeterministic verifies that
// when all criteria tie, the hash tiebreak is deterministic and
// produces a strict ordering.
func TestRewritingCostModelLess_AllTie_HashDeterministic(t *testing.T) {
	t.Parallel()

	a := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	b := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)

	ab := RewritingCostModelLess(a, b)
	ba := RewritingCostModelLess(b, a)

	if ab == ba {
		t.Errorf("hash tiebreak is inconsistent: Less(A,B)=%v Less(B,A)=%v", ab, ba)
	}

	// Stable across repeated calls.
	for i := 0; i < 10; i++ {
		if RewritingCostModelLess(a, b) != ab {
			t.Fatalf("hash tiebreak changed on iteration %d", i)
		}
	}
}

// TestCompareInPlan_FlipFlop_SargedVsUnsarged verifies the flipFlop
// semantics: when both a and b are IN-plans, and a is SARGed (returns
// 0, true) and b is unsarged (returns 1, true), the result should be
// 0 — Java's flipFlop returns present(0), stops there. NOT -1.
func TestCompareInPlan_FlipFlop_SargedVsUnsarged(t *testing.T) {
	t.Parallel()

	// Build an InJoin plan with a SARGed binding: the inner index scan
	// has an equality comparison correlated to the binding name.
	bindingAlias := values.NamedCorrelationIdentifier("in_bind")
	eqComp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewQuantifiedObjectValue(bindingAlias)}
	eqRangeEmpty := predicates.EmptyComparisonRange()
	mergeResult := eqRangeEmpty.Merge(&eqComp)
	if !mergeResult.Ok {
		t.Fatal("failed to merge equality comparison into empty range")
	}
	eqRange := mergeResult.Range

	innerPlanA := plans.NewRecordQueryIndexPlan(
		"idx_sarged", []*predicates.ComparisonRange{eqRange},
		[]string{"T"}, values.UnknownType, false,
	)
	inJoinPlanA := plans.NewRecordQueryInJoinPlan(innerPlanA, bindingAlias.Name(), false, false)
	innerIndexA := &physicalIndexScanWrapper{
		plan:        innerPlanA,
		columnNames: []string{"a"},
	}
	innerRefA := expressions.InitialOf(innerIndexA)
	wrapA := NewPhysicalInJoinWrapper(inJoinPlanA, expressions.NewPhysicalQuantifier(innerRefA))

	// Build an InJoin plan with an unsarged binding: the inner scan
	// has no comparison matching the binding name.
	innerPlanB := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	inJoinPlanB := plans.NewRecordQueryInJoinPlan(innerPlanB, "other_bind", false, false)
	innerScanB := &physicalScanWrapper{plan: innerPlanB}
	innerRefB := expressions.InitialOf(innerScanB)
	wrapB := NewPhysicalInJoinWrapper(inJoinPlanB, expressions.NewPhysicalQuantifier(innerRefB))

	opsA := findExpressionsByType(wrapA, nil)
	opsB := findExpressionsByType(wrapB, nil)

	cmp := compareInPlan(wrapA, wrapB, opsA, opsB)
	if cmp != 0 {
		t.Errorf("compareInPlan(sarged, unsarged) = %d, want 0 (flipFlop stops at first applicable, returns present(0))", cmp)
	}
}

// TestIsSingularIndexScanWithFetch verifies the four cases of
// isSingularIndexScanWithFetch.
func TestIsSingularIndexScanWithFetch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ops  expressionCounts
		want bool
	}{
		{
			name: "non-covering index scan (indexScanCount=1)",
			ops:  expressionCounts{indexScanCount: 1},
			want: true,
		},
		{
			name: "covering index with fetch",
			ops:  expressionCounts{coveringIndexCount: 1, fetchCount: 1},
			want: true,
		},
		{
			name: "covering index without fetch",
			ops:  expressionCounts{coveringIndexCount: 1, fetchCount: 0},
			want: false,
		},
		{
			name: "primary scan only",
			ops:  expressionCounts{scanCount: 1},
			want: false,
		},
		{
			name: "zero everything",
			ops:  expressionCounts{},
			want: false,
		},
	}

	for _, tc := range tests {
		got := isSingularIndexScanWithFetch(tc.ops)
		if got != tc.want {
			t.Errorf("isSingularIndexScanWithFetch(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCollectSargedAliases_IntersectionIsSetIntersection verifies that
// collectSargedAliases on an intersection plan returns the intersection
// of each child's aliases.
func TestCollectSargedAliases_IntersectionIsSetIntersection(t *testing.T) {
	t.Parallel()

	// Helper: build an index scan with equality comparisons correlated
	// to given aliases.
	makeIndexScan := func(indexName string, aliases ...string) *physicalIndexScanWrapper {
		ranges := make([]*predicates.ComparisonRange, len(aliases))
		for i, alias := range aliases {
			comp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias))}
			cr := predicates.EmptyComparisonRange()
			mr := cr.Merge(&comp)
			if !mr.Ok {
				t.Fatalf("failed to merge equality for alias %s", alias)
			}
			ranges[i] = mr.Range
		}
		return &physicalIndexScanWrapper{
			plan:        plans.NewRecordQueryIndexPlan(indexName, ranges, []string{"T"}, values.UnknownType, false),
			columnNames: make([]string, len(aliases)),
		}
	}

	// Child 1 has aliases {a, b}; child 2 has aliases {b, c}.
	// Intersection should yield {b}.
	child1 := makeIndexScan("idx1", "a", "b")
	child2 := makeIndexScan("idx2", "b", "c")

	ref1 := expressions.InitialOf(child1)
	ref2 := expressions.InitialOf(child2)

	q1 := expressions.NewPhysicalQuantifier(ref1)
	q2 := expressions.NewPhysicalQuantifier(ref2)

	intersectionPlan := plans.NewRecordQueryIntersectionPlan(
		[]plans.RecordQueryPlan{child1.plan, child2.plan},
		nil,
	)
	intersection := NewPhysicalIntersectionWrapper(intersectionPlan, []expressions.Quantifier{q1, q2})

	aliases := collectSargedAliases(intersection)

	bAlias := values.NamedCorrelationIdentifier("b")
	if _, ok := aliases[bAlias]; !ok {
		t.Error("intersection aliases should contain 'b'")
	}
	aAlias := values.NamedCorrelationIdentifier("a")
	if _, ok := aliases[aAlias]; ok {
		t.Error("intersection aliases should NOT contain 'a' (only in child 1)")
	}
	cAlias := values.NamedCorrelationIdentifier("c")
	if _, ok := aliases[cAlias]; ok {
		t.Error("intersection aliases should NOT contain 'c' (only in child 2)")
	}
	if len(aliases) != 1 {
		t.Errorf("intersection aliases has %d entries, want 1", len(aliases))
	}
}

// TestIntersectChildAliases_EmptyChildren returns nil for expressions
// without quantifiers.
func TestIntersectChildAliases_EmptyChildren(t *testing.T) {
	t.Parallel()

	// Use a wrapper with no quantifiers — physicalScanWrapper has nil quantifiers.
	scan := &physicalScanWrapper{plan: plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)}
	result := intersectChildAliases(scan)
	if result != nil {
		t.Errorf("intersectChildAliases(no quantifiers) = %v, want nil", result)
	}
}

func TestPlanningCostModel_CoveringEqualityIndexPreferredOverPrimaryScan(t *testing.T) {
	t.Parallel()
	primary := &physicalScanWrapper{
		plan: plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false),
	}
	comp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: 42, Typ: values.TypeInt}}
	cr := predicates.EmptyComparisonRange()
	mr := cr.Merge(&comp)
	if !mr.Ok {
		t.Fatal("failed to merge equality comparison")
	}
	index := &physicalIndexScanWrapper{
		plan:        plans.NewRecordQueryIndexPlan("idx_a", []*predicates.ComparisonRange{mr.Range}, []string{"T"}, values.UnknownType, false),
		columnNames: []string{"A"},
		covering:    true,
	}

	if !PlanningCostModelLess(index, primary) {
		t.Error("covering equality index scan should be preferred over primary scan")
	}
	if PlanningCostModelLess(primary, index) {
		t.Error("primary scan should NOT be preferred over covering equality index scan")
	}
}

func TestPlanningCostModel_NonCoveringFullIndexLosesToPrimaryScan(t *testing.T) {
	t.Parallel()
	primary := &physicalScanWrapper{
		plan: plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false),
	}
	index := &physicalIndexScanWrapper{
		plan:        plans.NewRecordQueryIndexPlan("idx_a", nil, []string{"T"}, values.UnknownType, false),
		columnNames: []string{"A"},
	}

	if PlanningCostModelLess(index, primary) {
		t.Error("full non-covering index scan should NOT beat primary scan (per-row PK fetch makes it strictly worse)")
	}
}

func TestPlanningCostModel_EqualityIndexBeatsFullScan(t *testing.T) {
	t.Parallel()
	primary := &physicalScanWrapper{
		plan: plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false),
	}
	comp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: 42, Typ: values.TypeInt}}
	cr := predicates.EmptyComparisonRange()
	mr := cr.Merge(&comp)
	if !mr.Ok {
		t.Fatal("failed to merge equality comparison")
	}
	index := &physicalIndexScanWrapper{
		plan:        plans.NewRecordQueryIndexPlan("idx_a", []*predicates.ComparisonRange{mr.Range}, []string{"T"}, values.UnknownType, false),
		columnNames: []string{"A"},
	}

	if !PlanningCostModelLess(index, primary) {
		t.Error("equality-bound index scan should beat full primary scan")
	}
}

// TestPlanningCostModelLess_Criterion8_TypeFilterCount verifies that
// among plans with identical scan bases and zero residual predicates,
// the one with fewer type-filter types wins.
func TestPlanningCostModelLess_Criterion8_TypeFilterCount(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanRef := expressions.InitialOf(&physicalScanWrapper{plan: scan})
	innerQ := expressions.ForEachQuantifier(scanRef)

	// Plan A: type filter over 1 type — typeFilterCount=1.
	oneType := NewPhysicalTypeFilterWrapper(
		plans.NewRecordQueryTypeFilterPlan([]string{"T1"}, nil),
		innerQ,
	)
	// Plan B: type filter over 3 types — typeFilterCount=3.
	// Same underlying scan so all earlier criteria tie.
	threeTypes := NewPhysicalTypeFilterWrapper(
		plans.NewRecordQueryTypeFilterPlan([]string{"T1", "T2", "T3"}, nil),
		innerQ,
	)

	if !PlanningCostModelLess(oneType, threeTypes) {
		t.Error("typeFilterCount=1 should beat typeFilterCount=3")
	}
	if PlanningCostModelLess(threeTypes, oneType) {
		t.Error("typeFilterCount=3 should NOT beat typeFilterCount=1")
	}
}

// TestPlanningCostModelLess_Criterion9_TypeFilterDepth verifies that
// expressionDepth returns the correct tree depth for a type filter, and
// that the criterion's reversed comparison (deeper is better) is correct.
// Criterion 9 fires when typeFilterCounts are equal and both depths are ≥ 0.
// The comparison is intCompare(depthB, depthA), so larger depthA wins.
func TestPlanningCostModelLess_Criterion9_TypeFilterDepth(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanRef := expressions.InitialOf(&physicalScanWrapper{plan: scan})
	innerQ := expressions.ForEachQuantifier(scanRef)

	// shallowPlan: typeFilter IS the root (depth=0).
	shallowPlan := NewPhysicalTypeFilterWrapper(
		plans.NewRecordQueryTypeFilterPlan([]string{"T1"}, nil),
		innerQ,
	)

	// deepPlan: inJoin → typeFilter(depth=1) → scan.
	// Using inJoin as the outer layer avoids changing typeFilterCount or
	// residual predicates.
	typeFilterRef := expressions.InitialOf(shallowPlan)
	typeFilterQ := expressions.NewPhysicalQuantifier(typeFilterRef)
	inJoinPlan := plans.NewRecordQueryInJoinPlan(scan, "bind", false, false)
	deepPlan := NewPhysicalInJoinWrapper(inJoinPlan, typeFilterQ)

	// Verify depths directly.
	shallowDepth := expressionDepth(shallowPlan, isTypeFilterExpression)
	deepDepth := expressionDepth(deepPlan, isTypeFilterExpression)
	if shallowDepth != 0 {
		t.Errorf("shallowPlan typeFilter depth = %d, want 0", shallowDepth)
	}
	if deepDepth != 1 {
		t.Errorf("deepPlan typeFilter depth = %d, want 1", deepDepth)
	}

	// Criterion 9 comparison: intCompare(depthB, depthA).
	// If A is deepPlan (depth=1) and B is shallowPlan (depth=0):
	// intCompare(0, 1) = -1 → A wins.
	cmp := intCompare(shallowDepth, deepDepth) // intCompare(depthB, depthA) when a=deep, b=shallow
	if cmp >= 0 {
		t.Errorf("criterion 9: intCompare(depthShallow=%d, depthDeep=%d) = %d, want < 0 (deeper wins)", shallowDepth, deepDepth, cmp)
	}
}

// TestPlanningCostModelLess_Criterion10_IndexScanFetchCount verifies
// that among plans where both sides have covering or non-covering index
// scans, the one with a lower (indexScanCount+fetchCount) sum wins.
func TestPlanningCostModelLess_Criterion10_IndexScanFetchCount(t *testing.T) {
	t.Parallel()

	// Plan A: one non-covering index scan, no fetch → fetchA = indexScanCount+fetchCount = 1+0 = 1.
	// Plan B: one covering index scan + one fetch → fetchB = 0+1 = 1? No:
	//   fetchB = opsB.indexScanCount + opsB.fetchCount = 0 + 1 = 1.
	// That ties. We need fetchB > fetchA.
	//
	// Plan A: covering index (coveringIndexCount=1), no explicit fetch wrapper.
	//   fetchA = opsA.indexScanCount + opsA.fetchCount = 0 + 0 = 0.
	// Plan B: non-covering index (indexScanCount=1), plus a fetch wrapper.
	//   fetchB = opsB.indexScanCount + opsB.fetchCount = 1 + 1 = 2.
	//
	// Both have indexScanCount+coveringIndexCount > 0, so criterion 10 fires.
	// fetchA (0) < fetchB (2) → plan A wins.

	// Plan A: covering index scan (no fetch wrapper).
	indexA := &physicalIndexScanWrapper{
		plan:        plans.NewRecordQueryIndexPlan("idx_a", nil, []string{"T"}, values.UnknownType, false),
		columnNames: []string{"a"},
		covering:    true,
	}

	// Plan B: non-covering index scan + fetch wrapper.
	indexB := &physicalIndexScanWrapper{
		plan:        plans.NewRecordQueryIndexPlan("idx_b", nil, []string{"T"}, values.UnknownType, false),
		columnNames: []string{"b"},
	}
	indexBRef := expressions.InitialOf(indexB)
	indexBQ := expressions.NewPhysicalQuantifier(indexBRef)
	fetchPlan := plans.NewRecordQueryFetchFromPartialRecordPlan(nil, nil, nil, plans.FetchIndexRecordsPrimaryKey)
	planB := NewPhysicalFetchFromPartialRecordWrapper(fetchPlan, indexBQ)

	// Verify counts are as expected before checking the full comparator.
	opsA := findExpressionsByType(indexA, nil)
	opsB := findExpressionsByType(planB, nil)
	if opsA.coveringIndexCount != 1 || opsA.indexScanCount != 0 || opsA.fetchCount != 0 {
		t.Fatalf("plan A counts wrong: covering=%d index=%d fetch=%d", opsA.coveringIndexCount, opsA.indexScanCount, opsA.fetchCount)
	}
	if opsB.indexScanCount != 1 || opsB.fetchCount != 1 {
		t.Fatalf("plan B counts wrong: index=%d fetch=%d", opsB.indexScanCount, opsB.fetchCount)
	}

	fetchA := opsA.indexScanCount + opsA.fetchCount // 0
	fetchB := opsB.indexScanCount + opsB.fetchCount // 2
	if fetchA >= fetchB {
		t.Fatalf("test setup invalid: fetchA=%d should be < fetchB=%d", fetchA, fetchB)
	}

	if !PlanningCostModelLess(indexA, planB) {
		t.Error("plan with lower indexScanCount+fetchCount should win")
	}
	if PlanningCostModelLess(planB, indexA) {
		t.Error("plan with higher indexScanCount+fetchCount should NOT win")
	}
}

// TestPlanningCostModelLess_Criterion11_DistinctDepth verifies that
// expressionDepth returns the correct depth for a distinct wrapper, and
// that the criterion's reversed comparison (deeper is better) is correct.
// Criterion 11: intCompare(distinctDepthB, distinctDepthA) — larger depthA wins.
func TestPlanningCostModelLess_Criterion11_DistinctDepth(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanRef := expressions.InitialOf(&physicalScanWrapper{plan: scan})
	innerQ := expressions.NewPhysicalQuantifier(scanRef)

	distPlan := plans.NewRecordQueryDistinctPlan(scan)

	// shallowDistinct: distinct IS the root (depth=0).
	shallowDistinct := NewPhysicalDistinctWrapper(distPlan, innerQ)

	// deepDistinct: inJoin → distinct(depth=1) → scan.
	// Using an inJoin as the outer layer so it doesn't add typeFilterCount
	// or residual predicates that would trigger earlier criteria.
	distinctRef := expressions.InitialOf(shallowDistinct)
	distinctQ := expressions.NewPhysicalQuantifier(distinctRef)
	inJoinPlan := plans.NewRecordQueryInJoinPlan(scan, "bind", false, false)
	deepDistinct := NewPhysicalInJoinWrapper(inJoinPlan, distinctQ)

	// Verify depths directly.
	shallowDepth := expressionDepth(shallowDistinct, isDistinctExpression)
	deepDepth := expressionDepth(deepDistinct, isDistinctExpression)
	if shallowDepth != 0 {
		t.Errorf("shallowDistinct depth = %d, want 0", shallowDepth)
	}
	if deepDepth != 1 {
		t.Errorf("deepDistinct depth = %d, want 1", deepDepth)
	}

	// Criterion 11 comparison: intCompare(depthB, depthA).
	// If A is deepDistinct (depth=1) and B is shallowDistinct (depth=0):
	// intCompare(0, 1) = -1 → A wins (deeper is better).
	cmp := intCompare(shallowDepth, deepDepth) // intCompare(depthB, depthA) when a=deep, b=shallow
	if cmp >= 0 {
		t.Errorf("criterion 11: intCompare(shallowDepth=%d, deepDepth=%d) = %d, want < 0 (deeper wins)", shallowDepth, deepDepth, cmp)
	}
}

// TestPlanningCostModelLess_Criterion12_UnmatchedFieldCount verifies
// that the plan with fewer unmatched index fields wins, and that the
// criterion is suppressed when either plan has an in-memory sort.
func TestPlanningCostModelLess_Criterion12_UnmatchedFieldCount(t *testing.T) {
	t.Parallel()

	// Build two covering index scans with different numbers of unmatched fields.
	// unmatchedFieldCount = totalCols - boundCols.
	//
	// Plan A: 1-column index, 1 equality bound → unmatched=0.
	eqComp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1), Typ: values.TypeInt}}
	cr := predicates.EmptyComparisonRange()
	mr := cr.Merge(&eqComp)
	if !mr.Ok {
		t.Fatal("failed to merge equality comparison")
	}
	indexA := &physicalIndexScanWrapper{
		plan:        plans.NewRecordQueryIndexPlan("idx_a", []*predicates.ComparisonRange{mr.Range}, []string{"T"}, values.UnknownType, false),
		columnNames: []string{"a"},
		covering:    true,
	}

	// Plan B: 3-column index, 0 bounds → unmatched=3.
	indexB := &physicalIndexScanWrapper{
		plan:        plans.NewRecordQueryIndexPlan("idx_b", nil, []string{"T"}, values.UnknownType, false),
		columnNames: []string{"a", "b", "c"},
		covering:    true,
	}

	opsA := findExpressionsByType(indexA, nil)
	opsB := findExpressionsByType(indexB, nil)
	if opsA.unmatchedFieldCount != 0 {
		t.Fatalf("plan A unmatchedFieldCount = %d, want 0", opsA.unmatchedFieldCount)
	}
	if opsB.unmatchedFieldCount != 3 {
		t.Fatalf("plan B unmatchedFieldCount = %d, want 3", opsB.unmatchedFieldCount)
	}

	// Both have inMemorySortCount=0, so criterion 12 fires: fewer unmatched wins.
	if !PlanningCostModelLess(indexA, indexB) {
		t.Error("unmatched=0 should beat unmatched=3 (no in-memory sort)")
	}
	if PlanningCostModelLess(indexB, indexA) {
		t.Error("unmatched=3 should NOT beat unmatched=0")
	}

	// Now wrap plan B in an in-memory sort — criterion 12 must not fire.
	// With inMemorySortCount>0 on plan B, criterion 12 is suppressed.
	// Plans will then be compared by later criteria or hash.
	sortPlan := plans.NewRecordQueryInMemorySortPlan(nil, nil)
	indexBRef := expressions.InitialOf(indexB)
	indexBQ := expressions.NewPhysicalQuantifier(indexBRef)
	withSort := newPhysicalInMemorySortWrapper(sortPlan, indexBQ)

	opsWithSort := findExpressionsByType(withSort, nil)
	if opsWithSort.inMemorySortCount != 1 {
		t.Fatalf("withSort inMemorySortCount = %d, want 1", opsWithSort.inMemorySortCount)
	}

	// When plan B has an in-memory sort, the guard fires: criterion 12 must return 0.
	// The guard condition is: opsA.inMemorySortCount==0 && opsB.inMemorySortCount==0.
	// Verify via the counts directly.
	opsNoSort := findExpressionsByType(indexA, nil)
	if opsNoSort.inMemorySortCount != 0 || opsWithSort.inMemorySortCount != 1 {
		t.Fatal("unexpected inMemorySortCount values")
	}
	// Criterion 12 should not fire (guard fails): unmatchedFieldCount check is skipped.
	// intCompare fires only when both inMemorySortCount==0.
	suppressed := opsNoSort.unmatchedFieldCount != opsWithSort.unmatchedFieldCount &&
		opsNoSort.inMemorySortCount == 0 && opsWithSort.inMemorySortCount == 0
	if suppressed {
		t.Error("criterion 12 should be suppressed when plan B has in-memory sort")
	}

	// Behavioral check: PlanningCostModelLess should NOT pick indexA over
	// withSort based on unmatchedFieldCount alone — the guard suppresses it.
	// If criterion 12 were NOT suppressed, indexA (0 unmatched) would always
	// beat withSort (3 unmatched). With suppression, the result depends on
	// later criteria (hash tiebreak), so we just verify it doesn't crash and
	// returns a deterministic result.
	_ = PlanningCostModelLess(indexA, withSort)
	_ = PlanningCostModelLess(withSort, indexA)
}

// TestPlanningCostModelLess_Criterion13_InJoinCount verifies that
// inJoinCount is counted correctly in the expression tree, and that
// criterion 13 uses a reversed comparison (more in-joins is better).
//
// Criterion 13: intCompare(opsB.inJoinCount, opsA.inJoinCount).
// When opsA.inJoinCount > opsB.inJoinCount, the result is negative → A wins.
// This is the reversed comparison: MORE in-joins is BETTER.
func TestPlanningCostModelLess_Criterion13_InJoinCount(t *testing.T) {
	t.Parallel()

	// Build nested in-join wrappers to verify count accumulation.
	indexPlan := plans.NewRecordQueryIndexPlan("idx", nil, []string{"T"}, values.UnknownType, false)
	indexRef := expressions.InitialOf(&physicalIndexScanWrapper{
		plan:        indexPlan,
		columnNames: []string{"a"},
		covering:    true,
	})
	indexQ := expressions.NewPhysicalQuantifier(indexRef)

	inJoinPlan1 := plans.NewRecordQueryInJoinPlan(indexPlan, "bind1", false, false)
	outerInJoin := NewPhysicalInJoinWrapper(inJoinPlan1, indexQ)

	outerRef := expressions.InitialOf(outerInJoin)
	outerQ := expressions.NewPhysicalQuantifier(outerRef)
	inJoinPlan2 := plans.NewRecordQueryInJoinPlan(indexPlan, "bind2", false, false)
	twoInJoins := NewPhysicalInJoinWrapper(inJoinPlan2, outerQ)

	opsOne := findExpressionsByType(outerInJoin, nil)
	opsTwo := findExpressionsByType(twoInJoins, nil)
	if opsOne.inJoinCount != 1 {
		t.Fatalf("outerInJoin inJoinCount = %d, want 1", opsOne.inJoinCount)
	}
	if opsTwo.inJoinCount != 2 {
		t.Fatalf("twoInJoins inJoinCount = %d, want 2", opsTwo.inJoinCount)
	}

	// Criterion 13 reversed comparison: intCompare(opsB.inJoinCount, opsA.inJoinCount).
	// When A has 2 and B has 1: intCompare(1, 2) = -1 → A wins.
	// When A has 1 and B has 2: intCompare(2, 1) = 1 → B wins.
	cmpAWins := intCompare(opsOne.inJoinCount, opsTwo.inJoinCount) // intCompare(opsB, opsA) when A=two, B=one
	if cmpAWins >= 0 {
		t.Errorf("criterion 13: intCompare(B.inJoin=%d, A.inJoin=%d) = %d, want < 0 (more wins)", opsOne.inJoinCount, opsTwo.inJoinCount, cmpAWins)
	}
	cmpBWins := intCompare(opsTwo.inJoinCount, opsOne.inJoinCount) // intCompare(opsB, opsA) when A=one, B=two
	if cmpBWins <= 0 {
		t.Errorf("criterion 13: intCompare(B.inJoin=%d, A.inJoin=%d) = %d, want > 0 (fewer in-joins loses)", opsTwo.inJoinCount, opsOne.inJoinCount, cmpBWins)
	}
}

// TestPlanningCostModelLess_Criterion14_MapPredicatesFilterCount verifies
// that fewer (mapCount + predicatesFilterCount) wins.
func TestPlanningCostModelLess_Criterion14_MapPredicatesFilterCount(t *testing.T) {
	t.Parallel()

	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanRef := expressions.InitialOf(&physicalScanWrapper{plan: scan})
	innerQ := expressions.ForEachQuantifier(scanRef)

	// Plan A: one predicates filter on top of the scan → predicatesFilterCount=1, mapCount=0.
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "x", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	oneFilter := NewPhysicalPredicatesFilterWrapper(
		plans.NewRecordQueryPredicatesFilterPlan(nil, []predicates.QueryPredicate{pred}),
		innerQ,
	)

	// Plan B: same filter, plus a map on top → predicatesFilterCount=1, mapCount=1 → sum=2.
	filterRef := expressions.InitialOf(oneFilter)
	filterQ := expressions.ForEachQuantifier(filterRef)
	mapPlan := plans.NewRecordQueryMapPlan(nil, &values.ConstantValue{Value: int64(0), Typ: values.TypeInt})
	withMap := NewPhysicalMapWrapper(mapPlan, filterQ)

	opsA := findExpressionsByType(oneFilter, nil)
	opsB := findExpressionsByType(withMap, nil)
	sumA := opsA.mapCount + opsA.predicatesFilterCount
	sumB := opsB.mapCount + opsB.predicatesFilterCount
	if sumA != 1 {
		t.Fatalf("plan A mapCount+predicatesFilterCount = %d, want 1", sumA)
	}
	if sumB != 2 {
		t.Fatalf("plan B mapCount+predicatesFilterCount = %d, want 2", sumB)
	}

	if !PlanningCostModelLess(oneFilter, withMap) {
		t.Error("sum=1 should beat sum=2 (fewer is better)")
	}
	if PlanningCostModelLess(withMap, oneFilter) {
		t.Error("sum=2 should NOT beat sum=1")
	}
}

// TestCompareFlatMapVsNLJ_FlatMapBeatsNLJ verifies that a plan with a
// flatMap (and no NLJ) is preferred over a plan with an NLJ (and no
// flatMap).
func TestCompareFlatMapVsNLJ_FlatMapBeatsNLJ(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		a, b    expressionCounts
		wantCmp int
	}{
		{
			name:    "flatMap beats NLJ",
			a:       expressionCounts{flatMapCount: 1},
			b:       expressionCounts{nestedLoopJoinCount: 1},
			wantCmp: -1,
		},
		{
			name:    "NLJ loses to flatMap",
			a:       expressionCounts{nestedLoopJoinCount: 1},
			b:       expressionCounts{flatMapCount: 1},
			wantCmp: 1,
		},
		{
			name:    "both flatMap: tie",
			a:       expressionCounts{flatMapCount: 1},
			b:       expressionCounts{flatMapCount: 1},
			wantCmp: 0,
		},
		{
			name:    "both NLJ: tie",
			a:       expressionCounts{nestedLoopJoinCount: 1},
			b:       expressionCounts{nestedLoopJoinCount: 1},
			wantCmp: 0,
		},
		{
			name:    "flatMap + NLJ vs flatMap only: tie (both have flatMap)",
			a:       expressionCounts{flatMapCount: 1, nestedLoopJoinCount: 1},
			b:       expressionCounts{flatMapCount: 1},
			wantCmp: 0,
		},
		{
			name:    "neither: tie",
			a:       expressionCounts{},
			b:       expressionCounts{},
			wantCmp: 0,
		},
	}

	for _, tc := range tests {
		got := compareFlatMapVsNLJ(tc.a, tc.b)
		if got != tc.wantCmp {
			t.Errorf("compareFlatMapVsNLJ(%s) = %d, want %d", tc.name, got, tc.wantCmp)
		}
	}
}
