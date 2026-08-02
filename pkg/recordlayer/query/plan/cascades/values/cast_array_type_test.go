package values

import (
	"errors"
	"math"
	"testing"
)

// TestCastArrayPreservesSourceElementType pins the static-type handoff used by
// each injected element cast. The Go carriers alone cannot distinguish FLOAT
// from DOUBLE or INT from LONG, so replacing the declared element type with
// UnknownType changes which Java-compatible cast operator is selected.
func TestCastArrayPreservesSourceElementType(t *testing.T) {
	t.Parallel()

	t.Run("FLOAT identity retains infinity", func(t *testing.T) {
		t.Parallel()
		floatArray := NewArrayType(false, NullableFloat)
		cast := NewCastValue(
			&ConstantValue{Value: []any{math.Inf(1)}, Typ: floatArray},
			floatArray,
		)
		got, err := cast.Evaluate(nil)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		items, ok := got.([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("Evaluate = %#v (%T), want one-element array", got, got)
		}
		value, ok := items[0].(float64)
		if !ok || !math.IsInf(value, 1) {
			t.Fatalf("FLOAT identity element = %#v (%T), want +Inf retained", items[0], items[0])
		}
	})

	t.Run("LONG to BOOLEAN remains unsupported", func(t *testing.T) {
		t.Parallel()
		cast := NewCastValue(
			&ConstantValue{Value: []any{int64(1)}, Typ: NewArrayType(false, NullableLong)},
			NewArrayType(false, NullableBoolean),
		)
		_, err := cast.Evaluate(nil)
		var invalid *InvalidCastError
		if !errors.As(err, &invalid) {
			t.Fatalf("Evaluate error = %T %v, want *InvalidCastError", err, err)
		}
	})

	t.Run("missing source element type fails closed", func(t *testing.T) {
		t.Parallel()
		cast := NewCastValue(
			&ConstantValue{Value: []any{int64(1)}, Typ: UnknownType},
			NewArrayType(false, NullableLong),
		)
		_, err := cast.Evaluate(nil)
		var invalid *InvalidCastError
		if !errors.As(err, &invalid) {
			t.Fatalf("Evaluate error = %T %v, want *InvalidCastError", err, err)
		}
	})
}

// TestCastArrayWithoutSourceElementType pins WHERE the missing-source-element
// guard may fire. Java raises only when a real element cast needs the type
// (CastValue.java:599-602); an empty array returns early at :586-589 and a NULL
// element is copied through, so neither ever reads it.
//
// The dimension this covers is the ABSENCE of an element to cast, and it is the
// one the sibling test above cannot reach: it passes a single non-null element,
// so a guard hoisted to the top of the array arm looks correct there while
// rejecting `CAST([] AS INTEGER ARRAY)` — legal in both engines, and the shape
// that broke two Java corpus files.
func TestCastArrayWithoutSourceElementType(t *testing.T) {
	t.Parallel()

	t.Run("empty array casts without a source element type", func(t *testing.T) {
		t.Parallel()
		cast := NewCastValue(
			&ConstantValue{Value: []any{}, Typ: UnknownType},
			NewArrayType(false, NullableLong),
		)
		got, err := cast.Evaluate(nil)
		if err != nil {
			t.Fatalf("Evaluate empty array = %v, want an empty array; nothing needs the source element type", err)
		}
		items, ok := got.([]any)
		if !ok || len(items) != 0 {
			t.Fatalf("Evaluate = %#v (%T), want an empty array", got, got)
		}
	})

	t.Run("NULL elements pass through without a source element type", func(t *testing.T) {
		t.Parallel()
		cast := NewCastValue(
			&ConstantValue{Value: []any{nil, nil}, Typ: UnknownType},
			NewArrayType(false, NullableLong),
		)
		got, err := cast.Evaluate(nil)
		if err != nil {
			t.Fatalf("Evaluate all-NULL array = %v, want NULLs preserved; a NULL element is never cast", err)
		}
		items, ok := got.([]any)
		if !ok || len(items) != 2 || items[0] != nil || items[1] != nil {
			t.Fatalf("Evaluate = %#v, want [nil nil]", got)
		}
	})

	t.Run("a real element cast still fails closed", func(t *testing.T) {
		t.Parallel()
		// The guard must stay ARMED. Without this arm the two above could be
		// satisfied by deleting the check outright.
		cast := NewCastValue(
			&ConstantValue{Value: []any{nil, int64(1)}, Typ: UnknownType},
			NewArrayType(false, NullableBoolean),
		)
		_, err := cast.Evaluate(nil)
		var invalid *InvalidCastError
		if !errors.As(err, &invalid) {
			t.Fatalf("Evaluate error = %T %v, want *InvalidCastError for an element that must actually be cast", err, err)
		}
	})
}
