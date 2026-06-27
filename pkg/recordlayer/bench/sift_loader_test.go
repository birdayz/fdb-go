package bench

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"sync"

	"fdb.dev/pkg/recordlayer"
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

// vectorSource supplies base vectors by index WITHOUT requiring the dataset
// to be materialized: the synthetic source regenerates vector i
// deterministically on demand. At 10M × 128-D the materialized float64
// dataset alone is 10.24 GB and OOM-killed the harness twice — the
// deterministic generator IS the dataset.
type vectorSource interface {
	at(i int, out []float64)
	size() int
	dimensions() int
}

// sliceSource wraps an already-loaded dataset (the SIFT files).
type sliceSource struct{ base [][]float64 }

func (s sliceSource) at(i int, out []float64) { copy(out, s.base[i]) }
func (s sliceSource) size() int               { return len(s.base) }
func (s sliceSource) dimensions() int         { return len(s.base[0]) }

// synthSource regenerates Gaussian-mixture vectors per index: only the
// cluster centers are materialized (n/5000 × dims — 2 MB at 10M).
type synthSource struct {
	seed    int64
	centers [][]float64
	n, dims int
}

func newSynthSource(n, dims int, seed int64) synthSource {
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
	return synthSource{seed: seed, centers: centers, n: n, dims: dims}
}

func (s synthSource) at(i int, out []float64) {
	rng := rand.New(rand.NewSource(s.seed ^ (int64(i)+1)*-0x61C8864680B583EB))
	c := s.centers[rng.Intn(len(s.centers))]
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
func (s synthSource) size() int       { return s.n }
func (s synthSource) dimensions() int { return s.dims }

// queriesFromSource draws numQueries query vectors from the same
// distribution, deterministically disjoint from the base index space.
func queriesFromSource(src vectorSource, numQueries int, seed int64) [][]float32 {
	queries := make([][]float32, numQueries)
	buf := make([]float64, src.dimensions())
	switch s := src.(type) {
	case synthSource:
		q := synthSource{seed: s.seed ^ 0x5DEECE66D, centers: s.centers, n: numQueries, dims: s.dims}
		for i := range queries {
			q.at(i, buf)
			queries[i] = float64sToFloat32s(buf)
		}
	default:
		panic("queriesFromSource: only synthetic sources draw queries")
	}
	return queries
}

func float64sToFloat32s(v []float64) []float32 {
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(x)
	}
	return out
}

// bruteForceIDsStreaming computes exact top-k for ALL queries in ONE pass
// over the source, parallel over index ranges with deterministic merges —
// O(queries × k) memory regardless of n.
func bruteForceIDsStreaming(src vectorSource, queries [][]float64, k int) [][]int64 {
	type hit struct {
		id int64
		d  float64
	}
	workers := runtime.NumCPU()
	if workers > 16 {
		workers = 16
	}
	n := src.size()
	chunk := (n + workers - 1) / workers
	partials := make([][][]hit, workers) // worker → query → sorted top-k
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lo, hi := w*chunk, min((w+1)*chunk, n)
			tops := make([][]hit, len(queries))
			buf := make([]float64, src.dimensions())
			for i := lo; i < hi; i++ {
				src.at(i, buf)
				for qi, q := range queries {
					var d float64
					for j := range q {
						diff := q[j] - buf[j]
						d += diff * diff
					}
					top := tops[qi]
					if len(top) == k && d >= top[k-1].d {
						continue
					}
					pos := sort.Search(len(top), func(x int) bool {
						if top[x].d != d {
							return d < top[x].d
						}
						return int64(i) < top[x].id
					})
					top = append(top, hit{})
					copy(top[pos+1:], top[pos:])
					top[pos] = hit{id: int64(i), d: d}
					if len(top) > k {
						top = top[:k]
					}
					tops[qi] = top
				}
			}
			partials[w] = tops
		}(w)
	}
	wg.Wait()
	out := make([][]int64, len(queries))
	for qi := range queries {
		var merged []hit
		for w := 0; w < workers; w++ {
			if partials[w] == nil {
				continue
			}
			merged = append(merged, partials[w][qi]...)
		}
		sort.Slice(merged, func(a, b int) bool {
			if merged[a].d != merged[b].d {
				return merged[a].d < merged[b].d
			}
			return merged[a].id < merged[b].id
		})
		if len(merged) > k {
			merged = merged[:k]
		}
		ids := make([]int64, len(merged))
		for x, h := range merged {
			ids[x] = h.id
		}
		out[qi] = ids
	}
	return out
}
