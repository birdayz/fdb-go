package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustPredicatePushDownConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct predicate-pushdown fixture: " + err.Error())
	}
	return value
}

// ppdRowType is the single exact leaf layout shared by the direct and
// Java-ported fixtures. The nested G element is intentional: EXPLODE(G) must
// flow the record whose TWO/THREE fields the nested-select cases address.
func ppdRowType() values.Type {
	nested := values.NewRecordType("PPD_NESTED", false, []values.Field{
		{Name: "one", FieldType: values.NotNullLong},
		{Name: "two", FieldType: values.NotNullString},
		{Name: "three", FieldType: values.NotNullLong},
	})
	return values.NewRecordType("PPD_ROW", false, []values.Field{
		{Name: "A", FieldType: values.NotNullLong},
		{Name: "NAME", FieldType: values.NotNullString},
		{Name: "a", FieldType: values.NotNullLong},
		{Name: "b", FieldType: values.NotNullString},
		{Name: "c", FieldType: values.NotNullLong},
		{Name: "d", FieldType: values.NotNullLong},
		{Name: "alpha", FieldType: values.NotNullLong},
		{Name: "beta", FieldType: values.NotNullString},
		{Name: "gamma", FieldType: values.NotNullLong},
		{Name: "delta", FieldType: values.NotNullLong},
		{Name: "x", FieldType: values.NotNullLong},
		{Name: "y", FieldType: values.NotNullString},
		{Name: "id", FieldType: values.NotNullLong},
		{Name: "k", FieldType: values.NotNullLong},
		{Name: "f", FieldType: values.NewArrayType(false, values.NotNullLong)},
		{Name: "g", FieldType: values.NewArrayType(false, nested)},
		{Name: "sub", FieldType: values.NewArrayType(false, values.NotNullLong)},
	})
}

func ppdScan() *expressions.FullUnorderedScanExpression {
	return mustPredicatePushDownConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"PPD_ROW"}, ppdRowType()))
}

func ppdFlowed(quantifier expressions.Quantifier) values.QuantifiedObjectValue {
	return mustPredicatePushDownConstruct(quantifier.RequireFlowedObjectValue())
}

func ppdSelect(
	resultValue values.Value,
	quantifiers []expressions.Quantifier,
	preds []predicates.QueryPredicate,
) *expressions.SelectExpression {
	return mustPredicatePushDownConstruct(expressions.NewSelectExpression(
		resultValue, quantifiers, preds))
}

// TestPredicatePushDown_SingleQuantifierPush tests the basic case:
// a SelectExpression with one ForEach quantifier whose child is a
// SelectExpression. A predicate referencing only that quantifier's
// alias is pushed into the child.
func TestPredicatePushDown_SingleQuantifierPush(t *testing.T) {
	t.Parallel()

	// Child: SELECT * FROM scan
	scan := ppdScan()
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	childSel := ppdSelect(
		ppdFlowed(scanQ),
		[]expressions.Quantifier{scanQ},
		nil,
	)
	childRef := expressions.InitialOf(childSel)
	childQ := expressions.ForEachQuantifier(childRef)

	// Outer: SELECT childQ.* WHERE childQ.name = 'foo'
	pred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(childQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "foo"},
		},
	}
	outerSel := ppdSelect(
		ppdFlowed(childQ),
		[]expressions.Quantifier{childQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	// The outer should have zero predicates (all pushed).
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}
	// The child quantifier should now range over a Reference whose
	// member is a SelectExpression with the pushed predicate.
	newQ := result.GetQuantifiers()[0]
	newChildRef := newQ.GetRangesOver()
	if newChildRef == nil {
		t.Fatal("expected non-nil child Reference")
	}
	members := newChildRef.AllMembers()
	if len(members) == 0 {
		t.Fatal("expected at least 1 member in child Reference")
	}
	newChildSel, ok := members[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected child to be SelectExpression, got %T", members[0])
	}
	if len(newChildSel.GetPredicates()) != 1 {
		t.Fatalf("expected 1 pushed predicate, got %d", len(newChildSel.GetPredicates()))
	}
}

// TestPredicatePushDown_MultiQuantifierPartial tests that with two
// quantifiers, only predicates referencing a single quantifier are
// pushed; cross-quantifier predicates stay on the outer.
func TestPredicatePushDown_MultiQuantifierPartial(t *testing.T) {
	t.Parallel()

	// Two children: A (scan1), B (scan2)
	scan1 := ppdScan()
	scan1Ref := expressions.InitialOf(scan1)
	scan1Q := expressions.ForEachQuantifier(scan1Ref)
	childA := ppdSelect(
		ppdFlowed(scan1Q),
		[]expressions.Quantifier{scan1Q},
		nil,
	)
	childARef := expressions.InitialOf(childA)
	qA := expressions.ForEachQuantifier(childARef)

	scan2 := ppdScan()
	scan2Ref := expressions.InitialOf(scan2)
	scan2Q := expressions.ForEachQuantifier(scan2Ref)
	childB := ppdSelect(
		ppdFlowed(scan2Q),
		[]expressions.Quantifier{scan2Q},
		nil,
	)
	childBRef := expressions.InitialOf(childB)
	qB := expressions.ForEachQuantifier(childBRef)

	// Predicate on A only: qA.col = 'x'
	predA := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(qA),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "x"},
		},
	}
	// Cross-predicate: qA.id = qB.id (references both)
	predCross := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(qA),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: ppdFlowed(qB),
		},
	}

	outerSel := ppdSelect(
		ppdFlowed(qA),
		[]expressions.Quantifier{qA, qB},
		[]predicates.QueryPredicate{predA, predCross},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	// The cross-predicate stays on the outer.
	if len(result.GetPredicates()) != 1 {
		t.Fatalf("expected 1 remaining predicate on outer, got %d", len(result.GetPredicates()))
	}
}

// TestPredicatePushDown_NoPushablePredicates verifies the rule yields
// nothing when all predicates reference multiple quantifiers.
func TestPredicatePushDown_NoPushablePredicates(t *testing.T) {
	t.Parallel()

	scan1 := ppdScan()
	scan1Ref := expressions.InitialOf(scan1)
	qA := expressions.ForEachQuantifier(scan1Ref)

	scan2 := ppdScan()
	scan2Ref := expressions.InitialOf(scan2)
	qB := expressions.ForEachQuantifier(scan2Ref)

	// Cross-predicate only.
	predCross := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(qA),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: ppdFlowed(qB),
		},
	}

	outerSel := ppdSelect(
		ppdFlowed(qA),
		[]expressions.Quantifier{qA, qB},
		[]predicates.QueryPredicate{predCross},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (no pushable predicates), got %d", len(yielded))
	}
}

// TestPredicatePushDown_NoPredicates verifies the rule yields nothing
// when the SelectExpression has no predicates.
func TestPredicatePushDown_NoPredicates(t *testing.T) {
	t.Parallel()

	scan := ppdScan()
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	sel := ppdSelect(
		ppdFlowed(scanQ),
		[]expressions.Quantifier{scanQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (no predicates), got %d", len(yielded))
	}
}

// TestPredicatePushDown_PushWithExistentialSibling verifies that the
// rule pushes predicates into ForEach children even when Existential
// quantifiers are present. Matches Java's behavior: existential
// quantifiers are skipped per-quantifier, not blocked globally.
func TestPredicatePushDown_PushWithExistentialSibling(t *testing.T) {
	t.Parallel()

	// Inner: SELECT a, b FROM T (a SelectExpression the predicate can push into)
	baseScan := ppdScan()
	baseScanRef := expressions.InitialOf(baseScan)
	baseQ := expressions.ForEachQuantifier(baseScanRef)
	innerSel := ppdSelect(
		ppdFlowed(baseQ),
		[]expressions.Quantifier{baseQ},
		nil,
	)
	innerRef := expressions.InitialOf(innerSel)
	innerQ := expressions.ForEachQuantifier(innerRef)

	// EXISTS subquery
	existScan := ppdScan()
	existRef := expressions.InitialOf(existScan)
	existQ := expressions.ExistentialQuantifier(existRef)

	pred := &predicates.ComparisonPredicate{
		Operand: ppdFieldValue(innerQ, "A"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(42)},
		},
	}

	sel := ppdSelect(
		ppdFlowed(innerQ),
		[]expressions.Quantifier{innerQ, existQ},
		[]predicates.QueryPredicate{pred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), selRef)
	if len(yielded) == 0 {
		t.Fatal("expected yields: predicate should push into ForEach child despite existential sibling")
	}
}

