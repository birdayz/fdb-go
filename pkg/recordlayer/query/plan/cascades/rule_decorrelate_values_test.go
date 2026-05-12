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

func TestDecorrelateValuesRule_AndPredicateTranslation(t *testing.T) {
	t.Parallel()

	// Values box with constant result.
	rangeSource := &expressions.FullUnorderedScanExpression{}
	rangeRef := expressions.InitialOf(rangeSource)
	rangeQ := expressions.ForEachQuantifier(rangeRef)
	constResult := &values.ConstantValue{Value: int64(7)}
	valuesBox := expressions.NewSelectExpression(constResult, []expressions.Quantifier{rangeQ}, nil)
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// AND predicate: f.col = p.x AND f.col > 0
	andPred := predicates.NewAnd(
		&predicates.ComparisonPredicate{
			Operand: &values.FieldValue{Field: "COL"},
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: values.NewQuantifiedObjectValue(valuesBoxQ.GetAlias()),
			},
		},
		&predicates.ComparisonPredicate{
			Operand: &values.FieldValue{Field: "COL"},
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonGreaterThan,
				Operand: &values.ConstantValue{Value: int64(0)},
			},
		},
	)
	outerSel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{valuesBoxQ, scanQ},
		[]predicates.QueryPredicate{andPred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(decorrelated.GetQuantifiers()))
	}
	// The AND predicate should have the constant substituted.
	ap, ok := decorrelated.GetPredicates()[0].(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected AndPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	cp := ap.SubPredicates[0].(*predicates.ComparisonPredicate)
	cv, ok := cp.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue after decorrelation, got %T", cp.Comparison.Operand)
	}
	if cv.Value != int64(7) {
		t.Errorf("expected 7, got %v", cv.Value)
	}
}

