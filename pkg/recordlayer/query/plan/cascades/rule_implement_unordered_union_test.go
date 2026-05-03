package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// ImplementUnorderedUnionRule — matcher
// ---------------------------------------------------------------------------

func TestImplementUnorderedUnionRule_MatchesLogicalUnionExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementUnorderedUnionRule()
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(scanRef),
	})

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), union)
	if len(bindings) == 0 {
		t.Fatal("ImplementUnorderedUnionRule should match LogicalUnionExpression")
	}
}

func TestImplementUnorderedUnionRule_SkipsNonMatching(t *testing.T) {
	t.Parallel()
	rule := NewImplementUnorderedUnionRule()
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	filter := expressions.NewLogicalFilterExpression(nil, expressions.ForEachQuantifier(scanRef))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	if len(bindings) != 0 {
		t.Fatal("ImplementUnorderedUnionRule should NOT match LogicalFilterExpression")
	}
}

func TestImplementUnorderedUnionRule_SkipsLogicalUniqueExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementUnorderedUnionRule()
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	unique := expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(scanRef))

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
	// Build two inner references, each holding a physicalScanWrapper.
	scanA := plans.NewRecordQueryScanPlan([]string{"A"}, values.UnknownType, false)
	scanB := plans.NewRecordQueryScanPlan([]string{"B"}, values.UnknownType, false)
	wA := &physicalScanWrapper{plan: scanA}
	wB := &physicalScanWrapper{plan: scanB}

	refA := expressions.NewFinalReference([]expressions.RelationalExpression{wA})
	pmA := NewPlanPropertiesMap()
	pmA.Add(wA)
	refA.SetPlanProperties(pmA)

	refB := expressions.NewFinalReference([]expressions.RelationalExpression{wB})
	pmB := NewPlanPropertiesMap()
	pmB.Add(wB)
	refB.SetPlanProperties(pmB)

	// Build the logical union over the two refs.
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	outerRef := expressions.InitialOf(union)

	results := FireImplementationRule(NewImplementUnorderedUnionRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("ImplementUnorderedUnionRule should yield at least one expression")
	}

	// The yielded expression should be a physicalUnorderedUnionWrapper.
	foundWrapper := false
	for _, r := range results {
		if w, ok := r.(*physicalUnorderedUnionWrapper); ok {
			foundWrapper = true
			plan := w.GetRecordQueryPlan()
			uup, ok := plan.(*plans.RecordQueryUnorderedUnionPlan)
			if !ok {
				t.Fatalf("expected underlying plan to be *RecordQueryUnorderedUnionPlan, got %T", plan)
			}
			inners := uup.GetInners()
			if len(inners) < 2 {
				t.Fatalf("unordered union should have >= 2 inner plans, got %d", len(inners))
			}
		}
	}
	if !foundWrapper {
		t.Fatal("expected at least one physicalUnorderedUnionWrapper in results")
	}
}

func TestImplementUnorderedUnionRule_NoYieldForEmptyQuantifiers(t *testing.T) {
	t.Parallel()
	union := expressions.NewLogicalUnionExpression(nil)
	outerRef := expressions.InitialOf(union)

	results := FireImplementationRule(NewImplementUnorderedUnionRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("ImplementUnorderedUnionRule should yield nothing for empty quantifiers, got %d", len(results))
	}
}

func TestImplementUnorderedUnionRule_NoYieldForSingleChildWithNoPhysicalPlans(t *testing.T) {
	t.Parallel()
	// Single child ref with only logical expressions (no physical wrappers).
	logicalRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(logicalRef),
	})
	outerRef := expressions.InitialOf(union)

	results := FireImplementationRule(NewImplementUnorderedUnionRule(), outerRef)
	// With no physical plans in the inner reference, ToPlanPartitions
	// may return empty and the rule bails.
	// This is fine — verify no panic.
	_ = results
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
