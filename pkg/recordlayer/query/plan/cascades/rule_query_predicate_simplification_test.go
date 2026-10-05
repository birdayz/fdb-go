package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
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

func queryPredicateComparison(t testing.TB, pred predicates.QueryPredicate) *predicates.ComparisonPredicate {
	t.Helper()
	residual, err := predicates.ToResidualPredicate(pred)
	if err != nil {
		t.Fatalf("ToResidualPredicate: %v", err)
	}
	comparison, ok := residual.(*predicates.ComparisonPredicate)
	if !ok {
		t.Fatalf("expected a single ComparisonPredicate residual, got %T", residual)
	}
	return comparison
}

func TestQueryPredicateSimplification_WholeConjunction(t *testing.T) {
	t.Parallel()
	q, root := queryPredicateFixture()
	a := predicates.NewComparisonPredicate(queryPredicateField(root, 1), predicates.Comparison{
		Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong},
	})
	b := predicates.NewComparisonPredicate(queryPredicateField(root, 3), predicates.Comparison{
		Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(4), Typ: values.NotNullLong},
	})
	// Equality is class-sensitive: use the normalized form of a at both levels.
	normalizedA := predicates.NewPredicateWithValueAndRanges(a.Operand, []*predicates.RangeConstraints{
		predicates.NewRangeConstraints([]predicates.Comparison{a.Comparison}, nil),
	})
	sel := queryPredicateSelect(q, root, []predicates.QueryPredicate{normalizedA, &predicates.OrPredicate{SubPredicates: []predicates.QueryPredicate{normalizedA, b}}})
	yielded := fireQueryPredicateRule(t, NewQueryPredicateSimplificationRule(), expressions.InitialOf(sel))
	if len(yielded) != 1 {
		t.Fatalf("whole-conjunction absorption yielded %d expressions, want 1", len(yielded))
	}
	result := yielded[0].(*expressions.SelectExpression)
	if got := result.GetPredicates(); len(got) != 1 || !predicates.PredicateEquals(queryPredicateComparison(t, got[0]), a) {
		t.Fatalf("absorption retained redundant predicates: %v", got)
	}
	if again := fireQueryPredicateRule(t, NewQueryPredicateSimplificationRule(), expressions.InitialOf(result)); len(again) != 0 {
		t.Fatalf("simplified conjunction yielded again: %v", again)
	}
}

func TestQueryPredicateSimplification_RangeFixpoint(t *testing.T) {
	t.Parallel()
	q, root := queryPredicateFixture()
	p := predicates.NewPredicateWithValueAndRanges(queryPredicateField(root, 1), []*predicates.RangeConstraints{
		predicates.NewRangeConstraints([]predicates.Comparison{{Type: predicates.ComparisonGreaterThan, Operand: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}}}, nil),
	})
	sel := queryPredicateSelect(q, root, []predicates.QueryPredicate{p})
	yielded := fireQueryPredicateRule(t, NewQueryPredicateSimplificationRule(), expressions.InitialOf(sel))
	if len(yielded) != 0 {
		t.Fatalf("unchanged range predicate yielded %d expressions", len(yielded))
	}
}

