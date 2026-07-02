package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// errAfterNCursor returns N values from items then errors on the (N+1)th OnNext call.
type errAfterNCursor[T any] struct {
	items []T
	pos   int
	err   error
}

func newErrAfterNCursor[T any](items []T, err error) *errAfterNCursor[T] {
	return &errAfterNCursor[T]{items: items, err: err}
}

func (c *errAfterNCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	if c.pos < len(c.items) {
		val := c.items[c.pos]
		c.pos++
		return NewResultWithValue(val, &BytesContinuation{bytes: listCursorContinuation(c.pos)}), nil
	}
	return RecordCursorResult[T]{}, c.err
}

func (c *errAfterNCursor[T]) Close() error { return nil }

func (c *errAfterNCursor[T]) IsClosed() bool { return false }

// faultAfterNCursor wraps a real cursor and injects an error after N successful results.
// Used to test AutoContinuingCursor's recovery from mid-scan errors.
type faultAfterNCursor[T any] struct {
	inner    RecordCursor[T]
	afterN   int
	faultErr error
	count    int
}

func (c *faultAfterNCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.count >= c.afterN {
		return RecordCursorResult[T]{}, c.faultErr
	}
	result, err := c.inner.OnNext(ctx)
	if err == nil && result.HasNext() {
		c.count++
	}
	return result, err
}

func (c *faultAfterNCursor[T]) Close() error { return c.inner.Close() }

func (c *faultAfterNCursor[T]) IsClosed() bool { return c.inner.IsClosed() }

// oobStopCursorUnit returns N values then stops with a non-exhaustion reason.
type oobStopCursorUnit[T any] struct {
	items    []T
	pos      int
	reason   NoNextReason
	stopCont RecordCursorContinuation
}

func newOOBStopCursorUnit[T any](items []T, reason NoNextReason, stopCont RecordCursorContinuation) *oobStopCursorUnit[T] {
	return &oobStopCursorUnit[T]{items: items, reason: reason, stopCont: stopCont}
}