// TestPredicatePushDown_ThroughSort tests pushing predicates through a
// LogicalSortExpression — the predicate appears in a new
// SelectExpression below the sort.
func TestPredicatePushDown_ThroughSort(t *testing.T) {
	t.Parallel()

	scan := ppdScan()
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sort := mustPredicatePushDownConstruct(expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: ppdFieldValue(scanQ, "NAME")}},
		scanQ,
	))
	sortRef := expressions.InitialOf(sort)
	sortQ := expressions.ForEachQuantifier(sortRef)

	pred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(sortQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "x"},
		},
	}

	outerSel := ppdSelect(
		ppdFlowed(sortQ),
		[]expressions.Quantifier{sortQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer (pushed through sort), got %d", len(result.GetPredicates()))
	}

	// The child of the outer should now be a sort whose child has the predicate.
	newSortRef := result.GetQuantifiers()[0].GetRangesOver()
	newSort, ok := newSortRef.AllMembers()[0].(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("expected LogicalSortExpression, got %T", newSortRef.AllMembers()[0])
	}
	// Below the sort should be a SelectExpression with the predicate.
	innerRef := newSort.GetInner().GetRangesOver()
	innerSel, ok := innerRef.AllMembers()[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression below sort, got %T", innerRef.AllMembers()[0])
	}
	if len(innerSel.GetPredicates()) != 1 {
		t.Errorf("expected 1 predicate below sort, got %d", len(innerSel.GetPredicates()))
	}
}

// TestPredicatePushDown_ThroughDistinct tests pushing predicates through
// a LogicalDistinctExpression.
func TestPredicatePushDown_ThroughDistinct(t *testing.T) {
	t.Parallel()

	scan := ppdScan()
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	distinct := mustPredicatePushDownConstruct(expressions.NewLogicalDistinctExpression(scanQ))
	distinctRef := expressions.InitialOf(distinct)
	distinctQ := expressions.ForEachQuantifier(distinctRef)

	pred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(distinctQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(42)},
		},
	}

	outerSel := ppdSelect(
		ppdFlowed(distinctQ),
		[]expressions.Quantifier{distinctQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer (pushed through distinct), got %d", len(result.GetPredicates()))
	}
}

// TestPredicatePushDown_ThroughUnion tests pushing predicates through a
// LogicalUnionExpression — the predicate is pushed into each leg.
func TestPredicatePushDown_ThroughUnion(t *testing.T) {
	t.Parallel()

	scan1 := ppdScan()
	scan1Ref := expressions.InitialOf(scan1)
	leg1Q := expressions.ForEachQuantifier(scan1Ref)

	scan2 := ppdScan()
	scan2Ref := expressions.InitialOf(scan2)
	leg2Q := expressions.ForEachQuantifier(scan2Ref)

	union := mustPredicatePushDownConstruct(expressions.NewLogicalUnionExpression(
		[]expressions.Quantifier{leg1Q, leg2Q}))
	unionRef := expressions.InitialOf(union)
	unionQ := expressions.ForEachQuantifier(unionRef)

	pred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(unionQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "pushed"},
		},
	}

	outerSel := ppdSelect(
		ppdFlowed(unionQ),
		[]expressions.Quantifier{unionQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer (pushed through union), got %d", len(result.GetPredicates()))
	}

	// The child should be a union with new legs, each wrapping a SelectExpression.
	newUnionRef := result.GetQuantifiers()[0].GetRangesOver()
	newUnion, ok := newUnionRef.AllMembers()[0].(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("expected LogicalUnionExpression, got %T", newUnionRef.AllMembers()[0])
	}
	for i, q := range newUnion.GetQuantifiers() {
		innerRef := q.GetRangesOver()
		innerSel, ok := innerRef.AllMembers()[0].(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("leg %d: expected SelectExpression, got %T", i, innerRef.AllMembers()[0])
		}
		if len(innerSel.GetPredicates()) != 1 {
			t.Errorf("leg %d: expected 1 predicate, got %d", i, len(innerSel.GetPredicates()))
		}
	}
}

// TestPredicatePushDown_IntoLogicalFilter tests pushing predicates
// into a LogicalFilterExpression — the predicate is absorbed into the
// filter's predicate list (as a new SelectExpression).
func TestPredicatePushDown_IntoLogicalFilter(t *testing.T) {
	t.Parallel()

	scan := ppdScan()
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	existingPred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := mustPredicatePushDownConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{existingPred},
		scanQ,
	))
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	pushedPred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(filterQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "pushed"},
		},
	}

	outerSel := ppdSelect(
		ppdFlowed(filterQ),
		[]expressions.Quantifier{filterQ},
		[]predicates.QueryPredicate{pushedPred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	// The child should now be a SelectExpression with 2 predicates
	// (existing + pushed).
	newRef := result.GetQuantifiers()[0].GetRangesOver()
	newSel, ok := newRef.AllMembers()[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression after push into filter, got %T", newRef.AllMembers()[0])
	}
	if len(newSel.GetPredicates()) != 2 {
		t.Errorf("expected 2 predicates (existing + pushed), got %d", len(newSel.GetPredicates()))
	}
}

// TestPredicatePushDown_IntoSelectExpression tests pushing predicates
// into a nested SelectExpression child — the predicate is translated
// through the child's result value.
func TestPredicatePushDown_IntoSelectExpression(t *testing.T) {
	t.Parallel()

	scan := ppdScan()
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	childSel := ppdSelect(
		ppdFlowed(scanQ),
		[]expressions.Quantifier{scanQ},
		nil,
	)
	childRef := expressions.InitialOf(childSel)
	childQ := expressions.ForEachQuantifier(childRef)

	pred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(childQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "pushed"},
		},
	}

	outerSel := ppdSelect(
		ppdFlowed(childQ),
		[]expressions.Quantifier{childQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	// The child SelectExpression should now have the pushed predicate.
	newRef := result.GetQuantifiers()[0].GetRangesOver()
	newSel, ok := newRef.AllMembers()[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression, got %T", newRef.AllMembers()[0])
	}
	if len(newSel.GetPredicates()) != 1 {
		t.Errorf("expected 1 pushed predicate, got %d", len(newSel.GetPredicates()))
	}
}

// TestPredicatePushDownRule_DoesNotPushIntoOuterJoinChild pins the
// nested_box_conjunct regression (TestFDB_FilteredBoxUnnest): pushIntoSelect
// must decline (return nil) when the child SelectExpression is a Go-only
// FULL/LEFT/RIGHT OUTER join (ChildrenAsSet()==false), not just when it's
// wrapped in a strict-single quantifier.
//
// Java's PredicatePushDownRule.visitSelectExpression (PushToVisitor) pushes
// into a child SelectExpression UNCONDITIONALLY, because Java's
// SelectExpression can never carry outer-join semantics — LEFT OUTER is a
// null-on-empty quantifier (already guarded, see NullOnEmptySkipped) and
// Java's Cascades has no representation for FULL OUTER at all, so every
// Java SelectExpression is inner-equivalent. Go's SelectExpression is a
// wider, documented extension that ALSO carries FULL/LEFT/RIGHT OUTER
// directly via joinType (select.go's ChildrenAsSet doc,
// RewriteOuterJoinRule's header) — a child in that shape needs the SAME
// opacity guard PushFilterBelowJoinRule, PartitionBinarySelectRule, and
// SelectMergeRule already carry (JoinInner / ChildrenAsSet checks).
//
// Absorbing a WHERE-class predicate into a FULL-OUTER child's own
// predicate list turns it into an ON-condition: the join's null-extension
// drain for an unmatched row runs regardless of that extra condition, so
// WHERE-above-OUTER silently degrades into ON-below-OUTER. This was a
// real bug: TestFDB_FilteredBoxUnnest/nested_box_conjunct returned a
// NULL-extended row through `WHERE LA.K = 100` over a nested
// `(LA FULL LB) FULL CC` box, because the rule fused the WHERE predicate
// into the FULL child's own predicate list at BOTH nesting levels.
func TestPredicatePushDownRule_DoesNotPushIntoOuterJoinChild(t *testing.T) {
	t.Parallel()

	// Child: A FULL OUTER JOIN B ON a.id = b.id — a 2-quantifier
	// SelectExpression with joinType=JoinFullOuter.
	scanA := ppdScan()
	aQ := expressions.ForEachQuantifier(expressions.InitialOf(scanA))

	scanB := ppdScan()
	bQ := expressions.ForEachQuantifier(expressions.InitialOf(scanB))

	onPred := &predicates.ComparisonPredicate{
		Operand: ppdFieldValue(aQ, "id"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: ppdFieldValue(bQ, "id"),
		},
	}

	fullChild := mustPredicatePushDownConstruct(expressions.NewSelectExpressionWithJoinType(
		ppdFlowed(aQ),
		[]expressions.Quantifier{aQ, bQ},
		[]predicates.QueryPredicate{onPred},
		nil,
		expressions.JoinFullOuter,
	))
	childRef := expressions.InitialOf(fullChild)
	childQ := expressions.ForEachQuantifier(childRef)

	// WHERE childQ.k = 100 — a WHERE-class predicate referencing only the
	// box quantifier, so the rule classifies it as pushable.
	wherePred := &predicates.ComparisonPredicate{
		Operand: ppdFieldValue(childQ, "k"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(100)},
		},
	}

	outerSel := ppdSelect(
		ppdFlowed(childQ),
		[]expressions.Quantifier{childQ},
		[]predicates.QueryPredicate{wherePred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields — a FULL OUTER child must stay opaque to predicate "+
			"absorption (fusing WHERE into its own predicate list turns it into an "+
			"ON-condition the null-extension drain bypasses), got %d: %#v", len(yielded), yielded)
	}
}

