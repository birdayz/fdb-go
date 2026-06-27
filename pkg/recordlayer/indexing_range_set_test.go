package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("IndexingRangeSet", func() {
	var idx *Index

	BeforeEach(func() {
		idx = NewIndex("test_range_idx", Field("order_id"))
	})

	It("EmptyRangeSetIsNotComplete", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			irs := NewIndexingRangeSet(specSubspace(), idx)

			complete, err := irs.IsComplete(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(complete).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("FullRangeIsComplete", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			irs := NewIndexingRangeSet(specSubspace(), idx)

			modified, err := irs.InsertRange(rtx.Transaction(), nil, nil, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(modified).To(BeTrue())

			complete, err := irs.IsComplete(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(complete).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ContainsKeyInBuiltRange", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			irs := NewIndexingRangeSet(specSubspace(), idx)

			// Build range [0x01, 0x50)
			begin := []byte{0x01}
			end := []byte{0x50}
			_, err := irs.InsertRange(rtx.Transaction(), begin, end, false)
			Expect(err).NotTo(HaveOccurred())

			// Key inside range
			contains, err := irs.ContainsKey(rtx.Transaction(), []byte{0x20})
			Expect(err).NotTo(HaveOccurred())
			Expect(contains).To(BeTrue())

			// Key outside range
			contains, err = irs.ContainsKey(rtx.Transaction(), []byte{0x60})
			Expect(err).NotTo(HaveOccurred())
			Expect(contains).To(BeFalse())

			// Key at boundary (begin is inclusive)
			contains, err = irs.ContainsKey(rtx.Transaction(), []byte{0x01})
			Expect(err).NotTo(HaveOccurred())
			Expect(contains).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ContainsKeyWithTuplePackedPrimaryKey", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			irs := NewIndexingRangeSet(specSubspace(), idx)

			// Build full range
			_, err := irs.InsertRange(rtx.Transaction(), nil, nil, true)
			Expect(err).NotTo(HaveOccurred())

			// Check a tuple-packed primary key
			pk := tuple.Tuple{int64(42)}.Pack()
			contains, err := irs.ContainsKey(rtx.Transaction(), pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(contains).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("FirstMissingRangeReturnsGap", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			irs := NewIndexingRangeSet(specSubspace(), idx)

			// Build range [0x01, 0x50)
			_, err := irs.InsertRange(rtx.Transaction(), []byte{0x01}, []byte{0x50}, false)
			Expect(err).NotTo(HaveOccurred())

			// First missing should be [0x00, 0x01)
			missing, err := irs.FirstMissingRange(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(missing).NotTo(BeNil())
			Expect(missing.Begin).To(Equal(rangeSetFirstKey))
			Expect(missing.End).To(Equal([]byte{0x01}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("FirstMissingRangeNilWhenComplete", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			irs := NewIndexingRangeSet(specSubspace(), idx)

			_, err := irs.InsertRange(rtx.Transaction(), nil, nil, true)
			Expect(err).NotTo(HaveOccurred())

			missing, err := irs.FirstMissingRange(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(missing).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ListMissingRangesMultipleGaps", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			irs := NewIndexingRangeSet(specSubspace(), idx)

			// Build two non-adjacent ranges: [0x10, 0x20) and [0x40, 0x60)
			_, err := irs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x20}, false)
			Expect(err).NotTo(HaveOccurred())
			_, err = irs.InsertRange(rtx.Transaction(), []byte{0x40}, []byte{0x60}, false)
			Expect(err).NotTo(HaveOccurred())

			// Should have 3 gaps: [0x00, 0x10), [0x20, 0x40), [0x60, 0xff)
			gaps, err := irs.ListMissingRanges(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(gaps).To(HaveLen(3))

			Expect(gaps[0].Begin).To(Equal(rangeSetFirstKey))
			Expect(gaps[0].End).To(Equal([]byte{0x10}))

			Expect(gaps[1].Begin).To(Equal([]byte{0x20}))
			Expect(gaps[1].End).To(Equal([]byte{0x40}))

			Expect(gaps[2].Begin).To(Equal([]byte{0x60}))
			Expect(gaps[2].End).To(Equal(rangeSetFinalKey))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ClearRemovesAllTracking", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			irs := NewIndexingRangeSet(specSubspace(), idx)

			// Mark complete
			_, err := irs.InsertRange(rtx.Transaction(), nil, nil, true)
			Expect(err).NotTo(HaveOccurred())

			complete, err := irs.IsComplete(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(complete).To(BeTrue())

			// Clear
			irs.Clear(rtx.Transaction())

			// Should be incomplete again
			complete, err = irs.IsComplete(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(complete).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RequireEmptyRejectsOverlap", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			irs := NewIndexingRangeSet(specSubspace(), idx)

			// First insert
			modified, err := irs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x30}, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(modified).To(BeTrue())

			// Overlapping insert with requireEmpty=true should return false
			modified, err = irs.InsertRange(rtx.Transaction(), []byte{0x20}, []byte{0x40}, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(modified).To(BeFalse())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("IncrementalBuildSimulation", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			irs := NewIndexingRangeSet(specSubspace(), idx)

			// Simulate building in chunks
			pk1 := tuple.Tuple{int64(1)}.Pack()
			pk2 := tuple.Tuple{int64(100)}.Pack()
			pk3 := tuple.Tuple{int64(200)}.Pack()

			// Build chunk 1: [first, pk1)
			_, err := irs.InsertRange(rtx.Transaction(), nil, pk1, true)
			Expect(err).NotTo(HaveOccurred())

			complete, err := irs.IsComplete(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(complete).To(BeFalse())

			// Build chunk 2: [pk1, pk2)
			_, err = irs.InsertRange(rtx.Transaction(), pk1, pk2, true)
			Expect(err).NotTo(HaveOccurred())

			// Build chunk 3: [pk2, pk3)
			_, err = irs.InsertRange(rtx.Transaction(), pk2, pk3, true)
			Expect(err).NotTo(HaveOccurred())

			// Build final chunk: [pk3, final)
			_, err = irs.InsertRange(rtx.Transaction(), pk3, nil, true)
			Expect(err).NotTo(HaveOccurred())

			// Should now be complete
			complete, err = irs.IsComplete(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(complete).To(BeTrue())

			// All keys should be in range
			for _, pk := range [][]byte{pk1, pk2, pk3} {
				contains, err := irs.ContainsKey(rtx.Transaction(), pk)
				Expect(err).NotTo(HaveOccurred())
				Expect(contains).To(BeTrue())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
