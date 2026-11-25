package executor

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
)

var codecRowNames = []string{"K", "I", "L", "F", "D"}

func codecTestCursor(t *testing.T, keyType values.Type) *aggregateCursor {
	t.Helper()
	rowType := values.NewRecordType("", false, []values.Field{
		{Name: "K", FieldType: keyType, Ordinal: 0},
		{Name: "I", FieldType: values.NullableInt, Ordinal: 1},
		{Name: "L", FieldType: values.NullableLong, Ordinal: 2},
		{Name: "F", FieldType: values.NullableFloat, Ordinal: 3},
		{Name: "D", FieldType: values.NullableDouble, Ordinal: 4},
	})
	qov := mustTestQOV(t, values.NamedCorrelationIdentifier("T"), rowType)
	col := func(ord int) values.Value { return mustTestFieldOrdinal(t, qov, ord) }
	agg := func(fn expressions.AggregateFunction, ord int, lane values.TypeCode) expressions.AggregateSpec {
		return expressions.AggregateSpec{Function: fn, Operand: col(ord), OperandIntType: lane}
	}
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount},
		agg(expressions.AggCount, 2, values.TypeCodeLong),
		agg(expressions.AggSum, 1, values.TypeCodeInt),
		agg(expressions.AggSum, 2, values.TypeCodeLong),
		agg(expressions.AggSum, 3, values.TypeCodeFloat),
		agg(expressions.AggSum, 4, values.TypeCodeDouble),
		agg(expressions.AggAvg, 1, values.TypeCodeInt),
		agg(expressions.AggAvg, 4, values.TypeCodeDouble),
		agg(expressions.AggMin, 1, values.TypeCodeInt),
		agg(expressions.AggMax, 2, values.TypeCodeLong),
		agg(expressions.AggMin, 3, values.TypeCodeFloat),
		agg(expressions.AggMax, 4, values.TypeCodeDouble),
		agg(expressions.AggBitmapConstructAgg, 1, values.TypeCodeInt),
		{Function: expressions.AggArrayAgg, Operand: col(0), Limit: values.ArrayAggNoLimit},
		{Function: expressions.AggArrayAgg, Operand: col(4), Limit: 3, IgnoreNulls: true},
	}
	return &aggregateCursor{groupingKeys: []values.Value{col(0)}, aggregates: aggs}
}

// Every aggregate kind resumed from Java's partial payload after every prefix
// of the group, including the all-NULL prefix where every value accumulator is
// still empty (Java omits those states, upstream #4573), finishes with the
// uninterrupted answer.
func TestAggregateJavaPartialResumeEveryPrefix(t *testing.T) {
	t.Parallel()
	rows := [][]any{
		{"g", nil, nil, nil, nil},
		{"g", int64(3), int64(10), float64(float32(1.5)), 2.25},
		{"g", nil, int64(-4), nil, math.Copysign(0, -1)},
		{"g", int64(9), nil, float64(float32(-2.5)), nil},
		{"g", int64(1), int64(5), float64(float32(0.1)), 8.0},
	}
	run := func(split int) string {
		c := codecTestCursor(t, values.NullableString)
		for i := 0; i <= len(rows); i++ {
			if i == split && i > 0 {
				encoded, err := encodeAggregateContinuation(nil, c.groupingKeys, c.currentKeyVals, c.current, c.aggregates)
				if err != nil {
					t.Fatalf("split %d: encode: %v", split, err)
				}
				_, key, gs, err := decodeAggregateContinuation(encoded, c.groupingKeys, c.aggregates, nil)
				if err != nil {
					t.Fatalf("split %d: decode: %v", split, err)
				}
				if key != c.currentGroupKey {
					t.Fatalf("split %d: resumed group key %q, live %q", split, key, c.currentGroupKey)
				}
				c = codecTestCursor(t, values.NullableString)
				c.withPartialState(key, gs.keyVals, gs)
			}
			if i == len(rows) {
				break
			}
			row := dorder(codecRowNames, rows[i])
			if c.current == nil {
				key, keyVals, err := c.computeGroupKey(row)
				if err != nil {
					t.Fatal(err)
				}
				c.currentGroupKey, c.currentKeyVals = key, keyVals
				c.current = c.newGroupState()
			}
			if err := c.accumulateRow(row); err != nil {
				t.Fatal(err)
			}
		}
		return fmt.Sprintf("%#v", c.finalizeGroup().Positional.Slots)
	}
	want := run(-1)
	for split := 1; split <= len(rows); split++ {
		if got := run(split); got != want {
			t.Errorf("split %d:\n got %s\nwant %s", split, got, want)
		}
	}
}

