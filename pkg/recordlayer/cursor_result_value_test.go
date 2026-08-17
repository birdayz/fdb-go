package recordlayer

import "testing"

// RecordCursorResult holds its value BY VALUE. These pin the two facts that make
// that representation safe, because both are load-bearing and neither is visible
// in a type signature:
//
//   - hasNext, not a nil pointer, is what says "there is a value". A no-next
//     result now carries a zero T where it used to carry nil, so anything that
//     read the value without checking would get a plausible zero instead of a
//     panic — GetValue must keep refusing.
//   - a result's value is INDEPENDENT of the variable it was built from, so a
//     producer that reuses one variable across rows cannot rewrite a result it
//     already handed out.
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

func TestResultValueIsIndependentOfTheVariableItWasBuiltFrom(t *testing.T) {
	t.Parallel()

	continuation := &BytesContinuation{bytes: []byte{1}}
	row := 1
	first := NewResultWithValue(row, continuation)
	row = 2
	second := NewResultWithValue(row, continuation)

	if got := first.GetValue(); got != 1 {
		t.Errorf("first result value = %d, want 1 — the result tracked the caller's variable", got)
	}
	if got := second.GetValue(); got != 2 {
		t.Errorf("second result value = %d, want 2", got)
	}

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
