package recordlayer

// Regression tests for continuation deserialization in ConcatCursors,
// FlatMapPipelined(WithCheck), Dedup, and the vector index scan cursors —
// the same bug class as OrElseWithContinuation (orelse_continuation_test.go):
// a continuation token is external wire input, and any byte sequence must
// produce either a working cursor or an explicit error — never a silent
// restart from scratch, which re-emits rows the caller already consumed
// (a wrong-results divergence). Java references:
//   - ConcatCursor.java (constructor): RecordCoreException("Error parsing
//     ConcatCursor continuation")
//   - RecordCursor.java flatMapPipelined: RecordCoreException("error parsing
//     continuation")
//   - DedupCursor.java (constructor): RecordCoreException("Error parsing
//     continuation")
//   - VectorIndexMaintainer.java Continuation.fromBytes: RecordCoreException
//     ("error parsing continuation")

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// contPos is a FromListWithContinuation continuation: a 4-byte big-endian
// position (Java ListCursor format).
func contPos(p int) []byte { return []byte{0x00, 0x00, 0x00, byte(p)} }

// requireContinuationParseError asserts err is a *ContinuationParseError with
// the given Java wording and raw bytes, and that it wraps an unmarshal cause.
func requireContinuationParseError(t *testing.T, err error, wantMessage string, wantRaw []byte) {
	t.Helper()
	var parseErr *ContinuationParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("want *ContinuationParseError, got %T: %v", err, err)
	}
	if !strings.HasPrefix(parseErr.Error(), wantMessage+" (raw_bytes=") {
		t.Errorf("Error() = %q, want prefix %q (Java's RecordCoreException wording)", parseErr.Error(), wantMessage)
	}
	if string(parseErr.RawBytes) != string(wantRaw) {
		t.Errorf("RawBytes = %x, want %x", parseErr.RawBytes, wantRaw)
	}
	if parseErr.Unwrap() == nil {
		t.Error("Unwrap() = nil, want wrapped unmarshal error")
	}
}

// requireLatched asserts a cursor keeps failing with the same error and that
// Close still works.
func requireLatched(t *testing.T, ctx context.Context, cursor interface {
	OnNext(context.Context) (RecordCursorResult[int], error)
	Close() error
}, first error,
) {
	t.Helper()
	_, err2 := cursor.OnNext(ctx)
	if err2 == nil {
		t.Fatal("second OnNext: want latched error, got nil")
	}
	if first.Error() != err2.Error() {
		t.Errorf("error not latched: first %q, second %q", first, err2)
	}
	if err := cursor.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ConcatCursors
// ---------------------------------------------------------------------------

func TestConcatContinuationInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	corrupt := []byte{0xff, 0xff, 0xff}

	firstCalled, secondCalled := false, false
	cursor := ConcatCursors(
		func(_ []byte) RecordCursor[int] { firstCalled = true; return FromList([]int{1, 2}) },
		func(_ []byte) RecordCursor[int] { secondCalled = true; return FromList([]int{3}) },
		corrupt,
	)

	_, err := cursor.OnNext(ctx)
	if err == nil {
		t.Fatal("OnNext: want error for corrupt continuation, got nil (silent restart is a wrong-results divergence)")
	}
	// Java: ConcatCursor.java constructor —
	// RecordCoreException("Error parsing ConcatCursor continuation").
	requireContinuationParseError(t, err, "Error parsing ConcatCursor continuation", corrupt)
	requireLatched(t, ctx, cursor, err)

	if firstCalled || secondCalled {
		t.Errorf("no inner cursor may be built for an invalid continuation (first=%v second=%v)", firstCalled, secondCalled)
	}
}

