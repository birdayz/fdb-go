package values

import (
	"math"
	"testing"
)

func TestDistanceOperator_String(t *testing.T) {
	t.Parallel()
	cases := map[DistanceOperator]string{
		DistanceEuclidean:       "euclidean_distance",
		DistanceEuclideanSquare: "euclidean_square_distance",
		DistanceCosine:          "cosine_distance",
		DistanceDotProduct:      "dot_product_distance",
		DistanceOperator(99):    "INVALID",
	}
	for op, want := range cases {
		if got := op.String(); got != want {
			t.Errorf("DistanceOperator(%d).String() = %q, want %q", op, got, want)
		}
	}
}

func TestDistanceValue_Type(t *testing.T) {
	t.Parallel()
	v := NewDistanceValue(DistanceEuclidean,
		LiteralValue([]float64{1, 0, 0}),
		LiteralValue([]float64{0, 1, 0}))
	if !v.Type().Equals(NotNullDouble) {
		t.Fatalf("Type = %v, want NotNullDouble", v.Type())
	}
}

func TestDistanceValue_Children(t *testing.T) {
	t.Parallel()
	left := LiteralValue([]float64{1, 0, 0})
	right := LiteralValue([]float64{0, 1, 0})
	v := NewDistanceValue(DistanceEuclidean, left, right)
	cs := v.Children()
	if len(cs) != 2 || cs[0] != left || cs[1] != right {
		t.Fatalf("Children = %v, want [left, right]", cs)
	}
}

func TestDistanceValue_Name(t *testing.T) {
	t.Parallel()
	v := NewDistanceValue(DistanceCosine,
		LiteralValue([]float64{1}),
		LiteralValue([]float64{1}))
	if got := v.Name(); got != "cosine_distance" {
		t.Fatalf("Name = %q, want cosine_distance", got)
	}
}

func TestDistanceValue_EuclideanDistance(t *testing.T) {
	t.Parallel()
	// distance([1,0,0], [0,1,0]) = sqrt(1+1+0) = sqrt(2) ≈ 1.4142
	v := NewDistanceValue(DistanceEuclidean,
		LiteralValue([]float64{1, 0, 0}),
		LiteralValue([]float64{0, 1, 0}))
	got, ok := v.Evaluate(nil).(float64)
	if !ok {
		t.Fatalf("Evaluate = %T, want float64", v.Evaluate(nil))
	}
	want := math.Sqrt(2)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("Evaluate = %v, want %v", got, want)
	}
}

func TestDistanceValue_EuclideanSquareDistance(t *testing.T) {
	t.Parallel()
	v := NewDistanceValue(DistanceEuclideanSquare,
		LiteralValue([]float64{1, 0, 0}),
		LiteralValue([]float64{0, 1, 0}))
	got, ok := v.Evaluate(nil).(float64)
	if !ok || got != 2.0 {
		t.Fatalf("Evaluate = %v, want 2.0", got)
	}
}

func TestDistanceValue_CosineDistance_Orthogonal(t *testing.T) {
	t.Parallel()
	// cosine_distance([1,0], [0,1]) = 1 - 0/(1*1) = 1.0 (orthogonal)
	v := NewDistanceValue(DistanceCosine,
		LiteralValue([]float64{1, 0}),
		LiteralValue([]float64{0, 1}))
	got, _ := v.Evaluate(nil).(float64)
	if math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("Evaluate orthogonal = %v, want 1.0", got)
	}
}

func TestDistanceValue_CosineDistance_Identical(t *testing.T) {
	t.Parallel()
	// cosine_distance([1,2,3], [1,2,3]) = 1 - dot/(norm*norm) = 0 (identical)
	v := NewDistanceValue(DistanceCosine,
		LiteralValue([]float64{1, 2, 3}),
		LiteralValue([]float64{1, 2, 3}))
	got, _ := v.Evaluate(nil).(float64)
	if math.Abs(got) > 1e-9 {
		t.Fatalf("Evaluate identical = %v, want ~0", got)
	}
}

func TestDistanceValue_CosineDistance_ZeroVector(t *testing.T) {
	t.Parallel()
	// cosine_distance with zero vector → 1.0 (degenerate fallback).
	v := NewDistanceValue(DistanceCosine,
		LiteralValue([]float64{0, 0, 0}),
		LiteralValue([]float64{1, 2, 3}))
	got, _ := v.Evaluate(nil).(float64)
	if got != 1.0 {
		t.Fatalf("Evaluate zero vector = %v, want 1.0", got)
	}
}

func TestDistanceValue_DotProductDistance(t *testing.T) {
	t.Parallel()
	// dot_product_distance([1,2,3], [4,5,6]) = -(4+10+18) = -32
	v := NewDistanceValue(DistanceDotProduct,
		LiteralValue([]float64{1, 2, 3}),
		LiteralValue([]float64{4, 5, 6}))
	got, _ := v.Evaluate(nil).(float64)
	if got != -32.0 {
		t.Fatalf("Evaluate = %v, want -32", got)
	}
}

func TestDistanceValue_DimensionMismatch(t *testing.T) {
	t.Parallel()
	v := NewDistanceValue(DistanceEuclidean,
		LiteralValue([]float64{1, 2, 3}),
		LiteralValue([]float64{1, 2}))
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate(dim mismatch) = %v, want nil", got)
	}
}

func TestDistanceValue_NilOperandReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewDistanceValue(DistanceEuclidean,
		LiteralValue(nil),
		LiteralValue([]float64{1, 2, 3}))
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate(nil left) = %v, want nil", got)
	}
}

func TestDistanceValue_NonVectorReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewDistanceValue(DistanceEuclidean,
		LiteralValue("not-a-vector"),
		LiteralValue([]float64{1}))
	if got := v.Evaluate(nil); got != nil {
		t.Fatalf("Evaluate(non-vector) = %v, want nil", got)
	}
}

func TestDistanceValue_AcceptsFloat32(t *testing.T) {
	t.Parallel()
	// The asFloat64Slice helper accepts []float32 too — Java's
	// RealVector.getData often returns float-precision; lift to
	// float64 for the metric math.
	v := NewDistanceValue(DistanceEuclideanSquare,
		LiteralValue([]float32{1, 0, 0}),
		LiteralValue([]float32{0, 1, 0}))
	got, _ := v.Evaluate(nil).(float64)
	if got != 2.0 {
		t.Fatalf("Evaluate(float32) = %v, want 2.0", got)
	}
}
