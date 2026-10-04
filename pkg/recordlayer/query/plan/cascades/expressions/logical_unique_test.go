package expressions

import (
	"testing"
)

func TestLogicalUnique_Construction(t *testing.T) {
	t.Parallel()
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	q := ForEachQuantifier(InitialOf(scan))
	u := mustExpression(NewLogicalUniqueExpression(q))
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

func TestLogicalUnique_QuantifiersReuseOwnedStorage(t *testing.T) {
	t.Parallel()
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	firstQ := ForEachQuantifier(InitialOf(scan))
	secondQ := ForEachQuantifier(InitialOf(scan))
	for _, required := range []bool{false, true} {
		unique := mustExpression(NewLogicalUniqueExpression(firstQ))
		if required {
			unique = mustExpression(NewRequiredLogicalUniqueExpression(firstQ))
		}
		before, again := unique.GetQuantifiers(), unique.GetQuantifiers()
		if len(before) != 1 || before[0] != firstQ || &before[0] != &again[0] {
			t.Fatal("unique rebuilt its read-only quantifier slice")
		}
		rebuilt := mustExpression(unique.WithQuantifiers([]Quantifier{secondQ}))
		after := rebuilt.GetQuantifiers()
		if len(after) != 1 || after[0] != secondQ || &after[0] == &before[0] || before[0] != firstQ {
			t.Fatal("relinked unique did not own independent quantifier storage")
		}
	}
}

func BenchmarkLogicalUniqueQuantifiers(b *testing.B) {
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	unique := mustExpression(NewLogicalUniqueExpression(ForEachQuantifier(InitialOf(scan))))
	b.ReportAllocs()
	var quantifiers []Quantifier
	for b.Loop() {
		quantifiers = unique.GetQuantifiers()
	}
	if len(quantifiers) != 1 {
		b.Fatal("lost inner quantifier")
	}
}

func TestLogicalUnique_GetResultValue(t *testing.T) {
	t.Parallel()
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	q := ForEachQuantifier(InitialOf(scan))
	u := mustExpression(NewLogicalUniqueExpression(q))
	if u.GetResultValue() == nil {
		t.Fatal("GetResultValue returned nil")
	}
}

func TestLogicalUnique_GetCorrelatedToWithoutChildren(t *testing.T) {
	t.Parallel()
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	u := mustExpression(NewLogicalUniqueExpression(ForEachQuantifier(InitialOf(scan))))
	if got := u.GetCorrelatedToWithoutChildren(); got != nil {
		t.Fatalf("GetCorrelatedToWithoutChildren = %v, want nil without allocating an empty read-only set", got)
	}
}

func TestLogicalUnique_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	q1 := ForEachQuantifier(InitialOf(scan))
	q2 := ForEachQuantifier(InitialOf(scan))
	u1 := mustExpression(NewLogicalUniqueExpression(q1))
	u2 := mustExpression(NewLogicalUniqueExpression(q2))
	if !u1.EqualsWithoutChildren(u2, nil) {
		t.Fatal("two LogicalUnique should be EqualsWithoutChildren")
	}
	// vs Distinct: should NOT be equal (different class).
	d := mustExpression(NewLogicalDistinctExpression(q1))
	if u1.EqualsWithoutChildren(d, nil) {
		t.Fatal("LogicalUnique should NOT equal LogicalDistinct (different classes)")
	}

	required1 := mustExpression(NewRequiredLogicalUniqueExpression(q1))
	required2 := mustExpression(NewRequiredLogicalUniqueExpression(q2))
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
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	u := mustExpression(NewLogicalUniqueExpression(ForEachQuantifier(InitialOf(scan))))
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
	scan := mustExpression(NewFullUnorderedScanExpression([]string{"T"}, testRecordType()))
	u := mustExpression(NewLogicalUniqueExpression(ForEachQuantifier(InitialOf(scan))))
	d := mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(InitialOf(scan))))
	if u.HashCodeWithoutChildren() == d.HashCodeWithoutChildren() {
		t.Fatal("LogicalUnique and LogicalDistinct should hash differently (251 vs 31)")
	}
}

func TestLogicalUnique_RequiredHashAndWithQuantifiers(t *testing.T) {
	t.Parallel()

	scan1 := mustExpression(NewFullUnorderedScanExpression([]string{"T1"}, testRecordType()))
	scan2 := mustExpression(NewFullUnorderedScanExpression([]string{"T2"}, testRecordType()))
	q1 := ForEachQuantifier(InitialOf(scan1))
	q2 := ForEachQuantifier(InitialOf(scan2))

	ordinary := mustExpression(NewLogicalUniqueExpression(q1))
	required := mustExpression(NewRequiredLogicalUniqueExpression(q1))
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

	rebuilt, ok := mustWithQuantifiers(t, required, []Quantifier{q2}).(*LogicalUniqueExpression)
	if !ok {
		t.Fatalf("WithQuantifiers type = %T, want *LogicalUniqueExpression", rebuilt)
	}
	if rebuilt.GetInner() != q2 {
		t.Fatal("WithQuantifiers did not install the replacement inner")
	}
	if !rebuilt.IsRequired() {
		t.Fatal("WithQuantifiers dropped required mode")
	}

	ordinaryRebuilt := mustWithQuantifiers(t, ordinary, []Quantifier{q2}).(*LogicalUniqueExpression)
	if ordinaryRebuilt.IsRequired() {
		t.Fatal("WithQuantifiers promoted ordinary mode to required")
	}
}
