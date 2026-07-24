package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func intersectionRuleRecordType() *values.RecordType {
	return &values.RecordType{
		RecordName: "T",
		Fields: []values.Field{{
			Name:      "ID",
			FieldType: values.NotNullLong,
			Ordinal:   0,
		}},
	}
}

func intersectionRuleKey() values.Value {
	return &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
}

func intersectionRuleOrderedScan(
	rt *values.RecordType,
	reverse bool,
) *plans.RecordQueryScanPlan {
	return plans.NewRecordQueryScanPlan([]string{"T"}, rt, reverse).
		WithPrimaryKey([]values.Value{intersectionRuleKey()})
}

// TestImplementIntersectionRule_FiresAfterAllChildrenImplemented pins
// per-child gating: the rule yields only when every child has a physical
// member satisfying the comparison-key ordering.
func TestImplementIntersectionRule_FiresAfterAllChildrenImplemented(t *testing.T) {
	t.Parallel()
	rt := intersectionRuleRecordType()
	scanA := intersectionRuleOrderedScan(rt, false)
	scanB := intersectionRuleOrderedScan(rt, false)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	keyValues := []values.Value{intersectionRuleKey()}
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(refB),
		},
		keyValues,
	)
	topRef := expressions.InitialOf(intr)

	yielded := FireExpressionRule(NewImplementIntersectionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementIntersectionRule yielded %d, want 1", len(yielded))
	}
	plan, ok := yielded[0].(*plans.RecordQueryIntersectionPlan)
	if !ok {
		t.Fatalf("yield = %T, want *plans.RecordQueryIntersectionPlan", yielded[0])
	}
	if got := len(plan.GetInners()); got != 2 {
		t.Fatalf("intersection inners = %d, want 2", got)
	}
	if got := len(plan.GetComparisonKeyValues()); got != 1 {
		t.Fatalf("comparison keys = %d, want 1 (carried through from logical)", got)
	}
	if key, ok := plan.GetComparisonKeyValues()[0].(*values.FieldValue); !ok ||
		key.Resolved == nil || key.Resolved.Root().Ordinal != 0 {
		t.Fatalf("comparison key = %#v, want plan-time-baked ordinal 0", plan.GetComparisonKeyValues()[0])
	}
	if _, ok := plan.GetInners()[0].(*plans.RecordQueryScanPlan); !ok {
		t.Fatalf("inner[0] = %T, want *RecordQueryScanPlan", plan.GetInners()[0])
	}
	for i, quantifier := range plan.GetQuantifiers() {
		if quantifier.Kind() != expressions.QuantifierPhysical {
			t.Fatalf("quantifier[%d] kind = %v, want physical", i, quantifier.Kind())
		}
		if got := len(quantifier.GetRangesOver().AllMembers()); got != 1 {
			t.Fatalf("quantifier[%d] members = %d, want pinned singleton", i, got)
		}
	}
}

// TestImplementIntersectionRule_NoFireWhenAnyChildIsLogical pins
// per-child gating: even one logical child blocks the fire.
func TestImplementIntersectionRule_NoFireWhenAnyChildIsLogical(t *testing.T) {
	t.Parallel()
	rt := intersectionRuleRecordType()
	scanA := intersectionRuleOrderedScan(rt, false)
	scanB := expressions.NewFullUnorderedScanExpression([]string{"T"}, rt)
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(refB),
		},
		[]values.Value{intersectionRuleKey()},
	)
	topRef := expressions.InitialOf(intr)

	yielded := FireExpressionRule(NewImplementIntersectionRule(), topRef)
	if len(yielded) != 0 {
		t.Fatalf("ImplementIntersectionRule fired with one logical child; yielded %d, want 0", len(yielded))
	}
}

// TestImplementIntersectionRule_NoFireWhenAnyChildIsUnordered pins the sorted
// merge precondition. An arbitrary physical winner is not sufficient.
func TestImplementIntersectionRule_NoFireWhenAnyChildIsUnordered(t *testing.T) {
	t.Parallel()
	rt := intersectionRuleRecordType()
	ordered := intersectionRuleOrderedScan(rt, false)
	unordered := plans.NewRecordQueryScanPlan([]string{"T"}, rt, false)
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(ordered)),
			expressions.ForEachQuantifier(expressions.InitialOf(unordered)),
		},
		[]values.Value{intersectionRuleKey()},
	)

	if yielded := FireExpressionRule(
		NewImplementIntersectionRule(),
		expressions.InitialOf(intr),
	); len(yielded) != 0 {
		t.Fatalf("ImplementIntersectionRule fired with an unordered physical child; yielded %d, want 0", len(yielded))
	}
}

