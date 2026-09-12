package executor

import (
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// NumericAggregationValue.NumericAccumulator starts with a null state, not
// positive zero. Its first non-null partial is adopted without addition.
func TestAggregateSumAvgSignedZero(t *testing.T) {
	t.Parallel()
	negZero := math.Copysign(0, -1)
	for _, fn := range []expressions.AggregateFunction{expressions.AggSum, expressions.AggAvg} {
		for _, tc := range []struct {
			name string
			in   []any
			want any
		}{
			{"empty", nil, nil},
			{"nulls", []any{nil, nil}, nil},
			{"one_negative_zero", []any{negZero}, negZero},
			{"negative_zeros", []any{negZero, negZero}, negZero},
			{"nulls_around_negative_zeros", []any{nil, negZero, nil, negZero, nil}, negZero},
			{"positive_zeros", []any{0.0, 0.0}, 0.0},
			{"negative_then_positive", []any{negZero, 0.0}, 0.0},
			{"positive_then_negative", []any{0.0, negZero}, 0.0},
			{"cancellation", []any{-1.0, 1.0}, 0.0},
		} {
			for split := 0; split <= len(tc.in); split++ {
				t.Run(fmt.Sprintf("%s/%s/resume_after_%d", fn, tc.name, split), func(t *testing.T) {
					t.Parallel()
					c := avgTestCursor(t, values.NullableDouble)
					c.aggregates[0].Function = fn
					for _, v := range tc.in[:split] {
						if err := c.accumulateRow(dorder([]string{"V"}, []any{v})); err != nil {
							t.Fatal(err)
						}
					}
					token, err := encodeAggregateContinuation(recordlayer.NewBytesContinuation([]byte{7}), "", nil, c.current, c.aggregates)
					if err != nil {
						t.Fatal(err)
					}
					_, key, state, err := decodeAggregateContinuation(token, len(c.aggregates))
					if err != nil || state == nil {
						t.Fatalf("restore accumulator: state=%v err=%v", state, err)
					}
					resumed := avgTestCursor(t, values.NullableDouble)
					resumed.aggregates[0].Function = fn
					resumed.withPartialState(key, state.keyVals, state)
					got := avgAccumulateAll(t, resumed, tc.in[split:])
					if tc.want == nil {
						if got != nil {
							t.Fatalf("got %v, want NULL", got)
						}
						return
					}
					f, ok := got.(float64)
					want := tc.want.(float64)
					if !ok || math.Float64bits(f) != math.Float64bits(want) {
						t.Fatalf("%s(%v) = %T(%v) bits=%016x, want %v bits=%016x", fn, tc.in, got, got, math.Float64bits(f), want, math.Float64bits(want))
					}
				})
			}
		}
	}
}
