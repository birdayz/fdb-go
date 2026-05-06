package expressions

import (
	"testing"
)

func TestLogicalDistinct_Construction(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	d := NewLogicalDistinctExpression(q)
	if d.GetInner().GetAlias() != q.GetAlias() {
		t.Fatal("inner alias mismatch")
	}
	if got := d.GetQuantifiers(); len(got) != 1 || got[0].GetAlias() != q.GetAlias() {
		t.Fatalf("GetQuantifiers wrong: %v", got)
	}
	if d.CanCorrelate() {
		t.Fatal("distinct should not anchor a correlation")
	}
}

func TestLogicalDistinct_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q1 := ForEachQuantifier(InitialOf(leaf))
	q2 := ForEachQuantifier(InitialOf(leaf))
	d1 := NewLogicalDistinctExpression(q1)
	d2 := NewLogicalDistinctExpression(q2)
	if !d1.EqualsWithoutChildren(d2, EmptyAliasMap()) {
		t.Fatal("two LogicalDistincts reported unequal-without-children — should always be class-equal")
	}
	if d1.EqualsWithoutChildren(leaf, EmptyAliasMap()) {
		t.Fatal("distinct reported equal to non-distinct expression")
	}
}

func TestLogicalDistinct_HashCodeStable(t *testing.T) {
	t.Parallel()
	leaf := &leafScan{name: "T"}
	q := ForEachQuantifier(InitialOf(leaf))
	d1 := NewLogicalDistinctExpression(q)
	d2 := NewLogicalDistinctExpression(q)
	if d1.HashCodeWithoutChildren() != d2.HashCodeWithoutChildren() {
		t.Fatal("two LogicalDistincts produced different constant hashes")
	}
}
