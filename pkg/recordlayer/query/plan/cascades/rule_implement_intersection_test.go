package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// TestImplementIntersectionRule_FiresAfterAllChildrenImplemented pins
// per-child gating: rule yields only when every child has a physical
// plan member.
func TestImplementIntersectionRule_FiresAfterAllChildrenImplemented(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	keyValues := []values.Value{
		&values.FieldValue{Field: "id", Typ: values.NotNullLong},
	}
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(refB),
		},
		keyValues,
	)
	topRef := expressions.InitialOf(intr)

	FireExpressionRule(NewPrimaryScanRule(), refA)
	FireExpressionRule(NewPrimaryScanRule(), refB)

	yielded := FireExpressionRule(NewImplementIntersectionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementIntersectionRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalIntersectionWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalIntersectionWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	if got := len(plan.GetInners()); got != 2 {
		t.Fatalf("intersection inners = %d, want 2", got)
	}
	if got := len(plan.GetComparisonKeyValues()); got != 1 {
		t.Fatalf("comparison keys = %d, want 1 (carried through from logical)", got)
	}
	if _, ok := plan.GetInners()[0].(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner[0] = %T, want *RecordQueryScanPlan", plan.GetInners()[0])
	}
}

// TestImplementIntersectionRule_NoFireWhenAnyChildIsLogical pins
// per-child gating: even one logical child blocks the fire.
func TestImplementIntersectionRule_NoFireWhenAnyChildIsLogical(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(refB),
		},
		nil,
	)
	topRef := expressions.InitialOf(intr)

	// Implement only refA; refB stays logical.
	FireExpressionRule(NewPrimaryScanRule(), refA)

	yielded := FireExpressionRule(NewImplementIntersectionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementIntersectionRule fired with one logical child; yielded %d, want 0", len(yielded))
	}
}

// TestImplementIntersectionRule_NoFireOnEmptyIntersection pins the
// empty-intersection guard.
func TestImplementIntersectionRule_NoFireOnEmptyIntersection(t *testing.T) {
	t.Parallel()
	intr := expressions.NewLogicalIntersectionExpression(nil, nil)
	topRef := expressions.InitialOf(intr)

	yielded := FireExpressionRule(NewImplementIntersectionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementIntersectionRule fired on empty intersection; yielded %d, want 0", len(yielded))
	}
}

// TestPlannerWithBatchA_ImplementsIntersectionOverScan pins
// end-to-end Planner integration: Intersection(Scan, Scan) input
// + Default + Batch A rules drives EXPLORE through saturation and
// cost-extraction picks the physical IntersectionPlan over the
// logical Intersection.
func TestPlannerWithBatchA_ImplementsIntersectionOverScan(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
			expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
		},
		nil,
	)
	ref := expressions.InitialOf(intr)

	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, nil).WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// Planning should produce a physical IntersectionPlan.
	if _, ok := plan.(*physicalIntersectionWrapper); !ok {
		t.Fatalf("plan = %T, want *physicalIntersectionWrapper", plan)
	}
}

// TestImplementIntersectionRule_ThreeChildren pins scaling.
func TestImplementIntersectionRule_ThreeChildren(t *testing.T) {
	t.Parallel()
	refs := make([]*expressions.Reference, 3)
	qs := make([]expressions.Quantifier, 3)
	for i, name := range []string{"A", "B", "C"} {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		refs[i] = expressions.InitialOf(scan)
		qs[i] = expressions.ForEachQuantifier(refs[i])
	}
	intr := expressions.NewLogicalIntersectionExpression(qs, nil)
	topRef := expressions.InitialOf(intr)

	for _, r := range refs {
		FireExpressionRule(NewPrimaryScanRule(), r)
	}

	yielded := FireExpressionRule(NewImplementIntersectionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementIntersectionRule yielded %d, want 1", len(yielded))
	}
	wrap := yielded[0].(*physicalIntersectionWrapper)
	if got := len(wrap.GetPlan().GetInners()); got != 3 {
		t.Fatalf("intersection inners = %d, want 3", got)
	}
}
