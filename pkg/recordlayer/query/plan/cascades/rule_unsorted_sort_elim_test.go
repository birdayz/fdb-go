package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestUnsortedSortElimRule_FiresOnUnsortedSort(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	s := expressions.UnsortedLogicalSortExpression(scanQ)
	ref := expressions.InitialOf(s)
	rule := NewUnsortedSortElimRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	if _, ok := yielded[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("yielded type=%T, want *FullUnorderedScanExpression", yielded[0])
	}
}

func TestUnsortedSortElimRule_DeclinesOnSortedSort(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	keys := []expressions.SortKey{{Value: values.NewBooleanValue(true), Reverse: false}}
	s := expressions.NewLogicalSortExpression(keys, scanQ)
	ref := expressions.InitialOf(s)
	rule := NewUnsortedSortElimRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on a sort with keys — yielded %d, want 0", len(yielded))
	}
}

func TestUnsortedSortElimRule_DeclinesOnNonSort(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	ref := expressions.InitialOf(scan)
	rule := NewUnsortedSortElimRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on Scan (no Sort) — yielded %d, want 0", len(yielded))
	}
}
