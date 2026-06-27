package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// makeSelectWithOrPredicates builds a SelectExpression with a single ForEach
// quantifier over a scan, carrying the given predicates.
func makeSelectWithOrPredicates(preds []predicates.QueryPredicate) (*expressions.SelectExpression, *expressions.Reference) {
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sel := expressions.NewSelectExpression(
		q.GetFlowedObjectValue(),
		[]expressions.Quantifier{q},
		preds,
	)
	ref := expressions.InitialOf(sel)
	return sel, ref
}

func TestPredicateToLogicalUnionRule_SingleOR(t *testing.T) {
	t.Parallel()

	// SELECT WHERE (A OR B) -> DISTINCT(UNION(UNIQUE(SELECT WHERE A), UNIQUE(SELECT WHERE B)))
	pA := predicates.NewConstantPredicate(predicates.TriTrue)
	pB := predicates.NewConstantPredicate(predicates.TriFalse)
	orPred := predicates.NewOr(pA, pB)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{orPred})
	rule := NewPredicateToLogicalUnionRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}

	// The output should be a LogicalDistinctExpression (simple result value case).
	distinct, ok := yielded[0].(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("yielded type=%T, want *LogicalDistinctExpression", yielded[0])
	}

	// Inside: Union with 2 legs.
	unionRef := distinct.GetInner().GetRangesOver()
	if unionRef == nil {
		t.Fatal("distinct's inner reference is nil")
	}
	union, ok := unionRef.Get().(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("inner type=%T, want *LogicalUnionExpression", unionRef.Get())
	}
	if len(union.GetQuantifiers()) != 2 {
		t.Fatalf("union children=%d, want 2", len(union.GetQuantifiers()))
	}

	// Each leg should be UNIQUE(SELECT WHERE <term>).
	for i, q := range union.GetQuantifiers() {
		legRef := q.GetRangesOver()
		if legRef == nil {
			t.Fatalf("union leg %d reference is nil", i)
		}
		unique, ok := legRef.Get().(*expressions.LogicalUniqueExpression)
		if !ok {
			t.Fatalf("union leg %d type=%T, want *LogicalUniqueExpression", i, legRef.Get())
		}
		selRef := unique.GetInner().GetRangesOver()
		if selRef == nil {
			t.Fatalf("unique leg %d inner reference is nil", i)
		}
		sel, ok := selRef.Get().(*expressions.SelectExpression)
		if !ok {
			t.Fatalf("unique leg %d inner type=%T, want *SelectExpression", i, selRef.Get())
		}
		// Each leg has exactly 1 predicate (the OR term).
		if len(sel.GetPredicates()) != 1 {
			t.Fatalf("leg %d predicate count=%d, want 1", i, len(sel.GetPredicates()))
		}
	}
}

func TestPredicateToLogicalUnionRule_ORWithFixedPredicates(t *testing.T) {
	t.Parallel()

	// SELECT WHERE fixed AND (A OR B)
	// -> DISTINCT(UNION(UNIQUE(SELECT WHERE fixed AND A), UNIQUE(SELECT WHERE fixed AND B)))
	fixed := &predicates.ComparisonPredicate{
		Operand:    values.NewFlatFieldValue("x", values.UnknownType),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
	}
	pA := predicates.NewConstantPredicate(predicates.TriTrue)
	pB := predicates.NewConstantPredicate(predicates.TriFalse)
	orPred := predicates.NewOr(pA, pB)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{fixed, orPred})
	rule := NewPredicateToLogicalUnionRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}

	distinct, ok := yielded[0].(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("yielded type=%T, want *LogicalDistinctExpression", yielded[0])
	}

	union, ok := distinct.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("inner type=%T, want *LogicalUnionExpression", distinct.GetInner().GetRangesOver().Get())
	}

	// Each leg should have 2 predicates: the fixed predicate + the OR term.
	for i, q := range union.GetQuantifiers() {
		unique := q.GetRangesOver().Get().(*expressions.LogicalUniqueExpression)
		sel := unique.GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
		if len(sel.GetPredicates()) != 2 {
			t.Fatalf("leg %d predicate count=%d, want 2", i, len(sel.GetPredicates()))
		}
	}
}

