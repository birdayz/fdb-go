package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestOuterJoinMaterializationRule_ReformsTheOuterJoin(t *testing.T) {
	t.Parallel()
	original := buildCorrelatedLeftOuterSelect()
	canonical := deriveCanonicalRewrite(t, original)

	yielded := mustFireExpressionRule(t, NewOuterJoinMaterializationRule(), expressions.InitialOf(canonical))
	if len(yielded) != 1 {
		t.Fatalf("yielded %d expressions, want the LEFT OUTER select", len(yielded))
	}
	outer, ok := yielded[0].(*expressions.SelectExpression)
	if !ok || outer.GetJoinType() != expressions.JoinLeftOuter {
		t.Fatalf("yielded %T, want a LEFT OUTER SelectExpression", yielded[0])
	}
	qs := outer.GetQuantifiers()
	if len(qs) != 2 || qs[0].GetAlias() != original.GetQuantifiers()[0].GetAlias() ||
		qs[1].GetAlias() != original.GetQuantifiers()[1].GetAlias() || qs[1].IsNullOnEmpty() {
		t.Fatalf("legs = %v, want the preserved and the plain null-supplying leg", qs)
	}
	if len(outer.GetPredicates()) != 1 ||
		!predicates.StructurallyEqual(outer.GetPredicates()[0], original.GetPredicates()[0]) {
		t.Fatalf("ON predicates = %v, want the original ON", outer.GetPredicates())
	}
	if !values.ValuesStructurallyEqual(outer.GetResultValue(), canonical.GetResultValue()) {
		t.Fatal("the LEFT OUTER select must flow the canonical select's result")
	}
}

// A WHERE conjunct stays above the LEFT OUTER select, over a box flowing both
// legs; inside it would be an ON condition and keep null-extended rows it
// must drop.
func TestOuterJoinMaterializationRule_KeepsWhereAboveTheOuterJoin(t *testing.T) {
	t.Parallel()
	canonical := deriveCanonicalRewrite(t, buildCorrelatedLeftOuterSelect())
	nullSupplied := canonical.GetQuantifiers()[1]
	row, err := nullSupplied.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	flag, err := values.ResolveFieldOrdinals(row, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	where := predicates.NewComparisonPredicate(flag, predicates.Comparison{Type: predicates.ComparisonIsNull})
	// The fixture's result types the null-supplied leg NOT NULL; flow the
	// preserved row only, so the re-anchored result keeps its type.
	preservedRow, err := canonical.GetQuantifiers()[0].RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := expressions.NewSelectExpressionWithJoinType(
		preservedRow, canonical.GetQuantifiers(), []predicates.QueryPredicate{where},
		canonical.GetSourceAliases(), expressions.JoinInner)
	if err != nil {
		t.Fatal(err)
	}

	ref := expressions.InitialOf(sel)
	yielded := mustFireExpressionRule(t, NewOuterJoinMaterializationRule(), ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded %d expressions, want one select over the outer join", len(yielded))
	}
	ref.Insert(yielded[0])
	if again := mustFireExpressionRule(t, NewOuterJoinMaterializationRule(), ref); len(again) != 0 {
		t.Fatalf("a second firing yielded %d more members under a new box alias", len(again))
	}
	above, ok := yielded[0].(*expressions.SelectExpression)
	if !ok || above.GetJoinType() != expressions.JoinInner || len(above.GetQuantifiers()) != 1 ||
		len(above.GetPredicates()) != 1 {
		t.Fatalf("yielded %v, want an INNER select with the WHERE over one box quantifier", yielded[0])
	}
	if _, correlated := predicates.GetCorrelatedToOfPredicate(above.GetPredicates()[0])[above.GetQuantifiers()[0].GetAlias()]; !correlated {
		t.Fatal("the WHERE conjunct must read the box, not the legs it replaced")
	}
	var outer *expressions.SelectExpression
	for _, m := range above.GetQuantifiers()[0].GetRangesOver().Members() {
		if s, isSel := m.(*expressions.SelectExpression); isSel && s.GetJoinType() == expressions.JoinLeftOuter {
			outer = s
		}
	}
	if outer == nil || len(outer.GetPredicates()) != 1 {
		t.Fatal("the box must range over the LEFT OUTER select carrying only the ON predicate")
	}
}

func TestOuterJoinMaterializationRule_DeclinesWithoutTheCanonicalInner(t *testing.T) {
	t.Parallel()
	canonical := deriveCanonicalRewrite(t, buildCorrelatedLeftOuterSelect())
	qs := canonical.GetQuantifiers()
	// A null-on-empty edge over a plain scan is not RewriteOuterJoinRule's
	// inner select, so there is no ON to recover.
	bare := expressions.NamedForEachNullOnEmptyQuantifier(qs[1].GetAlias(),
		expressions.InitialOf(mustOuterJoinCostConstruct(expressions.NewFullUnorderedScanExpression(
			[]string{"B"}, outerJoinCostRowType()))))
	sel, err := expressions.NewSelectExpressionWithJoinType(
		canonical.GetResultValue(), []expressions.Quantifier{qs[0], bare}, nil,
		canonical.GetSourceAliases(), expressions.JoinInner)
	if err != nil {
		t.Fatal(err)
	}
	if yielded := mustFireExpressionRule(t, NewOuterJoinMaterializationRule(), expressions.InitialOf(sel)); len(yielded) != 0 {
		t.Fatalf("yielded %v over a null-on-empty scan", yielded)
	}
}

// A block over RewriteOuterJoinRule's canonical form merges with it, as Java's
// SelectMergeRule merges any child Select over a plain ForEach edge; the
// null-on-empty edge travels into the merged select.
func TestSelectMergeRule_DissolvesTheCanonicalOuterJoin(t *testing.T) {
	t.Parallel()
	canonical := deriveCanonicalRewrite(t, buildCorrelatedLeftOuterSelect())
	child := expressions.NamedForEachQuantifier(values.UniqueCorrelationIdentifier(), expressions.InitialOf(canonical))
	row, err := child.RequireFlowedObjectValue()
	if err != nil {
		t.Fatal(err)
	}
	block, err := expressions.NewSelectExpression(row, []expressions.Quantifier{child}, nil)
	if err != nil {
		t.Fatal(err)
	}

	yielded := mustFirePrunedFinalRule(t, NewSelectMergeRule(), expressions.InitialOf(block))
	if len(yielded) != 1 {
		t.Fatalf("SelectMergeRule yielded %d expressions over the canonical outer join, want the merged block", len(yielded))
	}
	merged, ok := yielded[0].(*expressions.SelectExpression)
	if !ok || len(merged.GetQuantifiers()) != 2 || !merged.GetQuantifiers()[1].IsNullOnEmpty() {
		t.Fatalf("merged = %v, want the preserved leg and the null-on-empty edge", yielded[0])
	}
}
