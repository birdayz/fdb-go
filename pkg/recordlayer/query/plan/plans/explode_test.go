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
