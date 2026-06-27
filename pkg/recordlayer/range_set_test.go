package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RangeSet", func() {
	ctx := context.Background()

	newRangeSet := func() *RangeSet {
		ss := subspace.FromBytes(tuple.Tuple{CurrentSpecReport().FullText(), "rangeset"}.Pack())
		return NewRangeSet(ss)
	}

	Describe("Contains", func() {
		It("returns false for empty set", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				result, err := rs.Contains(rtx.Transaction(), []byte{0x50})
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns true for key inside a range", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				changed, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				result, err := rs.Contains(rtx.Transaction(), []byte{0x20})
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false for key outside a range", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())

				result, err := rs.Contains(rtx.Transaction(), []byte{0x60})
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns true for key at range begin (inclusive)", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())

				result, err := rs.Contains(rtx.Transaction(), []byte{0x10})
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false for key at range end (exclusive)", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())

				result, err := rs.Contains(rtx.Transaction(), []byte{0x50})
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects empty key", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.Contains(rtx.Transaction(), []byte{})
				Expect(err).To(BeAssignableToTypeOf(&RangeSetEmptyKeyError{}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects key >= 0xff", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.Contains(rtx.Transaction(), []byte{0xff})
				Expect(err).To(BeAssignableToTypeOf(&RangeSetKeyTooLargeError{}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("InsertRange", func() {
		It("inserts a range into empty set", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				changed, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				// Verify contents
				yes, err := rs.Contains(rtx.Transaction(), []byte{0x20})
				Expect(err).NotTo(HaveOccurred())
				Expect(yes).To(BeTrue())

				no, err := rs.Contains(rtx.Transaction(), []byte{0x60})
				Expect(err).NotTo(HaveOccurred())
				Expect(no).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false for empty range (begin == end)", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				changed, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x10}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles nil begin/end (full range)", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				changed, err := rs.InsertRange(rtx.Transaction(), nil, nil, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				// Everything should be contained
				yes, err := rs.Contains(rtx.Transaction(), []byte{0x01})
				Expect(err).NotTo(HaveOccurred())
				Expect(yes).To(BeTrue())

				yes, err = rs.Contains(rtx.Transaction(), []byte{0xfe})
				Expect(err).NotTo(HaveOccurred())
				Expect(yes).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false when range already fully contained", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())

				// Re-insert same range
				changed, err := rs.InsertRange(rtx.Transaction(), []byte{0x20}, []byte{0x40}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("fills gap between two existing ranges", func() {
			rs := newRangeSet()

			// Insert two disjoint ranges in first transaction.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x20}, false)
				Expect(err).NotTo(HaveOccurred())
				_, err = rs.InsertRange(rtx.Transaction(), []byte{0x40}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Fill gap in second transaction.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				changed, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				// 0x30 should now be covered
				yes, err := rs.Contains(rtx.Transaction(), []byte{0x30})
				Expect(err).NotTo(HaveOccurred())
				Expect(yes).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects inverted range", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x50}, []byte{0x10}, false)
				Expect(err).To(BeAssignableToTypeOf(&RangeSetInvertedRangeError{}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("InsertRange with requireEmpty", func() {
		It("inserts into empty range", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				changed, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x50}, true)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				yes, err := rs.Contains(rtx.Transaction(), []byte{0x20})
				Expect(err).NotTo(HaveOccurred())
				Expect(yes).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false when range overlaps existing (before covers begin)", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x30}, false)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				changed, err := rs.InsertRange(rtx.Transaction(), []byte{0x20}, []byte{0x50}, true)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false when range has entries inside", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x30}, []byte{0x40}, false)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				changed, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x50}, true)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("consolidates abutting before-range", func() {
			rs := newRangeSet()
			// Insert [0x10, 0x30)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x30}, false)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Insert [0x30, 0x50) with requireEmpty — should consolidate with [0x10, 0x30).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				changed, err := rs.InsertRange(rtx.Transaction(), []byte{0x30}, []byte{0x50}, true)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				// Full range [0x10, 0x50) should be covered
				yes, err := rs.Contains(rtx.Transaction(), []byte{0x20})
				Expect(err).NotTo(HaveOccurred())
				Expect(yes).To(BeTrue())
				yes, err = rs.Contains(rtx.Transaction(), []byte{0x40})
				Expect(err).NotTo(HaveOccurred())
				Expect(yes).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("MissingRanges", func() {
		It("returns full range for empty set", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				ranges, err := rs.MissingRanges(rtx.Transaction(), nil, nil, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(ranges).To(HaveLen(1))
				Expect(ranges[0].Begin).To(Equal(rangeSetFirstKey))
				Expect(ranges[0].End).To(Equal(rangeSetFinalKey))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns no ranges for full set", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), nil, nil, false)
				Expect(err).NotTo(HaveOccurred())

				ranges, err := rs.MissingRanges(rtx.Transaction(), nil, nil, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(ranges).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns gaps between ranges", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x20}, false)
				Expect(err).NotTo(HaveOccurred())
				_, err = rs.InsertRange(rtx.Transaction(), []byte{0x40}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())

				ranges, err := rs.MissingRanges(rtx.Transaction(), nil, nil, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(ranges).To(HaveLen(3))
				// Before first: [0x00, 0x10)
				Expect(ranges[0].Begin).To(Equal(rangeSetFirstKey))
				Expect(ranges[0].End).To(Equal([]byte{0x10}))
				// Between: [0x20, 0x40)
				Expect(ranges[1].Begin).To(Equal([]byte{0x20}))
				Expect(ranges[1].End).To(Equal([]byte{0x40}))
				// After last: [0x50, 0xff)
				Expect(ranges[2].Begin).To(Equal([]byte{0x50}))
				Expect(ranges[2].End).To(Equal(rangeSetFinalKey))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("respects limit", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x20}, false)
				Expect(err).NotTo(HaveOccurred())
				_, err = rs.InsertRange(rtx.Transaction(), []byte{0x40}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())

				ranges, err := rs.MissingRanges(rtx.Transaction(), nil, nil, 1)
				Expect(err).NotTo(HaveOccurred())
				Expect(ranges).To(HaveLen(1))
				Expect(ranges[0].Begin).To(Equal(rangeSetFirstKey))
				Expect(ranges[0].End).To(Equal([]byte{0x10}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("scopes to given begin/end", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x30}, []byte{0x40}, false)
				Expect(err).NotTo(HaveOccurred())

				// Only look in [0x20, 0x60)
				ranges, err := rs.MissingRanges(rtx.Transaction(), []byte{0x20}, []byte{0x60}, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(ranges).To(HaveLen(2))
				Expect(ranges[0].Begin).To(Equal([]byte{0x20}))
				Expect(ranges[0].End).To(Equal([]byte{0x30}))
				Expect(ranges[1].Begin).To(Equal([]byte{0x40}))
				Expect(ranges[1].End).To(Equal([]byte{0x60}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns nothing when queried range is fully covered", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x50}, false)
				Expect(err).NotTo(HaveOccurred())

				ranges, err := rs.MissingRanges(rtx.Transaction(), []byte{0x20}, []byte{0x40}, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(ranges).To(BeEmpty())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("IsEmpty", func() {
		It("returns true for empty set", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				empty, err := rs.IsEmpty(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(empty).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false after insert", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x20}, false)
				Expect(err).NotTo(HaveOccurred())

				empty, err := rs.IsEmpty(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(empty).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns false for full set", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), nil, nil, false)
				Expect(err).NotTo(HaveOccurred())

				empty, err := rs.IsEmpty(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(empty).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Clear", func() {
		It("removes all ranges", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := rs.InsertRange(rtx.Transaction(), nil, nil, false)
				Expect(err).NotTo(HaveOccurred())

				rs.Clear(rtx.Transaction())

				empty, err := rs.IsEmpty(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(empty).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("multi-key ranges", func() {
		It("handles multi-byte keys correctly", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// Insert a range with multi-byte keys
				begin := []byte{0x10, 0x20, 0x30}
				end := []byte{0x50, 0x60, 0x70}
				changed, err := rs.InsertRange(rtx.Transaction(), begin, end, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				// Key inside range
				yes, err := rs.Contains(rtx.Transaction(), []byte{0x30, 0x00})
				Expect(err).NotTo(HaveOccurred())
				Expect(yes).To(BeTrue())

				// Key outside range
				no, err := rs.Contains(rtx.Transaction(), []byte{0x60, 0x00})
				Expect(err).NotTo(HaveOccurred())
				Expect(no).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles primary key bytes from tuple packing", func() {
			// This simulates how the online indexer uses RangeSet:
			// primary keys are tuple-packed byte arrays.
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				pk1 := tuple.Tuple{int64(1)}.Pack()
				pk2 := tuple.Tuple{int64(100)}.Pack()

				changed, err := rs.InsertRange(rtx.Transaction(), pk1, pk2, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				// pk 50 should be contained (between 1 and 100 in tuple encoding)
				pk50 := tuple.Tuple{int64(50)}.Pack()
				yes, err := rs.Contains(rtx.Transaction(), pk50)
				Expect(err).NotTo(HaveOccurred())
				Expect(yes).To(BeTrue())

				// pk 200 should not be contained
				pk200 := tuple.Tuple{int64(200)}.Pack()
				no, err := rs.Contains(rtx.Transaction(), pk200)
				Expect(err).NotTo(HaveOccurred())
				Expect(no).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("incremental building pattern", func() {
		It("simulates online index build with requireEmpty=true", func() {
			rs := newRangeSet()

			// Simulate building in 3 chunks: [0x00, 0x30), [0x30, 0x60), [0x60, 0xff)
			chunks := [][2][]byte{
				{[]byte{0x00}, []byte{0x30}},
				{[]byte{0x30}, []byte{0x60}},
				{[]byte{0x60}, rangeSetFinalKey},
			}

			for _, chunk := range chunks {
				_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
					// Find first missing range
					missing, err := rs.MissingRanges(rtx.Transaction(), nil, nil, 1)
					Expect(err).NotTo(HaveOccurred())
					Expect(missing).NotTo(BeEmpty())

					// Insert with requireEmpty (atomic test-and-set)
					changed, err := rs.InsertRange(rtx.Transaction(), chunk[0], chunk[1], true)
					Expect(err).NotTo(HaveOccurred())
					Expect(changed).To(BeTrue())
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// After all chunks, set should be full.
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				missing, err := rs.MissingRanges(rtx.Transaction(), nil, nil, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(missing).To(BeEmpty())

				empty, err := rs.IsEmpty(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(empty).To(BeFalse())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("overlapping inserts", func() {
		It("handles overlapping ranges correctly", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// Insert [0x10, 0x40)
				_, err := rs.InsertRange(rtx.Transaction(), []byte{0x10}, []byte{0x40}, false)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				// Insert overlapping [0x30, 0x60) — extends the range
				changed, err := rs.InsertRange(rtx.Transaction(), []byte{0x30}, []byte{0x60}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(changed).To(BeTrue())

				// 0x50 should now be covered
				yes, err := rs.Contains(rtx.Transaction(), []byte{0x50})
				Expect(err).NotTo(HaveOccurred())
				Expect(yes).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("wire format", func() {
		It("stores key as tuple-packed bytes and value as raw bytes", func() {
			rs := newRangeSet()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				begin := []byte{0x10}
				end := []byte{0x50}
				_, err := rs.InsertRange(rtx.Transaction(), begin, end, false)
				Expect(err).NotTo(HaveOccurred())

				// Read raw FDB data to verify wire format
				ssBegin, ssEnd := rs.subspace.FDBRangeKeys()
				kvs, err := rtx.Transaction().GetRange(
					fdb.KeyRange{Begin: ssBegin, End: ssEnd},
					fdb.RangeOptions{},
				).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(kvs).To(HaveLen(1))

				// Key should be tuple-unpacked to the begin bytes
				unpacked, err := rs.subspace.Unpack(kvs[0].Key)
				Expect(err).NotTo(HaveOccurred())
				Expect(unpacked[0].([]byte)).To(Equal(begin))

				// Value should be raw end bytes (NOT tuple-packed)
				Expect(kvs[0].Value).To(Equal(end))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
