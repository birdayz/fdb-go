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
	scanRef := expressions.NewFinalReference([]expressions.RelationalExpression{&physicalScanWrapper{plan: scan}})
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

	refA := expressions.NewFinalReference([]expressions.RelationalExpression{&physicalScanWrapper{plan: scanA}})
	refB := expressions.NewFinalReference([]expressions.RelationalExpression{&physicalScanWrapper{plan: scanB}})

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
	scanRef := expressions.NewFinalReference([]expressions.RelationalExpression{&physicalScanWrapper{plan: scan}})
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
	scanRef := expressions.NewFinalReference([]expressions.RelationalExpression{&physicalScanWrapper{plan: scan}})
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

	counts1 := findExpressionsByType(oneType)
	counts3 := findExpressionsByType(threeTypes)

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
	innerRefA := expressions.NewFinalReference([]expressions.RelationalExpression{innerIndexA})
	wrapA := NewPhysicalInJoinWrapper(inJoinPlanA, expressions.NewPhysicalQuantifier(innerRefA))

	// Build an InJoin plan with an unsarged binding: the inner scan
	// has no comparison matching the binding name.
	innerPlanB := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	inJoinPlanB := plans.NewRecordQueryInJoinPlan(innerPlanB, "other_bind", false, false)
	innerScanB := &physicalScanWrapper{plan: innerPlanB}
	innerRefB := expressions.NewFinalReference([]expressions.RelationalExpression{innerScanB})
	wrapB := NewPhysicalInJoinWrapper(inJoinPlanB, expressions.NewPhysicalQuantifier(innerRefB))

	opsA := findExpressionsByType(wrapA)
	opsB := findExpressionsByType(wrapB)

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

	ref1 := expressions.NewFinalReference([]expressions.RelationalExpression{child1})
	ref2 := expressions.NewFinalReference([]expressions.RelationalExpression{child2})

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

func TestPlanningCostModel_IndexScanPreferredOverPrimaryScan(t *testing.T) {
	t.Parallel()
	primary := &physicalScanWrapper{
		plan: plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false),
	}
	index := &physicalIndexScanWrapper{
		plan:        plans.NewRecordQueryIndexPlan("idx_a", nil, []string{"T"}, values.UnknownType, false),
		columnNames: []string{"A"},
	}

	if !PlanningCostModelLess(index, primary) {
		t.Error("index scan should be preferred over primary scan (Java default: PREFER_INDEX)")
	}
	if PlanningCostModelLess(primary, index) {
		t.Error("primary scan should NOT be preferred over index scan")
	}
}
