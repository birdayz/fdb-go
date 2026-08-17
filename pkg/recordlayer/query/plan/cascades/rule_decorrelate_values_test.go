package cascades

import (
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustDecorrelateConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct decorrelate-values fixture: " + err.Error())
	}
	return value
}

func decorrelateBaseRowType() *values.RecordType {
	return values.NewRecordType("T", false, []values.Field{
		{Name: "a", FieldType: values.NullableLong},
		{Name: "b", FieldType: values.NullableLong},
		{Name: "c", FieldType: values.NullableLong},
		{Name: "d", FieldType: values.NullableLong},
		{Name: "COL", FieldType: values.NullableLong},
	})
}

func decorrelateObject(q expressions.Quantifier) values.QuantifiedObjectValue {
	return mustDecorrelateConstruct(q.RequireFlowedObjectValue())
}

func decorrelateField(child values.Value, name string) values.Value {
	request := mustDecorrelateConstruct(values.FieldByName(name))
	return mustDecorrelateConstruct(values.ResolveFieldAccess(
		child, []values.FieldRequest{request}))
}

func decorrelateOrdinal(child values.Value, ordinal int) values.Value {
	return mustDecorrelateConstruct(values.ResolveFieldOrdinals(child, []int{ordinal}))
}

func decorrelateLiteral(literal any) *values.ConstantValue {
	var typ values.Type
	switch literal.(type) {
	case int, int32, int64:
		typ = values.NotNullLong
	case string:
		typ = values.NotNullString
	case bool:
		typ = values.NotNullBoolean
	case []byte:
		typ = values.NotNullBytes
	default:
		panic("unsupported decorrelate-values literal type")
	}
	return &values.ConstantValue{Value: literal, Typ: typ}
}

func TestDecorrelateValuesRule_ExactConstructorCollapseWithConstantObjectSibling(t *testing.T) {
	t.Parallel()

	captured := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"), "0", values.NotNullBytes)
	selected := decorrelateLiteral(int64(42))
	constructor := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "x", Value: captured},
		values.RecordConstructorField{Name: "y", Value: selected},
	)

	resolved, err := values.ResolveFieldOrdinals(constructor, []int{1})
	if err != nil {
		t.Fatalf("resolve exact constructor field: %v", err)
	}
	if resolved != selected {
		t.Fatalf("resolved field = %T %p, want selected literal %p", resolved, resolved, selected)
	}

	// Canonical collapse returns the selected Value itself. Later mutation of
	// the source constructor cannot redirect the already-resolved program.
	constructor.Fields[1].Value = decorrelateLiteral(int64(99))
	if got := resolved.(*values.ConstantValue).Value; got != int64(42) {
		t.Fatalf("resolved field changed with source constructor: got %v, want 42", got)
	}
}

func TestDecorrelateValuesRule_InlineConstantBox(t *testing.T) {
	t.Parallel()

	// Values box: SELECT 42 AS x FROM range(1)
	rangeQ := makeRangeOneQ()
	constResult := decorrelateLiteral(int64(42))
	valuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(constResult, []expressions.Quantifier{rangeQ}, nil))
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	// Real table scan
	scanQ, scanRef := makeBaseScan()

	// Outer select: SELECT f.col FROM (values box) p, (scan) f WHERE f.col = p.x
	outerPred := &predicates.ComparisonPredicate{
		Operand: decorrelateField(decorrelateObject(scanQ), "COL"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: decorrelateObject(valuesBoxQ),
		},
	}
	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(scanQ),
		[]expressions.Quantifier{valuesBoxQ, scanQ},
		[]predicates.QueryPredicate{outerPred}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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
	rangeQ := makeRangeOneQ()
	correlatedResult := decorrelateObject(rangeQ)
	notAValuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(correlatedResult, []expressions.Quantifier{rangeQ}, nil))
	notAValuesBoxRef := expressions.InitialOf(notAValuesBox)
	notAValuesBoxQ := expressions.ForEachQuantifier(notAValuesBoxRef)

	scanQ, _ := makeBaseScan()

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(scanQ),
		[]expressions.Quantifier{notAValuesBoxQ, scanQ},
		nil))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (result is correlated), got %d", len(yielded))
	}
}

func TestDecorrelateValuesRule_SingleQuantifier(t *testing.T) {
	t.Parallel()

	// Single quantifier → rule requires ≥2.
	scanQ, _ := makeBaseScan()

	sel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(scanQ),
		[]expressions.Quantifier{scanQ},
		nil))

	selRef := expressions.InitialOf(sel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (single quantifier), got %d", len(yielded))
	}
}

func TestDecorrelateValuesRule_SidewaysCorrelation(t *testing.T) {
	t.Parallel()

	// Values box whose result references a sibling → must not be inlined.
	scanQ, _ := makeBaseScan()

	rangeQ := makeRangeOneQ()
	// Result references the sibling scanQ's alias.
	sidewaysResult := decorrelateObject(scanQ)
	valuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(sidewaysResult, []expressions.Quantifier{rangeQ}, nil))
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(scanQ),
		[]expressions.Quantifier{valuesBoxQ, scanQ},
		nil))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (sideways correlation), got %d", len(yielded))
	}
}

