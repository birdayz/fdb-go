package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestSelect_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	rv := values.NewBooleanValue(true)
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	s := NewSelectExpression(rv, []Quantifier{q1, q2}, []predicates.QueryPredicate{pTrue})
	if s.GetResultValue() != rv {
		t.Fatal("result value mismatch")
	}
	if len(s.GetQuantifiers()) != 2 {
		t.Fatal("quantifier count wrong")
	}
	if !s.HasPredicates() {
		t.Fatal("HasPredicates false on non-empty predicate list")
	}
	if !s.CanCorrelate() {
		t.Fatal("Select MUST anchor a correlation — distinguishing property")
	}
}

func TestSelect_NoPredicates(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	rv := values.NewBooleanValue(true)
	s := NewSelectExpression(rv, []Quantifier{q}, nil)
	if s.HasPredicates() {
		t.Fatal("HasPredicates true on empty predicate list")
	}
}

func TestSelect_DefensiveCopies(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	srcQs := []Quantifier{q}
	srcPs := []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)}
	rv := values.NewBooleanValue(true)
	s := NewSelectExpression(rv, srcQs, srcPs)
	srcQs[0] = ForEachQuantifier(InitialOf(leaf))
	srcPs[0] = predicates.NewConstantPredicate(predicates.TriFalse)
	if s.GetQuantifiers()[0].GetAlias() != q.GetAlias() {
		t.Fatal("quantifier list not defensively copied")
	}
	if s.GetPredicates()[0].(*predicates.ConstantPredicate).Value != predicates.TriTrue {
		t.Fatal("predicate list not defensively copied")
	}
}

func TestSelect_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	rv := values.NewBooleanValue(true)
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	s1 := NewSelectExpression(rv, []Quantifier{q1}, []predicates.QueryPredicate{pTrue})
	s2 := NewSelectExpression(values.NewBooleanValue(true), []Quantifier{q2}, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)})
	if !s1.EqualsWithoutChildren(s2, EmptyAliasMap()) {
		t.Fatal("structurally identical Selects reported unequal-without-children")
	}
}

func TestSelect_EqualsWithoutChildren_DifferentResult(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	s1 := NewSelectExpression(values.NewBooleanValue(true), []Quantifier{q}, []predicates.QueryPredicate{pTrue})
	s2 := NewSelectExpression(values.NewBooleanValue(false), []Quantifier{q}, []predicates.QueryPredicate{pTrue})
	if s1.EqualsWithoutChildren(s2, EmptyAliasMap()) {
		t.Fatal("Selects with different result values reported equal")
	}
}

func TestSelect_EqualsWithoutChildren_DifferentPredicates(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	rv := values.NewBooleanValue(true)
	s1 := NewSelectExpression(rv, []Quantifier{q}, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)})
	s2 := NewSelectExpression(rv, []Quantifier{q}, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriFalse)})
	if s1.EqualsWithoutChildren(s2, EmptyAliasMap()) {
		t.Fatal("Selects with different predicates reported equal")
	}
}

func TestSelect_NotEqualToFilter(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	rv := values.NewBooleanValue(true)
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	s := NewSelectExpression(rv, []Quantifier{q}, []predicates.QueryPredicate{pTrue})
	f := NewLogicalFilterExpression([]predicates.QueryPredicate{pTrue}, q)
	if s.EqualsWithoutChildren(f, EmptyAliasMap()) {
		t.Fatal("Select reported equal to LogicalFilter")
	}
}

func TestSelect_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	rv := values.NewBooleanValue(true)
	pTrue := predicates.NewConstantPredicate(predicates.TriTrue)
	s1 := NewSelectExpression(rv, []Quantifier{q}, []predicates.QueryPredicate{pTrue})
	s2 := NewSelectExpression(values.NewBooleanValue(true), []Quantifier{q}, []predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)})
	if s1.HashCodeWithoutChildren() != s2.HashCodeWithoutChildren() {
		t.Fatal("structurally equal Selects produced different hashes")
	}
}
