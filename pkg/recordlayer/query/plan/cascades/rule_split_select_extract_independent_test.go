package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestSplitSelectExtractIndependentQuantifiersRule_Fires(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	scanQ := expressions.ForEachQuantifier(scanRef)

	explode := expressions.NewExplodeExpression(values.LiteralValue([]any{int64(1), int64(2)}))
	explodeRef := expressions.InitialOf(explode)
	explodeQ := expressions.ForEachQuantifier(explodeRef)

	// The predicate makes the SELECT non-trivial, but it belongs entirely to
	// the scan leg. The independent explode leg is therefore eligible for
	// extraction into the outer SELECT.
	pred := predicates.NewComparisonPredicate(
		values.NewFieldValue(
			values.NewQuantifiedObjectValue(scanQ.GetAlias()),
			"ID",
			values.NullableLong,
		),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)),
	)
	originalResult := scanQ.GetFlowedObjectValue()
	sel := expressions.NewSelectExpression(
		originalResult,
		[]expressions.Quantifier{scanQ, explodeQ},
		[]predicates.QueryPredicate{pred},
	)

	// Invoke one matched binding directly. FireExpressionRule also exercises
	// the Select ChildrenAsSet permutation and therefore reports both
	// equivalent quantifier orders; this test isolates the rule's one firing.
	ref := expressions.InitialOf(sel)
	rule := NewSplitSelectExtractIndependentQuantifiersRule()
	bindings := rule.Matcher().BindMatches(matching.NewBindings(), sel)
	if len(bindings) != 1 {
		t.Fatalf("matcher produced %d bindings, want one", len(bindings))
	}
	call := NewExpressionRuleCall(ref, bindings[0], EmptyPlanContext())
	rule.OnMatch(call)
	yielded := call.Yielded()
	if len(yielded) != 1 {
		t.Fatalf("expected one split SELECT, got %d", len(yielded))
	}

	outer, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("yielded %T, want *expressions.SelectExpression", yielded[0])
	}
	outerQs := outer.GetQuantifiers()
	if len(outerQs) != 2 {
		t.Fatalf("outer SELECT has %d quantifiers, want explode + lower SELECT", len(outerQs))
	}
	if outerQs[0].GetRangesOver() != explodeRef {
		t.Fatal("independent explode quantifier was not extracted to the outer SELECT")
	}
	outerResult, ok := outer.GetResultValue().(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("outer result = %T, want QuantifiedObjectValue over the lower SELECT", outer.GetResultValue())
	}
	if outerResult.Correlation != outerQs[1].GetAlias() {
		t.Fatalf(
			"outer result alias = %s, want lower SELECT alias %s",
			outerResult.Correlation.Name(),
			outerQs[1].GetAlias().Name(),
		)
	}

	lowerRef := outerQs[1].GetRangesOver()
	if lowerRef == nil {
		t.Fatal("outer SELECT has no lower SELECT reference")
	}
	lower, ok := lowerRef.Get().(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("lower member is %T, want *expressions.SelectExpression", lowerRef.Get())
	}
	lowerQs := lower.GetQuantifiers()
	if len(lowerQs) != 1 || lowerQs[0].GetRangesOver() != scanRef {
		t.Fatalf("lower SELECT quantifiers = %#v, want only the scan leg", lowerQs)
	}
	if len(lower.GetPredicates()) != 1 {
		t.Fatalf("lower SELECT has %d predicates, want the original predicate", len(lower.GetPredicates()))
	}
	if lower.GetPredicates()[0] != pred {
		t.Fatal("lower SELECT did not retain the original predicate")
	}
	if !values.ValuesStructurallyEqual(lower.GetResultValue(), originalResult) {
		t.Fatalf(
			"lower result = %s, want original result %s",
			values.ExplainValue(lower.GetResultValue()),
			values.ExplainValue(originalResult),
		)
	}
}

// TestSplitSelectExtractIndependentQuantifiersRule_OuterJoinFailsClosed pins
// the ChildrenAsSet() guard: both halves this rule builds
// (NewSelectExpression) always default to JoinInner, so splitting a
// JoinLeftOuter select would silently erase its null-extension semantics.
// The quantifier shape is identical to
// TestSplitSelectExtractIndependentQuantifiersRule_Fires (an independent
// Explode leg extractable into the outer SELECT), so JoinLeftOuter alone
// must be what blocks the split.
func TestSplitSelectExtractIndependentQuantifiersRule_OuterJoinFailsClosed(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))

	explode := expressions.NewExplodeExpression(values.LiteralValue([]any{int64(1), int64(2)}))
	explodeQ := expressions.ForEachQuantifier(expressions.InitialOf(explode))

	pred := predicates.NewComparisonPredicate(
		values.NewFieldValue(
			values.NewQuantifiedObjectValue(scanQ.GetAlias()),
			"ID",
			values.NullableLong,
		),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)),
	)
	sel := expressions.NewSelectExpressionWithJoinType(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{scanQ, explodeQ},
		[]predicates.QueryPredicate{pred},
		nil,
		expressions.JoinLeftOuter,
	)

	yielded := FireExpressionRule(
		NewSplitSelectExtractIndependentQuantifiersRule(), expressions.InitialOf(sel))
	if len(yielded) != 0 {
		t.Fatalf("LEFT OUTER select yielded %d split(s), want fail-closed zero", len(yielded))
	}
}

func TestSplitSelectExtractIndependentQuantifiersRule_StrictSingleFailsClosed(t *testing.T) {
	t.Parallel()

	scanRef := expressions.InitialOf(
		expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType))
	scanAlias := values.NamedCorrelationIdentifier("STRICT")
	scanQ := expressions.NamedForEachStrictSingleQuantifier(scanAlias, scanRef)
	explodeQ := expressions.ForEachQuantifier(expressions.InitialOf(
		expressions.NewExplodeExpression(values.LiteralValue([]any{int64(1), int64(2)}))))
	pred := predicates.NewComparisonPredicate(
		values.NewFieldValue(scanQ.GetFlowedObjectValue(), "ID", values.NullableLong),
		predicates.NewLiteralComparison(predicates.ComparisonGreaterThan, int64(0)),
	)
	sel := expressions.NewSelectExpression(
		scanQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{scanQ, explodeQ},
		[]predicates.QueryPredicate{pred},
	)

	yielded := FireExpressionRule(
		NewSplitSelectExtractIndependentQuantifiersRule(), expressions.InitialOf(sel))
	if len(yielded) != 0 {
		t.Fatalf("strict-single select yielded %d split(s), want zero", len(yielded))
	}
}