func TestDecorrelateValuesRule_ResultValueTranslation(t *testing.T) {
	t.Parallel()

	// Values box.
	rangeSource := &expressions.FullUnorderedScanExpression{}
	rangeRef := expressions.InitialOf(rangeSource)
	rangeQ := expressions.ForEachQuantifier(rangeRef)
	constResult := &values.ConstantValue{Value: "hello"}
	valuesBox := expressions.NewSelectExpression(constResult, []expressions.Quantifier{rangeQ}, nil)
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	// Result value references the values box alias.
	outerResult := values.NewQuantifiedObjectValue(valuesBoxQ.GetAlias())
	outerSel := expressions.NewSelectExpression(
		outerResult,
		[]expressions.Quantifier{valuesBoxQ, scanQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	// Result value should be the constant "hello", not the QOV.
	rv := decorrelated.GetResultValue()
	cv, ok := rv.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue in result, got %T", rv)
	}
	if cv.Value != "hello" {
		t.Errorf("expected 'hello', got %v", cv.Value)
	}
}

func TestDecorrelateValuesRule_WithSourceAliases(t *testing.T) {
	t.Parallel()

	// Values box.
	rangeSource := &expressions.FullUnorderedScanExpression{}
	rangeRef := expressions.InitialOf(rangeSource)
	rangeQ := expressions.ForEachQuantifier(rangeRef)
	constResult := &values.ConstantValue{Value: int64(1)}
	valuesBox := expressions.NewSelectExpression(constResult, []expressions.Quantifier{rangeQ}, nil)
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	outerSel := expressions.NewSelectExpressionWithAliases(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{valuesBoxQ, scanQ},
		nil,
		[]string{"P", "F"},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	aliases := decorrelated.GetSourceAliases()
	if len(aliases) != 1 {
		t.Fatalf("expected 1 source alias (F), got %d: %v", len(aliases), aliases)
	}
	if aliases[0] != "F" {
		t.Errorf("expected alias F, got %s", aliases[0])
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

// makeValuesBox creates a values box: SELECT constResult FROM range(1).
// This is the Go equivalent of Java's valuesQun(...) helper.
func makeValuesBox(resultValue values.Value) (expressions.Quantifier, *expressions.Reference) {
	rangeSource := &expressions.FullUnorderedScanExpression{}
	rangeRef := expressions.InitialOf(rangeSource)
	rangeQ := expressions.ForEachQuantifier(rangeRef)
	valuesBox := expressions.NewSelectExpression(resultValue, []expressions.Quantifier{rangeQ}, nil)
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)
	return valuesBoxQ, valuesBoxRef
}

// makeRecordValuesBox creates a multi-field values box:
// SELECT RecordConstructorValue{fields} FROM range(1).
// Mirrors Java's valuesQun(ImmutableMap.of("x", v1, "y", v2)).
func makeRecordValuesBox(fields map[string]values.Value) (expressions.Quantifier, *expressions.Reference) {
	rcFields := make([]values.RecordConstructorField, 0, len(fields))
	for name, v := range fields {
		rcFields = append(rcFields, values.RecordConstructorField{Name: name, Value: v})
	}
	rcv := values.NewRecordConstructorValue(rcFields...)
	return makeValuesBox(rcv)
}

// makeBaseScan creates a base table scan quantifier.
func makeBaseScan() (expressions.Quantifier, *expressions.Reference) {
	scan := &expressions.FullUnorderedScanExpression{}
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)
	return scanQ, scanRef
}

// TestDecorrelateValuesRule_TrimUncorrelatedValuesBoxes ports Java's
// trimUncorrelatedValuesBoxes. A values box is present but nothing
// in the parent (predicates, result) references it. The rule should
// remove it and leave everything else unchanged.
//
// Java:
//
//	SELECT s.d FROM values(x=@0, y=42) AS v, (SELECT b,c,d FROM T WHERE a=42) AS s WHERE s.b = @0
//	→ SELECT s.d FROM (SELECT b,c,d FROM T WHERE a=42) AS s WHERE s.b = @0
func TestDecorrelateValuesRule_TrimUncorrelatedValuesBoxes(t *testing.T) {
	t.Parallel()

	// A ConstantObjectValue simulates a parameter reference (@0).
	cov := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"),
		"0", values.NotNullBytes,
	)

	// Values box: SELECT RecordConstructorValue{x=cov, y=42} FROM range(1)
	valuesBoxQ, _ := makeRecordValuesBox(map[string]values.Value{
		"x": cov,
		"y": &values.ConstantValue{Value: int64(42)},
	})

	// Lower select: SELECT b,c,d FROM T WHERE a = 42
	baseQ, _ := makeBaseScan()
	lowerSel := expressions.NewSelectExpression(
		baseQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: values.NewFieldValue(baseQ.GetFlowedObjectValue(), "a", nil),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: &values.ConstantValue{Value: int64(42)},
				},
			},
		},
	)
	lowerRef := expressions.InitialOf(lowerSel)
	lowerQ := expressions.ForEachQuantifier(lowerRef)

	// Top select: uses cov (NOT the values box alias) in predicate
	// and references lowerQ in result — values box is NOT referenced.
	outerSel := expressions.NewSelectExpression(
		values.NewFieldValue(lowerQ.GetFlowedObjectValue(), "d", nil),
		[]expressions.Quantifier{valuesBoxQ, lowerQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: values.NewFieldValue(lowerQ.GetFlowedObjectValue(), "c", nil),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: cov,
				},
			},
		},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)

	// Values box removed: only lowerQ remains.
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (values box removed), got %d", len(decorrelated.GetQuantifiers()))
	}

	// Predicate is unchanged (still uses cov directly, not via the values box).
	if len(decorrelated.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(decorrelated.GetPredicates()))
	}
	cp, ok := decorrelated.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	if _, isCOV := cp.Comparison.Operand.(*values.ConstantObjectValue); !isCOV {
		t.Errorf("expected ConstantObjectValue preserved in predicate, got %T", cp.Comparison.Operand)
	}
}

