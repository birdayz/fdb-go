package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// buildNestedFilter constructs:
//
//	Filter([pOuter])
//	  → Filter([pInner])
//	    → Scan(Order)
func buildNestedFilter(pOuter, pInner predicates.QueryPredicate) *expressions.LogicalFilterExpression {
	scan := filterRuleScan("Order")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerFilter := filterRuleFilter([]predicates.QueryPredicate{pInner}, scanQ)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerFilter))
	return filterRuleFilter([]predicates.QueryPredicate{pOuter}, innerQ)
}

func requireFilterMergeOnlyCorrelation(
	t testing.TB,
	predicate predicates.QueryPredicate,
	want values.CorrelationIdentifier,
) {
	t.Helper()
	correlated := predicates.GetCorrelatedToOfPredicate(predicate)
	if len(correlated) != 1 {
		t.Fatalf("predicate correlations = %v, want only %q", correlated, want.Name())
	}
	if _, ok := correlated[want]; !ok {
		t.Fatalf("predicate correlations = %v, want only %q", correlated, want.Name())
	}
}

func TestFilterMergeRule_TranslatesOnlyExactOuterEdge(t *testing.T) {
	t.Parallel()

	replacementAlias := values.NamedCorrelationIdentifier("T")
	scanQ := expressions.NamedForEachQuantifier(
		replacementAlias,
		expressions.InitialOf(filterRuleScan("T")),
	)
	innerPredicate := qFieldPred(t, scanQ, "x", predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	innerFilter := filterRuleFilter([]predicates.QueryPredicate{innerPredicate}, scanQ)

	outerAlias := values.NamedCorrelationIdentifier("D")
	outerQ := expressions.NamedForEachQuantifier(outerAlias, expressions.InitialOf(innerFilter))
	ownedPredicate := qFieldPred(t, outerQ, "a", predicates.Comparison{Type: predicates.ComparisonIsNotNull})

	foreignAlias := values.NamedCorrelationIdentifier("FOREIGN")
	foreignQ := expressions.NamedForEachQuantifier(
		foreignAlias,
		expressions.InitialOf(filterRuleScan("FOREIGN")),
	)
	foreignPredicate := qFieldPred(t, foreignQ, "b", predicates.Comparison{Type: predicates.ComparisonIsNotNull})

	// A retained source window may conventionally reuse the removed edge's
	// alias. Its different exact record identity is authoritative even though
	// width, field names, leaf types, and nullability otherwise coincide.
	retainedType := values.NewRecordType("RetainedFilterWindow", false, []values.Field{
		{Name: "a", FieldType: values.NullableLong},
		{Name: "b", FieldType: values.NullableLong},
		{Name: "c", FieldType: values.NullableLong},
		{Name: "x", FieldType: values.NullableLong},
	})
	retainedScan := mustFilterConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"RETAINED"}, retainedType))
	retainedQ := expressions.NamedForEachQuantifier(
		outerAlias,
		expressions.InitialOf(retainedScan),
	)
	retainedPredicate := qFieldPred(t, retainedQ, "c", predicates.Comparison{Type: predicates.ComparisonIsNotNull})

	outerFilter := filterRuleFilter(
		[]predicates.QueryPredicate{ownedPredicate, foreignPredicate, retainedPredicate},
		outerQ,
	)
	yielded := fireFilterRule(t, NewFilterMergeRule(), expressions.InitialOf(outerFilter))
	if len(yielded) != 1 {
		t.Fatalf("FilterMergeRule yielded %d expressions, want 1", len(yielded))
	}
	merged, ok := yielded[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("yielded type = %T, want *LogicalFilterExpression", yielded[0])
	}
	mergedPredicates := merged.GetPredicates()
	if len(mergedPredicates) != 4 {
		t.Fatalf("merged predicate count = %d, want 4", len(mergedPredicates))
	}

	if mergedPredicates[0] == ownedPredicate {
		t.Fatal("owned outer predicate was not rebuilt onto the replacement edge")
	}
	requireFilterMergeOnlyCorrelation(t, mergedPredicates[0], replacementAlias)
	if mergedPredicates[1] != foreignPredicate {
		t.Fatal("foreign predicate was rebuilt")
	}
	requireFilterMergeOnlyCorrelation(t, mergedPredicates[1], foreignAlias)
	if mergedPredicates[2] != retainedPredicate {
		t.Fatal("same-alias predicate with a different exact type was rebuilt")
	}
	requireFilterMergeOnlyCorrelation(t, mergedPredicates[2], outerAlias)
	if mergedPredicates[3] != innerPredicate {
		t.Fatal("inner predicate was rebuilt")
	}
	requireFilterMergeOnlyCorrelation(t, mergedPredicates[3], replacementAlias)

	// The rule is copy-on-write: its source graph still declares the outer
	// predicate on D, and both guarded predicates remain pointer-identical.
	requireFilterMergeOnlyCorrelation(t, ownedPredicate, outerAlias)
	requireFilterMergeOnlyCorrelation(t, foreignPredicate, foreignAlias)
	requireFilterMergeOnlyCorrelation(t, retainedPredicate, outerAlias)
	if outerFilter.GetPredicates()[0] != ownedPredicate ||
		outerFilter.GetPredicates()[1] != foreignPredicate ||
		outerFilter.GetPredicates()[2] != retainedPredicate {
		t.Fatal("source outer filter predicates were mutated")
	}
}

