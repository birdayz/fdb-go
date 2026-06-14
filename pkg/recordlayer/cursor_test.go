package recordlayer

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NoNextReason", func() {
	Describe("IsOutOfBand", func() {
		It("returns false for SourceExhausted", func() {
			Expect(SourceExhausted.IsOutOfBand()).To(BeFalse())
		})

		It("returns false for ReturnLimitReached", func() {
			Expect(ReturnLimitReached.IsOutOfBand()).To(BeFalse())
		})

		It("returns true for ByteLimitReached", func() {
			Expect(ByteLimitReached.IsOutOfBand()).To(BeTrue())
		})

		It("returns true for TimeLimitReached", func() {
			Expect(TimeLimitReached.IsOutOfBand()).To(BeTrue())
		})

		It("returns true for ScanLimitReached", func() {
			Expect(ScanLimitReached.IsOutOfBand()).To(BeTrue())
		})
	})

	Describe("IsSourceExhausted", func() {
		It("returns true only for SourceExhausted", func() {
			Expect(SourceExhausted.IsSourceExhausted()).To(BeTrue())
		})

		It("returns false for ReturnLimitReached", func() {
			Expect(ReturnLimitReached.IsSourceExhausted()).To(BeFalse())
		})

		It("returns false for ByteLimitReached", func() {
			Expect(ByteLimitReached.IsSourceExhausted()).To(BeFalse())
		})

		It("returns false for TimeLimitReached", func() {
			Expect(TimeLimitReached.IsSourceExhausted()).To(BeFalse())
		})

		It("returns false for ScanLimitReached", func() {
			Expect(ScanLimitReached.IsSourceExhausted()).To(BeFalse())
		})
	})

	Describe("IsLimitReached", func() {
		It("returns false for SourceExhausted", func() {
			Expect(SourceExhausted.IsLimitReached()).To(BeFalse())
		})

		It("returns true for ReturnLimitReached", func() {
			Expect(ReturnLimitReached.IsLimitReached()).To(BeTrue())
		})

		It("returns true for ByteLimitReached", func() {
			Expect(ByteLimitReached.IsLimitReached()).To(BeTrue())
		})

		It("returns true for TimeLimitReached", func() {
			Expect(TimeLimitReached.IsLimitReached()).To(BeTrue())
		})

		It("returns true for ScanLimitReached", func() {
			Expect(ScanLimitReached.IsLimitReached()).To(BeTrue())
		})
	})
})

var _ = Describe("BytesContinuation", func() {
	It("ToBytes returns data and nil error when data is non-nil", func() {
		c := &BytesContinuation{bytes: []byte{1, 2, 3}}
		b, err := c.ToBytes()
		Expect(err).To(BeNil())
		Expect(b).To(Equal([]byte{1, 2, 3}))
	})

	It("ToBytes returns nil and nil error when data is nil", func() {
		c := &BytesContinuation{bytes: nil}
		b, err := c.ToBytes()
		Expect(err).To(BeNil())
		Expect(b).To(BeNil())
	})

	It("IsEnd returns false when data is non-nil", func() {
		c := &BytesContinuation{bytes: []byte{0x00}}
		Expect(c.IsEnd()).To(BeFalse())
	})

	It("IsEnd returns true when data is nil", func() {
		c := &BytesContinuation{bytes: nil}
		Expect(c.IsEnd()).To(BeTrue())
	})
})

var _ = Describe("EndContinuation", func() {
	It("ToBytes returns nil, nil", func() {
		c := &EndContinuation{}
		b, err := c.ToBytes()
		Expect(err).To(BeNil())
		Expect(b).To(BeNil())
	})

	It("IsEnd returns true", func() {
		c := &EndContinuation{}
		Expect(c.IsEnd()).To(BeTrue())
	})
})

var _ = Describe("StartContinuation", func() {
	It("ToBytes returns nil, nil", func() {
		c := &StartContinuation{}
		b, err := c.ToBytes()
		Expect(err).To(BeNil())
		Expect(b).To(BeNil())
	})

	It("IsEnd returns false", func() {
		c := &StartContinuation{}
		Expect(c.IsEnd()).To(BeFalse())
	})
})

