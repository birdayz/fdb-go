package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// TestSIFTBenchmark runs the SIFT-1M benchmark against our HNSW vector index.
//
// Prerequisites:
//   - Run scripts/download-sift.sh to download SIFT-1M (~500MB)
//   - Set SIFT_BENCH=1 to enable
//
// Configuration via environment:
//
//	SIFT_N=10000           how many base vectors to index (default 10000)
//	SIFT_K=10              k nearest neighbors to retrieve (default 10)
//	SIFT_EF=64             efSearch parameter (default 64)
//	SIFT_M=16              HNSW M parameter (default 16, note: hardcoded in defaults)
//	SIFT_EF_CONSTRUCTION=200  efConstruction parameter (default 200, note: hardcoded)
//	SIFT_BATCH_SIZE=50     vectors per transaction (default 50)
//	SIFT_NUM_QUERIES=100   number of queries for recall/latency measurement (default 100)
//
// Run:
//
//	bazelisk test //pkg/recordlayer:sift_benchmark_test \
//	  --test_arg="-test.run=TestSIFTBenchmark" --test_output=streamed \
//	  --test_env=SIFT_BENCH=1
func TestSIFTBenchmark(t *testing.T) {
	if os.Getenv("SIFT_BENCH") != "1" {
		t.Skip("set SIFT_BENCH=1 to run SIFT benchmark")
	}

	// Parse configuration.
	n := siftEnvInt("SIFT_N", 10000)
	k := siftEnvInt("SIFT_K", 10)
	efSearch := siftEnvInt("SIFT_EF", 64)
	m := siftEnvInt("SIFT_M", 16)
	efConstruction := siftEnvInt("SIFT_EF_CONSTRUCTION", 200)
	batchSize := siftEnvInt("SIFT_BATCH_SIZE", 50)
	numQueries := siftEnvInt("SIFT_NUM_QUERIES", 100)
	parallelism := siftEnvInt("SIFT_PARALLELISM", 10)
	useRaBitQ := os.Getenv("SIFT_RABITQ") == "1"

	t.Logf("SIFT benchmark config: N=%d, K=%d, efSearch=%d, M=%d, efConstruction=%d, batch=%d, queries=%d, parallelism=%d, rabitq=%v",
		n, k, efSearch, m, efConstruction, batchSize, numQueries, parallelism, useRaBitQ)

	// Resolve SIFT data directory. Priority:
	// 1. SIFT_DATA_DIR env var (explicit override)
	// 2. Bazel runfiles (@sift1m repository — auto-downloaded by Bazel)
	// 3. Local testdata/ fallback (from scripts/download-sift.sh)
	siftDir := resolveSIFTDir()
	baseVecs, err := LoadFVecs(filepath.Join(siftDir, "sift_base.fvecs"), n)
	if err != nil {
		t.Fatalf("Failed to load base vectors: %v\n\nRun scripts/download-sift.sh first.", err)
	}
	queryVecs, err := LoadFVecs(filepath.Join(siftDir, "sift_query.fvecs"), 0) // all 10K queries
	if err != nil {
		t.Fatalf("Failed to load query vectors: %v", err)
	}
	groundTruth, err := LoadIVecs(filepath.Join(siftDir, "sift_groundtruth.ivecs"), 0) // all 10K ground truth
	if err != nil {
		t.Fatalf("Failed to load ground truth: %v", err)
	}

	t.Logf("Loaded %d base vectors (dim=%d), %d queries, %d ground truth entries",
		len(baseVecs), len(baseVecs[0]), len(queryVecs), len(groundTruth))

	if len(baseVecs) < n {
		t.Fatalf("Requested %d base vectors but file only contains %d", n, len(baseVecs))
	}
	baseVecs = baseVecs[:n]

	// Check if ground truth is valid for our subset.
	gtOutOfRange := siftValidateGroundTruth(groundTruth, n)
	if gtOutOfRange {
		t.Logf("Ground truth references vectors beyond our %d-vector subset; will use brute-force recall", n)
	}

	// Pre-convert base vectors to float64 for brute-force recall computation.
	baseVecsF64 := make([][]float64, n)
	for i, v := range baseVecs {
		baseVecsF64[i] = float32sToFloat64s(v)
	}

	// Initialize FDB.
	ensureVectorBenchDB(t)
	ctx := context.Background()

	// Build metadata with VECTOR index.
	// Note: M and efConstruction are controlled by DefaultHNSWConfig defaults
	// (M=16, efConstruction=200). These match the env var defaults.
	md, vecIdx := vecBuildMetaData(128, useRaBitQ)
	ss := vecBenchSubspace("sift-benchmark")

	// Create store.
	_, err = vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		return nil, err
	})
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	// =========================================================================
	// Phase 1: Build index
	// =========================================================================
	t.Log("")
	t.Log("Phase 1: Building index...")
	buildStart := time.Now()

	for batch := 0; batch*batchSize < n; batch++ {
		batchStart := batch * batchSize
		batchEnd := batchStart + batchSize
		if batchEnd > n {
			batchEnd = n
		}
		_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			if parallelism <= 1 {
				for i := batchStart; i < batchEnd; i++ {
					_, err := store.SaveRecord(&gen.Order{
						OrderId:    proto.Int64(int64(i)),
						Price:      proto.Int32(int32(i % 1000)),
						VectorData: serializeVectorF32(baseVecs[i]),
					})
					if err != nil {
						return nil, fmt.Errorf("insert vector %d: %w", i, err)
					}
				}
			} else {
				var wg sync.WaitGroup
				errs := make(chan error, batchEnd-batchStart)
				sem := make(chan struct{}, parallelism)
				for i := batchStart; i < batchEnd; i++ {
					wg.Add(1)
					sem <- struct{}{}
					go func(id int) {
						defer wg.Done()
						defer func() { <-sem }()
						_, err := store.SaveRecord(&gen.Order{
							OrderId:    proto.Int64(int64(id)),
							Price:      proto.Int32(int32(id % 1000)),
							VectorData: serializeVectorF32(baseVecs[id]),
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
			t.Fatalf("Batch insert starting at %d: %v", batch*batchSize, err)
		}

		// Progress reporting every 10 batches.
		inserted := batchEnd
		if (batch+1)%10 == 0 || batchEnd == n {
			elapsed := time.Since(buildStart)
			rate := float64(inserted) / elapsed.Seconds()
			t.Logf("  Inserted %d/%d vectors (%.1f vec/sec, elapsed %v, parallelism=%d)",
				inserted, n, rate, elapsed.Round(time.Millisecond), parallelism)
		}
	}

	buildDuration := time.Since(buildStart)
	buildRate := float64(n) / buildDuration.Seconds()
	t.Logf("  Build complete: %d vectors in %v (%.1f vec/sec)", n, buildDuration.Round(time.Millisecond), buildRate)

	// =========================================================================
	// Phase 2: Query — measure recall and latency
	// =========================================================================
	t.Log("")
	t.Log("Phase 2: Querying...")

	if numQueries > len(queryVecs) {
		numQueries = len(queryVecs)
	}

	var (
		latencies    []time.Duration
		recall1Sum   float64
		recall10Sum  float64
		recall100Sum float64
	)

	for qi := 0; qi < numQueries; qi++ {
		queryF64 := float32sToFloat64s(queryVecs[qi])

		opStart := time.Now()
		var results []recordlayer.VectorSearchResult
		_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			// Use max(k, 100) for efSearch to get enough candidates for recall@100.
			searchEf := efSearch
			if searchEf < 100 {
				searchEf = 100
			}
			results, err = store.SearchVectorIndex(vecIdx, queryF64, 100, searchEf)
			return nil, err
		})
		latencies = append(latencies, time.Since(opStart))
		if err != nil {
			t.Fatalf("Query %d failed: %v", qi, err)
		}

		// Compute recall at various k values.
		if gtOutOfRange {
			// Ground truth is for full 1M dataset; compute brute-force recall.
			recall1Sum += siftBruteForceRecall(results, queryF64, baseVecsF64, 1)
			recall10Sum += siftBruteForceRecall(results, queryF64, baseVecsF64, 10)
			recall100Sum += siftBruteForceRecall(results, queryF64, baseVecsF64, 100)
		} else {
			// Ground truth is valid for our subset.
			recall1Sum += siftRecallAtK(results, groundTruth[qi], 1)
			recall10Sum += siftRecallAtK(results, groundTruth[qi], 10)
			recall100Sum += siftRecallAtK(results, groundTruth[qi], 100)
		}

		// Progress reporting.
		if (qi+1)%25 == 0 || qi+1 == numQueries {
			avgLatency := time.Duration(0)
			for _, l := range latencies {
				avgLatency += l
			}
			avgLatency /= time.Duration(len(latencies))
			t.Logf("  Queried %d/%d (avg latency %v)", qi+1, numQueries, avgLatency.Round(time.Microsecond))
		}
	}

	// Compute latency percentiles.
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := siftLatencyPercentile(latencies, 0.50)
	p99 := siftLatencyPercentile(latencies, 0.99)
	totalQueryTime := time.Duration(0)
	for _, l := range latencies {
		totalQueryTime += l
	}
	qps := float64(numQueries) / totalQueryTime.Seconds()

	recall1 := recall1Sum / float64(numQueries)
	recall10 := recall10Sum / float64(numQueries)
	recall100 := recall100Sum / float64(numQueries)

	// =========================================================================
	// Phase 3: Print results
	// =========================================================================
	recallMethod := "vs ground truth"
	if gtOutOfRange {
		recallMethod = "vs brute force"
	}

	t.Log("")
	t.Log("=== SIFT-1M BENCHMARK ===")
	t.Logf("  Dataset:       SIFT-1M (first %d vectors)", n)
	t.Logf("  Config:        M=%d, efConstruction=%d, efSearch=%d", m, efConstruction, efSearch)
	t.Logf("  Dimensions:    128 (float32->float64)")
	t.Log("  ------------------------------------------------")
	t.Logf("  Build:         %.1f vec/sec (%d vectors in %v, parallelism=%d)", buildRate, n, buildDuration.Round(time.Millisecond), parallelism)
	t.Log("  ------------------------------------------------")
	t.Logf("  Recall@1:      %.3f (%s)", recall1, recallMethod)
	t.Logf("  Recall@10:     %.3f", recall10)
	t.Logf("  Recall@100:    %.3f", recall100)
	t.Logf("  QPS:           %.1f (sequential, single thread)", qps)
	t.Logf("  p50:           %v", p50.Round(100*time.Microsecond))
	t.Logf("  p99:           %v", p99.Round(100*time.Microsecond))
	t.Log("  ------------------------------------------------")
	t.Log("  Comparison (k=10, similar params):")
	t.Log("    hnswlib:     0.95 recall, 5065 QPS")
	t.Log("    Weaviate:    0.98 recall, 10940 QPS")
	t.Log("    Qdrant:      0.995 recall, 626 QPS")
	t.Logf("    FDB Go:      %.3f recall, %.0f QPS  <- sequential FDB reads", recall10, qps)
	t.Log("  ================================================")
}

// siftEnvInt reads an integer from an environment variable with a default.
func siftEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

// TestSIFTLoaderUnit verifies the fvecs/ivecs loaders compile and handle
// missing files gracefully. Does not require SIFT data.
func TestSIFTLoaderUnit(t *testing.T) {
	if os.Getenv("SIFT_BENCH") != "1" {
		t.Skip("set SIFT_BENCH=1 to run SIFT tests")
	}

	t.Run("fvecs_missing_file", func(t *testing.T) {
		_, err := LoadFVecs("/nonexistent/path.fvecs", 10)
		if err == nil {
			t.Fatal("expected error for missing fvecs file")
		}
	})

	t.Run("ivecs_missing_file", func(t *testing.T) {
		_, err := LoadIVecs("/nonexistent/path.ivecs", 10)
		if err == nil {
			t.Fatal("expected error for missing ivecs file")
		}
	})

	t.Run("recall_calculation", func(t *testing.T) {
		// Verify recall calculation with known data.
		results := []recordlayer.VectorSearchResult{
			{PrimaryKey: tuple.Tuple{int64(0)}},
			{PrimaryKey: tuple.Tuple{int64(1)}},
			{PrimaryKey: tuple.Tuple{int64(5)}},
		}
		gt := []int32{0, 1, 2}
		recall := siftRecallAtK(results, gt, 3)
		// results contain IDs 0,1,5; ground truth is 0,1,2 -> 2 hits out of 3
		expected := 2.0 / 3.0
		if recall < expected-0.001 || recall > expected+0.001 {
			t.Fatalf("expected recall %.3f, got %.3f", expected, recall)
		}
	})

	t.Run("serialize_roundtrip_f32", func(t *testing.T) {
		// Verify float32 -> serialized -> float64 roundtrip.
		input := []float32{1.0, 2.5, -3.7, 0.0}
		serialized := serializeVectorF32(input)
		deserialized, err := deserializeToFloat64(serialized)
		if err != nil {
			t.Fatalf("deserialize failed: %v", err)
		}
		if len(deserialized) != len(input) {
			t.Fatalf("length mismatch: %d vs %d", len(deserialized), len(input))
		}
		for i, v := range input {
			if diff := deserialized[i] - float64(v); diff > 1e-6 || diff < -1e-6 {
				t.Fatalf("value mismatch at %d: %f vs %f", i, float64(v), deserialized[i])
			}
		}
	})
}

// resolveSIFTDir finds the SIFT data files from env, Bazel runfiles, or local testdata.
func resolveSIFTDir() string {
	// 1. Explicit override.
	if dir := os.Getenv("SIFT_DATA_DIR"); dir != "" {
		return dir
	}
	// 2. Bazel runfiles: @sift1m data lands under the runfiles tree.
	//    bzlmod canonicalizes the repo name to "+sift_dataset+sift1m".
	candidates := []string{
		"+sift_dataset+sift1m", // bzlmod canonical
		"sift1m",               // workspace name fallback
	}
	for _, envKey := range []string{"RUNFILES_DIR", "TEST_SRCDIR"} {
		base := os.Getenv(envKey)
		if base == "" {
			continue
		}
		for _, repoName := range candidates {
			dir := filepath.Join(base, repoName)
			if _, err := os.Stat(filepath.Join(dir, "sift_base.fvecs")); err == nil {
				return dir
			}
		}
	}
	// 3. Local testdata/ (from scripts/download-sift.sh).
	return "testdata"
}
