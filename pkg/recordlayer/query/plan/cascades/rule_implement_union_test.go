package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func unionRuleRowType() *values.RecordType {
	return values.NewRecordType("UnionRow", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "V", FieldType: values.NullableLong},
	})
}

func mustUnionRuleConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct union-rule fixture: " + err.Error())
	}
	return value
}

func mustUnionRuleQOV(value values.Value) values.QuantifiedObjectValue {
	qov, ok := values.AsQuantifiedObjectValue(value)
	if !ok {
		panic("union-rule fixture result is not an exact QOV")
	}
	return qov
}

func unionRuleFullScan(name string) *expressions.FullUnorderedScanExpression {
	return mustUnionRuleConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{name}, unionRuleRowType()))
}

func unionRulePlanScan(name string) *plans.RecordQueryScanPlan {
	return mustUnionRuleConstruct(plans.NewRecordQueryScanPlan(
		[]string{name}, unionRuleRowType(), false))
}

func fireUnionExpressionRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func fireUnionImplementationRule(
	t testing.TB, rule ImplementationRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireImplementationRule(rule, ref)
	if err != nil {
		t.Fatalf("FireImplementationRule: %v", err)
	}
	return result
}

// TestImplementUnionRule_FiresAfterAllChildrenImplemented pins the
// per-child gating contract: ImplementUnionRule yields the physical
// UnionPlan only when EVERY child Reference has a physical-plan
// member. Partial physical implementation produces an invalid mixed-
// hierarchy plan tree, so the rule must wait until all children are
// physical-ready.
func TestImplementUnionRule_FiresAfterAllChildrenImplemented(t *testing.T) {
	t.Parallel()
	scanA := unionRuleFullScan("A")
	scanB := unionRuleFullScan("B")
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))
	topRef := expressions.InitialOf(union)

	// Step 1: implement BOTH child scans.
	fireUnionExpressionRule(t, NewPrimaryScanRule(), refA)
	fireUnionExpressionRule(t, NewPrimaryScanRule(), refB)

	// Step 2: fire the union rule.
	yielded := fireUnionExpressionRule(t, NewImplementUnionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementUnionRule yielded %d, want 1", len(yielded))
	}
	plan, ok := yielded[0].(*plans.RecordQueryUnionPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryUnionPlan", yielded[0])
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
	scanA := unionRuleFullScan("A")
	scanB := unionRuleFullScan("B")
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(refA),
		expressions.ForEachQuantifier(refB),
	}))
	topRef := expressions.InitialOf(union)

	// Implement only the FIRST child; second remains logical.
	fireUnionExpressionRule(t, NewPrimaryScanRule(), refA)

	yielded := fireUnionExpressionRule(t, NewImplementUnionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementUnionRule fired with one logical child; yielded %d, want 0", len(yielded))
	}
}

// TestImplementUnionRule_NoFireOnEmptyUnion pins the empty-union
// guard: an empty Union yields nothing rather than producing a
// degenerate UnionPlan with zero inners.
func TestImplementUnionRule_NoFireOnEmptyUnion(t *testing.T) {
	t.Parallel()
	if union, err := expressions.NewLogicalUnionExpression(nil); err == nil || union != nil {
		t.Fatalf("empty logical union = %T, %v; want atomic constructor rejection", union, err)
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
		scan := unionRuleFullScan(name)
		refs[i] = expressions.InitialOf(scan)
		qs[i] = expressions.ForEachQuantifier(refs[i])
	}
	union := mustUnionRuleConstruct(expressions.NewLogicalUnionExpression(qs))
	topRef := expressions.InitialOf(union)

	for _, r := range refs {
		fireUnionExpressionRule(t, NewPrimaryScanRule(), r)
	}

	yielded := fireUnionExpressionRule(t, NewImplementUnionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementUnionRule yielded %d, want 1", len(yielded))
	}
	plan := yielded[0].(*plans.RecordQueryUnionPlan)
	if got := len(plan.GetInners()); got != 3 {
		t.Fatalf("union plan inners = %d, want 3", got)
	}
}
