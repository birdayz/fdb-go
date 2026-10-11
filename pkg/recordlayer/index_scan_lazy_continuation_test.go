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

// javaContinuationBytes hand-encodes Java's KeyValueCursorBase.Continuation.toProto
// (KeyValueCursorBase.java:225-236) for lastKey: inner_continuation is always
// lastKey[prefixLength:], then field 2 sfixed64 magic_number.
func javaContinuationBytes(lastKey []byte, prefixLength int) []byte {
	suffix := lastKey[prefixLength:]
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
				suffix := []byte(key[c.prefixLength:]) // Java's slice, computed independently of keySuffix
				eager, err := wrapContinuation(suffix)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, eager) {
					t.Fatalf("row %d: lazy %x != eager %x", row, got, eager)
				}
				if java := javaContinuationBytes(key, c.prefixLength); !bytes.Equal(got, java) {
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

func drainIndexCursor(t testing.TB, c *indexCursor) []RecordCursorResult[*IndexEntry] {
	var out []RecordCursorResult[*IndexEntry]
	for {
		res, err := c.OnNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, res)
		if !res.HasNext() {
			return out
		}
	}
}

func newSliceIndexCursor(ss subspace.Subspace, kvs []fdb.KeyValue) *indexCursor {
	return &indexCursor{
		indexSubspace: ss,
		prefixLength:  len(ss.Bytes()),
		iterator:      &sliceRangeIterator{kvs: kvs},
		scanState:     resolveScanLimiterState(ExecuteProperties{}),
	}
}

// TestIndexCursorContinuationsSurviveDrain pins that the un-copied key suffix held by a
// lazy continuation still serializes correctly after the whole scan has been consumed.
func TestIndexCursorContinuationsSurviveDrain(t *testing.T) {
	t.Parallel()
	ss := subspace.Sub("idx")
	var kvs []fdb.KeyValue
	for i := range 50 {
		kvs = append(kvs, fdb.KeyValue{Key: ss.Pack(tuple.Tuple{int64(i), "pk"})})
	}
	results := drainIndexCursor(t, newSliceIndexCursor(ss, kvs))
	if len(results) != len(kvs)+1 {
		t.Fatalf("got %d results, want %d", len(results), len(kvs)+1)
	}
	for i, kv := range kvs {
		got, err := results[i].GetContinuation().ToBytes()
		if err != nil {
			t.Fatal(err)
		}
		if want := javaContinuationBytes(kv.Key, len(ss.Bytes())); !bytes.Equal(got, want) {
			t.Fatalf("row %d after drain: %x want %x", i, got, want)
		}
	}
}

// TestIndexCursorRowAllocs pins the per-row allocation count, so the continuation
// marshal cannot silently come back onto the per-row path. Not parallel: the
// process-wide malloc counter must not see sibling tests.
func TestIndexCursorRowAllocs(t *testing.T) {
	ss := subspace.Sub("idx")
	kvsFor := func(n int) []fdb.KeyValue {
		var kvs []fdb.KeyValue
		for i := range n {
			kvs = append(kvs, fdb.KeyValue{Key: ss.Pack(tuple.Tuple{int64(i % 100), int64(i % 100)})})
		}
		return kvs
	}
	allocsFor := func(n int) int64 {
		kvs := kvsFor(n)
		res := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				c := newSliceIndexCursor(ss, kvs)
				for {
					r, err := c.OnNext(context.Background())
					if err != nil {
						b.Fatal(err)
					}
					if !r.HasNext() {
						break
					}
				}
			}
		})
		if res.N == 0 {
			t.Fatal("benchmark ran zero iterations")
		}
		return res.AllocsPerOp()
	}
	const small, large = 64, 128
	perRow := float64(allocsFor(large)-allocsFor(small)) / float64(large-small)
	// IndexEntry, key tuple slice, continuation; the proto marshal was a 4th.
	const want = 3
	if perRow != want {
		t.Fatalf("indexCursor allocates %v per row, want %d", perRow, want)
	}
}

// TestKeyContinuationEmptySuffixMatchesJava pins Java's toProto for a key equal to the
// prefix (KeyValueCursorBase.java:233-235): an empty but present suffix, 0A 00 11 <magic>.
func TestKeyContinuationEmptySuffixMatchesJava(t *testing.T) {
	t.Parallel()
	ss := subspace.Sub("idx", int64(7))
	key := fdb.Key(ss.Bytes())
	pl := len(key)
	want := binary.LittleEndian.AppendUint64([]byte{0x0A, 0x00, 0x11}, uint64(continuationMagicNumber))

	idx, err := (&indexCursor{prefixLength: pl}).makeContinuation(key).ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	count, err := (&countKVCursor{prefixLength: pl}).makeContinuation(key)
	if err != nil {
		t.Fatal(err)
	}
	bitmap, err := (&bitmapKVCursor{prefixLength: pl}).makeContinuation(key)
	if err != nil {
		t.Fatal(err)
	}
	kvc, err := (&keyValueCursor{prefixLength: pl}).makeKeyContinuation(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string][]byte{"index": idx, "count": count, "bitmap": bitmap, "keyValue": kvc} {
		if !bytes.Equal(got, want) {
			t.Errorf("%s cursor: %x, want Java's %x", name, got, want)
		}
	}
	if (&keyValueContinuation{suffix: key[pl:]}).IsEnd() {
		t.Error("an empty suffix is a real position, not the end")
	}
}

func TestKeySuffix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key  []byte
		pl   int
		want []byte
	}{
		{[]byte{1, 2, 3}, 1, []byte{2, 3}},
		{[]byte{1, 2}, 2, []byte{}}, // equal to the prefix: empty, not nil
		{[]byte{1}, 2, []byte{1}},   // shorter: kept whole
		{nil, 0, nil},
	} {
		got := keySuffix(tc.key, tc.pl)
		if !bytes.Equal(got, tc.want) || (got == nil) != (tc.want == nil) {
			t.Errorf("keySuffix(%x, %d) = %x (nil=%v), want %x (nil=%v)", tc.key, tc.pl, got, got == nil, tc.want, tc.want == nil)
		}
	}
}