// The group key rides as the grouping record's message, so every scalar key
// type has to come back as the value live evaluation produces, or the resumed
// group never matches and is emitted twice.
func TestAggregateJavaPartialGroupKeyTypes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		typ values.Type
		key any
	}{
		{values.NullableString, "abc"},
		{values.NullableString, ""},
		{values.NullableString, nil},
		{values.NullableLong, int64(math.MinInt64)},
		{values.NullableInt, int64(-7)},
		{values.NullableDouble, math.Copysign(0, -1)},
		{values.NullableDouble, math.NaN()},
		{values.NullableFloat, float64(float32(3.25))},
		{values.NullableBoolean, false},
		{values.NullableBytes, []byte{0, 1, 0xff}},
	} {
		t.Run(fmt.Sprintf("%v/%v", tc.typ, tc.key), func(t *testing.T) {
			t.Parallel()
			c := codecTestCursor(t, tc.typ)
			c.aggregates = c.aggregates[:1]
			row := dorder(codecRowNames, []any{tc.key, int64(1), int64(2), 0.5, 1.5})
			liveKey, keyVals, err := c.computeGroupKey(row)
			if err != nil {
				t.Fatal(err)
			}
			c.currentGroupKey, c.currentKeyVals = liveKey, keyVals
			c.current = c.newGroupState()
			if err := c.accumulateRow(row); err != nil {
				t.Fatal(err)
			}
			encoded, err := encodeAggregateContinuation(nil, c.groupingKeys, keyVals, c.current, c.aggregates)
			if err != nil {
				t.Fatal(err)
			}
			_, key, gs, err := decodeAggregateContinuation(encoded, c.groupingKeys, c.aggregates, nil)
			if err != nil {
				t.Fatal(err)
			}
			if key != liveKey {
				t.Errorf("resumed group key %x, live %x", key, liveKey)
			}
			if got, want := fmt.Sprintf("%#v", gs.keyVals), fmt.Sprintf("%#v", keyVals); got != want {
				t.Errorf("resumed key values %s, live %s", got, want)
			}
		})
	}
}

