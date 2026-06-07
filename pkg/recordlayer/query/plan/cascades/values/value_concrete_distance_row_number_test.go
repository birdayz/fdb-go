package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// --- CosineDistanceRowNumberValue ---

func TestCosineDistanceRowNumberValue_Name(t *testing.T) {
	t.Parallel()
	v := NewCosineDistanceRowNumberValue(nil, nil)
	if got := v.Name(); got != "CosineDistanceRowNumber" {
		t.Fatalf("Name = %q, want CosineDistanceRowNumber", got)
	}
}

func TestCosineDistanceRowNumberValue_Type(t *testing.T) {
	t.Parallel()
	v := NewCosineDistanceRowNumberValue(nil, nil)
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", v.Type())
	}
}

func TestCosineDistanceRowNumberValue_IsIndexOnly(t *testing.T) {
	t.Parallel()
	v := NewCosineDistanceRowNumberValue(nil, nil)
	if !v.IsIndexOnly() {
		t.Fatal("IsIndexOnly = false, want true")
	}
}

func TestCosineDistanceRowNumberValue_EvaluateFromHarness(t *testing.T) {
	t.Parallel()
	v := NewCosineDistanceRowNumberValue(nil, nil)
	row := map[string]any{"_row_number": int64(3)}
	got, errEv0 := v.Evaluate(row)
	require.NoError(t, errEv0)
	if got != int64(3) {
		t.Fatalf("Evaluate = %v, want 3", got)
	}
}

func TestCosineDistanceRowNumberValue_EvaluateNilReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewCosineDistanceRowNumberValue(nil, nil)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate(nil) = %v, want nil", got)
	}
}

func TestCosineDistanceRowNumberValue_PartitionAndArgs(t *testing.T) {
	t.Parallel()
	p := LiteralValue("region")
	a1 := LiteralValue("vec")
	a2 := LiteralValue("query")
	v := NewCosineDistanceRowNumberValue([]Value{p}, []Value{a1, a2})
	if len(v.PartitioningValues) != 1 || v.PartitioningValues[0] != p {
		t.Fatalf("PartitioningValues = %v, want [p]", v.PartitioningValues)
	}
	if len(v.ArgumentValues) != 2 {
		t.Fatalf("ArgumentValues len = %d, want 2", len(v.ArgumentValues))
	}
}

func TestCosineDistanceRowNumberValue_Children(t *testing.T) {
	t.Parallel()
	p := LiteralValue("p")
	a := LiteralValue("a")
	v := NewCosineDistanceRowNumberValue([]Value{p}, []Value{a})
	children := v.Children()
	if len(children) != 2 {
		t.Fatalf("Children len = %d, want 2", len(children))
	}
	if children[0] != p || children[1] != a {
		t.Fatalf("Children = %v, want [p, a]", children)
	}
}

func TestCosineDistanceRowNumberValue_WithChildren(t *testing.T) {
	t.Parallel()
	v := NewCosineDistanceRowNumberValue([]Value{LiteralValue("p")}, []Value{LiteralValue("a")})
	newP := LiteralValue("P2")
	newA := LiteralValue("A2")
	rebuilt := v.WithChildren([]Value{newP, newA})
	if len(rebuilt.PartitioningValues) != 1 || rebuilt.PartitioningValues[0] != newP {
		t.Fatalf("rebuilt.PartitioningValues = %v, want [P2]", rebuilt.PartitioningValues)
	}
	if len(rebuilt.ArgumentValues) != 1 || rebuilt.ArgumentValues[0] != newA {
		t.Fatalf("rebuilt.ArgumentValues = %v, want [A2]", rebuilt.ArgumentValues)
	}
}

// --- DotProductDistanceRowNumberValue ---

func TestDotProductDistanceRowNumberValue_Name(t *testing.T) {
	t.Parallel()
	v := NewDotProductDistanceRowNumberValue(nil, nil)
	if got := v.Name(); got != "DotProductDistanceRowNumber" {
		t.Fatalf("Name = %q, want DotProductDistanceRowNumber", got)
	}
}

func TestDotProductDistanceRowNumberValue_Type(t *testing.T) {
	t.Parallel()
	v := NewDotProductDistanceRowNumberValue(nil, nil)
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", v.Type())
	}
}

func TestDotProductDistanceRowNumberValue_IsIndexOnly(t *testing.T) {
	t.Parallel()
	v := NewDotProductDistanceRowNumberValue(nil, nil)
	if !v.IsIndexOnly() {
		t.Fatal("IsIndexOnly = false, want true")
	}
}

func TestDotProductDistanceRowNumberValue_EvaluateFromHarness(t *testing.T) {
	t.Parallel()
	v := NewDotProductDistanceRowNumberValue(nil, nil)
	row := map[string]any{"_row_number": int64(5)}
	got, errEv0 := v.Evaluate(row)
	require.NoError(t, errEv0)
	if got != int64(5) {
		t.Fatalf("Evaluate = %v, want 5", got)
	}
}