func TestPredicateToLogicalUnionRule_MultipleORs(t *testing.T) {
	t.Parallel()

	// SELECT WHERE (A OR B) AND (C OR D)
	// -> DISTINCT(UNION(
	//      UNIQUE(SELECT WHERE A AND C),
	//      UNIQUE(SELECT WHERE A AND D),
	//      UNIQUE(SELECT WHERE B AND C),
	//      UNIQUE(SELECT WHERE B AND D),
	//    ))
	pA := predicates.NewConstantPredicate(predicates.TriTrue)
	pB := predicates.NewConstantPredicate(predicates.TriFalse)
	pC := &predicates.ComparisonPredicate{
		Operand:    values.NewFlatFieldValue("x", values.UnknownType),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
	}
	pD := &predicates.ComparisonPredicate{
		Operand:    values.NewFlatFieldValue("y", values.UnknownType),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(2)}},
	}
	or1 := predicates.NewOr(pA, pB)
	or2 := predicates.NewOr(pC, pD)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{or1, or2})
	rule := NewPredicateToLogicalUnionRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}

	distinct := yielded[0].(*expressions.LogicalDistinctExpression)
	union := distinct.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)

	// 2 * 2 = 4 cross-product terms.
	if len(union.GetQuantifiers()) != 4 {
		t.Fatalf("union children=%d, want 4", len(union.GetQuantifiers()))
	}
}

func TestPredicateToLogicalUnionRule_DeclinesNoOR(t *testing.T) {
	t.Parallel()

	// SELECT WHERE A AND B (no ORs) -> rule declines.
	pA := predicates.NewConstantPredicate(predicates.TriTrue)
	pB := predicates.NewConstantPredicate(predicates.TriFalse)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{pA, pB})
	rule := NewPredicateToLogicalUnionRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("rule fired despite no OR predicates — yielded %d, want 0", len(yielded))
	}
}

func TestPredicateToLogicalUnionRule_DeclinesNoPredicates(t *testing.T) {
	t.Parallel()

	_, ref := makeSelectWithOrPredicates(nil)
	rule := NewPredicateToLogicalUnionRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("rule fired despite no predicates — yielded %d, want 0", len(yielded))
	}
}

func TestPredicateToLogicalUnionRule_DeclinesExistentialQuantifier(t *testing.T) {
	t.Parallel()

	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	forEachQ := expressions.ForEachQuantifier(scanRef)

	subquery := expressions.NewFullUnorderedScanExpression([]string{"S"}, values.UnknownType)
	existQ := expressions.ExistentialQuantifier(expressions.InitialOf(subquery))

	orPred := predicates.NewOr(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewConstantPredicate(predicates.TriFalse),
	)

	sel := expressions.NewSelectExpression(
		forEachQ.GetFlowedObjectValue(),
		[]expressions.Quantifier{forEachQ, existQ},
		[]predicates.QueryPredicate{orPred},
	)
	ref := expressions.InitialOf(sel)

	rule := NewPredicateToLogicalUnionRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("rule fired despite existential quantifier — yielded %d, want 0", len(yielded))
	}
}

func TestPredicateToLogicalUnionRule_DeclinesMultipleForEach(t *testing.T) {
	t.Parallel()

	// Two ForEach quantifiers (a join) — rule should decline.
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"T1"}, values.UnknownType)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"T2"}, values.UnknownType)
	q1 := expressions.ForEachQuantifier(expressions.InitialOf(scan1))
	q2 := expressions.ForEachQuantifier(expressions.InitialOf(scan2))

	orPred := predicates.NewOr(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewConstantPredicate(predicates.TriFalse),
	)

	sel := expressions.NewSelectExpression(
		q1.GetFlowedObjectValue(),
		[]expressions.Quantifier{q1, q2},
		[]predicates.QueryPredicate{orPred},
	)
	ref := expressions.InitialOf(sel)

	rule := NewPredicateToLogicalUnionRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 0 {
		t.Fatalf("rule fired despite multiple ForEach quantifiers — yielded %d, want 0", len(yielded))
	}
}

