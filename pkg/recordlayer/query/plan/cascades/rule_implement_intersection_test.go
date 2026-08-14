package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func intersectionRuleRecordType() *values.RecordType {
	return values.NewRecordType("T", false, []values.Field{{
		Name:      "ID",
		FieldType: values.NotNullLong,
		Ordinal:   0,
	}})
}

func mustIntersectionRuleConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct intersection-rule fixture: " + err.Error())
	}
	return value
}

func mustFireIntersectionRule(
	t testing.TB,
	rule ExpressionRule,
	ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	yielded, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule() unexpected error: %v", err)
	}
	return yielded
}

func mustFireIntersectionRuleWithMemo(
	t testing.TB,
	rule ExpressionRule,
	ref *expressions.Reference,
	ctx PlanContext,
	memo *Memo,
) []expressions.RelationalExpression {
	t.Helper()
	yielded, err := FireExpressionRuleWithMemo(rule, ref, ctx, memo)
	if err != nil {
		t.Fatalf("FireExpressionRuleWithMemo() unexpected error: %v", err)
	}
	return yielded
}

func intersectionRuleRoot(rt *values.RecordType) values.QuantifiedObjectValue {
	return mustIntersectionRuleConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("intersection_key"), rt))
}

func intersectionRuleField(rt *values.RecordType, ordinal int) values.Value {
	return mustIntersectionRuleConstruct(values.ResolveFieldOrdinals(
		intersectionRuleRoot(rt), []int{ordinal}))
}

func intersectionRuleKey(rt *values.RecordType) values.Value {
	return intersectionRuleField(rt, 0)
}

func intersectionRuleExpression(
	quantifiers []expressions.Quantifier,
	comparisonKeys []values.Value,
) *expressions.LogicalIntersectionExpression {
	return mustIntersectionRuleConstruct(expressions.NewLogicalIntersectionExpression(
		quantifiers, comparisonKeys))
}

func intersectionRuleOrderedScan(
	rt *values.RecordType,
	reverse bool,
) *plans.RecordQueryScanPlan {
	return mustIntersectionRuleConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, rt, reverse)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithPrimaryKey([]values.Value{intersectionRuleKey(rt)})
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
	keyValues := []values.Value{intersectionRuleKey(rt)}
	intr := intersectionRuleExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(refB),
		},
		keyValues,
	)
	topRef := expressions.InitialOf(intr)

	yielded := mustFireIntersectionRule(t, NewImplementIntersectionRule(), topRef)
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
	key, ok := values.AsFieldValue(plan.GetComparisonKeyValues()[0])
	if !ok || key.Path() == nil || key.Path().Len() != 1 {
		t.Fatalf("comparison key = %#v, want plan-time-baked ordinal 0", plan.GetComparisonKeyValues()[0])
	}
	accessor, ok := key.Path().Accessor(0)
	if !ok || accessor.Ordinal() != 0 {
		t.Fatalf("comparison key path = %#v, want plan-time-baked ordinal 0", key.Path().Ordinals())
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
	scanB := mustIntersectionRuleConstruct(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, rt))
	refA := expressions.InitialOf(scanA)
	refB := expressions.InitialOf(scanB)
	intr := intersectionRuleExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(refB),
		},
		[]values.Value{intersectionRuleKey(rt)},
	)
	topRef := expressions.InitialOf(intr)

	yielded := mustFireIntersectionRule(t, NewImplementIntersectionRule(), topRef)
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
	unordered := mustIntersectionRuleConstruct(
		plans.NewRecordQueryScanPlan([]string{"T"}, rt, false))
	intr := intersectionRuleExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(ordered)),
			expressions.ForEachQuantifier(expressions.InitialOf(unordered)),
		},
		[]values.Value{intersectionRuleKey(rt)},
	)

	if yielded := mustFireIntersectionRule(t,
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
	intr := intersectionRuleExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(refA),
			expressions.ForEachQuantifier(expressions.InitialOf(orderedB)),
		},
		[]values.Value{intersectionRuleKey(rt)},
	)
	topRef := expressions.InitialOf(intr)
	memo := NewMemo(topRef)

	yielded := mustFireIntersectionRuleWithMemo(t,
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
// RFC-232 resolver rejects a same-name key baked to PAYLOAD's slot, and that
// the implementation rule also declines the only admissible representation
// of that slot: an exact PAYLOAD key when both children are ordered by ID.
func TestImplementIntersectionRule_DeclinesStaleBakedOrdinal(t *testing.T) {
	t.Parallel()
	rt := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "PAYLOAD", FieldType: values.NotNullString, Ordinal: 1},
	})
	id := intersectionRuleField(rt, 0)
	scanA := mustIntersectionRuleConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, rt, false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithPrimaryKey([]values.Value{id})
	scanB := mustIntersectionRuleConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, rt, false)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong}).
		WithPrimaryKey([]values.Value{id})
	staleRequest := mustIntersectionRuleConstruct(values.FieldByNameAndOrdinal("ID", 1))
	staleID, err := values.ResolveFieldAccess(
		intersectionRuleRoot(rt), []values.FieldRequest{staleRequest})
	if err == nil || staleID != nil {
		t.Fatalf("resolve stale ID@1 = (%v, %v), want (nil, error)", staleID, err)
	}

	payload := intersectionRuleField(rt, 1)
	intr := intersectionRuleExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
			expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
		},
		[]values.Value{payload},
	)

	if yielded := mustFireIntersectionRule(t,
		NewImplementIntersectionRule(),
		expressions.InitialOf(intr),
	); len(yielded) != 0 {
		t.Fatalf("PAYLOAD comparison key over ID-ordered inputs yielded %d plans, want 0", len(yielded))
	}
}

