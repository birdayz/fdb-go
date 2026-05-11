package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TestPredicatePushDown_SingleQuantifierPush tests the basic case:
// a SelectExpression with one ForEach quantifier whose child is a
// SelectExpression. A predicate referencing only that quantifier's
// alias is pushed into the child.
func TestPredicatePushDown_SingleQuantifierPush(t *testing.T) {
	t.Parallel()

	// Child: SELECT * FROM scan
	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	childSel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{scanQ},
		nil,
	)
	childRef := expressions.InitialOf(childSel)
	childQ := expressions.ForEachQuantifier(childRef)

	// Outer: SELECT childQ.* WHERE childQ.name = 'foo'
	pred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(childQ.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "foo"},
		},
	}
	outerSel := expressions.NewSelectExpression(
		childQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{childQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), outerRef)
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
	scan1 := &expressions.FullUnorderedScanExpression{}
	scan1Ref := expressions.InitialOf(scan1)
	scan1Q := expressions.ForEachQuantifier(scan1Ref)
	childA := expressions.NewSelectExpression(
		scan1Q.GetFlowedObjectValue(),
		[]expressions.Quantifier{scan1Q},
		nil,
	)
	childARef := expressions.InitialOf(childA)
	qA := expressions.ForEachQuantifier(childARef)

	scan2 := &expressions.FullUnorderedScanExpression{}
	scan2Ref := expressions.InitialOf(scan2)
	scan2Q := expressions.ForEachQuantifier(scan2Ref)
	childB := expressions.NewSelectExpression(
		scan2Q.GetFlowedObjectValue(),
		[]expressions.Quantifier{scan2Q},
		nil,
	)
	childBRef := expressions.InitialOf(childB)
	qB := expressions.ForEachQuantifier(childBRef)

	// Predicate on A only: qA.col = 'x'
	predA := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(qA.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "x"},
		},
	}
	// Cross-predicate: qA.id = qB.id (references both)
	predCross := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(qA.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewQuantifiedObjectValue(qB.GetAlias()),
		},
	}

	outerSel := expressions.NewSelectExpression(
		qA.GetFlowedObjectValue(),
		[]expressions.Quantifier{qA, qB},
		[]predicates.QueryPredicate{predA, predCross},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), outerRef)
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

	scan1 := &expressions.FullUnorderedScanExpression{}
	scan1Ref := expressions.InitialOf(scan1)
	qA := expressions.ForEachQuantifier(scan1Ref)

	scan2 := &expressions.FullUnorderedScanExpression{}
	scan2Ref := expressions.InitialOf(scan2)
	qB := expressions.ForEachQuantifier(scan2Ref)

	// Cross-predicate only.
	predCross := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(qA.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewQuantifiedObjectValue(qB.GetAlias()),
		},
	}

	outerSel := expressions.NewSelectExpression(
		qA.GetFlowedObjectValue(),
		[]expressions.Quantifier{qA, qB},
		[]predicates.QueryPredicate{predCross},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (no pushable predicates), got %d", len(yielded))
	}
}

// TestPredicatePushDown_NoPredicates verifies the rule yields nothing
// when the SelectExpression has no predicates.
func TestPredicatePushDown_NoPredicates(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	sel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{scanQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (no predicates), got %d", len(yielded))
	}
}

// TestPredicatePushDown_ExistentialGuard verifies the rule skips
// SelectExpressions containing Existential quantifiers.
func TestPredicatePushDown_ExistentialGuard(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	existScan := &expressions.FullUnorderedScanExpression{}
	existRef := expressions.InitialOf(existScan)
	existQ := expressions.ExistentialQuantifier(existRef)

	pred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(scanQ.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "test"},
		},
	}

	sel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{scanQ, existQ},
		[]predicates.QueryPredicate{pred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (existential quantifier guard), got %d", len(yielded))
	}
}

