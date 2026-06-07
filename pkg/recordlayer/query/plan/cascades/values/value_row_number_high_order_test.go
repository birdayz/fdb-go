package values

import "testing"

func TestRowNumberHighOrderValue_Type(t *testing.T) {
	t.Parallel()
	h := NewRowNumberHighOrderValue(nil, nil)
	if !h.Type().Equals(UnknownType) {
		t.Fatalf("Type = %v, want UnknownType (high-order)", h.Type())
	}
}

func TestRowNumberHighOrderValue_Name(t *testing.T) {
	t.Parallel()
	h := NewRowNumberHighOrderValue(nil, nil)
	if got := h.Name(); got != "ROW_NUMBER_HIGH_ORDER" {
		t.Fatalf("Name = %q, want ROW_NUMBER_HIGH_ORDER", got)
	}
}

func TestRowNumberHighOrderValue_NoChildren(t *testing.T) {
	t.Parallel()
	h := NewRowNumberHighOrderValue(nil, nil)
	if got := h.Children(); len(got) != 0 {
		t.Fatalf("Children = %v, want empty (leaf)", got)
	}
}

func TestRowNumberHighOrderValue_EvaluateIsPlaceholder(t *testing.T) {
	t.Parallel()
	h := NewRowNumberHighOrderValue(nil, nil)
	if got := mustEvalForTest(h, nil); got != nil {
		t.Fatalf("Evaluate = %v, want nil (high-order placeholder)", got)
	}
}

func TestRowNumberHighOrderValue_ConfigStored(t *testing.T) {
	t.Parallel()
	ef := 100
	rv := true
	h := NewRowNumberHighOrderValue(&ef, &rv)
	if h.EfSearch == nil || *h.EfSearch != 100 {
		t.Fatalf("EfSearch = %v, want 100", h.EfSearch)
	}
	if h.IsReturningVectors == nil || *h.IsReturningVectors != true {
		t.Fatalf("IsReturningVectors = %v, want true", h.IsReturningVectors)
	}
}

func TestRowNumberHighOrderValue_NilConfig(t *testing.T) {
	t.Parallel()
	h := NewRowNumberHighOrderValue(nil, nil)
	if h.EfSearch != nil {
		t.Fatalf("EfSearch = %v, want nil", h.EfSearch)
	}
	if h.IsReturningVectors != nil {
		t.Fatalf("IsReturningVectors = %v, want nil", h.IsReturningVectors)
	}
}

func TestRowNumberHighOrderValue_ApplyProducesRowNumberValue(t *testing.T) {
	t.Parallel()
	ef := 50
	h := NewRowNumberHighOrderValue(&ef, nil)
	partition := []Value{LiteralValue("region")}
	args := []Value{LiteralValue("dist")}
	rv := h.Apply(partition, args)
	if rv == nil {
		t.Fatal("Apply returned nil")
	}
	if len(rv.PartitioningValues) != 1 {
		t.Fatalf("PartitioningValues = %d, want 1", len(rv.PartitioningValues))
	}
	if len(rv.ArgumentValues) != 1 {
		t.Fatalf("ArgumentValues = %d, want 1", len(rv.ArgumentValues))
	}
	if rv.EfSearch == nil || *rv.EfSearch != 50 {
		t.Fatalf("EfSearch = %v, want 50 (carried through Apply)", rv.EfSearch)
	}
	if !rv.IsIndexOnly() {
		t.Fatal("Applied RowNumberValue should be index-only")
	}
}

func TestRowNumberHighOrderValue_ApplyWithEmptyArgs(t *testing.T) {
	t.Parallel()
	h := NewRowNumberHighOrderValue(nil, nil)
	rv := h.Apply(nil, nil)
	if rv == nil {
		t.Fatal("Apply with empty args returned nil")
	}
	if len(rv.PartitioningValues) != 0 || len(rv.ArgumentValues) != 0 {
		t.Fatalf("Apply with empty args produced non-empty: p=%v a=%v",
			rv.PartitioningValues, rv.ArgumentValues)
	}
}
