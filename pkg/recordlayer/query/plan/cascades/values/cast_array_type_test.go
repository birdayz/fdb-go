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