func TestQueryPredicateSimplification_PreservesSwappedMetadata(t *testing.T) {
	t.Parallel()
	q, root := queryPredicateFixture()
	other, _ := queryPredicateFixture()
	p := predicates.NewComparisonPredicate(queryPredicateField(root, 1), predicates.Comparison{
		Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong},
	})
	sel := mustTypeRewriteConstruct(expressions.NewSelectExpressionWithJoinType(root, []expressions.Quantifier{q, other}, []predicates.QueryPredicate{p, predicates.NewConstantPredicate(predicates.TriTrue)}, []string{"first", "second"}, expressions.JoinLeftOuter)).WithSwappedQuantifiers()
	rule := NewQueryPredicateSimplificationRule()
	matches := rule.Matcher().BindMatches(matching.NewBindings(), sel)
	if len(matches) != 1 {
		t.Fatalf("rule bindings=%d, want 1", len(matches))
	}
	call := &ExpressionRuleCall{Bindings: matches[0], Reference: expressions.InitialOf(sel)}
	rule.OnMatch(call)
	if call.Err() != nil {
		t.Fatal(call.Err())
	}
	yielded := call.Yielded()
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	result := yielded[0].(*expressions.SelectExpression)
	if !result.IsQuantifiersSwapped() {
		t.Fatal("simplification erased quantifier-swap metadata")
	}
	if result.GetResultValue() != root || result.GetJoinType() != expressions.JoinLeftOuter {
		t.Fatal("simplification changed the projection or join kind")
	}
	if got := result.GetSourceAliases(); len(got) != 2 || got[0] != "second" || got[1] != "first" {
		t.Fatalf("simplification changed source aliases: %v", got)
	}
	qs := result.GetQuantifiers()
	if len(qs) != 2 || qs[0].GetAlias() != other.GetAlias() || qs[1].GetAlias() != q.GetAlias() {
		t.Fatal("simplification changed the swapped edges")
	}
	if len(sel.GetPredicates()) != 2 || len(result.GetPredicates()) != 1 {
		t.Fatal("predicate replacement mutated the input or retained the identity")
	}
	if !predicates.PredicateEquals(queryPredicateComparison(t, result.GetPredicates()[0]), p) {
		t.Fatal("simplification changed the surviving comparison")
	}
	if !predicates.PredicateEquals(sel.GetPredicates()[0], predicates.NewConstantPredicate(predicates.TriTrue)) ||
		!predicates.PredicateEquals(queryPredicateComparison(t, sel.GetPredicates()[1]), p) {
		t.Fatal("predicate replacement mutated the input predicates")
	}
}

func TestQueryPredicateSimplification_PromotedAtomicChild(t *testing.T) {
	t.Parallel()
	_, root := queryPredicateFixture()
	compare := func(ordinal int) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(queryPredicateField(root, ordinal), predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong},
		})
	}
	for _, kind := range []string{"and", "or", "not"} {
		for _, promotion := range []string{"and_identity", "or_identity", "double_not", "nested"} {
			t.Run(kind+"/"+promotion, func(t *testing.T) {
				t.Parallel()
				var fixed predicates.QueryPredicate
				switch kind {
				case "and":
					fixed = predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), compare(1))
				case "or":
					fixed = predicates.NewOr(compare(1), predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), compare(3)))
				case "not":
					fixed = predicates.NewNot(predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), compare(1)))
				}
				fixed = predicates.WithAtomicity(fixed, true)
				var input predicates.QueryPredicate
				switch promotion {
				case "and_identity":
					input = predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), fixed)
				case "or_identity":
					input = predicates.NewOr(predicates.NewConstantPredicate(predicates.TriFalse), fixed)
				case "double_not":
					input = predicates.NewNot(predicates.NewNot(fixed))
				case "nested":
					input = predicates.NewAnd(predicates.NewConstantPredicate(predicates.TriTrue), predicates.NewOr(predicates.NewConstantPredicate(predicates.TriFalse), fixed))
				}
				out, err := Simplify(input, queryPredicateSimplificationRules())
				if err != nil {
					t.Fatal(err)
				}
				negate := func(ordinal int) predicates.QueryPredicate {
					return predicates.NewComparisonPredicate(queryPredicateField(root, ordinal), predicates.Comparison{
						Type: predicates.ComparisonNotEquals, Operand: &values.ConstantValue{Value: int64(7), Typ: values.NotNullLong},
					})
				}
				var want predicates.QueryPredicate
				switch kind {
				case "and":
					want = compare(1)
					if promotion == "double_not" {
						want = predicates.NewNot(negate(1))
					}
				case "or":
					want = predicates.WithAtomicity(predicates.NewOr(compare(1), compare(3)), true)
					if promotion == "double_not" {
						want = predicates.NewOr(predicates.NewNot(negate(1)), predicates.NewNot(negate(3)))
					}
				case "not":
					want = negate(1)
					if promotion == "double_not" {
						want = predicates.NewNot(predicates.NewNot(want))
					}
				}
				assertSimplificationTree(t, out, want)
			})
		}
	}
}