func TestConcatContinuationRoundTrips(t *testing.T) {
	t.Parallel()

	first := func(cont []byte) RecordCursor[int] { return FromListWithContinuation([]int{1, 2}, cont) }
	second := func(cont []byte) RecordCursor[int] { return FromListWithContinuation([]int{3, 4}, cont) }

	tests := []struct {
		name         string
		continuation *gen.ConcatContinuation
		want         []int
	}{
		{
			name:         "resume mid-first",
			continuation: &gen.ConcatContinuation{Second: proto.Bool(false), Continuation: contPos(1)},
			want:         []int{2, 3, 4},
		},
		{
			name:         "resume mid-second",
			continuation: &gen.ConcatContinuation{Second: proto.Bool(true), Continuation: contPos(1)},
			want:         []int{4},
		},
		{
			name:         "resume on second from its start",
			continuation: &gen.ConcatContinuation{Second: proto.Bool(true)},
			want:         []int{3, 4},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := tt.continuation.MarshalVT()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := AsList(context.Background(), ConcatCursors(first, second, raw))
			if err != nil {
				t.Fatalf("AsList: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// FlatMapPipelined / FlatMapPipelinedWithCheck
// ---------------------------------------------------------------------------

func TestFlatMapContinuationInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	corrupt := []byte{0xff, 0xff, 0xff}

	outerCalled, innerCalled := false, false
	cursor := FlatMapPipelined(
		func(_ []byte) RecordCursor[int] { outerCalled = true; return FromList([]int{1, 2}) },
		func(outer int, _ []byte) RecordCursor[int] { innerCalled = true; return FromList([]int{outer * 10}) },
		corrupt,
		1,
	)

	_, err := cursor.OnNext(ctx)
	if err == nil {
		t.Fatal("OnNext: want error for corrupt continuation, got nil (silent restart is a wrong-results divergence)")
	}
	// Java: RecordCursor.flatMapPipelined —
	// RecordCoreException("error parsing continuation").
	requireContinuationParseError(t, err, "error parsing continuation", corrupt)
	requireLatched(t, ctx, cursor, err)

	if outerCalled || innerCalled {
		t.Errorf("no inner cursor may be built for an invalid continuation (outer=%v inner=%v)", outerCalled, innerCalled)
	}
}

func TestFlatMapContinuationRoundTrips(t *testing.T) {
	t.Parallel()

	outer := func(cont []byte) RecordCursor[int] { return FromListWithContinuation([]int{1, 2}, cont) }
	inner := func(v int, cont []byte) RecordCursor[int] {
		return FromListWithContinuation([]int{v * 10, v*10 + 1}, cont)
	}

	tests := []struct {
		name         string
		continuation *gen.FlatMapContinuation
		want         []int
	}{
		{
			name:         "outer-only resume skips consumed outer rows",
			continuation: &gen.FlatMapContinuation{OuterContinuation: contPos(1)},
			want:         []int{20, 21},
		},
		{
			name: "outer+inner resume replays mid-inner",
			continuation: &gen.FlatMapContinuation{
				OuterContinuation: contPos(0),
				InnerContinuation: contPos(1),
			},
			want: []int{11, 20, 21},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := tt.continuation.MarshalVT()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := AsList(context.Background(), FlatMapPipelined(outer, inner, raw, 1))
			if err != nil {
				t.Fatalf("AsList: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Dedup
// ---------------------------------------------------------------------------

func dedupIntFuncs() (equal func(a, b int) bool, pack func(int) []byte, unpack func([]byte) (int, bool)) {
	equal = func(a, b int) bool { return a == b }
	pack = func(v int) []byte { return []byte{byte(v)} }
	unpack = func(b []byte) (int, bool) {
		if len(b) == 1 {
			return int(b[0]), true
		}
		return 0, false
	}
	return equal, pack, unpack
}

func TestDedupContinuationInvalid(t *testing.T) {
	t.Parallel()

	equal, pack, unpack := dedupIntFuncs()

	marshalDedup := func(t *testing.T, cont *gen.DedupContinuation) []byte {
		t.Helper()
		raw, err := cont.MarshalVT()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return raw
	}

	tests := []struct {
		name         string
		continuation func(t *testing.T) []byte
		unpack       func([]byte) (int, bool)
		checkErr     func(t *testing.T, err error, raw []byte)
	}{
		{
			name:         "corrupt bytes fail with ContinuationParseError",
			continuation: func(_ *testing.T) []byte { return []byte{0xff, 0xff, 0xff} },
			unpack:       unpack,
			checkErr: func(t *testing.T, err error, raw []byte) {
				// Java: DedupCursor.java constructor —
				// RecordCoreException("Error parsing continuation").
				requireContinuationParseError(t, err, "Error parsing continuation", raw)
			},
		},
		{
			name: "lastValue that fails to unpack is an error, not dropped dedup state",
			continuation: func(t *testing.T) []byte {
				// 2-byte lastValue: unpack only accepts exactly 1 byte.
				return marshalDedup(t, &gen.DedupContinuation{
					InnerContinuation: contPos(0),
					LastValue:         []byte{0x01, 0x02},
				})
			},
			unpack: unpack,
			checkErr: func(t *testing.T, err error, _ []byte) {
				// Dropping the state instead would re-emit the last value as a
				// duplicate on resume. Java propagates the unpackValue failure
				// out of the constructor.
				if !strings.Contains(err.Error(), "unpack lastValue failed") {
					t.Errorf("Error() = %q, want mention of failed lastValue unpack", err)
				}
			},
		},
		{
			name: "lastValue with nil unpack function is an error",
			continuation: func(t *testing.T) []byte {
				return marshalDedup(t, &gen.DedupContinuation{
					InnerContinuation: contPos(0),
					LastValue:         []byte{0x01},
				})
			},
			unpack: nil,
			checkErr: func(t *testing.T, err error, _ []byte) {
				if !strings.Contains(err.Error(), "no unpack function") {
					t.Errorf("Error() = %q, want mention of missing unpack function", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			raw := tt.continuation(t)

			innerCalled := false
			cursor := Dedup(
				func(_ []byte) RecordCursor[int] { innerCalled = true; return FromList([]int{1, 1, 2}) },
				equal, pack, tt.unpack, raw,
			)

			_, err := cursor.OnNext(ctx)
			if err == nil {
				t.Fatal("OnNext: want error for invalid continuation, got nil (silent restart is a wrong-results divergence)")
			}
			tt.checkErr(t, err, raw)
			requireLatched(t, ctx, cursor, err)

			if innerCalled {
				t.Error("no inner cursor may be built for an invalid continuation")
			}
		})
	}
}

func TestDedupContinuationRoundTrips(t *testing.T) {
	t.Parallel()

	equal, pack, unpack := dedupIntFuncs()
	items := []int{1, 1, 2, 2, 3}
	factory := func(cont []byte) RecordCursor[int] { return FromListWithContinuation(items, cont) }

	tests := []struct {
		name         string
		continuation *gen.DedupContinuation
		want         []int
	}{
		{
			// lastValue=2 restored → the 2s at positions 2,3 are duplicates.
			name:         "lastValue restores dedup state across resume",
			continuation: &gen.DedupContinuation{InnerContinuation: contPos(2), LastValue: []byte{0x02}},
			want:         []int{3},
		},
		{
			name:         "resume without lastValue emits the next distinct run",
			continuation: &gen.DedupContinuation{InnerContinuation: contPos(2)},
			want:         []int{2, 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := tt.continuation.MarshalVT()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := AsList(context.Background(), Dedup(factory, equal, pack, unpack, raw))
			if err != nil {
				t.Fatalf("AsList: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Vector index multi-partition cursor (FlatMapContinuation wrapper)
// ---------------------------------------------------------------------------

func TestVectorMultiPartitionContinuationInvalid(t *testing.T) {
	t.Parallel()

	// The constructor validates the continuation before touching FDB, so a
	// corrupt token is fully testable without a transaction: the returned
	// cursor is an errorCursor whose OnNext never reaches the skip-scan.
	m := &vectorIndexMaintainer{
		standardIndexMaintainer: standardIndexMaintainer{index: &Index{Name: "vec_mp"}},
		hnswSubspace:            subspace.Sub("continuation-parse-test"),
		hnswConfig:              HNSWConfig{NumDimensions: 3},
	}

	tests := []struct {
		name         string
		continuation func(t *testing.T) []byte
		checkErr     func(t *testing.T, err error, raw []byte)
	}{
		{
			name:         "corrupt bytes fail with ContinuationParseError",
			continuation: func(_ *testing.T) []byte { return []byte{0xff, 0xff, 0xff} },
			checkErr: func(t *testing.T, err error, raw []byte) {
				// Java analog (RecordCursor.flatMapPipelined over the partition
				// skip-scan): RecordCoreException("error parsing continuation").
				requireContinuationParseError(t, err, "error parsing continuation", raw)
			},
		},
		{
			name: "corrupt outer partition prefix is an error",
			continuation: func(t *testing.T) []byte {
				raw, err := (&gen.FlatMapContinuation{
					OuterContinuation: []byte{0xff}, // not a valid packed tuple
					InnerContinuation: contPos(0),
				}).MarshalVT()
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				return raw
			},
			checkErr: func(t *testing.T, err error, _ []byte) {
				if !strings.Contains(err.Error(), "outer prefix") {
					t.Errorf("Error() = %q, want mention of the outer prefix", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			raw := tt.continuation(t)

			cursor := m.newVectorMultiPartitionCursor(nil, []float64{1, 2, 3}, 5, 16, 1, raw, ScanProperties{})

			_, err := cursor.OnNext(ctx)
			if err == nil {
				t.Fatal("OnNext: want error for invalid continuation, got nil (silent restart is a wrong-results divergence)")
			}
			tt.checkErr(t, err, raw)

			_, err2 := cursor.OnNext(ctx)
			if err2 == nil || err.Error() != err2.Error() {
				t.Errorf("error not latched: first %q, second %v", err, err2)
			}
			if err := cursor.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
}

// TestVectorSearchCursorCorruptEntryKey pins that a continuation whose entry
// key bytes are not a valid packed tuple errors instead of restarting (Java:
// Tuple.fromBytes throws inside scanSinglePartition).
func TestVectorSearchCursorCorruptEntryKey(t *testing.T) {
	t.Parallel()

	m := &vectorIndexMaintainer{standardIndexMaintainer: standardIndexMaintainer{index: &Index{Name: "vec_entry"}}}

	raw, err := (&gen.VectorIndexScanContinuation{
		IndexEntries: []*gen.VectorIndexScanContinuation_IndexEntry{
			{Key: []byte{0xff}, Value: tuple.Tuple{nil}.Pack()}, // key: invalid packed tuple
		},
		InnerContinuation: contPos(0),
	}).MarshalVT()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	cursor, err := m.newVectorSearchCursor(nil, raw, nil)
	if err == nil {
		t.Fatal("newVectorSearchCursor: want error for corrupt entry key, got nil")
	}
	if cursor != nil {
		t.Errorf("cursor = %v, want nil on error", cursor)
	}
	if !strings.Contains(err.Error(), "entry 0 key") {
		t.Errorf("Error() = %q, want mention of the corrupt entry key", err)
	}
}
