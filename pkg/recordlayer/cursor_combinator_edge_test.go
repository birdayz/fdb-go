package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("CursorCombinatorEdgeCases", func() {
	ctx := context.Background()

	// Helper: drain a cursor into a slice, returning values + final NoNextReason.
	drain := func(cursor RecordCursor[int]) ([]int, NoNextReason) {
		var vals []int
		var reason NoNextReason
		for {
			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			if !result.HasNext() {
				reason = result.GetNoNextReason()
				break
			}
			vals = append(vals, result.GetValue())
		}
		return vals, reason
	}

	It("ConcatCursors with two empty cursors produces no results and source-exhausted", func() {
		cursor := ConcatCursors[int](
			func(_ []byte) RecordCursor[int] { return Empty[int]() },
			func(_ []byte) RecordCursor[int] { return Empty[int]() },
			nil,
		)

		vals, reason := drain(cursor)
		Expect(vals).To(BeEmpty())
		Expect(reason).To(Equal(SourceExhausted))
		Expect(cursor.Close()).To(Succeed())
	})

	It("FilterCursor that filters everything returns 0 results with source-exhausted", func() {
		inner := FromList([]int{1, 2, 3, 4, 5})
		cursor := &filterCursor[int]{
			inner:     inner,
			predicate: func(_ int) bool { return false },
		}

		vals, reason := drain(cursor)
		Expect(vals).To(BeEmpty())
		Expect(reason).To(Equal(SourceExhausted))
		Expect(cursor.Close()).To(Succeed())
	})

	It("FilterCursor preserves continuation on limit", func() {
		// 10 items, filter keeps even-indexed values (0-based: items at pos 0,2,4,6,8 = values 1,3,5,7,9)
		inner := FromList([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
		idx := 0
		filtered := &filterCursor[int]{
			inner: inner,
			predicate: func(_ int) bool {
				keep := idx%2 == 0
				idx++
				return keep
			},
		}
		limited := LimitRowsCursor[int](filtered, 2)

		var vals []int
		var lastResult RecordCursorResult[int]
		for {
			result, err := limited.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			if !result.HasNext() {
				lastResult = result
				break
			}
			vals = append(vals, result.GetValue())
		}

		Expect(vals).To(HaveLen(2))
		Expect(vals).To(Equal([]int{1, 3}))

		// Continuation should be valid (not nil, not end) since more items remain.
		cont := lastResult.GetContinuation()
		Expect(cont).NotTo(BeNil())
		Expect(cont.IsEnd()).To(BeFalse())
		Expect(lastResult.GetNoNextReason()).To(Equal(ReturnLimitReached))
		Expect(limited.Close()).To(Succeed())
	})

	It("MapErrCursor propagates error from transform on 3rd element", func() {
		inner := FromList([]int{10, 20, 30, 40, 50})
		errBoom := fmt.Errorf("boom on third")
		callCount := 0
		cursor := MapErrCursor[int, int](inner, func(v int) (int, error) {
			callCount++
			if callCount == 3 {
				return 0, errBoom
			}
			return v * 2, nil
		})

		// Should get first 2 results successfully.
		result1, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(result1.HasNext()).To(BeTrue())
		Expect(result1.GetValue()).To(Equal(20))

		result2, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(result2.HasNext()).To(BeTrue())
		Expect(result2.GetValue()).To(Equal(40))

		// Third call should return the error.
		_, err = cursor.OnNext(ctx)
		Expect(err).To(MatchError("boom on third"))
		Expect(cursor.Close()).To(Succeed())
	})

	It("LimitRowsCursor with limit=0 returns empty result with source exhausted", func() {
		inner := FromList([]int{1, 2, 3})
		cursor := LimitRowsCursor[int](inner, 0)

		result, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.HasNext()).To(BeFalse())
		Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		Expect(cursor.Close()).To(Succeed())
	})

	It("SkipCursor skipping past all elements returns 0 results", func() {
		inner := FromList([]int{1, 2, 3})
		cursor := SkipCursor[int](inner, 5)

		vals, reason := drain(cursor)
		Expect(vals).To(BeEmpty())
		Expect(reason).To(Equal(SourceExhausted))
		Expect(cursor.Close()).To(Succeed())
	})

	It("Deep composition: Filter(Map(Limit(FromList)))", func() {
		// FromList of 10 ints, limit=7 -> [1..7], map *2 -> [2,4,6,8,10,12,14], filter >10 -> [12,14]
		inner := FromList([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
		limited := LimitRowsCursor[int](inner, 7)
		mapped := MapCursor[int, int](limited, func(v int) int { return v * 2 })
		cursor := &filterCursor[int]{
			inner:     mapped,
			predicate: func(v int) bool { return v > 10 },
		}

		vals, reason := drain(cursor)
		Expect(vals).To(Equal([]int{12, 14}))
		// The limit cursor stops the source, so the filter sees ReturnLimitReached
		// after the 7 items are consumed, not SourceExhausted. However, the filter
		// propagates the inner cursor's stop reason.
		Expect(reason).To(Equal(ReturnLimitReached))
		Expect(cursor.Close()).To(Succeed())
	})

	It("UnionCursor with two empty cursors is empty with source-exhausted", func() {
		c1 := Empty[int]()
		c2 := Empty[int]()
		cursor := Union[int](
			[]RecordCursor[int]{c1, c2},
			func(v int) tuple.Tuple { return tuple.Tuple{v} },
			false,
		)

		vals, reason := drain(cursor)
		Expect(vals).To(BeEmpty())
		Expect(reason).To(Equal(SourceExhausted))
		Expect(cursor.Close()).To(Succeed())
	})

	It("IntersectionCursor with two empty cursors is empty with source-exhausted", func() {
		c1 := Empty[int]()
		c2 := Empty[int]()
		cursor := Intersection[int](
			[]RecordCursor[int]{c1, c2},
			func(v int) tuple.Tuple { return tuple.Tuple{v} },
			false,
		)

		vals, reason := drain(cursor)
		Expect(vals).To(BeEmpty())
		Expect(reason).To(Equal(SourceExhausted))
		Expect(cursor.Close()).To(Succeed())
	})

	It("ConcatCursors preserves ordering and exhaustion across both cursors", func() {
		cursor := ConcatCursors[int](
			func(_ []byte) RecordCursor[int] { return FromList([]int{1, 2, 3}) },
			func(_ []byte) RecordCursor[int] { return FromList([]int{4, 5}) },
			nil,
		)

		vals, reason := drain(cursor)
		Expect(vals).To(Equal([]int{1, 2, 3, 4, 5}))
		Expect(reason).To(Equal(SourceExhausted))
		Expect(cursor.Close()).To(Succeed())
	})
})