// TestQueryPredicateSimplification_DoesNotEvaluateArithmetic verifies that a
// comparison against constant arithmetic (name = 1+2) is left as it is: Java's
// ConstantFoldingRuleSet evaluates no constant, so the rule yields nothing.
func TestQueryPredicateSimplification_DoesNotEvaluateArithmetic(t *testing.T) {
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

	if yielded := fireQueryPredicateRule(t, NewQueryPredicateSimplificationRule(), selRef); len(yielded) != 0 {
		t.Fatalf("expected no yield, got %d", len(yielded))
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
// when multiple predicates exist and none is changed by Java's set (constant
// arithmetic is not evaluated), the rule does not yield.
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

	if yielded := fireQueryPredicateRule(t, NewQueryPredicateSimplificationRule(), selRef); len(yielded) != 0 {
		t.Fatalf("expected no yield, got %d", len(yielded))
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
	// Java's identity-AND rule removes TRUE; the comparison keeps its
	// arithmetic, which Java's set does not evaluate.
	if got := len(result.GetPredicates()); got != 1 {
		t.Fatalf("expected only the comparison, got %d: %v", got, result.GetPredicates())
	}
	cp := queryPredicateComparison(t, result.GetPredicates()[0])
	if _, ok := cp.Comparison.Operand.(*values.ArithmeticValue); !ok {
		t.Fatalf("expected the arithmetic kept, got %T", cp.Comparison.Operand)
	}
}

// TestRewritingRules_ContainsExpectedRules pins Java's RewritingRuleSet: the
// exploration rules are decorrelation-then-simplification and the outer-join
// rewrite; SelectMerge-then-PushDown and the finalizer are implementation rules.
func TestRewritingRules_ContainsExpectedRules(t *testing.T) {
	t.Parallel()

	rules := RewritingRules()
	if len(rules) != 2 {
		t.Fatalf("expected 2 rewriting exploration rules, got %d", len(rules))
	}
	cond, ok := rules[0].(*conditionalExpressionRule)
	if !ok || len(cond.rules) != 2 {
		t.Fatalf("rules[0]: expected a two-rule conditional, got %T", rules[0])
	}
	if _, ok := cond.rules[0].(*DecorrelateValuesRule); !ok {
		t.Errorf("conditional[0]: expected DecorrelateValuesRule, got %T", cond.rules[0])
	}
	if _, ok := cond.rules[1].(*QueryPredicateSimplificationRule); !ok {
		t.Errorf("conditional[1]: expected QueryPredicateSimplificationRule, got %T", cond.rules[1])
	}
	if _, ok := rules[1].(*RewriteOuterJoinRule); !ok {
		t.Errorf("rules[1]: expected RewriteOuterJoinRule, got %T", rules[1])
	}

	impl := RewritingImplementationRules()
	if len(impl) != 2 {
		t.Fatalf("expected 2 rewriting implementation rules, got %d", len(impl))
	}
	implCond, ok := impl[0].(*conditionalImplementationRule)
	if !ok || len(implCond.rules) != 2 {
		t.Fatalf("impl[0]: expected a two-rule conditional, got %T", impl[0])
	}
	if _, ok := implCond.rules[0].(*SelectMergeRule); !ok {
		t.Errorf("impl conditional[0]: expected SelectMergeRule, got %T", implCond.rules[0])
	}
	if _, ok := implCond.rules[1].(*PredicatePushDownRule); !ok {
		t.Errorf("impl conditional[1]: expected PredicatePushDownRule, got %T", implCond.rules[1])
	}
	if !isPrunedInputsRule(impl[0]) {
		t.Error("SelectMerge-then-PushDown must run only on pruned inputs")
	}
	if _, ok := impl[1].(*FinalizeExpressionsRule); !ok {
		t.Errorf("impl[1]: expected FinalizeExpressionsRule, got %T", impl[1])
	}
}
