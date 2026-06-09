package bench

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/testcontainers/testcontainers-go"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// --- Self-contained FDB init for the standalone manual target ---

var (
	vectorBenchDB     *recordlayer.FDBDatabase
	vectorBenchDBOnce sync.Once
)

// ensureVectorBenchDB initializes the FDB database for vector benchmarks.
// Starts its own FDB testcontainer. Safe to call multiple times (sync.Once).
func ensureVectorBenchDB(tb testing.TB) {
	tb.Helper()
	vectorBenchDBOnce.Do(func() {
		if vectorBenchDB != nil {
			return
		}
		setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer setupCancel()
		container, err := foundationdbtc.Run(setupCtx, "",
			foundationdbtc.WithAPIVersion(720),
		)
		if err != nil {
			tb.Fatalf("failed to start FDB container: %v", err)
		}
		clusterFile, err := container.ClusterFile(setupCtx)
		if err != nil {
			tb.Fatalf("failed to get cluster file: %v", err)
		}
		tmpFile, err := os.CreateTemp("", "fdb_vecbench_*.txt")
		if err != nil {
			tb.Fatalf("failed to create temp file: %v", err)
		}
		if _, err := tmpFile.WriteString(clusterFile); err != nil {
			tb.Fatalf("failed to write cluster file: %v", err)
		}
		tmpFile.Close()
		fdb.MustAPIVersion(720)
		dbConn, err := fdb.OpenDatabase(tmpFile.Name())
		if err != nil {
			tb.Fatalf("failed to open FDB: %v", err)
		}
		vectorBenchDB = recordlayer.NewFDBDatabase(dbConn)
	})
	if vectorBenchDB == nil {
		tb.Fatal("vectorBenchDB initialization failed")
	}
}

// --- Configuration via environment variables ---

func vecEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

func vecEnvBool(key string, defaultVal bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return defaultVal
}

// --- Helpers ---

// vecRandomVector generates a random float64 vector of the given dimensions
// using the provided PRNG source for reproducibility.
func vecRandomVector(rng *rand.Rand, dims int) []float64 {
	v := make([]float64, dims)
	for i := range v {
		v[i] = rng.NormFloat64()
	}
	return v
}

// extractPKInt64 extracts an int64 primary key from a tuple.Tuple, handling nested tuples.
func extractPKInt64(pk tuple.Tuple) int64 {
	if len(pk) == 0 {
		return -1
	}
	switch v := pk[0].(type) {
	case int64:
		return v
	case tuple.Tuple:
		if len(v) > 0 {
			if id, ok := v[0].(int64); ok {
				return id
			}
		}
	}
	return -1
}

// vecBruteForceKNN computes exact k nearest neighbors by scanning all vectors.
// Returns order_id values (int64) sorted by distance (ascending).
func vecBruteForceKNN(query []float64, vectors [][]float64, k int) []int64 {
	type distIdx struct {
		dist float64
		id   int64
	}
	dists := make([]distIdx, len(vectors))
	for i, v := range vectors {
		dists[i] = distIdx{dist: euclideanDistance(query, v), id: int64(i)}
	}
	sort.Slice(dists, func(a, b int) bool { return dists[a].dist < dists[b].dist })
	if k > len(dists) {
		k = len(dists)
	}
	result := make([]int64, k)
	for i := 0; i < k; i++ {
		result[i] = dists[i].id
	}
	return result
}

// vecBenchSubspace returns a unique subspace for the given benchmark or test.
func vecBenchSubspace(name string) subspace.Subspace {
	return subspace.FromBytes(tuple.Tuple{name, time.Now().UnixNano()}.Pack())
}

// vecBuildMetaData builds metadata with a VECTOR index on Order.vector_data.
// Uses KWV(recordlayer.Field("vector_data"), 0) — all vectors in one HNSW graph (no prefix grouping).
func vecBuildMetaData(dims int, useRaBitQ bool) (*recordlayer.RecordMetaData, *recordlayer.Index) {
	vecIdx := recordlayer.NewVectorIndex("vec_data",
		recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0), dims)
	if useRaBitQ {
		vecIdx.Options["hnswUseRaBitQ"] = "true"
	}

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", vecIdx)
	md, err := builder.Build()
	if err != nil {
		panic(fmt.Sprintf("failed to build vector metadata: %v", err))
	}
	return md, vecIdx
}

// vecInsertVectors inserts n vectors into the store in batches to avoid FDB
// transaction size limits. Returns the vectors that were inserted (indexed by
// their order_id starting at 0). Sequential inserts within each batch.
func vecInsertVectors(tb testing.TB, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData, ss subspace.Subspace, n, dims int, rng *rand.Rand) [][]float64 {
	return vecInsertVectorsParallel(tb, db, md, ss, n, dims, 1, rng)
}

