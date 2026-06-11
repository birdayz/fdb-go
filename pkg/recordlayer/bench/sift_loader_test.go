package bench

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// LoadFVecs reads vectors from the fvecs binary format.
// Format: [dim: int32LE] [component_0: float32LE] ... [component_{dim-1}: float32LE]
// Repeated for each vector, no file header.
// If maxVectors <= 0, all vectors are loaded.
func LoadFVecs(path string, maxVectors int) ([][]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open fvecs file: %w", err)
	}
	defer f.Close()

	var vectors [][]float32
	for maxVectors <= 0 || len(vectors) < maxVectors {
		var dim int32
		if err := binary.Read(f, binary.LittleEndian, &dim); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read fvecs dim at vector %d: %w", len(vectors), err)
		}
		if dim <= 0 || dim > 10000 {
			return nil, fmt.Errorf("invalid fvecs dimension %d at vector %d", dim, len(vectors))
		}

		raw := make([]float32, dim)
		if err := binary.Read(f, binary.LittleEndian, &raw); err != nil {
			return nil, fmt.Errorf("read fvecs data at vector %d: %w", len(vectors), err)
		}
		vectors = append(vectors, raw)
	}

	return vectors, nil
}

// LoadIVecs reads integer vectors from ivecs format (same layout as fvecs but
// with int32 components). Used for ground truth kNN indices.
// If maxVectors <= 0, all vectors are loaded.
func LoadIVecs(path string, maxVectors int) ([][]int32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open ivecs file: %w", err)
	}
	defer f.Close()

	var vectors [][]int32
	for maxVectors <= 0 || len(vectors) < maxVectors {
		var dim int32
		if err := binary.Read(f, binary.LittleEndian, &dim); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read ivecs dim at vector %d: %w", len(vectors), err)
		}
		if dim <= 0 || dim > 10000 {
			return nil, fmt.Errorf("invalid ivecs dimension %d at vector %d", dim, len(vectors))
		}

		raw := make([]int32, dim)
		if err := binary.Read(f, binary.LittleEndian, &raw); err != nil {
			return nil, fmt.Errorf("read ivecs data at vector %d: %w", len(vectors), err)
		}
		vectors = append(vectors, raw)
	}

	return vectors, nil
}

// float32sToFloat64s converts a float32 vector to float64.
func float32sToFloat64s(v []float32) []float64 {
	out := make([]float64, len(v))
	for i, f := range v {
		out[i] = float64(f)
	}
	return out
}

// siftRecallAtK computes recall@k: the fraction of ground-truth nearest neighbors
// that appear in the search results.
func siftRecallAtK(searchResults []recordlayer.VectorSearchResult, groundTruth []int32, k int) float64 {
	gtSet := make(map[int64]bool, k)
	for i := 0; i < k && i < len(groundTruth); i++ {
		gtSet[int64(groundTruth[i])] = true
	}
	hits := 0
	for i := 0; i < k && i < len(searchResults); i++ {
		pk := extractPKInt64(searchResults[i].PrimaryKey)
		if gtSet[pk] {
			hits++
		}
	}
	denom := k
	if len(groundTruth) < denom {
		denom = len(groundTruth)
	}
	if denom == 0 {
		return 0
	}
	return float64(hits) / float64(denom)
}

// siftBruteForceRecall computes recall by comparing HNSW results against
// brute-force ground truth computed from actual float64 vectors.
// This is used as a sanity check when ground truth file may not align with
// our subset (e.g., ground truth was computed against all 1M vectors but we
// only indexed n < 1M).
func siftBruteForceRecall(searchResults []recordlayer.VectorSearchResult, queryVec []float64, allVecs [][]float64, k int) float64 {
	expected := vecBruteForceKNN(queryVec, allVecs, k)
	expectedSet := make(map[int64]bool, len(expected))
	for _, id := range expected {
		expectedSet[id] = true
	}
	hits := 0
	for i := 0; i < k && i < len(searchResults); i++ {
		pk := extractPKInt64(searchResults[i].PrimaryKey)
		if pk >= 0 && expectedSet[pk] {
			hits++
		}
	}
	if len(expected) == 0 {
		return 0
	}
	return float64(hits) / float64(len(expected))
}

// siftValidateGroundTruth checks if ground truth IDs are within the indexed range.
// Returns true if the ground truth was computed against the full 1M dataset and
// our subset is smaller, meaning we should use brute-force recall instead.
func siftValidateGroundTruth(groundTruth [][]int32, n int) bool {
	for _, gt := range groundTruth {
		for _, id := range gt {
			if int(id) >= n {
				return true // ground truth references vectors beyond our subset
			}
		}
	}
	return false
}

// siftLatencyPercentile returns the percentile latency from sorted durations.
// This is just a type-alias wrapper for consistency with the SIFT benchmark output.
var siftLatencyPercentile = vecLatencyPercentile

// serializeVectorF32 converts a float32 SIFT vector to the storage format.
// Our HNSW uses float64, so we convert float32 -> float64 -> serialized bytes.
func serializeVectorF32(vec []float32) []byte {
	f64 := float32sToFloat64s(vec)
	return serializeVector(f64)
}

// deserializeToFloat64 extracts float64 values from a serialized vector
// (type ordinal 2 = DOUBLE). Used by unit tests.
func deserializeToFloat64(data []byte) ([]float64, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("empty vector data")
	}
	if data[0] != 2 {
		return nil, fmt.Errorf("unsupported vector type ordinal: %d", data[0])
	}
	nFloats := (len(data) - 1) / 8
	result := make([]float64, nFloats)
	for i := 0; i < nFloats; i++ {
		bits := binary.BigEndian.Uint64(data[1+i*8:])
		result[i] = math.Float64frombits(bits)
	}
	return result, nil
}

// synthesizeClusteredVectors generates a deterministic SIFT-shaped Gaussian
// mixture for scales past the SIFT-1M file (the 10M soak): m = n/5000
// cluster centers (≥64) uniform in the SIFT byte range, points = center +
// N(0, 12) clamped to [0, 255], queries drawn from the same mixture. Base
// vectors are produced as float64 directly (no float32 intermediate — at
// 10M × 128-D the intermediate alone is 5 GB).
func synthesizeClusteredVectors(n, numQueries, dims int, seed int64) ([][]float64, [][]float32) {
	rng := rand.New(rand.NewSource(seed))
	m := n / 5000
	if m < 64 {
		m = 64
	}
	centers := make([][]float64, m)
	for i := range centers {
		c := make([]float64, dims)
		for d := range c {
			c[d] = rng.Float64() * 255
		}
		centers[i] = c
	}
	point := func(out []float64) {
		c := centers[rng.Intn(m)]
		for d := range out {
			v := c[d] + rng.NormFloat64()*12
			if v < 0 {
				v = 0
			} else if v > 255 {
				v = 255
			}
			out[d] = v
		}
	}
	base := make([][]float64, n)
	flat := make([]float64, n*dims) // one backing array: allocator-friendly at 10M
	for i := range base {
		base[i] = flat[i*dims : (i+1)*dims : (i+1)*dims]
		point(base[i])
	}
	queries := make([][]float32, numQueries)
	qbuf := make([]float64, dims)
	for i := range queries {
		point(qbuf)
		q := make([]float32, dims)
		for d, v := range qbuf {
			q[d] = float32(v)
		}
		queries[i] = q
	}
	return base, queries
}