func TestDotProductDistanceRowNumberValue_EvaluateNilReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewDotProductDistanceRowNumberValue(nil, nil)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate(nil) = %v, want nil", got)
	}
}

func TestDotProductDistanceRowNumberValue_PartitionAndArgs(t *testing.T) {
	t.Parallel()
	p := LiteralValue("region")
	a := LiteralValue("vec")
	v := NewDotProductDistanceRowNumberValue([]Value{p}, []Value{a})
	if len(v.PartitioningValues) != 1 {
		t.Fatalf("PartitioningValues len = %d, want 1", len(v.PartitioningValues))
	}
	if len(v.ArgumentValues) != 1 {
		t.Fatalf("ArgumentValues len = %d, want 1", len(v.ArgumentValues))
	}
}

func TestDotProductDistanceRowNumberValue_WithChildren(t *testing.T) {
	t.Parallel()
	v := NewDotProductDistanceRowNumberValue([]Value{LiteralValue("p")}, []Value{LiteralValue("a")})
	newP := LiteralValue("P2")
	newA := LiteralValue("A2")
	rebuilt := v.WithChildren([]Value{newP, newA})
	if len(rebuilt.PartitioningValues) != 1 || rebuilt.PartitioningValues[0] != newP {
		t.Fatalf("rebuilt.PartitioningValues = %v, want [P2]", rebuilt.PartitioningValues)
	}
	if len(rebuilt.ArgumentValues) != 1 || rebuilt.ArgumentValues[0] != newA {
		t.Fatalf("rebuilt.ArgumentValues = %v, want [A2]", rebuilt.ArgumentValues)
	}
}

// --- EuclideanDistanceRowNumberValue ---

func TestEuclideanDistanceRowNumberValue_Name(t *testing.T) {
	t.Parallel()
	v := NewEuclideanDistanceRowNumberValue(nil, nil)
	if got := v.Name(); got != "EuclideanDistanceRowNumber" {
		t.Fatalf("Name = %q, want EuclideanDistanceRowNumber", got)
	}
}

func TestEuclideanDistanceRowNumberValue_Type(t *testing.T) {
	t.Parallel()
	v := NewEuclideanDistanceRowNumberValue(nil, nil)
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", v.Type())
	}
}

func TestEuclideanDistanceRowNumberValue_IsIndexOnly(t *testing.T) {
	t.Parallel()
	v := NewEuclideanDistanceRowNumberValue(nil, nil)
	if !v.IsIndexOnly() {
		t.Fatal("IsIndexOnly = false, want true")
	}
}

func TestEuclideanDistanceRowNumberValue_EvaluateFromHarness(t *testing.T) {
	t.Parallel()
	v := NewEuclideanDistanceRowNumberValue(nil, nil)
	row := map[string]any{"_row_number": int64(1)}
	got, errEv0 := v.Evaluate(row)
	require.NoError(t, errEv0)
	if got != int64(1) {
		t.Fatalf("Evaluate = %v, want 1", got)
	}
}

func TestEuclideanDistanceRowNumberValue_EvaluateNilReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewEuclideanDistanceRowNumberValue(nil, nil)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate(nil) = %v, want nil", got)
	}
}

func TestEuclideanDistanceRowNumberValue_PartitionAndArgs(t *testing.T) {
	t.Parallel()
	p := LiteralValue("region")
	a1 := LiteralValue("vec")
	a2 := LiteralValue("query")
	v := NewEuclideanDistanceRowNumberValue([]Value{p}, []Value{a1, a2})
	if len(v.PartitioningValues) != 1 || v.PartitioningValues[0] != p {
		t.Fatalf("PartitioningValues = %v, want [p]", v.PartitioningValues)
	}
	if len(v.ArgumentValues) != 2 {
		t.Fatalf("ArgumentValues len = %d, want 2", len(v.ArgumentValues))
	}
}

func TestEuclideanDistanceRowNumberValue_WithChildren(t *testing.T) {
	t.Parallel()
	v := NewEuclideanDistanceRowNumberValue([]Value{LiteralValue("p")}, []Value{LiteralValue("a")})
	newP := LiteralValue("P2")
	newA := LiteralValue("A2")
	rebuilt := v.WithChildren([]Value{newP, newA})
	if len(rebuilt.PartitioningValues) != 1 || rebuilt.PartitioningValues[0] != newP {
		t.Fatalf("rebuilt.PartitioningValues = %v, want [P2]", rebuilt.PartitioningValues)
	}
	if len(rebuilt.ArgumentValues) != 1 || rebuilt.ArgumentValues[0] != newA {
		t.Fatalf("rebuilt.ArgumentValues = %v, want [A2]", rebuilt.ArgumentValues)
	}
}

// --- EuclideanSquareDistanceRowNumberValue ---

func TestEuclideanSquareDistanceRowNumberValue_Name(t *testing.T) {
	t.Parallel()
	v := NewEuclideanSquareDistanceRowNumberValue(nil, nil)
	if got := v.Name(); got != "EuclideanSquareDistanceRowNumber" {
		t.Fatalf("Name = %q, want EuclideanSquareDistanceRowNumber", got)
	}
}