// vecInsertVectorsParallel inserts n vectors with configurable intra-transaction
// parallelism. When parallelism > 1, each batch fires that many goroutines
// sharing one recordlayer.FDBRecordStore within a single FDB transaction. The HNSW write
// lock serializes graph mutations, but FDB I/O is pipelined across goroutines.
func vecInsertVectorsParallel(tb testing.TB, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData, ss subspace.Subspace, n, dims, parallelism int, rng *rand.Rand) [][]float64 {
	tb.Helper()
	ctx := context.Background()
	vectors := make([][]float64, n)
	for i := 0; i < n; i++ {
		vectors[i] = vecRandomVector(rng, dims)
	}

	// Batch insert to stay under FDB transaction limits. HNSW inserts are
	// expensive (graph traversal) AND high-dimensional vectors + the neighbor-list
	// writes scale the per-transaction byte size — at 1536D a batch of 50 overflows
	// the 10MB tx limit (transaction_too_large 2101) once the graph densifies. Scale
	// the batch down with dimensionality (≈ const bytes/tx), overridable via env.
	batchSize := vecEnvInt("VECTOR_BENCH_BATCH", max(1, 50*128/dims))
	insertStart := time.Now()
	progressEvery := max(10000, n/50)
	nextProgress := progressEvery
	for batch := 0; batch*batchSize < n; batch++ {
		batchStart := batch * batchSize
		batchEnd := batchStart + batchSize
		if batchEnd > n {
			batchEnd = n
		}
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(md).
				SetSubspace(ss).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			if parallelism <= 1 {
				// Sequential path.
				for i := batchStart; i < batchEnd; i++ {
					_, err := store.SaveRecord(&gen.Order{
						OrderId:    proto.Int64(int64(i)),
						Price:      proto.Int32(int32(i % 1000)),
						VectorData: serializeVector(vectors[i]),
					})
					if err != nil {
						return nil, fmt.Errorf("insert vector %d: %w", i, err)
					}
				}
			} else {
				// Concurrent path: fire goroutines within one transaction.
				var wg sync.WaitGroup
				errs := make(chan error, batchEnd-batchStart)
				sem := make(chan struct{}, parallelism)

				for i := batchStart; i < batchEnd; i++ {
					wg.Add(1)
					sem <- struct{}{} // limit concurrency
					go func(id int) {
						defer wg.Done()
						defer func() { <-sem }()
						_, err := store.SaveRecord(&gen.Order{
							OrderId:    proto.Int64(int64(id)),
							Price:      proto.Int32(int32(id % 1000)),
							VectorData: serializeVector(vectors[id]),
						})
						if err != nil {
							errs <- fmt.Errorf("insert vector %d: %w", id, err)
						}
					}(i)
				}
				wg.Wait()
				close(errs)
				for err := range errs {
					return nil, err
				}
			}
			return nil, nil
		})
		if err != nil {
			tb.Fatalf("batch %d: %v", batch, err)
		}
		// Progress (for long builds): count, elapsed, instantaneous rate.
		if done := batchEnd; done >= nextProgress {
			el := time.Since(insertStart)
			tb.Logf("  ...inserted %d/%d in %v (%.1f vec/sec)", done, n, el.Round(time.Second), float64(done)/el.Seconds())
			nextProgress += progressEvery
		}
	}
	return vectors
}

// --- Benchmarks ---

// BenchmarkVectorInsert measures the cost of inserting a single vector into an
// HNSW index. Each iteration is one FDB transaction with one record save.
func BenchmarkVectorInsert(b *testing.B) {
	ensureVectorBenchDB(b)

	dims := vecEnvInt("VECTOR_BENCH_DIMS", 128)
	useRaBitQ := vecEnvBool("VECTOR_BENCH_RABITQ", false)
	md, _ := vecBuildMetaData(dims, useRaBitQ)
	ss := vecBenchSubspace(b.Name())
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	// Pre-create the store.
	_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	// Pre-generate all vectors.
	vectors := make([][]float64, b.N)
	for i := range vectors {
		vectors[i] = vecRandomVector(rng, dims)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			return store.SaveRecord(&gen.Order{
				OrderId:    proto.Int64(int64(i)),
				Price:      proto.Int32(100),
				VectorData: serializeVector(vectors[i]),
			})
		})
		if err != nil {
			b.Fatalf("insert %d: %v", i, err)
		}
	}
}

// BenchmarkVectorSearch measures kNN search latency with a pre-populated HNSW
// index. Also reports recall vs brute-force for a sample of queries.
func BenchmarkVectorSearch(b *testing.B) {
	ensureVectorBenchDB(b)

	size := vecEnvInt("VECTOR_BENCH_SIZE", 1000)
	dims := vecEnvInt("VECTOR_BENCH_DIMS", 128)
	k := vecEnvInt("VECTOR_BENCH_K", 10)
	efSearch := vecEnvInt("VECTOR_BENCH_EF_SEARCH", 64)
	useRaBitQ := vecEnvBool("VECTOR_BENCH_RABITQ", false)
	md, vecIdx := vecBuildMetaData(dims, useRaBitQ)
	ss := vecBenchSubspace(b.Name())
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	// Insert N vectors.
	vectors := vecInsertVectors(b, vectorBenchDB, md, ss, size, dims, rng)

	// Pre-generate query vectors for all iterations.
	queryRng := rand.New(rand.NewSource(99))
	queries := make([][]float64, b.N)
	for i := range queries {
		queries[i] = vecRandomVector(queryRng, dims)
	}

	// Measure recall on a sample before timing.
	const recallSampleSize = 10
	sampleRng := rand.New(rand.NewSource(77))
	var totalRecall float64
	for s := 0; s < recallSampleSize; s++ {
		q := vecRandomVector(sampleRng, dims)
		expected := vecBruteForceKNN(q, vectors, k)
		expectedSet := make(map[int64]bool)
		for _, id := range expected {
			expectedSet[id] = true
		}
		var results []recordlayer.VectorSearchResult
		_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			results, err = store.SearchVectorIndex(vecIdx, q, k, efSearch)
			return nil, err
		})
		if err != nil {
			b.Fatalf("recall measurement: %v", err)
		}
		hits := 0
		for _, r := range results {
			if len(r.PrimaryKey) > 0 {
				if id, ok := r.PrimaryKey[0].(int64); ok && expectedSet[id] {
					hits++
				}
			}
		}
		if len(expected) > 0 {
			totalRecall += float64(hits) / float64(len(expected))
		}
	}
	avgRecall := totalRecall / float64(recallSampleSize)
	b.ReportMetric(avgRecall, "recall")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SearchVectorIndex(vecIdx, queries[i], k, efSearch)
			return nil, err
		})
		if err != nil {
			b.Fatalf("search %d: %v", i, err)
		}
	}
}

