package bench

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// TestSPFreshMultiTenantSoak is the many-tenant aggregate proof (TODO.md
// "SPFresh multi-tenant scale-out" item 4): T independent tenant stores fill
// CONCURRENTLY through the production SaveRecord path (§6b cold start, no
// bulk build) while a sweeper fleet — not per-tenant rebalancer loops —
// maintains all of them with bounded per-tenant budgets. Then every tenant's
// index must verify: queue drained, recall vs per-tenant brute force.
//
// Every prior number was single-tenant; the dimensions only this test
// exercises are cross-tenant isolation under concurrent maintenance, sweeper
// fairness with many busy tenants, and the pending-work probe at fleet scale.
//
//	SPFRESH_BENCH=1 SIFT_TENANTS=20 SIFT_TENANT_N=2000 bazelisk test \
//	  //pkg/recordlayer/bench:bench_test \
//	  --test_arg="--test.run=TestSPFreshMultiTenantSoak" --test_output=streamed \
//	  --test_env=SPFRESH_BENCH --test_env=SIFT_TENANTS --test_env=SIFT_TENANT_N
func TestSPFreshMultiTenantSoak(t *testing.T) {
	if os.Getenv("SPFRESH_BENCH") != "1" {
		t.Skip("set SPFRESH_BENCH=1 to run the SPFresh multi-tenant soak")
	}
	tenantCount := siftEnvInt("SIFT_TENANTS", 20)
	perTenant := siftEnvInt("SIFT_TENANT_N", 2000)
	sweepers := siftEnvInt("SIFT_SWEEPERS", 2)
	k := siftEnvInt("SIFT_K", 10)

	siftDir := resolveSIFTDir()
	need := tenantCount * perTenant
	baseVecs, err := LoadFVecs(siftDir+"/sift_base.fvecs", need)
	if err != nil {
		t.Fatalf("load base vectors: %v", err)
	}
	if len(baseVecs) < need {
		t.Fatalf("need %d vectors, file has %d", need, len(baseVecs))
	}
	queryVecs, err := LoadFVecs(siftDir+"/sift_query.fvecs", 20)
	if err != nil {
		t.Fatalf("load query vectors: %v", err)
	}

	ensureVectorBenchDB(t)
	ctx := context.Background()

	// One store + one SPFresh index per tenant, disjoint slices of SIFT.
	type tenantState struct {
		recordlayer.SPFreshTenant
		base [][]float64 // this tenant's vectors (by order_id - 1)
	}
	tenants := make([]tenantState, tenantCount)
	spfIdx := recordlayer.NewIndex("spf_mt",
		recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0))
	spfIdx.Type = recordlayer.IndexTypeVectorSPFresh
	spfIdx.Options = map[string]string{recordlayer.IndexOptionSPFreshNumDimensions: "128"}
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", spfIdx)
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	stamp := time.Now().UnixNano()
	for ti := 0; ti < tenantCount; ti++ {
		ss := vecBenchSubspace(fmt.Sprintf("spfresh-mt-%d-%d", stamp, ti))
		sb := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
			return recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		}
		lo := ti * perTenant
		base := make([][]float64, perTenant)
		for i := 0; i < perTenant; i++ {
			base[i] = float32sToFloat64s(baseVecs[lo+i])
		}
		tenants[ti] = tenantState{
			SPFreshTenant: recordlayer.SPFreshTenant{StoreBuilder: sb, IndexName: "spf_mt"},
			base:          base,
		}
	}
	sweepTenants := make([]recordlayer.SPFreshTenant, tenantCount)
	for i := range tenants {
		sweepTenants[i] = tenants[i].SPFreshTenant
	}

	// Fill: 4 writers round-robin across ALL tenants (interleaved cross-
	// tenant load — the fleet shape), sweepers maintaining alongside.
	t.Logf("MT soak: %d tenants × %d vectors, %d sweepers", tenantCount, perTenant, sweepers)
	fillStart := time.Now()
	var sweepDone atomic.Bool
	var sweepActions atomic.Int64
	var sweeperWG sync.WaitGroup
	for s := 0; s < sweepers; s++ {
		sweeperWG.Add(1)
		go func() {
			defer sweeperWG.Done()
			for !sweepDone.Load() {
				res, serr := recordlayer.SweepSPFreshIndexes(ctx, vectorBenchDB, sweepTenants, recordlayer.SPFreshSweepOptions{MaxRoundsPerTenant: 4})
				if serr != nil {
					t.Errorf("sweep: %v", serr)
					return
				}
				sweepActions.Add(int64(res.Actions))
				if res.Actions == 0 {
					time.Sleep(50 * time.Millisecond)
				}
			}
		}()
	}

	const writers = 4
	const batch = 100
	var writerWG sync.WaitGroup
	var nextChunk atomic.Int64 // global chunk cursor: tenant-interleaved
	chunksPerTenant := (perTenant + batch - 1) / batch
	totalChunks := chunksPerTenant * tenantCount
	for w := 0; w < writers; w++ {
		writerWG.Add(1)
		go func() {
			defer writerWG.Done()
			for {
				chunk := int(nextChunk.Add(1) - 1)
				if chunk >= totalChunks {
					return
				}
				// Round-robin tenants: consecutive chunks hit different
				// tenants, so writers continuously interleave across stores.
				ti := chunk % tenantCount
				ci := chunk / tenantCount
				lo := ci * batch
				hi := min(lo+batch, perTenant)
				tn := &tenants[ti]
				if _, werr := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, serr := tn.StoreBuilder(rtx)
					if serr != nil {
						return nil, serr
					}
					for i := lo; i < hi; i++ {
						if _, serr := store.SaveRecord(&gen.Order{
							OrderId:    proto.Int64(int64(i + 1)),
							VectorData: recordlayer.SerializeVector(tn.base[i]),
						}); serr != nil {
							return nil, serr
						}
					}
					return nil, nil
				}); werr != nil {
					t.Errorf("writer tenant %d chunk %d: %v", ti, ci, werr)
					return
				}
			}
		}()
	}
	writerWG.Wait()
	if t.Failed() {
		sweepDone.Store(true)
		sweeperWG.Wait()
		t.Fatal("writers failed")
	}

	// Drain the whole fleet to quiescence, then stop the sweepers.
	for pass := 0; pass < 200; pass++ {
		res, serr := recordlayer.SweepSPFreshIndexes(ctx, vectorBenchDB, sweepTenants, recordlayer.SPFreshSweepOptions{MaxRoundsPerTenant: 8})
		if serr != nil {
			t.Fatalf("final drain: %v", serr)
		}
		sweepActions.Add(int64(res.Actions))
		if res.Worked == 0 {
			break
		}
	}
	sweepDone.Store(true)
	sweeperWG.Wait()
	fillDur := time.Since(fillStart)
	t.Logf("FILL: %d tenants × %d in %v (%.0f vec/s aggregate, %d sweep actions)",
		tenantCount, perTenant, fillDur, float64(need)/fillDur.Seconds(), sweepActions.Load())

	// Per-tenant verification: probe says quiet; recall vs THIS tenant's
	// brute force (cross-tenant leakage would crater it: the queries are
	// shared but every tenant's truth differs).
	type sbd interface {
		ScanByDistance(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) recordlayer.RecordCursor[*recordlayer.IndexEntry]
	}
	worstRecall := 1.0
	for ti := range tenants {
		tn := &tenants[ti]
		pending, perr := recordlayer.SPFreshHasPendingMaintenance(ctx, vectorBenchDB, tn.StoreBuilder, tn.IndexName)
		if perr != nil {
			t.Fatalf("tenant %d probe: %v", ti, perr)
		}
		if pending {
			t.Errorf("tenant %d still has pending maintenance after fleet drain", ti)
		}
		hits, total := 0, 0
		for qi := range queryVecs {
			query := float32sToFloat64s(queryVecs[qi])
			want := bruteForceIDs(tn.base, query, k)
			wantSet := make(map[int64]bool, k)
			for _, id := range want {
				wantSet[id+1] = true // order_id = base index + 1
			}
			var got []int64
			if _, qerr := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, serr := tn.StoreBuilder(rtx)
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
			}); qerr != nil {
				t.Fatalf("tenant %d query %d: %v", ti, qi, qerr)
			}
			for _, id := range got {
				if wantSet[id] {
					hits++
				}
				total++
			}
		}
		recall := float64(hits) / float64(total)
		if recall < worstRecall {
			worstRecall = recall
		}
		if recall < 0.90 {
			t.Errorf("tenant %d recall@%d = %.4f, want >= 0.90", ti, k, recall)
		}
	}
	t.Logf("VERIFY: %d tenants, worst recall@%d = %.4f", tenantCount, k, worstRecall)
}
