package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// makeFilter wraps `inner` in a LogicalFilter that tests `pred`.
// Convenience for the SemanticEquals tests.
func makeFilter(pred predicates.QueryPredicate, inner Quantifier) *LogicalFilterExpression {
	return NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, inner)
}

// leafScan is a zero-child RelationalExpression placeholder used by
// SemanticEquals tests — shape-only stand-in for the real
// FullUnorderedScanExpression that will land in a follow-on.
type leafScan struct {
	name string
}

func (l *leafScan) GetResultValue() values.Value    { return values.NewNullValue(values.UnknownType) }
func (l *leafScan) GetQuantifiers() []Quantifier    { return nil }
func (l *leafScan) CanCorrelate() bool              { return false }
func (l *leafScan) ChildrenAsSet() bool             { return false }
func (l *leafScan) HashCodeWithoutChildren() uint64 { return uint64(len(l.name)) }
func (l *leafScan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return nil
}

func (l *leafScan) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*leafScan)
	return ok && o.name == l.name
}

func (l *leafScan) WithQuantifiers(_ []Quantifier) RelationalExpression { return l }

func TestSemanticEquals_NilHandling(t *testing.T) {
	t.Parallel()
	if !SemanticEquals(nil, nil, EmptyAliasMap()) {
		t.Fatal("nil == nil should be true")
	}
	leaf := &leafScan{name: "T"}
	if SemanticEquals(leaf, nil, EmptyAliasMap()) {
		t.Fatal("non-nil != nil should be false")
	}
	if SemanticEquals(nil, leaf, EmptyAliasMap()) {
		t.Fatal("nil != non-nil should be false")
	}
}

func TestSemanticEquals_LeafIdentity(t *testing.T) {
	t.Parallel()
	a := &leafScan{name: "T"}
	b := &leafScan{name: "T"}
	c := &leafScan{name: "U"}
	if !SemanticEquals(a, b, EmptyAliasMap()) {
		t.Fatal("equal leaves reported unequal")
	}
	if SemanticEquals(a, c, EmptyAliasMap()) {
		t.Fatal("distinct leaves reported equal")
	}
}

func TestSemanticEquals_FilterOverLeaf(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	f1 := makeFilter(pred, q1)
	f2 := makeFilter(pred, q2)
	if !SemanticEquals(f1, f2, EmptyAliasMap()) {
		t.Fatal("structurally-equivalent filters reported unequal under empty aliases")
	}
}

func TestSemanticEquals_DifferentInner(t *testing.T) {
	t.Parallel()
	leafT := &leafScan{name: "T"}
	leafU := &leafScan{name: "U"}
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	f1 := makeFilter(pred, ForEachQuantifier(InitialOf(leafT)))
	f2 := makeFilter(pred, ForEachQuantifier(InitialOf(leafU)))
	if SemanticEquals(f1, f2, EmptyAliasMap()) {
		t.Fatal("filters over distinct leaves reported equal")
	}
}

func TestSemanticEquals_DifferentPredicates(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	tru := predicates.NewConstantPredicate(predicates.TriTrue)
	fls := predicates.NewConstantPredicate(predicates.TriFalse)
	f1 := makeFilter(tru, ForEachQuantifier(InitialOf(leaf)))
	f2 := makeFilter(fls, ForEachQuantifier(InitialOf(leaf)))
	if SemanticEquals(f1, f2, EmptyAliasMap()) {
		t.Fatal("filters with different predicates reported equal")
	}
}

func TestSemanticEquals_DifferentExpressionType(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	pred := predicates.NewConstantPredicate(predicates.TriTrue)
	f := makeFilter(pred, ForEachQuantifier(InitialOf(leaf)))
	if SemanticEquals(f, leaf, EmptyAliasMap()) {
		t.Fatal("expressions of different concrete types reported equal")
	}
}