// BenchmarkVectorInsertParallel measures concurrent vector insert throughput.
// Multiple goroutines insert into the same HNSW graph within one transaction.
// The HNSW write lock serializes graph mutations, but FDB I/O is pipelined.
//
// Configure via environment:
//
//	VECTOR_BENCH_PARALLELISM=4  (default: 4, goroutines per transaction)
//	VECTOR_BENCH_DIMS=128       (default: 128)
//	VECTOR_BENCH_RABITQ=true    (default: false)
func BenchmarkVectorInsertParallel(b *testing.B) {
	ensureVectorBenchDB(b)

	dims := vecEnvInt("VECTOR_BENCH_DIMS", 128)
	parallelism := vecEnvInt("VECTOR_BENCH_PARALLELISM", 4)
	useRaBitQ := vecEnvBool("VECTOR_BENCH_RABITQ", false)
	md, _ := vecBuildMetaData(dims, useRaBitQ)
	ss := vecBenchSubspace(b.Name())
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	// Pre-create the store.
	_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		b.Fatalf("setup: %v", err)
	}

	// Pre-generate all vectors.
	vectors := make([][]float64, b.N)
	for i := range vectors {
		vectors[i] = vecRandomVector(rng, dims)
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.ReportMetric(float64(parallelism), "parallelism")

	// Insert in batches of `parallelism` goroutines per transaction.
	batchSize := parallelism
	for batchStart := 0; batchStart < b.N; batchStart += batchSize {
		batchEnd := batchStart + batchSize
		if batchEnd > b.N {
			batchEnd = b.N
		}
		_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			var wg sync.WaitGroup
			errs := make(chan error, batchEnd-batchStart)
			for i := batchStart; i < batchEnd; i++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					_, err := store.SaveRecord(&gen.Order{
						OrderId:    proto.Int64(int64(id)),
						Price:      proto.Int32(100),
						VectorData: serializeVector(vectors[id]),
					})
					if err != nil {
						errs <- err
					}
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				return nil, err
			}
			return nil, nil
		})
		if err != nil {
			b.Fatalf("batch at %d: %v", batchStart, err)
		}
	}
}

// BenchmarkVectorDelete measures the cost of deleting a single vector from the
// HNSW index (reconnecting neighbors).
func BenchmarkVectorDelete(b *testing.B) {
	ensureVectorBenchDB(b)

	dims := vecEnvInt("VECTOR_BENCH_DIMS", 128)
	useRaBitQ := vecEnvBool("VECTOR_BENCH_RABITQ", false)
	md, _ := vecBuildMetaData(dims, useRaBitQ)
	ss := vecBenchSubspace(b.Name())
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	// Insert b.N vectors.
	vecInsertVectors(b, vectorBenchDB, md, ss, b.N, dims, rng)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.DeleteRecord(tuple.Tuple{int64(i)})
			return nil, err
		})
		if err != nil {
			b.Fatalf("delete %d: %v", i, err)
		}
	}
}

// BenchmarkVectorConcurrentSearch measures concurrent kNN search throughput.
// With N vectors inserted, R goroutines continuously search for a fixed
// duration and report total ops, ops/sec, and latency percentiles.
func BenchmarkVectorConcurrentSearch(b *testing.B) {
	ensureVectorBenchDB(b)

	size := vecEnvInt("VECTOR_BENCH_SIZE", 1000)
	dims := vecEnvInt("VECTOR_BENCH_DIMS", 128)
	k := vecEnvInt("VECTOR_BENCH_K", 10)
	efSearch := vecEnvInt("VECTOR_BENCH_EF_SEARCH", 64)
	readers := vecEnvInt("VECTOR_BENCH_READERS", 10)
	useRaBitQ := vecEnvBool("VECTOR_BENCH_RABITQ", false)
	md, vecIdx := vecBuildMetaData(dims, useRaBitQ)
	ss := vecBenchSubspace(b.Name())
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	// Insert N vectors.
	vecInsertVectors(b, vectorBenchDB, md, ss, size, dims, rng)

	// Each goroutine gets its own PRNG for query generation.
	const duration = 10 * time.Second
	var totalOps atomic.Int64
	var mu sync.Mutex
	var latencies []time.Duration

	b.ResetTimer()

	var wg sync.WaitGroup
	start := time.Now()
	deadline := start.Add(duration)

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			localRng := rand.New(rand.NewSource(int64(workerID * 1000)))
			var localLatencies []time.Duration

			for time.Now().Before(deadline) {
				q := vecRandomVector(localRng, dims)
				opStart := time.Now()
				_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					if err != nil {
						return nil, err
					}
					_, err = store.SearchVectorIndex(vecIdx, q, k, efSearch)
					return nil, err
				})
				elapsed := time.Since(opStart)
				if err != nil {
					// FDB conflicts under contention are expected; count but don't fail.
					continue
				}
				totalOps.Add(1)
				localLatencies = append(localLatencies, elapsed)
			}

			mu.Lock()
			latencies = append(latencies, localLatencies...)
			mu.Unlock()
		}(r)
	}

	wg.Wait()
	b.StopTimer()

	elapsed := time.Since(start)
	ops := totalOps.Load()
	opsPerSec := float64(ops) / elapsed.Seconds()

	// Compute percentiles.
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := vecLatencyPercentile(latencies, 0.50)
	p99 := vecLatencyPercentile(latencies, 0.99)

	b.ReportMetric(opsPerSec, "ops/sec")
	b.ReportMetric(float64(p50.Microseconds()), "p50_us")
	b.ReportMetric(float64(p99.Microseconds()), "p99_us")
	b.ReportMetric(float64(readers), "readers")

	b.Logf("Concurrent search: %d ops in %v (%.1f ops/sec), p50=%v p99=%v, %d readers",
		ops, elapsed, opsPerSec, p50, p99, readers)
}

func vecLatencyPercentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// --- Manual stress test ---

