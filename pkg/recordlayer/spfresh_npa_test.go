package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("SPFresh NPA reassignment", func() {
	ctx := context.Background()

	testConfig := func() SPFreshConfig {
		c := DefaultSPFreshConfig(2)
		c.Lmax = 32
		c.CellTarget = 4
		c.CellMax = 8
		return c
	}

	It("moves a boundary vector to a nearer split child (§6 step 3)", func() {
		config := testConfig()
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-npa").Sub("move"), 1)

		// Cluster A around (0,0) with a boundary member at (2.8, 0); cluster
		// B split across (5,0) and (8,0) so its 2-means children land near
		// those — childA at ~(5,0) is then NEARER to the boundary point than
		// its current centroid (~(0.3,0)).
		var inputs []spfreshBuildInput
		id := int64(1)
		for i := 0; i < 8; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{float64(i%2) * 0.1, float64(i%3) * 0.1}})
			id++
		}
		boundary := tuple.Tuple{int64(100)}
		inputs = append(inputs, spfreshBuildInput{pk: boundary, vec: []float64{2.8, 0}})
		for i := 0; i < 4; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{5 + float64(i%2)*0.1, float64(i%2) * 0.1}})
			id++
		}
		for i := 0; i < 4; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{8 + float64(i%2)*0.1, float64(i%2) * 0.1}})
			id++
		}
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-1")
		Expect(builder.build(ctx, inputs, 42)).To(Succeed())

		// Find the posting holding the cluster-B members (the one to split)
		// and the boundary's current membership.
		var bFine, bCell int64
		var oldMembership []int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{id - 1}) // a (8,0) member
			Expect(merr).NotTo(HaveOccurred())
			Expect(mem).NotTo(BeEmpty())
			bFine = mem[0]
			var ferr error
			bCell, ferr = spfreshFindCentroidCell(tx, storage, bFine)
			Expect(ferr).NotTo(HaveOccurred())
			oldMembership, merr = spfreshReadMembership(tx, storage, boundary)
			Expect(merr).NotTo(HaveOccurred())
			Expect(oldMembership).NotTo(ContainElement(bFine), "boundary must start OUTSIDE the split posting (it is the neighborhood under test)")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Split the B posting, then run the NPA follow-up it enqueued.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskSplit, bFine)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		out, err := spfreshSealFine(ctx, sharedDB, storage, "npa-test", bCell, bFine)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())
		Expect(spfreshSplitFine(ctx, sharedDB, storage, config, "npa-test", bCell, bFine, 7)).To(Succeed())

		// The split enqueued the NPA task.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			data, gerr := rtx.Transaction().Get(storage.taskKey(spfreshTaskNPA, bFine)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(data).NotTo(BeNil(), "split must enqueue the NPA follow-up")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(errOnly(spfreshNPARun(ctx, sharedDB, storage, config, "npa-test", bFine, nil))).To(Succeed())

		// The boundary's copy-set now includes the nearer child; its posting
		// entry exists there; the task row is gone. Re-running is a no-op.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			// Which child actually sits near (5,0) is 2-means order; assert
			// against the one nearer to the boundary vector.
			nearChild := out.childA
			rowA, raErr := spfreshReadCentroidForWrite(tx, storage, bCell, out.childA)
			Expect(raErr).NotTo(HaveOccurred())
			rowB, rbErr := spfreshReadCentroidForWrite(tx, storage, bCell, out.childB)
			Expect(rbErr).NotTo(HaveOccurred())
			vecA, vaErr := rowA.vector()
			Expect(vaErr).NotTo(HaveOccurred())
			vecB, vbErr := rowB.vector()
			Expect(vbErr).NotTo(HaveOccurred())
			bv := []float64{2.8, 0}
			if spfreshSquaredDistance(bv, vecB) < spfreshSquaredDistance(bv, vecA) {
				nearChild = out.childB
			}
			mem, merr := spfreshReadMembership(tx, storage, boundary)
			Expect(merr).NotTo(HaveOccurred())
			Expect(mem).To(ContainElement(nearChild),
				"NPA must move the boundary vector toward the nearer child (old membership %v, new %v)", oldMembership, mem)
			data, gerr := tx.Get(storage.postingKey(nearChild, boundary)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(data).NotTo(BeNil(), "membership and posting must agree")
			task, terr := tx.Get(storage.taskKey(spfreshTaskNPA, bFine)).Get()
			Expect(terr).NotTo(HaveOccurred())
			Expect(task).To(BeNil(), "NPA task cleared on completion")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(errOnly(spfreshNPARun(ctx, sharedDB, storage, config, "npa-test", bFine, nil))).To(Succeed(), "re-run on a cleared task is a no-op")
		// Membership/posting invariant across the WHOLE index after NPA:
		// every membership entry has its posting row.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			for _, in := range inputs {
				mem, merr := spfreshReadMembership(tx, storage, in.pk)
				Expect(merr).NotTo(HaveOccurred())
				for _, fineID := range mem {
					data, gerr := tx.Get(storage.postingKey(fineID, in.pk)).Get()
					Expect(gerr).NotTo(HaveOccurred())
					Expect(data).NotTo(BeNil(), "membership names a posting entry that does not exist")
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a zombie NPA task (parent HDR gone) is deleted as a no-op", func() {
		config := testConfig()
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-npa").Sub("zombie"), 1)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			tx.Set(storage.taskKey(spfreshTaskNPA, 12345), encodeTaskRow(spfreshTaskRow{childA: 1, childB: 2}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(errOnly(spfreshNPARun(ctx, sharedDB, storage, config, "npa-test", 12345, nil))).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			data, gerr := rtx.Transaction().Get(storage.taskKey(spfreshTaskNPA, 12345)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(data).To(BeNil(), "zombie NPA task deleted")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