// TestPredicatePushDown_NullOnEmptySkipped verifies that ForEach
// quantifiers with nullOnEmpty=true (LEFT JOIN) are skipped.
func TestPredicatePushDown_NullOnEmptySkipped(t *testing.T) {
	t.Parallel()

	scan := ppdScan()
	scanRef := expressions.InitialOf(scan)
	nullOnEmptyQ := expressions.ForEachNullOnEmptyQuantifier(scanRef)

	pred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(nullOnEmptyQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "test"},
		},
	}

	sel := ppdSelect(
		ppdFlowed(nullOnEmptyQ),
		[]expressions.Quantifier{nullOnEmptyQ},
		[]predicates.QueryPredicate{pred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (nullOnEmpty quantifier skipped), got %d", len(yielded))
	}
}

// TestPredicatePushDown_StrictSingleSkipped verifies that a scalar
// cardinality edge is a non-pushable barrier. Moving the predicate below the
// strict FirstOrDefault could filter away a second row before it raises 21000,
// and rebuilding the edge would drop the StrictSingle flag entirely.
func TestPredicatePushDown_StrictSingleSkipped(t *testing.T) {
	t.Parallel()

	scanRef := expressions.InitialOf(ppdScan())
	strictAlias := values.NamedCorrelationIdentifier("SCALAR")
	strictQ := expressions.NamedForEachStrictSingleQuantifier(strictAlias, scanRef)
	pred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(strictQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "one-row-match"},
		},
	}
	sel := ppdSelect(
		ppdFlowed(strictQ),
		[]expressions.Quantifier{strictQ},
		[]predicates.QueryPredicate{pred},
	)

	yielded := mustFireExpressionRule(t,
		NewPredicatePushDownRule(), expressions.InitialOf(sel))
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (strictSingle quantifier skipped), got %d", len(yielded))
	}
	if !sel.GetQuantifiers()[0].IsStrictSingle() {
		t.Fatal("original strictSingle carrier was mutated")
	}
}

// TestPredicatePushDown_StrictSingleNestedChildFailsClosed covers the second
// carrier boundary: the parent quantifier itself is plain, but its child Select
// owns the strict scalar edge. Absorbing the parent predicate into that child
// would let it filter rows before strict FirstOrDefault observes row two.
func TestPredicatePushDown_StrictSingleNestedChildFailsClosed(t *testing.T) {
	t.Parallel()

	outerRef := expressions.InitialOf(ppdScan())
	scalarRef := expressions.InitialOf(ppdScan())
	outerQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("OUTER"), outerRef)
	scalarQ := expressions.NamedForEachStrictSingleQuantifier(
		values.NamedCorrelationIdentifier("SCALAR"), scalarRef)
	strictChild := mustPredicatePushDownConstruct(expressions.NewSelectExpressionWithJoinType(
		ppdFlowed(outerQ),
		[]expressions.Quantifier{outerQ, scalarQ},
		nil,
		[]string{"OUTER", "SCALAR"},
		expressions.JoinLeftOuter,
	))

	parentQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("PARENT"), expressions.InitialOf(strictChild))
	parentPredicate := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(parentQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "could-hide-row-two"},
		},
	}
	parent := ppdSelect(
		ppdFlowed(parentQ),
		[]expressions.Quantifier{parentQ},
		[]predicates.QueryPredicate{parentPredicate},
	)

	yielded := mustFireExpressionRule(t,
		NewPredicatePushDownRule(), expressions.InitialOf(parent))
	if len(yielded) != 0 {
		t.Fatalf("nested strict-single child yielded %d predicate-push rewrite(s), want zero", len(yielded))
	}
	if !strictChild.GetQuantifiers()[1].IsStrictSingle() {
		t.Fatal("nested strict-single carrier was mutated")
	}
}