// TestVectorStressManual is a comprehensive vector index stress test.
// Skipped unless VECTOR_STRESS=1 is set. Tagged as manual in Bazel.
//
// Run explicitly:
//
//	bazelisk test //pkg/recordlayer:vector_benchmark_test \
//	  --test_arg="-test.run=TestVectorStressManual" --test_output=streamed \
//	  --test_env=VECTOR_STRESS=1
//
// Configure via environment:
//
//	VECTOR_BENCH_SIZE=10000 VECTOR_BENCH_DIMS=128 VECTOR_BENCH_K=10
//	VECTOR_BENCH_EF_SEARCH=64 VECTOR_BENCH_READERS=100
func TestVectorStressManual(t *testing.T) {
	if os.Getenv("VECTOR_STRESS") != "1" {
		t.Skip("skipping manual vector stress test (set VECTOR_STRESS=1 to run)")
	}
	ensureVectorBenchDB(t)

	size := vecEnvInt("VECTOR_BENCH_SIZE", 10000)
	dims := vecEnvInt("VECTOR_BENCH_DIMS", 128)
	k := vecEnvInt("VECTOR_BENCH_K", 10)
	efSearch := vecEnvInt("VECTOR_BENCH_EF_SEARCH", 64)
	readers := vecEnvInt("VECTOR_BENCH_READERS", 100)
	useRaBitQ := vecEnvBool("VECTOR_BENCH_RABITQ", false)

	parallelism := vecEnvInt("VECTOR_BENCH_PARALLELISM", 1)

	t.Logf("Vector stress test: size=%d dims=%d k=%d ef=%d readers=%d rabitq=%v parallelism=%d",
		size, dims, k, efSearch, readers, useRaBitQ, parallelism)

	md, vecIdx := vecBuildMetaData(dims, useRaBitQ)
	ss := vecBenchSubspace(t.Name())
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	// Phase 1: Insert vectors.
	t.Log("Phase 1: Inserting vectors...")
	insertStart := time.Now()
	vectors := vecInsertVectorsParallel(t, vectorBenchDB, md, ss, size, dims, parallelism, rng)
	insertDuration := time.Since(insertStart)
	insertRate := float64(size) / insertDuration.Seconds()
	t.Logf("  Inserted %d vectors in %v (%.1f vec/sec, parallelism=%d)", size, insertDuration, insertRate, parallelism)

	// Phase 2: Sequential search throughput + recall.
	t.Log("Phase 2: Sequential search throughput...")
	const seqSearchOps = 100
	queryRng := rand.New(rand.NewSource(99))
	var seqLatencies []time.Duration
	var totalRecall float64

	for i := 0; i < seqSearchOps; i++ {
		q := vecRandomVector(queryRng, dims)
		opStart := time.Now()
		var results []recordlayer.VectorSearchResult
		_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, fmt.Errorf("open store: %w", err)
			}
			results, err = store.SearchVectorIndex(vecIdx, q, k, efSearch)
			if err != nil {
				return nil, fmt.Errorf("search: %w", err)
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("sequential search %d: %v", i, err)
		}
		seqLatencies = append(seqLatencies, time.Since(opStart))

		// Compute recall for this query.
		expected := vecBruteForceKNN(q, vectors, k)
		expectedSet := make(map[int64]bool)
		for _, id := range expected {
			expectedSet[id] = true
		}
		hits := 0
		for _, r := range results {
			pk := extractPKInt64(r.PrimaryKey)
			if pk >= 0 && expectedSet[pk] {
				hits++
			}
		}
		if len(expected) > 0 {
			totalRecall += float64(hits) / float64(len(expected))
		}
	}

	sort.Slice(seqLatencies, func(i, j int) bool { return seqLatencies[i] < seqLatencies[j] })
	seqP50 := vecLatencyPercentile(seqLatencies, 0.50)
	seqP99 := vecLatencyPercentile(seqLatencies, 0.99)
	avgRecall := totalRecall / float64(seqSearchOps)
	seqOpsPerSec := float64(seqSearchOps) / vecSumDurations(seqLatencies).Seconds()

	t.Logf("  %d searches: %.1f ops/sec, p50=%v, p99=%v, recall=%.3f",
		seqSearchOps, seqOpsPerSec, seqP50, seqP99, avgRecall)

	// Phase 3: Concurrent search at multiple concurrency levels.
	for _, numReaders := range []int{10, readers} {
		if numReaders <= 0 {
			continue
		}
		t.Logf("Phase 3: Concurrent search (%d readers, 10s)...", numReaders)
		concResult := vecRunConcurrentSearch(t, vectorBenchDB, md, vecIdx, ss, dims, k, efSearch, numReaders, 10*time.Second)
		t.Logf("  %d ops in %v (%.1f ops/sec), p50=%v, p99=%v",
			concResult.ops, concResult.elapsed, concResult.opsPerSec,
			concResult.p50, concResult.p99)
	}

	// Phase 4: Write throughput (insert + delete cycle).
	t.Log("Phase 4: Write throughput (insert+delete cycle)...")
	const writeCycleOps = 200
	writeRng := rand.New(rand.NewSource(777))
	writeStart := time.Now()
	baseID := int64(size) // Start after existing records.
	for i := 0; i < writeCycleOps; i++ {
		id := baseID + int64(i)
		vec := vecRandomVector(writeRng, dims)
		_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{
				OrderId:    proto.Int64(id),
				Price:      proto.Int32(100),
				VectorData: serializeVector(vec),
			})
			return nil, err
		})
		if err != nil {
			t.Fatalf("write cycle insert %d: %v", i, err)
		}

		_, err = vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			_, err = store.DeleteRecord(tuple.Tuple{id})
			return nil, err
		})
		if err != nil {
			t.Fatalf("write cycle delete %d: %v", i, err)
		}
	}
	writeDuration := time.Since(writeStart)
	writeOpsPerSec := float64(writeCycleOps*2) / writeDuration.Seconds()

	t.Logf("  %d insert+delete pairs in %v (%.1f ops/sec)", writeCycleOps, writeDuration, writeOpsPerSec)

	// Summary table.
	t.Log("")
	t.Log("=== VECTOR BENCHMARK SUMMARY ===")
	t.Logf("  Dataset:          %d vectors x %d dims", size, dims)
	t.Logf("  RaBitQ:           %v", useRaBitQ)
	t.Logf("  k=%d, efSearch=%d", k, efSearch)
	t.Log("  ------------------------------------------------")
	t.Logf("  Insert:           %.1f vec/sec (%d vectors in %v)", insertRate, size, insertDuration)
	t.Logf("  Seq search:       %.1f ops/sec, p50=%v, p99=%v", seqOpsPerSec, seqP50, seqP99)
	t.Logf("  Recall@%d:        %.3f", k, avgRecall)
	t.Logf("  Write cycle:      %.1f ops/sec (%d pairs in %v)", writeOpsPerSec, writeCycleOps, writeDuration)
	t.Log("  ================================================")
}