// A GROUP BY with no aggregates still resumes its open group: the partial is
// the group key alone.
func TestAggregateJavaPartialNoAggregates(t *testing.T) {
	t.Parallel()
	c := codecTestCursor(t, values.NullableLong)
	c.aggregates = nil
	row := dorder(codecRowNames, []any{int64(4), nil, nil, nil, nil})
	key, keyVals, err := c.computeGroupKey(row)
	if err != nil {
		t.Fatal(err)
	}
	c.currentKeyVals = keyVals
	gs := c.newGroupState()
	gs.count = 1
	encoded, err := encodeAggregateContinuation(nil, c.groupingKeys, keyVals, gs, nil)
	if err != nil {
		t.Fatal(err)
	}
	var msg gen.AggregateCursorContinuation
	if err := proto.Unmarshal(encoded, &msg); err != nil {
		t.Fatal(err)
	}
	if n := len(msg.GetPartialAggregationResults().GetAccumulatorStates()); n != 0 {
		t.Fatalf("%d accumulator states for no aggregates", n)
	}
	_, gotKey, got, err := decodeAggregateContinuation(encoded, c.groupingKeys, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || gotKey != key || fmt.Sprint(got.keyVals) != "[4]" {
		t.Fatalf("resumed %q %+v, want the group [4]", gotKey, got)
	}
}

// A partial that cannot be a state of these aggregates is refused rather than
// resumed into a wrong answer.
func TestAggregateJavaPartialRejectsMalformed(t *testing.T) {
	t.Parallel()
	c := codecTestCursor(t, values.NullableString)
	row := dorder(codecRowNames, []any{"g", int64(3), int64(10), 1.5, 2.25})
	key, keyVals, err := c.computeGroupKey(row)
	if err != nil {
		t.Fatal(err)
	}
	c.currentGroupKey, c.currentKeyVals = key, keyVals
	c.current = c.newGroupState()
	if err := c.accumulateRow(row); err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeAggregateContinuation(nil, c.groupingKeys, keyVals, c.current, c.aggregates)
	if err != nil {
		t.Fatal(err)
	}
	bytesState := func(b []byte) *gen.OneOfTypedState {
		return &gen.OneOfTypedState{State: &gen.OneOfTypedState_BytesState{BytesState: b}}
	}
	i64 := int64State
	for _, tc := range []struct {
		name   string
		mutate func(*gen.PartialAggregationResult)
		want   string
	}{
		{"missing aggregate", func(p *gen.PartialAggregationResult) {
			p.AccumulatorStates = p.AccumulatorStates[1:]
		}, "accumulator states"},
		{"negative count", func(p *gen.PartialAggregationResult) {
			p.AccumulatorStates[0].State[0] = i64(-1)
		}, "not a count"},
		{"SUM_I as a LONG", func(p *gen.PartialAggregationResult) {
			p.AccumulatorStates[2].State[0] = i64(3)
		}, "does not hold a INT"},
		{"MIN_F as bytes", func(p *gen.PartialAggregationResult) {
			p.AccumulatorStates[10].State[0] = bytesState([]byte("x"))
		}, "does not hold a FLOAT"},
		{"AVG without its count", func(p *gen.PartialAggregationResult) {
			p.AccumulatorStates[7].State = p.AccumulatorStates[7].State[:1]
		}, "typed states, want 2"},
		{"AVG count zero", func(p *gen.PartialAggregationResult) {
			p.AccumulatorStates[7].State[1] = i64(0)
		}, "not positive"},
		{"two states for MAX", func(p *gen.PartialAggregationResult) {
			p.AccumulatorStates[9].State = append(p.AccumulatorStates[9].State, i64(1))
		}, "typed states, want 1"},
		{"ARRAY_AGG past its limit", func(p *gen.PartialAggregationResult) {
			wrapper := []byte{}
			for _, d := range []float64{1, 2, 3, 4} {
				wrapper = append(wrapper, 0x09)
				wrapper = append(wrapper, mustLittleEndian(d)...)
			}
			p.AccumulatorStates[14].State[0] = bytesState(wrapper)
		}, "past the limit"},
		{"ARRAY_AGG not bytes", func(p *gen.PartialAggregationResult) {
			p.AccumulatorStates[13].State[0] = i64(1)
		}, "not bytes"},
		{"group key not a grouping record", func(p *gen.PartialAggregationResult) {
			p.GroupKey = []byte{0xff}
		}, "grouping key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var msg gen.AggregateCursorContinuation
			if err := proto.Unmarshal(encoded, &msg); err != nil {
				t.Fatal(err)
			}
			tc.mutate(msg.PartialAggregationResults)
			bad, err := proto.Marshal(&msg)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = decodeAggregateContinuation(bad, c.groupingKeys, c.aggregates, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("decode = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

func mustLittleEndian(d float64) []byte {
	b := make([]byte, 8)
	bits := math.Float64bits(d)
	for i := range b {
		b[i] = byte(bits >> (8 * i))
	}
	return b
}
