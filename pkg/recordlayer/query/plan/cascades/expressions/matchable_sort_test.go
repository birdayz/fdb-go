package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestMatchableSort_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ids := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
		values.NamedCorrelationIdentifier("p2"),
	}
	e := NewMatchableSortExpression(ids, false, q)

	if got := e.GetSortParameterIDs(); len(got) != 2 {
		t.Fatalf("GetSortParameterIDs: got %d, want 2", len(got))
	}
	if e.IsReverse() {
		t.Fatal("IsReverse: expected false")
	}
	if e.GetInner() != q {
		t.Fatal("GetInner: did not return the provided quantifier")
	}
}

func TestMatchableSort_ConstructionReverse(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ids := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
	}
	e := NewMatchableSortExpression(ids, true, q)
	if !e.IsReverse() {
		t.Fatal("IsReverse: expected true for reverse sort")
	}
}

func TestMatchableSort_FromExpr(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	ids := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
	}
	e := NewMatchableSortExpressionFromExpr(ids, false, leaf)

	// The inner quantifier should be a ForEach ranging over a
	// Reference containing leaf.
	inner := e.GetInner()
	if inner.GetRangesOver() == nil {
		t.Fatal("inner quantifier's Reference is nil")
	}
	if inner.GetRangesOver().Get() != leaf {
		t.Fatal("inner quantifier does not range over the provided expression")
	}
}

func TestMatchableSort_DefensiveCopy(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ids := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
	}
	e := NewMatchableSortExpression(ids, false, q)
	// Mutate the original slice — should not affect the expression.
	ids[0] = values.NamedCorrelationIdentifier("MUTATED")
	if e.GetSortParameterIDs()[0].String() == "MUTATED" {
		t.Fatal("constructor failed to defensively copy sort parameter IDs")
	}
}

func TestMatchableSort_GetQuantifiers(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ids := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
	}
	e := NewMatchableSortExpression(ids, false, q)

	qs := e.GetQuantifiers()
	if len(qs) != 1 {
		t.Fatalf("GetQuantifiers: got %d, want 1", len(qs))
	}
	if qs[0] != q {
		t.Fatal("GetQuantifiers[0] is not the inner quantifier")
	}
}

func TestMatchableSort_CanCorrelate(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	e := NewMatchableSortExpression(nil, false, q)
	if e.CanCorrelate() {
		t.Fatal("CanCorrelate: expected false")
	}
}

func TestMatchableSort_ChildrenAsSet(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	e := NewMatchableSortExpression(nil, false, q)
	if e.ChildrenAsSet() {
		t.Fatal("ChildrenAsSet: expected false")
	}
}

func TestMatchableSort_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ids := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
	}
	e := NewMatchableSortExpression(ids, false, q)
	corr := e.GetCorrelatedToWithoutChildren()
	if len(corr) != 0 {
		t.Fatalf("GetCorrelatedToWithoutChildren: got %d entries, want 0", len(corr))
	}
}

func TestMatchableSort_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ids1 := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
		values.NamedCorrelationIdentifier("p2"),
	}
	ids2 := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
		values.NamedCorrelationIdentifier("p2"),
	}
	ids3 := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
		values.NamedCorrelationIdentifier("p3"), // different
	}
	ids4 := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"), // different length
	}

	e1 := NewMatchableSortExpression(ids1, false, q)
	e2 := NewMatchableSortExpression(ids2, false, q) // same
	e3 := NewMatchableSortExpression(ids3, false, q) // different param
	e4 := NewMatchableSortExpression(ids1, true, q)  // different reverse
	e5 := NewMatchableSortExpression(ids4, false, q) // different length

	if !e1.EqualsWithoutChildren(e2, EmptyAliasMap()) {
		t.Fatal("structurally identical expressions reported unequal")
	}
	if e1.EqualsWithoutChildren(e3, EmptyAliasMap()) {
		t.Fatal("expressions with different param IDs reported equal")
	}
	if e1.EqualsWithoutChildren(e4, EmptyAliasMap()) {
		t.Fatal("expressions with different reverse flags reported equal")
	}
	if e1.EqualsWithoutChildren(e5, EmptyAliasMap()) {
		t.Fatal("expressions with different param list lengths reported equal")
	}
	if e1.EqualsWithoutChildren(leaf, EmptyAliasMap()) {
		t.Fatal("matchable sort reported equal to non-matchable-sort expression")
	}
}

func TestMatchableSort_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ids1 := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
		values.NamedCorrelationIdentifier("p2"),
	}
	ids2 := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
		values.NamedCorrelationIdentifier("p2"),
	}
	ids3 := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
		values.NamedCorrelationIdentifier("p2"),
	}

	e1 := NewMatchableSortExpression(ids1, false, q)
	e2 := NewMatchableSortExpression(ids2, false, q)
	e3 := NewMatchableSortExpression(ids3, true, q) // different reverse

	if e1.HashCodeWithoutChildren() != e2.HashCodeWithoutChildren() {
		t.Fatal("structurally equal expressions produced different hash codes")
	}
	if e1.HashCodeWithoutChildren() == e3.HashCodeWithoutChildren() {
		t.Fatal("expressions with different reverse flags produced identical hashes (collision unlikely)")
	}
}

func TestMatchableSort_GetResultValue(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	ids := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
	}
	e := NewMatchableSortExpression(ids, false, q)

	rv := e.GetResultValue()
	qov, ok := rv.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("GetResultValue: got %T, want *QuantifiedObjectValue", rv)
	}
	// The QuantifiedObjectValue should carry the inner quantifier's alias.
	if _, has := qov.GetCorrelatedTo()[q.GetAlias()]; !has {
		t.Fatal("GetResultValue does not carry the inner quantifier's alias")
	}
}

func TestMatchableSort_WithQuantifiers(t *testing.T) {
	t.Parallel()
	leaf1 := &leafScan{name: "T1"}
	leaf2 := &leafScan{name: "T2"}
	q1 := ForEachQuantifier(InitialOf(leaf1))
	q2 := ForEachQuantifier(InitialOf(leaf2))
	ids := []values.CorrelationIdentifier{
		values.NamedCorrelationIdentifier("p1"),
	}
	e := NewMatchableSortExpression(ids, true, q1)
	rebuilt := e.WithQuantifiers([]Quantifier{q2})
	mse, ok := rebuilt.(*MatchableSortExpression)
	if !ok {
		t.Fatalf("WithQuantifiers: got %T, want *MatchableSortExpression", rebuilt)
	}
	if mse.GetInner() != q2 {
		t.Fatal("WithQuantifiers: inner quantifier not replaced")
	}
	if !mse.IsReverse() {
		t.Fatal("WithQuantifiers: reverse flag not preserved")
	}
	if len(mse.GetSortParameterIDs()) != 1 {
		t.Fatal("WithQuantifiers: sort parameter IDs not preserved")
	}
}

// Verify compile-time interface satisfaction.
var _ RelationalExpression = (*MatchableSortExpression)(nil)