// TestPredicatePushDown_ThroughUnique tests pushing predicates through
// a LogicalUniqueExpression.
func TestPredicatePushDown_ThroughUnique(t *testing.T) {
	t.Parallel()

	scan := ppdScan()
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	unique := mustPredicatePushDownConstruct(expressions.NewLogicalUniqueExpression(scanQ))
	uniqueRef := expressions.InitialOf(unique)
	uniqueQ := expressions.ForEachQuantifier(uniqueRef)

	pred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(uniqueQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}

	outerSel := ppdSelect(
		ppdFlowed(uniqueQ),
		[]expressions.Quantifier{uniqueQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer (pushed through unique), got %d", len(result.GetPredicates()))
	}
}

func TestPredicatePushDown_ThroughRequiredUniquePreservesMode(t *testing.T) {
	t.Parallel()

	scanRef := expressions.InitialOf(ppdScan())
	requiredUnique := mustPredicatePushDownConstruct(expressions.NewRequiredLogicalUniqueExpression(
		expressions.ForEachQuantifier(scanRef),
	))
	uniqueRef := expressions.InitialOf(requiredUnique)
	uniqueQ := expressions.ForEachQuantifier(uniqueRef)
	pred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(uniqueQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}
	outerSel := ppdSelect(
		ppdFlowed(uniqueQ),
		[]expressions.Quantifier{uniqueQ},
		[]predicates.QueryPredicate{pred},
	)

	yielded := mustFireExpressionRule(t,
		NewPredicatePushDownRule(),
		expressions.InitialOf(outerSel),
	)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 predicate-pushdown result, got %d", len(yielded))
	}
	result, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("pushdown result type = %T, want *SelectExpression", yielded[0])
	}
	if len(result.GetPredicates()) != 0 {
		t.Fatalf(
			"outer predicates after required-Unique pushdown = %d, want 0",
			len(result.GetPredicates()),
		)
	}

	resultQuantifiers := result.GetQuantifiers()
	if len(resultQuantifiers) != 1 {
		t.Fatalf("result quantifier count = %d, want 1", len(resultQuantifiers))
	}
	rewrittenRef := resultQuantifiers[0].GetRangesOver()
	if rewrittenRef == nil {
		t.Fatal("rewritten outer quantifier has nil child reference")
	}
	rewrittenMembers := rewrittenRef.AllMembers()
	if len(rewrittenMembers) != 1 {
		t.Fatalf("rewritten child member count = %d, want 1", len(rewrittenMembers))
	}
	rewrittenUnique, ok := rewrittenMembers[0].(*expressions.LogicalUniqueExpression)
	if !ok {
		t.Fatalf(
			"rewritten child type = %T, want *LogicalUniqueExpression",
			rewrittenMembers[0],
		)
	}
	if !rewrittenUnique.IsRequired() {
		t.Fatal("predicate pushdown dropped required LogicalUnique mode")
	}
}

// TestPredicatePushDown_UnsupportedChild verifies that pushing into an
// unsupported expression type (e.g., FullUnorderedScanExpression
// directly) yields nothing.
func TestPredicatePushDown_UnsupportedChild(t *testing.T) {
	t.Parallel()

	// Direct scan — no filter/select/sort/union/distinct/unique wrapper.
	scan := ppdScan()
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	pred := &predicates.ComparisonPredicate{
		Operand: ppdFlowed(scanQ),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "x"},
		},
	}

	outerSel := ppdSelect(
		ppdFlowed(scanQ),
		[]expressions.Quantifier{scanQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (unsupported child type), got %d", len(yielded))
	}
}

// --------------------------------------------------------------------------
// Tests ported from Java's PredicatePushDownRuleTest
// --------------------------------------------------------------------------

// ppd helper: build a SelectExpression projecting specific columns from a
// quantifier, with optional predicates. Result value is a
// RecordConstructorValue with the given field names sourced from the
// quantifier. Uses the same column names for source and result.
func ppdSelectWithColumns(qun expressions.Quantifier, columns []string, preds ...predicates.QueryPredicate) *expressions.SelectExpression {
	if len(columns) == 0 {
		return ppdSelect(ppdFlowed(qun), []expressions.Quantifier{qun}, preds)
	}
	fields := make([]values.RecordConstructorField, len(columns))
	for i, c := range columns {
		fields[i] = values.RecordConstructorField{
			Name:  c,
			Value: ppdFieldValue(qun, c),
		}
	}
	rv := values.NewRecordConstructorValue(fields...)
	return ppdSelect(rv, []expressions.Quantifier{qun}, preds)
}

// ppdSelectWithRenames builds a SelectExpression projecting columns with
// renames. sourceToOutput maps source field name -> output field name.
func ppdSelectWithRenames(qun expressions.Quantifier, sourceToOutput map[string]string, preds ...predicates.QueryPredicate) *expressions.SelectExpression {
	fields := make([]values.RecordConstructorField, 0, len(sourceToOutput))
	for src, out := range sourceToOutput {
		fields = append(fields, values.RecordConstructorField{
			Name:  out,
			Value: ppdFieldValue(qun, src),
		})
	}
	rv := values.NewRecordConstructorValue(fields...)
	return ppdSelect(rv, []expressions.Quantifier{qun}, preds)
}

// ppdFieldPred creates a ComparisonPredicate on a quantifier's named field.
// Mirrors Java's fieldPredicate(qun, fieldName, comparison).
func ppdFieldPred(q expressions.Quantifier, field string, cmp predicates.Comparison) *predicates.ComparisonPredicate {
	return &predicates.ComparisonPredicate{
		Operand:    ppdFieldValue(q, field),
		Comparison: cmp,
	}
}

// ppdFieldValue creates a FieldValue referencing a quantifier's field.
func ppdFieldValue(q expressions.Quantifier, field string) values.Value {
	request := mustPredicatePushDownConstruct(values.FieldByName(field))
	return mustPredicatePushDownConstruct(values.ResolveFieldAccess(
		ppdFlowed(q), []values.FieldRequest{request}))
}

// ppdJoinSelect creates a SelectExpression over two quantifiers (a join),
// using a RecordConstructorValue built from the provided columns.
// Each column specifies which quantifier and field name to project.
type ppdJoinColumn struct {
	Qun   expressions.Quantifier
	Field string
	Alias string
}

func ppdJoinSelect(quns []expressions.Quantifier, columns []ppdJoinColumn, preds ...predicates.QueryPredicate) *expressions.SelectExpression {
	fields := make([]values.RecordConstructorField, len(columns))
	for i, c := range columns {
		fields[i] = values.RecordConstructorField{
			Name:  c.Alias,
			Value: ppdFieldValue(c.Qun, c.Field),
		}
	}
	rv := values.NewRecordConstructorValue(fields...)
	return ppdSelect(rv, quns, preds)
}