// TestDecorrelateValuesRule_RewritePredicatesAndReturnValueOnUncorrelatedValuesBox
// ports Java's rewritePredicatesAndReturnValueOnUncorrelatedValuesBox.
// The values box is referenced in the parent's result and predicates
// (but not by any child quantifier), so it is inlined and removed.
//
// Java:
//
//	SELECT v.y, s.d FROM values(x=@0, y=42) AS v, (SELECT b,c,d FROM T WHERE a=42) AS s WHERE s.c = v.x
//	→ SELECT 42 AS y, s.d FROM (SELECT b,c,d FROM T WHERE a=42) AS s WHERE s.c = @0
func TestDecorrelateValuesRule_RewritePredicatesAndReturnValueOnUncorrelatedValuesBox(t *testing.T) {
	t.Parallel()

	cov := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"),
		"0", values.NotNullBytes,
	)

	// Values box: SELECT {x=cov, y=42} FROM range(1)
	valuesBoxQ, _ := makeRecordValuesBox(map[string]values.Value{
		"x": cov,
		"y": &values.ConstantValue{Value: int64(42)},
	})

	// Lower select: SELECT b,c,d FROM T WHERE a = 42
	baseQ, _ := makeBaseScan()
	lowerSel := expressions.NewSelectExpression(
		baseQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: values.NewFieldValue(baseQ.GetFlowedObjectValue(), "a", nil),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: &values.ConstantValue{Value: int64(42)},
				},
			},
		},
	)
	lowerRef := expressions.InitialOf(lowerSel)
	lowerQ := expressions.ForEachQuantifier(lowerRef)

	// Top select result references v.y, predicate references v.x
	outerResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "y",
			Value: values.NewFieldValue(valuesBoxQ.GetFlowedObjectValue(), "y", nil),
		},
		values.RecordConstructorField{
			Name:  "d",
			Value: values.NewFieldValue(lowerQ.GetFlowedObjectValue(), "d", nil),
		},
	)
	outerPred := &predicates.ComparisonPredicate{
		Operand: values.NewFieldValue(lowerQ.GetFlowedObjectValue(), "c", nil),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewFieldValue(valuesBoxQ.GetFlowedObjectValue(), "x", nil),
		},
	}

	outerSel := expressions.NewSelectExpression(
		outerResult,
		[]expressions.Quantifier{valuesBoxQ, lowerQ},
		[]predicates.QueryPredicate{outerPred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)

	// Values box removed.
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (values box removed), got %d", len(decorrelated.GetQuantifiers()))
	}

	// Result value: v.y inlined → the "y" field should now reference the
	// RecordConstructorValue from the values box (which contains the 42 literal).
	rv, ok := decorrelated.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("expected RecordConstructorValue result, got %T", decorrelated.GetResultValue())
	}
	// The "y" field's value had FieldValue{Child: QOV(vbAlias), Field: "y"}.
	// After translation, the QOV is replaced with the RCV from the values box.
	// So we get FieldValue{Child: RCV{x:cov, y:42}, Field: "y"}.
	yField := rv.Fields[0]
	if yField.Name != "y" {
		t.Fatalf("expected first field name 'y', got %q", yField.Name)
	}
	yFV, ok := yField.Value.(*values.FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue for y, got %T", yField.Value)
	}
	// The child of the FieldValue should now be the RecordConstructorValue
	// (the values box's result) instead of a QuantifiedObjectValue.
	if _, stillQOV := yFV.Child.(*values.QuantifiedObjectValue); stillQOV {
		t.Error("expected QOV to be replaced by values box result, but QOV still present")
	}

	// Predicate: v.x inlined → comparison operand's child is RCV.
	if len(decorrelated.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(decorrelated.GetPredicates()))
	}
	cp, ok := decorrelated.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	compFV, ok := cp.Comparison.Operand.(*values.FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue in comparison operand, got %T", cp.Comparison.Operand)
	}
	if _, stillQOV := compFV.Child.(*values.QuantifiedObjectValue); stillQOV {
		t.Error("expected QOV in predicate comparison to be replaced, but QOV still present")
	}
}

