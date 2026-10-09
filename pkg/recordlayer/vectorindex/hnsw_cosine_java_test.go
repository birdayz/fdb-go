package vectorindex

import (
	"container/heap"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// TestCosineDistance_IsJavasCosineMetric pins MetricDefinition.CosineMetric:
// a zero vector is +Inf from every vector (checked first), a non-finite norm
// or dot product is NaN, and the similarity is not clamped. The byte-direct
// path agrees. SPFresh keeps its own cosine (zero at 1, clamped).
func TestCosineDistance_IsJavasCosineMetric(t *testing.T) {
	t.Parallel()
	inf := math.Inf(1)
	cases := []struct {
		name string
		a, b []float64
		want float64 // NaN means NaN
	}{
		{"zero query", []float64{0, 0}, []float64{1, 2}, inf},
		{"zero stored", []float64{1, 2}, []float64{0, 0}, inf},
		{"zero beats non-finite", []float64{0, 0}, []float64{inf, 1}, inf},
		{"infinite component", []float64{inf, 1}, []float64{1, 1}, math.NaN()},
		{"NaN component", []float64{math.NaN(), 1}, []float64{1, 1}, math.NaN()},
		{"orthogonal", []float64{1, 0}, []float64{0, 1}, 1},
		{"opposite", []float64{1, 0}, []float64{-1, 0}, 2},
	}
	for _, c := range cases {
		got := vectorDistance(c.a, c.b, VectorMetricCosine)
		bytesGot, ok := vectorDistanceFromBytes(c.a, vectorcodec.Serialize(c.b), VectorMetricCosine)
		if !ok {
			t.Fatalf("%s: byte path declined", c.name)
		}
		for _, d := range []float64{got, bytesGot} {
			if math.IsNaN(c.want) != math.IsNaN(d) || (!math.IsNaN(c.want) && d != c.want) {
				t.Errorf("%s: distance %v, want %v", c.name, d, c.want)
			}
		}
	}

	// No clamp: a similarity rounded above 1 gives a distance below 0.
	v := []float64{1.1, 2.3, 0.7}
	if got := cosineDistance(v, v); !(got < 0) {
		t.Errorf("self-distance %v, want the unclamped negative rounding (-2.2e-16)", got)
	}

	if got := spfreshVectorDistance([]float64{0, 0}, []float64{1, 2}, VectorMetricCosine); got != 1 {
		t.Errorf("SPFresh zero-vector cosine %v, want its own 1", got)
	}
	if got := spfreshVectorDistance(v, v, VectorMetricCosine); got < 0 {
		t.Errorf("SPFresh cosine %v, want it clamped at 0", got)
	}
}

// TestHnswDistanceOrder_IsDoubleCompare pins Java's NodeReferenceWithDistance
// order (Double.compare): NaN is the farthest and -0.0 precedes 0.0, in the
// candidate heap as everywhere HNSW compares distances.
func TestHnswDistanceOrder_IsDoubleCompare(t *testing.T) {
	t.Parallel()
	h := &distHeap{}
	for _, d := range []float64{math.NaN(), 3, math.Inf(1), 0, math.Copysign(0, -1), 1} {
		heap.Push(h, distItem{dist: d})
	}
	var got []float64
	for h.Len() > 0 {
		got = append(got, heap.Pop(h).(distItem).dist)
	}
	if !math.Signbit(got[0]) || got[1] != 0 || math.Signbit(got[1]) || got[2] != 1 || got[3] != 3 ||
		!math.IsInf(got[4], 1) || !math.IsNaN(got[5]) {
		t.Fatalf("heap order %v, want [-0 0 1 3 +Inf NaN]", got)
	}
}

// TestCentroidNodeOrder_IsDoubleCompareThenKey pins GuardiANN's centroid walk
// comparator (NodeReferenceWithDistance.comparator): Double.compare on the
// distance, so NaN is last and -0.0 precedes 0.0, then the primary key.
func TestCentroidNodeOrder_IsDoubleCompareThenKey(t *testing.T) {
	t.Parallel()
	negZero := math.Copysign(0, -1)
	cases := []struct {
		a, b centroidNode
		want bool
	}{
		{centroidNode{span: []byte{2}, distance: negZero}, centroidNode{span: []byte{1}, distance: 0}, true},
		{centroidNode{span: []byte{1}, distance: 0}, centroidNode{span: []byte{2}, distance: negZero}, false},
		{centroidNode{span: []byte{1}, distance: math.NaN()}, centroidNode{span: []byte{2}, distance: math.Inf(1)}, false},
		{centroidNode{span: []byte{2}, distance: math.Inf(1)}, centroidNode{span: []byte{1}, distance: math.NaN()}, true},
		{centroidNode{span: []byte{1}, distance: 3}, centroidNode{span: []byte{2}, distance: 3}, true},
	}
	for i, c := range cases {
		if got := lessCentroidNode(c.a, c.b); got != c.want {
			t.Errorf("case %d: less = %v, want %v", i, got, c.want)
		}
	}
}
