package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// TestSPFreshSIFTBenchmark is the 094.1 recall/latency benchmark (RFC-094
// §9/§13): SIFT vectors -> records -> BuildSPFreshIndex -> kNN through the
// maintainer's ScanByDistance, with recall against ground truth (or brute
// force for subsets) and latency percentiles. Env-gated like the HNSW SIFT
// benchmark so CI stays fast; A/B against TestSIFTBenchmark at the same N.
//
//	SPFRESH_BENCH=1 SIFT_N=50000 bazelisk test //pkg/recordlayer/bench:bench_test \
//	  --test_arg="--test.run=TestSPFreshSIFTBenchmark" --test_output=streamed \
//	  --test_env=SPFRESH_BENCH --test_env=SIFT_N
func TestSPFreshSIFTBenchmark(t *testing.T) {
	if os.Getenv("SPFRESH_BENCH") != "1" {
		t.Skip("set SPFRESH_BENCH=1 to run the SPFresh SIFT benchmark")
	}

	n := siftEnvInt("SIFT_N", 10000)
	k := siftEnvInt("SIFT_K", 10)
	numQueries := siftEnvInt("SIFT_NUM_QUERIES", 100)
	batchSize := siftEnvInt("SIFT_BATCH_SIZE", 200)

	siftDir := resolveSIFTDir()
	baseVecs, err := LoadFVecs(filepath.Join(siftDir, "sift_base.fvecs"), n)
	if err != nil {
		t.Fatalf("load base vectors: %v", err)
	}
	queryVecs, err := LoadFVecs(filepath.Join(siftDir, "sift_query.fvecs"), numQueries)
	if err != nil {
		t.Fatalf("load query vectors: %v", err)
	}
	if len(baseVecs) < n {
		t.Fatalf("requested %d base vectors, file has %d", n, len(baseVecs))
	}
	baseF64 := make([][]float64, n)
	for i, v := range baseVecs {
		baseF64[i] = float32sToFloat64s(v)
	}
	t.Logf("SPFresh SIFT: N=%d dims=%d k=%d queries=%d", n, len(baseVecs[0]), k, len(queryVecs))

	ensureVectorBenchDB(t)
	ctx := context.Background()

	// Metadata: SPFresh index over the 128D vector_data column.
	spfIdx := recordlayer.NewIndex("spf_data",
		recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0))
	spfIdx.Type = recordlayer.IndexTypeVectorSPFresh
	spfIdx.Options = map[string]string{
		recordlayer.IndexOptionSPFreshNumDimensions: "128",
	}
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", spfIdx)
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	ss := vecBenchSubspace(fmt.Sprintf("spfresh-sift-%d", time.Now().UnixNano()))
	storeBuilder := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
		return recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
	}

	// Load records with the index disabled (build-then-read, 094.1).
	_, err = vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return nil, serr
		}
		_, serr = store.MarkIndexDisabled("spf_data")
		return nil, serr
	})
	if err != nil {
		t.Fatalf("disable index: %v", err)
	}
	loadStart := time.Now()
	for lo := 0; lo < n; lo += batchSize {
		hi := min(lo+batchSize, n)
		_, err = vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			for i := lo; i < hi; i++ {
				if _, serr := store.SaveRecord(&gen.Order{
					OrderId:    proto.Int64(int64(i)),
					VectorData: recordlayer.SerializeVector(baseF64[i]),
				}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("load batch at %d: %v", lo, err)
		}
	}
	t.Logf("records loaded in %v", time.Since(loadStart))

	// Bulk build.
	buildStart := time.Now()
	if err := recordlayer.BuildSPFreshIndex(ctx, vectorBenchDB, storeBuilder, "spf_data", 42); err != nil {
		t.Fatalf("build: %v", err)
	}
	buildDur := time.Since(buildStart)
	_, err = vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return nil, serr
		}
		_, serr = store.MarkIndexReadable("spf_data")
		return nil, serr
	})
	if err != nil {
		t.Fatalf("mark readable: %v", err)
	}
	t.Logf("BUILD: %d vectors in %v (%.0f vec/sec)", n, buildDur, float64(n)/buildDur.Seconds())

	// Queries: recall vs brute force over the subset + latency percentiles.
	type sbd interface {
		ScanByDistance(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) recordlayer.RecordCursor[*recordlayer.IndexEntry]
	}
	latencies := make([]time.Duration, 0, len(queryVecs))
	hits, total := 0, 0
	for qi, qv := range queryVecs {
		query := float32sToFloat64s(qv)
		want := bruteForceIDs(baseF64, query, k)

		qStart := time.Now()
		var got []int64
		_, err = vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			maintainer, merr := store.GetIndexMaintainer(spfIdx)
			if merr != nil {
				return nil, merr
			}
			cursor := maintainer.(sbd).ScanByDistance(recordlayer.TupleRange{
				Low:  tuple.Tuple{recordlayer.SerializeVector(query)},
				High: tuple.Tuple{int64(k)},
			}, nil, recordlayer.ScanProperties{})
			got = got[:0]
			for {
				res, cerr := cursor.OnNext(ctx)
				if cerr != nil {
					return nil, cerr
				}
				if !res.HasNext() {
					break
				}
				got = append(got, res.GetValue().Key[0].(int64))
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("query %d: %v", qi, err)
		}
		latencies = append(latencies, time.Since(qStart))

		wantSet := make(map[int64]bool, k)
		for _, id := range want {
			wantSet[id] = true
		}
		for _, id := range got {
			if wantSet[id] {
				hits++
			}
			total++
		}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	recall := float64(hits) / float64(total)
	p50 := latencies[len(latencies)/2]
	p99 := latencies[len(latencies)*99/100]
	t.Logf("QUERY: recall@%d=%.4f p50=%v p99=%v (n=%d queries=%d)", k, recall, p50, p99, n, len(latencies))

	if recall < 0.90 {
		t.Errorf("recall@%d = %.4f, want >= 0.90 (RFC-094 §9 SIFT target is 0.95 tuned)", k, recall)
	}
}

// bruteForceIDs computes exact kNN ids over the base set.
func bruteForceIDs(base [][]float64, query []float64, k int) []int64 {
	type idD struct {
		id int64
		d  float64
	}
	all := make([]idD, len(base))
	for i, v := range base {
		var d float64
		for j := range query {
			diff := query[j] - v[j]
			d += diff * diff
		}
		all[i] = idD{id: int64(i), d: d}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].d != all[j].d {
			return all[i].d < all[j].d
		}
		return all[i].id < all[j].id
	})
	ids := make([]int64, k)
	for i := 0; i < k; i++ {
		ids[i] = all[i].id
	}
	return ids
}
