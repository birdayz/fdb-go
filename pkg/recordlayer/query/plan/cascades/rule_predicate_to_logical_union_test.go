package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

const predicateUnionSourceAlias = "predicate_union_source"

func mustPredicateUnionConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct predicate-to-logical-union fixture: " + err.Error())
	}
	return value
}

// Geometry tests supply admitted OR factors; matcher admission is tested separately.
func mustExplorePredicateUnion(t testing.TB, ref *expressions.Reference) []expressions.RelationalExpression {
	t.Helper()
	sel := ref.Get().(*expressions.SelectExpression)
	matched := make(map[predicates.QueryPredicate]struct{})
	for _, predicate := range sel.GetPredicates() {
		if _, ok := predicate.(*predicates.OrPredicate); ok {
			matched[predicate] = struct{}{}
		}
	}
	call := NewExpressionRuleCall(ref, nil, nil)
	explorePredicateUnion(call, sel, matched)
	if err := call.Err(); err != nil {
		t.Fatal(err)
	}
	return call.Yielded()
}

func predicateUnionRowType() *values.RecordType {
	return values.NewRecordType("predicate_to_logical_union_row", false, []values.Field{
		{Name: "x", FieldType: values.NotNullLong},
		{Name: "y", FieldType: values.NotNullLong},
		{Name: "a", FieldType: values.NotNullLong},
	})
}

func predicateUnionScan(recordType string) *expressions.FullUnorderedScanExpression {
	return mustPredicateUnionConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{recordType}, predicateUnionRowType()))
}

func predicateUnionForEach(recordType, alias string) expressions.Quantifier {
	return expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(alias),
		expressions.InitialOf(predicateUnionScan(recordType)),
	)
}

func predicateUnionField(alias, field string) values.Value {
	root := mustPredicateUnionConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(alias), predicateUnionRowType()))
	request := mustPredicateUnionConstruct(values.FieldByName(field))
	return mustPredicateUnionConstruct(values.ResolveFieldAccess(
		root, []values.FieldRequest{request}))
}

func predicateUnionEquals(alias, field string, operand int64) predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(
		predicateUnionField(alias, field),
		predicates.Comparison{
			Type: predicates.ComparisonEquals,
			Operand: &values.ConstantValue{
				Value: operand,
				Typ:   values.NotNullLong,
			},
		},
	)
}

func assertPredicateUnionOnlyAlias(
	t testing.TB,
	correlations map[values.CorrelationIdentifier]struct{},
	alias string,
) {
	t.Helper()
	want := values.NamedCorrelationIdentifier(alias)
	if len(correlations) != 1 {
		t.Fatalf("correlations = %v, want exactly {%s}", correlations, alias)
	}
	if _, ok := correlations[want]; !ok {
		t.Fatalf("correlations = %v, want exactly {%s}", correlations, alias)
	}
}

// makeSelectWithOrPredicates builds a SelectExpression with a single ForEach
// quantifier over a scan, carrying the given predicates.
func makeSelectWithOrPredicates(preds []predicates.QueryPredicate) (*expressions.SelectExpression, *expressions.Reference) {
	q := predicateUnionForEach("T", predicateUnionSourceAlias)
	sel := mustPredicateUnionConstruct(expressions.NewSelectExpression(
		mustPredicateUnionConstruct(q.RequireFlowedObjectValue()),
		[]expressions.Quantifier{q},
		preds,
	))
	ref := expressions.InitialOf(sel)
	return sel, ref
}