func TestDecorrelateValuesRule_AndPredicateTranslation(t *testing.T) {
	t.Parallel()

	// Values box with constant result.
	rangeQ := makeRangeOneQ()
	constResult := decorrelateLiteral(int64(7))
	valuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(constResult, []expressions.Quantifier{rangeQ}, nil))
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	scanQ, _ := makeBaseScan()

	// AND predicate: f.col = p.x AND f.col > 0
	andPred := predicates.NewAnd(
		&predicates.ComparisonPredicate{
			Operand: decorrelateField(decorrelateObject(scanQ), "COL"),
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: decorrelateObject(valuesBoxQ),
			},
		},
		&predicates.ComparisonPredicate{
			Operand: decorrelateField(decorrelateObject(scanQ), "COL"),
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonGreaterThan,
				Operand: decorrelateLiteral(int64(0)),
			},
		},
	)
	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(scanQ),
		[]expressions.Quantifier{valuesBoxQ, scanQ},
		[]predicates.QueryPredicate{andPred}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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
	rangeQ := makeRangeOneQ()
	constResult := decorrelateLiteral("hello")
	valuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(constResult, []expressions.Quantifier{rangeQ}, nil))
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	scanQ, _ := makeBaseScan()

	// Result value references the values box alias.
	outerResult := decorrelateObject(valuesBoxQ)
	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		outerResult,
		[]expressions.Quantifier{valuesBoxQ, scanQ},
		nil))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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
	rangeQ := makeRangeOneQ()
	constResult := decorrelateLiteral(int64(1))
	valuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(constResult, []expressions.Quantifier{rangeQ}, nil))
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	scanQ, _ := makeBaseScan()

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpressionWithAliases(
		decorrelateObject(scanQ),
		[]expressions.Quantifier{valuesBoxQ, scanQ},
		nil,
		[]string{"P", "F"}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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
	vb1 := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral(int64(10)),
		[]expressions.Quantifier{makeRangeOneQ()}, nil))

	vb1Ref := expressions.InitialOf(vb1)
	vb1Q := expressions.ForEachQuantifier(vb1Ref)

	vb2 := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral("hello"),
		[]expressions.Quantifier{makeRangeOneQ()}, nil))

	vb2Ref := expressions.InitialOf(vb2)
	vb2Q := expressions.ForEachQuantifier(vb2Ref)

	scanQ, _ := makeBaseScan()

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(scanQ),
		[]expressions.Quantifier{vb1Q, vb2Q, scanQ},
		nil))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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
func makeRangeOneQ() expressions.Quantifier {
	rangeOne := values.NewRangeValue(
		decorrelateLiteral(int64(0)),
		decorrelateLiteral(int64(1)),
		decorrelateLiteral(int64(1)),
	)
	rangeSource := mustDecorrelateConstruct(expressions.NewTableFunctionExpression(rangeOne))
	rangeRef := expressions.InitialOf(rangeSource)
	return expressions.ForEachQuantifier(rangeRef)
}

func makeValuesBox(resultValue values.Value) (expressions.Quantifier, *expressions.Reference) {
	rangeQ := makeRangeOneQ()
	valuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(resultValue, []expressions.Quantifier{rangeQ}, nil))
	valuesBoxRef := expressions.InitialOf(valuesBox)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)
	return valuesBoxQ, valuesBoxRef
}

// makeRecordValuesBox creates a multi-field values box:
// SELECT RecordConstructorValue{fields} FROM range(1).
// Mirrors Java's valuesQun(ImmutableMap.of("x", v1, "y", v2)).
func makeRecordValuesBox(fields map[string]values.Value) (expressions.Quantifier, *expressions.Reference) {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	rcFields := make([]values.RecordConstructorField, 0, len(fields))
	for _, name := range names {
		rcFields = append(rcFields, values.RecordConstructorField{Name: name, Value: fields[name]})
	}
	rcv := values.NewRecordConstructorValue(rcFields...)
	return makeValuesBox(rcv)
}

// makeBaseScan creates a base table scan quantifier.
func makeBaseScan() (expressions.Quantifier, *expressions.Reference) {
	scan := mustDecorrelateConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"T"}, decorrelateBaseRowType()))
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
		"y": decorrelateLiteral(int64(42)),
	})

	// Lower select: SELECT b,c,d FROM T WHERE a = 42
	baseQ, _ := makeBaseScan()
	lowerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateLiteral(int64(42)),
				},
			},
		}))

	lowerRef := expressions.InitialOf(lowerSel)
	lowerQ := expressions.ForEachQuantifier(lowerRef)

	// Top select: uses cov (NOT the values box alias) in predicate
	// and references lowerQ in result — values box is NOT referenced.
	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateField(decorrelateObject(lowerQ), "d"),
		[]expressions.Quantifier{valuesBoxQ, lowerQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(lowerQ), "c"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: cov,
				},
			},
		}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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
		"y": decorrelateLiteral(int64(42)),
	})

	// Lower select: SELECT b,c,d FROM T WHERE a = 42
	baseQ, _ := makeBaseScan()
	lowerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateLiteral(int64(42)),
				},
			},
		}))

	lowerRef := expressions.InitialOf(lowerSel)
	lowerQ := expressions.ForEachQuantifier(lowerRef)

	// Top select result references v.y, predicate references v.x
	outerResult := values.NewRecordConstructorValue(
		values.RecordConstructorField{
			Name:  "y",
			Value: decorrelateField(decorrelateObject(valuesBoxQ), "y"),
		},
		values.RecordConstructorField{
			Name:  "d",
			Value: decorrelateField(decorrelateObject(lowerQ), "d"),
		},
	)
	outerPred := &predicates.ComparisonPredicate{
		Operand: decorrelateField(decorrelateObject(lowerQ), "c"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: decorrelateField(decorrelateObject(valuesBoxQ), "x"),
		},
	}

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		outerResult,
		[]expressions.Quantifier{valuesBoxQ, lowerQ},
		[]predicates.QueryPredicate{outerPred}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)

	// Values box removed.
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (values box removed), got %d", len(decorrelated.GetQuantifiers()))
	}

	// Result value: v.y is resolved through the exact record constructor and
	// canonicalized directly to the 42 literal.
	rv, ok := decorrelated.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("expected RecordConstructorValue result, got %T", decorrelated.GetResultValue())
	}
	yField := rv.Fields[0]
	if yField.Name != "y" {
		t.Fatalf("expected first field name 'y', got %q", yField.Name)
	}
	yLiteral, ok := yField.Value.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected direct ConstantValue for y, got %T", yField.Value)
	}
	if yLiteral.Value != int64(42) {
		t.Fatalf("inlined y = %v, want 42", yLiteral.Value)
	}

	// Predicate: v.x is likewise canonicalized to the captured constant object.
	if len(decorrelated.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(decorrelated.GetPredicates()))
	}
	cp, ok := decorrelated.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	gotCOV, ok := cp.Comparison.Operand.(*values.ConstantObjectValue)
	if !ok {
		t.Fatalf("expected ConstantObjectValue in comparison operand, got %T", cp.Comparison.Operand)
	}
	if gotCOV != cov {
		t.Fatal("predicate did not preserve the captured constant object")
	}
}

