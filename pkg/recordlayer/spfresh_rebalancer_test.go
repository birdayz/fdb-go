package recordlayer

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("SPFresh rebalancer + coarse splits", func() {
	ctx := context.Background()

	spfIndex := func(name string) *Index {
		idx := NewIndex(name, Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "16",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		return idx
	}
	baseMD := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// setup: seeds, build, readable. Returns storeBuilder + index subspace.
	setup := func(name string, seedN int) (func(*FDBRecordContext) (*FDBRecordStore, error), subspace.Subspace) {
		ks := specSubspace()
		idx := spfIndex(name)
		builder := baseMD()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		var indexSubspace subspace.Subspace
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			indexSubspace = store.indexSubspace(idx)
			_, serr = store.MarkIndexDisabled(name)
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			for i := 0; i < seedN; i++ {
				if _, serr := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(int32(i % 3)), Quantity: proto.Int32(int32(i % 2)),
				}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, name, 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexReadable(name)
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		return storeBuilder, indexSubspace
	}

	It("coarse split defers on SEALED rows and the guard pauses fine-split issuance", func() {
		_, indexSubspace := setup("spf_csplit_defer", 8)
		storage := newSPFreshStorage(indexSubspace, 1)
		config := parseSPFreshConfig(spfIndex("spf_csplit_defer"))

		// Seal one fine centroid (a fine split mid-window), then file a
		// coarse split for its cell.
		var cellID, fineID int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
			Expect(merr).NotTo(HaveOccurred())
			fineID = mem[0]
			var ferr error
			cellID, ferr = spfreshFindCentroidCell(tx, storage, fineID)
			Expect(ferr).NotTo(HaveOccurred())
			_, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskSplit, fineID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		out, err := spfreshSealFine(ctx, sharedDB, storage, "csplit-test", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskCSplit, cellID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())

		// Deferrals accumulate; at the limit the guard pauses issuance.
		for i := 0; i < spfreshCSplitDeferLimit; i++ {
			Expect(spfreshCoarseSplit(ctx, sharedDB, storage, config, "csplit-test", cellID, 7)).To(Succeed())
		}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			paused, perr := spfreshCSplitPaused(tx, storage, cellID)
			Expect(perr).NotTo(HaveOccurred())
			Expect(paused).To(BeTrue(), "defer limit must pause fine-split issuance")
			// The cell is untouched (still ACTIVE, rows in place).
			coarse, cerr := spfreshReadCoarseForWrite(tx, storage, cellID)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(coarse.state).To(Equal(spfreshStateActive))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Complete the fine split; the coarse split can now proceed.
		Expect(spfreshSplitFine(ctx, sharedDB, storage, config, "csplit-test", cellID, fineID, 7)).To(Succeed())
		Expect(spfreshCoarseSplit(ctx, sharedDB, storage, config, "csplit-test", cellID, 7)).To(Succeed())
		// Idempotent re-run on the FORWARD cell.
		Expect(spfreshCoarseSplit(ctx, sharedDB, storage, config, "csplit-test", cellID, 7)).To(Succeed())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			coarse, cerr := spfreshReadCoarseForWrite(tx, storage, cellID)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(coarse.state).To(Equal(spfreshStateForward), "old cell forwards to the new ones")
			Expect(coarse.childA).NotTo(BeZero())
			// The fine rows moved: every child cell row preserved fineID and
			// state; the old cell's range holds only the HDR.
			rowsOld, _, _, lerr := spfreshLoadCell(tx, storage, cellID)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(rowsOld).To(BeEmpty(), "old cell's centroid rows cleared behind the HDR")
			hdr, herr := tx.Get(storage.centroidHDRKey(cellID)).Get()
			Expect(herr).NotTo(HaveOccurred())
			Expect(hdr).NotTo(BeNil())
			moved := 0
			for _, child := range []int64{coarse.childA, coarse.childB} {
				rows, _, _, cerr := spfreshLoadCell(tx, storage, child)
				Expect(cerr).NotTo(HaveOccurred())
				moved += len(rows)
				active := 0
				for _, r := range rows {
					if r.row.state == spfreshStateActive {
						active++
					}
				}
				count, cterr := spfreshCounterReadSnapshot(tx, storage, spfreshCounterCell, child)
				Expect(cterr).NotTo(HaveOccurred())
				// Tombstones ride the partition for GC discovery but the cell
				// counter counts ACTIVE centroids only.
				Expect(count).To(Equal(int64(active)), "exact cell counters by partition")
			}
			Expect(moved).To(BeNumerically(">=", 2), "fine rows rewritten under the new cells")
			// The csplit task (and with it the pause) is gone.
			paused, perr := spfreshCSplitPaused(tx, storage, cellID)
			Expect(perr).NotTo(HaveOccurred())
			Expect(paused).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("cold-start growth: inserts + rebalancing grow fine AND coarse topology with full recall", func() {
		storeBuilder, indexSubspace := setup("spf_coldstart", 8)
		storage := newSPFreshStorage(indexSubspace, 1)

		// 300 inserts across a widening grid, rebalancing every 50: postings
		// overfill -> fine splits -> cell fills -> coarse split(s).
		const n = 300
		for lo := 0; lo < n; lo += 50 {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, serr := storeBuilder(rtx)
				if serr != nil {
					return nil, serr
				}
				for i := lo; i < lo+50; i++ {
					id := int64(1000 + i)
					if _, serr := store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(id),
						Price:    proto.Int32(int32((i * 13) % 200)),
						Quantity: proto.Int32(int32((i * 7) % 200)),
					}); serr != nil {
						return nil, serr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = RebalanceSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_coldstart")
			Expect(err).NotTo(HaveOccurred())
		}
		actions, err := RebalanceSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_coldstart")
		Expect(err).NotTo(HaveOccurred())
		_ = actions

		// Topology grew at BOTH levels.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			ids, rows, lerr := spfreshLoadAllCoarse(tx, storage)
			Expect(lerr).NotTo(HaveOccurred())
			activeCells, fines := 0, 0
			for i := range ids {
				if rows[i].state != spfreshStateActive {
					continue
				}
				activeCells++
				cellRows, _, _, cerr := spfreshLoadCell(tx, storage, ids[i])
				Expect(cerr).NotTo(HaveOccurred())
				fines += len(cellRows)
			}
			Expect(activeCells).To(BeNumerically(">", 1), "coarse splits must have grown the cell count (§6b cold-start)")
			Expect(fines).To(BeNumerically(">", 8), "fine splits must have grown the centroid count")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Structural invariants after all the churn: every membership entry
		// has a posting row in an ACTIVE or SEALED posting.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			for i := 0; i < n; i++ {
				pk := tuple.Tuple{int64(1000 + i)}
				mem, merr := spfreshReadMembership(tx, storage, pk)
				Expect(merr).NotTo(HaveOccurred())
				Expect(mem).NotTo(BeEmpty())
				for _, fineID := range mem {
					data, gerr := tx.Get(storage.postingKey(fineID, pk)).Get()
					Expect(gerr).NotTo(HaveOccurred())
					Expect(data).NotTo(BeNil(), "membership names a missing posting entry after growth")
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		// Full recall: every inserted record is its own nearest neighbor.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			idx := store.GetMetaData().GetIndex("spf_coldstart")
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			sbd := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			for i := 0; i < n; i += 17 { // sample
				q := []float64{float64((i * 13) % 200), float64((i * 7) % 200)}
				cursor := sbd.ScanByDistance(TupleRange{
					Low:  tuple.Tuple{SerializeVector(q)},
					High: tuple.Tuple{int64(3)},
				}, nil, ScanProperties{})
				var got []int64
				for {
					res, cerr := cursor.OnNext(ctx)
					Expect(cerr).NotTo(HaveOccurred())
					if !res.HasNext() {
						break
					}
					got = append(got, res.GetValue().Key[0].(int64))
				}
				Expect(got).To(ContainElement(int64(1000+i)), fmt.Sprintf("record %d lost during growth", 1000+i))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// The SQL-shape lifecycle (RFC-094 §6b cold start): CREATE INDEX on an empty
// store, INSERT records — no bulk build anywhere. The first insert bootstraps
// generation 1 + the first centroid; growth is splits all the way up.
var _ = Describe("SPFresh cold-start bootstrap (no bulk build)", func() {
	ctx := context.Background()

	It("a readable empty index accepts inserts from zero and serves kNN", func() {
		ks := specSubspace()
		idx := NewIndex("spf_bootstrap", Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "16",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// Query against the EMPTY readable index: zero rows, no error.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			// First insert: bootstraps generation + first centroid in-tx.
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(10)})
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred(), "first insert must bootstrap the §6b cold-start shape")

		// 150 more inserts + periodic rebalancing grow the topology.
		for lo := 0; lo < 150; lo += 50 {
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, serr := storeBuilder(rtx)
				if serr != nil {
					return nil, serr
				}
				for i := lo; i < lo+50; i++ {
					id := int64(100 + i)
					if _, serr := store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(id),
						Price:    proto.Int32(int32((i * 13) % 150)),
						Quantity: proto.Int32(int32((i * 7) % 150)),
					}); serr != nil {
						return nil, serr
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = RebalanceSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_bootstrap")
			Expect(err).NotTo(HaveOccurred())
		}

		// Recall sampling: records findable at their own vectors.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			sbd := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			for i := 0; i < 150; i += 13 {
				q := []float64{float64((i * 13) % 150), float64((i * 7) % 150)}
				cursor := sbd.ScanByDistance(TupleRange{
					Low:  tuple.Tuple{SerializeVector(q)},
					High: tuple.Tuple{int64(3)},
				}, nil, ScanProperties{})
				var got []int64
				for {
					res, cerr := cursor.OnNext(ctx)
					Expect(cerr).NotTo(HaveOccurred())
					if !res.HasNext() {
						break
					}
					got = append(got, res.GetValue().Key[0].(int64))
				}
				Expect(got).To(ContainElement(int64(100+i)), fmt.Sprintf("record %d lost in cold-start growth", 100+i))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// Torvalds 094.4 NAK regressions: the bootstrap's fencing story.
var _ = Describe("SPFresh bootstrap fencing (Torvalds 094.4)", func() {
	ctx := context.Background()

	mdAndBuilder := func(name string) (*RecordMetaData, func(*FDBRecordContext) (*FDBRecordStore, error), *Index) {
		ks := specSubspace()
		idx := NewIndex(name, Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "16",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		return md, storeBuilder, idx
	}

	It("#1: an insert refuses to bootstrap while a bulk build holds the token", func() {
		_, storeBuilder, idx := mdAndBuilder("spf_boot_vs_build")
		// Simulate the build's entry state: token taken, no generation yet
		// (exactly what BuildSPFreshIndex's pre-build clear tx leaves).
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			metaStorage := newSPFreshStorage(store.indexSubspace(idx), 0)
			spfreshTakeBuilderToken(rtx.Transaction(), metaStorage, []byte("in-flight-build"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// The first insert must refuse loudly — pre-fix it bootstrapped
		// generation 1 and the racing build's flip would have self-ACKed the
		// bootstrap's generation as its own.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(1), Quantity: proto.Int32(1)})
			return nil, serr
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("bulk build is in flight"))
	})

	It("#2: SELECT against a never-touched index returns zero rows, not an error", func() {
		_, storeBuilder, idx := mdAndBuilder("spf_empty_select")
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			cursor := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			}).ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{1, 1})},
				High: tuple.Tuple{int64(5)},
			}, nil, ScanProperties{})
			res, cerr := cursor.OnNext(ctx)
			Expect(cerr).NotTo(HaveOccurred(), "empty-index kNN must not error (§6b insert-first flow)")
			Expect(res.HasNext()).To(BeFalse(), "empty index answers with zero rows")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// codex 094.4 P2: a query against a bootstrapped-but-centroidless index
// caches the bootstrap cell with zero candidates; the first centroid mint
// must evict that cached cell or same-process queries miss the record until
// the throttled refresh fires. The window is reachable from the public
// surface: a SaveRecord whose vector evaluates NULL (unset fields) bootstraps
// the generation WITHOUT minting a centroid (Update resolves-for-write before
// skipping the nil vector).
var _ = Describe("SPFresh bootstrap cache eviction (codex 094.4)", func() {
	ctx := context.Background()

	It("a query cached against the centroidless index sees the first real insert immediately", func() {
		ks := specSubspace()
		idx := NewIndex("spf_boot_cache", Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "16",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		knn := func(q []float64, k int) []int64 {
			var got []int64
			_, kerr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, serr := storeBuilder(rtx)
				Expect(serr).NotTo(HaveOccurred())
				maintainer, merr := store.getIndexMaintainer(idx)
				Expect(merr).NotTo(HaveOccurred())
				cursor := maintainer.(interface {
					ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
				}).ScanByDistance(TupleRange{
					Low:  tuple.Tuple{SerializeVector(q)},
					High: tuple.Tuple{int64(k)},
				}, nil, ScanProperties{})
				got = got[:0]
				for {
					res, cerr := cursor.OnNext(ctx)
					Expect(cerr).NotTo(HaveOccurred())
					if !res.HasNext() {
						break
					}
					got = append(got, res.GetValue().Key[0].(int64))
				}
				return nil, nil
			})
			Expect(kerr).NotTo(HaveOccurred())
			return got
		}

		// NULL-vector save: bootstraps generation 1 + the empty cell, mints
		// NO centroid (the vector evaluates nil and the insert is skipped).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1)})
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		// Query the centroidless index: zero rows — and the L2 cache now
		// holds the EMPTY bootstrap cell.
		Expect(knn([]float64{10, 10}, 3)).To(BeEmpty())

		// Pin the throttle window deterministically: the amortized refresh
		// just fired, so the next query inside the interval will NOT consult
		// the changelog.
		var indexSubspace subspace.Subspace
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			indexSubspace = store.indexSubspace(idx)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		spfreshCacheFor(indexSubspace, 1).lastRefreshMs.Store(spfreshNowMs())

		// First REAL insert: mints the centroid — and must evict the cached
		// empty cell (pre-fix it did not, and this query returned zero rows
		// until the refresh interval expired).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(11), Quantity: proto.Int32(11)})
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(knn([]float64{11, 11}, 3)).To(ContainElement(int64(2)),
			"first real insert must be visible to a process that cached the centroidless index")
	})
})

// codex 094.4 r2 regressions.
var _ = Describe("SPFresh cosine exact-match + build-vs-scan (codex 094.4 r2)", func() {
	ctx := context.Background()

	It("a cosine query exactly at a centroid returns the match, not an estimator error", func() {
		ks := specSubspace()
		idx := NewIndex("spf_cosine_exact", Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshMetric:        "COSINE_METRIC",
			IndexOptionSPFreshLmax:          "16",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// Cold-start insert: the FIRST centroid is minted AT this vector, so
		// querying the same vector makes the residual exactly zero — the case
		// the RaBitQ estimator rejects for cosine.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(3), Quantity: proto.Int32(4)})
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			cursor := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			}).ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{3, 4})},
				High: tuple.Tuple{int64(1)},
			}, nil, ScanProperties{})
			res, cerr := cursor.OnNext(ctx)
			Expect(cerr).NotTo(HaveOccurred(), "cosine exact-match query must not fail on the zero-residual estimate")
			Expect(res.HasNext()).To(BeTrue())
			Expect(res.GetValue().Key[0]).To(Equal(int64(1)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a scan during an in-flight build errors instead of reporting the index empty", func() {
		ks := specSubspace()
		idx := NewIndex("spf_scan_vs_build", Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "16",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// The build's entry state: token taken, no generation.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			metaStorage := newSPFreshStorage(store.indexSubspace(idx), 0)
			spfreshTakeBuilderToken(rtx.Transaction(), metaStorage, []byte("in-flight-build"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			cursor := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			}).ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{1, 1})},
				High: tuple.Tuple{int64(1)},
			}, nil, ScanProperties{})
			_, cerr := cursor.OnNext(ctx)
			Expect(cerr).To(HaveOccurred(), "a building index must not silently report zero rows")
			Expect(cerr.Error()).To(ContainSubstring("bulk build is in flight"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// codex 094.4 r3: a zero-residual cosine query must RANK its posting (the
// constant-tie workaround could evict the true match from the top-C cut by
// pk tie-break before the exact re-rank).
var _ = Describe("SPFresh cosine zero-residual ranking (codex 094.4 r3)", func() {
	ctx := context.Background()

	It("ranks the exact match first within the zero-residual posting", func() {
		ks := specSubspace()
		idx := NewIndex("spf_cos_rank", Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshMetric:        "COSINE_METRIC",
			IndexOptionSPFreshLmax:          "16",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// Insert id 5 FIRST: the cold-start centroid is minted AT its vector,
		// so querying it later is the zero-residual case. Then id 1 with a
		// different ANGLE — under a constant-estimate tie, the pk tie-break
		// would pick id 1 (the wrong answer) when c=1 truncates before
		// re-rank.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(4), Quantity: proto.Int32(3)}); serr != nil {
				return nil, serr
			}
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(3), Quantity: proto.Int32(4)})
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			// k=1 with re-rank pool c=1: the top-C cut happens BEFORE the
			// exact re-rank, so the estimate ordering is load-bearing.
			cursor := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			}).ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{4, 3})},
				High: tuple.Tuple{int64(1), int64(0), int64(0), int64(1)},
			}, nil, ScanProperties{})
			res, cerr := cursor.OnNext(ctx)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(res.HasNext()).To(BeTrue())
			Expect(res.GetValue().Key[0]).To(Equal(int64(5)),
				"the exact match must out-rank the different-angle vector in the estimate ordering, not lose a constant tie by pk")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