func TestPredicateToLogicalUnionRule_SingleOR(t *testing.T) {
	t.Parallel()

	// SELECT WHERE (A OR B) -> DISTINCT(UNION(UNIQUE(SELECT WHERE A), UNIQUE(SELECT WHERE B)))
	pA := predicateUnionEquals(predicateUnionSourceAlias, "a", 7)
	pB := predicateUnionEquals(predicateUnionSourceAlias, "a", 8)
	orPred := predicates.NewOr(pA, pB)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{orPred})
	yielded := mustExplorePredicateUnion(t, ref)

	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}

	// The output should be a primary-key dedup (LogicalUniqueExpression) — see
	// distinct_dedup_key_semantics_test.go for why the full-row node is the wrong
	// one here, despite Java spelling this position LogicalDistinctExpression.
	distinct, ok := yielded[0].(*expressions.LogicalUniqueExpression)
	if !ok {
		t.Fatalf("yielded type=%T, want *LogicalUniqueExpression (primary-key dedup)", yielded[0])
	}
	if got := distinct.GetResultValue().Type(); !got.Equals(predicateUnionRowType()) {
		t.Fatalf("simple rewrite result type = %v, want exact source row %v",
			got, predicateUnionRowType())
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

func TestPredicateToLogicalUnionRule_StrictSingleFailsClosed(t *testing.T) {
	t.Parallel()

	scanRef := expressions.InitialOf(predicateUnionScan("T"))
	alias := values.NamedCorrelationIdentifier("STRICT")
	q := expressions.NamedForEachStrictSingleQuantifier(alias, scanRef)
	sel := mustPredicateUnionConstruct(expressions.NewSelectExpression(
		mustPredicateUnionConstruct(q.RequireFlowedObjectValue()),
		[]expressions.Quantifier{q},
		[]predicates.QueryPredicate{predicates.NewOr(
			predicateUnionEquals("STRICT", "x", 1),
			predicateUnionEquals("STRICT", "y", 2),
		)},
	))

	yielded := mustExplorePredicateUnion(t, expressions.InitialOf(sel))
	if len(yielded) != 0 {
		t.Fatalf("strict-single OR select yielded %d logical-union rewrite(s), want zero", len(yielded))
	}
}

func TestPredicateToLogicalUnionRule_ORWithFixedPredicates(t *testing.T) {
	t.Parallel()

	// SELECT WHERE fixed AND (A OR B)
	// -> DISTINCT(UNION(UNIQUE(SELECT WHERE fixed AND A), UNIQUE(SELECT WHERE fixed AND B)))
	fixed := predicateUnionEquals(predicateUnionSourceAlias, "x", 1)
	pA := predicateUnionEquals(predicateUnionSourceAlias, "a", 7)
	pB := predicateUnionEquals(predicateUnionSourceAlias, "a", 8)
	orPred := predicates.NewOr(pA, pB)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{fixed, orPred})
	yielded := mustExplorePredicateUnion(t, ref)

	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}

	distinct, ok := yielded[0].(*expressions.LogicalUniqueExpression)
	if !ok {
		t.Fatalf("yielded type=%T, want *LogicalUniqueExpression (primary-key dedup)", yielded[0])
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
		residuals, err := predicates.ToResidualPredicates(sel.GetPredicates())
		want := predicates.NewAnd(fixed, []predicates.QueryPredicate{pA, pB}[i])
		if err != nil || !predicates.SemanticEqualsUnderAliasMap(predicates.NewAnd(residuals...), want, nil) {
			t.Fatalf("leg %d did not preserve both the fixed predicate and its OR term: %v", i, err)
		}
		for _, predicate := range sel.GetPredicates() {
			assertPredicateUnionOnlyAlias(t,
				predicates.GetCorrelatedToOfPredicate(predicate),
				predicateUnionSourceAlias)
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
	pA := predicateUnionEquals(predicateUnionSourceAlias, "a", 7)
	pB := predicateUnionEquals(predicateUnionSourceAlias, "a", 8)
	pC := predicateUnionEquals(predicateUnionSourceAlias, "x", 1)
	pD := predicateUnionEquals(predicateUnionSourceAlias, "y", 2)
	or1 := predicates.NewOr(pA, pB)
	or2 := predicates.NewOr(pC, pD)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{or1, or2})
	yielded := mustExplorePredicateUnion(t, ref)

	if len(yielded) != 3 {
		t.Fatalf("yielded=%d, want 3", len(yielded))
	}

	distinct := yielded[0].(*expressions.LogicalUniqueExpression)
	union := distinct.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)

	// 2 * 2 = 4 cross-product terms.
	if len(union.GetQuantifiers()) != 4 {
		t.Fatalf("union children=%d, want 4", len(union.GetQuantifiers()))
	}
}

