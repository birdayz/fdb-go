package plans

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func newTestArrayValue(elems ...any) values.Value {
	return &values.ConstantValue{
		Typ:   &values.ArrayType{ElementType: values.UnknownType},
		Value: elems,
	}
}

func TestExplodePlan_Construction(t *testing.T) {
	t.Parallel()
	v := newTestArrayValue(1)
	p := NewRecordQueryExplodePlan(v)
	if p.GetCollectionValue() != v {
		t.Fatal("collection value mismatch")
	}
}

func TestExplodePlan_GetChildren_Nil(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryExplodePlan(nil)
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestExplodePlan_GetResultType_ArrayElement(t *testing.T) {
	t.Parallel()
	elemType := values.NewPrimitiveType(values.TypeCodeInt, false)
	v := &values.ConstantValue{
		Typ:   &values.ArrayType{ElementType: elemType},
		Value: []any{1},
	}
	p := NewRecordQueryExplodePlan(v)
	if !p.GetResultType().Equals(elemType) {
		t.Fatalf("GetResultType() = %v, want %v", p.GetResultType(), elemType)
	}
}

func TestExplodePlan_GetResultType_NilCollection(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryExplodePlan(nil)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType", p.GetResultType())
	}
}

func TestExplodePlan_GetResultType_NonArrayType(t *testing.T) {
	t.Parallel()
	v := &values.ConstantValue{Typ: values.UnknownType, Value: "not_an_array"}
	p := NewRecordQueryExplodePlan(v)
	if !values.UnknownType.Equals(p.GetResultType()) {
		t.Fatalf("GetResultType() = %v, want UnknownType for non-array", p.GetResultType())
	}
}

func TestExplodePlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	v := newTestArrayValue(1)
	a := NewRecordQueryExplodePlan(v)
	b := NewRecordQueryExplodePlan(v)
	if !a.EqualsWithoutChildren(b) {
		t.Fatal("same collection value should be equal")
	}
}

func TestExplodePlan_EqualsWithoutChildren_Different(t *testing.T) {
	t.Parallel()
	v1 := newTestArrayValue(1)
	v2 := newTestArrayValue(2)
	a := NewRecordQueryExplodePlan(v1)
	b := NewRecordQueryExplodePlan(v2)
	if a.EqualsWithoutChildren(b) {
		t.Fatal("different collection values should not be equal")
	}
}

func TestExplodePlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryExplodePlan(nil)
	scan := NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
	if p.EqualsWithoutChildren(scan) {
		t.Fatal("ExplodePlan should not equal a ScanPlan")
	}
}

func TestExplodePlan_HashCodeWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	v := newTestArrayValue(1)
	a := NewRecordQueryExplodePlan(v)
	b := NewRecordQueryExplodePlan(v)
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("same collection value should produce same hash")
	}
}

func TestExplodePlan_HashCodeWithoutChildren_Consistent(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryExplodePlan(nil)
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestExplodePlan_Explain(t *testing.T) {
	t.Parallel()
	v := newTestArrayValue(1, 2)
	p := NewRecordQueryExplodePlan(v)
	exp := p.Explain()
	if !strings.Contains(exp, "Explode") {
		t.Fatalf("Explain = %q, want 'Explode'", exp)
	}
}

func TestExplodePlan_Explain_Nil(t *testing.T) {
	t.Parallel()
	p := NewRecordQueryExplodePlan(nil)
	if got := p.Explain(); got != "Explode(<nil>)" {
		t.Fatalf("Explain = %q, want 'Explode(<nil>)'", got)
	}
}

// TestExplodePlan_WithOrdinality pins the physical plan's RFC-142 ordinality
// threading: a WITH ORDINALITY plan is distinct (equals/hash) from a bare one
// over the same array, its result type is the 2-field record, and its Explain
// surfaces WITH ORDINALITY.
func TestExplodePlan_WithOrdinality(t *testing.T) {
	t.Parallel()
	arr := values.NewArrayConstructorValue(values.NotNullLong, []values.Value{
		values.LiteralValue(int64(1)),
	})
	plain := NewRecordQueryExplodePlan(arr)
	ord := NewRecordQueryExplodePlanWithOrdinality(arr, true)

	if !ord.IsWithOrdinality() || plain.IsWithOrdinality() {
		t.Fatal("IsWithOrdinality flag mismatch")
	}
	if plain.EqualsWithoutChildren(ord) {
		t.Fatal("ordinal and non-ordinal Explode plans must NOT be equal")
	}
	if plain.HashCodeWithoutChildren() == ord.HashCodeWithoutChildren() {
		t.Fatal("ordinal and non-ordinal Explode plans must hash differently")
	}
	rt, ok := ord.GetResultType().(*values.RecordType)
	if !ok || len(rt.Fields) != 2 {
		t.Fatalf("ordinality result type = %v, want 2-field record", ord.GetResultType())
	}
	if !strings.Contains(ord.Explain(), "WITH ORDINALITY") {
		t.Fatalf("ordinal Explain = %q, want WITH ORDINALITY", ord.Explain())
	}
	if strings.Contains(plain.Explain(), "WITH ORDINALITY") {
		t.Fatalf("non-ordinal Explain = %q, must NOT contain WITH ORDINALITY", plain.Explain())
	}
}