// TestDecorrelateValuesRule_DoNotPushDownExistentialValuesQuantifier ports
// Java's doNotPushDownExistentialValuesQuantifier. An existential
// quantifier over a values box should NOT be treated as a values box
// because existential quantifiers don't forward values in a useful way.
func TestDecorrelateValuesRule_DoNotPushDownExistentialValuesQuantifier(t *testing.T) {
	t.Parallel()

	// Values box: SELECT 42 FROM range(1), but wrapped in an existential quantifier.
	rangeQ := makeRangeOneQ()
	valuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral(int64(42)),
		[]expressions.Quantifier{rangeQ}, nil))

	valuesBoxRef := expressions.InitialOf(valuesBox)
	existsQ := expressions.ExistentialQuantifier(valuesBoxRef)

	baseQ, _ := makeBaseScan()

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateField(decorrelateObject(baseQ), "a"),
		[]expressions.Quantifier{baseQ, existsQ},
		[]predicates.QueryPredicate{
			mustExistentialAlias(t, existsQ.GetAlias()),
		}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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
	rangeQ := makeRangeOneQ()
	valuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral(int64(42)),
		[]expressions.Quantifier{rangeQ}, nil))

	valuesBoxRef := expressions.InitialOf(valuesBox)
	existsValuesQ := expressions.ExistentialQuantifier(valuesBoxRef)

	// Existential over a base scan with a predicate
	baseQ, _ := makeBaseScan()
	filteredSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateLiteral(int64(42)),
				},
			},
		}))

	filteredRef := expressions.InitialOf(filteredSel)
	existsTQ := expressions.ExistentialQuantifier(filteredRef)

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral("y"),
		[]expressions.Quantifier{existsValuesQ, existsTQ},
		[]predicates.QueryPredicate{
			mustExistentialAlias(t, existsValuesQ.GetAlias()),
			mustExistentialAlias(t, existsTQ.GetAlias()),
		}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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
	notAValuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral(int64(42)),
		[]expressions.Quantifier{makeRangeOneQ(), makeRangeOneQ()}, nil))

	notAValuesBoxRef := expressions.InitialOf(notAValuesBox)
	notAValuesBoxQ := expressions.ForEachQuantifier(notAValuesBoxRef)

	baseQ, _ := makeBaseScan()

	// Outer select that references the "values box"
	correlatedSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateObject(notAValuesBoxQ),
				},
			},
		}))

	correlatedRef := expressions.InitialOf(correlatedSel)
	correlatedQ := expressions.ForEachQuantifier(correlatedRef)

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateField(decorrelateObject(correlatedQ), "b"),
		[]expressions.Quantifier{notAValuesBoxQ, correlatedQ},
		nil))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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
	selectWhereSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		nil))

	selectWhereRef := expressions.InitialOf(selectWhereSel)
	selectWhereQ := expressions.ForEachQuantifier(selectWhereRef)

	groupByExpr := mustDecorrelateConstruct(expressions.NewGroupByExpression(
		nil,
		[]expressions.AggregateSpec{{
			Function: expressions.AggCount,
			Operand:  nil,
			Alias:    "count",
		}},
		selectWhereQ))

	groupByRef := expressions.InitialOf(groupByExpr)
	groupByQ := expressions.ForEachQuantifier(groupByRef)

	// selectHaving result references groupBy → correlated to its child.
	selectHavingSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateOrdinal(decorrelateObject(groupByQ), 0),
		[]expressions.Quantifier{groupByQ},
		nil))

	selectHavingRef := expressions.InitialOf(selectHavingSel)
	selectHavingQ := expressions.ForEachQuantifier(selectHavingRef)

	// Now build the outer join: (selectHaving as notQuiteValuesQun) JOIN T
	otherBaseQ, _ := makeBaseScan()
	correlatedSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(otherBaseQ),
		[]expressions.Quantifier{otherBaseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(otherBaseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateObject(selectHavingQ),
				},
			},
		}))

	correlatedRef := expressions.InitialOf(correlatedSel)
	correlatedQ := expressions.ForEachQuantifier(correlatedRef)

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateField(decorrelateObject(correlatedQ), "b"),
		[]expressions.Quantifier{selectHavingQ, correlatedQ},
		nil))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (ungrouped count is not a values box), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_RemoveValuesIfOnlyChild ports Java's
