package executor

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Java NumericAccumulator starts with null state: the first non-null partial
// becomes the state without an addition. An implicit +0 changes SUM/AVG(-0)
// to +0; IEEE equality alone cannot detect the wrong answer.
func TestAggregateSumInitialState(t *testing.T) {
	t.Parallel()
	negZero := math.Copysign(0, -1)
	for _, fn := range []expressions.AggregateFunction{expressions.AggSum, expressions.AggAvg} {
		for _, tc := range []struct {
			name string
			vals []any
			want any
		}{
			{name: "single_negative_zero", vals: []any{negZero}, want: negZero},
			{name: "negative_zeros", vals: []any{negZero, negZero}, want: negZero},
			{name: "leading_null", vals: []any{nil, negZero}, want: negZero},
			{name: "interleaved_nulls", vals: []any{nil, negZero, nil, negZero, nil}, want: negZero},
			{name: "positive_zero", vals: []any{float64(0)}, want: float64(0)},
			{name: "negative_then_positive", vals: []any{negZero, float64(0)}, want: float64(0)},
			{name: "positive_then_negative", vals: []any{float64(0), negZero}, want: float64(0)},
			{name: "cancellation", vals: []any{float64(-1), float64(1)}, want: float64(0)},
			{name: "all_null", vals: []any{nil, nil}},
			{name: "empty"},
		} {
			t.Run(fn.String()+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				// Round-trip after every possible prefix, including an all-NULL
				// prefix and a completed non-empty group. The saved non-null
				// count, not the row count or sum==0, determines initialization.
				for split := 0; split <= len(tc.vals); split++ {
					c := avgTestCursor(t, values.NullableDouble)
					c.aggregates[0].Function = fn
					for i := 0; i <= len(tc.vals); i++ {
						if i == split {
							encoded, err := encodeAggregateContinuation(nil, "", nil, c.current, c.aggregates)
							if err != nil {
								t.Fatal(err)
							}
							_, key, state, err := decodeAggregateContinuation(encoded, len(c.aggregates))
							if err != nil {
								t.Fatal(err)
							}
							c = avgTestCursor(t, values.NullableDouble)
							c.aggregates[0].Function = fn
							c.withPartialState(key, state.keyVals, state)
						}
						if i == len(tc.vals) {
							break
						}
						if err := c.accumulateRow(dorder([]string{"V"}, []any{tc.vals[i]})); err != nil {
							t.Fatal(err)
						}
					}
					got, ok := c.finalizeGroup().Positional.Get(0)
					if !ok {
						t.Fatal("aggregate emitted no output slot")
					}
					if tc.want == nil {
						if got != nil {
							t.Errorf("split %d: got %v, want NULL", split, got)
						}
						continue
					}
					f, ok := got.(float64)
					if !ok || math.Float64bits(f) != math.Float64bits(tc.want.(float64)) {
						t.Errorf("split %d: got %T(%v) bits=%016x, want %v bits=%016x", split,
							got, got, math.Float64bits(f), tc.want, math.Float64bits(tc.want.(float64)))
					}
				}
			})
		}
	}
}
