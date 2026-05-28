package recordlayer

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// infiniteCursor returns sequential integers forever. Tracks how many
// OnNext calls it received so tests can assert prompt cancellation.
type infiniteCursor struct {
	calls atomic.Int64
}

func (c *infiniteCursor) OnNext(_ context.Context) (RecordCursorResult[int], error) {
	n := c.calls.Add(1)
	return NewResultWithValue(int(n), &StartContinuation{}), nil
}
func (c *infiniteCursor) Close() error   { return nil }
func (c *infiniteCursor) IsClosed() bool { return false }

// constantCursor always returns the same value. Used to test dedup loops.
type constantCursor struct {
	val   int
	calls atomic.Int64
}

func (c *constantCursor) OnNext(_ context.Context) (RecordCursorResult[int], error) {
	c.calls.Add(1)
	return NewResultWithValue(c.val, &StartContinuation{}), nil
}
func (c *constantCursor) Close() error   { return nil }
func (c *constantCursor) IsClosed() bool { return false }

var _ = Describe("Cursor context cancellation", func() {
	// ---------------------------------------------------------------
	// Helper: pre-cancelled context
	// ---------------------------------------------------------------
	cancelledCtx := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}

	// Helper: context that expires in the future (for DeadlineExceeded)
	deadlineCtx := func(d time.Duration) (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), d)
	}

	// ---------------------------------------------------------------
	// filterCursor
	// ---------------------------------------------------------------
	Describe("filterCursor", func() {
		It("stops immediately on pre-cancelled context (reject-all predicate)", func() {
			inner := &infiniteCursor{}
			cursor := &filterCursor[int]{inner: inner, predicate: func(int) bool { return false }}

			_, err := cursor.OnNext(cancelledCtx())
			Expect(err).To(Equal(context.Canceled))
			Expect(inner.calls.Load()).To(BeNumerically("<=", 1))
		})

		It("stops immediately on pre-cancelled context (accept-all predicate)", func() {
			inner := &infiniteCursor{}
			cursor := &filterCursor[int]{inner: inner, predicate: func(int) bool { return true }}

			_, err := cursor.OnNext(cancelledCtx())
			Expect(err).To(Equal(context.Canceled))
			Expect(inner.calls.Load()).To(BeNumerically("==", 0))
		})

		It("returns context.DeadlineExceeded on expired deadline", func() {
			inner := &infiniteCursor{}
			cursor := &filterCursor[int]{inner: inner, predicate: func(int) bool { return false }}

			ctx, cancel := deadlineCtx(1 * time.Nanosecond)
			defer cancel()
			time.Sleep(1 * time.Millisecond) // let deadline expire

			_, err := cursor.OnNext(ctx)
			Expect(err).To(Equal(context.DeadlineExceeded))
		})

		It("works normally with non-cancelled context", func() {
			inner := FromList([]int{1, 2, 3, 4, 5})
			cursor := &filterCursor[int]{inner: inner, predicate: func(v int) bool { return v%2 == 0 }}

			ctx := context.Background()
			var results []int
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				results = append(results, result.GetValue())
			}
			Expect(results).To(Equal([]int{2, 4}))
		})
	})

	// ---------------------------------------------------------------
	// skipCursor
	// ---------------------------------------------------------------
	Describe("skipCursor", func() {
		It("stops on pre-cancelled context with large skip count", func() {
			inner := &infiniteCursor{}
			cursor := &skipCursor[int]{inner: inner, remaining: 1_000_000}

			_, err := cursor.OnNext(cancelledCtx())
			Expect(err).To(Equal(context.Canceled))
			Expect(inner.calls.Load()).To(BeNumerically("<=", 1))
		})

		It("works normally with non-cancelled context", func() {
			inner := FromList([]int{1, 2, 3, 4, 5})
			cursor := &skipCursor[int]{inner: inner, remaining: 3}

			result, err := cursor.OnNext(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeTrue())
			Expect(result.GetValue()).To(Equal(4))
		})
	})

	// ---------------------------------------------------------------
	// dedupCursor
	// ---------------------------------------------------------------
	Describe("dedupCursor", func() {
		It("stops on pre-cancelled context when all values are duplicates", func() {
			inner := &constantCursor{val: 42}
			cursor := Dedup[int](
				func(_ []byte) RecordCursor[int] { return inner },
				func(a, b int) bool { return a == b },
				func(v int) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, uint64(v)); return b },
				func(b []byte) (int, bool) { return int(binary.LittleEndian.Uint64(b)), true },
				nil,
			)

			// First call returns the first unique value (non-cancelled).
			result, err := cursor.OnNext(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeTrue())
			Expect(result.GetValue()).To(Equal(42))

			// Second call: all subsequent values are duplicates → loops.
			// With cancelled ctx, should stop immediately.
			_, err = cursor.OnNext(cancelledCtx())
			Expect(err).To(Equal(context.Canceled))
			Expect(inner.calls.Load()).To(BeNumerically("<=", 3))
		})
	})

	// ---------------------------------------------------------------
	// flatMapCursor (recordlayer)
	// ---------------------------------------------------------------
	Describe("flatMapCursor (recordlayer)", func() {
		It("stops on pre-cancelled context", func() {
			outer := &infiniteCursor{}
			cursor := FlatMapPipelined[int, int](
				func(_ []byte) RecordCursor[int] { return outer },
				func(outerVal int, continuation []byte) RecordCursor[int] {
					return FromList([]int{outerVal * 10})
				},
				nil, // continuation
				1,   // pipelineSize
			)

			_, err := cursor.OnNext(cancelledCtx())
			Expect(err).To(Equal(context.Canceled))
		})
	})

	// ---------------------------------------------------------------
	// ForEach utility
	// ---------------------------------------------------------------
	Describe("ForEach", func() {
		It("stops on pre-cancelled context", func() {
			inner := &infiniteCursor{}
			err := ForEach[int](cancelledCtx(), inner, func(int) error { return nil })
			Expect(err).To(Equal(context.Canceled))
			Expect(inner.calls.Load()).To(BeNumerically("<=", 1))
		})

		It("works normally with non-cancelled context", func() {
			inner := FromList([]int{1, 2, 3})
			var sum int
			err := ForEach[int](context.Background(), inner, func(v int) error { sum += v; return nil })
			Expect(err).NotTo(HaveOccurred())
			Expect(sum).To(Equal(6))
		})
	})

	// ---------------------------------------------------------------
	// GetCount utility
	// ---------------------------------------------------------------
	Describe("GetCount", func() {
		It("stops on pre-cancelled context", func() {
			inner := &infiniteCursor{}
			_, err := GetCount[int](cancelledCtx(), inner)
			Expect(err).To(Equal(context.Canceled))
			Expect(inner.calls.Load()).To(BeNumerically("<=", 1))
		})

		It("works normally with non-cancelled context", func() {
			inner := FromList([]int{1, 2, 3})
			count, err := GetCount[int](context.Background(), inner)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(3))
		})
	})

	// ---------------------------------------------------------------
	// Reduce utility
	// ---------------------------------------------------------------
	Describe("Reduce", func() {
		It("stops on pre-cancelled context", func() {
			inner := &infiniteCursor{}
			_, err := Reduce[int, int](cancelledCtx(), inner, 0, func(acc, v int) int { return acc + v })
			Expect(err).To(Equal(context.Canceled))
			Expect(inner.calls.Load()).To(BeNumerically("<=", 1))
		})

		It("works normally with non-cancelled context", func() {
			inner := FromList([]int{1, 2, 3})
			sum, err := Reduce[int, int](context.Background(), inner, 0, func(acc, v int) int { return acc + v })
			Expect(err).NotTo(HaveOccurred())
			Expect(sum).To(Equal(6))
		})
	})

	// ---------------------------------------------------------------
	// AsListWithContinuation utility
	// ---------------------------------------------------------------
	Describe("AsListWithContinuation", func() {
		It("stops on pre-cancelled context", func() {
			inner := &infiniteCursor{}
			_, _, err := AsListWithContinuation[int](cancelledCtx(), inner)
			Expect(err).To(Equal(context.Canceled))
			Expect(inner.calls.Load()).To(BeNumerically("<=", 1))
		})
	})

	// ---------------------------------------------------------------
	// Seq iterator
	// ---------------------------------------------------------------
	Describe("Seq", func() {
		It("yields nothing on pre-cancelled context", func() {
			inner := &infiniteCursor{}
			var count int
			for range Seq[int](inner, cancelledCtx()) {
				count++
			}
			Expect(count).To(Equal(0))
			Expect(inner.calls.Load()).To(BeNumerically("<=", 1))
		})

		It("works normally with non-cancelled context", func() {
			inner := FromList([]int{10, 20, 30})
			var results []int
			for v := range Seq[int](inner, context.Background()) {
				results = append(results, v)
			}
			Expect(results).To(Equal([]int{10, 20, 30}))
		})
	})

	// ---------------------------------------------------------------
	// Seq2 iterator
	// ---------------------------------------------------------------
	Describe("Seq2", func() {
		It("yields cancellation error on pre-cancelled context", func() {
			inner := &infiniteCursor{}
			var gotErr error
			for _, err := range Seq2[int](inner, cancelledCtx()) {
				gotErr = err
				break
			}
			Expect(gotErr).To(Equal(context.Canceled))
		})

		It("works normally with non-cancelled context", func() {
			inner := FromList([]int{10, 20})
			var results []int
			for v, err := range Seq2[int](inner, context.Background()) {
				Expect(err).NotTo(HaveOccurred())
				results = append(results, v)
			}
			Expect(results).To(Equal([]int{10, 20}))
		})
	})

	// ---------------------------------------------------------------
	// SeqWithContinuation iterator
	// ---------------------------------------------------------------
	Describe("SeqWithContinuation", func() {
		It("yields nothing on pre-cancelled context", func() {
			inner := &infiniteCursor{}
			var count int
			for range SeqWithContinuation[int](inner, cancelledCtx()) {
				count++
			}
			Expect(count).To(Equal(0))
		})
	})

	// ---------------------------------------------------------------
	// intersectionCursor (via NewIntersection)
	// ---------------------------------------------------------------
	Describe("intersectionCursor", func() {
		It("stops on pre-cancelled context during convergence", func() {
			// Two infinite cursors with a shared monotonic key counter.
			// Each child gets a distinct key on every advance, so the
			// intersection convergence loop never finds a match and
			// loops forever — unless ctx cancellation stops it.
			cursor1 := &infiniteCursor{}
			cursor2 := &infiniteCursor{}

			keyIdx := 0
			cursor := Intersection[int](
				[]RecordCursor[int]{cursor1, cursor2},
				func(v int) tuple.Tuple {
					keyIdx++
					return tuple.Tuple{keyIdx}
				},
				false,
			)

			_, err := cursor.OnNext(cancelledCtx())
			Expect(err).To(Equal(context.Canceled))
		})
	})

	// ---------------------------------------------------------------
	// autoContinuingCursor — CRITICAL: creates new FDB txns on dead ctx
	// ---------------------------------------------------------------
	Describe("autoContinuingCursor", func() {
		It("stops on pre-cancelled context without opening any transaction", func() {
			// With a pre-cancelled context, the ctx.Err() check at the top of the
			// loop fires immediately. The runner/generator are never called —
			// no FDB transaction is opened on a dead request.
			var generatorCalls atomic.Int64
			cursor := NewAutoContinuingCursor[int](
				nil, // runner is never called
				func(_ *FDBRecordContext, _ []byte) RecordCursor[int] {
					generatorCalls.Add(1)
					return &infiniteCursor{}
				},
				0,
			)

			_, err := cursor.OnNext(cancelledCtx())
			Expect(err).To(Equal(context.Canceled))
			Expect(generatorCalls.Load()).To(Equal(int64(0)))
		})
	})

	// ---------------------------------------------------------------
	// Edge case: context cancelled between calls (not pre-cancelled)
	// ---------------------------------------------------------------
	Describe("mid-iteration cancellation", func() {
		It("filterCursor stops when context cancelled after some successful calls", func() {
			ctx, cancel := context.WithCancel(context.Background())

			// Accept every 3rd item — forces the filter to loop.
			callCount := 0
			inner := FromList([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
			cursor := &filterCursor[int]{inner: inner, predicate: func(v int) bool {
				callCount++
				if callCount == 3 {
					cancel() // cancel after 3rd predicate evaluation
				}
				return v%3 == 0
			}}

			// First call: scans 1,2,3 — predicate fires 3 times, accepts 3, cancels ctx.
			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.GetValue()).To(Equal(3))

			// Second call: ctx is now cancelled, should error immediately.
			_, err = cursor.OnNext(ctx)
			Expect(err).To(Equal(context.Canceled))
		})
	})
})
