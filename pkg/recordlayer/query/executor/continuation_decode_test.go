package executor

// Regression tests for continuation deserialization in the executor — the
// same bug class as OrElse/Concat/FlatMapPipelined/Dedup in pkg/recordlayer
// (see continuation_parse_test.go there): a continuation token is external
// wire input, and corrupt content must produce an explicit error — never a
// silent restart (re-emitting consumed rows) and never a silently dropped
// buffered row or aggregate state (wrong results with no error).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

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
			_, _, err := decodeSortContinuation(tt.data(t))
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
	data, err := encodeSortContinuation(recordlayer.NewBytesContinuation([]byte("INNER")), buf)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	inner, got, err := decodeSortContinuation(data)
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
	); err == nil {
		f.Add(valid)
	}
	if sr, err := proto.Marshal(&gen.SortedRecord{Message: []byte("{not json")}); err == nil {
		if corrupt, err := proto.Marshal(&gen.MemorySortContinuation{Records: [][]byte{sr}}); err == nil {
			f.Add(corrupt)
		}
	}

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _, _ = decodeSortContinuation(data)
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
			GroupKey: encodeAggGroupKey("k", nil),
			AccumulatorStates: []*gen.AccumulatorState{{State: []*gen.OneOfTypedState{
				{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
				{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
				{State: &gen.OneOfTypedState_DoubleState{DoubleState: 1}},
				{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
				{State: &gen.OneOfTypedState_Int64State{Int64State: 1}},
				{State: &gen.OneOfTypedState_BytesState{BytesState: []byte{contValInt64}}},
				{State: &gen.OneOfTypedState_BytesState{BytesState: appendContValue(nil, int64(2))}},
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
				GroupKey: encodeAggGroupKey("k", []any{int64(1)}),
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
	validOne := appendContValue(nil, int64(1))
	validTwo := appendContValue(nil, int64(2))

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
