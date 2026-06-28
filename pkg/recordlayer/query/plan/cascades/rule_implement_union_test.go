package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestImplementUnionRule_FiresAfterAllChildrenImplemented pins the
// per-child gating contract: ImplementUnionRule yields the physical
// UnionPlan only when EVERY child Reference has a physical-plan
// member. Partial physical implementation produces an invalid mixed-
// hierarchy plan tree, so the rule must wait until all children are
// physical-ready.
func TestImplementUnionRule_FiresAfterAllChildrenImplemented(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	topRef := expressions.InitialOf(union)

	// Step 1: implement BOTH child scans.
	FireExpressionRule(NewPrimaryScanRule(), refA)
	FireExpressionRule(NewPrimaryScanRule(), refB)

	// Step 2: fire the union rule.
	yielded := FireExpressionRule(NewImplementUnionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementUnionRule yielded %d, want 1", len(yielded))
	}
	wrap, ok := yielded[0].(*physicalUnionWrapper)
	if !ok {
		t.Fatalf("yield = %T, want *physicalUnionWrapper", yielded[0])
	}
	plan := wrap.GetPlan()
	if plan == nil {
		t.Fatal("wrapper has no plan")
	}
	inners := plan.GetInners()
	if len(inners) != 2 {
		t.Fatalf("union plan inners = %d, want 2", len(inners))
	}
	if _, ok := inners[0].(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner[0] = %T, want *RecordQueryScanPlan", inners[0])
	}
	if _, ok := inners[1].(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner[1] = %T, want *RecordQueryScanPlan", inners[1])
	}
}

// TestImplementUnionRule_NoFireWhenAnyChildIsLogical pins that
// ImplementUnionRule waits if EVEN ONE child has no physical
// member yet. With 2 children, only implementing the first must
// leave the union un-implemented.
func TestImplementUnionRule_NoFireWhenAnyChildIsLogical(t *testing.T) {
	t.Parallel()
	scanA := expressions.NewFullUnorderedScanExpression([]string{"A"}, values.UnknownType)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"B"}, values.UnknownType)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	})
	topRef := expressions.InitialOf(union)

	// Implement only the FIRST child; second remains logical.
	FireExpressionRule(NewPrimaryScanRule(), refA)

	yielded := FireExpressionRule(NewImplementUnionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementUnionRule fired with one logical child; yielded %d, want 0", len(yielded))
	}
}

// TestImplementUnionRule_NoFireOnEmptyUnion pins the empty-union
// guard: an empty Union yields nothing rather than producing a
// degenerate UnionPlan with zero inners.
func TestImplementUnionRule_NoFireOnEmptyUnion(t *testing.T) {
	t.Parallel()
	union := expressions.NewLogicalUnionExpression(nil)
	topRef := expressions.InitialOf(union)

	yielded := FireExpressionRule(NewImplementUnionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementUnionRule fired on empty union; yielded %d, want 0", len(yielded))
	}
}

// TestImplementUnionRule_ThreeChildren pins that the rule scales
// past 2 children: a 3-child UNION ALL produces a 3-inner
// UnionPlan after all children are implemented.
func TestImplementUnionRule_ThreeChildren(t *testing.T) {
	t.Parallel()
	refs := make([]*expressions.Reference, 3)
	qs := make([]expressions.Quantifier, 3)
	for i, name := range []string{"A", "B", "C"} {
		scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
		refs[i] = expressions.InitialOf(scan)
		qs[i] = expressions.ForEachQuantifier(refs[i])
	}
	union := expressions.NewLogicalUnionExpression(qs)
	topRef := expressions.InitialOf(union)

	for _, r := range refs {
		FireExpressionRule(NewPrimaryScanRule(), r)
	}

	yielded := FireExpressionRule(NewImplementUnionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementUnionRule yielded %d, want 1", len(yielded))
	}
	wrap := yielded[0].(*physicalUnionWrapper)
	if got := len(wrap.GetPlan().GetInners()); got != 3 {
		t.Fatalf("union plan inners = %d, want 3", got)
	}
}