// TestPredicatePushDown_ThroughSort tests pushing predicates through a
// LogicalSortExpression — the predicate appears in a new
// SelectExpression below the sort.
func TestPredicatePushDown_ThroughSort(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{{Value: &values.FieldValue{Field: "NAME"}}},
		scanQ,
	)
	sortRef := expressions.InitialOf(sort)
	sortQ := expressions.ForEachQuantifier(sortRef)

	pred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(sortQ.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "x"},
		},
	}

	outerSel := expressions.NewSelectExpression(
		sortQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{sortQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), outerRef)
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

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	distinct := expressions.NewLogicalDistinctExpression(scanQ)
	distinctRef := expressions.InitialOf(distinct)
	distinctQ := expressions.ForEachQuantifier(distinctRef)

	pred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(distinctQ.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(42)},
		},
	}

	outerSel := expressions.NewSelectExpression(
		distinctQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{distinctQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), outerRef)
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

	scan1 := &expressions.FullUnorderedScanExpression{}
	scan1Ref := expressions.InitialOf(scan1)
	leg1Q := expressions.ForEachQuantifier(scan1Ref)

	scan2 := &expressions.FullUnorderedScanExpression{}
	scan2Ref := expressions.InitialOf(scan2)
	leg2Q := expressions.ForEachQuantifier(scan2Ref)

	union := expressions.NewLogicalUnionExpression([]expressions.Quantifier{leg1Q, leg2Q})
	unionRef := expressions.InitialOf(union)
	unionQ := expressions.ForEachQuantifier(unionRef)

	pred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(unionQ.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "pushed"},
		},
	}

	outerSel := expressions.NewSelectExpression(
		unionQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{unionQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), outerRef)
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

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	existingPred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{existingPred},
		scanQ,
	)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	pushedPred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(filterQ.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "pushed"},
		},
	}

	outerSel := expressions.NewSelectExpression(
		filterQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{filterQ},
		[]predicates.QueryPredicate{pushedPred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), outerRef)
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

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	childSel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{scanQ},
		nil,
	)
	childRef := expressions.InitialOf(childSel)
	childQ := expressions.ForEachQuantifier(childRef)

	pred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(childQ.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "pushed"},
		},
	}

	outerSel := expressions.NewSelectExpression(
		childQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{childQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), outerRef)
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

// TestPredicatePushDown_NullOnEmptySkipped verifies that ForEach
// quantifiers with nullOnEmpty=true (LEFT JOIN) are skipped.
func TestPredicatePushDown_NullOnEmptySkipped(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	nullOnEmptyQ := expressions.ForEachNullOnEmptyQuantifier(scanRef)

	pred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(nullOnEmptyQ.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "test"},
		},
	}

	sel := expressions.NewSelectExpression(
		nullOnEmptyQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{nullOnEmptyQ},
		[]predicates.QueryPredicate{pred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (nullOnEmpty quantifier skipped), got %d", len(yielded))
	}
}

// TestPredicatePushDown_ThroughUnique tests pushing predicates through
// a LogicalUniqueExpression.
func TestPredicatePushDown_ThroughUnique(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	unique := expressions.NewLogicalUniqueExpression(scanQ)
	uniqueRef := expressions.InitialOf(unique)
	uniqueQ := expressions.ForEachQuantifier(uniqueRef)

	pred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(uniqueQ.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}

	outerSel := expressions.NewSelectExpression(
		uniqueQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{uniqueQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 0 {
		t.Errorf("expected 0 predicates on outer (pushed through unique), got %d", len(result.GetPredicates()))
	}
}

// TestPredicatePushDown_UnsupportedChild verifies that pushing into an
// unsupported expression type (e.g., FullUnorderedScanExpression
// directly) yields nothing.
func TestPredicatePushDown_UnsupportedChild(t *testing.T) {
	t.Parallel()

	// Direct scan — no filter/select/sort/union/distinct/unique wrapper.
	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	pred := &predicates.ComparisonPredicate{
		Operand: values.NewQuantifiedObjectValue(scanQ.GetAlias()),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "x"},
		},
	}

	outerSel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{scanQ},
		[]predicates.QueryPredicate{pred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewPredicatePushDownRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (unsupported child type), got %d", len(yielded))
	}
}
