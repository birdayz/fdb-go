package bench

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
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
		ctx := context.Background()
		container, err := foundationdbtc.Run(ctx, "",
			foundationdbtc.WithAPIVersion(720),
		)
		if err != nil {
			tb.Fatalf("failed to start FDB container: %v", err)
		}
		if err := container.InitializeDatabase(ctx); err != nil {
			tb.Fatalf("failed to init FDB: %v", err)
		}
		clusterFile, err := container.ClusterFile(ctx)
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

	// Batch insert to stay under FDB transaction limits.
	// HNSW inserts are expensive (graph traversal), so keep batches small.
	batchSize := 50
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
