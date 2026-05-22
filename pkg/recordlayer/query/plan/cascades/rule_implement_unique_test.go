package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/matching"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// ---------------------------------------------------------------------------
// ImplementUniqueRule
// ---------------------------------------------------------------------------

func TestImplementUniqueRule_MatchesLogicalUniqueExpression(t *testing.T) {
	t.Parallel()
	rule := NewImplementUniqueRule()
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	unique := expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(scanRef))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), unique)
	if len(bindings) == 0 {
		t.Fatal("ImplementUniqueRule should match LogicalUniqueExpression")
	}
}

func TestImplementUniqueRule_SkipsNonMatching(t *testing.T) {
	t.Parallel()
	rule := NewImplementUniqueRule()
	// A filter expression should not match.
	scanRef := expressions.InitialOf(expressions.NewFullUnorderedScanExpression([]string{"T"}, nil))
	filter := expressions.NewLogicalFilterExpression(nil, expressions.ForEachQuantifier(scanRef))

	bindings := rule.Matcher().BindMatches(matching.NewBindings(), filter)
	if len(bindings) != 0 {
		t.Fatal("ImplementUniqueRule should NOT match LogicalFilterExpression")
	}
}

func TestImplementUniqueRule_AbsorbsWhenInnerIsDistinct(t *testing.T) {
	t.Parallel()
	// Build: Unique(innerRef) where innerRef holds a physicalScanWrapper
	// with distinct=true (scan is always distinct).
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanWrapper := &physicalScanWrapper{plan: scan}

	// Create inner reference with physical wrapper as final member.
	innerRef := expressions.InitialOf(scanWrapper)

	// Compute plan properties on the inner reference.
	pm := NewPlanPropertiesMap()
	pm.Add(scanWrapper)
	innerRef.SetPlanProperties(pm)

	// Build the LogicalUniqueExpression.
	unique := expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(innerRef))
	outerRef := expressions.InitialOf(unique)

	// Fire the rule.
	results := FireImplementationRule(NewImplementUniqueRule(), outerRef)
	if len(results) == 0 {
		t.Fatal("ImplementUniqueRule should yield expressions when inner is distinct")
	}

	// The yielded expression should be the inner scan wrapper itself
	// (Unique is absorbed, inner plans are promoted).
	found := false
	for _, r := range results {
		if r == scanWrapper {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("yielded expressions should include the inner scan wrapper (Unique absorbed)")
	}
}

func TestImplementUniqueRule_NoYieldWhenInnerNotDistinct(t *testing.T) {
	t.Parallel()
	// Streaming agg wrapper has distinct=false.
	aggPlan := plans.NewRecordQueryStreamingAggregationPlan(nil, nil, nil)
	aggWrapper := &physicalStreamingAggWrapper{plan: aggPlan}

	innerRef := expressions.InitialOf(aggWrapper)
	pm := NewPlanPropertiesMap()
	pm.Add(aggWrapper)
	innerRef.SetPlanProperties(pm)

	unique := expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(innerRef))
	outerRef := expressions.InitialOf(unique)

	results := FireImplementationRule(NewImplementUniqueRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("ImplementUniqueRule should NOT yield when inner is not distinct, got %d results", len(results))
	}
}

func TestImplementUniqueRule_NilInnerRef(t *testing.T) {
	t.Parallel()
	// LogicalUniqueExpression with a quantifier whose reference is nil.
	// The rule should bail without panicking.
	unique := expressions.NewLogicalUniqueExpression(expressions.ForEachQuantifier(nil))
	outerRef := expressions.InitialOf(unique)

	results := FireImplementationRule(NewImplementUniqueRule(), outerRef)
	if len(results) != 0 {
		t.Fatalf("ImplementUniqueRule with nil inner ref should yield nothing, got %d", len(results))
	}
}
