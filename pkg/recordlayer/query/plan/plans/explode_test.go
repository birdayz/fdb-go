package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func newTestArrayValue(elems ...any) values.Value {
	return &values.ConstantValue{
		Typ:   &values.ArrayType{ElementType: values.NotNullLong},
		Value: elems,
	}
}

func TestExplodePlan_Construction(t *testing.T) {
	t.Parallel()
	v := newTestArrayValue(1)
	p := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(v)
	})
	if p.GetCollectionValue() != v {
		t.Fatal("collection value mismatch")
	}
}

func TestExplodePlan_GetChildren_Nil(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(newTestArrayValue())
	})
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
	p := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(v)
	})
	if !p.GetResultType().Equals(elemType) {
		t.Fatalf("GetResultType() = %v, want %v", p.GetResultType(), elemType)
	}
}

func TestExplodePlan_FreezesResultTypeAgainstCollectionMutation(t *testing.T) {
	t.Parallel()
	row := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullInt},
	}}
	collection := values.NewArrayConstructorValue(row, nil)
	plain := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(collection)
	})
	ordinal := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlanWithOrdinality(collection, true)
	})
	plainWant := plain.GetResultType()
	ordinalWant := ordinal.GetResultType()

	// The collection Value is intentionally an ordinary mutable graph. A plan
	// that rereads it after publishing a layout can emit a differently typed row
	// than that layout admits.
	row.Fields[0].FieldType = values.NullableInt
	row.Fields[0].Name = "MUTATED"
	if got := plain.GetResultType(); !got.Equals(plainWant) {
		t.Fatalf("plain result type drifted to %v, want frozen %v", got, plainWant)
	}
	if got := plain.GetElementType(); !got.Equals(plainWant) {
		t.Fatalf("plain element type drifted to %v, want frozen %v", got, plainWant)
	}
	if got := ordinal.GetResultType(); !got.Equals(ordinalWant) {
		t.Fatalf("ordinal result type drifted to %v, want frozen %v", got, ordinalWant)
	}
	ordinalRow := ordinal.GetResultType().(*values.RecordType)
	if got := ordinal.GetElementType(); !got.Equals(ordinalRow.Fields[0].FieldType) {
		t.Fatalf("ordinal element type = %v, want frozen result slot %v", got, ordinalRow.Fields[0].FieldType)
	}
}

func TestExplodePlan_ConstructorRejectsNilCollection(t *testing.T) {
	t.Parallel()
	if _, err := NewRecordQueryExplodePlan(nil); err == nil {
		t.Fatal("constructor accepted a nil collection")
	}
	if got := (&RecordQueryExplodePlan{}).GetElementType(); !values.UnknownType.Equals(got) {
		t.Fatalf("malformed zero-value GetElementType() = %v, want UnknownType", got)
	}
}

func TestExplodePlan_ConstructorRejectsNonArrayType(t *testing.T) {
	t.Parallel()
	v := &values.ConstantValue{Typ: values.NotNullString, Value: "not_an_array"}
	if _, err := NewRecordQueryExplodePlan(v); err == nil {
		t.Fatal("constructor accepted a non-array collection")
	}
}

func TestExplodePlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	v := newTestArrayValue(1)
	a := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(v)
	})
	b := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(v)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same collection value should be equal")
	}
}

func TestExplodePlan_EqualsWithoutChildren_Different(t *testing.T) {
	t.Parallel()
	v1 := newTestArrayValue(1)
	v2 := newTestArrayValue(2)
	a := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(v1)
	})
	b := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(v2)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different collection values should not be equal")
	}
}

func TestExplodePlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(newTestArrayValue())
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if p.EqualsPlanWithoutChildren(scan) {
		t.Fatal("ExplodePlan should not equal a ScanPlan")
	}
}

func TestExplodePlan_HashCodeWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	v := newTestArrayValue(1)
	a := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(v)
	})
	b := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(v)
	})
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("same collection value should produce same hash")
	}
}

func TestExplodePlan_HashCodeWithoutChildren_Consistent(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(newTestArrayValue())
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestExplodePlan_Explain(t *testing.T) {
	t.Parallel()
	v := newTestArrayValue(1, 2)
	p := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(v)
	})
	exp := p.Explain()
	if !strings.Contains(exp, "Explode") {
		t.Fatalf("Explain = %q, want 'Explode'", exp)
	}
}

func TestExplodePlan_Explain_Nil(t *testing.T) {
	t.Parallel()
	p := &RecordQueryExplodePlan{}
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
	plain := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlan(arr)
	})
	ord := mustChecked(t, func() (*RecordQueryExplodePlan, error) {
		return NewRecordQueryExplodePlanWithOrdinality(arr, true)
	})

	if !ord.IsWithOrdinality() || plain.IsWithOrdinality() {
		t.Fatal("IsWithOrdinality flag mismatch")
	}
	if plain.EqualsPlanWithoutChildren(ord) {
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
