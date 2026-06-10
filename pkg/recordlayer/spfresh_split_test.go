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
