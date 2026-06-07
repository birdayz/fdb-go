package values

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDistanceRowNumberValue_Type(t *testing.T) {
	t.Parallel()
	v := NewDistanceRowNumberValue(DistanceEuclidean, nil, nil, nil, nil)
	if !v.Type().Equals(NotNullLong) {
		t.Fatalf("Type = %v, want NotNullLong", v.Type())
	}
}

func TestDistanceRowNumberValue_IsIndexOnly(t *testing.T) {
	t.Parallel()
	v := NewDistanceRowNumberValue(DistanceEuclidean, nil, nil, nil, nil)
	if !v.IsIndexOnly() {
		t.Fatalf("IsIndexOnly = false, want true (K-NN row-number is index-only)")
	}
}

func TestDistanceRowNumberValue_NamePerMetric(t *testing.T) {
	t.Parallel()
	cases := map[DistanceOperator]string{
		DistanceEuclidean:       "euclidean_distance_row_number",
		DistanceEuclideanSquare: "euclidean_square_distance_row_number",
		DistanceCosine:          "cosine_distance_row_number",
		DistanceDotProduct:      "dot_product_distance_row_number",
	}
	for metric, want := range cases {
		v := NewDistanceRowNumberValue(metric, nil, nil, nil, nil)
		if got := v.Name(); got != want {
			t.Errorf("Metric=%v Name = %q, want %q", metric, got, want)
		}
	}
}

func TestDistanceRowNumberValue_PartitionPreserved(t *testing.T) {
	t.Parallel()
	p1 := LiteralValue("region")
	v := NewDistanceRowNumberValue(DistanceEuclidean, []Value{p1}, nil, nil, nil)
	if len(v.PartitioningValues) != 1 || v.PartitioningValues[0] != p1 {
		t.Fatalf("PartitioningValues = %v, want [p1]", v.PartitioningValues)
	}
}

func TestDistanceRowNumberValue_ArgumentsPreserved(t *testing.T) {
	t.Parallel()
	a1 := LiteralValue("vec_field")
	a2 := LiteralValue("query_vec")
	v := NewDistanceRowNumberValue(DistanceCosine, nil, []Value{a1, a2}, nil, nil)
	if len(v.ArgumentValues) != 2 {
		t.Fatalf("ArgumentValues = %d, want 2", len(v.ArgumentValues))
	}
}

func TestDistanceRowNumberValue_HNSWConfigCarried(t *testing.T) {
	t.Parallel()
	ef := 100
	rv := true
	v := NewDistanceRowNumberValue(DistanceCosine, nil, nil, &ef, &rv)
	if v.EfSearch == nil || *v.EfSearch != 100 {
		t.Fatalf("EfSearch = %v, want 100", v.EfSearch)
	}
	if v.IsReturningVectors == nil || *v.IsReturningVectors != true {
		t.Fatalf("IsReturningVectors = %v, want true", v.IsReturningVectors)
	}
}

func TestDistanceRowNumberValue_EvaluateFromHarness(t *testing.T) {
	t.Parallel()
	v := NewDistanceRowNumberValue(DistanceEuclidean, nil, nil, nil, nil)
	row := map[string]any{"_row_number": int64(7)}
	got, errEv0 := v.Evaluate(row)
	require.NoError(t, errEv0)
	if got != int64(7) {
		t.Fatalf("Evaluate = %v, want 7", got)
	}
}

func TestDistanceRowNumberValue_EvaluateNilCtxReturnsNil(t *testing.T) {
	t.Parallel()
	v := NewDistanceRowNumberValue(DistanceEuclidean, nil, nil, nil, nil)
	got, errEv0 := v.Evaluate(nil)
	require.NoError(t, errEv0)
	if got != nil {
		t.Fatalf("Evaluate(nil) = %v, want nil", got)
	}
}

// The per-metric concrete types (CosineDistanceRowNumberValue, etc.)
// are tested in value_concrete_distance_row_number_test.go.

func TestDistanceRowNumberValue_WithChildrenPreservesMetricAndConfig(t *testing.T) {
	t.Parallel()
	ef := 50
	original := NewDistanceRowNumberValue(DistanceCosine,
		[]Value{LiteralValue("p")},
		[]Value{LiteralValue("a")},
		&ef, nil)

	newP := LiteralValue("P")
	newA := LiteralValue("A")
	rebuilt := original.WithChildren([]Value{newP, newA})

	if rebuilt.Metric != DistanceCosine {
		t.Fatalf("rebuilt.Metric = %v, want DistanceCosine (carried through)", rebuilt.Metric)
	}
	if rebuilt.EfSearch == nil || *rebuilt.EfSearch != 50 {
		t.Fatalf("rebuilt.EfSearch = %v, want 50 (carried through)", rebuilt.EfSearch)
	}
	if len(rebuilt.PartitioningValues) != 1 || rebuilt.PartitioningValues[0] != newP {
		t.Fatalf("rebuilt.PartitioningValues = %v, want [newP]", rebuilt.PartitioningValues)
	}
	if len(rebuilt.ArgumentValues) != 1 || rebuilt.ArgumentValues[0] != newA {
		t.Fatalf("rebuilt.ArgumentValues = %v, want [newA]", rebuilt.ArgumentValues)
	}
}
