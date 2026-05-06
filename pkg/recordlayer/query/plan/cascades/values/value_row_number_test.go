package values

import "testing"

func TestRowNumberValue_Type(t *testing.T) {
	t.Parallel()
	r := NewRowNumberValue(nil, nil, nil, nil)
	if !r.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", r.Type())
	}
}

func TestRowNumberValue_Name(t *testing.T) {
	t.Parallel()
	r := NewRowNumberValue(nil, nil, nil, nil)
	if got := r.Name(); got != "ROW_NUMBER" {
		t.Fatalf("Name = %q, want ROW_NUMBER", got)
	}
}

func TestRowNumberValue_IsIndexOnly(t *testing.T) {
	t.Parallel()
	r := NewRowNumberValue(nil, nil, nil, nil)
	if !r.IsIndexOnly() {
		t.Fatalf("IsIndexOnly = false, want true (Java contract: ROW_NUMBER is index-only)")
	}
}

func TestRowNumberValue_PartitioningPreserved(t *testing.T) {
	t.Parallel()
	p1 := LiteralValue("region")
	r := NewRowNumberValue([]Value{p1}, nil, nil, nil)
	if len(r.PartitioningValues) != 1 || r.PartitioningValues[0] != p1 {
		t.Fatalf("PartitioningValues = %v, want [p1]", r.PartitioningValues)
	}
}

func TestRowNumberValue_ArgumentsPreserved(t *testing.T) {
	t.Parallel()
	a1 := LiteralValue("distance")
	r := NewRowNumberValue(nil, []Value{a1}, nil, nil)
	if len(r.ArgumentValues) != 1 || r.ArgumentValues[0] != a1 {
		t.Fatalf("ArgumentValues = %v, want [a1]", r.ArgumentValues)
	}
}

func TestRowNumberValue_HNSWConfigPreserved(t *testing.T) {
	t.Parallel()
	ef := 100
	rv := true
	r := NewRowNumberValue(nil, nil, &ef, &rv)
	if r.EfSearch == nil || *r.EfSearch != 100 {
		t.Fatalf("EfSearch = %v, want 100", r.EfSearch)
	}
	if r.IsReturningVectors == nil || *r.IsReturningVectors != true {
		t.Fatalf("IsReturningVectors = %v, want true", r.IsReturningVectors)
	}
}

func TestRowNumberValue_NilHNSWDefaults(t *testing.T) {
	t.Parallel()
	r := NewRowNumberValue(nil, nil, nil, nil)
	if r.EfSearch != nil {
		t.Fatalf("EfSearch = %v, want nil (default)", r.EfSearch)
	}
	if r.IsReturningVectors != nil {
		t.Fatalf("IsReturningVectors = %v, want nil (default)", r.IsReturningVectors)
	}
}

func TestRowNumberValue_EvaluateFromHarness(t *testing.T) {
	t.Parallel()
	r := NewRowNumberValue(nil, nil, nil, nil)
	row := map[string]any{"_row_number": int64(42)}
	if got := r.Evaluate(row); got != int64(42) {
		t.Fatalf("Evaluate = %v, want 42", got)
	}
}

func TestRowNumberValue_EvaluateMissingKeyReturnsNil(t *testing.T) {
	t.Parallel()
	r := NewRowNumberValue(nil, nil, nil, nil)
	if got := r.Evaluate(map[string]any{"x": int64(99)}); got != nil {
		t.Fatalf("Evaluate(no _row_number) = %v, want nil", got)
	}
}

func TestRowNumberValue_EvaluateNilCtxReturnsNil(t *testing.T) {
	t.Parallel()
	r := NewRowNumberValue(nil, nil, nil, nil)
	if got := r.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate(nil) = %v, want nil", got)
	}
}

func TestRowNumberValue_WithChildrenSplitsByPosition(t *testing.T) {
	t.Parallel()
	p1 := LiteralValue("p")
	a1 := LiteralValue("a")
	ef := 50
	original := NewRowNumberValue([]Value{p1}, []Value{a1}, &ef, nil)

	newP := LiteralValue("P")
	newA := LiteralValue("A")
	rebuilt := original.WithChildren([]Value{newP, newA})

	if len(rebuilt.PartitioningValues) != 1 || rebuilt.PartitioningValues[0] != newP {
		t.Fatalf("rebuilt.PartitioningValues = %v, want [newP]", rebuilt.PartitioningValues)
	}
	if len(rebuilt.ArgumentValues) != 1 || rebuilt.ArgumentValues[0] != newA {
		t.Fatalf("rebuilt.ArgumentValues = %v, want [newA]", rebuilt.ArgumentValues)
	}
	// HNSW config carries through.
	if rebuilt.EfSearch == nil || *rebuilt.EfSearch != 50 {
		t.Fatalf("rebuilt.EfSearch = %v, want 50 (carried through)", rebuilt.EfSearch)
	}
}
