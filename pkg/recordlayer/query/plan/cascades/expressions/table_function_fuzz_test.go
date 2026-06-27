package expressions

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzTableFunctionExpression_NoPanic pins that
// TableFunctionExpression doesn't panic on random RangeValue
// configurations. The walker has to handle nil-streamValue and
// non-streaming Values gracefully.
func FuzzTableFunctionExpression_NoPanic(f *testing.F) {
	f.Add(int64(0), int64(10), int64(1))     // typical
	f.Add(int64(-100), int64(100), int64(7)) // negative + non-divisible
	f.Add(int64(0), int64(0), int64(1))      // empty range

	f.Fuzz(func(t *testing.T, begin, end, step int64) {
		// Skip step == 0 (range guard). Caller's responsibility.
		if step == 0 {
			return
		}
		r := values.NewRangeValue(
			values.LiteralValue(begin),
			values.LiteralValue(end),
			values.LiteralValue(step))
		tf := NewTableFunctionExpression(r)

		// Property 1: no panic on Children() / GetResultValue / etc.
		_ = tf.GetQuantifiers()
		_ = tf.GetResultValue()
		_ = tf.GetCorrelatedToWithoutChildren()
		_ = tf.HashCodeWithoutChildren()
		// Property 2: GetResultValue.Type() is non-nil.
		if tf.GetResultValue().Type() == nil {
			t.Fatal("GetResultValue.Type() = nil")
		}
		// Property 3: GetCorrelatedToWithoutChildren is empty for
		// constant-arg RangeValue.
		if got := tf.GetCorrelatedToWithoutChildren(); len(got) != 0 {
			t.Fatalf("GetCorrelatedTo = %v, want empty for constant-arg RangeValue", got)
		}
	})
}

// FuzzExplodeExpression_NoPanic pins that ExplodeExpression doesn't
// panic on random ArrayConstructor configurations.
func FuzzExplodeExpression_NoPanic(f *testing.F) {
	f.Add(uint8(0)) // empty array
	f.Add(uint8(3)) // 3 elements
	f.Add(uint8(0xFF))

	f.Fuzz(func(t *testing.T, count uint8) {
		// Build an array with `count` elements.
		const maxCount = 8
		n := int(count) % (maxCount + 1)
		elements := make([]values.Value, n)
		for i := 0; i < n; i++ {
			elements[i] = values.LiteralValue(int64(i))
		}
		arr := values.NewArrayConstructorValue(values.NotNullLong, elements)
		ex := NewExplodeExpression(arr)

		// Property 1: no panic.
		_ = ex.GetQuantifiers()
		_ = ex.GetResultValue()
		_ = ex.GetCorrelatedToWithoutChildren()
		_ = ex.HashCodeWithoutChildren()

		// Property 2: GetResultValue.Type() is non-nil.
		if ex.GetResultValue().Type() == nil {
			t.Fatal("GetResultValue.Type() = nil")
		}
	})
}