// removeValuesIfOnlyChild. When the ONLY quantifier in a select is a
// values box, it is replaced with a range(1) quantifier and the values
// are inlined into result + predicates.
func TestDecorrelateValuesRule_RemoveValuesIfOnlyChild(t *testing.T) {
	t.Parallel()

	// SELECT 'hello' FROM values(true) WHERE values.flowed = true
	valuesBoxQ, _ := makeValuesBox(decorrelateLiteral(true))

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral("hello"),
		[]expressions.Quantifier{valuesBoxQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateObject(valuesBoxQ),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateLiteral(true),
				},
			},
		}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	// Should have exactly 1 quantifier: the range(1) replacement.
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (range(1) inserted), got %d", len(decorrelated.GetQuantifiers()))
	}
	// The range(1) quantifier should point to a TableFunctionExpression.
	rangeRef := decorrelated.GetQuantifiers()[0].GetRangesOver()
	if rangeRef == nil {
		t.Fatal("range(1) quantifier has nil Reference")
	}
	if _, ok := rangeRef.Get().(*expressions.TableFunctionExpression); !ok {
		t.Fatalf("expected TableFunctionExpression, got %T", rangeRef.Get())
	}
	// The predicate should have the constant value substituted.
	if len(decorrelated.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(decorrelated.GetPredicates()))
	}
	cp, ok := decorrelated.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	cv, ok := cp.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue operand after decorrelation, got %T", cp.Operand)
	}
	if cv.Value != true {
		t.Errorf("expected true, got %v", cv.Value)
	}
}

// TestDecorrelateValuesRule_RemoveValuesIfAllChildren ports Java's
// removeValuesIfAllChildren. When ALL quantifiers are values boxes,
// they are replaced with a single range(1) and values are inlined.
func TestDecorrelateValuesRule_RemoveValuesIfAllChildren(t *testing.T) {
	t.Parallel()

	vb1Q, _ := makeValuesBox(decorrelateLiteral("hello"))
	vb2Q, _ := makeValuesBox(decorrelateLiteral("world"))

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral(int64(42)),
		[]expressions.Quantifier{vb1Q, vb2Q},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateObject(vb1Q),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonLessThan,
					Operand: decorrelateObject(vb2Q),
				},
			},
		}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	// Should have exactly 1 quantifier: the range(1) replacement.
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (range(1) inserted), got %d", len(decorrelated.GetQuantifiers()))
	}
	rangeRef := decorrelated.GetQuantifiers()[0].GetRangesOver()
	if rangeRef == nil {
		t.Fatal("range(1) quantifier has nil Reference")
	}
	if _, ok := rangeRef.Get().(*expressions.TableFunctionExpression); !ok {
		t.Fatalf("expected TableFunctionExpression, got %T", rangeRef.Get())
	}
	// The predicate should have both values inlined.
	if len(decorrelated.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(decorrelated.GetPredicates()))
	}
	cp, ok := decorrelated.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	lhs, ok := cp.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue operand, got %T", cp.Operand)
	}
	if lhs.Value != "hello" {
		t.Errorf("expected 'hello', got %v", lhs.Value)
	}
	rhs, ok := cp.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue comparison operand, got %T", cp.Comparison.Operand)
	}
	if rhs.Value != "world" {
		t.Errorf("expected 'world', got %v", rhs.Value)
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
	lowerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "d"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateField(decorrelateObject(baseQ), "b"),
				},
			},
		}))

	lowerRef := expressions.InitialOf(lowerSel)
	lowerQ := expressions.ForEachQuantifier(lowerRef)

	// Values box whose result references lowerQ (a sibling).
	// SELECT {x=lowerQ.b} FROM range(1) — the result is correlated to lowerQ.
	valuesBoxSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{
				Name:  "x",
				Value: decorrelateField(decorrelateObject(lowerQ), "b"),
			},
		),
		[]expressions.Quantifier{makeRangeOneQ()}, nil))

	valuesBoxRef := expressions.InitialOf(valuesBoxSel)
	valuesBoxQ := expressions.ForEachQuantifier(valuesBoxRef)

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{
				Name:  "a",
				Value: decorrelateField(decorrelateObject(lowerQ), "a"),
			},
			values.RecordConstructorField{
				Name:  "x",
				Value: decorrelateField(decorrelateObject(valuesBoxQ), "x"),
			},
			values.RecordConstructorField{
				Name:  "c",
				Value: decorrelateField(decorrelateObject(lowerQ), "c"),
			},
		),
		[]expressions.Quantifier{valuesBoxQ, lowerQ},
		nil))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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
		"x": decorrelateLiteral(int64(3)),
		"y": decorrelateLiteral("hello"),
	})

	baseQ, _ := makeBaseScan()

	// SELECT base.b FROM (values) v, (scan) base WHERE base.a = v.x
	outerPred := &predicates.ComparisonPredicate{
		Operand: decorrelateField(decorrelateObject(baseQ), "a"),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: decorrelateField(decorrelateObject(valuesBoxQ), "x"),
		},
	}
	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateField(decorrelateObject(baseQ), "b"),
		[]expressions.Quantifier{valuesBoxQ, baseQ},
		[]predicates.QueryPredicate{outerPred}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (values box removed), got %d", len(decorrelated.GetQuantifiers()))
	}

	// Exact reconstruction resolves FieldValue(QOV(vbAlias), "x") through the
	// replacement constructor and canonicalizes it to the selected literal.
	cp, ok := decorrelated.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", decorrelated.GetPredicates()[0])
	}
	literal, ok := cp.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected direct ConstantValue in comparison operand, got %T", cp.Comparison.Operand)
	}
	if literal.Value != int64(3) {
		t.Fatalf("inlined x = %v, want 3", literal.Value)
	}
}

