package executor

import (
	"bytes"
	"fmt"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
)

// ===========================================================================
// aggregateCursor continuation round-trip
// ===========================================================================

func TestAggregateContinuation_RoundTrip_SumCount(t *testing.T) {
	t.Parallel()

	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggSum, Operand: mustNamedTestField(t, "amount", values.NullableLong)},
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: nil}}, // COUNT(*)
	}

	gs := &groupState{
		keyVals: []any{int64(42), "group-a"},
		count:   7,
		counts:  []int64{5, 7},
		sums:    []float64{123.5, 0},
		sumsI:   []int64{100, 0},
		allInt:  []bool{false, true},
		mins:    []any{int64(1), nil},
		maxs:    []any{int64(50), nil},
	}

	innerCont := recordlayer.NewBytesContinuation([]byte{0xDE, 0xAD})
	groupKey := "test-group-key"

	encoded, err := encodeLegacyAggregateContinuation(innerCont, groupKey, gs.keyVals, gs, aggs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	gotInner, gotGroupKey, gotGS, err := decodeAggregateContinuation(encoded, nil, aggs, nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !bytes.Equal(gotInner, []byte{0xDE, 0xAD}) {
		t.Errorf("innerContinuation = %x, want dead", gotInner)
	}
	if gotGroupKey != groupKey {
		t.Errorf("groupKey = %q, want %q", gotGroupKey, groupKey)
	}
	if gotGS == nil {
		t.Fatal("decoded groupState is nil")
	}
	if gotGS.count != 7 {
		t.Errorf("count = %d, want 7", gotGS.count)
	}
	if gotGS.counts[0] != 5 || gotGS.counts[1] != 7 {
		t.Errorf("counts = %v, want [5, 7]", gotGS.counts)
	}
	if gotGS.sums[0] != 123.5 {
		t.Errorf("sums[0] = %f, want 123.5", gotGS.sums[0])
	}
	if gotGS.sumsI[0] != 100 {
		t.Errorf("sumsI[0] = %d, want 100", gotGS.sumsI[0])
	}
	if gotGS.allInt[0] != false || gotGS.allInt[1] != true {
		t.Errorf("allInt = %v, want [false, true]", gotGS.allInt)
	}
	if gotGS.mins[0] != int64(1) {
		t.Errorf("mins[0] = %v, want 1", gotGS.mins[0])
	}
	if gotGS.maxs[0] != int64(50) {
		t.Errorf("maxs[0] = %v, want 50", gotGS.maxs[0])
	}

	// keyVals round-trip with exact Go types (typed codec, no float64 detour).
	if len(gotGS.keyVals) != 2 {
		t.Fatalf("keyVals len = %d, want 2", len(gotGS.keyVals))
	}
	if gotGS.keyVals[0] != int64(42) {
		t.Errorf("keyVals[0] = %v (%T), want int64(42)", gotGS.keyVals[0], gotGS.keyVals[0])
	}
	if gotGS.keyVals[1] != "group-a" {
		t.Errorf("keyVals[1] = %v, want \"group-a\"", gotGS.keyVals[1])
	}
}

func TestAggregateContinuation_NilGroupState(t *testing.T) {
	t.Parallel()

	innerCont := recordlayer.NewBytesContinuation([]byte{0x01})
	encoded, err := encodeAggregateContinuation(innerCont, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	gotInner, gotGroupKey, gotGS, err := decodeAggregateContinuation(encoded, nil, make([]expressions.AggregateSpec, 0), nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(gotInner, []byte{0x01}) {
		t.Errorf("innerContinuation = %x, want 01", gotInner)
	}
	if gotGroupKey != "" {
		t.Errorf("groupKey = %q, want empty", gotGroupKey)
	}
	if gotGS != nil {
		t.Errorf("expected nil groupState, got %+v", gotGS)
	}
}

func TestAggregateContinuation_FloatMinMax(t *testing.T) {
	t.Parallel()

	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggMin, Operand: mustNamedTestField(t, "price", values.NullableDouble)},
	}
	gs := &groupState{
		keyVals: []any{"x"},
		count:   3,
		counts:  []int64{3},
		sums:    []float64{9.75},
		sumsI:   []int64{0},
		allInt:  []bool{false},
		mins:    []any{float64(1.25)},
		maxs:    []any{float64(5.0)},
	}

	encoded, err := encodeLegacyAggregateContinuation(nil, "k", gs.keyVals, gs, aggs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	_, _, gotGS, err := decodeAggregateContinuation(encoded, nil, make([]expressions.AggregateSpec, 1), nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Assert the exact Go type, not a Sprintf string. A loose "%v == 5" tolerance
	// masked the JSON round-trip's float64->int64 collapse: float64(5.0) rendered
	// as "5" whether it stayed float64 or was re-narrowed to int64(5). MIN/MAX
	// over a DOUBLE column MUST resume as float64, or finalizeGroup emits an
	// int-typed value into a DOUBLE output slot (wrong type on the straddling row).
	if f, ok := gotGS.mins[0].(float64); !ok || f != 1.25 {
		t.Errorf("mins[0] = %#v (%T), want float64(1.25)", gotGS.mins[0], gotGS.mins[0])
	}
	if f, ok := gotGS.maxs[0].(float64); !ok || f != 5.0 {
		t.Errorf("maxs[0] = %#v (%T), want float64(5.0)", gotGS.maxs[0], gotGS.maxs[0])
	}
}

// TestAggregateContinuation_GroupKeyBytesSurvive_F4 pins that the packed
// group-break key survives the continuation round-trip BYTE-FOR-BYTE. The key is
// string(tuple.Pack(...)) — arbitrary bytes. The prior JSON payload stored it in a
// JSON string field, and json.Marshal rewrites invalid UTF-8 to U+FFFD, so a
// tuple-packed int64>=128, any negative int, any float64, or a raw []byte key was
// corrupted on resume: the restored key never matched the recomputed key, forcing
// a FALSE group break that split one straddling group into two rows.
func TestAggregateContinuation_GroupKeyBytesSurvive_F4(t *testing.T) {
	t.Parallel()

	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggCount, Operand: &values.ConstantValue{Value: nil}}, // COUNT(*)
	}
	cases := []tuple.Tuple{
		{int64(200)},   // 0x15 0xC8 — 0xC8 is invalid UTF-8
		{int64(-1)},    // 0x13 0xFE — 0xFE is invalid UTF-8
		{float64(1.5)}, // 0x21 ... — float bytes are invalid UTF-8
		{[]byte{0xff}}, // raw 0xFF — invalid UTF-8
	}
	for _, tup := range cases {
		key := string(tup.Pack())
		gs := &groupState{
			keyVals: []any{tup[0]},
			counts:  []int64{0},
			sums:    []float64{0},
			sumsI:   []int64{0},
			allInt:  []bool{true},
			mins:    []any{nil},
			maxs:    []any{nil},
		}
		encoded, err := encodeLegacyAggregateContinuation(nil, key, gs.keyVals, gs, aggs)
		if err != nil {
			t.Fatalf("encode %v: %v", tup, err)
		}
		_, gotKey, _, err := decodeAggregateContinuation(encoded, nil, aggs, nil)
		if err != nil {
			t.Fatalf("decode %v: %v", tup, err)
		}
		if gotKey != key {
			t.Errorf("group key for %v: got %x, want %x (byte-for-byte)", tup, gotKey, key)
		}
	}
}

// TestAggregateContinuation_TypesPreserved_F5 pins that keyVals and MIN/MAX
// partial state keep their exact Go type and 64-bit precision across the
// continuation. The prior JSON round-trip flipped []byte to a base64 string,
// collapsed every number to float64 (re-narrowing integral doubles to int64), and
// rounded an int64 above 2^53 through float64 — so a straddling group's key column
// and MIN/MAX values resumed wrong-typed or off-by-one.
func TestAggregateContinuation_TypesPreserved_F5(t *testing.T) {
	t.Parallel()

	bigInt := int64(1<<60 + 1) // 1152921504606846977 — not representable in float64
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggMin, Operand: mustNamedTestField(t, "v", values.NullableLong)},
		{Function: expressions.AggMax, Operand: mustNamedTestField(t, "w", values.NullableDouble)},
	}
	gs := &groupState{
		keyVals: []any{[]byte{1, 2}, float64(2.0), bigInt},
		count:   1,
		counts:  []int64{1, 1},
		sums:    []float64{0, 0},
		sumsI:   []int64{0, 0},
		allInt:  []bool{true, false},
		mins:    []any{bigInt, nil},
		maxs:    []any{nil, float64(2.0)},
	}

	encoded, err := encodeLegacyAggregateContinuation(nil, "k", gs.keyVals, gs, aggs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, _, gotGS, err := decodeAggregateContinuation(encoded, nil, aggs, nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// keyVals: []byte stays []byte (not "AQI="), float64(2.0) stays float64 (not
	// int64(2)), int64>2^53 exact (not rounded through float64).
	if b, ok := gotGS.keyVals[0].([]byte); !ok || !bytes.Equal(b, []byte{1, 2}) {
		t.Errorf("keyVals[0] = %#v (%T), want []byte{1,2}", gotGS.keyVals[0], gotGS.keyVals[0])
	}
	if f, ok := gotGS.keyVals[1].(float64); !ok || f != 2.0 {
		t.Errorf("keyVals[1] = %#v (%T), want float64(2.0)", gotGS.keyVals[1], gotGS.keyVals[1])
	}
	if n, ok := gotGS.keyVals[2].(int64); !ok || n != bigInt {
		t.Errorf("keyVals[2] = %#v (%T), want int64(%d)", gotGS.keyVals[2], gotGS.keyVals[2], bigInt)
	}

	// MIN/MAX partial state preserves type + precision the same way.
	if n, ok := gotGS.mins[0].(int64); !ok || n != bigInt {
		t.Errorf("mins[0] = %#v (%T), want int64(%d)", gotGS.mins[0], gotGS.mins[0], bigInt)
	}
	if f, ok := gotGS.maxs[1].(float64); !ok || f != 2.0 {
		t.Errorf("maxs[1] = %#v (%T), want float64(2.0)", gotGS.maxs[1], gotGS.maxs[1])
	}
}

func TestAggregateContinuation_ArrayAgg(t *testing.T) {
	t.Parallel()
	aggs := []expressions.AggregateSpec{
		{Function: expressions.AggArrayAgg, Operand: mustNamedTestField(t, "s", values.NullableString), Limit: values.ArrayAggNoLimit},
		{Function: expressions.AggSum, Operand: mustNamedTestField(t, "amount", values.NullableLong)},
		{Function: expressions.AggArrayAgg, Operand: mustNamedTestField(t, "n", values.NullableLong), IgnoreNulls: true, Limit: 3},
	}
	gs := &groupState{
		count: 2, counts: []int64{0, 2, 0}, sums: []float64{0, 3, 0}, sumsI: []int64{0, 3, 0},
		allInt: []bool{true, true, true}, mins: []any{nil, nil, nil}, maxs: []any{nil, nil, nil},
		arrays: [][]any{{"a", "b"}, nil, nil},
	}
	encoded, err := encodeLegacyAggregateContinuation(nil, "k", nil, gs, aggs)
	if err != nil {
		t.Fatal(err)
	}
	_, _, got, err := decodeAggregateContinuation(encoded, nil, aggs, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A group that saw rows but kept no element restores an empty, not absent, array.
	if fmt.Sprint(got.arrays[0]) != "[a b]" || got.arrays[2] == nil || len(got.arrays[2]) != 0 {
		t.Fatalf("arrays = %#v", got.arrays)
	}
	if got.sumsI[1] != 3 {
		t.Fatalf("sumsI = %v", got.sumsI)
	}
	// A continuation written without ARRAY_AGG slots is not one for this plan.
	legacy, err := encodeLegacyAggregateContinuation(nil, "k", nil, gs, aggs[1:2])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := decodeAggregateContinuation(legacy, nil, aggs, nil); err == nil {
		t.Fatal("decoded a continuation missing its ARRAY_AGG slots")
	}
}

func TestBitmapAggregateStreaming(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		positions []any
		length    int
		first     byte
		last      byte
		fail      bool
	}{
		{"bits", []any{int64(1), int64(2), int64(2), nil}, 1250, 6, 0, false},
		{"long narrows", []any{int64(1<<32) + 1}, 1250, 2, 0, false},
		{"extended", []any{int64(10000)}, 1251, 0, 1, false},
		{"maximum", []any{int64(249999)}, 31250, 0, 128, false},
		{"null", []any{nil}, 0, 0, 0, false},
		{"negative", []any{int64(-1)}, 0, 0, 0, true},
		{"too large", []any{int64(250000)}, 0, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			operand := &values.ConstantValue{Typ: values.NullableLong}
			c := &aggregateCursor{aggregates: []expressions.AggregateSpec{{Function: expressions.AggBitmapConstructAgg, Operand: operand}}}
			c.current = c.newGroupState()
			var err error
			for _, v := range tc.positions {
				operand.Value = v
				if err = c.accumulateRow(QueryResult{}); err != nil {
					break
				}
			}
			if tc.fail {
				if err == nil {
					t.Fatal("expected invalid bitmap position error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// Resume a partially accumulated group before producing its result.
			encoded, err := encodeAggregateContinuation(nil, nil, nil, c.current, c.aggregates)
			if err != nil {
				t.Fatal(err)
			}
			_, _, c.current, err = decodeAggregateContinuation(encoded, nil, c.aggregates, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := c.finalizeGroup().Positional.Slots[0]
			if tc.length == 0 {
				if got != nil {
					t.Fatalf("all-null aggregate = %v", got)
				}
				return
			}
			b, ok := got.([]byte)
			if !ok || len(b) != tc.length {
				t.Fatalf("bitmap type/length = %T/%v, want %d bytes", got, got, tc.length)
			}
			if b[0] != tc.first || b[len(b)-1] != tc.last {
				t.Fatalf("bitmap endpoints %d/%d", b[0], b[len(b)-1])
			}
		})
	}
}

func TestBitmapAggregateContinuationValidation(t *testing.T) {
	t.Parallel()
	operand := &values.ConstantValue{Typ: values.NullableLong, Value: int64(1)}
	aggs := []expressions.AggregateSpec{{Function: expressions.AggBitmapConstructAgg, Operand: operand}, {Function: expressions.AggArrayAgg, Operand: operand, Limit: values.ArrayAggNoLimit}}
	c := &aggregateCursor{aggregates: aggs}
	c.current = c.newGroupState()
	if err := c.accumulateRow(QueryResult{}); err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeAggregateContinuation(nil, nil, nil, c.current, aggs)
	if err != nil {
		t.Fatal(err)
	}
	var sent gen.AggregateCursorContinuation
	if err := proto.Unmarshal(encoded, &sent); err != nil {
		t.Fatal(err)
	}
	// Java writes BitSet.toByteArray(), which trims trailing zero bytes.
	if got := sent.GetPartialAggregationResults().GetAccumulatorStates()[0].GetState()[0].GetBytesState(); !bytes.Equal(got, []byte{2}) {
		t.Fatalf("bitmap state = %x, want Java's trimmed 02", got)
	}
	_, _, c.current, err = decodeAggregateContinuation(encoded, nil, aggs, nil)
	if err != nil {
		t.Fatal(err)
	}
	operand.Value = int64(9)
	if err := c.accumulateRow(QueryResult{}); err != nil {
		t.Fatal(err)
	}
	row := c.finalizeGroup().Positional.Slots
	bitmap := row[0].([]byte)
	if bitmap[0] != 2 || bitmap[1] != 2 || fmt.Sprint(row[1]) != "[1 9]" {
		t.Fatalf("resumed bitmap/array: %v", row)
	}
	for _, tc := range []struct {
		name  string
		state *gen.OneOfTypedState
	}{
		{"oversize", &gen.OneOfTypedState{State: &gen.OneOfTypedState_BytesState{BytesState: make([]byte, 31251)}}},
		{"wrong type", &gen.OneOfTypedState{State: &gen.OneOfTypedState_Int64State{Int64State: 2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var msg gen.AggregateCursorContinuation
			if err := proto.Unmarshal(encoded, &msg); err != nil {
				t.Fatal(err)
			}
			msg.PartialAggregationResults.AccumulatorStates[0].State[0] = tc.state
			bad, err := proto.Marshal(&msg)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := decodeAggregateContinuation(bad, nil, aggs, nil); err == nil {
				t.Fatal("accepted malformed bitmap state")
			}
		})
	}
}
