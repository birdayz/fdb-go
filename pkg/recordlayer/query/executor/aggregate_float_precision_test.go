package executor

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Java SUM_F and AVG_F add in float, not double. Widening the operands
// before addition preserves bits Java rounds away and misses float overflow.
func TestAggregateFloatPrecision(t *testing.T) {
	t.Parallel()
	negZero := math.Copysign(0, -1)
	for _, fn := range []expressions.AggregateFunction{expressions.AggSum, expressions.AggAvg} {
		for _, tc := range []struct {
			name string
			typ  values.Type
			vals []any
			sum  any
			avg  any
		}{
			{name: "float_rounding", vals: []any{float64(1 << 24), float64(1), float64(-1 << 24)}, sum: float64(0), avg: float64(0)},
			{name: "native_float_rounding", vals: []any{float32(1 << 24), float32(1), float32(-1 << 24)}, sum: float64(0), avg: float64(0)},
			{name: "float_avg_widens_before_division", vals: []any{float64(1 << 24), float64(1), float64(1)}, sum: float64(1 << 24), avg: float64(1<<24) / 3},
			{name: "float_overflow", vals: []any{float64(math.MaxFloat32), float64(math.MaxFloat32)}, sum: math.Inf(1), avg: math.Inf(1)},
			{name: "float_negative_overflow", vals: []any{float64(-math.MaxFloat32), float64(-math.MaxFloat32)}, sum: math.Inf(-1), avg: math.Inf(-1)},
			{name: "double_control", typ: values.NullableDouble, vals: []any{float64(1 << 24), float64(1), float64(-1 << 24)}, sum: float64(1), avg: 1.0 / 3},
			{name: "double_overflow_control", typ: values.NullableDouble, vals: []any{float64(math.MaxFloat32), float64(math.MaxFloat32)}, sum: float64(math.MaxFloat32) * 2, avg: float64(math.MaxFloat32)},
			{name: "nulls_do_not_count", vals: []any{nil, float64(1 << 24), nil, float64(1), nil}, sum: float64(1 << 24), avg: float64(1 << 23)},
			{name: "negative_zero", vals: []any{nil, negZero, nil, negZero}, sum: negZero, avg: negZero},
			{name: "mixed_zeros", vals: []any{negZero, float64(0)}, sum: float64(0), avg: float64(0)},
			{name: "nan", vals: []any{math.Inf(1), math.NaN()}, sum: math.NaN(), avg: math.NaN()},
			{name: "all_null", vals: []any{nil, nil}},
			{name: "empty"},
			// Direct long-to-float conversion rounds UP. A detour through
			// double loses the low bit, hits a tie, and rounds DOWN instead.
			{name: "integer_carrier_single_rounding", vals: []any{int64(1<<62 + 1<<38 + 1)}, sum: float64(1<<62 + 1<<39), avg: float64(1<<62 + 1<<39)},
		} {
			t.Run(fn.String()+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				typ := tc.typ
				if typ == nil {
					typ = values.NullableFloat
				}
				want := tc.sum
				if fn == expressions.AggAvg {
					want = tc.avg
				}
				for split := 0; split <= len(tc.vals); split++ {
					c := avgTestCursor(t, typ)
					c.aggregates[0].Function = fn
					c.aggregates[0].OperandIntType = typ.Code()
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
							c = avgTestCursor(t, typ)
							c.aggregates[0].Function = fn
							c.aggregates[0].OperandIntType = typ.Code()
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
					if want == nil {
						if got != nil {
							t.Errorf("split %d: got %v, want NULL", split, got)
						}
						continue
					}
					f, ok := got.(float64)
					w := want.(float64)
					if !ok || (math.IsNaN(w) && !math.IsNaN(f)) || (!math.IsNaN(w) && math.Float64bits(f) != math.Float64bits(w)) {
						t.Errorf("split %d: got %T(%v) bits=%016x, want %v bits=%016x", split, got, got, math.Float64bits(f), want, math.Float64bits(w))
					}
				}
			})
		}
	}
}
