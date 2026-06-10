package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// §6 SEAL→SPLIT→FORWARD primitives and the foreground-vs-split interleavings
// (RFC-094 §13 phase 094.2: the transaction primitives and their pinned
// interleavings live here; the autonomous rebalancer is 094.3).
var _ = Describe("SPFresh fine-split primitives", func() {
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

	// setupBuilt: records at the given points, built and readable. Returns the
	// store builder, the index subspace, and the generation-1 storage.
	setupBuilt := func(name string, points map[int64][2]int32) (func(*FDBRecordContext) (*FDBRecordStore, error), subspace.Subspace, *spfreshStorage) {
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
			for id, p := range points {
				if _, serr := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(p[0]), Quantity: proto.Int32(p[1])}); serr != nil {
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
		return storeBuilder, indexSubspace, newSPFreshStorage(indexSubspace, 1)
	}

	// largestPosting finds the (cellID, fineID, memberPKs) of the fullest
	// ACTIVE posting.
	largestPosting := func(storage *spfreshStorage) (cellID, fineID int64, members []tuple.Tuple) {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cellID, fineID, members = 0, 0, nil
			cells, _, lerr := spfreshLoadAllCoarse(tx, storage)
			Expect(lerr).NotTo(HaveOccurred())
			for _, cid := range cells {
				rows, _, _, cerr := spfreshLoadCell(tx, storage, cid)
				Expect(cerr).NotTo(HaveOccurred())
				for _, r := range rows {
					if r.row.state != spfreshStateActive {
						continue
					}
					entries, perr := spfreshLoadPostingForSplit(tx, storage, r.fineID)
					Expect(perr).NotTo(HaveOccurred())
					if len(entries) > len(members) {
						cellID, fineID = cid, r.fineID
						members = nil
						for _, e := range entries {
							members = append(members, e.pk)
						}
					}
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return cellID, fineID, members
	}

	postingPKs := func(storage *spfreshStorage, fineID int64) []string {
		var pks []string
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			pks = pks[:0]
			entries, perr := spfreshLoadPostingForSplit(rtx.Transaction(), storage, fineID)
			Expect(perr).NotTo(HaveOccurred())
			for _, e := range entries {
				pks = append(pks, string(e.pk.Pack()))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return pks
	}

	fileTask := func(storage *spfreshStorage, fineID int64) {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskSplit, fineID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
	}

	clusteredPoints := func(n int) map[int64][2]int32 {
		points := map[int64][2]int32{}
		for i := 0; i < n; i++ {
			id := int64(i + 1)
			points[id] = [2]int32{int32(10 + i%3), int32(10 + i%5)}
		}
		return points
	}

	knn := func(storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), idxName string, q []float64, k int) []int64 {
		var got []int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			idx := store.GetMetaData().GetIndex(idxName)
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
		Expect(err).NotTo(HaveOccurred())
		return got
	}

	It("SEAL→SPLIT moves every member to exactly one child, forwards the parent, and queries survive", func() {
		storeBuilder, _, storage := setupBuilt("spf_split_e2e", clusteredPoints(12))
		cellID, fineID, members := largestPosting(storage)
		Expect(len(members)).To(BeNumerically(">=", 2), "need a posting with members to split")

		fileTask(storage, fineID)
		out, err := spfreshSealFine(ctx, sharedDB, storage, "splitter-1", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())
		Expect(out.childA).NotTo(BeZero())
		Expect(out.childB).To(Equal(out.childA + 1))

		// SEAL is idempotent: a re-run resumes with the SAME children.
		again, err := spfreshSealFine(ctx, sharedDB, storage, "splitter-1", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(out))

		cfg := DefaultSPFreshConfig(2)
		Expect(spfreshSplitFine(ctx, sharedDB, storage, cfg, "splitter-1", cellID, fineID, 7)).To(Succeed())
		// SPLIT is idempotent: parent FORWARD short-circuits the retry.
		Expect(spfreshSplitFine(ctx, sharedDB, storage, cfg, "splitter-1", cellID, fineID, 7)).To(Succeed())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			// Parent: FORWARD row carrying the children; posting = HDR only.
			cent, cerr := spfreshReadCentroidForWrite(tx, storage, cellID, fineID)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(cent.state).To(Equal(spfreshStateForward))
			Expect(cent.childA).To(Equal(out.childA))
			Expect(cent.childB).To(Equal(out.childB))
			entries, perr := spfreshLoadPostingForSplit(tx, storage, fineID)
			Expect(perr).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty(), "parent posting cleared behind the HDR")
			hdr, herr := tx.Get(storage.postingHDRKey(fineID)).Get()
			Expect(herr).NotTo(HaveOccurred())
			hCell, hA, hB, derr := decodePostingHDR(hdr)
			Expect(derr).NotTo(HaveOccurred())
			Expect([3]int64{hCell, hA, hB}).To(Equal([3]int64{cellID, out.childA, out.childB}))
			// Task row cleared.
			row, terr := tx.Get(storage.taskKey(spfreshTaskSplit, fineID)).Get()
			Expect(terr).NotTo(HaveOccurred())
			Expect(row).To(BeNil())
			// Every member is in exactly one child posting, and its membership
			// row names that child instead of the parent.
			childPKs := map[string]int64{}
			for _, child := range []int64{out.childA, out.childB} {
				es, eerr := spfreshLoadPostingForSplit(tx, storage, child)
				Expect(eerr).NotTo(HaveOccurred())
				for _, e := range es {
					k := string(e.pk.Pack())
					Expect(childPKs).NotTo(HaveKey(k), "member in both children")
					childPKs[k] = child
				}
				// Exact counter.
				count, cerr := spfreshCounterReadSnapshot(tx, storage, spfreshCounterFine, child)
				Expect(cerr).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(len(es))))
			}
			Expect(childPKs).To(HaveLen(len(members)))
			for _, pk := range members {
				child, ok := childPKs[string(pk.Pack())]
				Expect(ok).To(BeTrue(), "member lost in split")
				mem, merr := spfreshReadMembership(tx, storage, pk)
				Expect(merr).NotTo(HaveOccurred())
				Expect(mem).To(ContainElement(child))
				Expect(mem).NotTo(ContainElement(fineID))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Queries traverse the FORWARD: every record still findable.
		got := knn(storeBuilder, "spf_split_e2e", []float64{10, 10}, 12)
		Expect(got).To(HaveLen(12), "all records visible through the forwarded posting")
	})

	It("a foreground delete between SEAL and SPLIT is honored — the split's REAL read sees truth", func() {
		storeBuilder, _, storage := setupBuilt("spf_split_del", clusteredPoints(10))
		cellID, fineID, members := largestPosting(storage)
		Expect(len(members)).To(BeNumerically(">=", 2))

		fileTask(storage, fineID)
		out, err := spfreshSealFine(ctx, sharedDB, storage, "splitter-1", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())

		// Foreground delete of a member AFTER seal, BEFORE split (RFC-094 §6:
		// sealing freezes appends; updates/deletes still clear parent keys).
		victim := members[0]
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, derr := store.DeleteRecord(victim)
			return nil, derr
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(spfreshSplitFine(ctx, sharedDB, storage, DefaultSPFreshConfig(2), "splitter-1", cellID, fineID, 7)).To(Succeed())

		// The victim is in NEITHER child; the survivors are all there.
		var childPKs []string
		childPKs = append(childPKs, postingPKs(storage, out.childA)...)
		childPKs = append(childPKs, postingPKs(storage, out.childB)...)
		Expect(childPKs).NotTo(ContainElement(string(victim.Pack())), "deleted member resurrected by the split")
		Expect(childPKs).To(HaveLen(len(members) - 1))
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, merr := spfreshReadMembership(rtx.Transaction(), storage, victim)
			Expect(merr).To(MatchError(errSPFreshNotFound))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("an insert against a SEALED centroid re-routes to the next-nearest ACTIVE one", func() {
		// Two well-separated clusters so the build creates distinct centroids.
		points := map[int64][2]int32{}
		for i := 0; i < 6; i++ {
			points[int64(i+1)] = [2]int32{int32(10 + i%2), int32(10 + i%2)}
			points[int64(i+101)] = [2]int32{int32(200 + i%2), int32(200 + i%2)}
		}
		storeBuilder, _, storage := setupBuilt("spf_split_ins", points)

		// Seal the centroid nearest to (10,10).
		var cellID, fineID int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cache := newSPFreshRoutingCache(0)
			Expect(cache.fullReload(tx, storage, 1)).To(Succeed())
			routed, rerr := cache.route(tx, storage, []float64{10, 10}, 8, 1)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(routed).NotTo(BeEmpty())
			cellID, fineID = routed[0].cellID, routed[0].fineID
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		fileTask(storage, fineID)
		out, err := spfreshSealFine(ctx, sharedDB, storage, "splitter-1", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())

		// Insert right at the sealed centroid's cluster: the REAL state fence
		// must drop it and take the next-nearest ACTIVE centroid.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(999), Price: proto.Int32(10), Quantity: proto.Int32(10)})
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			mem, merr := spfreshReadMembership(rtx.Transaction(), storage, tuple.Tuple{int64(999)})
			Expect(merr).NotTo(HaveOccurred())
			Expect(mem).NotTo(BeEmpty())
			Expect(mem).NotTo(ContainElement(fineID), "insert landed in a SEALED posting")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		// And it is findable.
		Expect(knn(storeBuilder, "spf_split_ins", []float64{10, 10}, 7)).To(ContainElement(int64(999)))
	})

	It("a foreground probe never clobbers a claimed task row", func() {
		_, _, storage := setupBuilt("spf_split_probe", clusteredPoints(8))
		_, fineID, _ := largestPosting(storage)

		// A rebalancer holds the task: trigger filed, claimed with a lease and
		// minted children (the state a probe must never overwrite).
		fileTask(storage, fineID)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			row, terr := spfreshTaskClaim(rtx.Transaction(), storage, spfreshTaskSplit, fineID, "rebalancer-1", spfreshLeaseDeadline(), spfreshNowMs())
			Expect(terr).NotTo(HaveOccurred())
			row.state = spfreshSplitTaskSealed
			row.childA, row.childB = 7777, 7778
			rtx.Transaction().Set(storage.taskKey(spfreshTaskSplit, fineID), encodeTaskRow(row))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// The foreground probe path: Set-if-absent against the live claim.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			wrote, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskSplit, fineID)
			Expect(terr).NotTo(HaveOccurred())
			Expect(wrote).To(BeFalse(), "probe must not overwrite an existing task")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			data, gerr := rtx.Transaction().Get(storage.taskKey(spfreshTaskSplit, fineID)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			row, derr := decodeTaskRow(data)
			Expect(derr).NotTo(HaveOccurred())
			Expect(row.owner).To(Equal("rebalancer-1"))
			Expect(row.childA).To(Equal(int64(7777)), "claimed childIDs survived the probe")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("zombie rules: a re-filed task on a FORWARD parent is deleted as a no-op", func() {
		_, _, storage := setupBuilt("spf_split_zombie", clusteredPoints(8))
		cellID, fineID, _ := largestPosting(storage)
		fileTask(storage, fineID)
		out, err := spfreshSealFine(ctx, sharedDB, storage, "splitter-1", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())
		Expect(spfreshSplitFine(ctx, sharedDB, storage, DefaultSPFreshConfig(2), "splitter-1", cellID, fineID, 7)).To(Succeed())

		// A stale probe re-files the trigger on the now-FORWARD parent.
		fileTask(storage, fineID)
		out, err = spfreshSealFine(ctx, sharedDB, storage, "splitter-2", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeFalse(), "FORWARD parent is a zombie task")
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			data, gerr := rtx.Transaction().Get(storage.taskKey(spfreshTaskSplit, fineID)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(data).To(BeNil(), "zombie task deleted")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Same for a task pointing at a centroid absent from this cell.
		fileTask(storage, 987654)
		out, err = spfreshSealFine(ctx, sharedDB, storage, "splitter-2", cellID, 987654)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeFalse(), "absent centroid is a zombie task")
	})

	It("queries keep returning a SEALED posting's members (codex 094.2 r1)", func() {
		// Until SPLIT commits, the parent posting is the ONLY place its
		// members live. A cache loaded during the seal window must still
		// route reads to it — filtering SEALED out of query routing made
		// every member invisible for the whole window.
		storeBuilder, _, storage := setupBuilt("spf_sealed_vis", clusteredPoints(10))
		cellID, fineID, members := largestPosting(storage)
		Expect(len(members)).To(BeNumerically(">=", 2))
		fileTask(storage, fineID)
		out, err := spfreshSealFine(ctx, sharedDB, storage, "splitter-1", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())

		// The maintainer cache for this subspace loads NOW — post-seal.
		got := knn(storeBuilder, "spf_sealed_vis", []float64{10, 10}, 10)
		Expect(got).To(HaveLen(10), "SEALED posting's members missing from kNN during the seal window")
	})

	It("an insert routed by a stale cache follows a FORWARD parent to its children (codex 094.2 r1)", func() {
		// One tight cluster → one fine centroid. Warm the cache, split the
		// centroid, then insert: the stale cache still routes to the parent;
		// the REAL state fence sees FORWARD and must follow childA/childB
		// instead of failing with "no ACTIVE fine centroid".
		storeBuilder, _, storage := setupBuilt("spf_fwd_insert", clusteredPoints(8))
		// Warm the per-process cache pre-split.
		Expect(knn(storeBuilder, "spf_fwd_insert", []float64{10, 10}, 4)).To(HaveLen(4))

		cellID, fineID, _ := largestPosting(storage)
		fileTask(storage, fineID)
		out, err := spfreshSealFine(ctx, sharedDB, storage, "splitter-1", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())
		Expect(spfreshSplitFine(ctx, sharedDB, storage, DefaultSPFreshConfig(2), "splitter-1", cellID, fineID, 7)).To(Succeed())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(777), Price: proto.Int32(10), Quantity: proto.Int32(10)})
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred(), "insert near a freshly split centroid must follow the forward, not fail")

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			mem, merr := spfreshReadMembership(rtx.Transaction(), storage, tuple.Tuple{int64(777)})
			Expect(merr).NotTo(HaveOccurred())
			Expect(mem).NotTo(BeEmpty())
			Expect(mem).NotTo(ContainElement(fineID), "insert landed in the FORWARD parent")
			for _, id := range mem {
				Expect([]int64{out.childA, out.childB}).To(ContainElement(id), "insert must land in a split child")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("the assignment scan batch is byte-bounded by dimension (codex 094.2 r1)", func() {
		small := DefaultSPFreshConfig(2)
		Expect(small.stagingScanBatch()).To(Equal(spfreshScanBatchSize), "small vectors keep the row cap")
		big := DefaultSPFreshConfig(4096)
		Expect(big.stagingScanBatch()).To(BeNumerically("<", 300), "4096-dim staging batches must shrink to fit the tx budget")
		Expect(big.stagingScanBatch()).To(BeNumerically(">=", 1))
	})

	It("write routing never lets SEALED rows starve the ACTIVE fallbacks (codex 094.2 r2+r4)", func() {
		// 40 SEALED centroids — more than ANY combined cap (2·kc = 32) —
		// nearer than the only ACTIVE one: per-state budgets must still keep
		// the ACTIVE fallback. Two prior shapes of this bug: r2 (SEALED
		// consumed the kc slots), r4 (the combined 2·kc cap filled with
		// SEALED before reaching the first ACTIVE).
		s := newSPFreshStorage(specSubspace().Sub("spfresh-r2").Sub("sealed-budget"), 1)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, s, 1)
			spfreshSaveCoarse(tx, s, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			for i := int64(0); i < 40; i++ {
				spfreshSaveCentroid(tx, s, 1, 10+i, encodeCentroidRow(spfreshStateSealed, 0, 0, 0, []float64{float64(i + 1), 0}))
			}
			spfreshSaveCentroid(tx, s, 1, 99, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{50, 0}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		cache := newSPFreshRoutingCache(0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			Expect(cache.fullReload(tx, s, 1)).To(Succeed())
			write, werr := cache.routeForWrite(tx, s, []float64{0, 0}, 8, spfreshInsertCandidates)
			Expect(werr).NotTo(HaveOccurred())
			// SEALED candidates ride along (the fence may need to follow a
			// SEALED-turned-FORWARD parent — codex r3) but don't consume the
			// budget: the ACTIVE fallback beyond all 17 must be present.
			var fineIDs []int64
			for _, r := range write {
				fineIDs = append(fineIDs, r.fineID)
			}
			Expect(fineIDs).To(ContainElement(int64(99)), "the ACTIVE fallback must survive the cutoff")
			read, rerr := cache.route(tx, s, []float64{0, 0}, 8, spfreshInsertCandidates)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(len(read)).To(Equal(spfreshInsertCandidates), "reads still see SEALED postings")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a parent cached SEALED but FORWARD in storage is still followed by inserts (codex 094.2 r3)", func() {
		// The SEAL→SPLIT staleness window: the cache loaded during SEAL, the
		// split committed after. Write routing must keep the SEALED-cached
		// parent so the fence can real-read FORWARD and follow the children —
		// the r2 fix dropped it and broke the recovery path.
		ks := specSubspace()
		idx := spfIndex("spf_r3_sealedfwd")
		builder := baseMD()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())
			storage := newSPFreshStorage(store.indexSubspace(idx), 1)
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			// The parent: SEALED on disk (what the cache will load).
			spfreshSaveCentroid(tx, storage, 1, 70, encodeCentroidRow(spfreshStateSealed, 0, 0, 0, []float64{1, 0}))
			// A far ACTIVE fallback so routing has somewhere else to go.
			spfreshSaveCentroid(tx, storage, 1, 80, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{200, 0}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		var storage *spfreshStorage
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())
			storage = newSPFreshStorage(store.indexSubspace(idx), 1)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Cache loads while the parent is SEALED — including the L2 cell
		// (route forces ensureCell), or the lazy load would read post-split
		// state and dodge the staleness window under test.
		cache := newSPFreshRoutingCache(0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			if rerr := cache.fullReload(tx, storage, 1); rerr != nil {
				return nil, rerr
			}
			routed, rerr := cache.route(tx, storage, []float64{1, 0}, 8, 16)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(routed).To(HaveLen(2), "cell cached with the SEALED parent")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// The split commits: parent FORWARD, children ACTIVE near it.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSaveCentroid(tx, storage, 1, 70, encodeCentroidRow(spfreshStateForward, 0, 71, 72, []float64{1, 0}))
			spfreshSaveCentroid(tx, storage, 1, 71, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0.5, 0}))
			spfreshSaveCentroid(tx, storage, 1, 72, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{1.5, 0}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Insert near the parent on the STALE cache: route keeps the
		// SEALED-cached parent, the fence reads FORWARD, follows children.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())
			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			m := maintainer.(*spfreshIndexMaintainer)
			tx := rtx.Transaction()
			query := []float64{1, 0}
			routed, rerr := cache.routeForWrite(tx, storage, query, 8, spfreshInsertCandidates)
			Expect(rerr).NotTo(HaveOccurred())
			var ids []int64
			for _, r := range routed {
				ids = append(ids, r.fineID)
			}
			Expect(ids).To(ContainElement(int64(70)), "the SEALED-cached parent must stay routable for writes")
			Expect(m.spfreshInsertRouted(storage, routed, tuple.Tuple{int64(5)}, query)).To(Succeed())
			mem, memErr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(5)})
			Expect(memErr).NotTo(HaveOccurred())
			for _, id := range mem {
				Expect([]int64{71, 72}).To(ContainElement(id), "insert must land in the followed children, not the far fallback")
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a followed FORWARD child keeps the verified list nearest-first for closure (codex 094.2 r2)", func() {
		// Stale-cache order: an ACTIVE candidate pops BEFORE a FORWARD parent
		// whose child is nearer than it. The closure's c1 must be the child —
		// pre-fix the child was appended after the farther candidate, closure
		// took the wrong c1 and kept both copies.
		ks := specSubspace()
		idx := spfIndex("spf_r2_order")
		builder := baseMD()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())
			storage := newSPFreshStorage(store.indexSubspace(idx), 1)
			tx := rtx.Transaction()
			// activeX at d2≈10.2, FORWARD parent at d2≈12.3 with childA at
			// d2≈2.9 (nearer than activeX) and childB far away.
			spfreshSaveCentroid(tx, storage, 1, 50, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{3.2, 0}))
			spfreshSaveCentroid(tx, storage, 1, 60, encodeCentroidRow(spfreshStateForward, 0, 61, 62, []float64{3.5, 0}))
			spfreshSaveCentroid(tx, storage, 1, 61, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{1.7, 0}))
			spfreshSaveCentroid(tx, storage, 1, 62, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{10, 0}))

			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			m := maintainer.(*spfreshIndexMaintainer)
			query := []float64{0, 0}
			routed := []spfreshRouted{
				{cellID: 1, fineID: 50, vec: []float64{3.2, 0}, d2: spfreshSquaredDistance(query, []float64{3.2, 0})},
				{cellID: 1, fineID: 60, vec: []float64{3.5, 0}, d2: spfreshSquaredDistance(query, []float64{3.5, 0})},
			}
			Expect(m.spfreshInsertRouted(storage, routed, tuple.Tuple{int64(1)}, query)).To(Succeed())

			mem, memErr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
			Expect(memErr).NotTo(HaveOccurred())
			Expect(mem).To(Equal([]int64{61}),
				"closure must see the followed child as c1: alpha drops the farther activeX; keeping both means c1 was wrong")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("an insert committing before SEAL is in the frozen posting the split reads", func() {
		// The §6 fencing-sound-both-directions claim, forward direction: insert
		// commits first, then SEAL+SPLIT — the member must come out the other
		// side in a child posting.
		storeBuilder, _, storage := setupBuilt("spf_split_pre", clusteredPoints(8))
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := storeBuilder(rtx)
			Expect(serr).NotTo(HaveOccurred())
			_, serr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(500), Price: proto.Int32(10), Quantity: proto.Int32(10)})
			return nil, serr
		})
		Expect(err).NotTo(HaveOccurred())

		// Find the posting the new record landed in and split THAT.
		var fineID int64
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			mem, merr := spfreshReadMembership(rtx.Transaction(), storage, tuple.Tuple{int64(500)})
			Expect(merr).NotTo(HaveOccurred())
			Expect(mem).NotTo(BeEmpty())
			fineID = mem[0]
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		var cellID int64
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cells, _, lerr := spfreshLoadAllCoarse(tx, storage)
			Expect(lerr).NotTo(HaveOccurred())
			for _, cid := range cells {
				rows, _, _, cerr := spfreshLoadCell(tx, storage, cid)
				Expect(cerr).NotTo(HaveOccurred())
				for _, r := range rows {
					if r.fineID == fineID {
						cellID = cid
					}
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(cellID).NotTo(BeZero())

		fileTask(storage, fineID)
		out, err := spfreshSealFine(ctx, sharedDB, storage, "splitter-1", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())
		Expect(spfreshSplitFine(ctx, sharedDB, storage, DefaultSPFreshConfig(2), "splitter-1", cellID, fineID, 7)).To(Succeed())

		childPKs := append(postingPKs(storage, out.childA), postingPKs(storage, out.childB)...)
		Expect(childPKs).To(ContainElement(string(tuple.Tuple{int64(500)}.Pack())), "pre-seal insert lost by the split")
	})
})
