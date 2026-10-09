package vectorindex

import (
	"context"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

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

func TestVectorMultiPartitionContinuationInvalid(t *testing.T) {
	t.Parallel()

	// The constructor validates the continuation before touching FDB, so a
	// corrupt token is fully testable without a transaction: the returned
	// cursor is an errorCursor whose OnNext never reaches the skip-scan.
	m := &vectorIndexMaintainer{
		StandardIndexMaintainer: *recordlayer.NewStandardIndexMaintainer(recordlayer.IndexMaintainerState{Index: &recordlayer.Index{Name: "vec_mp"}}),
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

			cursor := m.newVectorMultiPartitionCursor(nil, []float64{1, 2, 3}, 5, recordlayer.VectorIndexScanOptions{EfSearch: positiveEfSearch(16)}, 1, raw, recordlayer.ScanProperties{})

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

	m := &vectorIndexMaintainer{StandardIndexMaintainer: *recordlayer.NewStandardIndexMaintainer(recordlayer.IndexMaintainerState{Index: &recordlayer.Index{Name: "vec_entry"}})}

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