func TestPredicateToLogicalUnionRule_DeclinesNoOR(t *testing.T) {
	t.Parallel()

	// SELECT WHERE A AND B (no ORs) -> rule declines.
	pA := predicateUnionEquals(predicateUnionSourceAlias, "a", 7)
	pB := predicateUnionEquals(predicateUnionSourceAlias, "a", 8)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{pA, pB})
	yielded := mustExplorePredicateUnion(t, ref)

	if len(yielded) != 0 {
		t.Fatalf("rule fired despite no OR predicates — yielded %d, want 0", len(yielded))
	}
}

func TestPredicateToLogicalUnionRule_DeclinesNoPredicates(t *testing.T) {
	t.Parallel()

	_, ref := makeSelectWithOrPredicates(nil)
	yielded := mustExplorePredicateUnion(t, ref)

	if len(yielded) != 0 {
		t.Fatalf("rule fired despite no predicates — yielded %d, want 0", len(yielded))
	}
}

func TestPredicateToLogicalUnionRule_SubsetsExistentialQuantifiers(t *testing.T) {
	t.Parallel()
	for _, fixed := range []bool{false, true} {
		t.Run(fmt.Sprint(fixed), func(t *testing.T) {
			t.Parallel()
			q := predicateUnionForEach("T", predicateUnionSourceAlias)
			ex := expressions.ExistentialQuantifier(expressions.InitialOf(predicateUnionScan("S")))
			unused := expressions.ExistentialQuantifier(expressions.InitialOf(predicateUnionScan("U")))
			ep := mustPredicateUnionConstruct(predicates.NewExistentialAlias(ex.GetAlias(), predicateUnionRowType()))
			ps := []predicates.QueryPredicate{predicates.NewOr(predicateUnionEquals(predicateUnionSourceAlias, "x", 1), ep)}
			if fixed {
				ps = []predicates.QueryPredicate{ep, predicates.NewOr(predicateUnionEquals(predicateUnionSourceAlias, "x", 1), predicateUnionEquals(predicateUnionSourceAlias, "y", 2))}
			}
			sel := mustPredicateUnionConstruct(expressions.NewSelectExpression(mustPredicateUnionConstruct(q.RequireFlowedObjectValue()), []expressions.Quantifier{q, ex, unused}, ps))
			yielded := mustExplorePredicateUnion(t, expressions.InitialOf(sel))
			if len(yielded) != 1 {
				t.Fatalf("yielded=%d, want union alternative", len(yielded))
			}
			unique := yielded[0].(*expressions.LogicalUniqueExpression)
			union := unique.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
			if len(union.GetQuantifiers()) != 2 {
				t.Fatal("expected two union legs")
			}
			existentialLegs := 0
			for _, legQ := range union.GetQuantifiers() {
				legUnique := legQ.GetRangesOver().Get().(*expressions.LogicalUniqueExpression)
				leg := legUnique.GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
				for _, lq := range leg.GetQuantifiers() {
					if lq.GetAlias() == unused.GetAlias() {
						t.Fatal("unused existential copied into leg")
					}
					if lq.Kind() == expressions.QuantifierExistential {
						existentialLegs++
						if lq.GetAlias() != ex.GetAlias() || lq.GetRangesOver() != ex.GetRangesOver() {
							t.Fatal("existential binding changed")
						}
					}
				}
			}
			want := 1
			if fixed {
				want = 2
			}
			if existentialLegs != want {
				t.Fatalf("existential legs=%d want %d", existentialLegs, want)
			}
		})
	}
}

func TestPredicateToLogicalUnionRule_DeclinesMultipleForEach(t *testing.T) {
	t.Parallel()

	// Two ForEach quantifiers (a join) — rule should decline.
	q1 := predicateUnionForEach("T1", "predicate_union_left")
	q2 := predicateUnionForEach("T2", "predicate_union_right")

	orPred := predicates.NewOr(
		predicateUnionEquals(q1.GetAlias().Name(), "x", 1),
		predicateUnionEquals(q1.GetAlias().Name(), "y", 2),
	)

	sel := mustPredicateUnionConstruct(expressions.NewSelectExpression(
		mustPredicateUnionConstruct(q1.RequireFlowedObjectValue()),
		[]expressions.Quantifier{q1, q2},
		[]predicates.QueryPredicate{orPred},
	))
	ref := expressions.InitialOf(sel)

	yielded := mustExplorePredicateUnion(t, ref)

	if len(yielded) != 0 {
		t.Fatalf("rule fired despite multiple ForEach quantifiers — yielded %d, want 0", len(yielded))
	}
}

