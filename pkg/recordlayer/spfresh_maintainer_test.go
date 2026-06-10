package recordlayer

import (
	"context"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("SPFresh index maintainer e2e", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// NewSPFreshIndex-style helper: a 2D vector index over (price, quantity).
	newIndex := func(name string) *Index {
		idx := NewIndex(name, Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "32",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		return idx
	}

	It("build-then-read: records -> BuildSPFreshIndex -> ScanByDistance -> live writes (094.2)", func() {
		ks := specSubspace()
		idx := newIndex("spf_price_qty")
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}

		// Phase A: disable the index, load records (the build-then-write
		// contract — a disabled index receives no maintenance).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexDisabled("spf_price_qty")
			Expect(serr).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		points := []struct {
			id       int64
			price    int32
			quantity int32
		}{
			{1, 10, 10},
			{2, 20, 20},
			{3, 100, 100},
			{4, 50, 50},
			{5, 12, 9},
			{6, 95, 105},
			{7, 47, 52},
			{8, 22, 18},
		}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			for _, p := range points {
				_, serr = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(p.id), Price: proto.Int32(p.price), Quantity: proto.Int32(p.quantity),
				})
				Expect(serr).NotTo(HaveOccurred(), "disabled SPFresh index must not block record writes")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase B: bulk-build and mark readable.
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_price_qty", 42)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexReadable("spf_price_qty")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase C: kNN through the maintainer's ScanByDistance (the executor's
		// entry point). Query (15,15), squared distances: id=5 (12,9) d²=45,
		// id=1 (10,10) d²=50, id=2 (20,20) d²=50, id=8 (22,18) d²=58.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			sbd, ok := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			Expect(ok).To(BeTrue(), "SPFresh maintainer must expose ScanByDistance")

			cursor := sbd.ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{15, 15})},
				High: tuple.Tuple{int64(4)},
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
			Expect(got).To(HaveLen(4))
			Expect(got[0]).To(Equal(int64(5)), "exact re-rank: (12,9) at d²=45 is nearest to (15,15)")
			sorted := append([]int64(nil), got...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			Expect(sorted).To(Equal([]int64{1, 2, 5, 8}), "the four nearest points")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase D: the 094.2 foreground write path, end to end through
		// SaveRecord/DeleteRecord against the READABLE index.
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

		// Insert: a brand-new record becomes searchable.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(1), Quantity: proto.Int32(1)})
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred(), "094.2: foreground insert against the readable index")
		Expect(knn([]float64{1, 1}, 1)).To(Equal([]int64{99}), "inserted record is the nearest to its own vector")

		// Update: the SAME pk re-saved with a new vector moves; the old
		// location is cleared (membership-driven, same-tx read).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(200), Quantity: proto.Int32(200)})
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(knn([]float64{200, 200}, 1)).To(Equal([]int64{99}), "updated record found at its new vector")
		Expect(knn([]float64{1, 1}, 1)).NotTo(Equal([]int64{99}), "updated record no longer at its old vector")

		// Delete: the record disappears from kNN results entirely.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, derr := store.DeleteRecord(tuple.Tuple{int64(99)})
			return nil, derr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(knn([]float64{200, 200}, 8)).NotTo(ContainElement(int64(99)), "deleted record gone from kNN")
	})

	It("rejects an invalid SPFresh config at maintainer construction", func() {
		ks := specSubspace()
		idx := newIndex("spf_bad")
		idx.Options[IndexOptionSPFreshAlpha] = "1.0" // the closure bug as a config error
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if serr != nil {
				return nil, serr
			}
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(1), Quantity: proto.Int32(1)})
			return nil, serr
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("alpha"))
	})

	It("ScanByDistance before any build reports a clear error", func() {
		ks := specSubspace()
		idx := newIndex("spf_unbuilt")
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			sbd := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			})
			cursor := sbd.ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{1, 1})},
				High: tuple.Tuple{int64(1)},
			}, nil, ScanProperties{})
			_, cerr := cursor.OnNext(ctx)
			Expect(cerr).To(HaveOccurred())
			Expect(cerr.Error()).To(ContainSubstring("no readable generation"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("SPFresh 094.2 write path", func() {
	ctx := context.Background()

	baseMD := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}
	spfIndex := func(name string) *Index {
		idx := NewIndex(name, Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "32",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		return idx
	}

	// setupBuilt loads the given points, builds, and marks readable; returns
	// the store builder and the index subspace for direct state inspection.
	setupBuilt := func(name string, ids []int64, at func(int64) (int32, int32)) (func(*FDBRecordContext) (*FDBRecordStore, error), subspace.Subspace) {
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
			for _, id := range ids {
				p, q := at(id)
				if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(p), Quantity: proto.Int32(q)}); serr != nil {
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

	countTasks := func(indexSubspace subspace.Subspace, kind int64) int {
		storage := newSPFreshStorage(indexSubspace, 1)
		n := 0
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			n = 0
			r, rerr := fdb.PrefixRange(storage.tasks.Bytes())
			Expect(rerr).NotTo(HaveOccurred())
			kvs, gerr := rtx.Transaction().GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
			Expect(gerr).NotTo(HaveOccurred())
			for _, kv := range kvs {
				t, terr := storage.tasks.Unpack(kv.Key)
				Expect(terr).NotTo(HaveOccurred())
				if t[0].(int64) == kind {
					n++
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return n
	}

	It("overfilling a posting past Lmax writes a split trigger task", func() {
		ids := make([]int64, 8)
		for i := range ids {
			ids[i] = int64(i + 1)
		}
		// All build points tightly clustered: one fine centroid takes them all.
		storeBuilder, indexSubspace := setupBuilt("spf_split_trigger", ids, func(id int64) (int32, int32) {
			return int32(10 + id%2), int32(10 + id%3)
		})
		Expect(countTasks(indexSubspace, spfreshTaskSplit)).To(BeZero(), "no split trigger after a balanced build")

		// Insert far past 2×Lmax = 64 at the same spot: the unconditional
		// branch guarantees the trigger regardless of pk-hash sampling.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			for id := int64(100); id < 180; id++ {
				if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(10), Quantity: proto.Int32(10)}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(countTasks(indexSubspace, spfreshTaskSplit)).To(BeNumerically(">", 0),
			"a posting past the overfill ceiling must have enqueued a split task")
	})

	It("draining a posting below Lmin writes a merge trigger task", func() {
		// Deletion order ends on SAMPLED pks (the probe is 1-in-8 by pk hash,
		// deterministic) so the sub-Lmin probe is guaranteed to run.
		var sampled, unsampled []int64
		for id := int64(1); len(sampled) < 4 || len(unsampled) < 8; id++ {
			if spfreshSampledProbe(tuple.Tuple{id}) {
				if len(sampled) < 4 {
					sampled = append(sampled, id)
				}
			} else if len(unsampled) < 8 {
				unsampled = append(unsampled, id)
			}
		}
		ids := append(append([]int64{}, unsampled...), sampled...)
		storeBuilder, indexSubspace := setupBuilt("spf_merge_trigger", ids, func(id int64) (int32, int32) {
			return int32(10 + id%2), int32(10 + id%3)
		})
		Expect(countTasks(indexSubspace, spfreshTaskMerge)).To(BeZero())

		// Delete unsampled first, sampled last: by the time the posting is
		// below Lmin = Lmax/8 = 4, sampled deletes are still arriving.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			for _, id := range ids {
				if _, derr := store.DeleteRecord(tuple.Tuple{id}); derr != nil {
					return nil, derr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(countTasks(indexSubspace, spfreshTaskMerge)).To(BeNumerically(">", 0),
			"a drained posting must have enqueued a merge task")
	})
})

var _ = Describe("SPFresh §8 staging interleaving", func() {
	ctx := context.Background()

	It("saves during every build window land in (or leave) the index correctly", func() {
		ks := specSubspace()
		idx := NewIndex("spf_wo", Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "32",
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
		save := func(id int64, p, q int32) {
			_, serr := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, berr := storeBuilder(rtx)
				Expect(berr).NotTo(HaveOccurred())
				_, berr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(p), Quantity: proto.Int32(q)})
				return nil, berr
			})
			Expect(serr).NotTo(HaveOccurred())
		}

		var indexSubspace subspace.Subspace
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			indexSubspace = store.indexSubspace(idx)
			_, serr = store.MarkIndexWriteOnly("spf_wo")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		storage := newSPFreshStorage(indexSubspace, 1)

		// WINDOW 1 — pre-coarse: saves are index no-ops; the assignment scan
		// covers them later. Records 1..8 in one tight cluster.
		inputs := make([]spfreshBuildInput, 0, 8)
		for i := int64(1); i <= 8; i++ {
			p, q := int32(10+i%3), int32(10+i%5)
			save(i, p, q)
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{i}, vec: []float64{float64(p), float64(q)}})
		}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, merr := spfreshReadMembership(rtx.Transaction(), storage, tuple.Tuple{int64(1)})
			Expect(merr).To(MatchError(errSPFreshNotFound), "pre-coarse save must not write index state")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// The build starts: coarse table commits.
		config := parseSPFreshConfig(idx)
		bld := newSPFreshBuilder(sharedDB, storage, config, "build-test")
		sample := make([][]float64, len(inputs))
		for i := range inputs {
			sample[i] = inputs[i].vec
		}
		Expect(bld.coarsePass(ctx, sample, 42)).To(Succeed())

		// WINDOW 2 — post-coarse, pre-finalize: the save STAGES itself.
		save(100, 11, 12)
		var stagedCell int64
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			stagedCell = 0
			for _, cellID := range bld.cellIDs {
				pks, _, serr := spfreshLoadStagingCell(tx, storage, cellID)
				Expect(serr).NotTo(HaveOccurred())
				for _, pk := range pks {
					if pk[0].(int64) == 100 {
						stagedCell = cellID
					}
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(stagedCell).NotTo(BeZero(), "window-2 save must stage itself into its routed cell")

		// A delete in the same window removes the staged copy: record 100
		// saved then deleted mid-build must NOT surface in the final index.
		save(101, 12, 11)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, derr := store.DeleteRecord(tuple.Tuple{int64(101)})
			return nil, derr
		})
		Expect(err).NotTo(HaveOccurred())

		// The assignment scan (records 1..8; windows may double-cover — Sets
		// are idempotent), then wave A.
		Expect(bld.stageBatch(ctx, inputs)).To(Succeed())
		fineIDs := make(map[int64][]int64)
		fineVecs := make(map[int64][][]float64)
		for _, cellID := range bld.cellIDs {
			Expect(bld.waveA(ctx, cellID, 42, fineIDs, fineVecs)).To(Succeed())
		}
		router := bld.buildRouter(fineIDs, fineVecs)
		for _, cellID := range bld.cellIDs {
			Expect(bld.waveB(ctx, cellID, router)).To(Succeed())
		}

		// WINDOW 3 — post-FINALIZE, pre-flip: the save goes LIVE within its
		// finalized cell.
		save(200, 13, 10)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			mem, merr := spfreshReadMembership(rtx.Transaction(), storage, tuple.Tuple{int64(200)})
			Expect(merr).NotTo(HaveOccurred(), "window-3 save must take the live path")
			Expect(mem).NotTo(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Flip, complete the range set, mark readable.
		Expect(bld.flip(ctx)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			rangeSet := NewIndexingRangeSet(store.subspace, idx)
			if _, ierr := rangeSet.InsertRange(rtx.Transaction(), nil, rangeSetFinalKey, false); ierr != nil {
				return nil, ierr
			}
			_, serr = store.MarkIndexReadable("spf_wo")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		// The verdict: 1..8 (pre-coarse, scan-covered), 100 (self-staged) and
		// 200 (live) are all findable; 101 (deleted mid-build) is not.
		var got []int64
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			cursor := maintainer.(interface {
				ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
			}).ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{11, 11})},
				High: tuple.Tuple{int64(16)},
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
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(ContainElements(int64(1), int64(2), int64(3), int64(4), int64(5), int64(6), int64(7), int64(8)), "pre-coarse saves covered by the assignment scan")
		Expect(got).To(ContainElement(int64(100)), "window-2 save staged itself and was wave-B assigned")
		Expect(got).To(ContainElement(int64(200)), "window-3 save took the live path")
		Expect(got).NotTo(ContainElement(int64(101)), "mid-build delete must stick")
	})
})

