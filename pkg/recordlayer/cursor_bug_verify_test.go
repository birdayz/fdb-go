package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// limitedListCursor is a test helper that returns items from a list, then stops
// with a specified NoNextReason (e.g. ScanLimitReached) instead of SourceExhausted.
type limitedListCursor[T any] struct {
	items     []T
	pos       int
	stopAfter int          // stop after this many items
	reason    NoNextReason // reason to report when stopped
	closed    bool
}

func newLimitedListCursor[T any](items []T, stopAfter int, reason NoNextReason) RecordCursor[T] {
	return &limitedListCursor[T]{items: items, stopAfter: stopAfter, reason: reason}
}

func (c *limitedListCursor[T]) OnNext(_ context.Context) (RecordCursorResult[T], error) {
	if c.closed || c.pos >= c.stopAfter {
		return NewResultNoNext[T](c.reason, &BytesContinuation{bytes: []byte{byte(c.pos)}}), nil
	}
	if c.pos >= len(c.items) {
		return NewResultNoNext[T](SourceExhausted, &EndContinuation{}), nil
	}
	value := c.items[c.pos]
	c.pos++
	return NewResultWithValue(value, &BytesContinuation{bytes: []byte{byte(c.pos)}}), nil
}

func (c *limitedListCursor[T]) Close() error {
	c.closed = true
	return nil
}

func (c *limitedListCursor[T]) IsClosed() bool { return c.closed }

var _ = Describe("CursorBugVerify", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// Bug 1: UnionCursor must stop when a child hits an out-of-band limit.
	// Java: UnionCursorBase.computeNextResultStates() stops when ANY child
	// has !hasNext() && isLimitReached().
	Describe("UnionCursor stops on child OOB limit", func() {
		It("stops returning values when a child hits ScanLimitReached", func() {
			cursorA := FromList([]int{1, 3, 5})
			cursorB := newLimitedListCursor([]int{2}, 1, ScanLimitReached)
			union := Union([]RecordCursor[int]{cursorA, cursorB}, intCompKey, false)

			// First: A=1, B=2. Min=1.
			r1, err := union.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeTrue())
			Expect(r1.GetValue()).To(Equal(1))

			// Second: A=3, B=2. Min=2 (B). Consume B -> B hits limit.
			// Returns 2 (last safe value), defers stop.
			r2, err := union.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.HasNext()).To(BeTrue())
			Expect(r2.GetValue()).To(Equal(2))

			// Third: union stops because B hit ScanLimitReached.
			r3, err := union.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r3.HasNext()).To(BeFalse())
			Expect(r3.GetNoNextReason()).To(Equal(ScanLimitReached))
		})

		It("stops immediately when child starts with OOB limit", func() {
			cursorA := FromList([]int{1, 2, 3})
			cursorB := newLimitedListCursor([]int{}, 0, ScanLimitReached)
			union := Union([]RecordCursor[int]{cursorA, cursorB}, intCompKey, false)

			r1, err := union.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeFalse())
			Expect(r1.GetNoNextReason()).To(Equal(ScanLimitReached))
		})
	})

	// Bug 2: LimitRowsCursor must preserve inner continuation for resumability.
	// Java: RowLimitedCursor returns nextResult.getContinuation().
	Describe("LimitRowsCursor preserves continuation", func() {
		It("returns inner continuation when row limit reached", func() {
			inner := FromList([]int{10, 20, 30, 40, 50})
			limited := LimitRowsCursor(inner, 2)

			r1, err := limited.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeTrue())
			Expect(r1.GetValue()).To(Equal(10))

			r2, err := limited.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.HasNext()).To(BeTrue())
			Expect(r2.GetValue()).To(Equal(20))

			// Limit reached — must have a non-nil, non-end continuation
			r3, err := limited.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r3.HasNext()).To(BeFalse())
			Expect(r3.GetNoNextReason()).To(Equal(ReturnLimitReached))

			cont := r3.GetContinuation()
			Expect(cont).NotTo(BeNil())
			Expect(cont.IsEnd()).To(BeFalse(), "continuation must be resumable, not EndContinuation")
			contBytes, contBytesErr := cont.ToBytes()
			Expect(contBytesErr).NotTo(HaveOccurred())
			Expect(contBytes).NotTo(BeNil(), "continuation bytes must not be nil")
		})
	})

	// Bug 3: OrElseCursor must stay undecided when primary hits OOB limit.
	// Java: UNDECIDED state + isOutOfBand() -> return as-is, don't switch.
	Describe("OrElseCursor stays undecided on OOB", func() {
		It("does not switch to alternative when primary hits ScanLimitReached", func() {
			primary := newLimitedListCursor([]int{100, 200}, 0, ScanLimitReached)

			alternativeCalled := false
			alternative := func() RecordCursor[int] {
				alternativeCalled = true
				return FromList([]int{999})
			}

			cursor := OrElse(primary, alternative)

			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(alternativeCalled).To(BeFalse(), "must not call alternative on OOB limit")
			Expect(r1.HasNext()).To(BeFalse())
			Expect(r1.GetNoNextReason()).To(Equal(ScanLimitReached))
		})

		It("switches to alternative on SourceExhausted", func() {
			primary := FromList([]int{}) // empty = SourceExhausted
			cursor := OrElse(primary, func() RecordCursor[int] {
				return FromList([]int{42})
			})

			r1, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeTrue())
			Expect(r1.GetValue()).To(Equal(42))
		})
	})

	// Bug 4: IntersectionCursor.weakestNoNextReason must return correct reason.
	// Was always returning SourceExhausted because initial value (strength 0)
	// was never replaced by anything weaker.
	Describe("IntersectionCursor weakestNoNextReason", func() {
		It("returns ScanLimitReached when only stopped child has that reason", func() {
			cursorA := FromList([]int{1, 2, 3})
			cursorB := newLimitedListCursor([]int{1, 2, 3}, 0, ScanLimitReached)
			inter := Intersection([]RecordCursor[int]{cursorA, cursorB}, intCompKey, false)

			r1, err := inter.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r1.HasNext()).To(BeFalse())
			Expect(r1.GetNoNextReason()).To(Equal(ScanLimitReached))
		})

		It("returns ReturnLimitReached as weakest when mixed with ScanLimitReached", func() {
			cursorA := newLimitedListCursor([]int{}, 0, ReturnLimitReached)
			cursorB := newLimitedListCursor([]int{}, 0, ScanLimitReached)
			inter := Intersection([]RecordCursor[int]{cursorA, cursorB}, intCompKey, false)

			r, err := inter.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HasNext()).To(BeFalse())
			Expect(r.GetNoNextReason()).To(Equal(ReturnLimitReached))
		})

		It("returns SourceExhausted when a child is truly exhausted", func() {
			cursorA := FromList([]int{}) // exhausted
			cursorB := newLimitedListCursor([]int{}, 0, ScanLimitReached)
			inter := Intersection([]RecordCursor[int]{cursorA, cursorB}, intCompKey, false)

			r, err := inter.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.HasNext()).To(BeFalse())
			Expect(r.GetNoNextReason()).To(Equal(SourceExhausted))
		})
	})
})