// TestDecorrelateValuesRule_OrPredicateTranslation verifies that
// correlations inside OR predicates are translated correctly.
func TestDecorrelateValuesRule_OrPredicateTranslation(t *testing.T) {
	t.Parallel()

	valuesBoxQ, _ := makeValuesBox(decorrelateLiteral(int64(99)))
	baseQ, _ := makeBaseScan()

	// OR predicate: f.col = p OR f.col > 10
	orPred := predicates.NewOr(
		&predicates.ComparisonPredicate{
			Operand: decorrelateField(decorrelateObject(baseQ), "COL"),
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: decorrelateObject(valuesBoxQ),
			},
		},
		&predicates.ComparisonPredicate{
			Operand: decorrelateField(decorrelateObject(baseQ), "COL"),
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonGreaterThan,
				Operand: decorrelateLiteral(int64(10)),
			},
		},
	)
	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{valuesBoxQ, baseQ},
		[]predicates.QueryPredicate{orPred}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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

	valuesBoxQ, _ := makeValuesBox(decorrelateLiteral(int64(5)))
	baseQ, _ := makeBaseScan()

	// NOT(f.col = p)
	notPred := predicates.NewNot(
		&predicates.ComparisonPredicate{
			Operand: decorrelateField(decorrelateObject(baseQ), "COL"),
			Comparison: predicates.Comparison{
				Type:    predicates.ComparisonEquals,
				Operand: decorrelateObject(valuesBoxQ),
			},
		},
	)
	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{valuesBoxQ, baseQ},
		[]predicates.QueryPredicate{notPred}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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

// TestDecorrelateValuesRule_ExistentialPredicateNotTranslated verifies that
// an ExistentialValuePredicate whose alias is NOT a values box is left unchanged
// by the translation.
func TestDecorrelateValuesRule_ExistentialPredicateNotTranslated(t *testing.T) {
	t.Parallel()

	valuesBoxQ, _ := makeValuesBox(decorrelateLiteral(int64(1)))
	baseQ, _ := makeBaseScan()

	// Outer select: SELECT f.a FROM values(1) v, T f WHERE EXISTS(f)
	// The exists predicate references f (not the values box).
	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateField(decorrelateObject(baseQ), "a"),
		[]expressions.Quantifier{valuesBoxQ, baseQ},
		[]predicates.QueryPredicate{
			mustExistentialAlias(t, baseQ.GetAlias()),
		}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)
	// Values box removed, only baseQ remains.
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier, got %d", len(decorrelated.GetQuantifiers()))
	}
	// ExistentialValuePredicate should still reference baseQ's alias.
	ep, ok := decorrelated.GetPredicates()[0].(*predicates.ExistentialValuePredicate)
	if !ok {
		t.Fatalf("expected ExistentialValuePredicate, got %T", decorrelated.GetPredicates()[0])
	}
	if ep.GetExistentialAlias() != baseQ.GetAlias() {
		t.Errorf("expected ExistentialValuePredicate alias %v, got %v", baseQ.GetAlias(), ep.GetExistentialAlias())
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

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{valuesBoxQ, baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateObject(valuesBoxQ),
				},
			},
		}))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
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

// TestDecorrelateValuesRule_DoNotUseValuesBoxWithPredicates ports Java's
// doNotUseValuesBoxWithPredicates. A values box that has predicates
// (even tautologies like TRUE) should NOT be treated as a values box.
func TestDecorrelateValuesRule_DoNotUseValuesBoxWithPredicates(t *testing.T) {
	t.Parallel()

	rangeQ := makeRangeOneQ()

	notAValuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral(int64(42)),
		[]expressions.Quantifier{rangeQ},
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}))

	notAValuesBoxRef := expressions.InitialOf(notAValuesBox)
	notAValuesBoxQ := expressions.ForEachQuantifier(notAValuesBoxRef)

	baseQ, _ := makeBaseScan()

	correlatedSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateObject(notAValuesBoxQ),
				},
			},
		}))

	correlatedRef := expressions.InitialOf(correlatedSel)
	correlatedQ := expressions.ForEachQuantifier(correlatedRef)

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateField(decorrelateObject(correlatedQ), "b"),
		[]expressions.Quantifier{notAValuesBoxQ, correlatedQ},
		nil))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (values box has predicates), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_DoNotTreatRangeTwoAsValues ports Java's
// doNotTreatRangeTwoAsValues. A values box over range(2) has
// cardinality 2, not 1. The rule should not treat it as a values box.
func TestDecorrelateValuesRule_DoNotTreatRangeTwoAsValues(t *testing.T) {
	t.Parallel()

	rangeTwoValue := values.NewRangeValue(
		decorrelateLiteral(int64(0)),
		decorrelateLiteral(int64(2)),
		decorrelateLiteral(int64(1)),
	)
	rangeTwoExpr := mustDecorrelateConstruct(expressions.NewTableFunctionExpression(rangeTwoValue))
	rangeTwoRef := expressions.InitialOf(rangeTwoExpr)
	rangeTwoQ := expressions.ForEachQuantifier(rangeTwoRef)

	notAValuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral(int64(42)),
		[]expressions.Quantifier{rangeTwoQ}, nil))

	notAValuesBoxRef := expressions.InitialOf(notAValuesBox)
	notAValuesBoxQ := expressions.ForEachQuantifier(notAValuesBoxRef)

	baseQ, _ := makeBaseScan()

	correlatedSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateObject(notAValuesBoxQ),
				},
			},
		}))

	correlatedRef := expressions.InitialOf(correlatedSel)
	correlatedQ := expressions.ForEachQuantifier(correlatedRef)

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateField(decorrelateObject(correlatedQ), "b"),
		[]expressions.Quantifier{notAValuesBoxQ, correlatedQ},
		nil))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (values box over range(2)), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_DoNotTreatRangeWithConstantObjectValueAsValueBox
