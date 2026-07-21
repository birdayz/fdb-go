package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
	// Build: Unique(innerRef) where innerRef holds a bare scan plan
	// with distinct=true (scan is always distinct).
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	scanWrapper := scan

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
	// Streaming agg has distinct=false. Since RFC-184 W2 the memo holds the bare
	// *plans.RecordQueryStreamingAggregationPlan (no physicalStreamingAggWrapper).
	aggPlan := plans.NewRecordQueryStreamingAggregationPlan(nil, nil, nil)
	aggWrapper := aggPlan

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