type vecConcurrentResult struct {
	ops       int64
	elapsed   time.Duration
	opsPerSec float64
	p50       time.Duration
	p99       time.Duration
}

func vecRunConcurrentSearch(
	tb testing.TB,
	db *recordlayer.FDBDatabase,
	md *recordlayer.RecordMetaData,
	vecIdx *recordlayer.Index,
	ss subspace.Subspace,
	dims, k, efSearch, numReaders int,
	dur time.Duration,
) vecConcurrentResult {
	tb.Helper()
	ctx := context.Background()
	var totalOps atomic.Int64
	var mu sync.Mutex
	var allLatencies []time.Duration

	var wg sync.WaitGroup
	start := time.Now()
	deadline := start.Add(dur)

	for r := 0; r < numReaders; r++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			localRng := rand.New(rand.NewSource(int64(workerID*1000 + 7)))
			var local []time.Duration

			for time.Now().Before(deadline) {
				q := vecRandomVector(localRng, dims)
				opStart := time.Now()
				_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					if err != nil {
						return nil, err
					}
					_, err = store.SearchVectorIndex(vecIdx, q, k, efSearch)
					return nil, err
				})
				elapsed := time.Since(opStart)
				if err != nil {
					continue
				}
				totalOps.Add(1)
				local = append(local, elapsed)
			}

			mu.Lock()
			allLatencies = append(allLatencies, local...)
			mu.Unlock()
		}(r)
	}

	wg.Wait()
	elapsed := time.Since(start)
	ops := totalOps.Load()

	sort.Slice(allLatencies, func(i, j int) bool { return allLatencies[i] < allLatencies[j] })

	return vecConcurrentResult{
		ops:       ops,
		elapsed:   elapsed,
		opsPerSec: float64(ops) / elapsed.Seconds(),
		p50:       vecLatencyPercentile(allLatencies, 0.50),
		p99:       vecLatencyPercentile(allLatencies, 0.99),
	}
}

func vecSumDurations(ds []time.Duration) time.Duration {
	var total time.Duration
	for _, d := range ds {
		total += d
	}
	return total
}

// serializeVector converts a float64 vector to the storage format (type ordinal 2 = DOUBLE).
// Local copy of the unexported recordlayer.serializeVector.
func serializeVector(vec []float64) []byte {
	buf := make([]byte, 1+8*len(vec))
	buf[0] = 2 // DOUBLE type ordinal — Java VectorType.DOUBLE.ordinal() = 2
	for i, v := range vec {
		binary.BigEndian.PutUint64(buf[1+i*8:], math.Float64bits(v))
	}
	return buf
}

// euclideanDistance computes squared Euclidean distance between two vectors.
// Local copy of the unexported recordlayer.euclideanDistance.
func euclideanDistance(a, b []float64) float64 {
	sum := 0.0
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

// TestVectorConcurrencyScaling measures how vector-search throughput scales
// with the number of concurrent readers. Each reader runs independent K-NN
// queries on independent transactions, so throughput SHOULD climb near-linearly
// with readers until the FDB server (or a client-side serialization point)
// saturates. A flat/sub-linear curve localizes the bottleneck; the optional
// profiles say whether it's CPU (distance math), network wait, or lock contention.
//
//	VECTOR_SCALING=1 [VECTOR_PROFILE=1] [VECTOR_BENCH_SIZE/DIMS/...] \
//	  go test ./pkg/recordlayer/bench -run TestVectorConcurrencyScaling -v -timeout 20m
//
// VECTOR_PROFILE=1 dumps cpu/block/mutex profiles for the peak concurrency
// level to /tmp/vecprof_{cpu,block,mutex}.prof.
func TestVectorConcurrencyScaling(t *testing.T) {
	if os.Getenv("VECTOR_SCALING") != "1" {
		t.Skip("set VECTOR_SCALING=1 to run")
	}
	ensureVectorBenchDB(t)

	size := vecEnvInt("VECTOR_BENCH_SIZE", 1000)
	dims := vecEnvInt("VECTOR_BENCH_DIMS", 1536)
	k := vecEnvInt("VECTOR_BENCH_K", 10)
	efSearch := vecEnvInt("VECTOR_BENCH_EF_SEARCH", 64)
	parallelism := vecEnvInt("VECTOR_BENCH_PARALLELISM", 8)
	perLevel := time.Duration(vecEnvInt("VECTOR_BENCH_SECS", 6)) * time.Second

	md, vecIdx := vecBuildMetaData(dims, false)
	ss := vecBenchSubspace(t.Name())
	rng := rand.New(rand.NewSource(42))

	t.Logf("Scaling test: size=%d dims=%d k=%d ef=%d perLevel=%v cores=%d",
		size, dims, k, efSearch, perLevel, runtime.NumCPU())
	insertStart := time.Now()
	vecInsertVectorsParallel(t, vectorBenchDB, md, ss, size, dims, parallelism, rng)
	t.Logf("  Inserted %d vectors in %v", size, time.Since(insertStart))

	profile := os.Getenv("VECTOR_PROFILE") == "1"
	if profile {
		runtime.SetBlockProfileRate(1)     // sample every blocking event
		runtime.SetMutexProfileFraction(1) // sample every mutex contention
	}

	levels := []int{1, 2, 4, 8, 16, 24}
	peak := levels[len(levels)-1]
	t.Logf("%-9s %-8s %-10s %-11s %-11s %-7s %-8s %-9s %-7s %-8s",
		"wall", "readers", "ops/sec", "p50", "p99", "scale", "cliCPU", "alloc/op", "nGC", "gcPause")
	var baseOpsPerSec float64
	for _, n := range levels {
		var stopCPU func()
		if profile && n == peak {
			f, err := os.Create("/tmp/vecprof_cpu.prof")
			if err == nil {
				_ = pprof.StartCPUProfile(f)
				stopCPU = func() { pprof.StopCPUProfile(); f.Close() }
			}
		}
		var ms0, ms1 runtime.MemStats
		runtime.ReadMemStats(&ms0)
		cpu0 := cpuSecondsSelf()
		t0 := time.Now()
		res := vecRunConcurrentSearch(t, vectorBenchDB, md, vecIdx, ss, dims, k, efSearch, n, perLevel)
		wall := time.Since(t0)
		cliCPU := (cpuSecondsSelf() - cpu0) / wall.Seconds()
		runtime.ReadMemStats(&ms1)
		if stopCPU != nil {
			stopCPU()
			vecWriteProfile("/tmp/vecprof_block.prof", "block")
			vecWriteProfile("/tmp/vecprof_mutex.prof", "mutex")
			vecWriteProfile("/tmp/vecprof_allocs.prof", "allocs")
		}
		if n == 1 {
			baseOpsPerSec = res.opsPerSec
		}
		scale := 0.0
		if baseOpsPerSec > 0 {
			scale = res.opsPerSec / baseOpsPerSec
		}
		allocPerOp := uint64(0)
		if res.ops > 0 {
			allocPerOp = (ms1.TotalAlloc - ms0.TotalAlloc) / uint64(res.ops)
		}
		gcPause := time.Duration(ms1.PauseTotalNs - ms0.PauseTotalNs)
		// wall window [start..end] printed so a parallel `docker stats` sampler can
		// be aligned to each concurrency level (server CPU vs client cores).
		t.Logf("%-9s %-8d %-10.1f %-11v %-11v %-6.2fx %-7.1f %-9s %-9d %v",
			t0.Format("15:04:05"), n, res.opsPerSec, res.p50, res.p99, scale,
			cliCPU, vecHumanBytes(allocPerOp), ms1.NumGC-ms0.NumGC, gcPause)
	}
}

// cpuSecondsSelf returns cumulative user+system CPU seconds consumed by this
// process (the client). Diffing it across a timed window and dividing by wall
// time yields "cores busy" — the client-side CPU cost of the workload.
func cpuSecondsSelf() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	u := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
	s := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
	return (u + s).Seconds()
}