func TestEuclideanSquareDistanceRowNumberValue_Type(t *testing.T) {
	t.Parallel()
	v := NewEuclideanSquareDistanceRowNumberValue(nil, nil)
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", v.Type())
	}
}

func TestEuclideanSquareDistanceRowNumberValue_IsIndexOnly(t *testing.T) {
	t.Parallel()
	v := NewEuclideanSquareDistanceRowNumberValue(nil, nil)
	if !v.IsIndexOnly() {
		t.Fatal("IsIndexOnly = false, want true")
	}
}

func TestEuclideanSquareDistanceRowNumberValue_EvaluateFromHarness(t *testing.T) {
	t.Parallel()
	v := NewEuclideanSquareDistanceRowNumberValue(nil, nil)
	row := map[string]any{"_row_number": int64(42)}
	got, errEv0 := v.Evaluate(row)
	require.NoError(t, errEv0)
	if got != int64(42) {
		t.Fatalf("Evaluate = %v, want 42", got)
	}
}

func TestEuclideanSquareDistanceRowNumberValue_EvaluateNilReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewEuclideanSquareDistanceRowNumberValue(nil, nil)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate(nil) = %v, want nil", got)
	}
}

func TestEuclideanSquareDistanceRowNumberValue_PartitionAndArgs(t *testing.T) {
	t.Parallel()
	p := LiteralValue("region")
	a := LiteralValue("vec")
	v := NewEuclideanSquareDistanceRowNumberValue([]Value{p}, []Value{a})
	if len(v.PartitioningValues) != 1 || v.PartitioningValues[0] != p {
		t.Fatalf("PartitioningValues = %v, want [p]", v.PartitioningValues)
	}
	if len(v.ArgumentValues) != 1 {
		t.Fatalf("ArgumentValues len = %d, want 1", len(v.ArgumentValues))
	}
}

func TestEuclideanSquareDistanceRowNumberValue_WithChildren(t *testing.T) {
	t.Parallel()
	v := NewEuclideanSquareDistanceRowNumberValue([]Value{LiteralValue("p")}, []Value{LiteralValue("a")})
	newP := LiteralValue("P2")
	newA := LiteralValue("A2")
	rebuilt := v.WithChildren([]Value{newP, newA})
	if len(rebuilt.PartitioningValues) != 1 || rebuilt.PartitioningValues[0] != newP {
		t.Fatalf("rebuilt.PartitioningValues = %v, want [P2]", rebuilt.PartitioningValues)
	}
	if len(rebuilt.ArgumentValues) != 1 || rebuilt.ArgumentValues[0] != newA {
		t.Fatalf("rebuilt.ArgumentValues = %v, want [A2]", rebuilt.ArgumentValues)
	}
}

// --- Evaluate with missing key returns nil ---

func TestConcreteDistanceRowNumber_EvaluateNoKeyReturnsNil(t *testing.T) {
	t.Parallel()
	row := map[string]any{"other_key": int64(9)}
	types := []Value{
		NewCosineDistanceRowNumberValue(nil, nil),
		NewDotProductDistanceRowNumberValue(nil, nil),
		NewEuclideanDistanceRowNumberValue(nil, nil),
		NewEuclideanSquareDistanceRowNumberValue(nil, nil),
	}
	for _, v := range types {
		got, errEv0 := v.Evaluate(row)
		require.NoError(t, errEv0)
		if got != nil {
			t.Fatalf("%s: Evaluate(no _row_number key) = %v, want nil", v.Name(), got)
		}
	}
}

// --- Evaluate with wrong context type returns nil ---

func TestConcreteDistanceRowNumber_EvaluateWrongCtxType(t *testing.T) {
	t.Parallel()
	types := []Value{
		NewCosineDistanceRowNumberValue(nil, nil),
		NewDotProductDistanceRowNumberValue(nil, nil),
		NewEuclideanDistanceRowNumberValue(nil, nil),
		NewEuclideanSquareDistanceRowNumberValue(nil, nil),
	}
	for _, v := range types {
		got, errEv0 := v.Evaluate("not-a-map")
		require.NoError(t, errEv0)
		if got != nil {
			t.Fatalf("%s: Evaluate(string ctx) = %v, want nil", v.Name(), got)
		}
	}
}

// --- IndexOnly interface satisfaction ---

func TestConcreteDistanceRowNumber_IndexOnlyInterface(t *testing.T) {
	t.Parallel()
	// Compile-time checks are in the source files (var _ IndexOnly = ...),
	// but verify at runtime too.
	types := []IndexOnly{
		NewCosineDistanceRowNumberValue(nil, nil),
		NewDotProductDistanceRowNumberValue(nil, nil),
		NewEuclideanDistanceRowNumberValue(nil, nil),
		NewEuclideanSquareDistanceRowNumberValue(nil, nil),
	}
	for _, v := range types {
		if !v.IsIndexOnly() {
			t.Fatalf("%s: IsIndexOnly = false", v.Name())
		}
	}
}
