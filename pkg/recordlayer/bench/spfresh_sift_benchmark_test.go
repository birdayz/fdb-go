package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// SIFT_REPLICATION / SIFT_ALPHA drive the α-led replication sweep
	// (paper-ACK follow-up): bulk builds give a clean comparable series.
	if rep := os.Getenv("SIFT_REPLICATION"); rep != "" {
		spfIdx.Options[recordlayer.IndexOptionSPFreshReplication] = rep
	}
	if alpha := os.Getenv("SIFT_ALPHA"); alpha != "" {
		spfIdx.Options[recordlayer.IndexOptionSPFreshAlpha] = alpha
	}
	// SIFT_BUILD_W sweeps the wave-B assignment width w_b (RFC-099): a large
	// value == the old flat scan (gathers all cells), the default 48 == two-level.
	if bw := os.Getenv("SIFT_BUILD_W"); bw != "" {
		spfIdx.Options[recordlayer.IndexOptionSPFreshBuildAssignCells] = bw
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
	if _, terr := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return nil, serr
		}
		// Effective replication (entries/N) is the x-axis of the α-led
		// replication sweep — log it for every build.
		t.Logf("TOPOLOGY: %s", recordlayer.SPFreshDebugTopology(rtx, store, "spf_data"))
		return nil, nil
	}); terr != nil {
		t.Fatalf("topology dump: %v", terr)
	}

	// Queries: recall vs brute force over the subset + latency percentiles.
	// SIFT_SWEEP=w:kc:c[,w:kc:c...] additionally sweeps searcher parameters
	// against the SAME built index (094.4 tuning) and logs one line each.
	if sweep := os.Getenv("SIFT_SWEEP"); sweep != "" {
		qf := make([][]float64, len(queryVecs))
		for i, qv := range queryVecs {
			qf[i] = float32sToFloat64s(qv)
		}
		runSIFTSweep(t, ctx, storeBuilder, bruteForceIDsStreaming(sliceSource{base: baseF64}, qf, k), queryVecs, k, sweep, "spf_data")
	}
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

