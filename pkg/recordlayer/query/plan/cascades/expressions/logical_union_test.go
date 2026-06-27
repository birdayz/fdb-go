package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestLogicalUnion_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	qs := []Quantifier{
		ForEachQuantifier(InitialOf(leaf)),
		ForEachQuantifier(InitialOf(leaf)),
	}
	u := NewLogicalUnionExpression(qs)
	if got := u.GetQuantifiers(); len(got) != 2 {
		t.Fatalf("GetQuantifiers size=%d, want 2", len(got))
	}
	if u.CanCorrelate() {
		t.Fatal("union should not anchor a correlation")
	}
}

func TestLogicalUnion_DefensiveCopy(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	a := ForEachQuantifier(InitialOf(leaf))
	b := ForEachQuantifier(InitialOf(leaf))
	src := []Quantifier{a, b}
	u := NewLogicalUnionExpression(src)
	src[0] = b
	if u.GetQuantifiers()[0].GetAlias() != a.GetAlias() {
		t.Fatal("constructor failed to defensively copy quantifiers")
	}
}

func TestLogicalUnion_EmptyChildren_Safe(t *testing.T) {
	t.Parallel()
	u := NewLogicalUnionExpression(nil)
	if got := u.GetResultValue(); got == nil {
		t.Fatal("empty union returned nil ResultValue — should be a NullValue placeholder")
	}
	if _, ok := u.GetResultValue().(*values.NullValue); !ok {
		t.Fatalf("empty union ResultValue type=%T, want *NullValue", u.GetResultValue())
	}
}

func TestLogicalUnion_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	u1 := NewLogicalUnionExpression([]Quantifier{q})
	u2 := NewLogicalUnionExpression([]Quantifier{q, q})
	if !u1.EqualsWithoutChildren(u2, EmptyAliasMap()) {
		t.Fatal("two LogicalUnions reported unequal-without-children — should always be class-equal")
	}
	if u1.EqualsWithoutChildren(leaf, EmptyAliasMap()) {
		t.Fatal("union reported equal to non-union expression")
	}
}

func TestLogicalUnion_DistinctFromDistinct(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	u := NewLogicalUnionExpression([]Quantifier{q})
	d := NewLogicalDistinctExpression(q)
	if u.HashCodeWithoutChildren() == d.HashCodeWithoutChildren() {
		t.Fatal("union and distinct produced identical class-discriminating hashes")
	}
}