// TestImplementIntersectionRule_PicksOrderedSibling pins that the ordering
// request, not the group's preserve-order cost winner, selects each leg.
func TestImplementIntersectionRule_PicksOrderedSibling(t *testing.T) {
	t.Parallel()
	rt := intersectionRuleRecordType()
	reverseOnly := intersectionRuleOrderedScan(rt, true)
	orderedA := intersectionRuleOrderedScan(rt, false)
	orderedB := intersectionRuleOrderedScan(rt, false)
	refA := expressions.InitialOf(reverseOnly)
	refA.Insert(orderedA)
	refA.SetWinner(reverseOnly)
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(expressions.InitialOf(orderedB)),
		},
		[]values.Value{intersectionRuleKey()},
	)
	topRef := expressions.InitialOf(intr)
	memo := NewMemo(topRef)

	yielded := FireExpressionRuleWithMemo(
		NewImplementIntersectionRule(),
		topRef,
		EmptyPlanContext(),
		memo,
	)
	if len(yielded) != 1 {
		t.Fatalf("ImplementIntersectionRule yielded %d, want 1", len(yielded))
	}
	plan := yielded[0].(*plans.RecordQueryIntersectionPlan)
	if got := plan.GetInners()[0].(*plans.RecordQueryScanPlan).GetPrimaryKeyValues(); len(got) != 1 {
		t.Fatalf("first winner primary key = %#v, want ordered sibling", got)
	}
	if plan.GetInners()[0].(*plans.RecordQueryScanPlan).IsReverse() {
		t.Fatal("first winner is the reverse sibling, want forward ASC")
	}
}

// TestImplementIntersectionRule_DeclinesStaleBakedOrdinal pins that the
// runtime comparison key is validated against the common row layout. A
// same-name key baked to PAYLOAD's slot is not an ordering by ID.
func TestImplementIntersectionRule_DeclinesStaleBakedOrdinal(t *testing.T) {
	t.Parallel()
	rt := &values.RecordType{
		RecordName: "T",
		Fields: []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "PAYLOAD", FieldType: values.NotNullString, Ordinal: 1},
		},
	}
	id := &values.FieldValue{Field: "ID", Typ: values.NotNullLong}
	scanA := plans.NewRecordQueryScanPlan([]string{"T"}, rt, false).
		WithPrimaryKey([]values.Value{id})
	scanB := plans.NewRecordQueryScanPlan([]string{"T"}, rt, false).
		WithPrimaryKey([]values.Value{id})
	staleID := values.NewFieldValueWithResolvedOrdinal("ID", 1, values.NotNullLong)
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
			expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
		},
		[]values.Value{staleID},
	)

	if yielded := FireExpressionRule(
		NewImplementIntersectionRule(),
		expressions.InitialOf(intr),
	); len(yielded) != 0 {
		t.Fatalf("stale baked comparison ordinal yielded %d plans, want 0", len(yielded))
	}
}

// TestImplementIntersectionRule_NoFireWithoutComparisonKey pins that an empty
// merge key cannot establish cursor progress.
func TestImplementIntersectionRule_NoFireWithoutComparisonKey(t *testing.T) {
	t.Parallel()
	rt := intersectionRuleRecordType()
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(intersectionRuleOrderedScan(rt, false))),
			expressions.ForEachQuantifier(expressions.InitialOf(intersectionRuleOrderedScan(rt, false))),
		},
		nil,
	)

	if yielded := FireExpressionRule(
		NewImplementIntersectionRule(),
		expressions.InitialOf(intr),
	); len(yielded) != 0 {
		t.Fatalf("ImplementIntersectionRule fired without a comparison key; yielded %d, want 0", len(yielded))
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

// TestPlannerWithBatchA_ImplementsIntersectionOverScan pins end-to-end Planner
// integration for two already-ordered scan alternatives.
func TestPlannerWithBatchA_ImplementsIntersectionOverScan(t *testing.T) {
	t.Parallel()
	rt := intersectionRuleRecordType()
	scanA := intersectionRuleOrderedScan(rt, false)
	scanB := intersectionRuleOrderedScan(rt, false)
	intr := expressions.NewLogicalIntersectionExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
			expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
		},
		[]values.Value{intersectionRuleKey()},
	)
	ref := expressions.InitialOf(intr)

	rules := DefaultExpressionRules()
	p := NewPlanner(rules, nil).
		WithPlanningExpressionRules(BatchAExpressionRules()).
		WithImplementationRules(DefaultImplementationRules())
	plan, _, err := p.Plan(ref)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan == nil {
		t.Fatal("Plan returned nil")
	}

	// Planning should produce a physical IntersectionPlan.
	if _, ok := plan.(*plans.RecordQueryIntersectionPlan); !ok {
		t.Fatalf("plan = %T, want *plans.RecordQueryIntersectionPlan", plan)
	}
}

// TestImplementIntersectionRule_ThreeChildren pins scaling.
func TestImplementIntersectionRule_ThreeChildren(t *testing.T) {
	t.Parallel()
	rt := intersectionRuleRecordType()
	refs := make([]*expressions.Reference, 3)
	qs := make([]expressions.Quantifier, 3)
	for i := range refs {
		scan := intersectionRuleOrderedScan(rt, false)
		refs[i] = expressions.InitialOf(scan)
		qs[i] = expressions.ForEachQuantifier(refs[i])
	}
	intr := expressions.NewLogicalIntersectionExpression(qs, []values.Value{intersectionRuleKey()})
	topRef := expressions.InitialOf(intr)

	yielded := FireExpressionRule(NewImplementIntersectionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementIntersectionRule yielded %d, want 1", len(yielded))
	}
	plan := yielded[0].(*plans.RecordQueryIntersectionPlan)
	if got := len(plan.GetInners()); got != 3 {
		t.Fatalf("intersection inners = %d, want 3", got)
	}
}
