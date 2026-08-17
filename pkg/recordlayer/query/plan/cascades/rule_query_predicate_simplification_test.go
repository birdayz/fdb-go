package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func queryPredicateFixture() (expressions.Quantifier, values.QuantifiedObjectValue) {
	rowType := values.NewRecordType("QueryPredicateRuleRow", false, []values.Field{
		{Name: "NAME", FieldType: values.NullableString},
		{Name: "A", FieldType: values.NullableLong},
		{Name: "B", FieldType: values.NullableString},
		{Name: "X", FieldType: values.NullableLong},
	})
	scan := mustTypeRewriteConstruct(expressions.NewFullUnorderedScanExpression([]string{"T"}, rowType))
	q := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	return q, mustTypeRewriteConstruct(q.RequireFlowedObjectValue())
}

func queryPredicateField(root values.Value, ordinal int) values.Value {
	return mustTypeRewriteConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func queryPredicateSelect(
	q expressions.Quantifier,
	root values.Value,
	preds []predicates.QueryPredicate,
) *expressions.SelectExpression {
	return mustTypeRewriteConstruct(expressions.NewSelectExpression(
		root, []expressions.Quantifier{q}, preds))
}

func fireQueryPredicateRule(
	t testing.TB, rule ExpressionRule, ref *expressions.Reference,
) []expressions.RelationalExpression {
	t.Helper()
	result, err := FireExpressionRule(rule, ref)
	if err != nil {
		t.Fatalf("FireExpressionRule: %v", err)
	}
	return result
}

// TestQueryPredicateSimplification_FoldsArithmetic verifies that a
// ComparisonPredicate with an ArithmeticValue operand (e.g., name = 1+2)
// is simplified to name = 3.
func TestQueryPredicateSimplification_FoldsArithmetic(t *testing.T) {
	t.Parallel()

	scanQ, scanRoot := queryPredicateFixture()

	// name = 1 + 2 (ArithmeticValue)
	pred := &predicates.ComparisonPredicate{
		Operand: queryPredicateField(scanRoot, 0),
		Comparison: predicates.Comparison{
			Type: predicates.ComparisonEquals,
			Operand: &values.ArithmeticValue{
				Left:  &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
				Op:    values.OpAdd,
				Right: &values.ConstantValue{Value: int64(2), Typ: values.NotNullLong},
			},
		},
	}

	sel := queryPredicateSelect(scanQ, scanRoot, []predicates.QueryPredicate{pred})
	selRef := expressions.InitialOf(sel)

	yielded := fireQueryPredicateRule(t, NewQueryPredicateSimplificationRule(), selRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(result.GetPredicates()))
	}

	cp, ok := result.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", result.GetPredicates()[0])
	}
	cv, ok := cp.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue after simplification, got %T", cp.Comparison.Operand)
	}
	if cv.Value != int64(3) {
		t.Errorf("expected 3, got %v", cv.Value)
	}
}

// TestQueryPredicateSimplification_NoChangeNoYield verifies that if no
// predicates change (nothing to fold), the rule does not yield.
func TestQueryPredicateSimplification_NoChangeNoYield(t *testing.T) {
	t.Parallel()

	scanQ, scanRoot := queryPredicateFixture()

	// Simple predicate — nothing to fold.
	pred := &predicates.ComparisonPredicate{
		Operand: queryPredicateField(scanRoot, 0),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "foo", Typ: values.NotNullString},
		},
	}

	sel := queryPredicateSelect(scanQ, scanRoot, []predicates.QueryPredicate{pred})
	selRef := expressions.InitialOf(sel)

	yielded := fireQueryPredicateRule(t, NewQueryPredicateSimplificationRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (nothing to simplify), got %d", len(yielded))
	}
}

// TestQueryPredicateSimplification_NoPredicates verifies the rule
// yields nothing when the SelectExpression has no predicates.
func TestQueryPredicateSimplification_NoPredicates(t *testing.T) {
	t.Parallel()

	scanQ, scanRoot := queryPredicateFixture()

	sel := queryPredicateSelect(scanQ, scanRoot, nil)
	selRef := expressions.InitialOf(sel)

	yielded := fireQueryPredicateRule(t, NewQueryPredicateSimplificationRule(), selRef)
	if len(yielded) != 0 {
		t.Fatalf("expected 0 yields (no predicates), got %d", len(yielded))
	}
}