// ports Java's doNotTreatRangeWithConstantObjectValueAsValueBox. A values
// box over range(constantObjectValue) should not be treated as a values
// box because the constant could change between plan executions.
func TestDecorrelateValuesRule_DoNotTreatRangeWithConstantObjectValueAsValueBox(t *testing.T) {
	t.Parallel()

	endCOV := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"),
		"0", values.NotNullLong,
	)
	rangeExpr := mustDecorrelateConstruct(expressions.NewTableFunctionExpression(
		values.NewRangeValue(
			decorrelateLiteral(int64(0)),
			endCOV,
			decorrelateLiteral(int64(1)),
		)))

	rangeRef := expressions.InitialOf(rangeExpr)
	rangeQ := expressions.ForEachQuantifier(rangeRef)

	notAValuesBox := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateLiteral(int64(42)),
		[]expressions.Quantifier{rangeQ}, nil))

	notAValuesBoxRef := expressions.InitialOf(notAValuesBox)
	notAValuesBoxQ := expressions.ForEachQuantifier(notAValuesBoxRef)

	baseQ, _ := makeBaseScan()

	correlatedSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateObject(notAValuesBoxQ),
				},
			},
		}))

	correlatedRef := expressions.InitialOf(correlatedSel)
	correlatedQ := expressions.ForEachQuantifier(correlatedRef)

	outerSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateField(decorrelateObject(correlatedQ), "b"),
		[]expressions.Quantifier{notAValuesBoxQ, correlatedQ},
		nil))

	outerRef := expressions.InitialOf(outerSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), outerRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (values box over range with ConstantObjectValue), got %d", len(yielded))
	}
}

// TestDecorrelateValuesRule_PushIntoChildSelect ports Java's pushIntoChildSelect.
// A values box with fields {x="hello", y=@0} is joined with a child select
// SELECT a,c,d FROM T WHERE b=v.x AND v.y >= c. The values box is correlated to
// the child select's predicates. The rule pushes the values box INTO the child
// select:
//
//	SELECT t.* FROM values(x='hello', y=@0) AS v, (SELECT a,c,d FROM T WHERE b=v.x AND v.y >= c) AS t
//	→ SELECT t.* FROM (SELECT a,c,d FROM values(x='hello', y=@0) AS v, T WHERE b=v.x AND v.y >= c) AS t
func TestDecorrelateValuesRule_PushIntoChildSelect(t *testing.T) {
	t.Parallel()

	cov := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"),
		"0", values.NotNullBytes,
	)

	// Values box: SELECT {x="hello", y=@0} FROM range(1)
	valuesBoxQ, _ := makeRecordValuesBox(map[string]values.Value{
		"x": decorrelateLiteral("hello"),
		"y": cov,
	})

	// Child select: SELECT a,c,d FROM T WHERE b = v.x AND v.y >= c
	baseQ, _ := makeBaseScan()
	childSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "b"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateField(decorrelateObject(valuesBoxQ), "x"),
				},
			},
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(valuesBoxQ), "y"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonGreaterThanEq,
					Operand: decorrelateField(decorrelateObject(baseQ), "c"),
				},
			},
		}))

	childSelRef := expressions.InitialOf(childSel)
	childSelQ := expressions.ForEachQuantifier(childSelRef)

	// Top select: SELECT t.* FROM values v, (child select) t
	topSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(childSelQ),
		[]expressions.Quantifier{valuesBoxQ, childSelQ},
		nil))

	topRef := expressions.InitialOf(topSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), topRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)

	// Values box removed from top: only the (rewritten) child select quantifier remains.
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (values box pushed into child), got %d", len(decorrelated.GetQuantifiers()))
	}

	// The child select should now have 2 quantifiers: values box + baseQ.
	pushedRef := decorrelated.GetQuantifiers()[0].GetRangesOver()
	if pushedRef == nil {
		t.Fatal("pushed child quantifier has nil Reference")
	}
	pushedSel, ok := pushedRef.Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression after push-down, got %T", pushedRef.Get())
	}
	if len(pushedSel.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers in pushed child (values box + base), got %d", len(pushedSel.GetQuantifiers()))
	}

	// The pushed child should preserve the original predicates.
	if len(pushedSel.GetPredicates()) != 2 {
		t.Fatalf("expected 2 predicates in pushed child, got %d", len(pushedSel.GetPredicates()))
	}
}

// TestDecorrelateValuesRule_PushIntoChildFilter ports Java's pushIntoChildFilter.
// A values box is joined with a LogicalFilterExpression child. The filter
// references values box fields. The rule converts the filter to a SelectExpression
// with the values box prepended.
//
//	SELECT t.d FROM values(x=@0, y="hello", z=tau.gamma) AS v,
//	  (FILTER T WHERE a=v.x AND b=v.y AND c=v.z) AS t
//	→ SELECT t.d FROM (SELECT T.* FROM values(...) AS v, T WHERE a=v.x AND b=v.y AND c=v.z) AS t
func TestDecorrelateValuesRule_PushIntoChildFilter(t *testing.T) {
	t.Parallel()

	cov := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"),
		"0", values.NotNullLong,
	)

	// Values box: SELECT {x=@0, y="hello"} FROM range(1)
	// (simplified from Java: no otherQun correlation for test clarity)
	valuesBoxQ, _ := makeRecordValuesBox(map[string]values.Value{
		"x": cov,
		"y": decorrelateLiteral("hello"),
	})

	// LogicalFilterExpression: FILTER T WHERE a = v.x AND b = v.y
	baseQ, _ := makeBaseScan()
	filterExpr := mustDecorrelateConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateField(decorrelateObject(valuesBoxQ), "x"),
				},
			},
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "b"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateField(decorrelateObject(valuesBoxQ), "y"),
				},
			},
		},
		baseQ))

	filterRef := expressions.InitialOf(filterExpr)
	filterQ := expressions.ForEachQuantifier(filterRef)

	// Top select: SELECT t.d FROM values v, (filter) t
	topSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateField(decorrelateObject(filterQ), "d"),
		[]expressions.Quantifier{valuesBoxQ, filterQ},
		nil))

	topRef := expressions.InitialOf(topSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), topRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)

	// Values box removed from top: only the rewritten child quantifier remains.
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (values box pushed into child), got %d", len(decorrelated.GetQuantifiers()))
	}

	// The pushed child should be a SelectExpression (NOT LogicalFilterExpression)
	// because selectWithQuantifiersPushed always creates a SelectExpression.
	pushedRef := decorrelated.GetQuantifiers()[0].GetRangesOver()
	if pushedRef == nil {
		t.Fatal("pushed child quantifier has nil Reference")
	}
	pushedSel, ok := pushedRef.Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression after push-down into filter, got %T", pushedRef.Get())
	}

	// The pushed select should have 2 quantifiers: values box + baseQ.
	if len(pushedSel.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers in pushed select (values box + base), got %d", len(pushedSel.GetQuantifiers()))
	}

	// The pushed select should preserve the 2 predicates from the filter.
	if len(pushedSel.GetPredicates()) != 2 {
		t.Fatalf("expected 2 predicates in pushed select, got %d", len(pushedSel.GetPredicates()))
	}

	// Result value of the pushed select should be the base's flowed object value
	// (LogicalFilter result = inner's flowed object value).
	rv := pushedSel.GetResultValue()
	if rv == nil {
		t.Fatal("expected non-nil result value in pushed select")
	}
}

