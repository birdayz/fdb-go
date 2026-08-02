package recordlayer

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Stopped is the cursor-level spelling of a resumable page boundary: an
// operator that had to drain its input eagerly to decide what to flow, and
// whose input halted out-of-band before it could decide, hands its parent this
// instead of an error. What makes that safe is entirely the RESULT it reports —
// the reason must survive so IsOutOfBand() still classifies it, and the
// continuation must be non-end so the parent treats it as a live resume point
// (RecordCursor.java:212-215) rather than as exhaustion.
var _ = Describe("Stopped", func() {
	ctx := context.Background()

	It("reports the out-of-band reason it was given", func() {
		c := Stopped[int](TimeLimitReached, NewBytesContinuation([]byte{0x03}))
		result, err := c.OnNext(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.HasNext()).To(BeFalse())
		Expect(result.GetNoNextReason()).To(Equal(TimeLimitReached))
		Expect(result.GetNoNextReason().IsOutOfBand()).To(BeTrue())
	})

	It("reports a resumable, non-end continuation", func() {
		c := Stopped[int](TimeLimitReached, NewBytesContinuation([]byte{0x03}))
		result, _ := c.OnNext(ctx)
		Expect(result.GetContinuation().IsEnd()).To(BeFalse())
		Expect(result.HasStoppedBeforeEnd()).To(BeTrue())
	})

	It("keeps reporting the same stop on re-call", func() {
		c := Stopped[int](ScanLimitReached, NewBytesContinuation([]byte{0x03}))
		first, _ := c.OnNext(ctx)
		second, _ := c.OnNext(ctx)
		Expect(second.GetNoNextReason()).To(Equal(first.GetNoNextReason()))
		Expect(second.HasStoppedBeforeEnd()).To(BeTrue())
	})

	It("still errors an eager drain, which cannot paginate", func() {
		// The propagation must not become a silent truncation for consumers
		// that have no resume point: errIfDrainTruncated is what keeps a
		// value-only drain loud.
		_, err := AsList[int](ctx, Stopped[int](TimeLimitReached, NewBytesContinuation([]byte{0x03})))
		Expect(err).To(HaveOccurred())
		var sle *ScanLimitReachedError
		Expect(errors.As(err, &sle)).To(BeTrue())
		Expect(sle.Reason).To(Equal(TimeLimitReached))
	})

	It("closes like any other cursor", func() {
		c := Stopped[int](ByteLimitReached, NewBytesContinuation([]byte{0x03}))
		Expect(c.IsClosed()).To(BeFalse())
		Expect(c.Close()).To(Succeed())
		Expect(c.IsClosed()).To(BeTrue())
	})

	// The contract in the doc comment is only worth stating if a violation is
	// rejected, and rejected AT THE BUILDER. Deferring to the first pull would
	// blame whichever parent happened to pull the cursor, so these assert the
	// panic escapes Stopped itself, with no OnNext in between.
	It("refuses a stop that no parent could resume from", func() {
		Expect(func() {
			Stopped[int](TimeLimitReached, &EndContinuation{})
		}).To(PanicWith(ContainSubstring("end continuation")))
	})

	It("refuses SourceExhausted, which is exhaustion and not a stop", func() {
		Expect(func() {
			Stopped[int](SourceExhausted, NewBytesContinuation([]byte{0x03}))
		}).To(PanicWith(ContainSubstring("SourceExhausted requires an end continuation")))
	})

	It("refuses a nil continuation", func() {
		Expect(func() {
			Stopped[int](TimeLimitReached, nil)
		}).To(PanicWith(ContainSubstring("resumable continuation")))
	})

	// A BytesContinuation built from nil bytes reports IsEnd() — it is an end
	// continuation wearing a resumable type, which is exactly the shape that
	// would slip past a type-only check.
	It("refuses nil bytes wearing a BytesContinuation", func() {
		Expect(func() {
			Stopped[int](TimeLimitReached, NewBytesContinuation(nil))
		}).To(PanicWith(ContainSubstring("end continuation")))
	})
})

// The invariants Stopped delegates to had no test of their own, so nothing
// pinned the behaviour its safety argument rests on: relax either panic in
// NewResultNoNext and Stopped silently starts minting the results Java's
// RecordCursorResult.withoutNextValue (RecordCursorResult.java:252-258) refuses
// to construct — an out-of-band stop that reads as exhaustion, which a parent
// treats as "no more rows" and turns into a silently short answer.
var _ = Describe("RecordCursorResult no-next invariants", func() {
	It("rejects an end continuation with a non-exhausted reason", func() {
		Expect(func() {
			NewResultNoNext[int](TimeLimitReached, &EndContinuation{})
		}).To(PanicWith(ContainSubstring("end continuation")))
	})

	It("rejects SourceExhausted without an end continuation", func() {
		Expect(func() {
			NewResultNoNext[int](SourceExhausted, NewBytesContinuation([]byte{0x03}))
		}).To(PanicWith(ContainSubstring("SourceExhausted requires an end continuation")))
	})

	It("accepts the two legal pairings", func() {
		exhausted := NewResultNoNext[int](SourceExhausted, &EndContinuation{})
		Expect(exhausted.GetContinuation().IsEnd()).To(BeTrue())
		Expect(exhausted.HasStoppedBeforeEnd()).To(BeFalse())

		stopped := NewResultNoNext[int](ScanLimitReached, NewBytesContinuation([]byte{0x07}))
		Expect(stopped.GetContinuation().IsEnd()).To(BeFalse())
		Expect(stopped.HasStoppedBeforeEnd()).To(BeTrue())
	})
})
