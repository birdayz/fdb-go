package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestSelectMergeRule_FilterChild(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	pred := &predicates.ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "X"},
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5)}},
	}
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	sel := expressions.NewSelectExpression(
		filterQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{filterQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewSelectMergeRule(), selRef)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded expression, got %d", len(yielded))
	}

	merged, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected *SelectExpression, got %T", yielded[0])
	}

	// Merged Select should have scan's quantifier, not filter's.
	mergedQs := merged.GetQuantifiers()
	if len(mergedQs) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(mergedQs))
	}
	if mergedQs[0].GetRangesOver() != scanRef {
		t.Error("merged quantifier should range over the scan Reference")
	}

	// Predicates should include the pulled-up filter predicate.
	mergedPreds := merged.GetPredicates()
	if len(mergedPreds) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(mergedPreds))
	}
}

func TestSelectMergeRule_FilterChildWithOuterPredicates(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	innerPred := &predicates.ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "X"},
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(5)}},
	}
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{innerPred}, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	outerPred := &predicates.ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "Y"},
		Comparison: predicates.Comparison{Type: predicates.ComparisonLessThan, Operand: &values.ConstantValue{Value: int64(10)}},
	}
	sel := expressions.NewSelectExpression(
		filterQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{filterQ},
		[]predicates.QueryPredicate{outerPred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewSelectMergeRule(), selRef)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	if len(merged.GetPredicates()) != 2 {
		t.Fatalf("expected 2 predicates (outer + inner), got %d", len(merged.GetPredicates()))
	}
	if len(merged.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(merged.GetQuantifiers()))
	}
}

func TestSelectMergeRule_NoMergeableChild(t *testing.T) {
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

	yielded := FireExpressionRule(NewSelectMergeRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (no mergeable child), got %d", len(yielded))
	}
}

func TestSelectMergeRule_SelectChild(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	childPred := &predicates.ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "A"},
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
	}
	childSel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{scanQ},
		[]predicates.QueryPredicate{childPred},
	)
	childRef := expressions.InitialOf(childSel)
	childQ := expressions.ForEachQuantifier(childRef)

	outerSel := expressions.NewSelectExpression(
		childQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{childQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewSelectMergeRule(), outerRef)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	if len(merged.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (from inner Select), got %d", len(merged.GetQuantifiers()))
	}
	if merged.GetQuantifiers()[0].GetRangesOver() != scanRef {
		t.Error("merged quantifier should range over the scan Reference")
	}
	if len(merged.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate (from child Select), got %d", len(merged.GetPredicates()))
	}
}

func TestSelectMergeRule_TwoQuantifiersOneFilter(t *testing.T) {
	t.Parallel()

	// Pattern: Select(ForEach(Filter(scan, preds)), ForEach(Explode), eqPred)
	// After merge: Select(ForEach(scan), ForEach(Explode), preds + eqPred)
	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filterPred := &predicates.ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "COL"},
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(42)}},
	}
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{filterPred}, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	explode := expressions.NewExplodeExpression(&values.ConstantValue{Value: []any{1, 2, 3}})
	explodeRef := expressions.InitialOf(explode)
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	eqPred := &predicates.ComparisonPredicate{
		Operand:    &values.FieldValue{Field: "COL"},
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.NewQuantifiedObjectValue(explodeQ.GetAlias())},
	}

	sel := expressions.NewSelectExpression(
		filterQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{filterQ, explodeQ},
		[]predicates.QueryPredicate{eqPred},
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewSelectMergeRule(), selRef)
	// Expect 2 yields: one from the original quantifier order, one
	// from the ChildrenAsSet-swapped order (both have a mergeable
	// Filter child).
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	if len(merged.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers (scan + explode), got %d", len(merged.GetQuantifiers()))
	}
	// First quantifier: scan (pulled up from filter)
	if merged.GetQuantifiers()[0].GetRangesOver() != scanRef {
		t.Error("first quantifier should range over scan")
	}
	// Second quantifier: explode (kept as-is)
	if merged.GetQuantifiers()[1].GetRangesOver() != explodeRef {
		t.Error("second quantifier should range over explode")
	}
	// Predicates: outer eq pred (rebased) + inner filter pred
	if len(merged.GetPredicates()) != 2 {
		t.Fatalf("expected 2 predicates, got %d", len(merged.GetPredicates()))
	}
}

