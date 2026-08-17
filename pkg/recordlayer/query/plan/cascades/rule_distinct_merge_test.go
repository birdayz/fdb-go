package cascades

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func distinctRuleRowType() *values.RecordType {
	return values.NewRecordType("DistinctRuleRow", false, []values.Field{
		{Name: "K", FieldType: values.NullableString},
		{Name: "ID", FieldType: values.NotNullLong},
	})
}

func mustDistinctConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct distinct-rule fixture: " + err.Error())
	}
	return value
}

func mustFireDistinctExpressionRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

func distinctRuleScan(t testing.TB, recordType string) *expressions.FullUnorderedScanExpression {
	t.Helper()
	return mustDistinctConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, distinctRuleRowType()))
}

func distinctRuleDistinct(
	t testing.TB, inner expressions.Quantifier,
) *expressions.LogicalDistinctExpression {
	t.Helper()
	return mustDistinctConstruct(expressions.NewLogicalDistinctExpression(inner))
}

func distinctRuleField(t testing.TB, root values.Value, ordinal int) values.Value {
	t.Helper()
	return mustDistinctConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

// exploreDistinctRewriting keeps this rule cluster isolated from the broad
// planner fixture while exercising the same production task stack.
func exploreDistinctRewriting(p *Planner, rootRef *expressions.Reference) (int, bool) {
	if rootRef == nil {
		return 0, true
	}
	if p.memo == nil {
		p.memo = NewMemo(rootRef)
	}
	if p.constraintMap == nil {
		p.constraintMap = NewConstraintMap()
	}
	if p.dataAccessConsumed == nil {
		p.dataAccessConsumed = make(map[*expressions.Reference]int)
	}
	p.push(&OptimizeGroupTask{Phase: PhaseRewriting, Ref: rootRef})
	p.push(&ExploreGroupTask{Phase: PhaseRewriting, Ref: rootRef})
	for len(p.stack) > 0 {
		if p.tasksRun >= p.MaxTasks {
			return p.tasksRun, false
		}
		p.pop().Run(context.Background(), p)
		p.tasksRun++
	}
	return p.tasksRun, true
}

func TestDistinctMergeRule_FiresOnNested(t *testing.T) {
	t.Parallel()
	scan := distinctRuleScan(t, "Order")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerD := distinctRuleDistinct(t, scanQ)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerD))
	outerD := distinctRuleDistinct(t, innerQ)
	ref := expressions.InitialOf(outerD)

	rule := NewDistinctMergeRule()
	yielded := mustFireDistinctExpressionRule(t, rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	merged := yielded[0].(*expressions.LogicalDistinctExpression)
	innerExpr := merged.GetInner().GetRangesOver().Get()
	if _, ok := innerExpr.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("merged inner=%T, want *FullUnorderedScanExpression — rule didn't strip the inner Distinct", innerExpr)
	}
}

func TestDistinctMergeRule_DeclinesOnSingle(t *testing.T) {
	t.Parallel()
	scan := distinctRuleScan(t, "Order")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	d := distinctRuleDistinct(t, scanQ)
	ref := expressions.InitialOf(d)
	rule := NewDistinctMergeRule()
	yielded := mustFireDistinctExpressionRule(t, rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on a single Distinct — yielded %d, want 0", len(yielded))
	}
}

func TestDistinctMergeRule_TripleNestedCollapsesToSingle(t *testing.T) {
	t.Parallel()
	scan := distinctRuleScan(t, "T")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	d1 := distinctRuleDistinct(t, scanQ)
	d1Q := expressions.ForEachQuantifier(expressions.InitialOf(d1))
	d2 := distinctRuleDistinct(t, d1Q)
	d2Q := expressions.ForEachQuantifier(expressions.InitialOf(d2))
	d3 := distinctRuleDistinct(t, d2Q)
	ref := expressions.InitialOf(d3)

	rule := NewDistinctMergeRule()
	// First fire: Distinct(Distinct(Distinct(Scan))) → Distinct(Distinct(Scan))
	y1 := mustFireDistinctExpressionRule(t, rule, ref)
	if len(y1) != 1 {
		t.Fatalf("first merge: yielded=%d, want 1", len(y1))
	}
	// The merged result is a Distinct whose inner's reference now has
	// both the original Distinct(Scan) and the newly-merged Distinct(Scan).
	// Re-fire on the new result to collapse the remaining nesting.
	ref2 := expressions.InitialOf(y1[0].(expressions.RelationalExpression))
	y2 := mustFireDistinctExpressionRule(t, rule, ref2)
	if len(y2) != 1 {
		t.Fatalf("second merge: yielded=%d, want 1", len(y2))
	}
	final := y2[0].(*expressions.LogicalDistinctExpression)
	inner := final.GetInner().GetRangesOver().Get()
	if _, ok := inner.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("after two merges, inner=%T, want Scan", inner)
	}
}

func TestDistinctMergeRule_DeclinesOnNonDistinctInner(t *testing.T) {
	t.Parallel()
	scan := distinctRuleScan(t, "T")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	scanRoot := mustDistinctConstruct(scanQ.RequireFlowedObjectValue())
	// Distinct over Sort — inner is Sort, not Distinct
	sort := mustDistinctConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: distinctRuleField(t, scanRoot, 0)}},
		scanQ,
	))
	sortQ := expressions.ForEachQuantifier(expressions.InitialOf(sort))
	d := distinctRuleDistinct(t, sortQ)
	ref := expressions.InitialOf(d)

	rule := NewDistinctMergeRule()
	yielded := mustFireDistinctExpressionRule(t, rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule should decline when inner is Sort, got %d yields", len(yielded))
	}
}