// TestDecorrelateValuesRule_DoNotPushDownExistentialValuesQuantifier ports
// Java's doNotPushDownExistentialValuesQuantifier. An existential
// quantifier over a values box should NOT be treated as a values box
// because existential quantifiers don't forward values in a useful way.
func TestDecorrelateValuesRule_DoNotPushDownExistentialValuesQuantifier(t *testing.T) {
	t.Parallel()

	// Values box: SELECT 42 FROM range(1), but wrapped in an existential quantifier.
	rangeSource := &expressions.FullUnorderedScanExpression{}
	rangeRef := expressions.InitialOf(rangeSource)
	rangeQ := expressions.ForEachQuantifier(rangeRef)
	valuesBox := expressions.NewSelectExpression(
		&values.ConstantValue{Value: int64(42)},
		[]expressions.Quantifier{rangeQ}, nil,
	)
	valuesBoxRef := expressions.InitialOf(valuesBox)
	existsQ := expressions.ExistentialQuantifier(valuesBoxRef)

	baseQ, _ := makeBaseScan()

	outerSel := expressions.NewSelectExpression(
		values.NewFieldValue(baseQ.GetFlowedObjectValue(), "a", nil),
		[]expressions.Quantifier{baseQ, existsQ},
		[]predicates.QueryPredicate{
			predicates.NewExistsPredicate(existsQ.GetAlias()),
		},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (existential quantifier over values box), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_DoNotMatchIfAllExistentialQuantifiers ports
// Java's doNotMatchIfAllExistentialQuantifiers. When every quantifier
// is existential, there are no ForEach values box candidates, and the
// rule should not fire.
func TestDecorrelateValuesRule_DoNotMatchIfAllExistentialQuantifiers(t *testing.T) {
	t.Parallel()

	// Existential over values box
	rangeSource := &expressions.FullUnorderedScanExpression{}
	rangeRef := expressions.InitialOf(rangeSource)
	rangeQ := expressions.ForEachQuantifier(rangeRef)
	valuesBox := expressions.NewSelectExpression(
		&values.ConstantValue{Value: int64(42)},
		[]expressions.Quantifier{rangeQ}, nil,
	)
	valuesBoxRef := expressions.InitialOf(valuesBox)
	existsValuesQ := expressions.ExistentialQuantifier(valuesBoxRef)

	// Existential over a base scan with a predicate
	baseQ, _ := makeBaseScan()
	filteredSel := expressions.NewSelectExpression(
		baseQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: values.NewFieldValue(baseQ.GetFlowedObjectValue(), "a", nil),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: &values.ConstantValue{Value: int64(42)},
				},
			},
		},
	)
	filteredRef := expressions.InitialOf(filteredSel)
	existsTQ := expressions.ExistentialQuantifier(filteredRef)

	outerSel := expressions.NewSelectExpression(
		&values.ConstantValue{Value: "y"},
		[]expressions.Quantifier{existsValuesQ, existsTQ},
		[]predicates.QueryPredicate{
			predicates.NewExistsPredicate(existsValuesQ.GetAlias()),
			predicates.NewExistsPredicate(existsTQ.GetAlias()),
		},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (all existential quantifiers), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_DoNotAllowValuesBoxWithJoin ports Java's
// doNotAllowValuesBoxWithJoin. A "values box" whose child select has
// multiple quantifiers (i.e., it's a join, not a simple values
// projection) should not be treated as a values box, even if the
// cardinality is still 1.
func TestDecorrelateValuesRule_DoNotAllowValuesBoxWithJoin(t *testing.T) {
	t.Parallel()

	// "Values box" with two child quantifiers: SELECT 42 FROM range(1), range(1)
	range1 := &expressions.FullUnorderedScanExpression{}
	range1Ref := expressions.InitialOf(range1)
	range1Q := expressions.ForEachQuantifier(range1Ref)

	range2 := &expressions.FullUnorderedScanExpression{}
	range2Ref := expressions.InitialOf(range2)
	range2Q := expressions.ForEachQuantifier(range2Ref)

	notAValuesBox := expressions.NewSelectExpression(
		&values.ConstantValue{Value: int64(42)},
		[]expressions.Quantifier{range1Q, range2Q}, nil,
	)
	notAValuesBoxRef := expressions.InitialOf(notAValuesBox)
	notAValuesBoxQ := expressions.ForEachQuantifier(notAValuesBoxRef)

	baseQ, _ := makeBaseScan()

	// Outer select that references the "values box"
	correlatedSel := expressions.NewSelectExpression(
		baseQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: values.NewFieldValue(baseQ.GetFlowedObjectValue(), "a", nil),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: values.NewQuantifiedObjectValue(notAValuesBoxQ.GetAlias()),
				},
			},
		},
	)
	correlatedRef := expressions.InitialOf(correlatedSel)
	correlatedQ := expressions.ForEachQuantifier(correlatedRef)

	outerSel := expressions.NewSelectExpression(
		values.NewFieldValue(correlatedQ.GetFlowedObjectValue(), "b", nil),
		[]expressions.Quantifier{notAValuesBoxQ, correlatedQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (values box has join / multiple children), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_DoNotTreatUngroupedCountAsValues ports Java's
// doNotTreatUngroupedCountAsValues. An ungrouped COUNT(*) produces exactly
// one row (cardinality 1), which looks like a values box. But it is NOT a
// values box because its result is semantically meaningful (an aggregate),
// not a simple constant. The Go rule rejects it because the inner select's
// result value is correlated to its child quantifier (the GroupBy).
func TestDecorrelateValuesRule_DoNotTreatUngroupedCountAsValues(t *testing.T) {
	t.Parallel()

	// Build: SELECT count(*) FROM T
	// Java models this as:
	//   selectWhere = forEach(SelectExpression(T.*, [T], []))
	//   groupBy = forEach(GroupByExpression(null, count(*), selectWhere))
	//   selectHaving = forEach(SelectExpression(groupBy._0, [groupBy], []))

	baseQ, _ := makeBaseScan()
	selectWhereSel := expressions.NewSelectExpression(
		baseQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{baseQ},
		nil,
	)
	selectWhereRef := expressions.InitialOf(selectWhereSel)
	selectWhereQ := expressions.ForEachQuantifier(selectWhereRef)

	groupByExpr := expressions.NewGroupByExpression(
		nil, // no grouping keys (ungrouped aggregate)
		[]expressions.AggregateSpec{{
			Function: expressions.AggCount,
			Operand:  nil,
			Alias:    "count",
		}},
		selectWhereQ,
	)
	groupByRef := expressions.InitialOf(groupByExpr)
	groupByQ := expressions.ForEachQuantifier(groupByRef)

	// selectHaving result references groupBy → correlated to its child.
	selectHavingSel := expressions.NewSelectExpression(
		values.NewFieldValue(groupByQ.GetFlowedObjectValue(), "_0", nil),
		[]expressions.Quantifier{groupByQ},
		nil,
	)
	selectHavingRef := expressions.InitialOf(selectHavingSel)
	selectHavingQ := expressions.ForEachQuantifier(selectHavingRef)

	// Now build the outer join: (selectHaving as notQuiteValuesQun) JOIN T
	otherBaseQ, _ := makeBaseScan()
	correlatedSel := expressions.NewSelectExpression(
		otherBaseQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{otherBaseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: values.NewFieldValue(otherBaseQ.GetFlowedObjectValue(), "a", nil),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: values.NewQuantifiedObjectValue(selectHavingQ.GetAlias()),
				},
			},
		},
	)
	correlatedRef := expressions.InitialOf(correlatedSel)
	correlatedQ := expressions.ForEachQuantifier(correlatedRef)

	outerSel := expressions.NewSelectExpression(
		values.NewFieldValue(correlatedQ.GetFlowedObjectValue(), "b", nil),
		[]expressions.Quantifier{selectHavingQ, correlatedQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (ungrouped count is not a values box), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_RemoveValuesIfOnlyChild ports Java's
// removeValuesIfOnlyChild. When the ONLY quantifier in a select is a
// values box, Java replaces it with range(1) and inlines the values.
// Go's current rule returns early when all quantifiers are values boxes
// (len(newQuantifiers) == 0), so no yield is produced.
//
// This test documents the current Go behavior. When Go's rule is extended
// to match Java's handling of this case, update this test to expect a
// yield with the values inlined.
func TestDecorrelateValuesRule_RemoveValuesIfOnlyChild(t *testing.T) {
	t.Parallel()

	// SELECT 'hello' FROM values(true) WHERE values.flowed = true
	valuesBoxQ, _ := makeValuesBox(&values.ConstantValue{Value: true})

	outerSel := expressions.NewSelectExpression(
		&values.ConstantValue{Value: "hello"},
		[]expressions.Quantifier{valuesBoxQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: values.NewQuantifiedObjectValue(valuesBoxQ.GetAlias()),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: &values.ConstantValue{Value: true},
				},
			},
		},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	// Go divergence: returns early when all quantifiers would be removed.
	// Java replaces with range(1). Document the current behavior.
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (Go rule returns early when all quantifiers are values boxes), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_RemoveValuesIfAllChildren ports Java's
// removeValuesIfAllChildren. When ALL quantifiers are values boxes,
// Java replaces them all with a single range(1). Go's current rule
// returns early (len(newQuantifiers) == 0).
func TestDecorrelateValuesRule_RemoveValuesIfAllChildren(t *testing.T) {
	t.Parallel()

	vb1Q, _ := makeValuesBox(&values.ConstantValue{Value: "hello"})
	vb2Q, _ := makeValuesBox(&values.ConstantValue{Value: "world"})

	outerSel := expressions.NewSelectExpression(
		&values.ConstantValue{Value: int64(42)},
		[]expressions.Quantifier{vb1Q, vb2Q},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: values.NewQuantifiedObjectValue(vb1Q.GetAlias()),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonLessThan,
					Operand: values.NewQuantifiedObjectValue(vb2Q.GetAlias()),
				},
			},
		},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	// Go divergence: returns early when all quantifiers would be removed.
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (Go rule returns early when all quantifiers are values boxes), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_DoNotUseValuesBoxWithCorrelationsInTheValue
// ports Java's doNotUseValuesBoxWithCorrelationsInTheValue. A values
// box whose result value references a sibling quantifier (at the same
// level) should be rejected — it can't be safely pushed into anything
// it correlates to.
func TestDecorrelateValuesRule_DoNotUseValuesBoxWithCorrelationsInTheValue(t *testing.T) {
	t.Parallel()

	// Lower select: SELECT a,b,c FROM T WHERE d = b
	baseQ, _ := makeBaseScan()
	lowerSel := expressions.NewSelectExpression(
		baseQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: values.NewFieldValue(baseQ.GetFlowedObjectValue(), "d", nil),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: values.NewFieldValue(baseQ.GetFlowedObjectValue(), "b", nil),
				},
			},
		},
	)
	lowerRef := expressions.InitialOf(lowerSel)
	lowerQ := expressions.ForEachQuantifier(lowerRef)

	// Values box whose result references lowerQ (a sibling).
	// SELECT {x=lowerQ.b} FROM range(1) — the result is correlated to lowerQ.
	rangeSource := &expressions.FullUnorderedScanExpression{}
	rangeRef := expressions.InitialOf(rangeSource)
	rangeRQ := expressions.ForEachQuantifier(rangeRef)
	valuesBoxSel := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{
				Name:  "x",
				Value: values.NewFieldValue(lowerQ.GetFlowedObjectValue(), "b", nil),
			},
		),
		[]expressions.Quantifier{rangeRQ}, nil,
	)
	valuesBoxRef := expressions.InitialOf(valuesBoxSel)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	outerSel := expressions.NewSelectExpression(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{
				Name:  "a",
				Value: values.NewFieldValue(lowerQ.GetFlowedObjectValue(), "a", nil),
			},
			values.RecordConstructorField{
				Name:  "x",
				Value: values.NewFieldValue(valuesBoxQ.GetFlowedObjectValue(), "x", nil),
			},
			values.RecordConstructorField{
				Name:  "c",
				Value: values.NewFieldValue(lowerQ.GetFlowedObjectValue(), "c", nil),
			},
		),
		[]expressions.Quantifier{valuesBoxQ, lowerQ},
		nil,
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (values box result references sibling), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_MultiFieldValuesBoxInline verifies that a
// multi-field values box (RecordConstructorValue result) is correctly
// inlined. This is similar to Java's simpleDecorrelation but explicitly
// tests the multi-field case with FieldValue references into the values
// box's fields.
func TestDecorrelateValuesRule_MultiFieldValuesBoxInline(t *testing.T) {
	t.Parallel()

	// Values box: SELECT {x=3, y="hello"} FROM range(1)
	valuesBoxQ, _ := makeRecordValuesBox(map[string]values.Value{
		"x": &values.ConstantValue{Value: int64(3)},
		"y": &values.ConstantValue{Value: "hello"},
	})

	baseQ, _ := makeBaseScan()

	// SELECT base.b FROM (values) v, (scan) base WHERE base.a = v.x
	outerPred := &predicates.ComparisonPredicate{
		Operand: values.NewFieldValue(baseQ.GetFlowedObjectValue(), "a", nil),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: values.NewFieldValue(valuesBoxQ.GetFlowedObjectValue(), "x", nil),
		},
	}
	outerSel := expressions.NewSelectExpression(
		values.NewFieldValue(baseQ.GetFlowedObjectValue(), "b", nil),
		[]expressions.Quantifier{valuesBoxQ, baseQ},
		[]predicates.QueryPredicate{outerPred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (values box removed), got %d", len(decorrelated.GetQuantifiers()))
	}

	// Predicate comparison operand was FieldValue(QOV(vbAlias), "x").
	// After decorrelation, the QOV should be replaced with the RCV.
	cp, ok := decorrelated.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	fv, ok := cp.Comparison.Operand.(*values.FieldValue)
	if !ok {
		t.Fatalf("expected FieldValue in comparison operand, got %T", cp.Comparison.Operand)
	}
	// The child should now be a RecordConstructorValue, not a QOV.
	if _, isRCV := fv.Child.(*values.RecordConstructorValue); !isRCV {
		t.Errorf("expected RCV as child of translated FieldValue, got %T", fv.Child)
	}
}