func vecHumanBytes(b uint64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// TestRawReadScaling measures the raw point-read throughput ceiling of the
// FDB testcontainer, bypassing HNSW and tuple-decode entirely. Each op is one
// transaction issuing RAW_BATCH pipelined Gets. If raw reads plateau at the
// same per-op ceiling as the HNSW search, the bottleneck is the single-node
// server's read throughput; if raw reads scale far higher, the HNSW/decode
// path is the client-side cap. Gated on RAW_SCALING=1.
func TestRawReadScaling(t *testing.T) {
	if os.Getenv("RAW_SCALING") != "1" {
		t.Skip("set RAW_SCALING=1 to run")
	}
	ensureVectorBenchDB(t)
	ctx := context.Background()
	ss := vecBenchSubspace(t.Name())
	batch := vecEnvInt("RAW_BATCH", 50)
	perLevel := time.Duration(vecEnvInt("VECTOR_BENCH_SECS", 4)) * time.Second

	const nKeys = 2000
	keys := make([][]byte, nKeys)
	for i := range keys {
		keys[i] = ss.Pack(tuple.Tuple{int64(i)})
	}
	// Seed keys (chunked to stay under the 10MB tx limit; values are tiny here).
	for off := 0; off < nKeys; off += 500 {
		end := off + 500
		if end > nKeys {
			end = nKeys
		}
		_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			for i := off; i < end; i++ {
				tx.Set(fdb.Key(keys[i]), []byte{byte(i), byte(i >> 8)})
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("seed keys: %v", err)
		}
	}

	levels := []int{1, 2, 4, 8, 16, 24, 48, 96}
	t.Logf("Raw point-read scaling: batch=%d reads/tx, cores=%d", batch, runtime.NumCPU())
	t.Logf("%-9s %-8s %-13s %-11s %-7s %-8s", "wall", "readers", "reads/sec", "p50/tx", "scale", "cliCPU")
	var base float64
	for _, nr := range levels {
		var ops atomic.Int64 // reads
		var mu sync.Mutex
		var lats []time.Duration
		var wg sync.WaitGroup
		t0 := time.Now()
		deadline := t0.Add(perLevel)
		cpu0 := cpuSecondsSelf()
		for r := 0; r < nr; r++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(int64(id*131 + 1)))
				var local []time.Duration
				for time.Now().Before(deadline) {
					op := time.Now()
					_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
						tx := rtx.Transaction()
						futs := make([]fdb.FutureByteSlice, batch)
						for b := 0; b < batch; b++ {
							futs[b] = tx.Get(fdb.Key(keys[rng.Intn(nKeys)]))
						}
						for b := 0; b < batch; b++ {
							if _, e := futs[b].Get(); e != nil {
								return nil, e
							}
						}
						return nil, nil
					})
					if err == nil {
						ops.Add(int64(batch))
						local = append(local, time.Since(op))
					}
				}
				mu.Lock()
				lats = append(lats, local...)
				mu.Unlock()
			}(r)
		}
		wg.Wait()
		el := time.Since(t0)
		cores := (cpuSecondsSelf() - cpu0) / el.Seconds()
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		readsPerSec := float64(ops.Load()) / el.Seconds()
		if nr == 1 {
			base = readsPerSec
		}
		scale := 0.0
		if base > 0 {
			scale = readsPerSec / base
		}
		t.Logf("%-9s %-8d %-13.0f %-11v %-6.2fx %-7.1f",
			t0.Format("15:04:05"), nr, readsPerSec, vecLatencyPercentile(lats, 0.5), scale, cores)
	}
}

func vecWriteProfile(path, name string) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	if p := pprof.Lookup(name); p != nil {
		_ = p.WriteTo(f, 0)
	}
}

