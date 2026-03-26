package bench

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

func TestHNSWSearchProfile(t *testing.T) {
	if os.Getenv("HNSW_PROFILE") != "1" {
		t.Skip("set HNSW_PROFILE=1")
	}
	ensureVectorBenchDB(t)
	ctx := context.Background()

	dims := 128
	size := 1000
	k := 10

	vecIdx := NewVectorIndex("vec_data", KeyWithValue(Field("vector_data"), 0), dims)
	vecIdx.Options["hnswUseRaBitQ"] = "true"
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", vecIdx)
	md, _ := builder.Build()

	ss := subspace.FromBytes(tuple.Tuple{"hnsw_profile", time.Now().UnixNano()}.Pack())
	rng := rand.New(rand.NewSource(42))

	t.Logf("Inserting %d vectors...", size)
	vecInsertVectors(t, vectorBenchDB, md, ss, size, dims, rng)

	queryRng := rand.New(rand.NewSource(99))

	for _, ef := range []int{10, 16, 32, 64} {
		t.Logf("")
		t.Logf("=== ef=%d, k=%d, %d vectors ===", ef, k, size)

		// Run 20 searches, collect stats from each.
		var totalGets, totalBatch, totalRange, totalCache int64
		var totalDur time.Duration
		numQueries := 20

		for qi := 0; qi < numQueries; qi++ {
			q := vecRandomVector(queryRng, dims)
			stats := &HNSWStats{}

			start := time.Now()
			vectorBenchDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, _ := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
				maintainer := store.getIndexMaintainer(vecIdx)
				vm := maintainer.(*vectorIndexMaintainer)
				storage := vm.getStorageForPrefix(nil)
				graph := NewHNSWGraph(storage, vm.hnswConfig)
				graph.SetStats(stats)
				graph.Search(rtx.Transaction().Snapshot(), q, k, ef)
				return nil, nil
			})
			totalDur += time.Since(start)
			totalGets += stats.FDBGets.Load()
			totalBatch += stats.FDBBatchGets.Load()
			totalRange += stats.FDBRangeReads.Load()
			totalCache += stats.CacheHits.Load()
		}

		avgDur := totalDur / time.Duration(numQueries)
		avgGets := float64(totalGets) / float64(numQueries)
		avgBatch := float64(totalBatch) / float64(numQueries)
		avgRange := float64(totalRange) / float64(numQueries)
		avgCache := float64(totalCache) / float64(numQueries)
		avgRT := avgGets + avgBatch + avgRange // each is ~1 network round-trip

		t.Logf("  Avg latency:       %v", avgDur)
		t.Logf("  Avg round-trips:   %.1f  (%.1f point + %.1f batch + %.1f range)", avgRT, avgGets, avgBatch, avgRange)
		t.Logf("  Avg cache hits:    %.0f", avgCache)
		t.Logf("  ~ms per RT:        %.2f", float64(avgDur.Microseconds())/avgRT/1000)
	}
	_ = fmt.Sprintf("")
}
