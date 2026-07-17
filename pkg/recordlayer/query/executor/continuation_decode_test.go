package executor

// Regression tests for continuation deserialization in the executor — the
// same bug class as OrElse/Concat/FlatMapPipelined/Dedup in pkg/recordlayer
// (see continuation_parse_test.go there): a continuation token is external
// wire input, and corrupt content must produce an explicit error — never a
// silent restart (re-emitting consumed rows) and never a silently dropped
// buffered row or aggregate state (wrong results with no error).

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// mustContValue / mustAggGroupKey wrap the now-erroring typed encoders for
// test call-sites that pass known-encodable values, failing the test on the
// (unexpected) encode error rather than dropping it.
func mustContValue(tb testing.TB, v any) []byte {
	tb.Helper()
	b, err := appendContValue(nil, v)
	if err != nil {
		tb.Fatalf("appendContValue(%#v): %v", v, err)
	}
	return b
}

func mustAggGroupKey(tb testing.TB, groupKey string, keyVals []any) []byte {
	tb.Helper()
	b, err := encodeAggGroupKey(groupKey, keyVals)
	if err != nil {
		tb.Fatalf("encodeAggGroupKey(%q, %#v): %v", groupKey, keyVals, err)
	}
	return b
}

// TestExecuteFlatMapCorruptContinuation pins that a FlatMap plan resume with
// unparseable continuation bytes errors (Java RecordCursor.flatMapPipelined:
// RecordCoreException("error parsing continuation")) instead of silently
// restarting the whole join from scratch. The parse happens before any plan or
// store access, so nils suffice.
func TestExecuteFlatMapCorruptContinuation(t *testing.T) {
	t.Parallel()

	corrupt := []byte{0xff, 0xff, 0xff}
	cursor, err := executeFlatMap(context.Background(), nil, nil, nil, corrupt, recordlayer.ExecuteProperties{})
	if cursor != nil {
		t.Errorf("cursor = %v, want nil on corrupt continuation", cursor)
	}
	if err == nil {
		t.Fatal("want error for corrupt continuation, got nil (silent restart is a wrong-results divergence)")
	}
	var parseErr *recordlayer.ContinuationParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("want *recordlayer.ContinuationParseError, got %T: %v", err, err)
	}
	if string(parseErr.RawBytes) != string(corrupt) {
		t.Errorf("RawBytes = %x, want %x", parseErr.RawBytes, corrupt)
	}
	if parseErr.Unwrap() == nil {
		t.Error("Unwrap() = nil, want wrapped unmarshal error")
	}
}