// runSIFTSweep measures recall/latency for each w:kc:c[:eps] configuration
// against the already-built index (094.4 tuning; the knobs ride the scan
// contract's High tuple). One log line per config. idxName selects the index
// (bulk build and foreground fill register under different names).
func runSIFTSweep(t *testing.T, ctx context.Context, storeBuilder func(*recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error), wants [][]int64, queries [][]float32, k int, sweep, idxName string) {
	type sbd interface {
		ScanByDistance(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) recordlayer.RecordCursor[*recordlayer.IndexEntry]
	}
	// SIFT_AMORTIZE=N batches N queries per transaction (one GRV + one store
	// open shared) — the production shape for query servers, and the
	// breakdown that separates harness overhead from index reads.
	amortize := siftEnvInt("SIFT_AMORTIZE", 1)
	if amortize < 1 {
		amortize = 1
	}
	for _, cfg := range strings.Split(sweep, ",") {
		parts := strings.Split(cfg, ":")
		if len(parts) != 3 && len(parts) != 4 {
			t.Fatalf("SIFT_SWEEP entry %q: want w:kc:c[:eps]", cfg)
		}
		w, _ := strconv.Atoi(parts[0])
		kc, _ := strconv.Atoi(parts[1])
		c, _ := strconv.Atoi(parts[2])
		high := tuple.Tuple{int64(k), int64(kc), int64(w), int64(c)}
		epsLabel := "default"
		if len(parts) == 4 {
			eps, perr := strconv.ParseFloat(parts[3], 64)
			if perr != nil {
				t.Fatalf("SIFT_SWEEP entry %q: bad eps: %v", cfg, perr)
			}
			high = append(high, eps)
			epsLabel = parts[3]
		}
		latencies := make([]time.Duration, 0, len(queries))
		hits, total := 0, 0
		for lo := 0; lo < len(queries); lo += amortize {
			hi := lo + amortize
			if hi > len(queries) {
				hi = len(queries)
			}
			batch := queries[lo:hi]
			batchLat := make([]time.Duration, 0, len(batch))
			batchHits, batchTotal := 0, 0
			_, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				// Per-attempt staging: the closure may retry.
				batchLat = batchLat[:0]
				batchHits, batchTotal = 0, 0
				store, serr := storeBuilder(rtx)
				if serr != nil {
					return nil, serr
				}
				idx := store.GetMetaData().GetIndex(idxName)
				maintainer, merr := store.GetIndexMaintainer(idx)
				if merr != nil {
					return nil, merr
				}
				for bi, qv := range batch {
					query := float32sToFloat64s(qv)
					qStart := time.Now()
					cursor := maintainer.(sbd).ScanByDistance(recordlayer.TupleRange{
						Low:  tuple.Tuple{recordlayer.SerializeVector(query)},
						High: high,
					}, nil, recordlayer.ScanProperties{})
					var got []int64
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
					batchLat = append(batchLat, time.Since(qStart))
					wantSet := make(map[int64]bool, k)
					for _, id := range wants[lo+bi] {
						wantSet[id] = true
					}
					for _, id := range got {
						if wantSet[id] {
							batchHits++
						}
						batchTotal++
					}
				}
				return nil, nil
			})
			if err != nil {
				t.Fatalf("sweep %s batch at %d: %v", cfg, lo, err)
			}
			hits += batchHits
			total += batchTotal
			latencies = append(latencies, batchLat...)
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		t.Logf("SWEEP w=%d kc=%d c=%d eps=%s amortize=%d: recall@%d=%.4f p50=%v p99=%v", w, kc, c, epsLabel, amortize, k,
			float64(hits)/float64(total), latencies[len(latencies)/2], latencies[len(latencies)*99/100])

		// SIFT_QPS=G additionally hammers this config with G concurrent
		// query workers (one query per transaction — the serving shape) and
		// reports aggregate throughput. Every read number without this was
		// single-threaded latency; queries are stateless snapshot reads, so
		// this measures the real capacity of one client process.
		if qpsG := siftEnvInt("SIFT_QPS", 0); qpsG > 0 {
			const totalQ = 800
			var next atomic.Int64
			var wg sync.WaitGroup
			qpsStart := time.Now()
			for g := 0; g < qpsG; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						i := int(next.Add(1) - 1)
						if i >= totalQ {
							return
						}
						query := float32sToFloat64s(queries[i%len(queries)])
						if _, qerr := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
							store, serr := storeBuilder(rtx)
							if serr != nil {
								return nil, serr
							}
							maintainer, merr := store.GetIndexMaintainer(store.GetMetaData().GetIndex(idxName))
							if merr != nil {
								return nil, merr
							}
							cursor := maintainer.(sbd).ScanByDistance(recordlayer.TupleRange{
								Low:  tuple.Tuple{recordlayer.SerializeVector(query)},
								High: high,
							}, nil, recordlayer.ScanProperties{})
							for {
								res, cerr := cursor.OnNext(ctx)
								if cerr != nil {
									return nil, cerr
								}
								if !res.HasNext() {
									return nil, nil
								}
							}
						}); qerr != nil {
							t.Errorf("qps worker query %d: %v", i, qerr)
							return
						}
					}
				}()
			}
			wg.Wait()
			elapsed := time.Since(qpsStart)
			t.Logf("SWEEP-QPS w=%d kc=%d c=%d eps=%s G=%d: %d queries in %v → %.0f QPS", w, kc, c, epsLabel, qpsG,
				totalQ, elapsed.Round(time.Millisecond), float64(totalQ)/elapsed.Seconds())
		}
	}
}