// TestVectorBuildLarge streams a large HNSW index to FDB without ever holding
// the whole dataset in RAM: vectors are generated per batch and discarded, so
// process memory is bounded by the per-batch working set, not the dataset. This
// is what makes a 1M-vector build runnable on a box with limited free RAM.
//
//	VECTOR_BUILD=1 VECTOR_BENCH_SIZE=1000000 VECTOR_BENCH_DIMS=1536 \
//	  VECTOR_BENCH_BATCH=16 \
//	  go test ./pkg/recordlayer/bench -run TestVectorBuildLarge -v -timeout 1200m
func TestVectorBuildLarge(t *testing.T) {
	if os.Getenv("VECTOR_BUILD") != "1" {
		t.Skip("set VECTOR_BUILD=1 to run the large streaming build")
	}
	// Datasets larger than RAM need disk-backed FDB (the shared bench DB uses the
	// memory engine on tmpfs). VECTOR_BENCH_DISK=1 spins a dedicated disk-backed
	// container with an SSD storage engine.
	db := vectorBenchDB
	if os.Getenv("VECTOR_BENCH_DISK") == "1" {
		db = vecDiskBackedDB(t)
	} else {
		ensureVectorBenchDB(t)
		db = vectorBenchDB
	}
	size := vecEnvInt("VECTOR_BENCH_SIZE", 1000000)
	dims := vecEnvInt("VECTOR_BENCH_DIMS", 1536)
	batchSize := vecEnvInt("VECTOR_BENCH_BATCH", 16)
	md, vecIdx := vecBuildMetaData(dims, false)
	ss := vecBenchSubspace(t.Name())
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))

	t.Logf("Streaming build: size=%d dims=%d batch=%d cores=%d",
		size, dims, batchSize, runtime.NumCPU())

	start := time.Now()
	progressEvery := max(20000, size/50)
	nextProgress := progressEvery
	buf := make([][]float64, batchSize)
	for batchStart := 0; batchStart < size; batchStart += batchSize {
		end := batchStart + batchSize
		if end > size {
			end = size
		}
		for i := batchStart; i < end; i++ {
			buf[i-batchStart] = vecRandomVector(rng, dims)
		}
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := batchStart; i < end; i++ {
				if _, err := store.SaveRecord(&gen.Order{
					OrderId:    proto.Int64(int64(i)),
					Price:      proto.Int32(int32(i % 1000)),
					VectorData: serializeVector(buf[i-batchStart]),
				}); err != nil {
					return nil, fmt.Errorf("insert %d: %w", i, err)
				}
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("batch at %d: %v", batchStart, err)
		}
		if end >= nextProgress {
			el := time.Since(start)
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			t.Logf("  ...%d/%d in %v (%.1f vec/sec, heap %dMB)",
				end, size, el.Round(time.Second), float64(end)/el.Seconds(), ms.HeapInuse>>20)
			nextProgress += progressEvery
		}
	}
	total := time.Since(start)
	t.Logf("BUILD DONE: %d vectors in %v (%.1f vec/sec)", size, total.Round(time.Second), float64(size)/total.Seconds())

	// Spot-check searchability (no brute-force recall — dataset isn't retained).
	q := vecRandomVector(rng, dims)
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
		if err != nil {
			return nil, err
		}
		res, err := store.SearchVectorIndex(vecIdx, q, 10, 200)
		if err != nil {
			return nil, err
		}
		t.Logf("sample search returned %d results", len(res))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("sample search: %v", err)
	}
}

