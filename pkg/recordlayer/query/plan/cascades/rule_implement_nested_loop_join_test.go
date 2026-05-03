package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestImplementNestedLoopJoin_Fires(t *testing.T) {
	t.Parallel()

	// Build: Select([a.id = b.id], [Scan(A), Scan(B)])
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)

	joinPred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "a_id", Typ: values.UnknownType},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, "b_id"),
	)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{scanAQ, scanBQ},
		[]predicates.QueryPredicate{joinPred},
	)
	selRef := expressions.InitialOf(sel)

	// First, need to implement the inner scans as physical plans
	// (PrimaryScanRule fires on FullUnorderedScan).
	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, EmptyPlanContext())
	if _, conv := p.Explore(selRef); !conv {
		t.Fatal("planner did not converge")
	}

	// After exploration, the Select should have a physical NLJ member.
	foundNLJ := false
	for _, m := range selRef.Members() {
		if IsPhysicalNestedLoopJoin(m) {
			foundNLJ = true
			break
		}
	}
	if !foundNLJ {
		t.Fatal("ImplementNestedLoopJoinRule didn't produce a physical NLJ member")
	}
}

func TestImplementNestedLoopJoin_DoesNotFireOnSingleQuantifier(t *testing.T) {
	t.Parallel()

	// Select with only 1 quantifier (not a join).
	scan := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{scanQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	results := FireExpressionRule(NewImplementNestedLoopJoinRule(), selRef)
	if len(results) != 0 {
		t.Fatal("ImplementNestedLoopJoinRule should NOT fire on single-quantifier Select")
	}
}

func TestImplementNestedLoopJoin_PlanOutput(t *testing.T) {
	t.Parallel()

	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanARef := expressions.InitialOf(scanA)
	scanAQ := expressions.ForEachQuantifier(scanARef)

	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	scanBRef := expressions.InitialOf(scanB)
	scanBQ := expressions.ForEachQuantifier(scanBRef)

	sel := expressions.NewSelectExpression(
		values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier()),
		[]expressions.Quantifier{scanAQ, scanBQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	// Plan the join.
	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, EmptyPlanContext())
	plan, _, err := p.Plan(selRef)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}
	if !IsPhysicalNestedLoopJoin(plan) {
		t.Fatalf("expected NLJ plan, got %T", plan)
	}

	// Verify explain output.
	explain := ExplainPhysicalPlan(plan)
	if explain == "" {
		t.Fatal("ExplainPhysicalPlan returned empty")
	}
	t.Logf("NLJ Explain: %s", explain)
}