func TestPredicateToLogicalUnionRule_NonSimpleResultValue(t *testing.T) {
	t.Parallel()

	// When the result value is NOT a simple QuantifiedObjectValue, the
	// rule wraps the result in an outer SelectExpression.
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)

	// Use a RecordConstructorValue as a non-simple result value.
	resultValue := values.NewRecordConstructorValue(values.RecordConstructorField{
		Name: "a", Value: values.NewFlatFieldValue("a", values.UnknownType),
	})

	orPred := predicates.NewOr(
		predicates.NewConstantPredicate(predicates.TriTrue),
		predicates.NewConstantPredicate(predicates.TriFalse),
	)

	sel := expressions.NewSelectExpression(
		resultValue,
		[]expressions.Quantifier{q},
		[]predicates.QueryPredicate{orPred},
	)
	ref := expressions.InitialOf(sel)

	rule := NewPredicateToLogicalUnionRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}

	// The output should be a SelectExpression wrapping the Distinct(Union(...)).
	outerSel, ok := yielded[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("yielded type=%T, want *SelectExpression (non-simple result)", yielded[0])
	}

	// No predicates on the outer select.
	if len(outerSel.GetPredicates()) != 0 {
		t.Fatalf("outer select predicate count=%d, want 0", len(outerSel.GetPredicates()))
	}

	// Inner should be Distinct.
	innerRef := outerSel.GetQuantifiers()[0].GetRangesOver()
	_, ok = innerRef.Get().(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("outer select's inner type=%T, want *LogicalDistinctExpression", innerRef.Get())
	}
}

func TestPredicateToLogicalUnionRule_Convergence(t *testing.T) {
	t.Parallel()

	// Verify the rule does NOT re-fire on its own output. Run through
	// the planner with just this rule and NormalizePredicatesRule.
	pA := predicates.NewConstantPredicate(predicates.TriTrue)
	pB := predicates.NewConstantPredicate(predicates.TriFalse)
	orPred := predicates.NewOr(pA, pB)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{orPred})

	rules := []ExpressionRule{
		NewNormalizePredicatesRule(),
		NewPredicateToLogicalUnionRule(),
	}
	_, converged := FixpointApply(rules, ref, 100)
	if !converged {
		t.Fatal("did not converge — rule is re-firing on its own output")
	}
}

func TestPredicateToLogicalUnionRule_ThreeWayOR(t *testing.T) {
	t.Parallel()

	// SELECT WHERE (A OR B OR C) -> DISTINCT(UNION(3 legs))
	pA := predicates.NewConstantPredicate(predicates.TriTrue)
	pB := predicates.NewConstantPredicate(predicates.TriFalse)
	pC := &predicates.ComparisonPredicate{
		Operand:    values.NewFlatFieldValue("x", values.UnknownType),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
	}
	orPred := predicates.NewOr(pA, pB, pC)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{orPred})
	rule := NewPredicateToLogicalUnionRule()
	yielded := FireExpressionRule(rule, ref)

	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}

	distinct := yielded[0].(*expressions.LogicalDistinctExpression)
	union := distinct.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)

	if len(union.GetQuantifiers()) != 3 {
		t.Fatalf("union children=%d, want 3", len(union.GetQuantifiers()))
	}
}

func TestOrsToDNFTerms_TwoByTwo(t *testing.T) {
	t.Parallel()

	pA := predicates.NewConstantPredicate(predicates.TriTrue)
	pB := predicates.NewConstantPredicate(predicates.TriFalse)
	pC := &predicates.ComparisonPredicate{
		Operand:    values.NewFlatFieldValue("x", values.UnknownType),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
	}
	pD := &predicates.ComparisonPredicate{
		Operand:    values.NewFlatFieldValue("y", values.UnknownType),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(2)}},
	}

	or1 := predicates.NewOr(pA, pB)
	or2 := predicates.NewOr(pC, pD)

	terms := orsToDNFTerms([]*predicates.OrPredicate{or1, or2})

	// 2 * 2 = 4 terms.
	if len(terms) != 4 {
		t.Fatalf("DNF terms=%d, want 4", len(terms))
	}

	// Each term should be an AND of 2 predicates.
	for i, term := range terms {
		and, ok := term.(*predicates.AndPredicate)
		if !ok {
			t.Fatalf("term %d type=%T, want *AndPredicate", i, term)
		}
		if len(and.SubPredicates) != 2 {
			t.Fatalf("term %d children=%d, want 2", i, len(and.SubPredicates))
		}
	}
}

func TestOrsToDNFTerms_ThreeByTwo(t *testing.T) {
	t.Parallel()

	pA := predicates.NewConstantPredicate(predicates.TriTrue)
	pB := predicates.NewConstantPredicate(predicates.TriFalse)
	pC := &predicates.ComparisonPredicate{
		Operand:    values.NewFlatFieldValue("x", values.UnknownType),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: &values.ConstantValue{Value: int64(1)}},
	}

	or1 := predicates.NewOr(pA, pB)
	or2 := predicates.NewOr(pC)

	terms := orsToDNFTerms([]*predicates.OrPredicate{or1, or2})

	// 2 * 1 = 2 terms.
	if len(terms) != 2 {
		t.Fatalf("DNF terms=%d, want 2", len(terms))
	}
}