// TestDecorrelateValuesRule_OrPredicateTranslation verifies that
// correlations inside OR predicates are translated correctly.
func TestDecorrelateValuesRule_OrPredicateTranslation(t *testing.T) {
	t.Parallel()

	valuesBoxQ, _ := makeValuesBox(&values.ConstantValue{Value: int64(99)})
	baseQ, _ := makeBaseScan()

	// OR predicate: f.col = p OR f.col > 10
	orPred := predicates.NewOr(
		&predicates.ComparisonPredicate{
			Operand: &values.FieldValue{Field: "COL"},
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: values.NewQuantifiedObjectValue(valuesBoxQ.GetAlias()),
			},
		},
		&predicates.ComparisonPredicate{
			Operand: &values.FieldValue{Field: "COL"},
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonGreaterThan,
				Operand: &values.ConstantValue{Value: int64(10)},
			},
		},
	)
	outerSel := expressions.NewSelectExpression(
		baseQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{valuesBoxQ, baseQ},
		[]predicates.QueryPredicate{orPred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(decorrelated.GetQuantifiers()))
	}

	// Check OR predicate's first child has the constant substituted.
	op, ok := decorrelated.GetPredicates()[0].(*predicates.OrPredicate)
	if !ok {
		t.Fatalf("expected OrPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	cp := op.SubPredicates[0].(*predicates.ComparisonPredicate)
	cv, ok := cp.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue after decorrelation, got %T", cp.Comparison.Operand)
	}
	if cv.Value != int64(99) {
		t.Errorf("expected 99, got %v", cv.Value)
	}
}

