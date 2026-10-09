package recordlayer

import (
	"context"
	"errors"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// failContCursor returns values, then a no-next result with a continuation
// whose ToBytes() returns an error. Used to test error handling in
// AsListWithContinuation.
type failContCursor[T any] struct {
	values []T
	pos    int
	closed bool
}

func (c *failContCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	if c.closed || c.pos >= len(c.values) {
		return NewResultNoNext[T](ReturnLimitReached, &failContinuation{}), nil
	}
	v := c.values[c.pos]
	c.pos++
	return NewResultWithValue(v, &StartContinuation{}), nil
}

func (c *failContCursor[T]) Close() error {
	c.closed = true
	return nil
}

func (c *failContCursor[T]) IsClosed() bool { return c.closed }

// failContinuation is a non-end continuation whose ToBytes always errors.
type failContinuation struct{}

func (f *failContinuation) ToBytes() ([]byte, error) {
	return nil, errors.New("cont boom")
}

func (f *failContinuation) IsEnd() bool {
	return false
}

var _ = Describe("Coverage Unit Tests", func() {
	// long_arithmetic_function.go: uncovered error paths.
	Describe("arithmetic edge cases", func() {
		It("bitnot with zero args errors", func() {
			fn := globalFunctionRegistry["bitnot"].Evaluator
			Expect(fn).NotTo(BeNil())
			_, err := fn(nil, nil, [][]any{{}})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("requires exactly 1 argument"))
		})

		It("bitnot with a non-Number argument is Java's InvalidResultException", func() {
			fn := globalFunctionRegistry["bitnot"].Evaluator
			_, err := fn(nil, nil, [][]any{{"not_an_int"}})
			var ire *KeyExpressionInvalidResultError
			Expect(errors.As(err, &ire)).To(BeTrue(), "got %v", err)
			Expect(ire.ExpectedType).To(Equal("java.lang.Number"))
		})

		It("bitmap_bit_position with zero divisor errors", func() {
			fn := globalFunctionRegistry["bitmap_bit_position"].Evaluator
			_, err := fn(nil, nil, [][]any{{int64(10), int64(0)}})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("division by zero"))
		})

		It("bitmap_bit_position with overflow errors", func() {
			// floorDiv(MinInt64, -1) = MinInt64/-1 which overflows in multiply step.
			// floorDiv(-MinInt64, -1) actually: floorDiv computes a/b = MinInt64/-1.
			// In Go, MinInt64 / -1 = MinInt64 (wraps). Then multiply: MinInt64 * -1 overflows.
			fn := globalFunctionRegistry["bitmap_bit_position"].Evaluator
			_, err := fn(nil, nil, [][]any{{int64(math.MinInt64), int64(-1)}})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("overflow"))
		})

		It("bitmap_bucket_offset with zero divisor errors", func() {
			fn := globalFunctionRegistry["bitmap_bucket_offset"].Evaluator
			_, err := fn(nil, nil, [][]any{{int64(10), int64(0)}})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("division by zero"))
		})

		It("bitmap_bucket_offset with overflow errors", func() {
			fn := globalFunctionRegistry["bitmap_bucket_offset"].Evaluator
			_, err := fn(nil, nil, [][]any{{int64(math.MinInt64), int64(-1)}})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("overflow"))
		})
	})

	// cursor_util.go: Filter2 and Limit.
	Describe("Filter2", func() {
		It("propagates errors and filters values", func() {
			testErr := errors.New("boom")
			calls := 0
			seq := func(yield func(int, error) bool) {
				if !yield(1, nil) {
					return
				}
				if !yield(0, testErr) {
					return
				}
				if !yield(2, nil) {
					return
				}
				if !yield(3, nil) {
					return
				}
			}

			filtered := Filter2(seq, func(v int) bool { return v >= 2 })
			var values []int
			var errs []error
			for v, err := range filtered {
				calls++
				if err != nil {
					errs = append(errs, err)
					continue
				}
				values = append(values, v)
			}
			// value 1 filtered out, error propagated, value 2 and 3 pass
			Expect(values).To(Equal([]int{2, 3}))
			Expect(errs).To(HaveLen(1))
			Expect(errs[0]).To(MatchError("boom"))
		})

		It("stops when yield returns false on error", func() {
			testErr := errors.New("stop")
			seq := func(yield func(int, error) bool) {
				if !yield(0, testErr) {
					return
				}
				yield(99, nil) // should not be reached
			}

			filtered := Filter2(seq, func(v int) bool { return true })
			count := 0
			for range filtered {
				count++
				break // stop after first
			}
			Expect(count).To(Equal(1))
		})

		It("stops when yield returns false on value", func() {
			seq := func(yield func(int, error) bool) {
				if !yield(1, nil) {
					return
				}
				if !yield(2, nil) {
					return
				}
				yield(3, nil)
			}

			filtered := Filter2(seq, func(v int) bool { return true })
			var values []int
			for v, err := range filtered {
				Expect(err).NotTo(HaveOccurred())
				values = append(values, v)
				if len(values) == 2 {
					break
				}
			}
			Expect(values).To(Equal([]int{1, 2}))
		})
	})

	Describe("Limit", func() {
		It("limits output to n values", func() {
			seq := func(yield func(int) bool) {
				for i := 0; i < 100; i++ {
					if !yield(i) {
						return
					}
				}
			}

			limited := Limit(seq, 3)
			var values []int
			for v := range limited {
				values = append(values, v)
			}
			Expect(values).To(Equal([]int{0, 1, 2}))
		})

		It("stops early when yield returns false", func() {
			seq := func(yield func(int) bool) {
				for i := 0; i < 100; i++ {
					if !yield(i) {
						return
					}
				}
			}

			limited := Limit(seq, 10)
			var values []int
			for v := range limited {
				values = append(values, v)
				if len(values) == 2 {
					break
				}
			}
			Expect(values).To(Equal([]int{0, 1}))
		})

		It("handles zero limit", func() {
			seq := func(yield func(int) bool) {
				yield(1)
			}
			limited := Limit(seq, 0)
			count := 0
			for range limited {
				count++
			}
			Expect(count).To(Equal(0))
		})
	})

	// dedup_cursor.go: various paths.
	Describe("dedupCursor", func() {
		It("returns end continuation when closed", func() {
			cursor := Dedup[int](
				func(cont []byte) RecordCursor[int] { return FromList([]int{1, 2, 3}) },
				func(a, b int) bool { return a == b },
				nil, nil, nil,
			)
			Expect(cursor.Close()).To(Succeed())

			result, err := cursor.OnNext(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("propagates inner cursor errors", func() {
			testErr := errors.New("inner error")
			errCursor := &errorCursor[int]{err: testErr}
			cursor := Dedup[int](
				func(cont []byte) RecordCursor[int] { return errCursor },
				func(a, b int) bool { return a == b },
				nil, nil, nil,
			)

			_, err := cursor.OnNext(context.Background())
			Expect(err).To(MatchError("inner error"))
		})

		It("deduplicates adjacent equal values with continuation", func() {
			cursor := Dedup[int](
				func(cont []byte) RecordCursor[int] {
					return FromList([]int{1, 1, 2, 2, 3})
				},
				func(a, b int) bool { return a == b },
				func(v int) []byte { return []byte{byte(v)} },
				func(b []byte) (int, bool) { return int(b[0]), true },
				nil,
			)
			defer cursor.Close()

			ctx := context.Background()
			var values []int
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				values = append(values, result.GetValue())
			}
			Expect(values).To(Equal([]int{1, 2, 3}))
		})

		It("wraps continuation with inner bytes and last value", func() {
			// Use a list cursor with continuation support. The dedup cursor
			// should produce continuation bytes containing the inner continuation
			// and the packed last value.
			cursor := Dedup[int](
				func(cont []byte) RecordCursor[int] {
					return FromListWithContinuation([]int{10, 20, 30}, cont)
				},
				func(a, b int) bool { return a == b },
				func(v int) []byte { return []byte{byte(v)} },
				func(b []byte) (int, bool) { return int(b[0]), true },
				nil,
			)
			defer cursor.Close()

			result, err := cursor.OnNext(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeTrue())
			Expect(result.GetValue()).To(Equal(10))

			// The continuation should not be an end continuation.
			cont := result.GetContinuation()
			Expect(cont).NotTo(BeNil())
			Expect(cont.IsEnd()).To(BeFalse())
			contBytes, contErr := cont.ToBytes()
			Expect(contErr).NotTo(HaveOccurred())
			Expect(contBytes).NotTo(BeEmpty())
		})

		It("Close with nil inner returns nil", func() {
			// Create a dedup cursor and force inner to nil by directly
			// constructing the struct.
			c := &dedupCursor[int]{
				equal: func(a, b int) bool { return a == b },
			}
			Expect(c.Close()).To(Succeed())
		})
	})

	// cursor.go: IsClosed tracks closure state.
	Describe("IsClosed", func() {
		It("returns false before Close and true after on list cursor", func() {
			cursor := FromList([]int{1, 2, 3})
			Expect(cursor.IsClosed()).To(BeFalse())

			Expect(cursor.Close()).To(Succeed())
			Expect(cursor.IsClosed()).To(BeTrue())
		})

		It("works on filterCursor wrapping list cursor", func() {
			inner := FromList([]int{1, 2, 3, 4, 5})
			filtered := &filterCursor[int]{inner: inner, predicate: func(v int) bool { return v%2 == 0 }}
			Expect(filtered.IsClosed()).To(BeFalse())

			Expect(filtered.Close()).To(Succeed())
			Expect(filtered.IsClosed()).To(BeTrue())
		})

		It("works on concat cursors", func() {
			c := ConcatCursors[int](
				func(_ []byte) RecordCursor[int] { return FromList([]int{1}) },
				func(_ []byte) RecordCursor[int] { return FromList([]int{2}) },
				nil,
			)
			Expect(c.IsClosed()).To(BeFalse())

			Expect(c.Close()).To(Succeed())
			Expect(c.IsClosed()).To(BeTrue())
		})

		It("empty cursor reports closed after Close (Java EmptyCursor)", func() {
			c := Empty[int]()
			Expect(c.IsClosed()).To(BeFalse())
			// No resources to release, but Java's EmptyCursor tracks the
			// closed flag and isClosed() reports it — the contract is
			// observable, so Go matches.
			Expect(c.Close()).To(Succeed())
			Expect(c.IsClosed()).To(BeTrue())
		})
	})

	// cursor_util.go: AsListWithContinuation error paths.
	Describe("AsListWithContinuation", func() {
		It("returns continuation error when ToBytes fails", func() {
			// Create a cursor whose continuation returns an error from ToBytes.
			cursor := &failContCursor[int]{
				values: []int{42},
			}
			_, _, err := AsListWithContinuation(context.Background(), cursor)
			Expect(err).To(MatchError(ContainSubstring("cont boom")))
		})
	})

	// cursor.go: saturatingAdd overflow.
	Describe("saturatingAdd", func() {
		It("returns MaxInt on overflow", func() {
			result := saturatingAdd(math.MaxInt, 1)
			Expect(result).To(Equal(math.MaxInt))
		})

		It("returns MaxInt when both are large", func() {
			result := saturatingAdd(math.MaxInt-5, 10)
			Expect(result).To(Equal(math.MaxInt))
		})

		It("returns normal sum when no overflow", func() {
			result := saturatingAdd(10, 20)
			Expect(result).To(Equal(30))
		})

		It("handles zero b", func() {
			result := saturatingAdd(42, 0)
			Expect(result).To(Equal(42))
		})
	})

	// index.go: SetClearWhenZero(false) and GetBooleanOption.
	Describe("Index options", func() {
		It("SetClearWhenZero false removes the option", func() {
			idx := NewIndex("test", Field("price"))
			idx.SetClearWhenZero(true)
			Expect(idx.Options[IndexOptionClearWhenZero]).To(Equal("true"))
			Expect(idx.IsClearWhenZero()).To(BeTrue())

			idx.SetClearWhenZero(false)
			_, exists := idx.Options[IndexOptionClearWhenZero]
			Expect(exists).To(BeFalse())
			Expect(idx.IsClearWhenZero()).To(BeFalse())
		})

		It("GetBooleanOption returns default when not set", func() {
			idx := NewIndex("test", Field("price"))
			Expect(idx.GetBooleanOption("nonexistent", true)).To(BeTrue())
			Expect(idx.GetBooleanOption("nonexistent", false)).To(BeFalse())
		})

		It("GetBooleanOption returns parsed value when set", func() {
			idx := NewIndex("test", Field("price"))
			idx.Options["myOpt"] = "true"
			Expect(idx.GetBooleanOption("myOpt", false)).To(BeTrue())

			idx.Options["myOpt"] = "false"
			Expect(idx.GetBooleanOption("myOpt", true)).To(BeFalse())

			idx.Options["myOpt"] = "notabool"
			Expect(idx.GetBooleanOption("myOpt", true)).To(BeFalse())
		})
	})

	// fallback_cursor.go: closed cursor and Close with nil inner.
	Describe("fallbackCursor", func() {
		It("returns end continuation when closed", func() {
			inner := FromList([]int{1, 2, 3})
			cursor := Fallback[int](inner, func(last *RecordCursorResult[int]) RecordCursor[int] {
				return FromList([]int{4, 5, 6})
			})
			Expect(cursor.Close()).To(Succeed())

			result, err := cursor.OnNext(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("Close with nil inner returns nil", func() {
			c := &fallbackCursor[int]{}
			Expect(c.Close()).To(Succeed())
		})
	})
})
