package expressions

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestLogicalFilter_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	pFalse := predicates.NewConstantPredicate(predicates.TriFalse)
	preds := []predicates.QueryPredicate{pTrue, pFalse}
	f := NewLogicalFilterExpression(preds, q)
	if got := f.GetPredicates(); len(got) != 2 {
		t.Fatalf("predicates size=%d, want 2", len(got))
	}
	if f.GetInner().GetAlias() != q.GetAlias() {
		t.Fatal("Inner Quantifier alias mismatch after construction")
	}
	if got := f.GetQuantifiers(); len(got) != 1 || got[0].GetAlias() != q.GetAlias() {
		t.Fatalf("GetQuantifiers wrong: %v", got)
	}
	if f.CanCorrelate() {
		t.Fatal("LogicalFilter should not anchor a correlation")
	}
}

func TestLogicalFilter_Construction_DefensiveCopy(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	preds := []predicates.QueryPredicate{
		predicates.NewConstantPredicate(predicates.TriTrue),
	}
	f := NewLogicalFilterExpression(preds, q)
	preds[0] = predicates.NewConstantPredicate(predicates.TriFalse)
	if f.GetPredicates()[0].(*predicates.ConstantPredicate).Value != predicates.TriTrue {
		t.Fatal("constructor failed to defensively copy predicate slice")
	}
}

func TestLogicalFilter_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	f1 := NewLogicalFilterExpression([]predicates.QueryPredicate{pTrue}, q1)
	f2 := NewLogicalFilterExpression([]predicates.QueryPredicate{pTrue}, q2)
	if !f1.EqualsWithoutChildren(f2, EmptyAliasMap()) {
		t.Fatal("structurally equal filters reported unequal-without-children")
	}
}

func TestLogicalFilter_EqualsWithoutChildren_DifferentLen(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	pFalse := predicates.NewConstantPredicate(predicates.TriFalse)
	q := ForEachQuantifier(InitialOf(leaf))
	f1 := NewLogicalFilterExpression([]predicates.QueryPredicate{pTrue}, q)
	f2 := NewLogicalFilterExpression([]predicates.QueryPredicate{pTrue, pFalse}, q)
	if f1.EqualsWithoutChildren(f2, EmptyAliasMap()) {
		t.Fatal("filters with different predicate counts reported equal-without-children")
	}
}

func TestLogicalFilter_EqualsWithoutChildren_DifferentExpressionType(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	q := ForEachQuantifier(InitialOf(leaf))
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pTrue}, q)
	if f.EqualsWithoutChildren(leaf, EmptyAliasMap()) {
		t.Fatal("filter reported equal to non-filter expression")
	}
}

func TestLogicalFilter_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	f1 := NewLogicalFilterExpression([]predicates.QueryPredicate{pTrue}, q1)
	f2 := NewLogicalFilterExpression([]predicates.QueryPredicate{pTrue}, q2)
	if f1.HashCodeWithoutChildren() != f2.HashCodeWithoutChildren() {
		t.Fatal("structurally equal filters produced different hash codes")
	}
}

func TestLogicalFilter_HashCodeDifferentForDifferentPredicates(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	pFalse := predicates.NewConstantPredicate(predicates.TriFalse)
	q := ForEachQuantifier(InitialOf(leaf))
	f1 := NewLogicalFilterExpression([]predicates.QueryPredicate{pTrue}, q)
	f2 := NewLogicalFilterExpression([]predicates.QueryPredicate{pFalse}, q)
	if f1.HashCodeWithoutChildren() == f2.HashCodeWithoutChildren() {
		t.Fatal("filters with different predicates produced identical hash codes (collision is unlikely)")
	}
}