// TestImplementIntersectionRule_NoFireWithoutComparisonKey pins that an empty
// merge key cannot establish cursor progress.
func TestImplementIntersectionRule_NoFireWithoutComparisonKey(t *testing.T) {
	t.Parallel()
	rt := intersectionRuleRecordType()
	intr := intersectionRuleExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(intersectionRuleOrderedScan(rt, false))),
			expressions.ForEachQuantifier(expressions.InitialOf(intersectionRuleOrderedScan(rt, false))),
		},
		nil,
	)

	if yielded := mustFireIntersectionRule(t,
		NewImplementIntersectionRule(),
		expressions.InitialOf(intr),
	); len(yielded) != 0 {
		t.Fatalf("ImplementIntersectionRule fired without a comparison key; yielded %d, want 0", len(yielded))
	}
}

// TestImplementIntersectionRule_RejectsEmptyIntersection pins the RFC-232
// admission guard: a set operation without a non-existential child has no
// exact result layout and cannot reach the implementation rule.
func TestImplementIntersectionRule_RejectsEmptyIntersection(t *testing.T) {
	t.Parallel()

	intr, err := expressions.NewLogicalIntersectionExpression(nil, nil)
	if err == nil || intr != nil {
		t.Fatalf("NewLogicalIntersectionExpression(nil, nil) = (%v, %v), want (nil, error)", intr, err)
	}
}

// TestPlannerWithBatchA_ImplementsIntersectionOverScan pins end-to-end Planner
// integration for two already-ordered scan alternatives.
func TestPlannerWithBatchA_ImplementsIntersectionOverScan(t *testing.T) {
	t.Parallel()
	rt := intersectionRuleRecordType()
	scanA := intersectionRuleOrderedScan(rt, false)
	scanB := intersectionRuleOrderedScan(rt, false)
	intr := intersectionRuleExpression(
		[]expressions.Quantifier{
			expressions.ForEachQuantifier(expressions.InitialOf(scanA)),
			expressions.ForEachQuantifier(expressions.InitialOf(scanB)),
		},
		[]values.Value{intersectionRuleKey(rt)},
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
	intr := intersectionRuleExpression(qs, []values.Value{intersectionRuleKey(rt)})
	topRef := expressions.InitialOf(intr)

	yielded := mustFireIntersectionRule(t, NewImplementIntersectionRule(), topRef)
	if len(yielded) != 1 {
		t.Fatalf("ImplementIntersectionRule yielded %d, want 1", len(yielded))
	}
	plan := yielded[0].(*plans.RecordQueryIntersectionPlan)
	if got := len(plan.GetInners()); got != 3 {
		t.Fatalf("intersection inners = %d, want 3", got)
	}
}
