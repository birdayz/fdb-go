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
})
