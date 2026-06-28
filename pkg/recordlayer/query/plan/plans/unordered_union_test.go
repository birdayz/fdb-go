package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ---------------------------------------------------------------------------
// RecordQueryUnorderedUnionPlan
// ---------------------------------------------------------------------------

func TestUnorderedUnionPlan_Construction(t *testing.T) {
	t.Parallel()
	a := stub("A")
	b := stub("B")
	p := NewRecordQueryUnorderedUnionPlan([]RecordQueryPlan{a, b})
	if p == nil {
		t.Fatal("constructor returned nil")
	}
	if len(p.GetInners()) != 2 {
		t.Fatalf("GetInners() len = %d, want 2", len(p.GetInners()))
	}
}

func TestUnorderedUnionPlan_GetResultType_FirstInner(t *testing.T) {
	t.Parallel()
	scan := NewRecordQueryScanPlan([]string{"T"}, values.NotNullLong, false)
	p := NewRecordQueryUnorderedUnionPlan([]RecordQueryPlan{scan, stub("B")})
	if !values.NotNullLong.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want NotNullLong (from first inner)", p.GetResultType())
	}
}

func TestUnorderedUnionPlan_GetResultType_Empty(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryUnorderedUnionPlan(nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestUnorderedUnionPlan_GetChildren(t *testing.T) {
	t.Parallel()
	a := stub("A")
	b := stub("B")
	p := NewRecordQueryUnorderedUnionPlan([]RecordQueryPlan{a, b})
	cs := p.GetChildren()
	if len(cs) != 2 || cs[0] != a || cs[1] != b {
		t.Fatal("GetChildren() mismatch")
	}
}

func TestUnorderedUnionPlan_Explain(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryUnorderedUnionPlan([]RecordQueryPlan{stub("A"), stub("B")})
	got := p.Explain()
	if !strings.Contains(got, "UnorderedUnion") {
		t.Fatalf("Explain = %q, missing 'UnorderedUnion'", got)
	}
	if !strings.Contains(got, "A") || !strings.Contains(got, "B") {
		t.Fatalf("Explain = %q, missing child labels", got)
	}
}

func TestUnorderedUnionPlan_Explain_NilInner(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryUnorderedUnionPlan([]RecordQueryPlan{nil})
	got := p.Explain()
	if !strings.Contains(got, "<nil>") {
		t.Fatalf("Explain = %q, missing '<nil>' for nil inner", got)
	}
}

func TestUnorderedUnionPlan_Explain_Empty(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryUnorderedUnionPlan(nil)
	if got := p.Explain(); got != "UnorderedUnion()" {
		t.Fatalf("Explain = %q, want 'UnorderedUnion()'", got)
	}
}

func TestUnorderedUnionPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryUnorderedUnionPlan(nil)
	b := NewRecordQueryUnorderedUnionPlan([]RecordQueryPlan{stub("X")})
	// Unordered union equality is type-only.
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("any two UnorderedUnionPlans should be EqualsWithoutChildren")
	}
}

func TestUnorderedUnionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	u := NewRecordQueryUnorderedUnionPlan(nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if u.EqualsWithoutChildren(scan) {
		t.Fatal("UnorderedUnionPlan should not equal ScanPlan")
	}
}

func TestUnorderedUnionPlan_EqualsWithoutChildren_NotEqualToOrderedUnion(t *testing.T) {
	t.Parallel()
	uu := NewRecordQueryUnorderedUnionPlan(nil)
	ou := NewRecordQueryUnionPlan(nil)
	if uu.EqualsWithoutChildren(ou) {
		t.Fatal("UnorderedUnionPlan should not equal UnionPlan")
	}
}

func TestUnorderedUnionPlan_HashCodeWithoutChildren_Deterministic(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryUnorderedUnionPlan(nil)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestUnorderedUnionPlan_HashCodeWithoutChildren_SameAcrossInstances(t *testing.T) {
	t.Parallel()
	a := NewRecordQueryUnorderedUnionPlan(nil)
	b := NewRecordQueryUnorderedUnionPlan([]RecordQueryPlan{stub("X")})
	// No operator params, so all instances hash the same.
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("all UnorderedUnionPlan instances should have the same hash (no params)")
	}
}

func TestUnorderedUnionPlan_HashDistinctFromOrderedUnion(t *testing.T) {
	t.Parallel()
	uu := NewRecordQueryUnorderedUnionPlan(nil)
	ou := NewRecordQueryUnionPlan(nil)
	if uu.HashCodeWithoutChildren() == ou.HashCodeWithoutChildren() {
		t.Fatal("UnorderedUnionPlan and UnionPlan should have different hashes")
	}
}

func TestUnorderedUnionPlan_CopiesInnerSlice(t *testing.T) {
	t.Parallel()
	inners := []RecordQueryPlan{stub("A")}
	p := NewRecordQueryUnorderedUnionPlan(inners)
	inners[0] = stub("B")
	if p.GetInners()[0].Explain() != "A" {
		t.Fatal("unordered union should have an independent copy of the inner slice")
	}
}

func TestUnorderedUnionPlan_SingleChild(t *testing.T) {
	t.Parallel()
	inner := stub("Only")
	p := NewRecordQueryUnorderedUnionPlan([]RecordQueryPlan{inner})
	if len(p.GetChildren()) != 1 {
		t.Fatalf("GetChildren() len = %d, want 1", len(p.GetChildren()))
	}
	got := p.Explain()
	if !strings.Contains(got, "Only") {
		t.Fatalf("Explain = %q, missing child label", got)
	}
}

func TestUnorderedUnionPlan_ThreeChildren(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryUnorderedUnionPlan([]RecordQueryPlan{stub("A"), stub("B"), stub("C")})
	if len(p.GetChildren()) != 3 {
		t.Fatalf("GetChildren() len = %d, want 3", len(p.GetChildren()))
	}
	got := p.Explain()
	if !strings.Contains(got, "A") || !strings.Contains(got, "B") || !strings.Contains(got, "C") {
		t.Fatalf("Explain = %q, missing child labels", got)
	}
}
