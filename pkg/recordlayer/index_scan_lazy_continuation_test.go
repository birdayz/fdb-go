package recordlayer

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// sliceRangeIterator replays a fixed batch of key-values.
type sliceRangeIterator struct {
	kvs []fdb.KeyValue
	pos int
}

func (s *sliceRangeIterator) Advance() bool {
	if s.pos >= len(s.kvs) {
		return false
	}
	s.pos++
	return true
}

func (s *sliceRangeIterator) Get() (fdb.KeyValue, error) {
	if s.pos == 0 || s.pos > len(s.kvs) {
		return fdb.KeyValue{}, nil
	}
	return s.kvs[s.pos-1], nil
}

// javaContinuationBytes hand-encodes what Java's KeyValueCursorBase.Continuation.toProto
// serializes (KeyValueCursorBase.java:225-236): field 1 bytes inner_continuation, then
// field 2 sfixed64 magic_number.
func javaContinuationBytes(suffix []byte) []byte {
	if suffix == nil {
		return nil
	}
	out := []byte{0x0A}
	out = binary.AppendUvarint(out, uint64(len(suffix)))
	out = append(out, suffix...)
	out = append(out, 0x11)
	return binary.LittleEndian.AppendUint64(out, uint64(continuationMagicNumber))
}

// TestIndexCursorLazyContinuationBytes pins that the lazily marshaled index-scan
// continuation is byte-identical to the eager wrapContinuation output and to Java's
// encoding, at every row and at every stop reason.
func TestIndexCursorLazyContinuationBytes(t *testing.T) {
	t.Parallel()
	prefixed := subspace.Sub("idx", int64(7))
	empty := subspace.FromBytes(nil)
	keys := func(ss subspace.Subspace, n int) []fdb.KeyValue {
		var kvs []fdb.KeyValue
		for i := range n {
			kvs = append(kvs, fdb.KeyValue{
				Key:   ss.Pack(tuple.Tuple{int64(i * 1000), "pk", []byte{0x00, 0xFF}, int64(i)}),
				Value: nil,
			})
		}
		return kvs
	}
	reversed := func(kvs []fdb.KeyValue) []fdb.KeyValue {
		out := slices.Clone(kvs)
		slices.Reverse(out)
		return out
	}
	long := keys(prefixed, 1)
	long[0].Key = prefixed.Pack(tuple.Tuple{string(bytes.Repeat([]byte{'x'}, 300))}) // multi-byte varint length

	cases := []struct {
		name  string
		ss    subspace.Subspace
		kvs   []fdb.KeyValue
		props ExecuteProperties
	}{
		{"forward-exhausted", prefixed, keys(prefixed, 5), ExecuteProperties{}},
		{"reverse-exhausted", prefixed, reversed(keys(prefixed, 5)), ExecuteProperties{}},
		{"row-limit-middle", prefixed, keys(prefixed, 5), ExecuteProperties{ReturnedRowLimit: 2}},
		{"row-limit-reverse", prefixed, reversed(keys(prefixed, 5)), ExecuteProperties{ReturnedRowLimit: 3}},
		{"scan-limit", prefixed, keys(prefixed, 5), ExecuteProperties{ScannedRecordsLimit: 2}},
		{"empty-scan", prefixed, nil, ExecuteProperties{}},
		{"empty-subspace-prefix", empty, keys(empty, 3), ExecuteProperties{}},
		{"key-equals-prefix", prefixed, []fdb.KeyValue{{Key: fdb.Key(prefixed.Bytes())}}, ExecuteProperties{}},
		{"long-suffix", prefixed, long, ExecuteProperties{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			props := ScanProperties{ExecuteProperties: tc.props}
			c := &indexCursor{
				indexSubspace: tc.ss,
				prefixLength:  len(tc.ss.Bytes()),
				scanProps:     props,
				iterator:      &sliceRangeIterator{kvs: tc.kvs},
				scanState:     resolveScanLimiterState(tc.props),
			}
			var lastEager []byte
			for row := 0; ; row++ {
				res, err := c.OnNext(context.Background())
				if err != nil {
					t.Fatalf("row %d: %v", row, err)
				}
				got, err := res.GetContinuation().ToBytes()
				if err != nil {
					t.Fatalf("row %d ToBytes: %v", row, err)
				}
				again, _ := res.GetContinuation().ToBytes()
				if !bytes.Equal(got, again) {
					t.Fatalf("row %d: ToBytes not stable", row)
				}
				if !res.HasNext() {
					want := lastEager // limit stops resume after the last returned row
					if res.GetNoNextReason() == SourceExhausted {
						want = nil
					}
					if !bytes.Equal(got, want) || (got == nil) != (want == nil) {
						t.Fatalf("stop %v: got %x want %x", res.GetNoNextReason(), got, want)
					}
					if res.GetContinuation().IsEnd() != (res.GetNoNextReason() == SourceExhausted) {
						t.Fatalf("stop %v: IsEnd=%v", res.GetNoNextReason(), res.GetContinuation().IsEnd())
					}
					break
				}
				key := tc.kvs[row].Key
				suffix := []byte(key)
				if len(key) > c.prefixLength {
					suffix = key[c.prefixLength:]
				}
				eager, err := wrapContinuation(suffix)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, eager) {
					t.Fatalf("row %d: lazy %x != eager %x", row, got, eager)
				}
				if java := javaContinuationBytes(suffix); !bytes.Equal(got, java) {
					t.Fatalf("row %d: lazy %x != java %x", row, got, java)
				}
				if res.GetContinuation().IsEnd() != (&BytesContinuation{bytes: eager}).IsEnd() {
					t.Fatalf("row %d: IsEnd diverges from eager", row)
				}
				if !bytes.Equal(unwrapContinuation(got), suffix) {
					t.Fatalf("row %d: round-trip suffix mismatch", row)
				}
				lastEager = eager
			}
		})
	}

	// A nil suffix is an end position, as BytesContinuation{wrapContinuation(nil)} was.
	nilCont := &keyValueContinuation{}
	if b, _ := nilCont.ToBytes(); b != nil || !nilCont.IsEnd() {
		t.Fatalf("nil suffix: bytes %x IsEnd %v", b, nilCont.IsEnd())
	}
}
