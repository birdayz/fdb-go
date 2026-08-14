package plans

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func newTestStreamValue() values.Value {
	return &values.ConstantValue{
		Typ:   values.NewPrimitiveType(values.TypeCodeInt, false),
		Value: 42,
	}
}

func TestTableFunctionPlan_Construction(t *testing.T) {
	t.Parallel()
	v := newTestStreamValue()
	p := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(v)
	})
	if p.GetStreamValue() != v {
		t.Fatal("stream value mismatch")
	}
}

func TestTableFunctionPlan_GetChildren_Nil(t *testing.T) {
	t.Parallel()
	// The constructor rejects a nil stream; the method remains total for a
	// legacy/deserialized zero value.
	p := &RecordQueryTableFunctionPlan{}
	if cs := p.GetChildren(); cs != nil {
		t.Fatalf("GetChildren() = %v, want nil", cs)
	}
}

func TestTableFunctionPlan_GetResultType(t *testing.T) {
	t.Parallel()
	v := newTestStreamValue()
	p := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(v)
	})
	if !p.GetResultType().Equals(v.Type()) {
		t.Fatalf("GetResultType() = %v, want %v", p.GetResultType(), v.Type())
	}
}

func TestTableFunctionPlan_GetResultType_NilStream(t *testing.T) {
	t.Parallel()
	p, err := NewRecordQueryTableFunctionPlan(nil)
	if err == nil || p != nil {
		t.Fatalf("nil-stream table function = (%#v, %v), want nil,error", p, err)
	}
}

func TestTableFunctionPlan_EqualsWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	v := newTestStreamValue()
	a := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(v)
	})
	b := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(v)
	})
	if !a.EqualsPlanWithoutChildren(b) {
		t.Fatal("same stream value should be equal")
	}
}

func TestTableFunctionPlan_EqualsWithoutChildren_Different(t *testing.T) {
	t.Parallel()
	v1 := newTestStreamValue()
	v2 := newTestStreamValue()
	a := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(v1)
	})
	b := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(v2)
	})
	if a.EqualsPlanWithoutChildren(b) {
		t.Fatal("different stream values should not be equal")
	}
}

func TestTableFunctionPlan_EqualsWithoutChildren_WrongType(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(newTestStreamValue())
	})
	scan := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, exactTestRecordType(), false)
	})
	if p.EqualsPlanWithoutChildren(scan) {
		t.Fatal("TableFunctionPlan should not equal a ScanPlan")
	}
}

func TestTableFunctionPlan_HashCodeWithoutChildren_Same(t *testing.T) {
	t.Parallel()
	v := newTestStreamValue()
	a := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(v)
	})
	b := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(v)
	})
	if a.HashCodeWithoutChildren() != b.HashCodeWithoutChildren() {
		t.Fatal("same stream value should produce same hash")
	}
}

func TestTableFunctionPlan_HashCodeWithoutChildren_Consistent(t *testing.T) {
	t.Parallel()
	p := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(newTestStreamValue())
	})
	h1 := p.HashCodeWithoutChildren()
	h2 := p.HashCodeWithoutChildren()
	if h1 != h2 {
		t.Fatalf("hash non-deterministic: %d vs %d", h1, h2)
	}
}

func TestTableFunctionPlan_Explain(t *testing.T) {
	t.Parallel()
	v := newTestStreamValue()
	p := mustChecked(t, func() (*RecordQueryTableFunctionPlan, error) {
		return NewRecordQueryTableFunctionPlan(v)
	})
	exp := p.Explain()
	if !strings.Contains(exp, "TableFunction") {
		t.Fatalf("Explain = %q, want 'TableFunction'", exp)
	}
}

func TestTableFunctionPlan_Explain_Nil(t *testing.T) {
	t.Parallel()
	// Constructor admission rejects a nil stream; Explain remains defensive for
	// legacy/deserialized zero values.
	p := &RecordQueryTableFunctionPlan{}
	if got := p.Explain(); got != "TableFunction(<nil>)" {
		t.Fatalf("Explain = %q, want 'TableFunction(<nil>)'", got)
	}
}