func TestPredicateToLogicalUnionRule_NonSimpleResultValue(t *testing.T) {
	t.Parallel()

	// When the result value is NOT a simple QuantifiedObjectValue, the
	// rule wraps the result in an outer SelectExpression.
	q := predicateUnionForEach("T", predicateUnionSourceAlias)

	// Use a RecordConstructorValue as a non-simple result value.
	resultValue := values.NewRecordConstructorValue(values.RecordConstructorField{
		Name: "a", Value: predicateUnionField(predicateUnionSourceAlias, "a"),
	})

	orPred := predicates.NewOr(
		predicateUnionEquals(predicateUnionSourceAlias, "x", 1),
		predicateUnionEquals(predicateUnionSourceAlias, "y", 2),
	)

	sel := mustPredicateUnionConstruct(expressions.NewSelectExpression(
		resultValue,
		[]expressions.Quantifier{q},
		[]predicates.QueryPredicate{orPred},
	))
	ref := expressions.InitialOf(sel)

	yielded := mustExplorePredicateUnion(t, ref)

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
	if outerSel.GetResultValue() != resultValue {
		t.Fatal("outer select did not retain the original non-simple projection value")
	}
	assertPredicateUnionOnlyAlias(t,
		values.GetCorrelatedToOfValue(outerSel.GetResultValue()),
		predicateUnionSourceAlias)
	if got := outerSel.GetQuantifiers()[0].GetAlias(); got != values.NamedCorrelationIdentifier(predicateUnionSourceAlias) {
		t.Fatalf("outer quantifier alias = %s, want retained source alias %s",
			got.Name(), predicateUnionSourceAlias)
	}

	// Inner should be Distinct.
	innerRef := outerSel.GetQuantifiers()[0].GetRangesOver()
	_, ok = innerRef.Get().(*expressions.LogicalUniqueExpression)
	if !ok {
		t.Fatalf("outer select's inner type=%T, want *LogicalUniqueExpression (primary-key dedup)", innerRef.Get())
	}
}

func TestPredicateToLogicalUnionRule_ThreeWayOR(t *testing.T) {
	t.Parallel()

	// SELECT WHERE (A OR B OR C) -> DISTINCT(UNION(3 legs))
	pA := predicateUnionEquals(predicateUnionSourceAlias, "a", 7)
	pB := predicateUnionEquals(predicateUnionSourceAlias, "a", 8)
	pC := predicateUnionEquals(predicateUnionSourceAlias, "x", 1)
	orPred := predicates.NewOr(pA, pB, pC)

	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{orPred})
	yielded := mustExplorePredicateUnion(t, ref)

	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}

	distinct := yielded[0].(*expressions.LogicalUniqueExpression)
	union := distinct.GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)

	if len(union.GetQuantifiers()) != 3 {
		t.Fatalf("union children=%d, want 3", len(union.GetQuantifiers()))
	}
}

func TestPredicateUnionDeclinesSimplifiedConstantOrSingleton(t *testing.T) {
	t.Parallel()
	p := predicateUnionEquals(predicateUnionSourceAlias, "x", 1)
	for _, pred := range []predicates.QueryPredicate{
		predicates.NewOr(predicates.NewConstantPredicate(predicates.TriTrue), p),
		predicates.NewOr(predicates.NewConstantPredicate(predicates.TriFalse), p),
		predicates.NewOr(predicates.NewConstantPredicate(predicates.TriFalse), predicates.NewConstantPredicate(predicates.TriFalse)),
	} {
		_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{pred})
		if got := mustExplorePredicateUnion(t, ref); len(got) != 0 {
			t.Fatalf("simplified non-disjunction generated union alternatives: %s", pred.Explain())
		}
	}
}