// TestSPFreshForegroundFillBenchmark measures the PRODUCTION write path: fill
// the index from zero through plain SaveRecord (§6b cold start — no bulk
// build) with concurrent writers and an in-process rebalancer looping beside
// them (the RFC-094 §6 deployment shape), then measure read latency + recall
// on the grown index. The three numbers the phase cares about: write
// throughput to fill N, query p50/p99 at N, recall@10 at N.
//
//	SPFRESH_BENCH=1 SIFT_N=100000 bazelisk test //pkg/recordlayer/bench:bench_test \
//	  --test_arg="--test.run=TestSPFreshForegroundFillBenchmark" --test_output=streamed \
//	  --test_env=SPFRESH_BENCH --test_env=SIFT_N
func TestSPFreshForegroundFillBenchmark(t *testing.T) {
	if os.Getenv("SPFRESH_BENCH") != "1" {
		t.Skip("set SPFRESH_BENCH=1 to run the SPFresh foreground fill benchmark")
	}
	n := siftEnvInt("SIFT_N", 100000)
	k := siftEnvInt("SIFT_K", 10)
	numQueries := siftEnvInt("SIFT_NUM_QUERIES", 100)
	batchSize := siftEnvInt("SIFT_BATCH_SIZE", 200)
	writers := siftEnvInt("SIFT_WRITERS", 4)

	var src vectorSource
	var queryVecs [][]float32
	if os.Getenv("SIFT_SYNTHETIC") == "1" {
		// Scales past the SIFT-1M file (the 10M soak): a deterministic
		// SIFT-shaped Gaussian mixture, STREAMED — vectors regenerate per
		// index on demand. Materializing the float64 dataset (10.24 GB at
		// 10M) OOM-killed the harness twice; only the mixture centers
		// (~2 MB) live in memory.
		s := newSynthSource(n, 128, 424242)
		src = s
		queryVecs = queriesFromSource(s, numQueries, 424242)
		t.Logf("SYNTHETIC dataset: N=%d (Gaussian mixture, 128-D, streaming)", n)
	} else {
		siftDir := resolveSIFTDir()
		baseVecs, err := LoadFVecs(filepath.Join(siftDir, "sift_base.fvecs"), n)
		if err != nil {
			t.Fatalf("load base vectors: %v", err)
		}
		queryVecs, err = LoadFVecs(filepath.Join(siftDir, "sift_query.fvecs"), numQueries)
		if err != nil {
			t.Fatalf("load query vectors: %v", err)
		}
		baseF64 := make([][]float64, n)
		for i, v := range baseVecs {
			baseF64[i] = float32sToFloat64s(v)
		}
		src = sliceSource{base: baseF64}
	}
	recordlayer.SPFreshEnableAudit()
	defer recordlayer.SPFreshDisableAudit()
	t.Logf("SPFresh foreground fill: N=%d writers=%d batch=%d", n, writers, batchSize)

	ensureVectorBenchDB(t)
	ctx := context.Background()
	spfIdx := recordlayer.NewIndex("spf_fill",
		recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0))
	spfIdx.Type = recordlayer.IndexTypeVectorSPFresh
	spfIdx.Options = map[string]string{
		recordlayer.IndexOptionSPFreshNumDimensions: "128",
	}
	// SIFT_REPLICATION / SIFT_ALPHA override the closure knobs for the
	// replication A/B runs (paper review F3: r=4 vs the r=2 default).
	if rep := os.Getenv("SIFT_REPLICATION"); rep != "" {
		spfIdx.Options[recordlayer.IndexOptionSPFreshReplication] = rep
	}
	if alpha := os.Getenv("SIFT_ALPHA"); alpha != "" {
		spfIdx.Options[recordlayer.IndexOptionSPFreshAlpha] = alpha
	}
	if lmax := os.Getenv("SIFT_LMAX"); lmax != "" {
		spfIdx.Options[recordlayer.IndexOptionSPFreshLmax] = lmax
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
	ss := vecBenchSubspace(fmt.Sprintf("spfresh-fill-%d", time.Now().UnixNano()))
	storeBuilder := func(rtx *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
		return recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
	}

	// Foreground fill: index READABLE from record one; writers split the id
	// space; a rebalancer loops beside them (the in-process-on-writers shape).
	fillStart := time.Now()
	var wg sync.WaitGroup
	var fillDone atomic.Bool
	var rebalanceActions atomic.Int64
	errs := make(chan error, writers+1)
	per := n / writers
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			vbuf := make([]float64, src.dimensions())
			lo, hi := w*per, (w+1)*per
			if w == writers-1 {
				hi = n
			}
			for b := lo; b < hi; b += batchSize {
				be := min(b+batchSize, hi)
				if _, werr := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, serr := storeBuilder(rtx)
					if serr != nil {
						return nil, serr
					}
					for i := b; i < be; i++ {
						src.at(i, vbuf)
						if _, serr := store.SaveRecord(&gen.Order{
							OrderId: proto.Int64(int64(i)), VectorData: recordlayer.SerializeVector(vbuf),
						}); serr != nil {
							return nil, serr
						}
					}
					return nil, nil
				}); werr != nil {
					errs <- fmt.Errorf("writer %d batch %d: %w", w, b, werr)
					return
				}
			}
		}(w)
	}
	var rebalancerWG sync.WaitGroup
	rebalancerWG.Add(1)
	go func() {
		defer rebalancerWG.Done()
		for !fillDone.Load() {
			acts, rerr := recordlayer.RebalanceSPFreshIndex(ctx, vectorBenchDB, storeBuilder, "spf_fill")
			if rerr != nil {
				errs <- fmt.Errorf("rebalancer: %w", rerr)
				return
			}
			rebalanceActions.Add(int64(acts))
			if acts == 0 {
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	wg.Wait()
	fillDone.Store(true)
	// Join the rebalancer BEFORE the final drain: overlapping executors are
	// exactly the lease-race the unique-owner fix guards against, and the
	// benchmark should measure the intended single-drain shape.
	rebalancerWG.Wait()
	select {
	case e := <-errs:
		t.Fatal(e)
	default:
	}
	// Drain the remaining task queue (counts toward fill time: the index
	// isn't "filled" until maintenance quiesces).
	acts, err := recordlayer.RebalanceSPFreshIndex(ctx, vectorBenchDB, storeBuilder, "spf_fill")
	if err != nil {
		t.Fatalf("final rebalance: %v", err)
	}
	rebalanceActions.Add(int64(acts))
	fillDur := time.Since(fillStart)
	t.Logf("FILL: %d vectors in %v (%.0f vec/sec, %d writers, batch %d, %d rebalance actions)",
		n, fillDur, float64(n)/fillDur.Seconds(), writers, batchSize, rebalanceActions.Load())
	if _, err := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return nil, serr
		}
		t.Logf("TOPOLOGY: %s", recordlayer.SPFreshDebugTopology(rtx, store, "spf_fill"))
		t.Logf("INTEGRITY: %s", recordlayer.SPFreshDebugIntegrity(rtx, store, "spf_fill", 100))
		return nil, nil
	}); err != nil {
		t.Fatalf("topology dump: %v", err)
	}

	// Ground truth for ALL queries in one streaming pass over the source
	// (O(queries × k) memory at any scale), shared by the read points and
	// the sweep below.
	queriesF64 := make([][]float64, len(queryVecs))
	for i, qv := range queryVecs {
		queriesF64[i] = float32sToFloat64s(qv)
	}
	gtStart := time.Now()
	wants := bruteForceIDsStreaming(src, queriesF64, k)
	t.Logf("ground truth: %d queries in %v (streaming)", len(queryVecs), time.Since(gtStart))

	// Reads + recall on the grown index, tuned default and fast points.
	type sbd interface {
		ScanByDistance(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) recordlayer.RecordCursor[*recordlayer.IndexEntry]
	}
	for _, cfg := range []struct {
		name     string
		kc, w, c int
	}{
		{"default(32/64/200)", 0, 0, 0},
		{"fast(16/24/64)", 24, 16, 64},
	} {
		latencies := make([]time.Duration, 0, len(queryVecs))
		hits, total := 0, 0
		for qi, qv := range queryVecs {
			query := float32sToFloat64s(qv)
			want := wants[qi]
			qStart := time.Now()
			var got []int64
			_, qerr := vectorBenchDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, serr := storeBuilder(rtx)
				if serr != nil {
					return nil, serr
				}
				maintainer, merr := store.GetIndexMaintainer(spfIdx)
				if merr != nil {
					return nil, merr
				}
				high := tuple.Tuple{int64(k)}
				if cfg.kc > 0 {
					high = tuple.Tuple{int64(k), int64(cfg.kc), int64(cfg.w), int64(cfg.c)}
				}
				cursor := maintainer.(sbd).ScanByDistance(recordlayer.TupleRange{
					Low:  tuple.Tuple{recordlayer.SerializeVector(query)},
					High: high,
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
			if qerr != nil {
				t.Fatalf("query: %v", qerr)
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
		t.Logf("READ %s: recall@%d=%.4f p50=%v p99=%v", cfg.name, k,
			float64(hits)/float64(total), latencies[len(latencies)/2], latencies[len(latencies)*99/100])
	}

	// SIFT_SWEEP=w:kc:c[:eps],... sweeps searcher knobs against the
	// foreground-filled index — the production topology, which is where the
	// fixed-probe recall decay shows up (bulk-built indexes mask it).
	if sweep := os.Getenv("SIFT_SWEEP"); sweep != "" {
		runSIFTSweep(t, ctx, storeBuilder, wants, queryVecs, k, sweep, "spf_fill")
	}
}
