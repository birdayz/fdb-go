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
	if u.IsRequired() {
		t.Fatal("ordinary LogicalUnique unexpectedly required")
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

	required1 := NewRequiredLogicalUniqueExpression(q1)
	required2 := NewRequiredLogicalUniqueExpression(q2)
	if !required1.IsRequired() {
		t.Fatal("required LogicalUnique did not retain required mode")
	}
	if !required1.EqualsWithoutChildren(required2, nil) {
		t.Fatal("two required LogicalUnique expressions should be EqualsWithoutChildren")
	}
	if u1.EqualsWithoutChildren(required1, nil) ||
		required1.EqualsWithoutChildren(u1, nil) {
		t.Fatal("ordinary and required LogicalUnique must have distinct memo identity")
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

func TestLogicalUnique_RequiredHashAndWithQuantifiers(t *testing.T) {
	t.Parallel()

	scan1 := NewFullUnorderedScanExpression([]string{"T1"}, values.UnknownType)
	scan2 := NewFullUnorderedScanExpression([]string{"T2"}, values.UnknownType)
	q1 := ForEachQuantifier(InitialOf(scan1))
	q2 := ForEachQuantifier(InitialOf(scan2))

	ordinary := NewLogicalUniqueExpression(q1)
	required := NewRequiredLogicalUniqueExpression(q1)
	if ordinary.HashCodeWithoutChildren() == required.HashCodeWithoutChildren() {
		t.Fatal("ordinary and required LogicalUnique hashes must differ")
	}
	memoRef := InitialOf(ordinary)
	memoRef.Insert(required)
	if got := len(memoRef.AllMembers()); got != 2 {
		t.Fatalf(
			"memo collapsed ordinary and required LogicalUnique to %d member(s)",
			got,
		)
	}

	rebuilt, ok := required.WithQuantifiers([]Quantifier{q2}).(*LogicalUniqueExpression)
	if !ok {
		t.Fatalf("WithQuantifiers type = %T, want *LogicalUniqueExpression", rebuilt)
	}
	if rebuilt.GetInner() != q2 {
		t.Fatal("WithQuantifiers did not install the replacement inner")
	}
	if !rebuilt.IsRequired() {
		t.Fatal("WithQuantifiers dropped required mode")
	}

	ordinaryRebuilt := ordinary.WithQuantifiers([]Quantifier{q2}).(*LogicalUniqueExpression)
	if ordinaryRebuilt.IsRequired() {
		t.Fatal("WithQuantifiers promoted ordinary mode to required")
	}
}
