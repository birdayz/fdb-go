package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

var _ = Describe("SPFresh storage primitives", func() {
	ctx := context.Background()

	newStorage := func(name string) *spfreshStorage {
		return newSPFreshStorage(specSubspace().Sub("spfresh-storage").Sub(name), 1)
	}

	It("generation read/write round-trips and reports absence", func() {
		s := newStorage("gen")
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			_, gerr := spfreshReadGenerationForWrite(tx, s)
			Expect(gerr).To(MatchError(errSPFreshNotFound))
			spfreshSetGeneration(tx, s, 3)
			gen, gerr := spfreshReadGenerationForWrite(tx, s)
			Expect(gerr).NotTo(HaveOccurred())
			Expect(gen).To(Equal(int64(3)))
			gen, gerr = spfreshReadGenerationSnapshot(tx, s)
			Expect(gerr).NotTo(HaveOccurred())
			Expect(gen).To(Equal(int64(3)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ID block claims are disjoint across transactions and start at 1", func() {
		s := newStorage("idblock")
		var first, second int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			var cerr error
			first, cerr = spfreshClaimIDBlock(rtx.Transaction(), s)
			return nil, cerr
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			var cerr error
			second, cerr = spfreshClaimIDBlock(rtx.Transaction(), s)
			return nil, cerr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(first).To(Equal(int64(1)), "IDs start at 1; 0 is reserved as none")
		Expect(second).To(Equal(first+spfreshIDBlockSize), "blocks must be disjoint")
	})

	It("centroid rows round-trip; cell loads return rows and the coarse HDR", func() {
		s := newStorage("centroids")
		vec := []float64{1, 2, 3}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSaveCentroid(tx, s, 5, 10, encodeCentroidRow(spfreshStateActive, 1, 0, 0, vec))
			spfreshSaveCentroid(tx, s, 5, 11, encodeCentroidRow(spfreshStateSealed, 2, 0, 0, vec))
			tx.Set(s.centroidHDRKey(5), encodeCellHDR(7, 8))

			row, rerr := spfreshReadCentroidForWrite(tx, s, 5, 10)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(row.state).To(Equal(spfreshStateActive))
			_, rerr = spfreshReadCentroidForWrite(tx, s, 5, 99)
			Expect(rerr).To(MatchError(errSPFreshNotFound))
			// A row moved to another cell is absent at the old key.
			_, rerr = spfreshReadCentroidForWrite(tx, s, 6, 10)
			Expect(rerr).To(MatchError(errSPFreshNotFound))

			rows, fwdA, fwdB, lerr := spfreshLoadCell(tx, s, 5)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(2))
			Expect(rows[0].fineID).To(Equal(int64(10)))
			Expect(rows[1].fineID).To(Equal(int64(11)))
			Expect(rows[1].row.state).To(Equal(spfreshStateSealed))
			Expect(fwdA).To(Equal(int64(7)))
			Expect(fwdB).To(Equal(int64(8)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("coarse table loads all cells", func() {
		s := newStorage("coarse")
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSaveCoarse(tx, s, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			spfreshSaveCoarse(tx, s, 2, encodeCentroidRow(spfreshStateForward, 0, 3, 4, []float64{1, 1}))
			ids, rows, lerr := spfreshLoadAllCoarse(tx, s)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(ids).To(Equal([]int64{1, 2}))
			Expect(rows[1].state).To(Equal(spfreshStateForward))
			Expect(rows[1].childA).To(Equal(int64(3)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("posting loads return the HDR (sorted first) and entries; split read skips HDR", func() {
		s := newStorage("postings")
		pkA, pkB := tuple.Tuple{int64(1)}, tuple.Tuple{"user", int64(2)}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			tx.Set(s.postingKey(42, pkA), []byte{0xaa})
			tx.Set(s.postingKey(42, pkB), []byte{0xbb})
			tx.Set(s.postingHDRKey(42), encodePostingHDR(5, 100, 101))

			entries, fwdCell, fwdA, fwdB, lerr := spfreshLoadPostingSnapshot(tx, s, 42, 100)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(fwdCell).To(Equal(int64(5)))
			Expect(fwdA).To(Equal(int64(100)))
			Expect(fwdB).To(Equal(int64(101)))

			// The fetch cap (Limit) must still see the HDR: it sorts FIRST, so a
			// capped read of an oversized forwarded posting can never miss it.
			capped, fc, _, _, lerr := spfreshLoadPostingSnapshot(tx, s, 42, 1)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(fc).To(Equal(int64(5)), "HDR must be within any non-zero fetch cap")
			Expect(capped).To(BeEmpty())

			split, serr := spfreshLoadPostingForSplit(tx, s, 42)
			Expect(serr).NotTo(HaveOccurred())
			Expect(split).To(HaveLen(2), "split read returns entries only")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("membership round-trips and reports absence", func() {
		s := newStorage("membership")
		pk := tuple.Tuple{int64(77)}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			_, merr := spfreshReadMembership(tx, s, pk)
			Expect(merr).To(MatchError(errSPFreshNotFound))
			tx.Set(s.membershipKey(pk), encodeMembership([]int64{10, 20}))
			ids, merr := spfreshReadMembership(tx, s, pk)
			Expect(merr).NotTo(HaveOccurred())
			Expect(ids).To(Equal([]int64{10, 20}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("counters: ADD accumulates, exact Set reconciles, kinds never alias", func() {
		s := newStorage("counters")
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshCounterAdd(tx, s, spfreshCounterFine, 9, 2)
			spfreshCounterAdd(tx, s, spfreshCounterFine, 9, 3)
			spfreshCounterAdd(tx, s, spfreshCounterCell, 9, 7) // same id, other kind
			fine, cerr := spfreshCounterReadSnapshot(tx, s, spfreshCounterFine, 9)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(fine).To(Equal(int64(5)))
			cell, cerr := spfreshCounterReadSnapshot(tx, s, spfreshCounterCell, 9)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(cell).To(Equal(int64(7)), "kind tags must prevent aliasing (codex r4 #6)")
			spfreshCounterSet(tx, s, spfreshCounterFine, 9, 100)
			fine, cerr = spfreshCounterReadSnapshot(tx, s, spfreshCounterFine, 9)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(fine).To(Equal(int64(100)))
			// Negative deltas (deletes) work through two's complement.
			spfreshCounterAdd(tx, s, spfreshCounterFine, 9, -1)
			fine, cerr = spfreshCounterReadSnapshot(tx, s, spfreshCounterFine, 9)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(fine).To(Equal(int64(99)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("task set-if-absent, claim, foreign-lease rejection, expiry reclaim", func() {
		s := newStorage("tasks")
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			wrote, terr := spfreshTaskSetIfAbsent(tx, s, spfreshTaskSplit, 5)
			Expect(terr).NotTo(HaveOccurred())
			Expect(wrote).To(BeTrue())
			// Second probe sees the row and does not touch it.
			wrote, terr = spfreshTaskSetIfAbsent(tx, s, spfreshTaskSplit, 5)
			Expect(terr).NotTo(HaveOccurred())
			Expect(wrote).To(BeFalse())

			// Claim it; record child IDs (the SEAL step's resume state).
			row, cerr := spfreshTaskClaim(tx, s, spfreshTaskSplit, 5, "owner-A", 9_999_999_999_999, 1_000)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(row.owner).To(Equal("owner-A"))
			row.childA, row.childB = 100, 101
			tx.Set(s.taskKey(spfreshTaskSplit, 5), encodeTaskRow(row))

			// A different owner cannot steal a live lease...
			_, cerr = spfreshTaskClaim(tx, s, spfreshTaskSplit, 5, "owner-B", 9_999_999_999_999, 2_000)
			Expect(cerr).To(MatchError(errSPFreshNotFound))
			// ...but the SAME owner re-claims (commit_unknown retry), childIDs intact...
			row, cerr = spfreshTaskClaim(tx, s, spfreshTaskSplit, 5, "owner-A", 9_999_999_999_999, 2_000)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(row.childA).To(Equal(int64(100)), "resume state must survive re-claim")
			// ...and an EXPIRED lease is reclaimable by anyone, childIDs intact
			// (the inline-split wedge recovery — RFC-094 §5/§6).
			expired := row
			expired.leaseDeadlineMs = 500
			tx.Set(s.taskKey(spfreshTaskSplit, 5), encodeTaskRow(expired))
			row, cerr = spfreshTaskClaim(tx, s, spfreshTaskSplit, 5, "owner-B", 9_999_999_999_999, 1_000)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(row.owner).To(Equal("owner-B"))
			Expect(row.childA).To(Equal(int64(100)))

			// Claiming an absent task reports not-found.
			_, cerr = spfreshTaskClaim(tx, s, spfreshTaskMerge, 6, "x", 1, 1)
			Expect(cerr).To(MatchError(errSPFreshNotFound))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("changelog appends commit in order with distinct user-versions; incremental reads resume", func() {
		s := newStorage("changelog")
		// Tx 1: two deltas in ONE transaction (distinct user-versions).
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, spfreshAppendDeltas(rtx.Transaction(), s, []spfreshDelta{
				{op: spfreshOpAddCell, ids: []int64{1}},
				{op: spfreshOpAddFine, ids: []int64{1, 10}},
			})
		})
		Expect(err).NotTo(HaveOccurred())
		// Tx 2: one more.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, spfreshAppendDeltas(rtx.Transaction(), s, []spfreshDelta{
				{op: spfreshOpForwardFine, ids: []int64{10, 11, 12}},
			})
		})
		Expect(err).NotTo(HaveOccurred())

		var cursor fdb.Key
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			deltas, last, rerr := spfreshReadDeltasSince(tx, s, nil, 100)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(deltas).To(HaveLen(3))
			Expect(deltas[0].op).To(Equal(spfreshOpAddCell))
			Expect(deltas[1].op).To(Equal(spfreshOpAddFine), "user-versions order entries within a tx")
			Expect(deltas[2].op).To(Equal(spfreshOpForwardFine))
			// Incremental: reading from the second entry's position yields only the third.
			partial, mid, rerr := spfreshReadDeltasSince(tx, s, nil, 2)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(partial).To(HaveLen(2))
			rest, _, rerr := spfreshReadDeltasSince(tx, s, mid, 100)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rest).To(HaveLen(1))
			Expect(rest[0].op).To(Equal(spfreshOpForwardFine))
			cursor = last
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		// An up-to-date cursor reads nothing and keeps its position.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			deltas, last, rerr := spfreshReadDeltasSince(rtx.Transaction(), s, cursor, 100)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(deltas).To(BeEmpty())
			Expect([]byte(last)).To(Equal([]byte(cursor)), "empty read must not lose the cursor")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("staging and sidecar round-trip raw fp16", func() {
		s := newStorage("staging")
		pk := tuple.Tuple{int64(1), "k"}
		fp16 := vectorcodec.SerializeHalf([]float64{1, 2, 3, 4})
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSaveStaging(tx, s, 3, pk, fp16)
			spfreshSaveSidecar(tx, s, pk, fp16)
			pks, vecs, lerr := spfreshLoadStagingCell(tx, s, 3)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(pks).To(HaveLen(1))
			Expect(pks[0].Pack()).To(Equal(pk.Pack()))
			Expect(vecs[0]).To(Equal(fp16))
			// Other cells are empty.
			pks, _, lerr = spfreshLoadStagingCell(tx, s, 4)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(pks).To(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