// TestDecorrelateValuesRule_NotPredicateTranslation verifies that
// correlations inside NOT predicates are translated correctly.
func TestDecorrelateValuesRule_NotPredicateTranslation(t *testing.T) {
	t.Parallel()

	valuesBoxQ, _ := makeValuesBox(&values.ConstantValue{Value: int64(5)})
	baseQ, _ := makeBaseScan()

	// NOT(f.col = p)
	notPred := predicates.NewNot(
		&predicates.ComparisonPredicate{
			Operand: &values.FieldValue{Field: "COL"},
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: values.NewQuantifiedObjectValue(valuesBoxQ.GetAlias()),
			},
		},
	)
	outerSel := expressions.NewSelectExpression(
		baseQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{valuesBoxQ, baseQ},
		[]predicates.QueryPredicate{notPred},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(decorrelated.GetQuantifiers()))
	}

	np, ok := decorrelated.GetPredicates()[0].(*predicates.NotPredicate)
	if !ok {
		t.Fatalf("expected NotPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	cp := np.Child.(*predicates.ComparisonPredicate)
	cv, ok := cp.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue after decorrelation, got %T", cp.Comparison.Operand)
	}
	if cv.Value != int64(5) {
		t.Errorf("expected 5, got %v", cv.Value)
	}
}

