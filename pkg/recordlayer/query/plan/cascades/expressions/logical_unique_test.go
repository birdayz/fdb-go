package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestLogicalUnique_Construction(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := ForEachQuantifier(InitialOf(scan))
	u := NewLogicalUniqueExpression(q)
	if u.GetInner() != q {
		t.Fatalf("GetInner mismatch")
	}
	if got := u.GetQuantifiers(); len(got) != 1 {
		t.Fatalf("GetQuantifiers len = %d, want 1", len(got))
	}
	if u.CanCorrelate() {
		t.Fatal("CanCorrelate = true, want false")
	}
	if u.ChildrenAsSet() {
		t.Fatal("ChildrenAsSet = true, want false")
	}
}

func TestLogicalUnique_GetResultValue(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q := ForEachQuantifier(InitialOf(scan))
	u := NewLogicalUniqueExpression(q)
	if u.GetResultValue() == nil {
		t.Fatal("GetResultValue returned nil")
	}
}

func TestLogicalUnique_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	u := NewLogicalUniqueExpression(ForEachQuantifier(InitialOf(scan)))
	if got := u.GetCorrelatedToWithoutChildren(); len(got) != 0 {
		t.Fatalf("GetCorrelatedToWithoutChildren = %v, want empty", got)
	}
}

func TestLogicalUnique_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	q1 := ForEachQuantifier(InitialOf(scan))
	q2 := ForEachQuantifier(InitialOf(scan))
	u1 := NewLogicalUniqueExpression(q1)
	u2 := NewLogicalUniqueExpression(q2)
	if !u1.EqualsWithoutChildren(u2, nil) {
		t.Fatal("two LogicalUnique should be EqualsWithoutChildren")
	}
	// vs Distinct: should NOT be equal (different class).
	d := NewLogicalDistinctExpression(q1)
	if u1.EqualsWithoutChildren(d, nil) {
		t.Fatal("LogicalUnique should NOT equal LogicalDistinct (different classes)")
	}
}

func TestLogicalUnique_HashCodeStable(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	u := NewLogicalUniqueExpression(ForEachQuantifier(InitialOf(scan)))
	h1 := u.HashCodeWithoutChildren()
	h2 := u.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("HashCodeWithoutChildren non-deterministic: %d vs %d", h1, h2)
	}
	if h1 != 251 {
		t.Fatalf("HashCodeWithoutChildren = %d, want 251 (Java's class-discriminating constant)", h1)
	}
}

func TestLogicalUnique_DistinctFromDistinctHash(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	u := NewLogicalUniqueExpression(ForEachQuantifier(InitialOf(scan)))
	d := NewLogicalDistinctExpression(ForEachQuantifier(InitialOf(scan)))
	if u.HashCodeWithoutChildren() == d.HashCodeWithoutChildren() {
		t.Fatal("LogicalUnique and LogicalDistinct should hash differently (251 vs 31)")
	}
}