func TestSelectMergeRule_NullOnEmptyNotMerged(t *testing.T) {
	t.Parallel()

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	pred := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "X"},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(1)},
		},
	}
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, scanQ)
	filterRef := expressions.InitialOf(filter)

	// Use null-on-empty quantifier — must NOT be merged.
	nullQ := expressions.ForEachNullOnEmptyQuantifier(filterRef)
	sel := expressions.NewSelectExpression(
		nullQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{nullQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewSelectMergeRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (null-on-empty not merged), got %d", len(yielded))
	}
}

func TestSelectMergeRule_AliasRebase(t *testing.T) {
	t.Parallel()

	// Verify that the parent's result value is rebased when a child
	// is merged: QOV(parentAlias) → QOV(childInnerAlias).
	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	filter := expressions.NewLogicalFilterExpression(nil, scanQ)
	filterRef := expressions.InitialOf(filter)
	filterQ := expressions.ForEachQuantifier(filterRef)

	// Result value references filterQ's alias.
	resultVal := values.NewQuantifiedObjectValue(filterQ.GetAlias())
	sel := expressions.NewSelectExpression(resultVal, []expressions.Quantifier{filterQ}, nil)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewSelectMergeRule(), selRef)
	if len(yielded) != 1 {
		t.Fatalf("expected 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	// The merged result value should reference scanQ's alias,
	// not filterQ's alias.
	rv := merged.GetResultValue()
	qov, ok := rv.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected *QuantifiedObjectValue, got %T", rv)
	}
	if qov.Correlation != scanQ.GetAlias() {
		t.Errorf("result value alias = %s, want %s (child inner alias)",
			qov.Correlation.Name(), scanQ.GetAlias().Name())
	}
}

func TestSelectMergeRule_MultiQuantifierChild(t *testing.T) {
	t.Parallel()

	// Child Select with 2 quantifiers: Select([q1, q2], childPreds, childResult)
	scan1 := &expressions.FullUnorderedScanExpression{}
	scan1Ref := expressions.InitialOf(scan1)
	scan1Q := expressions.ForEachQuantifier(scan1Ref)

	scan2 := &expressions.FullUnorderedScanExpression{}
	scan2Ref := expressions.InitialOf(scan2)
	scan2Q := expressions.ForEachQuantifier(scan2Ref)

	childResult := values.NewQuantifiedObjectValue(scan1Q.GetAlias())
	childSel := expressions.NewSelectExpression(
		childResult,
		[]expressions.Quantifier{scan1Q, scan2Q},
		nil,
	)
	childRef := expressions.InitialOf(childSel)
	childQ := expressions.ForEachQuantifier(childRef)

	// Outer: result references childQ's alias (which is the multi-quant child).
	outerResult := values.NewQuantifiedObjectValue(childQ.GetAlias())
	outerSel := expressions.NewSelectExpression(
		outerResult,
		[]expressions.Quantifier{childQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewSelectMergeRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yielded, got %d", len(yielded))
	}

	merged := yielded[0].(*expressions.SelectExpression)
	// Should have 2 quantifiers (from child), not 1.
	if len(merged.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(merged.GetQuantifiers()))
	}
	// The result value should be the child's result value (QOV of scan1Q),
	// NOT a dangling reference to childQ's alias.
	rv := merged.GetResultValue()
	qov, ok := rv.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("expected *QuantifiedObjectValue, got %T", rv)
	}
	if qov.Correlation != scan1Q.GetAlias() {
		t.Errorf("result value alias = %s, want %s (child's inner scan alias)",
			qov.Correlation.Name(), scan1Q.GetAlias().Name())
	}
}