// TestDecorrelateValuesRule_ExistsPredicateNotTranslated verifies that
// an ExistsPredicate whose alias is NOT a values box is left unchanged
// by the translation.
func TestDecorrelateValuesRule_ExistsPredicateNotTranslated(t *testing.T) {
	t.Parallel()

	valuesBoxQ, _ := makeValuesBox(&values.ConstantValue{Value: int64(1)})
	baseQ, _ := makeBaseScan()

	// Outer select: SELECT f.a FROM values(1) v, T f WHERE EXISTS(f)
	// The exists predicate references f (not the values box).
	outerSel := expressions.NewSelectExpression(
		values.NewFieldValue(baseQ.GetFlowedObjectValue(), "a", nil),
		[]expressions.Quantifier{valuesBoxQ, baseQ},
		[]predicates.QueryPredicate{
			predicates.NewExistsPredicate(baseQ.GetAlias()),
		},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	// Values box removed, only baseQ remains.
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(decorrelated.GetQuantifiers()))
	}
	// ExistsPredicate should still reference baseQ's alias.
	ep, ok := decorrelated.GetPredicates()[0].(*predicates.ExistsPredicate)
	if !ok {
		t.Fatalf("expected ExistsPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	if ep.ExistentialAlias != baseQ.GetAlias() {
		t.Errorf("expected ExistsPredicate alias %v, got %v", baseQ.GetAlias(), ep.ExistentialAlias)
	}
}