// vecDiskBackedDB spins a dedicated FDB container that stores data on disk with
// an SSD storage engine, for datasets larger than RAM (the shared bench DB uses
// the in-RAM memory engine on tmpfs). The container is terminated at test end.
func vecDiskBackedDB(t *testing.T) *recordlayer.FDBDatabase {
	t.Helper()
	setupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(setupCtx, "",
		foundationdbtc.WithAPIVersion(720),
		foundationdbtc.WithDataOnDisk(),
		foundationdbtc.WithStorageEngine("ssd-redwood-1"),
	)
	if err != nil {
		t.Fatalf("start disk-backed FDB: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	clusterFile, err := container.ClusterFile(setupCtx)
	if err != nil {
		t.Fatalf("cluster file: %v", err)
	}
	tmpFile, err := os.CreateTemp("", "fdb_diskbench_*.txt")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := tmpFile.WriteString(clusterFile); err != nil {
		t.Fatalf("write cluster file: %v", err)
	}
	tmpFile.Close()
	fdb.MustAPIVersion(720)
	dbConn, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		t.Fatalf("open disk-backed FDB: %v", err)
	}
	return recordlayer.NewFDBDatabase(dbConn)
}

// TestVectorIngestScaling measures whether ingestion scales horizontally across
// independent shard graphs. Each shard is its own subspace (its own HNSW graph,
// per-tx per-prefix write lock, own shared cache), so concurrent shard builders
// run on separate transactions that don't contend or FDB-conflict. It builds the
// same shards sequentially then concurrently and reports the speedup — the answer
// to "the single-writer lock serializes one graph; shard to use all cores."
//
//	VECTOR_INGEST=1 [VECTOR_BENCH_SHARDS=8 VECTOR_BENCH_SIZE=3000] \
//	  go test ./pkg/recordlayer/bench -run TestVectorIngestScaling -v -timeout 30m
//
// TestVectorIngestSweep measures how ingestion throughput scales with the number
// of concurrent shard builders. Each shard is an independent HNSW graph (its own
// subspace, per-tx per-prefix write lock, own shared cache), so concurrent
// builders run on separate transactions that don't contend or FDB-conflict. It
// sweeps the builder count and reports aggregate vec/sec — the ceiling is where
// the single-node FDB or the client cores saturate.
//
//	VECTOR_INGEST=1 [VECTOR_BENCH_SIZE=4000 VECTOR_BENCH_LEVELS=1,2,4,8,16,24] \
//	  go test ./pkg/recordlayer/bench -run TestVectorIngestSweep -v -timeout 60m
func TestVectorIngestSweep(t *testing.T) {
	if os.Getenv("VECTOR_INGEST") != "1" {
		t.Skip("set VECTOR_INGEST=1 to run")
	}
	db := vectorBenchDB
	procs := vecEnvInt("VECTOR_BENCH_PROCS", 1)
	if procs > 1 {
		db = vecMultiProcDB(t, procs)
	} else {
		ensureVectorBenchDB(t)
		db = vectorBenchDB
	}
	perShard := vecEnvInt("VECTOR_BENCH_SIZE", 4000)
	dims := vecEnvInt("VECTOR_BENCH_DIMS", 1536)
	batch := vecEnvInt("VECTOR_BENCH_BATCH", 16)
	t.Logf("FDB processes: %d", procs)
	levels := []int{1, 2, 4, 8, 16, 24}
	if v := os.Getenv("VECTOR_BENCH_LEVELS"); v != "" {
		levels = nil
		for _, s := range strings.Split(v, ",") {
			if n, err := strconv.Atoi(s); err == nil {
				levels = append(levels, n)
			}
		}
	}
	md, _ := vecBuildMetaData(dims, false)
	ctx := context.Background()
	t.Logf("Ingest sweep: %d vec/shard (%dD), batch=%d, cores=%d", perShard, dims, batch, runtime.NumCPU())

	buildShard := func(tag string) error {
		ss := vecBenchSubspace(tag)
		rng := rand.New(rand.NewSource(int64(len(tag)*7919 + 1)))
		buf := make([][]float64, batch)
		for start := 0; start < perShard; start += batch {
			end := start + batch
			if end > perShard {
				end = perShard
			}
			for i := start; i < end; i++ {
				buf[i-start] = vecRandomVector(rng, dims)
			}
			if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := start; i < end; i++ {
					if _, err := store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(int64(i)), Price: proto.Int32(int32(i % 1000)),
						VectorData: serializeVector(buf[i-start]),
					}); err != nil {
						return nil, err
					}
				}
				return nil, nil
			}); err != nil {
				return err
			}
		}
		return nil
	}

	// VECTOR_PROFILE=1 captures a client-side CPU profile (plus alloc/block/mutex)
	// of the peak (most concurrent) level's build, written to /tmp/vecprof_ingest_*.
	// Answers "what is the client spending CPU on during ingest" — distance compute,
	// tuple/proto encode, FDB client serialization, or GC — and, by comparing the
	// client core count against total machine CPU, whether the build is client-CPU-
	// bound (won't scale horizontally) or FDB-bound (more nodes help).
	profile := os.Getenv("VECTOR_PROFILE") == "1"
	if profile {
		runtime.SetBlockProfileRate(1)     // sample every blocking event (I/O waits)
		runtime.SetMutexProfileFraction(1) // sample every mutex contention (write lock)
	}
	peak := levels[len(levels)-1]
	t.Logf("%-8s %-14s %-14s %-8s %-8s", "builders", "aggregate v/s", "per-shard v/s", "scale", "cliCPU")
	var base float64
	runID := 0
	for _, c := range levels {
		var stopCPU func()
		if profile && c == peak {
			if f, err := os.Create("/tmp/vecprof_ingest_cpu.prof"); err == nil {
				_ = pprof.StartCPUProfile(f)
				stopCPU = func() { pprof.StopCPUProfile(); f.Close() }
			}
		}
		cpu0 := cpuSecondsSelf()
		t0 := time.Now()
		var wg sync.WaitGroup
		errs := make(chan error, c)
		for s := 0; s < c; s++ {
			wg.Add(1)
			runID++
			go func(id int) {
				defer wg.Done()
				if err := buildShard(fmt.Sprintf("%s-r%d", t.Name(), id)); err != nil {
					errs <- err
				}
			}(runID)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("builder error: %v", err)
		}
		el := time.Since(t0)
		cores := (cpuSecondsSelf() - cpu0) / el.Seconds()
		agg := float64(c*perShard) / el.Seconds()
		if c == levels[0] {
			base = agg / float64(c) // per-shard baseline
		}
		if stopCPU != nil {
			stopCPU()
			vecWriteProfile("/tmp/vecprof_ingest_allocs.prof", "allocs")
			vecWriteProfile("/tmp/vecprof_ingest_block.prof", "block")
			vecWriteProfile("/tmp/vecprof_ingest_mutex.prof", "mutex")
			t.Logf("profiles written: /tmp/vecprof_ingest_{cpu,allocs,block,mutex}.prof")
		}
		t.Logf("%-8d %-14.1f %-14.1f %-7.2fx %-7.1f", c, agg, agg/float64(c), (agg/float64(c))/base, cores)
	}
}

// vecMultiProcDB spins a dedicated FDB container with N fdbserver processes, to
// test whether more FDB commit/storage capacity lifts the concurrent-ingest
// ceiling that one process imposes. Defaults to the disk-backed ssd-redwood-1
// engine (VECTOR_BENCH_ENGINE to override): the in-RAM memory engine holds the
// whole dataset in RAM and, with ~2N fdbserver processes competing for cores,
// fills memory and starves the cluster at high concurrency (process_behind /
// unresponsive container at C≈12). Disk-backed Redwood spills to the container's
// data volume, so commit capacity — not RAM — sets the ceiling. Terminated at
// test end.
func vecMultiProcDB(t *testing.T, procs int) *recordlayer.FDBDatabase {
	t.Helper()
	engine := os.Getenv("VECTOR_BENCH_ENGINE")
	if engine == "" {
		engine = "ssd-redwood-1"
	}
	setupCtx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	opts := []testcontainers.ContainerCustomizer{
		foundationdbtc.WithAPIVersion(720),
		foundationdbtc.WithProcessCount(procs),
		foundationdbtc.WithStorageEngine(engine),
	}
	if engine != "memory" {
		// Spill to the container's on-disk data volume instead of the default
		// tmpfs, so the dataset isn't capped by (and doesn't exhaust) host RAM.
		opts = append(opts, foundationdbtc.WithDataOnDisk())
	}
	t.Logf("FDB engine: %s (%d procs)", engine, procs)
	container, err := foundationdbtc.Run(setupCtx, "", opts...)
	if err != nil {
		t.Fatalf("start %d-proc FDB: %v", procs, err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	clusterFile, err := container.ClusterFile(setupCtx)
	if err != nil {
		t.Fatalf("cluster file: %v", err)
	}
	tmpFile, err := os.CreateTemp("", "fdb_mproc_*.txt")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := tmpFile.WriteString(clusterFile); err != nil {
		t.Fatalf("write cluster file: %v", err)
	}
	tmpFile.Close()
	fdb.MustAPIVersion(720)
	dbConn, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		t.Fatalf("open %d-proc FDB: %v", procs, err)
	}
	return recordlayer.NewFDBDatabase(dbConn)
}
