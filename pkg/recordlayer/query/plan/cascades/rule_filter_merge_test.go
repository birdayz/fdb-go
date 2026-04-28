package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// buildNestedFilter constructs:
//
//	Filter([pOuter])
//	  → Filter([pInner])
//	    → Scan(Order)
func buildNestedFilter(pOuter, pInner predicates.QueryPredicate) *expressions.LogicalFilterExpression {
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	innerFilter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pInner}, scanQ)
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(innerFilter))
	return expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pOuter}, innerQ)
}

func TestFilterMergeRule_FiresOnNestedFilter(t *testing.T) {
	t.Parallel()
	pOuter := predicates.NewConstantPredicate(predicates.TriTrue)
	pInner := predicates.NewConstantPredicate(predicates.TriFalse)
	outer := buildNestedFilter(pOuter, pInner)
	ref := expressions.InitialOf(outer)

	rule := NewFilterMergeRule()
	yielded := FireExpressionRule(rule, ref)
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
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	filter := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, scanQ)
	ref := expressions.InitialOf(filter)

	rule := NewFilterMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("FilterMergeRule fired on a single Filter (no nested inner) — yielded %d, want 0", len(yielded))
	}
}

func TestFilterMergeRule_DeclinesOnNonFilter(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	ref := expressions.InitialOf(scan)
	rule := NewFilterMergeRule()
	yielded := FireExpressionRule(rule, ref)
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
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	p3 := predicates.NewConstantPredicate(predicates.TriTrue)
	f3 := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p3}, scanQ)
	f3Q := expressions.ForEachQuantifier(expressions.InitialOf(f3))
	p2 := predicates.NewConstantPredicate(predicates.TriFalse)
	f2 := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p2}, f3Q)
	f2Q := expressions.ForEachQuantifier(expressions.InitialOf(f2))
	p1 := predicates.NewConstantPredicate(predicates.TriTrue)
	f1 := expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{p1}, f2Q)
	ref := expressions.InitialOf(f1)

	rule := NewFilterMergeRule()
	yielded := FireExpressionRule(rule, ref)
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