func TestFilterMergeRule_FiresOnNestedFilter(t *testing.T) {
	t.Parallel()
	pOuter := predicates.NewConstantPredicate(predicates.TriTrue)
	pInner := predicates.NewConstantPredicate(predicates.TriFalse)
	outer := buildNestedFilter(pOuter, pInner)
	ref := expressions.InitialOf(outer)

	rule := NewFilterMergeRule()
	yielded := fireFilterRule(t, rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("FilterMergeRule yielded %d expressions, want 1", len(yielded))
	}

	merged, ok := yielded[0].(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("yielded type=%T, want *LogicalFilterExpression", yielded[0])
	}
	if got := merged.GetPredicates(); len(got) != 2 {
		t.Fatalf("merged predicate count=%d, want 2 (one outer + one inner)", len(got))
	}

	// Outer first — preserves SQL textual ordering (the outer filter
	// reads first in the source query, applies first to the row stream).
	if merged.GetPredicates()[0] != pOuter {
		t.Fatal("merged[0] is not the outer predicate")
	}
	if merged.GetPredicates()[1] != pInner {
		t.Fatal("merged[1] is not the inner predicate")
	}

	// New filter's inner Quantifier ranges over the original Scan,
	// not the redundant intermediate filter.
	newInner := merged.GetInner().GetRangesOver().Get()
	if _, ok := newInner.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("merged inner=%T, want *FullUnorderedScanExpression — rule didn't strip the redundant filter", newInner)
	}
}

func TestFilterMergeRule_DeclinesOnSingleFilter(t *testing.T) {
	t.Parallel()
	scan := filterRuleScan("Order")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := filterRuleFilter([]predicates.QueryPredicate{pred}, scanQ)
	ref := expressions.InitialOf(filter)

	rule := NewFilterMergeRule()
	yielded := fireFilterRule(t, rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("FilterMergeRule fired on a single Filter (no nested inner) — yielded %d, want 0", len(yielded))
	}
}

func TestFilterMergeRule_DeclinesOnNonFilter(t *testing.T) {
	t.Parallel()
	scan := filterRuleScan("Order")
	ref := expressions.InitialOf(scan)
	rule := NewFilterMergeRule()
	yielded := fireFilterRule(t, rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("FilterMergeRule fired on a Scan (no Filter at all) — yielded %d, want 0", len(yielded))
	}
}

func TestFilterMergeRule_PredicateOrderPreserved(t *testing.T) {
	t.Parallel()
	// Build a triple-nest: Filter(p1) → Filter(p2) → Filter(p3) → Scan
	// FilterMergeRule fires once at a time (operates on the OUTER level).
	// First fire merges (p1, p2) → Filter(p1, p2) → Filter(p3) → Scan.
	// (Subsequent fires would continue, but we test one fire here.)
	scan := filterRuleScan("Order")
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	p3 := predicates.NewConstantPredicate(predicates.TriTrue)
	f3 := filterRuleFilter([]predicates.QueryPredicate{p3}, scanQ)
	f3Q := expressions.ForEachQuantifier(expressions.InitialOf(f3))
	p2 := predicates.NewConstantPredicate(predicates.TriFalse)
	f2 := filterRuleFilter([]predicates.QueryPredicate{p2}, f3Q)
	f2Q := expressions.ForEachQuantifier(expressions.InitialOf(f2))
	p1 := predicates.NewConstantPredicate(predicates.TriTrue)
	f1 := filterRuleFilter([]predicates.QueryPredicate{p1}, f2Q)
	ref := expressions.InitialOf(f1)

	rule := NewFilterMergeRule()
	yielded := fireFilterRule(t, rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	merged := yielded[0].(*expressions.LogicalFilterExpression)
	if got := merged.GetPredicates(); len(got) != 2 {
		t.Fatalf("merged size=%d, want 2 — one fire merges only the outer pair", len(got))
	}
	if merged.GetPredicates()[0] != p1 || merged.GetPredicates()[1] != p2 {
		t.Fatal("merged predicates not in (p1, p2) order")
	}
	// Inner of merged is f3 (the third filter), still wrapped.
	innerExpr := merged.GetInner().GetRangesOver().Get()
	if innerExpr != f3 {
		t.Fatal("merged inner is not the original f3 filter")
	}
}
