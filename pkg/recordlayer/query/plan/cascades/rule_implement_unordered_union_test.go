package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// ImplementUnorderedUnionRule — matcher
// ---------------------------------------------------------------------------

func TestImplementUnorderedUnionRule_MatchesLogicalUnionExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementUnorderedUnionRule()
	scanRef := expressions.InitialOf(unionRuleFullScan("T"))
	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(scanRef),
	}))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	if len(bindings) == 0 {
		t.Fatal("ImplementUnorderedUnionRule should match LogicalUnionExpression")
	}
}

func TestImplementUnorderedUnionRule_SkipsNonMatching(t *testing.T) {
	t.Parallel()
	rule := NewImplementUnorderedUnionRule()
	scanRef := expressions.InitialOf(unionRuleFullScan("T"))
	filter := mustUnionRuleConstruct(expressions.NewLogicalFilterExpression(
		nil, expressions.ForEachQuantifier(scanRef)))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	if len(bindings) != 0 {
		t.Fatal("ImplementUnorderedUnionRule should NOT match LogicalFilterExpression")
	}
}

func TestImplementUnorderedUnionRule_SkipsLogicalUniqueExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementUnorderedUnionRule()
	scanRef := expressions.InitialOf(unionRuleFullScan("T"))
	unique := mustUnionRuleConstruct(expressions.NewLogicalUniqueExpression(
		expressions.ForEachQuantifier(scanRef)))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), unique)
	if len(bindings) != 0 {
		t.Fatal("ImplementUnorderedUnionRule should NOT match LogicalUniqueExpression")
	}
}

// ---------------------------------------------------------------------------
// ImplementUnorderedUnionRule — OnMatch
// ---------------------------------------------------------------------------

func TestImplementUnorderedUnionRule_CreatesUnorderedUnionPlan(t *testing.T) {
	t.Parallel()
	// Build two inner references, each holding a bare scan plan.
	scanA := unionRulePlanScan("A")
	scanB := unionRulePlanScan("B")
	wA := scanA
	wB := scanB

	refA := expressions.InitialOf(wA)
	pmA := NewPlanPropertiesMap()
	pmA.Add(wA)
	refA.SetPlanProperties(pmA)

	refB := expressions.InitialOf(wB)
	pmB := NewPlanPropertiesMap()
	pmB.Add(wB)
	refB.SetPlanProperties(pmB)

	// Build the logical union over the two refs.
	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))
	outerRef := expressions.InitialOf(union)

	results := fireUnionImplementationRule(t, NewImplementUnorderedUnionRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("ImplementUnorderedUnionRule should yield at least one expression")
	}

	// The yielded expression should be the bare *RecordQueryUnorderedUnionPlan.
	foundPlan := false
	for _, r := range results {
		if uup, ok := r.(*plans.RecordQueryUnorderedUnionPlan); ok {
			foundPlan = true
			inners := uup.GetInners()
			if len(inners) < 2 {
				t.Fatalf("unordered union should have >= 2 inner plans, got %d", len(inners))
			}
		}
	}
	if !foundPlan {
		t.Fatal("expected at least one *RecordQueryUnorderedUnionPlan in results")
	}
}

func TestImplementUnorderedUnionRule_NoYieldForEmptyQuantifiers(t *testing.T) {
	t.Parallel()
	if union, err := expressions.NewLogicalUnionExpression(nil); err == nil || union != nil {
		t.Fatalf("empty logical union = %T, %v; want atomic constructor rejection", union, err)
	}
}

func TestImplementUnorderedUnionRule_NoYieldForSingleChildWithNoPhysicalPlans(t *testing.T) {
	t.Parallel()
	// Single child ref with only logical expressions (no physical wrappers).
	logicalRef := expressions.InitialOf(unionRuleFullScan("T"))
	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(logicalRef),
	}))
	outerRef := expressions.InitialOf(union)

	results := fireUnionImplementationRule(t, NewImplementUnorderedUnionRule(), outerRef)
	// The inner reference holds no physical plan, so ToPlanPartitions rolls up
	// to nothing and the rule declines. Assert the DECLINE rather than merely
	// surviving the call: a rule that yielded a union over an unimplemented leg
	// would produce a plan tree with a logical node inside it.
	for _, r := range results {
		if uup, ok := r.(*plans.RecordQueryUnorderedUnionPlan); ok {
			t.Fatalf("rule yielded %T over a leg with no physical plan", uup)
		}
	}
}

// TestImplementUnorderedUnionRule_SingleLegImplementsAsOneLegConcat pins the
// ARITY of the rule against Java's: RecordQueryUnorderedUnionPlan.fromQuantifiers
// imposes no minimum leg count, so a one-leg logical union implements.
//
// A Go-only two-leg floor used to live here, and it was invisible only because
// UnionSingletonElimRule normally rewrites a singleton union away in REWRITING
// — leaving ImplementUnionRule, since deleted, as the sole implementer of the
// shape that survives when that rewrite does not fire. With the floor and that
// rule both gone the union would have had NO implementer, and the planner
// returned no plan AND no error.
func TestImplementUnorderedUnionRule_SingleLegImplementsAsOneLegConcat(t *testing.T) {
	t.Parallel()
	scan := unionRulePlanScan("A")
	ref := expressions.InitialOf(scan)
	pm := NewPlanPropertiesMap()
	pm.Add(scan)
	ref.SetPlanProperties(pm)

	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(ref),
	}))
	outerRef := expressions.InitialOf(union)

	results := fireUnionImplementationRule(t, NewImplementUnorderedUnionRule(), outerRef)
	found := 0
	for _, r := range results {
		uup, ok := r.(*plans.RecordQueryUnorderedUnionPlan)
		if !ok {
			continue
		}
		found++
		if got := len(uup.GetInners()); got != 1 {
			t.Fatalf("one-leg union implemented with %d inners, want 1", got)
		}
	}
	if found != 1 {
		t.Fatalf("one-leg logical union yielded %d unordered union plans, want 1", found)
	}
}