// --------------------------------------------------------------------------
// Ported test: pushDownMultiplePredicates
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushMultiplePredicates ports Java's
// pushDownMultiplePredicates:
//
//	SELECT b FROM (SELECT a, b FROM T) WHERE a = 42 AND b > 'hello'
//	=> SELECT b FROM (SELECT a, b FROM T WHERE a = 42 AND b > 'hello')
func TestPredicatePushDownRule_PushMultiplePredicates(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	lowerSel := ppdSelectWithColumns(baseQun, []string{"a", "b"})
	lowerQun := forEachOf(lowerSel)

	// Outer: SELECT b WHERE a = 42 AND b > 'hello'
	pred1 := ppdFieldPred(lowerQun, "a", predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)))
	pred2 := ppdFieldPred(lowerQun, "b", predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "hello"))

	higher := ppdSelectWithColumns(lowerQun, []string{"b"}, pred1, pred2)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	newChildRef := result.GetQuantifiers()[0].GetRangesOver()
	newChildSel := newChildRef.AllMembers()[0].(*expressions.SelectExpression)
	if len(newChildSel.GetPredicates()) != 2 {
		t.Fatalf("expected 2 pushed predicates, got %d", len(newChildSel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: pushDownParameterPredicate
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushParameterPredicate ports Java's
// pushDownParameterPredicate:
//
//	SELECT b FROM (SELECT a, b FROM T) WHERE a = $p
//	=> SELECT b FROM (SELECT a, b FROM T WHERE a = $p)
func TestPredicatePushDownRule_PushParameterPredicate(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	lowerSel := ppdSelectWithColumns(baseQun, []string{"a", "b"})
	lowerQun := forEachOf(lowerSel)

	// Predicate: a = $param (parameter-bound comparison)
	pred := ppdFieldPred(lowerQun, "a", predicates.Comparison{
		Type:          predicates.ComparisonEquals,
		ParameterName: "param",
	})

	higher := ppdSelectWithColumns(lowerQun, []string{"b"}, pred)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	newChildSel := result.GetQuantifiers()[0].GetRangesOver().AllMembers()[0].(*expressions.SelectExpression)
	if len(newChildSel.GetPredicates()) != 1 {
		t.Fatalf("expected 1 pushed predicate, got %d", len(newChildSel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: pushDownFieldValuePredicate
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushFieldValuePredicate ports Java's
// pushDownFieldValuePredicate:
//
//	SELECT a FROM (SELECT a, b, d FROM T) WHERE b = d
//	=> SELECT a FROM (SELECT a, b, d FROM T WHERE b = d)
func TestPredicatePushDownRule_PushFieldValuePredicate(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	lowerSel := ppdSelectWithColumns(baseQun, []string{"a", "b", "d"})
	lowerQun := forEachOf(lowerSel)

	// Predicate: b = d (value comparison where RHS is also a field)
	pred := ppdFieldPred(lowerQun, "b",
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: ppdFieldValue(lowerQun, "d"),
		},
	)

	higher := ppdSelectWithColumns(lowerQun, []string{"a"}, pred)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	newChildSel := result.GetQuantifiers()[0].GetRangesOver().AllMembers()[0].(*expressions.SelectExpression)
	if len(newChildSel.GetPredicates()) != 1 {
		t.Fatalf("expected 1 pushed predicate, got %d", len(newChildSel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: pushDownConstantValuePredicate
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushConstantValuePredicate ports Java's
// pushDownConstantValuePredicate:
//
//	SELECT a, b FROM (SELECT a, b, c FROM T) WHERE c = @1
//	=> SELECT a, b FROM (SELECT a, b, c FROM T WHERE c = @1)
func TestPredicatePushDownRule_PushConstantValuePredicate(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	constAlias := values.UniqueCorrelationIdentifier()
	constVal := values.NewConstantObjectValue(constAlias, "1", values.NotNullBytes)

	lowerSel := ppdSelectWithColumns(baseQun, []string{"a", "b", "c"})
	lowerQun := forEachOf(lowerSel)

	pred := ppdFieldPred(lowerQun, "c",
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: constVal,
		},
	)

	higher := ppdSelectWithColumns(lowerQun, []string{"a", "b"}, pred)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	newChildSel := result.GetQuantifiers()[0].GetRangesOver().AllMembers()[0].(*expressions.SelectExpression)
	if len(newChildSel.GetPredicates()) != 1 {
		t.Fatalf("expected 1 pushed predicate, got %d", len(newChildSel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: pushDownToPlaceWithExistingPredicates
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushToExistingPredicates ports Java's
// pushDownToPlaceWithExistingPredicates:
//
//	SELECT a FROM (SELECT a, b, c FROM T WHERE b = @1) WHERE c = @2
//	=> SELECT a FROM (SELECT a, b, c FROM T WHERE b = @1 AND c = @2)
func TestPredicatePushDownRule_PushToExistingPredicates(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	existingPred := ppdFieldPred(baseQun, "b", predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: values.NewConstantObjectValue(values.UniqueCorrelationIdentifier(), "1", values.NotNullBytes),
	})

	lowerSel := ppdSelectWithColumns(baseQun, []string{"a", "b", "c"}, existingPred)
	lowerQun := forEachOf(lowerSel)

	pushedPred := ppdFieldPred(lowerQun, "c", predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: values.NewConstantObjectValue(values.UniqueCorrelationIdentifier(), "2", values.NullableString),
	})

	higher := ppdSelectWithColumns(lowerQun, []string{"a"}, pushedPred)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	newChildSel := result.GetQuantifiers()[0].GetRangesOver().AllMembers()[0].(*expressions.SelectExpression)
	if len(newChildSel.GetPredicates()) != 2 {
		t.Fatalf("expected 2 predicates (existing + pushed), got %d", len(newChildSel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: pushDownOrPredicate
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushOrPredicate ports Java's pushDownOrPredicate:
//
//	SELECT a, b FROM (SELECT a, b, d FROM T) WHERE a = $p OR b > 'hello' OR @1 = d
//	=> SELECT a, b FROM (SELECT a, b, d FROM T WHERE a = $p OR b > 'hello' OR @1 = d)
func TestPredicatePushDownRule_PushOrPredicate(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	lowerSel := ppdSelectWithColumns(baseQun, []string{"a", "b", "d"})
	lowerQun := forEachOf(lowerSel)

	orPred := predicates.NewOr(
		ppdFieldPred(lowerQun, "a", predicates.Comparison{
			Type:          predicates.ComparisonEquals,
			ParameterName: "p",
		}),
		ppdFieldPred(lowerQun, "b", predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "hello")),
		ppdFieldPred(lowerQun, "d", predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewConstantObjectValue(values.UniqueCorrelationIdentifier(), "1", values.NullableString),
		}),
	)

	higher := ppdSelectWithColumns(lowerQun, []string{"a", "b"}, orPred)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	newChildSel := result.GetQuantifiers()[0].GetRangesOver().AllMembers()[0].(*expressions.SelectExpression)
	if len(newChildSel.GetPredicates()) != 1 {
		t.Fatalf("expected 1 pushed OR predicate, got %d", len(newChildSel.GetPredicates()))
	}
	if _, ok := newChildSel.GetPredicates()[0].(*predicates.OrPredicate); !ok {
		t.Fatalf("expected pushed predicate to be OrPredicate, got %T", newChildSel.GetPredicates()[0])
	}
}

// --------------------------------------------------------------------------
// Ported test: pushSimplePredicateIntoLogicalFilter (empty filter)
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushIntoEmptyLogicalFilter ports Java's
// pushSimplePredicateIntoLogicalFilter:
//
//	SELECT c FROM (FILTER T WHERE <empty>) WHERE b > 'hello'
//	=> SELECT c FROM (SELECT * FROM T WHERE b > 'hello')
func TestPredicatePushDownRule_PushIntoEmptyLogicalFilter(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	// LogicalFilterExpression with no predicates (empty filter).
	filter := mustPredicatePushDownConstruct(expressions.NewLogicalFilterExpression(nil, baseQun))
	filterQun := forEachOf(filter)

	pred := ppdFieldPred(filterQun, "b", predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "hello"))

	sel := ppdSelectWithColumns(filterQun, []string{"c"}, pred)
	selRef := expressions.InitialOf(sel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), selRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	// The pushed result should be a SelectExpression with the predicate.
	newChildSel := result.GetQuantifiers()[0].GetRangesOver().AllMembers()[0].(*expressions.SelectExpression)
	if len(newChildSel.GetPredicates()) != 1 {
		t.Fatalf("expected 1 pushed predicate, got %d", len(newChildSel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: pushDownMultiplePredicatesToLogicalFilter
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushMultipleToLogicalFilter ports Java's
// pushDownMultiplePredicatesToLogicalFilter:
//
//	SELECT b, c FROM (FILTER T WHERE a = 42) WHERE b > d AND c != @1
//	=> SELECT b, c FROM (SELECT * FROM T WHERE a = 42 AND b > d AND c != @1)
func TestPredicatePushDownRule_PushMultipleToLogicalFilter(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	filterPred := ppdFieldPred(baseQun, "a", predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)))
	filter := mustPredicatePushDownConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{filterPred}, baseQun))
	filterQun := forEachOf(filter)

	pred1 := ppdFieldPred(filterQun, "b", predicates.Comparison{
		Type:    predicates.ComparisonGreaterThan,
		Operand: ppdFieldValue(filterQun, "d"),
	})
	pred2 := ppdFieldPred(filterQun, "c", predicates.Comparison{
		Type:    predicates.ComparisonNotEquals,
		Operand: values.NewConstantObjectValue(values.UniqueCorrelationIdentifier(), "1", values.NotNullBytes),
	})

	sel := ppdSelectWithColumns(filterQun, []string{"b", "c"}, pred1, pred2)
	selRef := expressions.InitialOf(sel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), selRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	// The child should now have 3 predicates: existing a=42, pushed b>d, pushed c!=@1.
	newChildSel := result.GetQuantifiers()[0].GetRangesOver().AllMembers()[0].(*expressions.SelectExpression)
	if len(newChildSel.GetPredicates()) != 3 {
		t.Fatalf("expected 3 predicates (1 existing + 2 pushed), got %d", len(newChildSel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: doNotPushNullCheckIntoSelectWithNullOnEmpty
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_DoNotPushNullCheckIntoNullOnEmpty ports Java's
// doNotPushNullCheckIntoSelectWithNullOnEmpty. IS NULL must not be pushed
// into a null-on-empty quantifier because the null might come from the
// injected null, not from actual data.
func TestPredicatePushDownRule_DoNotPushNullCheckIntoNullOnEmpty(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	lowerSel := ppdSelectWithColumns(baseQun, []string{"a", "b", "c", "d"},
		ppdFieldPred(baseQun, "d", predicates.NewLiteralComparison(predicates.ComparisonStartsWith, "blah")),
	)
	nullOnEmptyQun := expressions.ForEachNullOnEmptyQuantifier(expressions.InitialOf(lowerSel))

	// IS NULL on b — cannot push through null-on-empty.
	pred := ppdFieldPred(nullOnEmptyQun, "b", predicates.Comparison{
		Type: predicates.ComparisonIsNull,
	})

	higher := ppdSelectWithColumns(nullOnEmptyQun, []string{"a", "c"}, pred)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (IS NULL + null-on-empty), got %d", len(yielded))
	}
}

// --------------------------------------------------------------------------
// Ported test: canPushDownToMultipleChildren
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushToMultipleChildren ports Java's
// canPushDownToMultipleChildren: when a Reference has multiple member
// expressions that all accept a pushed predicate, all get the predicate.
func TestPredicatePushDownRule_PushToMultipleChildren(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	lower1 := ppdSelectWithColumns(baseQun, []string{"a", "b", "c"})
	lower2 := ppdSelectWithColumns(baseQun, []string{"a", "b", "c"},
		predicates.NewConstantPredicate(predicates.TriTrue))

	// Create a Reference with two members.
	lowerRef := expressions.InitialOf(lower1)
	lowerRef.Insert(lower2)
	lowerQun := expressions.ForEachQuantifier(lowerRef)

	pred := ppdFieldPred(lowerQun, "a", predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)))

	higher := ppdSelectWithColumns(lowerQun, []string{"b", "c"}, pred)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	// The child Reference should have two members, each with the pushed predicate.
	newChildRef := result.GetQuantifiers()[0].GetRangesOver()
	members := newChildRef.AllMembers()
	if len(members) < 2 {
		t.Fatalf("expected at least 2 members in child Reference, got %d", len(members))
	}
	for i, m := range members {
		sel, ok := m.(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("member %d: expected SelectExpression, got %T", i, m)
		}
		// lower1 had 0 preds, lower2 had 1 (ConstantPredicate). After push,
		// lower1 should have 1, lower2 should have 2.
		if i == 0 && len(sel.GetPredicates()) != 1 {
			t.Errorf("member 0: expected 1 predicate, got %d", len(sel.GetPredicates()))
		}
		if i == 1 && len(sel.GetPredicates()) != 2 {
			t.Errorf("member 1: expected 2 predicates, got %d", len(sel.GetPredicates()))
		}
	}
}

// --------------------------------------------------------------------------
// Ported test: canPushDownToSomeChildren
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushToSomeChildren ports Java's
// canPushDownToSomeChildren: when a Reference has multiple members and only
// some accept the predicate (others are unsupported types), the result
// contains only the accepting members.
func TestPredicatePushDownRule_PushToSomeChildren(t *testing.T) {
	t.Parallel()

	// Second member: a SelectExpression (supported for push).
	baseQun2, _ := baseLeaf(t)
	selectAll := selectWithPreds(t, baseQun2)
	// Both alternatives inhabit the same memo group, so the unsupported scan
	// must flow exactly the row projected by selectAll.
	scan := selectMergeScan(t)

	baseRef := expressions.InitialOf(scan)
	baseRef.Insert(selectAll)
	baseQun := expressions.ForEachQuantifier(baseRef)

	pred := ppdFieldPred(baseQun, "b", predicates.Comparison{
		Type:          predicates.ComparisonEquals,
		ParameterName: "p",
	})

	higher := ppdSelectWithColumns(baseQun, []string{"a", "c"}, pred)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	// The child Reference should contain only the SelectExpression member
	// (the scan was unsupported and filtered out).
	newChildRef := result.GetQuantifiers()[0].GetRangesOver()
	members := newChildRef.AllMembers()
	if len(members) != 1 {
		t.Fatalf("expected 1 member (only supported child), got %d", len(members))
	}
	sel, ok := members[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression, got %T", members[0])
	}
	if len(sel.GetPredicates()) != 1 {
		t.Errorf("expected 1 pushed predicate, got %d", len(sel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: testDoesNotPushJoinCriteria
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_DoesNotPushJoinCriteria ports Java's
// testDoesNotPushJoinCriteria. A join predicate (x.b = y.beta) referencing
// two quantifiers cannot be pushed to either side.
func TestPredicatePushDownRule_DoesNotPushJoinCriteria(t *testing.T) {
	t.Parallel()

	baseT, _ := baseLeaf(t)
	baseTau, _ := baseLeaf(t)

	tLow := ppdSelectWithColumns(baseT, []string{"a", "b"})
	tLowQun := forEachOf(tLow)

	tauLow := ppdSelectWithColumns(baseTau, []string{"alpha", "beta"})
	tauLowQun := forEachOf(tauLow)

	// Join predicate: t.b = tau.beta (crosses both quantifiers).
	joinPred := &predicates.ComparisonPredicate{
		Operand: ppdFieldValue(tLowQun, "b"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: ppdFieldValue(tauLowQun, "beta"),
		},
	}

	joinSel := ppdJoinSelect(
		[]expressions.Quantifier{tLowQun, tauLowQun},
		[]ppdJoinColumn{
			{tLowQun, "a", "a"},
			{tauLowQun, "alpha", "alpha"},
		},
		joinPred,
	)
	joinRef := expressions.InitialOf(joinSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), joinRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (join criteria not pushable), got %d", len(yielded))
	}
}

// --------------------------------------------------------------------------
// Ported test: doesNotPushOrWithMixedJoinElements
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_DoesNotPushOrMixedJoin ports Java's
// doesNotPushOrWithMixedJoinElements: an OR predicate spanning two join
// legs cannot be pushed down.
func TestPredicatePushDownRule_DoesNotPushOrMixedJoin(t *testing.T) {
	t.Parallel()

	baseT, _ := baseLeaf(t)
	baseTau, _ := baseLeaf(t)

	tLow := ppdSelectWithColumns(baseT, []string{"a", "b"})
	tLowQun := forEachOf(tLow)

	tauLow := ppdSelectWithColumns(baseTau, []string{"alpha", "beta"})
	tauLowQun := forEachOf(tauLow)

	// Join predicate: t.a = tau.alpha
	joinPred := &predicates.ComparisonPredicate{
		Operand: ppdFieldValue(tLowQun, "a"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: ppdFieldValue(tauLowQun, "alpha"),
		},
	}

	// OR predicate spanning both legs: (t.b > 'hello' OR tau.beta > 'hello')
	orPred := predicates.NewOr(
		ppdFieldPred(tLowQun, "b", predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "hello")),
		ppdFieldPred(tauLowQun, "beta", predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "hello")),
	)

	joinSel := ppdJoinSelect(
		[]expressions.Quantifier{tLowQun, tauLowQun},
		[]ppdJoinColumn{
			{tLowQun, "b", "b"},
			{tauLowQun, "beta", "beta"},
		},
		joinPred, orPred,
	)
	joinRef := expressions.InitialOf(joinSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), joinRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (OR spans both join legs), got %d", len(yielded))
	}
}

// --------------------------------------------------------------------------
// Ported test: testPartitionPredicatesByJoinSource
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PartitionByJoinSource ports Java's
// testPartitionPredicatesByJoinSource: predicates that reference only one
// leg of a join are pushed to that leg; join-criteria and cross-leg
// predicates remain.
//
//	SELECT t.a, tau.alpha
//	  FROM (SELECT a, b, c FROM t) t, (SELECT alpha, beta, gamma FROM tau) tau
//	  WHERE t.b = tau.beta AND t.c = @1 AND tau.gamma = @2
//
// Expects two yields: one pushing t.c=@1 to t, one pushing tau.gamma=@2 to tau.
func TestPredicatePushDownRule_PartitionByJoinSource(t *testing.T) {
	t.Parallel()

	baseT, _ := baseLeaf(t)
	baseTau, _ := baseLeaf(t)

	tLow := ppdSelectWithColumns(baseT, []string{"a", "b", "c"})
	tLowQun := forEachOf(tLow)

	tauLow := ppdSelectWithColumns(baseTau, []string{"alpha", "beta", "gamma"})
	tauLowQun := forEachOf(tauLow)

	// Join predicate: t.b = tau.beta
	joinPred := &predicates.ComparisonPredicate{
		Operand: ppdFieldValue(tLowQun, "b"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: ppdFieldValue(tauLowQun, "beta"),
		},
	}

	// t-only predicate: t.c = @1
	tPred := ppdFieldPred(tLowQun, "c", predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: values.NewConstantObjectValue(values.UniqueCorrelationIdentifier(), "1", values.NotNullBytes),
	})

	// tau-only predicate: tau.gamma = @2
	tauPred := ppdFieldPred(tauLowQun, "gamma", predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: values.NewConstantObjectValue(values.UniqueCorrelationIdentifier(), "2", values.NotNullBytes),
	})

	joinSel := ppdJoinSelect(
		[]expressions.Quantifier{tLowQun, tauLowQun},
		[]ppdJoinColumn{
			{tLowQun, "a", "a"},
			{tauLowQun, "alpha", "alpha"},
		},
		joinPred, tPred, tauPred,
	)
	joinRef := expressions.InitialOf(joinSel)

	// Go's rule fires once per quantifier and returns after the first
	// quantifier that has pushable predicates. So we get one yield that
	// pushes t.c=@1 into t's child.
	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), joinRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	// The result should still have the join predicate and the tau predicate
	// (the t-only predicate was pushed down).
	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 2 {
		t.Fatalf("expected 2 remaining predicates (join + tau), got %d", len(result.GetPredicates()))
	}

	// Now fire the rule again on the result to push the tau predicate.
	resultRef := expressions.InitialOf(result)
	yielded2 := mustFireExpressionRule(t, NewPredicatePushDownRule(), resultRef)
	if len(yielded2) < 1 {
		t.Fatalf("second pass: expected at least 1 yield, got %d", len(yielded2))
	}
	result2 := yielded2[0].(*expressions.SelectExpression)
	// Only the join predicate should remain.
	if len(result2.GetPredicates()) != 1 {
		t.Fatalf("second pass: expected 1 remaining predicate (join only), got %d", len(result2.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: rewritePushedPredicatesOntoAppropriateJoinSource
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_RewritePredicatesOntoJoinSource ports Java's
// rewritePushedPredicatesOntoAppropriateJoinSource. Predicates on a select
// above a join are pushed into the join body.
//
//	SELECT b, c1, c2 FROM (
//	  SELECT t.a AS a1, tau.alpha AS a2, t.b, t.c AS c1, tau.gamma AS c2
//	  FROM t, tau WHERE t.b = tau.beta
//	) WHERE a1 = 42 AND a2 = $param
//
// Becomes:
//
//	SELECT b, c1, c2 FROM (
//	  SELECT ... FROM t, tau WHERE t.b = tau.beta AND t.a = 42 AND tau.alpha = $param
//	)
func TestPredicatePushDownRule_RewritePredicatesOntoJoinSource(t *testing.T) {
	t.Parallel()

	baseT, _ := baseLeaf(t)
	baseTau, _ := baseLeaf(t)

	// Inner join: t.b = tau.beta
	joinPred := &predicates.ComparisonPredicate{
		Operand: ppdFieldValue(baseT, "b"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: ppdFieldValue(baseTau, "beta"),
		},
	}

	// Build the inner join select with renamed columns.
	innerJoinSel := ppdJoinSelect(
		[]expressions.Quantifier{baseT, baseTau},
		[]ppdJoinColumn{
			{baseT, "a", "a1"},
			{baseTau, "alpha", "a2"},
			{baseT, "b", "b"},
			{baseT, "c", "c1"},
			{baseTau, "gamma", "c2"},
		},
		joinPred,
	)
	joinQun := forEachOf(innerJoinSel)

	// Outer predicates on renamed columns.
	pred1 := ppdFieldPred(joinQun, "a1", predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)))
	pred2 := ppdFieldPred(joinQun, "a2", predicates.Comparison{
		Type:          predicates.ComparisonEquals,
		ParameterName: "param",
	})

	outer := ppdSelectWithColumns(joinQun, []string{"b", "c1", "c2"}, pred1, pred2)
	outerRef := expressions.InitialOf(outer)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	// Both predicates should have been pushed into the join.
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	// The inner join should now have 3 predicates: join + a1=42 + a2=$param.
	newJoinSel := result.GetQuantifiers()[0].GetRangesOver().AllMembers()[0].(*expressions.SelectExpression)
	if len(newJoinSel.GetPredicates()) != 3 {
		t.Fatalf("expected 3 predicates in join (1 existing + 2 pushed), got %d", len(newJoinSel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: pushDownPredicateOnRepeatedNested (uses ExplodeExpression)
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushThroughExplodeNestedSelect ports Java's
// pushDownPredicateOnRepeatedNested. A predicate on a select over an
// exploded array subquery is pushed into the subquery.
//
//	SELECT a, three FROM t, (SELECT two, three FROM EXPLODE(t.g)) sub
//	  WHERE b > 'hello' AND sub.two = $p
//
// The sub.two = $p predicate is pushable to the sub select; b > 'hello'
// is on baseQun (which is a FullUnorderedScan, unsupported for push).
func TestPredicatePushDownRule_PushThroughExplodeNestedSelect(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	// EXPLODE(baseQun.g)
	explode := mustPredicatePushDownConstruct(expressions.NewExplodeExpression(
		ppdFieldValue(baseQun, "g")))
	explodeQun := forEachOf(explode)

	// Nested select: SELECT two, three FROM explode
	nestedSel := ppdSelectWithColumns(explodeQun, []string{"two", "three"})
	nestedSelQun := forEachOf(nestedSel)

	// Predicates:
	// 1. baseQun.b > 'hello' (cannot push: baseQun child is FullUnorderedScan)
	pred1 := ppdFieldPred(baseQun, "b", predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "hello"))
	// 2. nestedSelQun.two = $p (pushable into nestedSel)
	pred2 := ppdFieldPred(nestedSelQun, "two", predicates.Comparison{
		Type:          predicates.ComparisonEquals,
		ParameterName: "p",
	})

	topSel := ppdJoinSelect(
		[]expressions.Quantifier{baseQun, nestedSelQun},
		[]ppdJoinColumn{
			{baseQun, "a", "a"},
			{nestedSelQun, "three", "three"},
		},
		pred1, pred2,
	)
	topRef := expressions.InitialOf(topSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), topRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	// pred1 stays (baseQun is unsupported), pred2 is pushed.
	if len(result.GetPredicates()) != 1 {
		t.Fatalf("expected 1 remaining predicate, got %d", len(result.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: doNotPushDownToExistential
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushForEachPredicateWithExistentialSibling ports
// Java's doNotPushDownToExistential. The predicate referencing the ForEach
// quantifier is pushed into its child (a SelectExpression); the EXISTS
// predicate stays at the top level.
func TestPredicatePushDownRule_PushForEachPredicateWithExistentialSibling(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	// Wrap baseQun in a SelectExpression so the push has a target
	innerSel := selectWithPreds(t, baseQun)
	innerQun := forEachOf(innerSel)

	// EXPLODE(innerQun.g)
	explode := mustPredicatePushDownConstruct(expressions.NewExplodeExpression(
		ppdFieldValue(innerQun, "g")))
	explodeQun := forEachOf(explode)

	// EXISTS subquery
	existsInner := selectWithPreds(t, explodeQun,
		ppdFieldPred(explodeQun, "two", predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "hello")),
	)
	existsQun := existentialOf(existsInner)

	// Top: SELECT a FROM (SELECT ... FROM T) WHERE EXISTS(...) AND b > 'hello'
	existsPred := mustExistentialAlias(t, existsQun.GetAlias())
	bPred := ppdFieldPred(innerQun, "b", predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "hello"))

	topSel := ppdSelect(
		ppdFlowed(innerQun),
		[]expressions.Quantifier{innerQun, existsQun},
		[]predicates.QueryPredicate{existsPred, bPred},
	)
	topRef := expressions.InitialOf(topSel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), topRef)
	if len(yielded) == 0 {
		t.Fatal("expected yields: bPred should push into ForEach child despite existential sibling")
	}
}

// --------------------------------------------------------------------------
// Ported test: pushDownWithFieldRenames
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_PushWithFieldRenames ports Java's
// testPushDownWithFieldRenames:
//
//	SELECT y FROM (SELECT a AS x, b AS y FROM T) WHERE x = 42 AND y > 'hello'
//	=> SELECT y FROM (SELECT a AS x, b AS y FROM T WHERE a = 42 AND b > 'hello')
//
// After push-down through the child's result value (RecordConstructorValue),
// predicates are translated to reference the child's source fields.
func TestPredicatePushDownRule_PushWithFieldRenames(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	// Lower: SELECT a AS x, b AS y FROM base
	lowerSel := ppdSelectWithRenames(baseQun, map[string]string{"a": "x", "b": "y"})
	lowerQun := forEachOf(lowerSel)

	// Outer predicates on renamed columns: x = 42 AND y > 'hello'
	pred1 := ppdFieldPred(lowerQun, "x", predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)))
	pred2 := ppdFieldPred(lowerQun, "y", predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, "hello"))

	higher := ppdSelectWithColumns(lowerQun, []string{"y"}, pred1, pred2)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	// The child SelectExpression now has the pushed predicates.
	newChildSel := result.GetQuantifiers()[0].GetRangesOver().AllMembers()[0].(*expressions.SelectExpression)
	if len(newChildSel.GetPredicates()) != 2 {
		t.Fatalf("expected 2 pushed predicates, got %d", len(newChildSel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: testRenameFieldComparisonWithValues
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_RenameFieldComparison ports Java's
// testRenameFieldComparisonWithValues:
//
//	SELECT x, y, z FROM (SELECT a AS x, b AS y, d AS z FROM T) WHERE y > z
//	=> SELECT x, y, z FROM (SELECT a AS x, b AS y, d AS z FROM T WHERE b > d)
func TestPredicatePushDownRule_RenameFieldComparison(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	lowerSel := ppdSelectWithRenames(baseQun, map[string]string{"a": "x", "b": "y", "d": "z"})
	lowerQun := forEachOf(lowerSel)

	// Predicate: y > z (field comparison through renames)
	pred := ppdFieldPred(lowerQun, "y", predicates.Comparison{
		Type:    predicates.ComparisonGreaterThan,
		Operand: ppdFieldValue(lowerQun, "z"),
	})

	higher := ppdSelectWithColumns(lowerQun, []string{"x", "y", "z"}, pred)
	higherRef := expressions.InitialOf(higher)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), higherRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer, got %d", len(result.GetPredicates()))
	}

	newChildSel := result.GetQuantifiers()[0].GetRangesOver().AllMembers()[0].(*expressions.SelectExpression)
	if len(newChildSel.GetPredicates()) != 1 {
		t.Fatalf("expected 1 pushed predicate, got %d", len(newChildSel.GetPredicates()))
	}
}

// --------------------------------------------------------------------------
// Ported test: doNotPushDownPredicateOnAggregateValueNestedResult /
//              doNotPushDownPredicateOnAggregateValueFlattenedResult
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_DoNotPushThroughGroupBy ports Java's
// doNotPushDownPredicateOnAggregateValueNestedResult and
// doNotPushDownPredicateOnAggregateValueFlattenedResult. GroupByExpression
// is not handled by the push-down rule, so predicates cannot be pushed
// through it.
func TestPredicatePushDownRule_DoNotPushThroughGroupBy(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)

	groupBy := mustPredicatePushDownConstruct(expressions.NewGroupByExpression(
		[]values.Value{
			ppdFieldValue(baseQun, "b"),
			ppdFieldValue(baseQun, "c"),
		},
		[]expressions.AggregateSpec{
			{Function: expressions.AggSum, Operand: ppdFieldValue(baseQun, "a"), Alias: "sum"},
		},
		baseQun,
	))
	groupByQun := forEachOf(groupBy)

	// HAVING SUM < @1 — GroupBy's exact native output names a GROUPING key by
	// the key value's own spelling (AggregateKeyColumnName takes it verbatim,
	// because that column exists in the row it reads and in the projection
	// above it), and an AGGREGATE by its upper-folded alias.
	pred := ppdFieldPred(groupByQun, "SUM", predicates.Comparison{
		Type:    predicates.ComparisonLessThan,
		Operand: values.NewConstantObjectValue(values.UniqueCorrelationIdentifier(), "1", values.NotNullLong),
	})

	sel := ppdSelectWithColumns(groupByQun, []string{"b", "c", "SUM"}, pred)
	selRef := expressions.InitialOf(sel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (cannot push through GroupBy), got %d", len(yielded))
	}
}

// --------------------------------------------------------------------------
// Ported test: pushDownOnePredicateOfMultiple (divergence)
// --------------------------------------------------------------------------

// TestPredicatePushDownRule_OneOfMultipleWithExistential ports Java's
// pushDownOnePredicateOfMultiple. The rule pushes pred1 (a=42) into the
// ForEach child while leaving the EXISTS predicate at the top level.
// Now matches Java's behavior after removing the existential guard.
func TestPredicatePushDownRule_OneOfMultipleWithExistential(t *testing.T) {
	t.Parallel()

	baseQun, _ := baseLeaf(t)
	baseTauQun, _ := baseLeaf(t)

	lowerSel := ppdSelectWithColumns(baseQun, []string{"a", "b"})
	lowerQun := forEachOf(lowerSel)

	existsInner := ppdSelectWithColumns(baseTauQun, []string{"alpha"})
	existsQun := existentialOf(existsInner)

	pred1 := ppdFieldPred(lowerQun, "a", predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(42)))
	existsPred := mustExistentialAlias(t, existsQun.GetAlias())

	sel := ppdSelect(
		ppdFlowed(lowerQun),
		[]expressions.Quantifier{lowerQun, existsQun},
		[]predicates.QueryPredicate{pred1, existsPred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := mustFireExpressionRule(t, NewPredicatePushDownRule(), selRef)
	if len(yielded) == 0 {
		t.Fatal("expected yields: pred1 (a=42) should push into ForEach child despite existential sibling")
	}
}

// --------------------------------------------------------------------------
// Ported test: pushDownGroupingValuePredicateWithNestedResult (disabled)
// --------------------------------------------------------------------------

// Java's pushDownGroupingValuePredicateWithNestedResult and
// pushDownGroupingValuePredicateWithFlattenedResults are @Disabled in Java
// ("should work once we add support for pushing through predicates on
// grouping columns"). Skipped here for the same reason.