func TestUnionDNFSimplifiesDistributedTerms(t *testing.T) {
	t.Parallel()
	a := predicateUnionEquals(predicateUnionSourceAlias, "x", 1)
	b := predicateUnionEquals(predicateUnionSourceAlias, "y", 2)
	c := predicateUnionEquals(predicateUnionSourceAlias, "y", 3)
	terms := mustPredicateUnionConstruct(predicateUnionDNFTerms([]predicates.QueryPredicate{
		predicates.NewOr(predicates.NewNot(a), predicates.NewConstantPredicate(predicates.TriFalse)),
		predicates.NewOr(b, c),
	}))
	if len(terms) != 2 {
		t.Fatalf("DNF retained annihilated branches: got %d terms, want 2", len(terms))
	}
	for _, term := range terms {
		for _, conjunct := range andConjuncts(term) {
			if _, not := conjunct.(*predicates.NotPredicate); not {
				t.Fatal("DNF retained NOT over a comparison instead of its inverse")
			}
		}
	}
}

func TestOrsToDNFTerms_TwoByTwo(t *testing.T) {
	t.Parallel()

	pA := predicateUnionEquals(predicateUnionSourceAlias, "a", 7)
	pB := predicateUnionEquals(predicateUnionSourceAlias, "a", 8)
	pC := predicateUnionEquals(predicateUnionSourceAlias, "x", 1)
	pD := predicateUnionEquals(predicateUnionSourceAlias, "y", 2)

	or1 := predicates.NewOr(pA, pB)
	or2 := predicates.NewOr(pC, pD)

	terms := mustPredicateUnionConstruct(predicateUnionDNFTerms([]predicates.QueryPredicate{or1, or2}))

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

	pA := predicateUnionEquals(predicateUnionSourceAlias, "a", 7)
	pB := predicateUnionEquals(predicateUnionSourceAlias, "a", 8)
	pC := predicateUnionEquals(predicateUnionSourceAlias, "x", 1)

	or1 := predicates.NewOr(pA, pB)
	or2 := predicates.NewOr(pC)

	terms := mustPredicateUnionConstruct(predicateUnionDNFTerms([]predicates.QueryPredicate{or1, or2}))

	// 2 * 1 = 2 terms.
	if len(terms) != 2 {
		t.Fatalf("DNF terms=%d, want 2", len(terms))
	}
}

func TestUnionDNFAbsorbsDistributedCrossProducts(t *testing.T) {
	t.Parallel()
	a := predicateUnionEquals(predicateUnionSourceAlias, "x", 1)
	b := predicateUnionEquals(predicateUnionSourceAlias, "y", 2)
	c := predicateUnionEquals(predicateUnionSourceAlias, "x", 3)
	d := predicateUnionEquals(predicateUnionSourceAlias, "y", 4)
	ors := []predicates.QueryPredicate{
		predicates.NewOr(a, c), predicates.NewOr(a, d), predicates.NewOr(b, c), predicates.NewOr(b, d),
	}
	terms := mustPredicateUnionConstruct(predicateUnionDNFTerms(ors))
	if len(terms) != 2 {
		t.Fatalf("CNF of (a AND b) OR (c AND d) expanded into %d legs, want 2 after absorption", len(terms))
	}
}

func TestPredicateToLogicalUnionRule_PreservesProjectedExistentialScope(t *testing.T) {
	t.Parallel()
	q := predicateUnionForEach("T", predicateUnionSourceAlias)
	ex := expressions.ExistentialQuantifier(expressions.InitialOf(predicateUnionScan("S")))
	rv := mustPredicateUnionConstruct(ex.RequireFlowedObjectValue())
	sel := mustPredicateUnionConstruct(expressions.NewSelectExpression(rv, []expressions.Quantifier{q, ex}, []predicates.QueryPredicate{predicates.NewOr(predicateUnionEquals(predicateUnionSourceAlias, "x", 1), predicateUnionEquals(predicateUnionSourceAlias, "y", 2))}))
	if out := mustExplorePredicateUnion(t, expressions.InitialOf(sel)); len(out) != 0 {
		t.Fatal("union moved the projected existential outside its owning scope")
	}
}