// ---------------------------------------------------------------------------
// crossProductPartitions
// ---------------------------------------------------------------------------

func TestCrossProductPartitions_Empty(t *testing.T) {
	t.Parallel()
	result := crossProductPartitions(nil)
	if result != nil {
		t.Fatalf("crossProductPartitions(nil) = %v, want nil", result)
	}
}

func TestCrossProductPartitions_SingleChildSinglePartition(t *testing.T) {
	t.Parallel()
	p := NewPlanPartition(nil, nil)
	result := crossProductPartitions([][]*PlanPartition{{p}})
	if len(result) != 1 {
		t.Fatalf("expected 1 combination, got %d", len(result))
	}
	if len(result[0]) != 1 {
		t.Fatalf("expected combination of length 1, got %d", len(result[0]))
	}
	if result[0][0] != p {
		t.Fatal("partition mismatch")
	}
}

func TestCrossProductPartitions_TwoChildrenSinglePartitionEach(t *testing.T) {
	t.Parallel()
	pA := NewPlanPartition(nil, nil)
	pB := NewPlanPartition(nil, nil)
	result := crossProductPartitions([][]*PlanPartition{{pA}, {pB}})
	if len(result) != 1 {
		t.Fatalf("expected 1 combination, got %d", len(result))
	}
	if len(result[0]) != 2 {
		t.Fatalf("expected combination of length 2, got %d", len(result[0]))
	}
}

func TestCrossProductPartitions_TwoChildrenTwoPartitionsEach(t *testing.T) {
	t.Parallel()
	pA1 := NewPlanPartition(nil, nil)
	pA2 := NewPlanPartition(nil, nil)
	pB1 := NewPlanPartition(nil, nil)
	pB2 := NewPlanPartition(nil, nil)
	result := crossProductPartitions([][]*PlanPartition{{pA1, pA2}, {pB1, pB2}})
	// 2 * 2 = 4 combinations.
	if len(result) != 4 {
		t.Fatalf("expected 4 combinations, got %d", len(result))
	}
	for _, combo := range result {
		if len(combo) != 2 {
			t.Fatalf("each combination should have 2 partitions, got %d", len(combo))
		}
	}
}

func TestCrossProductPartitions_ThreeChildren(t *testing.T) {
	t.Parallel()
	pA := NewPlanPartition(nil, nil)
	pB1 := NewPlanPartition(nil, nil)
	pB2 := NewPlanPartition(nil, nil)
	pC := NewPlanPartition(nil, nil)
	// 1 * 2 * 1 = 2 combinations.
	result := crossProductPartitions([][]*PlanPartition{{pA}, {pB1, pB2}, {pC}})
	if len(result) != 2 {
		t.Fatalf("expected 2 combinations, got %d", len(result))
	}
	for _, combo := range result {
		if len(combo) != 3 {
			t.Fatalf("each combination should have 3 partitions, got %d", len(combo))
		}
	}
}

// TestImplementUnorderedUnionRule_DeclinesANonForEachLeg pins the rule to Java's
// matcher, all(forEachQuantifierOverRef(...)): a logical union with an
// existential leg is not this rule's to implement. Without the guard the rule
// memoized every leg as a physical quantifier and yielded a concatenating
// union that emitted the existential leg's rows.
func TestImplementUnorderedUnionRule_DeclinesANonForEachLeg(t *testing.T) {
	t.Parallel()
	scanA := unionRulePlanScan("A")
	scanB := unionRulePlanScan("B")
	refA := expressions.InitialOf(scanA)
	pmA := NewPlanPropertiesMap()
	pmA.Add(scanA)
	refA.SetPlanProperties(pmA)
	refB := expressions.InitialOf(scanB)
	pmB := NewPlanPropertiesMap()
	pmB.Add(scanB)
	refB.SetPlanProperties(pmB)

	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ExistentialQuantifier(refB),
	}))
	results := fireUnionImplementationRule(t, NewImplementUnorderedUnionRule(), expressions.InitialOf(union))
	for _, r := range results {
		if uup, ok := r.(*plans.RecordQueryUnorderedUnionPlan); ok {
			t.Fatalf("rule yielded %T over an existential leg; Java's matcher accepts only for-each legs", uup)
		}
	}

	// Control: the same two references as for-each legs implement.
	forEach := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))
	found := false
	for _, r := range fireUnionImplementationRule(t, NewImplementUnorderedUnionRule(), expressions.InitialOf(forEach)) {
		if _, ok := r.(*plans.RecordQueryUnorderedUnionPlan); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("control: a union of two for-each legs must implement")
	}
}