func TestDecodeSortContinuationCorruptRecords(t *testing.T) {
	t.Parallel()

	mustMarshal := func(t *testing.T, m proto.Message) []byte {
		t.Helper()
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	tests := []struct {
		name    string
		data    func(t *testing.T) []byte
		wantErr string
	}{
		{
			name:    "corrupt top-level proto",
			data:    func(_ *testing.T) []byte { return []byte{0xff, 0xff, 0xff} },
			wantErr: "failed to unmarshal sort continuation",
		},
		{
			name: "corrupt SortedRecord entry",
			data: func(t *testing.T) []byte {
				return mustMarshal(t, &gen.MemorySortContinuation{
					Records: [][]byte{{0xff, 0xff, 0xff}},
				})
			},
			wantErr: "failed to unmarshal sorted record 0",
		},
		{
			name: "SortedRecord with invalid JSON message",
			data: func(t *testing.T) []byte {
				sr := mustMarshal(t, &gen.SortedRecord{Message: []byte("{not json")})
				return mustMarshal(t, &gen.MemorySortContinuation{Records: [][]byte{sr}})
			},
			wantErr: "failed to unmarshal sorted record 0 message",
		},
		{
			name: "SortedRecord with corrupt primary key",
			data: func(t *testing.T) []byte {
				sr := mustMarshal(t, &gen.SortedRecord{
					Message:    []byte(`{"a":1}`),
					PrimaryKey: []byte{0xff}, // not a valid packed tuple
				})
				return mustMarshal(t, &gen.MemorySortContinuation{Records: [][]byte{sr}})
			},
			wantErr: "failed to unpack sorted record 0 primary key",
		},
		{
			// A 1-element array carries no positional (slot 2 absent) — pre-positional
			// / corrupt. Only a 3-element array [_, _, positional] is resumable; any
			// other arity is rejected by the positional==nil guard, not silently
			// mis-split (which would resume with wrong rows).
			name: "1-element array (no positional)",
			data: func(t *testing.T) []byte {
				sr := mustMarshal(t, &gen.SortedRecord{Message: []byte(`[{"a":1}]`)})
				return mustMarshal(t, &gen.MemorySortContinuation{Records: [][]byte{sr}})
			},
			wantErr: "no positional payload",
		},
		{
			// A 2-element v2 [_, complete] array is pre-positional (the retired
			// Complete slot, no positional). Slot 1 is ignored now; the row has no
			// reconstructable positional → rejected.
			name: "v2 2-element array (no positional)",
			data: func(t *testing.T) []byte {
				sr := mustMarshal(t, &gen.SortedRecord{Message: []byte(`[{"a":1}, null]`)})
				return mustMarshal(t, &gen.MemorySortContinuation{Records: [][]byte{sr}})
			},
			wantErr: "no positional payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := decodeSortContinuation(tt.data(t), nil)
			if err == nil {
				t.Fatal("want error, got nil (a silently dropped buffer row is wrong results with no error)")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Error() = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeSortContinuationRoundTrip(t *testing.T) {
	t.Parallel()

	buf := []QueryResult{
		dmap(map[string]any{"a": int64(1), "b": "x"}),
		dmap(map[string]any{"a": int64(2), "b": "y"}),
	}
	data, err := encodeSortContinuation(recordlayer.NewBytesContinuation([]byte("INNER")), buf, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	inner, got, _, err := decodeSortContinuation(data, nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(inner) != "INNER" {
		t.Errorf("inner = %q, want INNER", inner)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	d0, ok0 := rowMapOK(got[0])
	d1, ok1 := rowMapOK(got[1])
	if !ok0 || !ok1 {
		t.Fatalf("positional rows = %v, %v, want non-nil", got[0].Positional, got[1].Positional)
	}
	if d0["a"] != int64(1) || d1["b"] != "y" {
		t.Errorf("round-trip mismatch: %v", got)
	}
}

// FuzzSortContinuation fuzzes the sort continuation decoder. Must never
// panic; unparseable input must error (silently dropping a buffered record
// would be a wrong-results divergence, pinned above).
func FuzzSortContinuation(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff})
	if valid, err := encodeSortContinuation(
		recordlayer.NewBytesContinuation([]byte("INNER")),
		[]QueryResult{dmap(map[string]any{"a": int64(1)})},
		false,
	); err == nil {
		f.Add(valid)
	}
	// F53 seeds: ARRAY ([]any, tag 13) and STRUCT (proto.Message, tag 14) slots.
	if valid, err := encodeSortContinuation(
		recordlayer.NewBytesContinuation([]byte("INNER")),
		[]QueryResult{dmap(map[string]any{
			"arr":    []any{int64(1), "x", []any{nil}},
			"struct": &gen.Flower{Type: proto.String("rose")},
		})},
		false,
	); err == nil {
		f.Add(valid)
	}
	// F55 seed: a flag-1 (dynamicpb) STRUCT slot — the tag-14 representation
	// flag's other value.
	ghost, _ := unregisteredDynamicMessage(f)
	if valid, err := encodeSortContinuation(nil, []QueryResult{dmap(map[string]any{"g": ghost})}, false); err == nil {
		f.Add(valid)
	}
	if sr, err := proto.Marshal(&gen.SortedRecord{Message: []byte("{not json")}); err == nil {
		if corrupt, err := proto.Marshal(&gen.MemorySortContinuation{Records: [][]byte{sr}}); err == nil {
			f.Add(corrupt)
		}
	}

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _, _, _ = decodeSortContinuation(data, nil)
	})
}

// FuzzAggregateContinuation fuzzes the aggregate continuation decoder. Must
// never panic; corrupt group-key/MIN/MAX state must error (pinned above).
func FuzzAggregateContinuation(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff})
	if corruptKey, err := proto.Marshal(&gen.AggregateCursorContinuation{
		PartialAggregationResults: &gen.PartialAggregationResult{GroupKey: []byte("{not json")},
	}); err == nil {
		f.Add(corruptKey)
	}
	// A valid group key reaches the per-aggregate min/max decode with a corrupt
	// MIN typed value (int64 tag, no payload), exercising the deeper error path.
	if corruptMin, err := proto.Marshal(&gen.AggregateCursorContinuation{
		PartialAggregationResults: &gen.PartialAggregationResult{
			GroupKey: mustAggGroupKey(f, "k", nil),
			AccumulatorStates: []*gen.AccumulatorState{{State: []*gen.OneOfTypedState{
				{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
				{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
				{State: &gen.OneOfTypedState_DoubleState{DoubleState: 1}},
				{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
				{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
				{State: &gen.OneOfTypedState_BytesState{BytesState: []byte{contValInt64}}},
				{State: &gen.OneOfTypedState_BytesState{BytesState: mustContValue(f, int64(2))}},
			}}},
		},
	}); err == nil {
		f.Add(corruptMin)
	}

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _, _, _ = decodeAggregateContinuation(data, 1)
		_, _, _, _ = decodeAggregateContinuation(data, 3)
	})
}

// TestDecodeAggregateContinuationCorruptMinMax pins that corrupt bytes in a
// present MIN/MAX typed accumulator state errors instead of silently dropping the
// partial aggregate (which would return a wrong MIN/MAX on resume).
func TestDecodeAggregateContinuationCorruptMinMax(t *testing.T) {
	t.Parallel()

	// State layout (see encodeAggregateContinuation): count, then per
	// aggregate: count_i, sum_i, sumsI_i, allInt_i, min_i, max_i. min/max carry
	// one typed continuation value each (appendContValue).
	buildStates := func(minBytes, maxBytes []byte) []*gen.OneOfTypedState {
		return []*gen.OneOfTypedState{
			{State: &gen.OneOfTypedState_Int64State{Int64State: 3}},
			{State: &gen.OneOfTypedState_Int64State{Int64State: 3}},
			{State: &gen.OneOfTypedState_DoubleState{DoubleState: 1.5}},
			{State: &gen.OneOfTypedState_Int64State{Int64State: 6}},
			{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
			{State: &gen.OneOfTypedState_BytesState{BytesState: minBytes}},
			{State: &gen.OneOfTypedState_BytesState{BytesState: maxBytes}},
		}
	}
	build := func(t *testing.T, minBytes, maxBytes []byte) []byte {
		t.Helper()
		data, err := proto.Marshal(&gen.AggregateCursorContinuation{
			PartialAggregationResults: &gen.PartialAggregationResult{
				GroupKey: mustAggGroupKey(t, "k", []any{int64(1)}),
				AccumulatorStates: []*gen.AccumulatorState{
					{State: buildStates(minBytes, maxBytes)},
				},
			},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return data
	}
	// A typed value that is corrupt: the int64 tag with no 8-byte payload.
	corruptVal := []byte{contValInt64}
	validOne := mustContValue(t, int64(1))
	validTwo := mustContValue(t, int64(2))

	t.Run("corrupt group key errors", func(t *testing.T) {
		t.Parallel()
		data, err := proto.Marshal(&gen.AggregateCursorContinuation{
			PartialAggregationResults: &gen.PartialAggregationResult{
				GroupKey: []byte("{not json"),
			},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		_, _, _, decErr := decodeAggregateContinuation(data, 1)
		if decErr == nil {
			t.Fatal("want error, got nil (coercing a corrupt group key to a raw string resumes under a never-matching group)")
		}
		if !strings.Contains(decErr.Error(), "group key") {
			t.Errorf("Error() = %q, want mention of group key", decErr)
		}
	})

	t.Run("corrupt MIN state errors", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := decodeAggregateContinuation(build(t, corruptVal, validTwo), 1)
		if err == nil {
			t.Fatal("want error, got nil (silently dropped MIN state is a wrong aggregate)")
		}
		if !strings.Contains(err.Error(), "MIN state") {
			t.Errorf("Error() = %q, want mention of MIN state", err)
		}
	})

	t.Run("corrupt MAX state errors", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := decodeAggregateContinuation(build(t, validOne, corruptVal), 1)
		if err == nil {
			t.Fatal("want error, got nil (silently dropped MAX state is a wrong aggregate)")
		}
		if !strings.Contains(err.Error(), "MAX state") {
			t.Errorf("Error() = %q, want mention of MAX state", err)
		}
	})

	t.Run("valid states round-trip", func(t *testing.T) {
		t.Parallel()
		_, gk, gs, err := decodeAggregateContinuation(build(t, validOne, validTwo), 1)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if gk != "k" {
			t.Errorf("groupKey = %q, want k", gk)
		}
		if gs == nil || gs.mins[0] != int64(1) || gs.maxs[0] != int64(2) {
			t.Errorf("groupState = %+v, want mins[0]=1 maxs[0]=2", gs)
		}
	})
}

// TestContValue_Uint64BigIntTime_F34 pins that the typed codec round-trips the
// three types F34 added — uint64 (a reachable index-sourced key in (2^63, 2^64),
// tuple.decodeInt → uint64 → tupleElementToRowValue passthrough), *big.Int (an
// integer beyond the uint64 range), and time.Time — EXACTLY, keeping Go type and
// full precision. Before F34 these fell to the JSON fallback: uint64/int64>2^53
// rounded through float64, *big.Int marshalled to a lossy JSON number, and
// time.Time became an RFC3339 string (wrong type on resume).
func TestContValue_Uint64BigIntTime_F34(t *testing.T) {
	t.Parallel()

	bigOverUint64 := new(big.Int).Lsh(big.NewInt(1), 80) // 2^80, far beyond uint64
	bigOverUint64.Add(bigOverUint64, big.NewInt(7))
	negBig := new(big.Int).Neg(bigOverUint64)
	// A location-bearing, sub-second timestamp to catch any lossy encoding.
	tm := time.Date(2026, 7, 16, 3, 4, 5, 123456789, time.FixedZone("X", 5*3600))

	cases := []struct {
		name string
		in   any
		want func(got any) bool
	}{
		{"uint64_high_bit", uint64(1<<63 + 1), func(g any) bool { v, ok := g.(uint64); return ok && v == uint64(1<<63+1) }},
		{"uint64_max", uint64(1<<64 - 1), func(g any) bool { v, ok := g.(uint64); return ok && v == uint64(1<<64-1) }},
		{"bigint_over_uint64", bigOverUint64, func(g any) bool { v, ok := g.(*big.Int); return ok && v.Cmp(bigOverUint64) == 0 }},
		{"bigint_negative", negBig, func(g any) bool { v, ok := g.(*big.Int); return ok && v.Cmp(negBig) == 0 }},
		{"bigint_zero", big.NewInt(0), func(g any) bool { v, ok := g.(*big.Int); return ok && v.Sign() == 0 }},
		{"time", tm, func(g any) bool { v, ok := g.(time.Time); return ok && v.Equal(tm) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enc, err := appendContValue(nil, tc.in)
			if err != nil {
				t.Fatalf("appendContValue(%#v): %v", tc.in, err)
			}
			got, rest, err := readContValue(enc)
			if err != nil {
				t.Fatalf("readContValue: %v", err)
			}
			if len(rest) != 0 {
				t.Errorf("readContValue left %d trailing bytes, want 0", len(rest))
			}
			if !tc.want(got) {
				t.Errorf("round-trip of %#v = %#v (%T), want exact type + value", tc.in, got, got)
			}
		})
	}
}

// TestAggGroupKey_Uint64Reachable_F34 pins the PRODUCTION group-key path: a uint64
// group-key value (the reachable large-index-int case) survives
// encodeAggGroupKey → decodeAggGroupKey with its Go type and 64-bit precision.
// Before F34 it JSON-rounded through float64, so the resumed key column was wrong.
func TestAggGroupKey_Uint64Reachable_F34(t *testing.T) {
	t.Parallel()
	keyVals := []any{uint64(1<<63 + 5)}
	enc, err := encodeAggGroupKey("gk", keyVals)
	if err != nil {
		t.Fatalf("encodeAggGroupKey: %v", err)
	}
	_, got, err := decodeAggGroupKey(enc)
	if err != nil {
		t.Fatalf("decodeAggGroupKey: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("keyVals len = %d, want 1", len(got))
	}
	if v, ok := got[0].(uint64); !ok || v != uint64(1<<63+5) {
		t.Errorf("keyVals[0] = %#v (%T), want uint64(%d)", got[0], got[0], uint64(1<<63+5))
	}
}

// TestContValue_UnencodableErrors_F34 pins the correct-or-loud fallback: a type
// the codec cannot faithfully reconstruct (a map — []any and proto.Message
// gained lossless encodings in F53) is an ERROR on encode, never a silent JSON
// blob. Before F34 these were silently JSON-marshalled (no error) and resumed
// with a mistyped value — this test is revert-proof: reinstating the JSON
// fallback makes appendContValue return a nil error and fails every subtest.
func TestContValue_UnencodableErrors_F34(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   any
	}{
		{"map", map[string]any{"a": int64(1)}},
		{"list_with_map_element", []any{int64(1), map[string]any{"a": int64(1)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := appendContValue(nil, tc.in)
			if err == nil {
				t.Fatalf("appendContValue(%T) = nil error, want a loud error (a silent JSON blob resumes with a mistyped value)", tc.in)
			}
			if !strings.Contains(err.Error(), "cannot encode value of type") {
				t.Errorf("Error() = %q, want mention of unsupported continuation value", err)
			}
		})
	}
	// The whole aggregate encode must fail loudly too: an unencodable group-key
	// value propagates out of encodeAggregateContinuation, not a silent lossy blob.
	_, err := encodeAggGroupKey("k", []any{map[string]any{"a": int64(1)}})
	if err == nil {
		t.Fatal("encodeAggGroupKey with an unencodable keyVal = nil error, want a loud error")
	}
}

// TestSortContinuation_TypedSlotsPreserved_F33 pins that the sort continuation
// carries ORDER BY slots through the typed codec (F33), so a BYTES / DOUBLE /
// large-int / NULL sort key survives a page-straddling resume with its exact Go
// type and value. Before F33 the slots went through JSON: []byte → base64 string,
// float64(2.0) → int64(2) (the decode's integral-double coercion), int64>2^53
// rounded through float64 — corrupting the sort order and the output values. This
// test is revert-proof: reinstating the JSON slot payload flips the types.
func TestSortContinuation_TypedSlotsPreserved_F33(t *testing.T) {
	t.Parallel()

	bigInt := int64(1<<60 + 1) // not representable in float64
	names := []string{"k_bytes", "k_double", "k_bigint", "k_null"}
	slots := []any{[]byte{0xff}, float64(2.0), bigInt, nil}
	buf := []QueryResult{dorder(names, slots)}

	encoded, err := encodeSortContinuation(recordlayer.NewBytesContinuation([]byte("INNER")), buf, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	inner, got, _, err := decodeSortContinuation(encoded, nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(inner) != "INNER" {
		t.Errorf("inner = %q, want INNER", inner)
	}
	if len(got) != 1 || got[0].Positional == nil {
		t.Fatalf("decoded buf = %+v, want 1 positional row", got)
	}
	gotSlots := got[0].Positional.Slots
	if len(gotSlots) != 4 {
		t.Fatalf("slots len = %d, want 4", len(gotSlots))
	}
	if b, ok := gotSlots[0].([]byte); !ok || !bytes.Equal(b, []byte{0xff}) {
		t.Errorf("slots[0] = %#v (%T), want []byte{0xff} (not a base64 string)", gotSlots[0], gotSlots[0])
	}
	if f, ok := gotSlots[1].(float64); !ok || f != 2.0 {
		t.Errorf("slots[1] = %#v (%T), want float64(2.0) (not int64(2))", gotSlots[1], gotSlots[1])
	}
	if n, ok := gotSlots[2].(int64); !ok || n != bigInt {
		t.Errorf("slots[2] = %#v (%T), want int64(%d) exact (not rounded)", gotSlots[2], gotSlots[2], bigInt)
	}
	if gotSlots[3] != nil {
		t.Errorf("slots[3] = %#v, want nil", gotSlots[3])
	}
}

// TestSortContinuation_LegacyTypedSlotBlobRejected_F33 pins that a 3-element
// payload with the OLD {n, s:[JSON slots]} shape (no typed `b` blob) is REJECTED
// on decode — fail-closed — rather than silently decoded with the lossy JSON
// slots. This preserves the payload versioning: an old-binary continuation fails
// the resume loudly (the caller restarts) instead of resuming with wrong values.
func TestSortContinuation_LegacyTypedSlotBlobRejected_F33(t *testing.T) {
	t.Parallel()
	// The pre-F33 3-element form: [null, false, {"n":[...], "s":[...]}] — a `b`-less
	// positional payload.
	sr, err := proto.Marshal(&gen.SortedRecord{Message: []byte(`[null, false, {"n":["c"], "s":[1]}]`)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data, err := proto.Marshal(&gen.MemorySortContinuation{Records: [][]byte{sr}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, _, _, decErr := decodeSortContinuation(data, nil)
	if decErr == nil {
		t.Fatal("want error, got nil (a b-less legacy positional payload must fail-closed, not resume with lossy JSON slots)")
	}
	if !strings.Contains(decErr.Error(), "no typed slot blob") {
		t.Errorf("Error() = %q, want 'no typed slot blob' rejection", decErr)
	}
}

// ===========================================================================
// F53: ARRAY ([]any) and STRUCT (proto.Message) slots in the typed codec
// ===========================================================================

// unregisteredDynamicMessage builds a proto message whose type exists ONLY as
// a runtime descriptor (protodesc over a synthetic FileDescriptorProto) —
// never registered in protoregistry.GlobalTypes. This is the dynamic-schema
// shape whose restore must go through the metadata resolver → dynamicpb path
// (it encodes as a flag-1 tag-14 value: a *dynamicpb.Message); a generated
// gen.* message encodes flag 0 and restores from the registry without any
// resolver, so it cannot exercise the resolver-rejection paths.
func unregisteredDynamicMessage(tb testing.TB) (*dynamicpb.Message, protoreflect.MessageDescriptor) {
	tb.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("f54_unregistered.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("f54test"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Ghost"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("label"),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			}},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		tb.Fatalf("build synthetic file descriptor: %v", err)
	}
	md := fd.Messages().Get(0)
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("label"), protoreflect.ValueOfString("boo"))
	return m, md
}

// demoMetadataResolver builds the PRODUCTION descriptor resolver
// (metadataMessageResolver) over the demo schema's RecordMetaData — the same
// resolver executeInMemorySort hands decodeSortContinuation.
func demoMetadataResolver(tb testing.TB) protoDescriptorResolver {
	tb.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		tb.Fatalf("build metadata: %v", err)
	}
	return metadataMessageResolver(md)
}

// TestContValue_ListRoundTrip_F53 pins the []any (ARRAY column slot) encoding:
// a list of mixed scalars — including the bit-sensitive float64 -0.0, []byte,
// nil, and a NESTED list — round-trips bit-exact through the typed codec.
// Before F53 an ARRAY column slot in a buffered sort row failed the checkpoint
// encode loudly ("cannot encode value of type []any"), so `SELECT * ... ORDER
// BY` over a table with an ARRAY column could not resume at all — this test is
// revert-proof: removing the contValList arm fails it at encode.
func TestContValue_ListRoundTrip_F53(t *testing.T) {
	t.Parallel()

	negZero := math.Copysign(0, -1)
	in := []any{
		int64(1<<60 + 1), // not representable in float64
		float64(2.5),
		negZero,
		"s",
		[]byte{0x00, 0xff},
		nil,
		[]any{int64(7), "nested", nil},
	}
	enc, err := appendContValue(nil, in)
	if err != nil {
		t.Fatalf("appendContValue([]any): %v", err)
	}
	got, rest, err := readContValue(enc)
	if err != nil {
		t.Fatalf("readContValue: %v", err)
	}
	if len(rest) != 0 {
		t.Errorf("readContValue left %d trailing bytes, want 0", len(rest))
	}
	list, ok := got.([]any)
	if !ok {
		t.Fatalf("round-trip = %#v (%T), want []any", got, got)
	}
	if len(list) != len(in) {
		t.Fatalf("list len = %d, want %d", len(list), len(in))
	}
	if v, ok := list[0].(int64); !ok || v != int64(1<<60+1) {
		t.Errorf("list[0] = %#v (%T), want int64(%d) exact", list[0], list[0], int64(1<<60+1))
	}
	if v, ok := list[1].(float64); !ok || v != 2.5 {
		t.Errorf("list[1] = %#v (%T), want float64(2.5)", list[1], list[1])
	}
	if v, ok := list[2].(float64); !ok || v != 0 || !math.Signbit(v) {
		t.Errorf("list[2] = %#v (%T), want float64(-0.0) with sign bit preserved", list[2], list[2])
	}
	if v, ok := list[3].(string); !ok || v != "s" {
		t.Errorf("list[3] = %#v (%T), want \"s\"", list[3], list[3])
	}
	if v, ok := list[4].([]byte); !ok || !bytes.Equal(v, []byte{0x00, 0xff}) {
		t.Errorf("list[4] = %#v (%T), want []byte{00 ff} (not a base64 string)", list[4], list[4])
	}
	if list[5] != nil {
		t.Errorf("list[5] = %#v, want nil", list[5])
	}
	nested, ok := list[6].([]any)
	if !ok || len(nested) != 3 || nested[0] != int64(7) || nested[1] != "nested" || nested[2] != nil {
		t.Errorf("list[6] = %#v (%T), want nested []any{int64(7), \"nested\", nil}", list[6], list[6])
	}

	// An EMPTY list round-trips as an empty (zero-length) []any, not nil.
	encEmpty, err := appendContValue(nil, []any{})
	if err != nil {
		t.Fatalf("appendContValue([]any{}): %v", err)
	}
	gotEmpty, _, err := readContValue(encEmpty)
	if err != nil {
		t.Fatalf("readContValue(empty list): %v", err)
	}
	if l, ok := gotEmpty.([]any); !ok || len(l) != 0 {
		t.Errorf("empty list round-trip = %#v (%T), want zero-length []any", gotEmpty, gotEmpty)
	}
}

// TestSortContinuation_ArrayAndStructSlots_F53 pins the PRODUCTION sort
// checkpoint path end-to-end at the codec level: a buffered row whose slots
// hold an ARRAY column ([]any), a STRUCT column (a raw proto.Message), and an
// ARRAY-OF-STRUCT ([]any of proto.Message) survives encodeSortContinuation →
// decodeSortContinuation with the metadata resolver. Before F53 the encode
// failed loudly the moment such a buffer straddled a scan/time-limit boundary
// — a hard failure on `SELECT * FROM t ORDER BY id` over any table with an
// ARRAY or STRUCT column.
func TestSortContinuation_ArrayAndStructSlots_F53(t *testing.T) {
	t.Parallel()

	flower := &gen.Flower{Type: proto.String("rose"), Color: gen.Color_RED.Enum()}
	elem := &gen.Flower{Type: proto.String("lily"), Color: gen.Color_BLUE.Enum()}
	names := []string{"FLOWER", "TAGS", "STRUCT_ARR", "PRICE"}
	slots := []any{flower, []any{"a", "b"}, []any{elem}, int64(5)}
	buf := []QueryResult{dorder(names, slots)}

	encoded, err := encodeSortContinuation(recordlayer.NewBytesContinuation([]byte("INNER")), buf, false)
	if err != nil {
		t.Fatalf("encode: %v (the F53 bug: an ARRAY/STRUCT slot must checkpoint losslessly, not fail)", err)
	}
	inner, got, _, err := decodeSortContinuation(encoded, demoMetadataResolver(t))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(inner) != "INNER" {
		t.Errorf("inner = %q, want INNER", inner)
	}
	if len(got) != 1 || got[0].Positional == nil {
		t.Fatalf("decoded buf = %+v, want 1 positional row", got)
	}
	gotSlots := got[0].Positional.Slots
	if len(gotSlots) != 4 {
		t.Fatalf("slots len = %d, want 4", len(gotSlots))
	}

	// STRUCT slot: rebuilt as a proto message (a generated message encodes
	// flag 0 and restores from the registry; concrete-type identity is pinned
	// separately in TestSortContinuation_GeneratedTypeIdentity_F54),
	// field-for-field equal to the original.
	gotFlower, ok := gotSlots[0].(proto.Message)
	if !ok {
		t.Fatalf("slots[0] = %#v (%T), want a proto.Message", gotSlots[0], gotSlots[0])
	}
	if name := string(gotFlower.ProtoReflect().Descriptor().FullName()); name != "com.apple.foundationdb.record.Flower" {
		t.Errorf("slots[0] type = %q, want com.apple.foundationdb.record.Flower", name)
	}
	if !proto.Equal(gotFlower, flower) {
		t.Errorf("slots[0] = %v, want %v (field-for-field equal)", gotFlower, flower)
	}

	// ARRAY slot: []any of strings, exact.
	tags, ok := gotSlots[1].([]any)
	if !ok || len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Errorf("slots[1] = %#v (%T), want []any{\"a\", \"b\"}", gotSlots[1], gotSlots[1])
	}

	// ARRAY-OF-STRUCT slot: the nested message inside the list is rebuilt too
	// (resolvePendingProtoValues recurses through lists).
	arr, ok := gotSlots[2].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("slots[2] = %#v (%T), want a 1-element []any", gotSlots[2], gotSlots[2])
	}
	gotElem, ok := arr[0].(proto.Message)
	if !ok {
		t.Fatalf("slots[2][0] = %#v (%T), want a proto.Message", arr[0], arr[0])
	}
	if !proto.Equal(gotElem, elem) {
		t.Errorf("slots[2][0] = %v, want %v", gotElem, elem)
	}

	// Scalar slot unchanged.
	if v, ok := gotSlots[3].(int64); !ok || v != 5 {
		t.Errorf("slots[3] = %#v (%T), want int64(5)", gotSlots[3], gotSlots[3])
	}

	// SECOND checkpoint: a resumed buffer must re-encode — the next scan-limit
	// boundary checkpoints the SAME rows again. A restored message rides the
	// same proto.Message arm and re-encodes the same representation flag it
	// was restored as (generated → 0, dynamicpb → 1).
	reEncoded, err := encodeSortContinuation(nil, got, false)
	if err != nil {
		t.Fatalf("re-encode of a resumed buffer: %v", err)
	}
	_, got2, _, err := decodeSortContinuation(reEncoded, demoMetadataResolver(t))
	if err != nil {
		t.Fatalf("second decode: %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("second round-trip lost the row: %+v", got2)
	}
	if f2, ok := got2[0].Positional.Slots[0].(proto.Message); !ok || !proto.Equal(f2, flower) {
		t.Errorf("second round-trip slots[0] = %#v, want Flower equal to original", got2[0].Positional.Slots[0])
	}
}

// TestSortContinuation_StructSlot_NoResolverRejected_F53 pins the
// correct-or-loud half for FLAG-1 (dynamic) slots: a buffered dynamicpb
// STRUCT slot decoded WITHOUT a descriptor resolver (nil — e.g. a store-less
// unit path) is a LOUD error, never a silent placeholder or nil slot leaking
// into the row domain. The no-resolver rejection applies to flag-1 ONLY: a
// generated (flag-0) slot restores from protoregistry.GlobalTypes without any
// resolver (pinned in TestSortContinuation_GeneratedTypeIdentity_F54). The
// ghost here is a *dynamicpb.Message, so its tag-14 encoding carries flag 1
// (pinned in TestContProtoMessage_RepresentationFlag_F55) — this test
// meaningfully rejects a flag-1 token.
func TestSortContinuation_StructSlot_NoResolverRejected_F53(t *testing.T) {
	t.Parallel()

	ghost, _ := unregisteredDynamicMessage(t)
	buf := []QueryResult{dorder([]string{"GHOST"}, []any{ghost})}
	encoded, err := encodeSortContinuation(nil, buf, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, _, _, decErr := decodeSortContinuation(encoded, nil)
	if decErr == nil {
		t.Fatal("want error, got nil (a descriptor-less placeholder must never leak into the row domain)")
	}
	if !strings.Contains(decErr.Error(), "no descriptor resolver") {
		t.Errorf("Error() = %q, want 'no descriptor resolver' rejection", decErr)
	}
}

// TestSortContinuation_StructSlot_UnresolvableTypeRejected_F53 pins that a
// buffered dynamic (flag-1) STRUCT slot whose type name the store metadata
// does not carry (a schema drift between checkpoint and resume, or a crafted
// token) is a LOUD error — never a silent nil.
func TestSortContinuation_StructSlot_UnresolvableTypeRejected_F53(t *testing.T) {
	t.Parallel()

	// f54test.Ghost exists only as a runtime descriptor; it encodes flag 1
	// (dynamicpb), and the demo schema's metadata does not carry it
	// (resolver miss).
	ghost, _ := unregisteredDynamicMessage(t)
	buf := []QueryResult{dorder([]string{"X"}, []any{ghost})}
	encoded, err := encodeSortContinuation(nil, buf, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, _, _, decErr := decodeSortContinuation(encoded, demoMetadataResolver(t))
	if decErr == nil {
		t.Fatal("want error, got nil (an unresolvable struct type must fail the resume loudly)")
	}
	if !strings.Contains(decErr.Error(), "not found in the store's record metadata descriptors") {
		t.Errorf("Error() = %q, want 'not found in the store's record metadata descriptors'", decErr)
	}
}

// TestSortContinuation_DynamicSchemaFallback_F54 pins the flag-1 (dynamic)
// half of representation-tagged resolution: an UNREGISTERED type (runtime
// descriptor only) restores through the resolver → dynamicpb path, proto-equal
// to the original. Fresh rows on a dynamic schema are dynamicpb too, so
// concrete-type identity is consistent on this path as well.
func TestSortContinuation_DynamicSchemaFallback_F54(t *testing.T) {
	t.Parallel()

	ghost, md := unregisteredDynamicMessage(t)
	buf := []QueryResult{dorder([]string{"GHOST", "ID"}, []any{ghost, int64(1)})}
	encoded, err := encodeSortContinuation(nil, buf, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	resolve := func(fullName string) (protoreflect.MessageDescriptor, error) {
		if fullName != string(md.FullName()) {
			return nil, fmt.Errorf("unexpected descriptor lookup %q", fullName)
		}
		return md, nil
	}
	_, got, _, err := decodeSortContinuation(encoded, resolve)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Positional == nil {
		t.Fatalf("decoded buf = %+v, want 1 positional row", got)
	}
	gotGhost, ok := got[0].Positional.Slots[0].(*dynamicpb.Message)
	if !ok {
		t.Fatalf("slots[0] = %T, want *dynamicpb.Message (the dynamic-schema fallback — fresh rows there are dynamicpb too)", got[0].Positional.Slots[0])
	}
	if !proto.Equal(gotGhost, ghost) {
		t.Errorf("slots[0] = %v, want %v (field-for-field equal)", gotGhost, ghost)
	}
	if v, ok := got[0].Positional.Slots[1].(int64); !ok || v != 1 {
		t.Errorf("slots[1] = %#v, want int64(1)", got[0].Positional.Slots[1])
	}
}

// TestDecodeAggregateContinuation_ProtoPlaceholderRejected_F53 pins the
// aggregate-path guard: group keys and MIN/MAX partials are SCALARS — the
// aggregate encoder never writes a contValProtoMsg — so a tag-14 placeholder
// arriving through a crafted/corrupt continuation must error LOUDLY on decode,
// never leak the descriptor-less placeholder into the resumed group's output.
func TestDecodeAggregateContinuation_ProtoPlaceholderRejected_F53(t *testing.T) {
	t.Parallel()

	flower := &gen.Flower{Type: proto.String("rose")}
	flowerVal := mustContValue(t, flower)
	validOne := mustContValue(t, int64(1))

	buildStates := func(minBytes, maxBytes []byte) []*gen.OneOfTypedState {
		return []*gen.OneOfTypedState{
			{State: &gen.OneOfTypedState_Int64State{Int64State: 3}},
			{State: &gen.OneOfTypedState_Int64State{Int64State: 3}},
			{State: &gen.OneOfTypedState_DoubleState{DoubleState: 1.5}},
			{State: &gen.OneOfTypedState_Int64State{Int64State: 6}},
			{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
			{State: &gen.OneOfTypedState_BytesState{BytesState: minBytes}},
			{State: &gen.OneOfTypedState_BytesState{BytesState: maxBytes}},
		}
	}
	build := func(t *testing.T, groupKey []byte, minBytes, maxBytes []byte) []byte {
		t.Helper()
		data, err := proto.Marshal(&gen.AggregateCursorContinuation{
			PartialAggregationResults: &gen.PartialAggregationResult{
				GroupKey: groupKey,
				AccumulatorStates: []*gen.AccumulatorState{
					{State: buildStates(minBytes, maxBytes)},
				},
			},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return data
	}

	t.Run("proto group key rejected", func(t *testing.T) {
		t.Parallel()
		// The codec now ENCODES a proto message, so the crafted key assembles
		// cleanly — the decode-side guard is the only line of defense.
		gk := mustAggGroupKey(t, "k", []any{flower})
		_, _, _, err := decodeAggregateContinuation(build(t, gk, validOne, validOne), 1)
		if err == nil {
			t.Fatal("want error, got nil (a proto-message group key must be rejected — group keys are scalars)")
		}
		if !strings.Contains(err.Error(), "invalid group-key value") {
			t.Errorf("Error() = %q, want 'invalid group-key value' rejection", err)
		}
	})

	t.Run("proto in list group key rejected", func(t *testing.T) {
		t.Parallel()
		// The guard must recurse: a placeholder buried inside a tag-13 list.
		gk := mustAggGroupKey(t, "k", []any{[]any{flower}})
		_, _, _, err := decodeAggregateContinuation(build(t, gk, validOne, validOne), 1)
		if err == nil {
			t.Fatal("want error, got nil (a placeholder inside a list group key must be rejected)")
		}
		if !strings.Contains(err.Error(), "invalid group-key value") {
			t.Errorf("Error() = %q, want 'invalid group-key value' rejection", err)
		}
	})

	t.Run("proto MIN state rejected", func(t *testing.T) {
		t.Parallel()
		gk := mustAggGroupKey(t, "k", []any{int64(1)})
		_, _, _, err := decodeAggregateContinuation(build(t, gk, flowerVal, validOne), 1)
		if err == nil {
			t.Fatal("want error, got nil (a proto-message MIN partial must be rejected)")
		}
		if !strings.Contains(err.Error(), "invalid MIN state") {
			t.Errorf("Error() = %q, want 'invalid MIN state' rejection", err)
		}
	})

	t.Run("proto MAX state rejected", func(t *testing.T) {
		t.Parallel()
		gk := mustAggGroupKey(t, "k", []any{int64(1)})
		_, _, _, err := decodeAggregateContinuation(build(t, gk, validOne, flowerVal), 1)
		if err == nil {
			t.Fatal("want error, got nil (a proto-message MAX partial must be rejected)")
		}
		if !strings.Contains(err.Error(), "invalid MAX state") {
			t.Errorf("Error() = %q, want 'invalid MAX state' rejection", err)
		}
	})
}

// TestContValue_RetiredJSONTagRejected_F53 pins that tag 15 (the deleted
// contValJSON) stays PERMANENTLY RETIRED: decode rejects it as an unknown tag.
// F53 allocated 13 (list) and 14 (proto) and must never resurrect 15 — a legacy
// JSON payload has no lossless decode.
func TestContValue_RetiredJSONTagRejected_F53(t *testing.T) {
	t.Parallel()
	_, _, err := readContValue([]byte{15, 'x'})
	if err == nil {
		t.Fatal("want error for retired tag 15, got nil")
	}
	if !strings.Contains(err.Error(), "unknown typed-value tag 15") {
		t.Errorf("Error() = %q, want 'unknown typed-value tag 15'", err)
	}
}

// ===========================================================================
// F54/F55: representation-tagged STRUCT restore (concrete-type identity),
// decode nesting cap, MIN/MAX scalar-numeric gate
// ===========================================================================

// TestSortContinuation_GeneratedTypeIdentity_F54 pins that a buffered STRUCT
// slot encoded from a GENERATED message (flag 0) restores with the SAME
// concrete Go type a freshly-read row carries (*gen.Flower, not
// *dynamicpb.Message). The group/dedup keys (computeGroupKey, packedDedupKey,
// mergeSortCursor.extractKey) fall back to %T:%v for composite slots, so a
// restored slot with a different concrete type would NEVER key-match an equal
// fresh row: equal rows straddling a checkpoint split into duplicate groups /
// duplicate DISTINCT rows — wrong results, no error. A flag-0 slot resolves
// via protoregistry.GlobalTypes ONLY — even when a metadata resolver is
// present (which would hand back a dynamicpb message with a different %T);
// the flag-1 (dynamicpb) inverse is pinned in
// TestSortContinuation_DynamicMessageOfRegisteredType_F55.
func TestSortContinuation_GeneratedTypeIdentity_F54(t *testing.T) {
	t.Parallel()

	flower := &gen.Flower{Type: proto.String("rose"), Color: gen.Color_RED.Enum()}
	elem := &gen.Flower{Type: proto.String("lily"), Color: gen.Color_BLUE.Enum()}
	names := []string{"FLOWER", "STRUCT_ARR", "ID"}
	slots := []any{flower, []any{elem}, int64(1)}
	buf := []QueryResult{dorder(names, slots)}

	encoded, err := encodeSortContinuation(nil, buf, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	cases := []struct {
		name    string
		resolve func(t *testing.T) protoDescriptorResolver
	}{
		// The production path: a resolver is present, but a flag-0 (generated)
		// slot must still rebuild from the registry (the resolver would hand
		// back a dynamicpb message with a different %T).
		{"with metadata resolver", func(t *testing.T) protoDescriptorResolver { return demoMetadataResolver(t) }},
		// A flag-0 (generated) slot needs NO resolver at all — the no-resolver
		// rejection applies only to flag-1 (dynamicpb) slots.
		{"nil resolver, registry alone", func(_ *testing.T) protoDescriptorResolver { return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, got, _, decErr := decodeSortContinuation(encoded, tc.resolve(t))
			if decErr != nil {
				t.Fatalf("decode: %v (a registered struct type must restore from the registry)", decErr)
			}
			if len(got) != 1 || got[0].Positional == nil {
				t.Fatalf("decoded buf = %+v, want 1 positional row", got)
			}
			gotSlots := got[0].Positional.Slots

			gotFlower, ok := gotSlots[0].(*gen.Flower)
			if !ok {
				t.Fatalf("slots[0] = %T, want *gen.Flower — a dynamicpb restore breaks %%T-keyed group/dedup identity across the checkpoint", gotSlots[0])
			}
			if !proto.Equal(gotFlower, flower) {
				t.Errorf("slots[0] = %v, want %v", gotFlower, flower)
			}

			arr, ok := gotSlots[1].([]any)
			if !ok || len(arr) != 1 {
				t.Fatalf("slots[1] = %#v (%T), want a 1-element []any", gotSlots[1], gotSlots[1])
			}
			gotElem, ok := arr[0].(*gen.Flower)
			if !ok {
				t.Fatalf("slots[1][0] = %T, want *gen.Flower (identity must hold inside ARRAY-OF-STRUCT too)", arr[0])
			}
			if !proto.Equal(gotElem, elem) {
				t.Errorf("slots[1][0] = %v, want %v", gotElem, elem)
			}

			// The SYMPTOM pin: the restored row's dedup key must equal the
			// original row's — a %T divergence here is the duplicate-groups /
			// duplicate-DISTINCT-rows bug.
			if orig, restored := packedDedupKey(slots), packedDedupKey(gotSlots); orig != restored {
				t.Errorf("packedDedupKey diverged across the checkpoint:\n  original = %q\n  restored = %q\n(equal rows straddling a resume would split into duplicate groups/DISTINCT rows)", orig, restored)
			}
		})
	}
}

// TestContProtoMessage_RepresentationFlag_F55 pins the tag-14 WIRE LAYOUT:
// the byte after the tag is the representation flag — 0 for a generated
// message, 1 for a *dynamicpb.Message — and the flag follows the VALUE's
// concrete Go type, not whether its name is registered (a dynamicpb message
// built over a registered descriptor still encodes flag 1). The decode side
// picks the rebuild path from this byte alone; without it, one side of a
// checkpoint restores the wrong representation and %T-keyed group/dedup keys
// split.
func TestContProtoMessage_RepresentationFlag_F55(t *testing.T) {
	t.Parallel()

	// Generated message → flag 0.
	encGen := mustContValue(t, &gen.Flower{Type: proto.String("rose")})
	if encGen[0] != contValProtoMsg || encGen[1] != 0 {
		t.Errorf("generated message encodes [tag flag] = [%d %d], want [%d 0]", encGen[0], encGen[1], contValProtoMsg)
	}

	// Unregistered dynamicpb message → flag 1.
	ghost, _ := unregisteredDynamicMessage(t)
	encGhost := mustContValue(t, ghost)
	if encGhost[0] != contValProtoMsg || encGhost[1] != 1 {
		t.Errorf("dynamicpb message encodes [tag flag] = [%d %d], want [%d 1]", encGhost[0], encGhost[1], contValProtoMsg)
	}

	// A dynamicpb message over a REGISTERED descriptor → still flag 1: the
	// name cannot pick the representation, only the concrete type can.
	dyn := dynamicpb.NewMessage((&gen.Flower{}).ProtoReflect().Descriptor())
	encDyn := mustContValue(t, dyn)
	if encDyn[0] != contValProtoMsg || encDyn[1] != 1 {
		t.Errorf("dynamicpb-over-registered-descriptor encodes [tag flag] = [%d %d], want [%d 1]", encDyn[0], encDyn[1], contValProtoMsg)
	}
}

// TestSortContinuation_DynamicMessageOfRegisteredType_F55 pins the INVERSE of
// the F54 identity bug: a dynamic record schema that IMPORTS a compiled
// message type materializes fresh rows as *dynamicpb.Message even though the
// type's full name IS registered in protoregistry.GlobalTypes. The restore
// must rebuild the ENCODED representation (flag 1 → metadata resolver →
// dynamicpb), NOT registry-first: a registry rebuild would hand back
// *gen.Flower, and the %T-keyed group/dedup keys would never match an equal
// fresh dynamicpb row — equal rows straddling a checkpoint split into
// duplicate groups / duplicate DISTINCT rows (wrong results, no error).
func TestSortContinuation_DynamicMessageOfRegisteredType_F55(t *testing.T) {
	t.Parallel()

	// dynamicpb messages over the REGISTERED Flower descriptor — the shape a
	// dynamic schema importing the compiled demo file materializes.
	flowerDesc := (&gen.Flower{}).ProtoReflect().Descriptor()
	newDynFlower := func(typ string) *dynamicpb.Message {
		m := dynamicpb.NewMessage(flowerDesc)
		m.Set(flowerDesc.Fields().ByName("type"), protoreflect.ValueOfString(typ))
		return m
	}
	dyn := newDynFlower("rose")
	elem := newDynFlower("lily")
	slots := []any{dyn, []any{elem}, int64(1)}
	buf := []QueryResult{dorder([]string{"FLOWER", "STRUCT_ARR", "ID"}, slots)}

	encoded, err := encodeSortContinuation(nil, buf, false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	t.Run("restores dynamicpb via the metadata resolver", func(t *testing.T) {
		t.Parallel()
		// The demo metadata carries Flower, so the PRODUCTION resolver finds
		// it — and the registry carries it too, so a registry-first decode
		// would wrongly restore *gen.Flower.
		_, got, _, decErr := decodeSortContinuation(encoded, demoMetadataResolver(t))
		if decErr != nil {
			t.Fatalf("decode: %v", decErr)
		}
		if len(got) != 1 || got[0].Positional == nil {
			t.Fatalf("decoded buf = %+v, want 1 positional row", got)
		}
		gotSlots := got[0].Positional.Slots

		gotDyn, ok := gotSlots[0].(*dynamicpb.Message)
		if !ok {
			t.Fatalf("slots[0] = %T, want *dynamicpb.Message — a registry (generated-type) restore breaks %%T-keyed group/dedup identity with fresh dynamicpb rows", gotSlots[0])
		}
		if !proto.Equal(gotDyn, dyn) {
			t.Errorf("slots[0] = %v, want %v (field-for-field equal)", gotDyn, dyn)
		}

		arr, ok := gotSlots[1].([]any)
		if !ok || len(arr) != 1 {
			t.Fatalf("slots[1] = %#v (%T), want a 1-element []any", gotSlots[1], gotSlots[1])
		}
		gotElem, ok := arr[0].(*dynamicpb.Message)
		if !ok {
			t.Fatalf("slots[1][0] = %T, want *dynamicpb.Message (representation identity must hold inside ARRAY-OF-STRUCT too)", arr[0])
		}
		if !proto.Equal(gotElem, elem) {
			t.Errorf("slots[1][0] = %v, want %v", gotElem, elem)
		}

		// The SYMPTOM pin: the restored row's dedup key must equal the
		// original dynamicpb row's — a %T divergence here is the
		// duplicate-groups / duplicate-DISTINCT-rows bug.
		if orig, restored := packedDedupKey(slots), packedDedupKey(gotSlots); orig != restored {
			t.Errorf("packedDedupKey diverged across the checkpoint:\n  original = %q\n  restored = %q\n(equal dynamicpb rows straddling a resume would split into duplicate groups/DISTINCT rows)", orig, restored)
		}
	})

	t.Run("nil resolver rejected — no registry fallback", func(t *testing.T) {
		t.Parallel()
		// A flag-1 slot must NOT launder through the registry just because its
		// name is registered: that restores the wrong representation — the
		// same split, from the other side.
		_, _, _, decErr := decodeSortContinuation(encoded, nil)
		if decErr == nil {
			t.Fatal("want error, got nil (a flag-1 slot with no resolver must fail loudly, never fall back to the registry)")
		}
		if !strings.Contains(decErr.Error(), "no descriptor resolver") {
			t.Errorf("Error() = %q, want 'no descriptor resolver' rejection", decErr)
		}
	})
}

// TestResolvePendingProto_GeneratedUnregisteredNoFallback_F55 pins the flag-0
// half of "never across representations": a flag-0 (generated) token whose
// type name is NOT registered in this binary must fail LOUDLY even when the
// metadata resolver COULD supply the descriptor — a resolver → dynamicpb
// rebuild would restore a *dynamicpb.Message where the encoder saw a generated
// message, recreating the representation split. (Unreachable from a genuine
// encode — a generated value implies a registered type — so this is the
// crafted/schema-drift arm.)
func TestResolvePendingProto_GeneratedUnregisteredNoFallback_F55(t *testing.T) {
	t.Parallel()

	ghost, md := unregisteredDynamicMessage(t)
	payload, err := proto.Marshal(ghost)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	name := string(md.FullName())
	// Craft the tag-14 token by hand with flag 0 (generated): the encoder
	// would write flag 1 for this dynamicpb value.
	tok := []byte{contValProtoMsg, 0x00}
	tok = binary.AppendUvarint(tok, uint64(len(name)))
	tok = append(tok, name...)
	tok = binary.AppendUvarint(tok, uint64(len(payload)))
	tok = append(tok, payload...)

	v, rest, err := readContValue(tok)
	if err != nil {
		t.Fatalf("readContValue: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("trailing bytes = %d, want 0", len(rest))
	}
	// A resolver that COULD resolve the name — resolution must not use it.
	resolve := func(fullName string) (protoreflect.MessageDescriptor, error) {
		if fullName != name {
			return nil, fmt.Errorf("unexpected descriptor lookup %q", fullName)
		}
		return md, nil
	}
	_, rErr := resolvePendingProtoValues(v, resolve)
	if rErr == nil {
		t.Fatal("want error, got nil (a flag-0 token with an unregistered name must fail loudly, never fall back to the resolver + dynamicpb)")
	}
	if !strings.Contains(rErr.Error(), "not registered") {
		t.Errorf("Error() = %q, want 'not registered' rejection", rErr)
	}
}

// TestContValue_NestingDepthCapped_F54 pins the decode-side recursion budget:
// a continuation token is EXTERNAL wire input, and a crafted token of deeply
// nested one-element lists (tag 13) costs one stack frame per two payload
// bytes — without a cap a small token overflows the stack (an unrecoverable
// runtime crash, not an error). Deep nesting must be a LOUD decode error;
// reasonable nesting must keep round-tripping.
func TestContValue_NestingDepthCapped_F54(t *testing.T) {
	t.Parallel()

	// levels nested single-element lists around one nil: [13 01] × levels + [00].
	deep := func(levels int) []byte {
		return append(bytes.Repeat([]byte{contValList, 0x01}, levels), contValNil)
	}

	t.Run("65-deep crafted token rejected", func(t *testing.T) {
		t.Parallel()
		_, _, err := readContValue(deep(65))
		if err == nil {
			t.Fatal("want error for a 65-deep nested-list token, got nil (unbounded recursion — a longer token is a stack overflow, not an error)")
		}
		if !strings.Contains(err.Error(), "nesting") {
			t.Errorf("Error() = %q, want a nesting-depth rejection", err)
		}
	})

	t.Run("64-deep boundary still decodes", func(t *testing.T) {
		t.Parallel()
		v, rest, err := readContValue(deep(64))
		if err != nil {
			t.Fatalf("readContValue(64-deep) = %v, want the boundary to decode (the cap must reject only BEYOND the budget)", err)
		}
		if len(rest) != 0 {
			t.Errorf("trailing bytes = %d, want 0", len(rest))
		}
		if _, ok := v.([]any); !ok {
			t.Errorf("decoded = %T, want []any", v)
		}
	})

	t.Run("depth-3 list round-trips", func(t *testing.T) {
		t.Parallel()
		in := []any{[]any{[]any{int64(7), "x"}, nil}, int64(1)}
		enc, err := appendContValue(nil, in)
		if err != nil {
			t.Fatalf("appendContValue: %v", err)
		}
		got, rest, err := readContValue(enc)
		if err != nil {
			t.Fatalf("readContValue: %v", err)
		}
		if len(rest) != 0 {
			t.Errorf("trailing bytes = %d, want 0", len(rest))
		}
		l1, ok := got.([]any)
		if !ok || len(l1) != 2 {
			t.Fatalf("round-trip = %#v, want 2-element []any", got)
		}
		l2, ok := l1[0].([]any)
		if !ok || len(l2) != 2 || l2[1] != nil {
			t.Fatalf("level 2 = %#v, want []any{[]any{...}, nil}", l1[0])
		}
		l3, ok := l2[0].([]any)
		if !ok || len(l3) != 2 || l3[0] != int64(7) || l3[1] != "x" {
			t.Fatalf("level 3 = %#v, want []any{int64(7), \"x\"}", l2[0])
		}
	})

	t.Run("placeholder-resolution walk capped", func(t *testing.T) {
		t.Parallel()
		// resolvePendingProtoValues recurses through lists the same way; its
		// walk needs its own budget (defense in depth — its input is decoded
		// values, but the cap must not depend on the caller's).
		v := any(nil)
		for i := 0; i < 70; i++ {
			v = []any{v}
		}
		if _, err := resolvePendingProtoValues(v, nil); err == nil {
			t.Fatal("want error resolving a 70-deep nested list, got nil (unbounded recursion)")
		}
	})
}

// TestDecodeAggregateContinuation_NonScalarMinMaxRejected_F54 pins the MIN/MAX
// decode gate: the accumulator (accumulateRow's isNumeric gate + aggMinMax) can
// only ever produce int64/int32/int/float64/float32 — or nil for "no rows yet"
// — as an extremum. A decoded MIN/MAX of any OTHER shape (a tag-13 []any, a
// string, a proto placeholder) is a crafted or corrupt continuation and must
// error LOUDLY, never seed the fold with a value aggMinMax cannot compare.
func TestDecodeAggregateContinuation_NonScalarMinMaxRejected_F54(t *testing.T) {
	t.Parallel()

	buildStates := func(minBytes, maxBytes []byte) []*gen.OneOfTypedState {
		return []*gen.OneOfTypedState{
			{State: &gen.OneOfTypedState_Int64State{Int64State: 3}},
			{State: &gen.OneOfTypedState_Int64State{Int64State: 3}},
			{State: &gen.OneOfTypedState_DoubleState{DoubleState: 1.5}},
			{State: &gen.OneOfTypedState_Int64State{Int64State: 6}},
			{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
			{State: &gen.OneOfTypedState_BytesState{BytesState: minBytes}},
			{State: &gen.OneOfTypedState_BytesState{BytesState: maxBytes}},
		}
	}
	build := func(t *testing.T, minBytes, maxBytes []byte) []byte {
		t.Helper()
		data, err := proto.Marshal(&gen.AggregateCursorContinuation{
			PartialAggregationResults: &gen.PartialAggregationResult{
				GroupKey: mustAggGroupKey(t, "k", []any{int64(1)}),
				AccumulatorStates: []*gen.AccumulatorState{
					{State: buildStates(minBytes, maxBytes)},
				},
			},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return data
	}

	validOne := mustContValue(t, int64(1))
	listVal := mustContValue(t, []any{int64(1)})
	strVal := mustContValue(t, "z")

	t.Run("list MIN state rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := decodeAggregateContinuation(build(t, listVal, validOne), 1)
		if err == nil {
			t.Fatal("want error, got nil (a []any MIN partial is crafted/corrupt — the accumulator only produces numerics)")
		}
		if !strings.Contains(err.Error(), "invalid MIN state") {
			t.Errorf("Error() = %q, want 'invalid MIN state' rejection", err)
		}
	})

	t.Run("list MAX state rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := decodeAggregateContinuation(build(t, validOne, listVal), 1)
		if err == nil {
			t.Fatal("want error, got nil (a []any MAX partial is crafted/corrupt)")
		}
		if !strings.Contains(err.Error(), "invalid MAX state") {
			t.Errorf("Error() = %q, want 'invalid MAX state' rejection", err)
		}
	})

	t.Run("string MIN state rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := decodeAggregateContinuation(build(t, strVal, validOne), 1)
		if err == nil {
			t.Fatal("want error, got nil (Go MIN/MAX is numeric-only — accumulateRow raises AggregateTypeMismatchError for non-numerics, so no genuine continuation carries a string extremum)")
		}
		if !strings.Contains(err.Error(), "invalid MIN state") {
			t.Errorf("Error() = %q, want 'invalid MIN state' rejection", err)
		}
	})

	t.Run("every accumulator-producible width accepted", func(t *testing.T) {
		t.Parallel()
		for _, v := range []any{int64(1), int32(2), int(3), float64(4.5), float32(5.5), nil} {
			data := build(t, mustContValue(t, v), mustContValue(t, v))
			_, _, gs, err := decodeAggregateContinuation(data, 1)
			if err != nil {
				t.Fatalf("decode with %T extremum: %v (the gate must accept every width the accumulator can produce)", v, err)
			}
			if gs == nil || gs.mins[0] != v || gs.maxs[0] != v {
				t.Errorf("extremum %#v (%T) did not round-trip: mins[0]=%#v maxs[0]=%#v", v, v, gs.mins[0], gs.maxs[0])
			}
		}
	})
}

// TestContValue_CorruptListAndProto_F53 pins that truncated/corrupt tag-13/14
// payloads error (never panic, never a silent wrong value).
func TestContValue_CorruptListAndProto_F53(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []byte
	}{
		{"list_count_exceeds_bytes", []byte{contValList, 0x05}},
		{"list_truncated_element", []byte{contValList, 0x01, contValInt64}},
		{"proto_no_flag", []byte{contValProtoMsg}},
		{"proto_bad_flag", []byte{contValProtoMsg, 0x02, 0x01, 'a', 0x00}},
		{"proto_no_name", []byte{contValProtoMsg, 0x00}},
		{"proto_truncated_name", []byte{contValProtoMsg, 0x00, 0x05, 'a'}},
		{"proto_no_payload_len", []byte{contValProtoMsg, 0x01, 0x01, 'a'}},
		{"proto_truncated_payload", []byte{contValProtoMsg, 0x00, 0x01, 'a', 0x05, 0x01}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := readContValue(tc.in); err == nil {
				t.Fatalf("readContValue(%x) = nil error, want corrupt-payload rejection", tc.in)
			}
		})
	}
}