func TestPredicateUnionFixedFactorsStayAtomic(t *testing.T) {
	t.Parallel()
	first := predicates.NewOr(predicateUnionEquals(predicateUnionSourceAlias, "x", 1), predicateUnionEquals(predicateUnionSourceAlias, "x", 2))
	second := predicates.NewOr(predicateUnionEquals(predicateUnionSourceAlias, "y", 3), predicateUnionEquals(predicateUnionSourceAlias, "y", 4))
	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{first, second})
	yielded := mustExplorePredicateUnion(t, ref)
	if len(yielded) != 3 {
		t.Fatalf("alternatives = %d, want full DNF plus either fixed factor", len(yielded))
	}
	counts := map[int]int{}
	for _, expression := range yielded {
		union := expression.(*expressions.LogicalUniqueExpression).GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
		counts[len(union.GetQuantifiers())]++
		for _, q := range union.GetQuantifiers() {
			leg := q.GetRangesOver().Get().(*expressions.LogicalUniqueExpression).GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
			for _, predicate := range leg.GetPredicates() {
				if _, isOr := predicate.(*predicates.OrPredicate); isOr && !predicates.IsAtomic(predicate) {
					t.Fatal("fixed factor remains expandable")
				}
			}
			if result := mustExplorePredicateUnion(t, expressions.InitialOf(leg)); len(result) != 0 {
				t.Fatal("fixed factor recursively expanded")
			}
		}
	}
	if counts[2] != 2 || counts[4] != 1 {
		t.Fatalf("union leg counts = %v", counts)
	}
}

func TestPredicateUnionRequiresOrTermMatch(t *testing.T) {
	t.Parallel()
	disjunction := predicates.NewOr(predicateUnionEquals(predicateUnionSourceAlias, "x", 1), predicateUnionEquals(predicateUnionSourceAlias, "y", 2))
	_, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{disjunction})
	yielded, err := FireExpressionRule(NewPredicateToLogicalUnionRule(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(yielded) != 0 {
		t.Fatal("union expanded before any candidate matched an OR term")
	}
}

func TestPredicateUnionKeepsUnmatchedOrFixed(t *testing.T) {
	t.Parallel()
	first := predicates.NewOr(predicateUnionEquals(predicateUnionSourceAlias, "x", 1), predicateUnionEquals(predicateUnionSourceAlias, "x", 2))
	second := predicates.NewOr(predicateUnionEquals(predicateUnionSourceAlias, "y", 3), predicateUnionEquals(predicateUnionSourceAlias, "y", 4))
	sel, ref := makeSelectWithOrPredicates([]predicates.QueryPredicate{first, second})
	call := NewExpressionRuleCall(ref, nil, nil)
	explorePredicateUnion(call, sel, map[predicates.QueryPredicate]struct{}{first: {}})
	if err := call.Err(); err != nil {
		t.Fatal(err)
	}
	if len(call.Yielded()) != 1 {
		t.Fatalf("want exactly the alternative fixing unmatched y OR, got %d", len(call.Yielded()))
	}
	union := call.Yielded()[0].(*expressions.LogicalUniqueExpression).GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
	if len(union.GetQuantifiers()) != 2 {
		t.Fatal("unmatched OR was expanded")
	}
	for _, q := range union.GetQuantifiers() {
		leg := q.GetRangesOver().Get().(*expressions.LogicalUniqueExpression).GetInner().GetRangesOver().Get().(*expressions.SelectExpression)
		fixed := leg.GetPredicates()[0]
		if !predicates.IsAtomic(fixed) || !predicates.PredicateEquals(predicates.WithAtomicity(fixed, false), second) {
			t.Fatal("wrong fixed residual")
		}
	}
}

func TestPredicateUnionAboveSubsetLimitStillExpandsDNF(t *testing.T) {
	t.Parallel()
	a := predicateUnionEquals(predicateUnionSourceAlias, "x", 1)
	b := predicateUnionEquals(predicateUnionSourceAlias, "y", 2)
	var factors []predicates.QueryPredicate
	for range DefaultMaxNumConjuncts + 1 {
		factors = append(factors, predicates.NewOr(a, b))
	}
	_, ref := makeSelectWithOrPredicates(factors)
	result := mustExplorePredicateUnion(t, ref)
	if len(result) != 1 {
		t.Fatalf("above subset limit: alternatives = %d, want full DNF only", len(result))
	}
	union := result[0].(*expressions.LogicalUniqueExpression).GetInner().GetRangesOver().Get().(*expressions.LogicalUnionExpression)
	if len(union.GetQuantifiers()) != 2 {
		t.Fatal("full DNF did not absorb repeated terms")
	}
}