// TestDecorrelateValuesRule_PartitionValuesByChild ports Java's partitionValuesByChild.
// Multiple values boxes, each correlated to a different child select.
// Only the relevant values box is pushed into each child. An unreferenced
// values box is trimmed entirely.
//
// Setup:
//   - values0 (cov0) → correlated to select0 (base0.a = values0)
//   - values1 (cov1) → correlated to select1 (values1 = base1.b)
//   - values2 (cov2) → correlated to select2 (base2.c = promote(values2))
//   - values3 (cov3) → NOT referenced by select3 (base3.d IS NULL) → trimmed
//
// Each select gets only the values box it references; values3 is removed.
func TestDecorrelateValuesRule_PartitionValuesByChild(t *testing.T) {
	t.Parallel()

	cov0 := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"),
		"0", values.NotNullLong,
	)
	values0Q, _ := makeValuesBox(cov0)

	cov1 := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"),
		"1", values.NotNullString,
	)
	values1Q, _ := makeValuesBox(cov1)

	cov2 := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"),
		"2", values.NotNullBytes,
	)
	values2Q, _ := makeValuesBox(cov2)

	cov3 := values.NewConstantObjectValue(
		values.NamedCorrelationIdentifier("__const__"),
		"3", values.NotNullBoolean,
	)
	values3Q, _ := makeValuesBox(cov3)

	// select0: SELECT * FROM T WHERE a = values0
	base0Q, _ := makeBaseScan()
	select0 := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(base0Q),
		[]expressions.Quantifier{base0Q},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(base0Q), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateObject(values0Q),
				},
			},
		}))

	select0Ref := expressions.InitialOf(select0)
	select0Q := expressions.ForEachQuantifier(select0Ref)

	// select1: SELECT * FROM T WHERE values1 = b
	base1Q, _ := makeBaseScan()
	select1 := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(base1Q),
		[]expressions.Quantifier{base1Q},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateObject(values1Q),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateField(decorrelateObject(base1Q), "b"),
				},
			},
		}))

	select1Ref := expressions.InitialOf(select1)
	select1Q := expressions.ForEachQuantifier(select1Ref)

	// select2: SELECT * FROM T WHERE c = promote(values2)
	base2Q, _ := makeBaseScan()
	select2 := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(base2Q),
		[]expressions.Quantifier{base2Q},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(base2Q), "c"),
				Comparison: predicates.Comparison{
					Type: predicates.ComparisonEquals,
					Operand: values.NewPromoteValue(
						decorrelateObject(values2Q),
						values.NotNullBytes,
					),
				},
			},
		}))

	select2Ref := expressions.InitialOf(select2)
	select2Q := expressions.ForEachQuantifier(select2Ref)

	// select3: SELECT * FROM T WHERE d IS NULL (NOT correlated to values3)
	base3Q, _ := makeBaseScan()
	select3 := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(base3Q),
		[]expressions.Quantifier{base3Q},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(base3Q), "d"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonIsNull,
					Operand: nil,
				},
			},
		}))

	select3Ref := expressions.InitialOf(select3)
	select3Q := expressions.ForEachQuantifier(select3Ref)

	// Top select: join all values boxes with all selects.
	topSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(select0Q),
		[]expressions.Quantifier{values0Q, select0Q, values1Q, values2Q, select1Q, select2Q, values3Q, select3Q},
		nil))

	topRef := expressions.InitialOf(topSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), topRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)

	// All 4 values boxes removed. Remaining quantifiers: 4 (select0..select3,
	// each possibly rewritten to absorb their values box).
	if len(decorrelated.GetQuantifiers()) != 4 {
		t.Fatalf("expected 4 quantifiers (all values boxes removed), got %d", len(decorrelated.GetQuantifiers()))
	}

	// Verify that select0 was rewritten to include values0.
	pushed0Ref := decorrelated.GetQuantifiers()[0].GetRangesOver()
	pushed0, ok := pushed0Ref.Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression for rewritten select0, got %T", pushed0Ref.Get())
	}
	if len(pushed0.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers in pushed select0 (values0 + base0), got %d", len(pushed0.GetQuantifiers()))
	}

	// Verify that select1 was rewritten to include values1.
	pushed1Ref := decorrelated.GetQuantifiers()[1].GetRangesOver()
	pushed1, ok := pushed1Ref.Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression for rewritten select1, got %T", pushed1Ref.Get())
	}
	if len(pushed1.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers in pushed select1 (values1 + base1), got %d", len(pushed1.GetQuantifiers()))
	}

	// Verify that select2 was rewritten to include values2.
	pushed2Ref := decorrelated.GetQuantifiers()[2].GetRangesOver()
	pushed2, ok := pushed2Ref.Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression for rewritten select2, got %T", pushed2Ref.Get())
	}
	if len(pushed2.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers in pushed select2 (values2 + base2), got %d", len(pushed2.GetQuantifiers()))
	}

	// select3 should be UNCHANGED (not correlated to any values box).
	pushed3Ref := decorrelated.GetQuantifiers()[3].GetRangesOver()
	pushed3, ok := pushed3Ref.Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression for unchanged select3, got %T", pushed3Ref.Get())
	}
	if len(pushed3.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier in unchanged select3 (base3 only), got %d", len(pushed3.GetQuantifiers()))
	}
}

