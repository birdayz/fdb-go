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

// TestSPFreshChurnSoak is the 094.5 churn soak (RFC-094 §12/§13): build from
// SIFT, then run delete/re-insert churn waves with continuous rebalancing,
// sampling recall ONLINE after each wave. The index must hold recall under
// sustained churn — the failure mode this hunts is slow topology decay
// (orphaned copies, drifting centroids, counter rot) that no single-op test
// can see. Env-gated; scale via SIFT_N / SOAK_WAVES / SOAK_CHURN.
//
//	SPFRESH_BENCH=1 SIFT_N=100000 SOAK_WAVES=10 bazelisk test \
//	  //pkg/recordlayer/bench:bench_test --test_arg="--test.run=TestSPFreshChurnSoak" \
//	  --test_output=streamed --test_env=SPFRESH_BENCH --test_env=SIFT_N --test_env=SOAK_WAVES
func TestSPFreshChurnSoak(t *testing.T) {
	if os.Getenv("SPFRESH_BENCH") != "1" {
		t.Skip("set SPFRESH_BENCH=1 to run the SPFresh churn soak")
	}
	n := siftEnvInt("SIFT_N", 20000)
	waves := siftEnvInt("SOAK_WAVES", 6)
	churn := siftEnvInt("SOAK_CHURN", n/10) // records deleted+reinserted per wave
	numQueries := siftEnvInt("SIFT_NUM_QUERIES", 50)
	k := siftEnvInt("SIFT_K", 10)

	siftDir := resolveSIFTDir()
	baseVecs, err := LoadFVecs(filepath.Join(siftDir, "sift_base.fvecs"), n)
	if err != nil {
		t.Fatalf("load base vectors: %v", err)
	}
	queryVecs, err := LoadFVecs(filepath.Join(siftDir, "sift_query.fvecs"), numQueries)
	if err != nil {
		t.Fatalf("load query vectors: %v", err)
	}
	baseF64 := make([][]float64, n)
	for i, v := range baseVecs {
		baseF64[i] = float32sToFloat64s(v)
	}

	ensureVectorBenchDB(t)
	ctx := context.Background()
	spfIdx := recordlayer.NewIndex("spf_soak",
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
	ss := vecBenchSubspace(fmt.Sprintf("spfresh-soak-%d", time.Now().UnixNano()))
	storeBuilder := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
		return recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
	}

	// Build-then-read baseline.
	_, err = vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return nil, serr
		}
		_, serr = store.MarkIndexDisabled("spf_soak")
		return nil, serr
	})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	for lo := 0; lo < n; lo += 200 {
		hi := min(lo+200, n)
		if _, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			for i := lo; i < hi; i++ {
				if _, serr := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)), VectorData: recordlayer.SerializeVector(baseF64[i]),
				}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("load batch %d: %v", lo, err)
		}
	}
	if err := recordlayer.BuildSPFreshIndex(ctx, vectorBenchDB, storeBuilder, "spf_soak", 42); err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return nil, serr
		}
		_, serr = store.MarkIndexReadable("spf_soak")
		return nil, serr
	}); err != nil {
		t.Fatalf("mark readable: %v", err)
	}

	// The online recall sampler (094.5): measured against brute force over
	// the CURRENT live set.
	type sbd interface {
		ScanByDistance(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) recordlayer.RecordCursor[*recordlayer.IndexEntry]
	}
	sampleRecall := func(live map[int64][]float64) float64 {
		liveVecs := make([][]float64, 0, len(live))
		liveIDs := make([]int64, 0, len(live))
		for id, v := range live {
			liveIDs = append(liveIDs, id)
			liveVecs = append(liveVecs, v)
		}
		hits, total := 0, 0
		for _, qv := range queryVecs {
			query := float32sToFloat64s(qv)
			type idD struct {
				id int64
				d  float64
			}
			all := make([]idD, len(liveVecs))
			for i, v := range liveVecs {
				var d float64
				for j := range query {
					diff := query[j] - v[j]
					d += diff * diff
				}
				all[i] = idD{id: liveIDs[i], d: d}
			}
			sort.Slice(all, func(i, j int) bool {
				if all[i].d != all[j].d {
					return all[i].d < all[j].d
				}
				return all[i].id < all[j].id
			})
			wantSet := map[int64]bool{}
			for i := 0; i < k && i < len(all); i++ {
				wantSet[all[i].id] = true
			}
			var got []int64
			if _, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
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
			}); err != nil {
				t.Fatalf("recall query: %v", err)
			}
			for _, id := range got {
				if wantSet[id] {
					hits++
				}
				total++
			}
		}
		return float64(hits) / float64(total)
	}

	live := make(map[int64][]float64, n)
	for i := 0; i < n; i++ {
		live[int64(i)] = baseF64[i]
	}
	base := sampleRecall(live)
	t.Logf("SOAK wave 0 (post-build): recall@%d=%.4f live=%d", k, base, len(live))

	// Churn waves: delete a rotating slice, re-insert it shifted (new ids,
	// same vectors — net size constant), rebalance, sample.
	nextID := int64(n)
	for wave := 1; wave <= waves; wave++ {
		start := ((wave - 1) * churn) % n
		victims := make([]int64, 0, churn)
		for id := range live {
			if int(id)%n >= start && int(id)%n < start+churn {
				victims = append(victims, id)
			}
			if len(victims) >= churn {
				break
			}
		}
		for lo := 0; lo < len(victims); lo += 100 {
			hi := min(lo+100, len(victims))
			if _, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, serr := storeBuilder(rtx)
				if serr != nil {
					return nil, serr
				}
				for _, id := range victims[lo:hi] {
					if _, derr := store.DeleteRecord(tuple.Tuple{id}); derr != nil {
						return nil, derr
					}
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("wave %d delete: %v", wave, err)
			}
		}
		reinserts := make(map[int64][]float64, len(victims))
		for _, id := range victims {
			v := live[id]
			delete(live, id)
			reinserts[nextID] = v
			nextID++
		}
		ids := make([]int64, 0, len(reinserts))
		for id := range reinserts {
			ids = append(ids, id)
		}
		for lo := 0; lo < len(ids); lo += 100 {
			hi := min(lo+100, len(ids))
			if _, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, serr := storeBuilder(rtx)
				if serr != nil {
					return nil, serr
				}
				for _, id := range ids[lo:hi] {
					if _, serr := store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(id), VectorData: recordlayer.SerializeVector(reinserts[id]),
					}); serr != nil {
						return nil, serr
					}
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("wave %d insert: %v", wave, err)
			}
		}
		for id, v := range reinserts {
			live[id] = v
		}
		actions, err := recordlayer.RebalanceSPFreshIndex(ctx, vectorBenchDB, storeBuilder, "spf_soak")
		if err != nil {
			t.Fatalf("wave %d rebalance: %v", wave, err)
		}
		r := sampleRecall(live)
		t.Logf("SOAK wave %d: recall@%d=%.4f live=%d rebalanceActions=%d", wave, k, r, len(live), actions)
		if r < base-0.05 {
			t.Errorf("wave %d: recall %.4f decayed >5pp below post-build %.4f — topology rot under churn", wave, r, base)
		}
	}
}