// TestDecorrelateValuesRule_ConstantObjectValueResult verifies that a
// values box whose result is a ConstantObjectValue (parameter reference)
// is correctly inlined.
func TestDecorrelateValuesRule_ConstantObjectValueResult(t *testing.T) {
	t.Parallel()

	cov := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"),
		"0", values.NotNullLong,
	)
	valuesBoxQ, _ := makeValuesBox(cov)

	baseQ, _ := makeBaseScan()

	outerSel := expressions.NewSelectExpression(
		baseQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{valuesBoxQ, baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: values.NewFieldValue(baseQ.GetFlowedObjectValue(), "a", nil),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: values.NewQuantifiedObjectValue(valuesBoxQ.GetAlias()),
				},
			},
		},
	)
	outerRef := expressions.InitialOf(outerSel)

	yielded := FireExpressionRule(NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(decorrelated.GetQuantifiers()))
	}

	// The predicate should now have the COV directly instead of QOV.
	cp, ok := decorrelated.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	gotCOV, ok := cp.Comparison.Operand.(*values.ConstantObjectValue)
	if !ok {
		t.Fatalf("expected ConstantObjectValue after decorrelation, got %T", cp.Comparison.Operand)
	}
	if gotCOV != cov {
		t.Error("expected the same ConstantObjectValue instance")
	}
}
