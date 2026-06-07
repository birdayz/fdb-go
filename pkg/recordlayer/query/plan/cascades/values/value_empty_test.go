package values

import "testing"

func TestEmptyValue_IsLeaf(t *testing.T) {
	t.Parallel()
	v := NewEmptyValue()
	if got := len(v.Children()); got != 0 {
		t.Fatalf("EmptyValue children = %d, want 0", got)
	}
}

func TestEmptyValue_TypeIsEmptyRecord(t *testing.T) {
	t.Parallel()
	v := NewEmptyValue()
	rt, ok := v.Type().(*RecordType)
	if !ok {
		t.Fatalf("EmptyValue Type = %T, want *RecordType", v.Type())
	}
	if rt.IsNullable() {
		t.Fatal("EmptyValue type should be non-nullable")
	}
	// No exported field-count accessor on RecordType; check via
	// LookupField for a sentinel that can't exist.
	if _, ok := rt.LookupField("anything"); ok {
		t.Fatal("EmptyValue type should have no fields")
	}
}

func TestEmptyValue_EvaluateReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewEmptyValue()
	if got := mustEvaluate(v, nil); got != nil {
		t.Fatalf("EmptyValue.Evaluate = %v, want nil", got)
	}
}

func TestEmptyValue_Singleton(t *testing.T) {
	t.Parallel()
	if Empty == nil {
		t.Fatal("Empty singleton should be non-nil")
	}
	if Empty.Name() != "empty" {
		t.Fatalf("Empty.Name() = %q, want %q", Empty.Name(), "empty")
	}
}