// TestQueryPredicateSimplification_MultiplePredicates verifies that
// when multiple predicates exist, only the ones that change are
// replaced — and the rule yields if any changed.
func TestQueryPredicateSimplification_MultiplePredicates(t *testing.T) {
	t.Parallel()

	scanQ, scanRoot := queryPredicateFixture()

	// First predicate: foldable (2+3)
	pred1 := &predicates.ComparisonPredicate{
		Operand: queryPredicateField(scanRoot, 1),
		Comparison: predicates.Comparison{
			Type: predicates.ComparisonEquals,
			Operand: &values.ArithmeticValue{
				Left:  &values.ConstantValue{Value: int64(2), Typ: values.NotNullLong},
				Op:    values.OpAdd,
				Right: &values.ConstantValue{Value: int64(3), Typ: values.NotNullLong},
			},
		},
	}
	// Second predicate: not foldable
	pred2 := &predicates.ComparisonPredicate{
		Operand: queryPredicateField(scanRoot, 2),
		Comparison: predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: "hello", Typ: values.NotNullString},
		},
	}

	sel := queryPredicateSelect(scanQ, scanRoot, []predicates.QueryPredicate{pred1, pred2})
	selRef := expressions.InitialOf(sel)

	yielded := fireQueryPredicateRule(t, NewQueryPredicateSimplificationRule(), selRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	if len(result.GetPredicates()) != 2 {
		t.Fatalf("expected 2 predicates, got %d", len(result.GetPredicates()))
	}

	// First predicate should be simplified.
	cp1, ok := result.GetPredicates()[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", result.GetPredicates()[0])
	}
	cv1, ok := cp1.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue after simplification, got %T", cp1.Comparison.Operand)
	}
	if cv1.Value != int64(5) {
		t.Errorf("expected 5, got %v", cv1.Value)
	}

	// Second predicate should be unchanged.
	cp2, ok := result.GetPredicates()[1].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", result.GetPredicates()[1])
	}
	cv2, ok := cp2.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue, got %T", cp2.Comparison.Operand)
	}
	if cv2.Value != "hello" {
		t.Errorf("expected 'hello', got %v", cv2.Value)
	}
}

// TestQueryPredicateSimplification_AndPredicate verifies that
// simplification recurses into AND predicates.
func TestQueryPredicateSimplification_AndPredicate(t *testing.T) {
	t.Parallel()

	scanQ, scanRoot := queryPredicateFixture()

	andPred := predicates.NewAnd(
		&predicates.ComparisonPredicate{
			Operand: queryPredicateField(scanRoot, 3),
			Comparison: predicates.Comparison{
				Type: predicates.ComparisonEquals,
				Operand: &values.ArithmeticValue{
					Left:  &values.ConstantValue{Value: int64(10), Typ: values.NotNullLong},
					Op:    values.OpAdd,
					Right: &values.ConstantValue{Value: int64(20), Typ: values.NotNullLong},
				},
			},
		},
		predicates.NewConstantPredicate(predicates.TriTrue),
	)

	sel := queryPredicateSelect(scanQ, scanRoot, []predicates.QueryPredicate{andPred})
	selRef := expressions.InitialOf(sel)

	yielded := fireQueryPredicateRule(t, NewQueryPredicateSimplificationRule(), selRef)
	if len(yielded) < 1 {
		t.Fatalf("expected at least 1 yield, got %d", len(yielded))
	}

	result := yielded[0].(*expressions.SelectExpression)
	// The AND predicate should have its first child simplified.
	ap, ok := result.GetPredicates()[0].(*predicates.AndPredicate)
	if !ok {
		t.Fatalf("expected AndPredicate, got %T", result.GetPredicates()[0])
	}
	cp, ok := ap.SubPredicates[0].(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected ComparisonPredicate, got %T", ap.SubPredicates[0])
	}
	cv, ok := cp.Comparison.Operand.(*values.ConstantValue)
	if !ok {
		t.Fatalf("expected ConstantValue, got %T", cp.Comparison.Operand)
	}
	if cv.Value != int64(30) {
		t.Errorf("expected 30, got %v", cv.Value)
	}
}

// TestRewritingRules_ContainsExpectedRules verifies the RewritingRules
// function returns the expected set of rules.
func TestRewritingRules_ContainsExpectedRules(t *testing.T) {
	t.Parallel()

	rules := RewritingRules()
	if len(rules) != 4 {
		t.Fatalf("expected 4 rewriting rules, got %d", len(rules))
	}

	// Check types.
	if _, ok := rules[0].(*QueryPredicateSimplificationRule); !ok {
		t.Errorf("rules[0]: expected QueryPredicateSimplificationRule, got %T", rules[0])
	}
	if _, ok := rules[1].(*PredicatePushDownRule); !ok {
		t.Errorf("rules[1]: expected PredicatePushDownRule, got %T", rules[1])
	}
	if _, ok := rules[2].(*DecorrelateValuesRule); !ok {
		t.Errorf("rules[2]: expected DecorrelateValuesRule, got %T", rules[2])
	}
	if _, ok := rules[3].(*RewriteOuterJoinRule); !ok {
		t.Errorf("rules[3]: expected RewriteOuterJoinRule, got %T", rules[3])
	}
}