var _ = Describe("RecordCursorResult", func() {
	Describe("NewResultWithValue", func() {
		It("HasNext returns true", func() {
			cont := &BytesContinuation{bytes: []byte{1}}
			r := NewResultWithValue(42, cont)
			Expect(r.HasNext()).To(BeTrue())
		})

		It("GetValue returns the stored value", func() {
			r := NewResultWithValue("hello", &StartContinuation{})
			Expect(r.GetValue()).To(Equal("hello"))
		})

		It("GetNoNextReason returns the zero value (SourceExhausted)", func() {
			r := NewResultWithValue(99, &StartContinuation{})
			Expect(r.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("GetContinuation returns the provided continuation", func() {
			cont := &BytesContinuation{bytes: []byte{7}}
			r := NewResultWithValue(1, cont)
			Expect(r.GetContinuation()).To(BeIdenticalTo(cont))
		})

		It("works with a struct type", func() {
			type Point struct{ X, Y int }
			r := NewResultWithValue(Point{X: 3, Y: 4}, &StartContinuation{})
			Expect(r.GetValue()).To(Equal(Point{X: 3, Y: 4}))
		})

		It("panics with EndContinuation", func() {
			Expect(func() {
				NewResultWithValue(42, &EndContinuation{})
			}).To(Panic())
		})
	})

	Describe("NewResultNoNext", func() {
		It("HasNext returns false", func() {
			r := NewResultNoNext[int](SourceExhausted, &EndContinuation{})
			Expect(r.HasNext()).To(BeFalse())
		})

		It("GetNoNextReason returns the provided reason", func() {
			r := NewResultNoNext[string](ReturnLimitReached, &BytesContinuation{bytes: []byte{1}})
			Expect(r.GetNoNextReason()).To(Equal(ReturnLimitReached))
		})

		It("GetContinuation returns the provided continuation", func() {
			cont := &BytesContinuation{bytes: []byte{1, 2, 3, 4}}
			r := NewResultNoNext[int](ByteLimitReached, cont)
			Expect(r.GetContinuation()).To(BeIdenticalTo(cont))
		})

		It("panics with EndContinuation for non-SourceExhausted", func() {
			Expect(func() {
				NewResultNoNext[int](ReturnLimitReached, &EndContinuation{})
			}).To(Panic())
		})

		It("panics with non-EndContinuation for SourceExhausted", func() {
			Expect(func() {
				NewResultNoNext[int](SourceExhausted, &BytesContinuation{bytes: []byte{1}})
			}).To(Panic())
		})
	})

	Describe("GetValue panics when HasNext is false", func() {
		It("panics", func() {
			r := NewResultNoNext[int](SourceExhausted, &EndContinuation{})
			Expect(func() { r.GetValue() }).To(Panic())
		})
	})

	Describe("HasStoppedBeforeEnd", func() {
		It("returns false when HasNext is true", func() {
			r := NewResultWithValue(1, &BytesContinuation{bytes: []byte{1}})
			Expect(r.HasStoppedBeforeEnd()).To(BeFalse())
		})

		It("returns false for SourceExhausted", func() {
			r := NewResultNoNext[int](SourceExhausted, &EndContinuation{})
			Expect(r.HasStoppedBeforeEnd()).To(BeFalse())
		})

		It("returns true for ReturnLimitReached with continuation", func() {
			r := NewResultNoNext[int](ReturnLimitReached, &BytesContinuation{bytes: []byte{1}})
			Expect(r.HasStoppedBeforeEnd()).To(BeTrue())
		})

		It("returns true for ByteLimitReached with continuation", func() {
			r := NewResultNoNext[int](ByteLimitReached, &BytesContinuation{bytes: []byte{1}})
			Expect(r.HasStoppedBeforeEnd()).To(BeTrue())
		})

		It("returns true for TimeLimitReached with continuation", func() {
			r := NewResultNoNext[int](TimeLimitReached, &BytesContinuation{bytes: []byte{1}})
			Expect(r.HasStoppedBeforeEnd()).To(BeTrue())
		})

		It("returns true for ScanLimitReached with continuation", func() {
			r := NewResultNoNext[int](ScanLimitReached, &BytesContinuation{bytes: []byte{1}})
			Expect(r.HasStoppedBeforeEnd()).To(BeTrue())
		})

		It("returns true for ReturnLimitReached with StartContinuation", func() {
			r := NewResultNoNext[int](ReturnLimitReached, &StartContinuation{})
			Expect(r.HasStoppedBeforeEnd()).To(BeTrue())
		})
	})

	Describe("WithContinuation", func() {
		It("returns a copy with the new continuation", func() {
			original := NewResultWithValue(10, &StartContinuation{})
			newCont := &BytesContinuation{bytes: []byte{0xAB}}
			updated := original.WithContinuation(newCont)
			Expect(updated.GetContinuation()).To(BeIdenticalTo(newCont))
		})

		It("leaves the original continuation unchanged", func() {
			origCont := &StartContinuation{}
			original := NewResultWithValue(10, origCont)
			_ = original.WithContinuation(&BytesContinuation{bytes: []byte{1}})
			Expect(original.GetContinuation()).To(BeIdenticalTo(origCont))
		})

		It("preserves HasNext and value", func() {
			original := NewResultWithValue(42, &BytesContinuation{bytes: []byte{1}})
			updated := original.WithContinuation(&BytesContinuation{bytes: []byte{9}})
			Expect(updated.HasNext()).To(BeTrue())
			Expect(updated.GetValue()).To(Equal(42))
		})
	})
})

var _ = Describe("MapResult", func() {
	It("maps the value when HasNext is true", func() {
		r := NewResultWithValue(3, &StartContinuation{})
		mapped := MapResult(r, func(v int) string { return "x" })
		Expect(mapped.HasNext()).To(BeTrue())
		Expect(mapped.GetValue()).To(Equal("x"))
	})

	It("preserves the continuation on a value result", func() {
		cont := &BytesContinuation{bytes: []byte{5}}
		r := NewResultWithValue(1, cont)
		mapped := MapResult(r, func(v int) int { return v * 2 })
		Expect(mapped.GetContinuation()).To(BeIdenticalTo(cont))
	})

	It("passes through noNextReason when HasNext is false", func() {
		r := NewResultNoNext[int](TimeLimitReached, &StartContinuation{})
		mapped := MapResult(r, func(v int) string { return "never" })
		Expect(mapped.HasNext()).To(BeFalse())
		Expect(mapped.GetNoNextReason()).To(Equal(TimeLimitReached))
	})

	It("passes through continuation when HasNext is false", func() {
		cont := &BytesContinuation{bytes: []byte{0xDE, 0xAD}}
		r := NewResultNoNext[int](ByteLimitReached, cont)
		mapped := MapResult(r, func(v int) bool { return false })
		Expect(mapped.GetContinuation()).To(BeIdenticalTo(cont))
	})
})

var _ = Describe("emptyCursor", func() {
	It("OnNext returns SourceExhausted with EndContinuation", func() {
		c := Empty[int]()
		result, err := c.OnNext(context.Background())
		Expect(err).To(BeNil())
		Expect(result.HasNext()).To(BeFalse())
		Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		Expect(result.GetContinuation().IsEnd()).To(BeTrue())
	})

	It("OnNext returns SourceExhausted on repeated calls", func() {
		c := Empty[string]()
		for i := 0; i < 3; i++ {
			result, err := c.OnNext(context.Background())
			Expect(err).To(BeNil())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		}
	})

	It("Close returns nil", func() {
		c := Empty[int]()
		Expect(c.Close()).To(BeNil())
	})
})

var _ = Describe("errorCursor", func() {
	It("OnNext returns the error", func() {
		sentinel := errors.New("boom")
		c := &errorCursor[int]{err: sentinel}
		_, err := c.OnNext(context.Background())
		Expect(err).To(MatchError(sentinel))
	})

	It("Close returns nil", func() {
		c := &errorCursor[string]{err: errors.New("x")}
		Expect(c.Close()).To(BeNil())
	})
})

var _ = Describe("listCursor / FromList / FromListWithContinuation", func() {
	ctx := context.Background()

	Describe("FromList", func() {
		It("iterates all items in order", func() {
			c := FromList([]int{10, 20, 30})
			for _, want := range []int{10, 20, 30} {
				result, err := c.OnNext(ctx)
				Expect(err).To(BeNil())
				Expect(result.HasNext()).To(BeTrue())
				Expect(result.GetValue()).To(Equal(want))
			}
		})

		It("returns SourceExhausted with EndContinuation after all items", func() {
			c := FromList([]int{1})
			_, _ = c.OnNext(ctx)
			result, err := c.OnNext(ctx)
			Expect(err).To(BeNil())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
			Expect(result.GetContinuation().IsEnd()).To(BeTrue())
		})

		It("returns SourceExhausted immediately on empty list", func() {
			c := FromList([]int{})
			result, err := c.OnNext(ctx)
			Expect(err).To(BeNil())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("encodes continuation as 4-byte big-endian position", func() {
			c := FromList([]string{"a", "b"})
			result, err := c.OnNext(ctx)
			Expect(err).To(BeNil())
			b, bErr := result.GetContinuation().ToBytes()
			Expect(bErr).To(BeNil())
			// After reading item 0, position is 1 → big-endian 4 bytes: [0,0,0,1]
			Expect(b).To(Equal([]byte{0, 0, 0, 1}))
		})

		It("continuation is not IsEnd while items remain", func() {
			c := FromList([]int{5, 6})
			result, _ := c.OnNext(ctx)
			Expect(result.GetContinuation().IsEnd()).To(BeFalse())
		})

		It("Close prevents further results", func() {
			c := FromList([]int{1, 2, 3})
			Expect(c.Close()).To(BeNil())
			result, err := c.OnNext(ctx)
			Expect(err).To(BeNil())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		})
	})

	Describe("FromListWithContinuation", func() {
		It("nil continuation starts from the beginning", func() {
			c := FromListWithContinuation([]int{1, 2, 3}, nil)
			result, err := c.OnNext(ctx)
			Expect(err).To(BeNil())
			Expect(result.HasNext()).To(BeTrue())
			Expect(result.GetValue()).To(Equal(1))
		})

		It("empty continuation starts from the beginning", func() {
			c := FromListWithContinuation([]int{10, 20}, []byte{})
			result, err := c.OnNext(ctx)
			Expect(err).To(BeNil())
			Expect(result.HasNext()).To(BeTrue())
			Expect(result.GetValue()).To(Equal(10))
		})

		It("resumes correctly from a valid continuation", func() {
			// continuation encoding position=2: [0,0,0,2]
			cont := []byte{0, 0, 0, 2}
			c := FromListWithContinuation([]int{10, 20, 30, 40}, cont)
			result, err := c.OnNext(ctx)
			Expect(err).To(BeNil())
			Expect(result.HasNext()).To(BeTrue())
			Expect(result.GetValue()).To(Equal(30))
		})

		It("past-end continuation returns immediate SourceExhausted", func() {
			cont := []byte{0, 0, 0, 99} // position 99 > len(items)=3
			c := FromListWithContinuation([]int{1, 2, 3}, cont)
			result, err := c.OnNext(ctx)
			Expect(err).To(BeNil())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))
		})

		It("invalid continuation (<4 bytes) produces an error from OnNext", func() {
			c := FromListWithContinuation([]int{1, 2, 3}, []byte{0, 1})
			_, err := c.OnNext(ctx)
			Expect(err).To(HaveOccurred())
		})

		It("round-trips: continuation from one cursor resumes the next", func() {
			items := []int{100, 200, 300, 400}
			c1 := FromList(items)

			// Read first two items and capture continuation after second
			_, _ = c1.OnNext(ctx)
			r2, _ := c1.OnNext(ctx)
			contBytes, _ := r2.GetContinuation().ToBytes()

			c2 := FromListWithContinuation(items, contBytes)
			r3, err := c2.OnNext(ctx)
			Expect(err).To(BeNil())
			Expect(r3.HasNext()).To(BeTrue())
			Expect(r3.GetValue()).To(Equal(300))
		})
	})
})

var _ = Describe("Seq2 / SeqWithContinuation", func() {
	ctx := context.Background()

	Describe("Seq2", func() {
		It("yields (value, nil) pairs from FromList", func() {
			c := FromList([]string{"a", "b"})
			var vals []string
			var errs []error
			for v, err := range Seq2(c, ctx) {
				vals = append(vals, v)
				errs = append(errs, err)
			}
			Expect(vals).To(Equal([]string{"a", "b"}))
			Expect(errs).To(Equal([]error{nil, nil}))
		})

		It("yields (zero, error) for an error cursor and stops", func() {
			sentinel := errors.New("cursor error")
			c := &errorCursor[int]{err: sentinel}
			var vals []int
			var errs []error
			for v, err := range Seq2(c, ctx) {
				vals = append(vals, v)
				errs = append(errs, err)
			}
			Expect(vals).To(Equal([]int{0}))
			Expect(errs).To(HaveLen(1))
			Expect(errs[0]).To(MatchError(sentinel))
		})
	})

	Describe("SeqWithContinuation", func() {
		It("yields (value, continuation) pairs from FromList", func() {
			c := FromList([]int{10, 20})
			type pair struct {
				v    int
				cont RecordCursorContinuation
			}
			var got []pair
			for v, cont := range SeqWithContinuation(c, ctx) {
				got = append(got, pair{v, cont})
			}
			Expect(got).To(HaveLen(2))
			Expect(got[0].v).To(Equal(10))
			Expect(got[0].cont.IsEnd()).To(BeFalse())
			Expect(got[1].v).To(Equal(20))
			// After last item position=2, list exhausted on next call, but continuation
			// returned with the item itself encodes the position after reading it.
			b, err := got[1].cont.ToBytes()
			Expect(err).To(BeNil())
			Expect(b).To(Equal([]byte{0, 0, 0, 2}))
		})

		It("yields nothing from an error cursor", func() {
			c := &errorCursor[string]{err: errors.New("x")}
			var got []string
			for v := range SeqWithContinuation(c, ctx) {
				got = append(got, v)
			}
			Expect(got).To(BeEmpty())
		})
	})
})
