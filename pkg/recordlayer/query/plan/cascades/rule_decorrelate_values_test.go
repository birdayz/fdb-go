package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestDecorrelateValuesRule_InlineConstantBox(t *testing.T) {
	t.Parallel()

	// Values box: SELECT 42 AS x FROM range(1)
	rangeSource := &expressions.FullUnorderedScanExpression{}
	rangeRef := expressions.InitialOf(rangeSource)
	rangeQ := expressions.ForEachQuantifier(rangeRef)
	constResult := &values.ConstantValue{Value: int64(42)}
	valuesBox := expressions.NewSelectExpression(constResult, []expressions.Quantifier{rangeQ}, nil)
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	// Real table scan
	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Outer select: SELECT f.col FROM (values box) p, (scan) f WHERE f.col = p.x
	outerPred := &predicates.ComparisonPredicate{
		Operand: &values.FieldValue{Field: "COL"},
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewQuantifiedObjectValue(valuesBoxQ.GetAlias()),
		},
	}
	outerSel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{valuesBoxQ, scanQ},
		[]predicates.QueryPredicate{outerPred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	// The decorrelated select should have only one quantifier (the scan)
	// and the predicate should have the constant value substituted.
	decorrelated := yielded[0].(*expressions.SelectExpression)
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (values box removed), got %d", len(decorrelated.GetQuantifiers()))
	}
	if decorrelated.GetQuantifiers()[0].GetRangesOver() != scanRef {
		t.Error("remaining quantifier should range over the scan")
	}
	if len(decorrelated.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(decorrelated.GetPredicates()))
	}
	// The predicate's comparison operand should now be the constant value.
	cp, ok := decorrelated.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	cv, ok := cp.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue after decorrelation, got %T", cp.Comparison.Operand)
	}
	if cv.Value != int64(42) {
		t.Errorf("expected constant 42, got %v", cv.Value)
	}
}

func TestDecorrelateValuesRule_SkipCorrelatedResult(t *testing.T) {
	t.Parallel()

	// Values box with result correlated to its own child → not a values box.
	rangeSource := &expressions.FullUnorderedScanExpression{}
	rangeRef := expressions.InitialOf(rangeSource)
	rangeQ := expressions.ForEachQuantifier(rangeRef)
	correlatedResult := values.NewQuantifiedObjectValue(rangeQ.GetAlias())
	notAValuesBox := expressions.NewSelectExpression(correlatedResult, []expressions.Quantifier{rangeQ}, nil)
	notAValuesBoxRef := expressions.InitialOf(notAValuesBox)
	notAValuesBoxQ := expressions.ForEachQuantifier(notAValuesBoxRef)

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	outerSel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{notAValuesBoxQ, scanQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (result is correlated), got %d", len(yielded))
	}
}

func TestDecorrelateValuesRule_SingleQuantifier(t *testing.T) {
	t.Parallel()

	// Single quantifier → rule requires ≥2.
	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	sel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{scanQ},
		nil,
	)
	selRef := expressions.InitialOf(sel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (single quantifier), got %d", len(yielded))
	}
}

func TestDecorrelateValuesRule_SidewaysCorrelation(t *testing.T) {
	t.Parallel()

	// Values box whose result references a sibling → must not be inlined.
	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	rangeSource := &expressions.FullUnorderedScanExpression{}
	rangeRef := expressions.InitialOf(rangeSource)
	rangeQ := expressions.ForEachQuantifier(rangeRef)
	// Result references the sibling scanQ's alias.
	sidewaysResult := values.NewQuantifiedObjectValue(scanQ.GetAlias())
	valuesBox := expressions.NewSelectExpression(sidewaysResult, []expressions.Quantifier{rangeQ}, nil)
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	outerSel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{valuesBoxQ, scanQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (sideways correlation), got %d", len(yielded))
	}
}

func TestDecorrelateValuesRule_MultipleValuesBoxes(t *testing.T) {
	t.Parallel()

	// Two values boxes + one real scan → both inlined.
	range1 := &expressions.FullUnorderedScanExpression{}
	range1Ref := expressions.InitialOf(range1)
	range1Q := expressions.ForEachQuantifier(range1Ref)
	vb1 := expressions.NewSelectExpression(
		&values.ConstantValue{Value: int64(10)},
		[]expressions.Quantifier{range1Q}, nil,
	)
	vb1Ref := expressions.InitialOf(vb1)
	vb1Q := expressions.ForEachQuantifier(vb1Ref)

	range2 := &expressions.FullUnorderedScanExpression{}
	range2Ref := expressions.InitialOf(range2)
	range2Q := expressions.ForEachQuantifier(range2Ref)
	vb2 := expressions.NewSelectExpression(
		&values.ConstantValue{Value: "hello"},
		[]expressions.Quantifier{range2Q}, nil,
	)
	vb2Ref := expressions.InitialOf(vb2)
	vb2Q := expressions.ForEachQuantifier(vb2Ref)

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	outerSel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{vb1Q, vb2Q, scanQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (both values boxes removed), got %d",
			len(decorrelated.GetQuantifiers()))
	}
}