// The three Torvalds 094.2 NAK holes, each pinned (red against the pre-fix
// behavior, green now).
var _ = Describe("SPFresh §8 fence regressions (Torvalds 094.2)", func() {
	ctx := context.Background()

	It("#1: the pre-coarse no-op decision carries a conflict range — a racing coarse commit aborts the save", func() {
		ks := specSubspace()
		storage := newSPFreshStorage(ks.Sub("spfresh-fence").Sub("coarse"), 1)
		config := DefaultSPFreshConfig(2)

		// The write-path shape, compressed: read the coarse table FOR WRITE,
		// decide "pre-coarse: no-op", then commit. Between the read and the
		// commit, the coarse pass lands. The REAL read's conflict range MUST
		// abort the first attempt — a snapshot read commits the stale no-op
		// and the record is lost (the pre-fix bug).
		attempts := 0
		var sawCoarse bool
		err := spfreshRun(ctx, sharedDB, func(rtx *FDBRecordContext) error {
			attempts++
			tx := rtx.Transaction()
			ids, _, lerr := spfreshLoadAllCoarseForWrite(tx, storage)
			if lerr != nil {
				return lerr
			}
			if attempts == 1 {
				Expect(ids).To(BeEmpty(), "first attempt sees the pre-coarse window")
				// The coarse pass commits AFTER our read version, BEFORE our
				// commit (a separate transaction).
				bld := newSPFreshBuilder(sharedDB, storage, config, "racer")
				Expect(bld.coarsePass(ctx, [][]float64{{1, 1}, {2, 2}, {100, 100}}, 7)).To(Succeed())
			}
			sawCoarse = len(ids) > 0
			// Any write makes this a committing transaction.
			tx.Set(storage.metaKey(spfreshMetaHorizon), []byte{1, 0, 0, 0, 0, 0, 0, 0})
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(attempts).To(Equal(2), "the coarse commit must conflict the no-op decision")
		Expect(sawCoarse).To(BeTrue(), "the retry sees the committed coarse table and can route itself")
	})

	It("#2: a delete racing the assignment scan never leaves a staged ghost", func() {
		ks := specSubspace()
		idx := NewIndex("spf_ghost", Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "32",
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
		var indexSubspace subspace.Subspace
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			indexSubspace = store.indexSubspace(idx)
			_, serr = store.MarkIndexWriteOnly("spf_ghost")
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			for i := int64(1); i <= 6; i++ {
				if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(10 + i%3)), Quantity: proto.Int32(int32(10 + i%5))}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		storage := newSPFreshStorage(indexSubspace, 1)
		config := parseSPFreshConfig(idx)
		bld := newSPFreshBuilder(sharedDB, storage, config, "ghost-build")
		Expect(bld.coarsePass(ctx, [][]float64{{10, 10}, {11, 11}, {12, 12}}, 7)).To(Succeed())

		// Assignment scan in batches of 3; mid-FIRST-batch a delete of record
		// 2 commits. The staging writes ride INSIDE the scan tx, whose REAL
		// read of the record range the delete conflicts: the scan retries and
		// re-reads truth. (Pre-fix, staging ran in a SEPARATE tx after the
		// scan: nothing conflicted, the dead pk was staged, and wave B would
		// have indexed a permanent ghost no future delete could clear.)
		oldBatch := spfreshScanBatchSize
		spfreshScanBatchSize = 3
		defer func() { spfreshScanBatchSize = oldBatch }()
		deleted := false
		err = spfreshScanRecordBatches(ctx, sharedDB, storeBuilder, idx, indexSubspace, spfreshScanBatchSize,
			func(rtx *FDBRecordContext, batch []spfreshBuildInput) error {
				if !deleted {
					deleted = true
					_, derr := sharedDB.Run(ctx, func(inner *FDBRecordContext) (any, error) {
						store, serr := storeBuilder(inner)
						if serr != nil {
							return nil, serr
						}
						_, derr := store.DeleteRecord(tuple.Tuple{int64(2)})
						return nil, derr
					})
					Expect(derr).NotTo(HaveOccurred())
				}
				return bld.stageInTx(rtx, batch)
			}, nil)
		Expect(err).NotTo(HaveOccurred())

		// No staged ghost: pk 2 is in NO cell's staging; the survivors are.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			staged := map[int64]bool{}
			for _, cellID := range bld.cellIDs {
				pks, _, serr := spfreshLoadStagingCell(tx, storage, cellID)
				Expect(serr).NotTo(HaveOccurred())
				for _, pk := range pks {
					staged[pk[0].(int64)] = true
				}
			}
			Expect(staged).NotTo(HaveKey(int64(2)), "deleted record staged: a permanent ghost")
			for _, want := range []int64{1, 3, 4, 5, 6} {
				Expect(staged).To(HaveKey(want), "survivor lost by the scan retry")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("#3: the builder routes staging on the SAME fp16 bytes foreground writers decode", func() {
		ks := specSubspace()
		storage := newSPFreshStorage(ks.Sub("spfresh-fence").Sub("fp16"), 1)
		config := DefaultSPFreshConfig(2)
		bld := newSPFreshBuilder(sharedDB, storage, config, "builder-1")
		// Vectors chosen so raw k-means output is NOT fp16-representable
		// (1.0005 rounds to 1.0 in half precision): the centroid the builder
		// routes on must equal the stored row's decode, or a boundary vector
		// double-stages into different cells on the two paths.
		sample := [][]float64{{1.0005, 0}, {1.0005, 0}, {500, 500}, {500, 500}}
		Expect(bld.coarsePass(ctx, sample, 7)).To(Succeed())

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			ids, rows, lerr := spfreshLoadAllCoarse(rtx.Transaction(), storage)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(ids).To(Equal(bld.cellIDs))
			for i := range rows {
				stored, verr := rows[i].vector()
				Expect(verr).NotTo(HaveOccurred())
				Expect(bld.coarseVec[i]).To(Equal(stored),
					"builder routing input %d diverges from the stored fp16 row — split-brain routing", i)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("SPFresh 094.1 review regressions", func() {
	ctx := context.Background()

	baseMD := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}
	spfIndex := func(name string) *Index {
		idx := NewIndex(name, Concat(Field("price"), Field("quantity")))
		idx.Type = IndexTypeVectorSPFresh
		idx.Options = map[string]string{
			IndexOptionSPFreshNumDimensions: "2",
			IndexOptionSPFreshLmax:          "32",
			IndexOptionSPFreshCellTarget:    "4",
			IndexOptionSPFreshCellMax:       "8",
		}
		return idx
	}

	It("build's record scan crosses continuation batches without duplicating records", func() {
		// The unbatched scan blew the 5s tx limit at scale and retried forever
		// (SIFT-100k hang); the env-gated benchmark can't guard it in CI.
		// Force the continuation path: 10 records, batch size 3.
		old := spfreshScanBatchSize
		spfreshScanBatchSize = 3
		defer func() { spfreshScanBatchSize = old }()

		ks := specSubspace()
		idx := spfIndex("spf_scanbatch")
		b := baseMD()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexDisabled("spf_scanbatch")
			Expect(serr).NotTo(HaveOccurred())
			for i := int64(1); i <= 10; i++ {
				_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i)), Quantity: proto.Int32(int32(i))})
				Expect(serr).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_scanbatch", 7)).To(Succeed())

		// Every record indexed: membership exists per pk. (Duplicates from a
		// re-scanned batch are structurally idempotent — staging keys are
		// (cellID, pk) Sets — so presence is the meaningful assertion here;
		// the per-attempt staging in BuildSPFreshIndex is what prevents the
		// retry-duplication class.)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			gen1 := newSPFreshStorage(store.indexSubspace(idx), 1)
			for i := int64(1); i <= 10; i++ {
				ids, merr := spfreshReadMembership(tx, gen1, tuple.Tuple{i})
				Expect(merr).NotTo(HaveOccurred())
				Expect(ids).NotTo(BeEmpty(), "record %d must be indexed", i)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a warm routing cache never reads the changelog on the query path", func() {
		// The per-query changelog refresh was the rev-2-NAK'd hot-key
		// anti-pattern (~15% of measured p50). Poison the changelog with a
		// generation delta AFTER warming: a query must keep serving (it never
		// reads the changelog); an explicit refresh must see the poison.
		ks := specSubspace().Sub("spfresh-warm")
		s := newSPFreshStorage(ks, 1)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, s, 1)
			spfreshSaveCoarse(tx, s, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			spfreshSaveCentroid(tx, s, 1, 10, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		cache := newSPFreshRoutingCache(0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, cache.fullReload(rtx.Transaction(), s, 1)
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(cache.ready(1)).To(BeTrue())
		Expect(cache.ready(2)).To(BeFalse(), "other generation must not be ready")

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, spfreshAppendDeltas(rtx.Transaction(), s, []spfreshDelta{
				{op: spfreshOpGeneration, ids: []int64{99}},
			})
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			// Query path: ready() short-circuits — no changelog read, so the
			// poison is invisible and routing still works.
			Expect(cache.ready(1)).To(BeTrue())
			routed, rerr := cache.route(tx, s, []float64{0, 0}, 1, 10)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(routed).To(HaveLen(1))
			// The refresh path DOES read the changelog and sees the poison —
			// proving the two paths are genuinely distinct.
			Expect(cache.refresh(tx, s, 1)).To(MatchError(errSPFreshNotFound))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rebuilding targets a fresh generation and clears the superseded one", func() {
		ks := specSubspace()
		idx := spfIndex("spf_rebuild")
		b := baseMD()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		storeBuilder := func(rtx *FDBRecordContext) (*FDBRecordStore, error) {
			return NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.MarkIndexDisabled("spf_rebuild")
			Expect(serr).NotTo(HaveOccurred())
			for i := int64(1); i <= 5; i++ {
				_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10)), Quantity: proto.Int32(0)})
				Expect(serr).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_rebuild", 7)).To(Succeed())
		Expect(BuildSPFreshIndex(ctx, sharedDB, storeBuilder, "spf_rebuild", 8)).To(Succeed(), "rebuild must work")

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			sub := store.indexSubspace(idx)
			g, gerr := spfreshReadGenerationSnapshot(tx, newSPFreshStorage(sub, 0))
			Expect(gerr).NotTo(HaveOccurred())
			Expect(g).To(Equal(int64(2)), "rebuild flips to generation 2")
			// Generation 1 fully cleared.
			r, rerr := newSPFreshStorage(sub, 1).generationRange()
			Expect(rerr).NotTo(HaveOccurred())
			kvs, kerr := tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
			Expect(kerr).NotTo(HaveOccurred())
			Expect(kvs).To(BeEmpty(), "superseded generation must be range-cleared")
			// Generation 2 serves.
			ids, merr := spfreshReadMembership(tx, newSPFreshStorage(sub, 2), tuple.Tuple{int64(1)})
			Expect(merr).NotTo(HaveOccurred())
			Expect(ids).NotTo(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
