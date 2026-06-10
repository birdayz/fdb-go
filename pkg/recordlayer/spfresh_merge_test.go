package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("SPFresh merge lifecycle", func() {
	ctx := context.Background()

	testConfig := func() SPFreshConfig {
		c := DefaultSPFreshConfig(2)
		c.Lmax = 32
		c.CellTarget = 4
		c.CellMax = 8
		return c
	}

	// buildTwoClusters: builds an index with two separated clusters so there
	// are >= 2 fine centroids; returns storage and the inputs.
	buildTwoClusters := func(sub string) (*spfreshStorage, []spfreshBuildInput) {
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-merge").Sub(sub), 1)
		var inputs []spfreshBuildInput
		id := int64(1)
		for i := 0; i < 8; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{float64(i%2) * 0.1, float64(i%3) * 0.1}})
			id++
		}
		for i := 0; i < 8; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{50 + float64(i%2)*0.1, float64(i%3) * 0.1}})
			id++
		}
		builder := newSPFreshBuilder(sharedDB, storage, testConfig(), "builder-1")
		Expect(builder.build(ctx, inputs, 42)).To(Succeed())
		return storage, inputs
	}

	It("drains a posting into its nearest ACTIVE siblings and forwards it", func() {
		storage, inputs := buildTwoClusters("drain")
		config := testConfig()

		// Pick the posting holding pk 1 (cluster A).
		var fineID, cellID int64
		var members []tuple.Tuple
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
			Expect(merr).NotTo(HaveOccurred())
			Expect(mem).NotTo(BeEmpty())
			fineID = mem[0]
			var ferr error
			cellID, ferr = spfreshFindCentroidCell(tx, storage, fineID)
			Expect(ferr).NotTo(HaveOccurred())
			entries, perr := spfreshLoadPostingForSplit(tx, storage, fineID)
			Expect(perr).NotTo(HaveOccurred())
			for _, e := range entries {
				members = append(members, e.pk)
			}
			Expect(members).NotTo(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskMerge, fineID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(spfreshMergeFine(ctx, sharedDB, storage, config, "merge-test", cellID, fineID)).To(Succeed())
		// Idempotent: a commit_unknown-style re-run no-ops on the FORWARD row.
		Expect(spfreshMergeFine(ctx, sharedDB, storage, config, "merge-test", cellID, fineID)).To(Succeed())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cent, cerr := spfreshReadCentroidForWrite(tx, storage, cellID, fineID)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(cent.state).To(Equal(spfreshStateForward), "merged centroid forwards to its targets")
			Expect(cent.childA).NotTo(BeZero())
			entries, perr := spfreshLoadPostingForSplit(tx, storage, fineID)
			Expect(perr).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty(), "merged posting drained behind the HDR")
			hdr, herr := tx.Get(storage.postingHDRKey(fineID)).Get()
			Expect(herr).NotTo(HaveOccurred())
			Expect(hdr).NotTo(BeNil())
			task, terr := tx.Get(storage.taskKey(spfreshTaskMerge, fineID)).Get()
			Expect(terr).NotTo(HaveOccurred())
			Expect(task).To(BeNil(), "merge task cleared")
			// Every former member still has copies, none naming the merged
			// posting, all agreeing with posting rows.
			for _, pk := range members {
				mem, merr := spfreshReadMembership(tx, storage, pk)
				Expect(merr).NotTo(HaveOccurred())
				Expect(mem).NotTo(BeEmpty(), "merge dropped a member's last copy")
				Expect(mem).NotTo(ContainElement(fineID))
				for _, id := range mem {
					data, gerr := tx.Get(storage.postingKey(id, pk)).Get()
					Expect(gerr).NotTo(HaveOccurred())
					Expect(data).NotTo(BeNil(), "membership names a missing posting entry")
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Queries still find everything (members reachable via targets).
		cache := newSPFreshRoutingCache(0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			Expect(cache.fullReload(tx, storage, 1)).To(Succeed())
			searcher := newSPFreshSearcher(storage, config, cache)
			got, serr := searcher.search(tx, inputs[0].vec, len(inputs))
			Expect(serr).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(len(inputs)), "all records visible after the merge")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("the post-split cooldown skips young children (oscillation guard)", func() {
		storage, _ := buildTwoClusters("cooldown")
		config := testConfig()

		// Split a posting: its children carry epoch = now.
		var fineID, cellID int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
			Expect(merr).NotTo(HaveOccurred())
			fineID = mem[0]
			var ferr error
			cellID, ferr = spfreshFindCentroidCell(tx, storage, fineID)
			return nil, ferr
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskSplit, fineID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		out, err := spfreshSealFine(ctx, sharedDB, storage, "merge-test", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())
		Expect(spfreshSplitFine(ctx, sharedDB, storage, config, "merge-test", cellID, fineID, 7)).To(Succeed())

		// A merge trigger on the fresh child must be skipped by the cooldown.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskMerge, out.childA)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(spfreshMergeFine(ctx, sharedDB, storage, config, "merge-test", cellID, out.childA)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cent, cerr := spfreshReadCentroidForWrite(tx, storage, cellID, out.childA)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(cent.state).To(Equal(spfreshStateActive), "cooldown must keep the young child intact")
			task, terr := tx.Get(storage.taskKey(spfreshTaskMerge, out.childA)).Get()
			Expect(terr).NotTo(HaveOccurred())
			Expect(task).To(BeNil(), "skipped trigger cleared (re-filed by the next probe)")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a merge task on a SEALED centroid is a no-op (the split owns it)", func() {
		storage, _ := buildTwoClusters("sealed")
		config := testConfig()
		var fineID, cellID int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
			Expect(merr).NotTo(HaveOccurred())
			fineID = mem[0]
			var ferr error
			cellID, ferr = spfreshFindCentroidCell(tx, storage, fineID)
			return nil, ferr
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskSplit, fineID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		out, err := spfreshSealFine(ctx, sharedDB, storage, "merge-test", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskMerge, fineID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(spfreshMergeFine(ctx, sharedDB, storage, config, "merge-test", cellID, fineID)).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cent, cerr := spfreshReadCentroidForWrite(tx, storage, cellID, fineID)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(cent.state).To(Equal(spfreshStateSealed), "merge must not touch a sealed centroid")
			entries, perr := spfreshLoadPostingForSplit(tx, storage, fineID)
			Expect(perr).NotTo(HaveOccurred())
			Expect(entries).NotTo(BeEmpty(), "sealed posting untouched")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
