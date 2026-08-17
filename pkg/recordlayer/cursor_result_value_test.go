package recordlayer

import "testing"

// RecordCursorResult holds its value BY VALUE. These pin the facts that make that
// representation safe, because they are load-bearing and not visible in a type
// signature:
//
//   - hasNext, not a nil pointer, is what says "there is a value". A no-next
//     result now carries a zero T where it used to carry nil, so anything that
//     read the value without checking would get a plausible zero instead of a
//     panic — GetValue must keep refusing.
//   - every transform that REBUILDS a result carries the value across rather than
//     dropping it. With the value inline there is no pointer to copy by accident,
//     so a rebuild that forgets the field compiles and yields a zero.
//
// What is deliberately NOT pinned here is independence from the caller's variable.
// It reads like the interesting property of the change and it is a Go tautology in
// both representations: the old code stored &value, the address of the by-value
// PARAMETER, never of the caller's variable. A test for it passes with the change
// fully reverted, so it would be coverage of the language rather than of this file.
//
// The representation exists because boxing cost one allocation per emitted row
// per cursor level, and a plan is a stack of cursors.

func TestNoNextResultStillRefusesToAnswerWithItsZeroValue(t *testing.T) {
	t.Parallel()

	result := NewResultNoNext[int](SourceExhausted, &EndContinuation{})
	if result.HasNext() {
		t.Fatal("a no-next result reports a value")
	}
	defer func() {
		if recover() == nil {
			t.Error("GetValue answered on a no-next result; with the value held inline " +
				"it would have handed back a zero that reads like real data")
		}
	}()
	_ = result.GetValue()
}

func TestEveryRebuildOfAResultCarriesItsValue(t *testing.T) {
	t.Parallel()

	continuation := &BytesContinuation{bytes: []byte{1}}
	first := NewResultWithValue(1, continuation)
	second := NewResultWithValue(2, continuation)

	// WithContinuation and MapResult must carry the value across, not drop it.
	moved := first.WithContinuation(&BytesContinuation{bytes: []byte{2}})
	if !moved.HasNext() || moved.GetValue() != 1 {
		t.Errorf("WithContinuation lost the value: hasNext=%v", moved.HasNext())
	}
	mapped := MapResult(second, func(v int) string {
		if v == 2 {
			return "two"
		}
		return "wrong"
	})
	if !mapped.HasNext() || mapped.GetValue() != "two" {
		t.Errorf("MapResult produced %q", mapped.GetValue())
	}

	// A no-next result maps to a no-next result rather than reading its value.
	exhausted := MapResult(
		NewResultNoNext[int](SourceExhausted, &EndContinuation{}),
		func(int) string { t.Error("MapResult read the value of a no-next result"); return "" },
	)
	if exhausted.HasNext() {
		t.Error("MapResult invented a value for a no-next result")
	}
}