func (c *oobStopCursorUnit[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	if c.pos < len(c.items) {
		val := c.items[c.pos]
		c.pos++
		return NewResultWithValue(val, &BytesContinuation{bytes: listCursorContinuation(c.pos)}), nil
	}
	return NewResultNoNext[T](c.reason, c.stopCont), nil
}

func (c *oobStopCursorUnit[T]) Close() error { return nil }

func (c *oobStopCursorUnit[T]) IsClosed() bool { return false }

// closeTrackerUnit wraps a cursor and tracks whether Close was called.
type closeTrackerUnit[T any] struct {
	inner  RecordCursor[T]
	closed atomic.Bool
}

func newCloseTrackerUnit[T any](inner RecordCursor[T]) *closeTrackerUnit[T] {
	return &closeTrackerUnit[T]{inner: inner}
}

func (c *closeTrackerUnit[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	return c.inner.OnNext(ctx)
}

func (c *closeTrackerUnit[T]) Close() error {
	c.closed.Store(true)
	return c.inner.Close()
}

func (c *closeTrackerUnit[T]) IsClosed() bool { return c.closed.Load() }

func (c *closeTrackerUnit[T]) wasClosed() bool {
	return c.closed.Load()
}

var _ = Describe("CursorCombinatorsUnit", func() {
	ctx := context.Background()

	// ---- filterCursor ----

	Describe("filterCursor", func() {
		It("returns all items when predicate always true", func() {
			inner := FromList([]int{1, 2, 3, 4, 5})
			cursor := &filterCursor[int]{
				inner:     inner,
				predicate: func(_ int) bool { return true },
			}
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{1, 2, 3, 4, 5}))
		})

		It("returns empty on empty inner cursor", func() {
			cursor := &filterCursor[int]{
				inner:     Empty[int](),
				predicate: func(_ int) bool { return true },
			}
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeEmpty())
		})

		It("propagates inner cursor error", func() {
			inner := newErrAfterNCursor([]int{1, 2}, fmt.Errorf("inner exploded"))
			cursor := &filterCursor[int]{
				inner:     inner,
				predicate: func(_ int) bool { return true },
			}
			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.GetValue()).To(Equal(1))

			r2, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.GetValue()).To(Equal(2))

			_, err = cursor.OnNext(ctx)
			Expect(err).To(MatchError("inner exploded"))
		})

		It("filter + limit interaction: filter returns fewer than limit", func() {
			// 10 items, filter keeps 3 (multiples of 3), limit 5.
			// Should get all 3 matching items, then SourceExhausted (not ReturnLimitReached).
			inner := FromList([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
			filtered := &filterCursor[int]{
				inner:     inner,
				predicate: func(v int) bool { return v%3 == 0 },
			}
			limited := LimitRowsCursor(filtered, 5)

			result, err := AsList(ctx, limited)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{3, 6, 9}))
		})

		It("filter keeps only first and last elements", func() {
			inner := FromList([]int{1, 2, 3, 4, 5})
			cursor := &filterCursor[int]{
				inner:     inner,
				predicate: func(v int) bool { return v == 1 || v == 5 },
			}
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{1, 5}))
		})

		It("filter with limit that stops mid-filter", func() {
			// 5 items, filter keeps all, limit 3.
			inner := FromList([]int{10, 20, 30, 40, 50})
			filtered := &filterCursor[int]{
				inner:     inner,
				predicate: func(_ int) bool { return true },
			}
			limited := LimitRowsCursor(filtered, 3)

			var vals []int
			var lastResult RecordCursorResult[int]
			for {
				r, err := limited.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !r.HasNext() {
					lastResult = r
					break
				}
				vals = append(vals, r.GetValue())
			}
			Expect(vals).To(Equal([]int{10, 20, 30}))
			Expect(lastResult.GetNoNextReason()).To(Equal(ReturnLimitReached))
		})
	})

	// ---- SkipCursor ----

	Describe("SkipCursor", func() {
		It("skip 0 returns the original cursor (identity)", func() {
			inner := FromList([]int{1, 2, 3})
			cursor := SkipCursor(inner, 0)
			// SkipCursor with n<=0 returns the cursor itself.
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{1, 2, 3}))
		})

		It("skip negative returns the original cursor (identity)", func() {
			inner := FromList([]int{7, 8, 9})
			cursor := SkipCursor(inner, -10)
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{7, 8, 9}))
		})

		It("skip exact count returns empty with SourceExhausted", func() {
			inner := FromList([]int{1, 2, 3})
			cursor := SkipCursor(inner, 3)
			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("skip 1 on single element returns empty", func() {
			inner := FromList([]int{42})
			cursor := SkipCursor(inner, 1)
			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("skip on empty cursor returns empty", func() {
			cursor := SkipCursor(Empty[int](), 5)
			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("skip propagates inner error during skipping", func() {
			// Error after 2 items, skip 5 -- error triggers during skip phase.
			inner := newErrAfterNCursor([]int{1, 2}, fmt.Errorf("skip phase boom"))
			cursor := SkipCursor(inner, 5)
			_, err := cursor.OnNext(ctx)
			Expect(err).To(MatchError("skip phase boom"))
		})

		It("skip propagates inner error after skipping", func() {
			// 3 items then error, skip 2 -- first OnNext succeeds, second errors.
			inner := newErrAfterNCursor([]int{10, 20, 30}, fmt.Errorf("post-skip boom"))
			cursor := SkipCursor(inner, 2)

			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeTrue())
			Expect(r1.GetValue()).To(Equal(30))

			_, err = cursor.OnNext(ctx)
			Expect(err).To(MatchError("post-skip boom"))
		})
	})

	// ---- LimitRowsCursor ----

	Describe("LimitRowsCursor", func() {
		It("limit == exact item count: last result is SourceExhausted not ReturnLimitReached", func() {
			// 3 items, limit 3. After getting 3 values, inner is exhausted.
			// The 4th OnNext should return SourceExhausted (inner ran out),
			// not ReturnLimitReached (we consumed our limit but inner still has data).
			inner := FromList([]int{1, 2, 3})
			cursor := LimitRowsCursor(inner, 3)

			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.GetValue()).To(Equal(1))

			r2, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.GetValue()).To(Equal(2))

			r3, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r3.GetValue()).To(Equal(3))

			// The inner cursor is exhausted. LimitRowsCursor decremented remaining to 0.
			// Next call: remaining<=0 branch fires, returns ReturnLimitReached.
			// This is actually correct: we consumed our limit, even though the inner
			// is also exhausted. The limit cursor doesn't know the inner is empty --
			// it just knows it consumed 3 items.
			r4, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r4.HasNext()).To(BeFalse())
			Expect(r4.GetNoNextReason()).To(Equal(ReturnLimitReached))
		})

		It("limit 1 returns exactly one item", func() {
			inner := FromList([]int{10, 20, 30})
			cursor := LimitRowsCursor(inner, 1)

			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeTrue())
			Expect(r1.GetValue()).To(Equal(10))

			r2, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.HasNext()).To(BeFalse())
			Expect(r2.GetNoNextReason()).To(Equal(ReturnLimitReached))
		})

		It("limit > item count returns all items with SourceExhausted", func() {
			inner := FromList([]int{1, 2})
			cursor := LimitRowsCursor(inner, 100)

			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{1, 2}))
		})

		It("propagates inner error", func() {
			inner := newErrAfterNCursor([]int{1}, fmt.Errorf("limit inner error"))
			cursor := LimitRowsCursor(inner, 5)

			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.GetValue()).To(Equal(1))

			_, err = cursor.OnNext(ctx)
			Expect(err).To(MatchError("limit inner error"))
		})

		It("repeated calls after limit always return ReturnLimitReached", func() {
			inner := FromList([]int{1})
			cursor := LimitRowsCursor(inner, 1)

			r1, _ := cursor.OnNext(ctx)
			Expect(r1.HasNext()).To(BeTrue())

			for range 3 {
				r, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(r.HasNext()).To(BeFalse())
				Expect(r.GetNoNextReason()).To(Equal(ReturnLimitReached))
			}
		})
	})

	// ---- OrElse ----

	Describe("OrElse", func() {
		It("primary produces multiple values, alternative never called", func() {
			alternativeCalled := false
			cursor := OrElse(
				FromList([]int{10, 20, 30}),
				func() RecordCursor[int] {
					alternativeCalled = true
					return FromList([]int{999})
				},
			)
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{10, 20, 30}))
			Expect(alternativeCalled).To(BeFalse())
		})

		It("primary errors on first OnNext", func() {
			cursor := OrElse[int](
				&errorCursor[int]{err: fmt.Errorf("primary broken")},
				func() RecordCursor[int] { return FromList([]int{1}) },
			)
			_, err := cursor.OnNext(ctx)
			Expect(err).To(MatchError("primary broken"))
		})

		It("primary returns OOB limit, does not switch, stays undecided for next call", func() {
			// Primary returns ScanLimitReached then real values on retry.
			callNum := 0
			primary := &callbackCursor[int]{
				fn: func(ctx context.Context) (RecordCursorResult[int], error) {
					callNum++
					if callNum == 1 {
						return NewResultNoNext[int](ScanLimitReached,
							&BytesContinuation{bytes: []byte{0x01}}), nil
					}
					if callNum == 2 {
						return NewResultWithValue(42, &BytesContinuation{bytes: []byte{0x02}}), nil
					}
					return NewResultNoNext[int](SourceExhausted, &EndContinuation{}), nil
				},
			}

			alternativeCalled := false
			cursor := OrElse(primary, func() RecordCursor[int] {
				alternativeCalled = true
				return FromList([]int{999})
			})

			// First call: OOB limit, stays undecided.
			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeFalse())
			Expect(r1.GetNoNextReason()).To(Equal(ScanLimitReached))
			Expect(alternativeCalled).To(BeFalse())

			// Second call: primary produces a value this time. Still no alternative.
			r2, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.HasNext()).To(BeTrue())
			Expect(r2.GetValue()).To(Equal(42))
			Expect(alternativeCalled).To(BeFalse())
		})

		It("Close with no OnNext calls closes primary", func() {
			inner := newCloseTrackerUnit(FromList([]int{1, 2}))
			cursor := OrElse[int](inner, func() RecordCursor[int] {
				return FromList([]int{99})
			})
			Expect(cursor.Close()).To(Succeed())
			Expect(inner.wasClosed()).To(BeTrue())
		})

		It("alternative also empty produces empty result", func() {
			cursor := OrElse(
				Empty[int](),
				func() RecordCursor[int] { return Empty[int]() },
			)
			r, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HasNext()).To(BeFalse())
			Expect(r.GetNoNextReason()).To(Equal(SourceExhausted))
		})
	})

	// ---- ConcatCursors ----

	Describe("ConcatCursors", func() {
		It("continuation serialization roundtrip", func() {
			items1 := []int{1, 2, 3}
			items2 := []int{4, 5, 6}
			makeFirst := func(c []byte) RecordCursor[int] {
				return FromListWithContinuation(items1, c)
			}
			makeSecond := func(c []byte) RecordCursor[int] {
				return FromListWithContinuation(items2, c)
			}

			// Read 2 items from first, grab continuation, resume.
			cursor := LimitRowsCursor(ConcatCursors(makeFirst, makeSecond, nil), 2)
			var lastCont RecordCursorContinuation
			for {
				r, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !r.HasNext() {
					lastCont = r.GetContinuation()
					break
				}
			}
			Expect(lastCont).NotTo(BeNil())
			Expect(lastCont.IsEnd()).To(BeFalse())

			contBytes, err := lastCont.ToBytes()
			Expect(err).NotTo(HaveOccurred())
			Expect(contBytes).NotTo(BeEmpty())

			// Resume from continuation.
			cursor2 := ConcatCursors(makeFirst, makeSecond, contBytes)
			result, err := AsList(ctx, cursor2)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{3, 4, 5, 6}))
		})

		It("continuation roundtrip crossing from first to second cursor", func() {
			items1 := []int{1, 2}
			items2 := []int{3, 4}
			makeFirst := func(c []byte) RecordCursor[int] {
				return FromListWithContinuation(items1, c)
			}
			makeSecond := func(c []byte) RecordCursor[int] {
				return FromListWithContinuation(items2, c)
			}

			// Read first cursor entirely (2 items) + 1 from second.
			cursor := LimitRowsCursor(ConcatCursors(makeFirst, makeSecond, nil), 3)
			items, cont, err := AsListWithContinuation(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(Equal([]int{1, 2, 3}))
			Expect(cont).NotTo(BeNil())

			// Resume: should get just [4].
			cursor2 := ConcatCursors(makeFirst, makeSecond, cont)
			result, err := AsList(ctx, cursor2)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{4}))
		})

		It("OnNext after Close returns SourceExhausted", func() {
			cursor := ConcatCursors(
				func(_ []byte) RecordCursor[int] { return FromList([]int{1}) },
				func(_ []byte) RecordCursor[int] { return FromList([]int{2}) },
				nil,
			)
			Expect(cursor.Close()).To(Succeed())

			r, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HasNext()).To(BeFalse())
			Expect(r.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("first cursor error propagates", func() {
			cursor := ConcatCursors(
				func(_ []byte) RecordCursor[int] {
					return &errorCursor[int]{err: fmt.Errorf("first cursor failed")}
				},
				func(_ []byte) RecordCursor[int] { return FromList([]int{1}) },
				nil,
			)
			_, err := cursor.OnNext(ctx)
			Expect(err).To(MatchError("first cursor failed"))
		})

		It("second cursor error propagates after first exhausted", func() {
			cursor := ConcatCursors(
				func(_ []byte) RecordCursor[int] { return FromList([]int{}) },
				func(_ []byte) RecordCursor[int] {
					return &errorCursor[int]{err: fmt.Errorf("second cursor failed")}
				},
				nil,
			)
			_, err := cursor.OnNext(ctx)
			Expect(err).To(MatchError("second cursor failed"))
		})

		It("first non-exhaustion stop propagates stop reason", func() {
			// First cursor returns 1 then ScanLimitReached. Concat should NOT switch
			// to second cursor -- it should propagate the scan limit.
			cursor := ConcatCursors(
				func(_ []byte) RecordCursor[int] {
					return newOOBStopCursorUnit([]int{1}, ScanLimitReached,
						&BytesContinuation{bytes: []byte{0x01}})
				},
				func(_ []byte) RecordCursor[int] { return FromList([]int{10, 20}) },
				nil,
			)
			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeTrue())
			Expect(r1.GetValue()).To(Equal(1))

			r2, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.HasNext()).To(BeFalse())
			Expect(r2.GetNoNextReason()).To(Equal(ScanLimitReached))
		})

		It("invalid continuation bytes fail with ContinuationParseError", func() {
			// Java: ConcatCursor's constructor throws
			// RecordCoreException("Error parsing ConcatCursor continuation");
			// a silent restart would re-emit rows the caller already consumed.
			cursor := ConcatCursors(
				func(_ []byte) RecordCursor[int] { return FromList([]int{1, 2}) },
				func(_ []byte) RecordCursor[int] { return FromList([]int{3}) },
				[]byte{0xFF, 0xFE, 0xFD}, // garbage, not valid proto
			)
			_, err := AsList(ctx, cursor)
			var parseErr *ContinuationParseError
			Expect(errors.As(err, &parseErr)).To(BeTrue(), "want *ContinuationParseError, got %T: %v", err, err)
			Expect(parseErr.RawBytes).To(Equal([]byte{0xFF, 0xFE, 0xFD}))
		})
	})

	// ---- MapCursor ----

	Describe("MapCursor", func() {
		It("inner error propagates through map", func() {
			inner := newErrAfterNCursor([]int{1}, fmt.Errorf("map inner err"))
			cursor := MapCursor(inner, func(v int) string {
				return fmt.Sprintf("v=%d", v)
			})

			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.GetValue()).To(Equal("v=1"))

			_, err = cursor.OnNext(ctx)
			Expect(err).To(MatchError("map inner err"))
		})

		It("maps single element", func() {
			inner := FromList([]int{42})
			cursor := MapCursor(inner, func(v int) int { return v * v })
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{1764}))
		})

		It("preserves no-next reason from inner", func() {
			inner := newOOBStopCursorUnit([]int{1}, ScanLimitReached,
				&BytesContinuation{bytes: []byte{0x01}})
			cursor := MapCursor(inner, func(v int) int { return v + 100 })

			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.GetValue()).To(Equal(101))

			r2, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.HasNext()).To(BeFalse())
			Expect(r2.GetNoNextReason()).To(Equal(ScanLimitReached))
		})
	})

	// ---- MapErrCursor ----

	Describe("MapErrCursor", func() {
		It("inner error takes precedence over transform error", func() {
			inner := &errorCursor[int]{err: fmt.Errorf("inner error")}
			cursor := MapErrCursor(inner, func(v int) (string, error) {
				return "", fmt.Errorf("should not reach")
			})
			_, err := cursor.OnNext(ctx)
			Expect(err).To(MatchError("inner error"))
		})

		It("preserves continuation from inner on no-next", func() {
			inner := newOOBStopCursorUnit([]int{}, ScanLimitReached,
				&BytesContinuation{bytes: []byte{0x42}})
			cursor := MapErrCursor(inner, func(v int) (int, error) {
				return v * 2, nil
			})
			r, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HasNext()).To(BeFalse())
			Expect(r.GetNoNextReason()).To(Equal(ScanLimitReached))
			Expect(r.GetContinuation().IsEnd()).To(BeFalse())
		})

		It("error on first element stops immediately", func() {
			inner := FromList([]int{1, 2, 3})
			cursor := MapErrCursor(inner, func(_ int) (int, error) {
				return 0, fmt.Errorf("first element error")
			})
			_, err := cursor.OnNext(ctx)
			Expect(err).To(MatchError("first element error"))
		})
	})

	// ---- FlatMapPipelined ----

	Describe("FlatMapPipelined", func() {
		It("check value mismatch restarts inner from beginning", func() {
			outerCallCount := 0
			makeOuter := func(cont []byte) RecordCursor[int] {
				outerCallCount++
				// Second call (resume) returns different value for same position.
				if outerCallCount == 2 {
					return FromListWithContinuation([]int{999}, cont)
				}
				return FromListWithContinuation([]int{1}, cont)
			}
			makeInner := func(outer int, cont []byte) RecordCursor[int] {
				if cont != nil {
					// If inner continuation is provided, we start from it.
					// If check value mismatched, cont will be nil (restart).
					return FromListWithContinuation([]int{outer*10 + 5, outer*10 + 6}, cont)
				}
				return FromList([]int{outer * 10, outer*10 + 1})
			}
			checkFunc := func(outer int) []byte {
				return []byte(fmt.Sprintf("id:%d", outer))
			}

			// First pass: read 1 item with check value, get continuation.
			cursor := LimitRowsCursor(
				FlatMapPipelinedWithCheck(makeOuter, makeInner, checkFunc, nil, 1),
				1,
			)
			r, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HasNext()).To(BeTrue())
			Expect(r.GetValue()).To(Equal(10))

			contBytes, err := r.GetContinuation().ToBytes()
			Expect(err).NotTo(HaveOccurred())

			// Resume: outer now returns 999 instead of 1. Check value mismatch!
			// The flatmap should restart inner from beginning (nil inner cont).
			cursor2 := FlatMapPipelinedWithCheck(makeOuter, makeInner, checkFunc, contBytes, 1)
			results, err := AsList(ctx, cursor2)
			Expect(err).NotTo(HaveOccurred())
			// Should get inner for 999 from beginning: [9990, 9991]
			Expect(results).To(Equal([]int{9990, 9991}))
		})

		It("OnNext after Close returns SourceExhausted", func() {
			cursor := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] { return FromList([]int{1}) },
				func(outer int, _ []byte) RecordCursor[int] { return FromList([]int{outer * 10}) },
				nil, 1,
			)
			Expect(cursor.Close()).To(Succeed())

			r, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HasNext()).To(BeFalse())
			Expect(r.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("outer error propagates", func() {
			cursor := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] {
					return &errorCursor[int]{err: fmt.Errorf("outer failed")}
				},
				func(outer int, _ []byte) RecordCursor[int] { return FromList([]int{outer}) },
				nil, 1,
			)
			_, err := cursor.OnNext(ctx)
			Expect(err).To(MatchError("outer failed"))
		})

		It("inner error propagates", func() {
			cursor := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] { return FromList([]int{1}) },
				func(_ int, _ []byte) RecordCursor[int] {
					return &errorCursor[int]{err: fmt.Errorf("inner failed")}
				},
				nil, 1,
			)
			_, err := cursor.OnNext(ctx)
			Expect(err).To(MatchError("inner failed"))
		})

		It("inner error after some values propagates", func() {
			cursor := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] { return FromList([]int{1}) },
				func(_ int, _ []byte) RecordCursor[int] {
					return newErrAfterNCursor([]int{10}, fmt.Errorf("inner mid-error"))
				},
				nil, 1,
			)
			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.GetValue()).To(Equal(10))

			_, err = cursor.OnNext(ctx)
			Expect(err).To(MatchError("inner mid-error"))
		})

		It("nested flat maps (flatmap of flatmap)", func() {
			// Outer: [1, 2]
			// Mid: for each outer x -> [x*10, x*10+1]
			// Inner: for each mid y -> [y*100]
			cursor := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] { return FromList([]int{1, 2}) },
				func(outer int, _ []byte) RecordCursor[int] {
					return FlatMapPipelined(
						func(_ []byte) RecordCursor[int] {
							return FromList([]int{outer * 10, outer*10 + 1})
						},
						func(mid int, _ []byte) RecordCursor[int] {
							return FromList([]int{mid * 100})
						},
						nil, 1,
					)
				},
				nil, 1,
			)
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{1000, 1100, 2000, 2100}))
		})

		It("inner OOB stop propagates stop reason", func() {
			cursor := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] { return FromList([]int{1}) },
				func(_ int, _ []byte) RecordCursor[int] {
					return newOOBStopCursorUnit([]int{10}, TimeLimitReached,
						&BytesContinuation{bytes: []byte{0x01}})
				},
				nil, 1,
			)
			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.GetValue()).To(Equal(10))

			r2, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.HasNext()).To(BeFalse())
			Expect(r2.GetNoNextReason()).To(Equal(TimeLimitReached))
		})

		It("outer OOB stop propagates", func() {
			cursor := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] {
					return newOOBStopCursorUnit([]int{}, ByteLimitReached,
						&BytesContinuation{bytes: []byte{0x01}})
				},
				func(outer int, _ []byte) RecordCursor[int] { return FromList([]int{outer * 10}) },
				nil, 1,
			)
			r, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HasNext()).To(BeFalse())
			Expect(r.GetNoNextReason()).To(Equal(ByteLimitReached))
		})

		It("invalid continuation fails with ContinuationParseError", func() {
			// Java: RecordCursor.flatMapPipelined throws
			// RecordCoreException("error parsing continuation"); a silent
			// restart would re-emit rows the caller already consumed.
			cursor := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] { return FromList([]int{1, 2}) },
				func(outer int, _ []byte) RecordCursor[int] {
					return FromList([]int{outer * 10})
				},
				[]byte{0xFF, 0xFE, 0xFD}, // garbage proto
				1,
			)
			_, err := AsList(ctx, cursor)
			var parseErr *ContinuationParseError
			Expect(errors.As(err, &parseErr)).To(BeTrue(), "want *ContinuationParseError, got %T: %v", err, err)
			Expect(parseErr.RawBytes).To(Equal([]byte{0xFF, 0xFE, 0xFD}))
		})

		It("single outer with many inner items", func() {
			cursor := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] { return FromList([]int{1}) },
				func(_ int, _ []byte) RecordCursor[int] {
					return FromList([]int{10, 20, 30, 40, 50})
				},
				nil, 1,
			)
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{10, 20, 30, 40, 50}))
		})
	})

	// ---- AutoContinuingCursor ----

	Describe("AutoContinuingCursor", func() {
		var metaData *RecordMetaData

		BeforeEach(func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			var buildErr error
			metaData, buildErr = builder.Build()
			Expect(buildErr).NotTo(HaveOccurred())
		})

		It("OnNext after Close returns SourceExhausted", func() {
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			runner := NewFDBDatabaseRunner(sharedDB)
			cursor := NewAutoContinuingCursor(
				runner,
				func(rtx *FDBRecordContext, continuation []byte) RecordCursor[*FDBStoredRecord[proto.Message]] {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())
					return store.ScanRecords(continuation, ForwardScan())
				},
				0,
			)
			Expect(cursor.Close()).To(Succeed())

			r, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HasNext()).To(BeFalse())
			Expect(r.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("auto-continues across byte limit boundaries", func() {
			ks := specSubspace()
			populate10Orders(ctx, metaData)

			runner := NewFDBDatabaseRunner(sharedDB)
			cursor := NewAutoContinuingCursor(
				runner,
				func(rtx *FDBRecordContext, continuation []byte) RecordCursor[*FDBStoredRecord[proto.Message]] {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())
					scan := ForwardScan()
					// Very small byte limit to force multiple continuations.
					scan.ExecuteProperties.ScannedBytesLimit = 100
					return store.ScanRecords(continuation, scan)
				},
				0,
			)

			records, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(records).To(HaveLen(10))

			for i, rec := range records {
				order := rec.Record.(*gen.Order)
				Expect(order.GetOrderId()).To(Equal(int64(i + 1)))
			}
		})

		It("produces correct results with reverse scan and scan limit", func() {
			ks := specSubspace()
			populate10Orders(ctx, metaData)

			runner := NewFDBDatabaseRunner(sharedDB)
			cursor := NewAutoContinuingCursor(
				runner,
				func(rtx *FDBRecordContext, continuation []byte) RecordCursor[*FDBStoredRecord[proto.Message]] {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())
					scan := ReverseScan()
					scan.ExecuteProperties.ScannedRecordsLimit = 2
					return store.ScanRecords(continuation, scan)
				},
				0,
			)

			records, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(records).To(HaveLen(10))

			// Reverse scan: order 10, 9, 8, ..., 1
			for i, rec := range records {
				order := rec.Record.(*gen.Order)
				Expect(order.GetOrderId()).To(Equal(int64(10 - i)))
			}
		})

		It("recovers from transaction_timed_out (1031) by creating new transaction", func() {
			ks := specSubspace()
			populate10Orders(ctx, metaData)

			var generatorCalls atomic.Int32
			runner := NewFDBDatabaseRunner(sharedDB)
			cursor := NewAutoContinuingCursor(
				runner,
				func(rtx *FDBRecordContext, continuation []byte) RecordCursor[*FDBStoredRecord[proto.Message]] {
					store, err := NewStoreBuilder().
						SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
					Expect(err).NotTo(HaveOccurred())
					scan := ForwardScan()
					inner := store.ScanRecords(continuation, scan)

					call := generatorCalls.Add(1)
					if call == 1 {
						// First transaction: inject transaction_timed_out after 3 records.
						return &faultAfterNCursor[*FDBStoredRecord[proto.Message]]{
							inner:    inner,
							afterN:   3,
							faultErr: fdb.Error{Code: 1031},
						}
					}
					return inner
				},
				3, // maxRetries
			)

			records, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(records).To(HaveLen(10))

			// Generator called twice: first tx (timed out after 3), second tx (completed).
			Expect(generatorCalls.Load()).To(Equal(int32(2)))

			for i, rec := range records {
				order := rec.Record.(*gen.Order)
				Expect(order.GetOrderId()).To(Equal(int64(i + 1)))
			}
		})
	})

	// ---- SkipThenLimit ----

	Describe("SkipThenLimit", func() {
		It("skip 0, limit 3 is just a limit", func() {
			cursor := SkipThenLimit(FromList([]int{1, 2, 3, 4, 5}), 0, 3)
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{1, 2, 3}))
		})

		It("skip all, limit anything returns empty", func() {
			cursor := SkipThenLimit(FromList([]int{1, 2, 3}), 10, 5)
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeEmpty())
		})

		It("skip and limit on empty returns empty", func() {
			cursor := SkipThenLimit(Empty[int](), 2, 3)
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(BeEmpty())
		})
	})

	// ---- Composition edge cases ----

	Describe("Composition", func() {
		It("MapCursor(SkipCursor(LimitRowsCursor(...)))", func() {
			// 10 items, limit 7, skip 2, map *3 -> [3*3, 4*3, 5*3, 6*3, 7*3] = [9, 12, 15, 18, 21]
			inner := FromList([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
			limited := LimitRowsCursor(inner, 7)
			skipped := SkipCursor(limited, 2)
			mapped := MapCursor(skipped, func(v int) int { return v * 3 })

			result, err := AsList(ctx, mapped)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{9, 12, 15, 18, 21}))
		})

		It("filterCursor wrapping MapCursor", func() {
			inner := FromList([]int{1, 2, 3, 4, 5, 6})
			mapped := MapCursor(inner, func(v int) int { return v * v })
			filtered := &filterCursor[int]{
				inner:     mapped,
				predicate: func(v int) bool { return v > 10 },
			}

			result, err := AsList(ctx, filtered)
			Expect(err).NotTo(HaveOccurred())
			// 1, 4, 9, 16, 25, 36 -> filter >10 -> [16, 25, 36]
			Expect(result).To(Equal([]int{16, 25, 36}))
		})

		It("ConcatCursors of FlatMapPipelined", func() {
			flatmap1 := func(_ []byte) RecordCursor[int] {
				return FlatMapPipelined(
					func(_ []byte) RecordCursor[int] { return FromList([]int{1}) },
					func(outer int, _ []byte) RecordCursor[int] { return FromList([]int{outer * 10}) },
					nil, 1,
				)
			}
			flatmap2 := func(_ []byte) RecordCursor[int] {
				return FlatMapPipelined(
					func(_ []byte) RecordCursor[int] { return FromList([]int{2}) },
					func(outer int, _ []byte) RecordCursor[int] { return FromList([]int{outer * 10}) },
					nil, 1,
				)
			}
			cursor := ConcatCursors(flatmap1, flatmap2, nil)
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{10, 20}))
		})

		It("OrElse with FlatMapPipelined primary", func() {
			// Primary: flatmap that produces values.
			primary := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] { return FromList([]int{1, 2}) },
				func(outer int, _ []byte) RecordCursor[int] { return FromList([]int{outer * 10}) },
				nil, 1,
			)
			cursor := OrElse(primary, func() RecordCursor[int] {
				return FromList([]int{999})
			})
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{10, 20}))
		})

		It("OrElse with empty FlatMapPipelined primary falls back", func() {
			// Primary: flatmap with empty outer -> no values.
			primary := FlatMapPipelined(
				func(_ []byte) RecordCursor[int] { return Empty[int]() },
				func(outer int, _ []byte) RecordCursor[int] { return FromList([]int{outer * 10}) },
				nil, 1,
			)
			cursor := OrElse(primary, func() RecordCursor[int] {
				return FromList([]int{999})
			})
			result, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal([]int{999}))
		})
	})
})

// callbackCursor is a test helper that delegates OnNext to a callback function.
type callbackCursor[T any] struct {
	fn func(context.Context) (RecordCursorResult[T], error)
}

func (c *callbackCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	return c.fn(ctx)
}

func (c *callbackCursor[T]) Close() error { return nil }

func (c *callbackCursor[T]) IsClosed() bool { return false }