// TestDecorrelateValuesRule_PushIntoExpressionsWithVariations ports Java's
// pushIntoExpressionsWithVariations. A Reference has multiple expression members
// (some correlated to the values box, some not). Only correlated members get
// the values box pushed in; uncorrelated members are left unchanged.
//
// Setup:
//   - valuesBox: SELECT {x=42, y="hello"} FROM range(1)
//   - ref with 3 members:
//     (1) selectBase: SELECT c,d FROM T WHERE a=v.x AND b=v.y  ← correlated
//     (2) distinct(reversePredicates): DISTINCT(SELECT c,d FROM T WHERE v.x=c AND v.y=d) ← correlated
//     (3) selectUncorrelated: SELECT c,d FROM T  ← NOT correlated
//
// Result: values box removed from top. The child Reference's correlated members
// get the values box pushed in; the uncorrelated member is left as-is.
func TestDecorrelateValuesRule_PushIntoExpressionsWithVariations(t *testing.T) {
	t.Parallel()

	// Values box: SELECT {x=42, y="hello"} FROM range(1)
	valuesBoxQ, _ := makeRecordValuesBox(map[string]values.Value{
		"x": decorrelateLiteral(int64(42)),
		"y": decorrelateLiteral("hello"),
	})

	// Base table scan (shared across all 3 members).
	baseQ, _ := makeBaseScan()

	// Member 1: SELECT c, d FROM T WHERE a = v.x AND b = v.y (correlated)
	selectBase := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "a"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateField(decorrelateObject(valuesBoxQ), "x"),
				},
			},
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(baseQ), "b"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateField(decorrelateObject(valuesBoxQ), "y"),
				},
			},
		}))

	// Member 2: DISTINCT(SELECT c,d FROM T WHERE v.x = c AND v.y = d)
	// (correlated, but the correlation is in a child under LogicalDistinct)
	reversePredicatesSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		[]predicates.QueryPredicate{
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(valuesBoxQ), "x"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateField(decorrelateObject(baseQ), "c"),
				},
			},
			&predicates.ComparisonPredicate{
				Operand: decorrelateField(decorrelateObject(valuesBoxQ), "y"),
				Comparison: predicates.Comparison{
					Type:    predicates.ComparisonEquals,
					Operand: decorrelateField(decorrelateObject(baseQ), "d"),
				},
			},
		}))

	reversePredicatesRef := expressions.InitialOf(reversePredicatesSel)
	reversePredicatesQ := expressions.ForEachQuantifier(reversePredicatesRef)
	distinct := mustDecorrelateConstruct(expressions.NewLogicalDistinctExpression(reversePredicatesQ))

	// Member 3: SELECT c,d FROM T (NOT correlated)
	selectUncorrelated := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(baseQ),
		[]expressions.Quantifier{baseQ},
		nil))

	// Create a Reference with all 3 members.
	multiRef := expressions.InitialOf(selectBase)
	multiRef.Insert(distinct)
	multiRef.Insert(selectUncorrelated)

	lowerQ := expressions.ForEachQuantifier(multiRef)

	// Top select: SELECT v.x, v.y, lower.c, lower.d FROM values v, lower
	topSel := mustDecorrelateConstruct(expressions.NewSelectExpression(
		decorrelateObject(lowerQ),
		[]expressions.Quantifier{valuesBoxQ, lowerQ},
		nil))

	topRef := expressions.InitialOf(topSel)

	yielded := mustFireExpressionRule(t, NewDecorrelateValuesRule(), topRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	decorrelated := yielded[0].(*expressions.SelectExpression)

	// Values box removed from top: only the (rewritten) lower quantifier remains.
	if len(decorrelated.GetQuantifiers()) != 1 {
		t.Fatalf("expected 1 quantifier (values box removed from top), got %d", len(decorrelated.GetQuantifiers()))
	}

	// The child Reference should contain members for the rewritten expressions.
	// Check that we have at least 2 members (the correlated ones got rewritten,
	// the uncorrelated one was preserved).
	childRef := decorrelated.GetQuantifiers()[0].GetRangesOver()
	if childRef == nil {
		t.Fatal("rewritten child quantifier has nil Reference")
	}
	members := childRef.AllMembers()
	if len(members) < 2 {
		t.Fatalf("expected at least 2 members in rewritten Reference (correlated pushed + uncorrelated), got %d", len(members))
	}

	// Check: at least one member should be a SelectExpression with 2 quantifiers
	// (the values box pushed into selectBase).
	foundPushedSelect := false
	for _, m := range members {
		sel, ok := m.(*expressions.SelectExpression)
		if !ok {
			continue
		}
		if len(sel.GetQuantifiers()) == 2 {
			foundPushedSelect = true
			break
		}
	}
	if !foundPushedSelect {
		t.Error("expected at least one pushed SelectExpression with 2 quantifiers (values box + base)")
	}

	// Check: the uncorrelated member should still be present (possibly as itself
	// or wrapped in a SelectExpression with the same quantifier count).
	foundUncorrelated := false
	for _, m := range members {
		sel, ok := m.(*expressions.SelectExpression)
		if !ok {
			continue
		}
		if len(sel.GetQuantifiers()) == 1 && len(sel.GetPredicates()) == 0 {
			foundUncorrelated = true
			break
		}
	}
	if !foundUncorrelated {
		t.Error("expected uncorrelated member to be preserved unchanged")
	}
}
