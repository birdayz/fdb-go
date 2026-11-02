package recordlayer

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// The TEXT index update hot path of a message transaction, without I/O: one
// BunchedMap put given the snapshot read's result (the token map's signposts
// around the key), the bunch (de)serialization and key comparisons inside it,
// and the tokenizer. Map keys are the Message grouped primary key of the
// GenAI exporter, (writer, seq, part), so most puts append to the last bunch.

// benchBunchTx answers a put's snapshot range read with canned key-values and
// drops its writes and conflict ranges.
type benchBunchTx struct {
	fdb.WritableTransaction
	kvs []fdb.KeyValue
}

type benchBunchSnapshot struct {
	fdb.ReadTransaction
	kvs []fdb.KeyValue
}

type benchBunchRange struct{ kvs []fdb.KeyValue }

func (tx *benchBunchTx) Snapshot() fdb.ReadTransaction { return benchBunchSnapshot{kvs: tx.kvs} }

func (s benchBunchSnapshot) GetRange(fdb.Range, fdb.RangeOptions) fdb.RangeResult {
	return benchBunchRange{kvs: s.kvs}
}

func (r benchBunchRange) GetSliceWithError() ([]fdb.KeyValue, error) { return r.kvs, nil }
func (r benchBunchRange) GetSliceOrPanic() []fdb.KeyValue            { return r.kvs }
func (r benchBunchRange) Iterator() fdb.RangeIterator                { panic("not used") }

func (*benchBunchTx) Set(fdb.KeyConvertible, []byte)               {}
func (*benchBunchTx) Clear(fdb.KeyConvertible)                     {}
func (*benchBunchTx) AppendIfFits(fdb.KeyConvertible, []byte)      {}
func (*benchBunchTx) AddReadConflictKey(fdb.KeyConvertible) error  { return nil }
func (*benchBunchTx) AddWriteConflictKey(fdb.KeyConvertible) error { return nil }
func (*benchBunchTx) AddReadConflictRange(fdb.ExactRange) error    { return nil }
func (*benchBunchTx) AddWriteConflictRange(fdb.ExactRange) error   { return nil }

func benchMessageKey(seq int64) tuple.Tuple {
	return tuple.Tuple{"writer-0042", seq, int64(0)}
}

// benchBunch is the signpost key-value of a bunch holding seqs [first, first+n).
func benchBunch(ss subspace.Subspace, first int64, n int) fdb.KeyValue {
	entries := make([]bunchedEntry, n)
	for i := range entries {
		entries[i] = bunchedEntry{Key: benchMessageKey(first + int64(i)), Value: []int{3 + i, 17 + i, 40 + i}}
	}
	value, err := TextIndexBunchedSerializerInstance().SerializeEntries(entries)
	if err != nil {
		panic(err)
	}
	return fdb.KeyValue{Key: fdb.Key(ss.Pack(entries[0].Key)), Value: value}
}

func BenchmarkTextBunchedMapPut(b *testing.B) {
	ss := subspace.Sub("idx", "tenant", "user", int64(3), "token")
	m := NewBunchedMap(textIndexBunchSize)
	for _, c := range []struct {
		name string
		kvs  []fdb.KeyValue
		seq  int64
	}{
		// The next seq of a writer: APPEND_IF_FITS to the last bunch.
		{"append", []fdb.KeyValue{benchBunch(ss, 1000, 10)}, 1010},
		// A key inside a bunch: the bunch is rewritten.
		{"middle", []fdb.KeyValue{benchBunch(ss, 1000, 10)}, 1004},
		// An existing key with new positions.
		{"update", []fdb.KeyValue{benchBunch(ss, 1000, 10)}, 1005},
		// The last bunch is full: a new signpost.
		{"full", []fdb.KeyValue{benchBunch(ss, 1000, textIndexBunchSize)}, 1000 + textIndexBunchSize},
		// A key inside a full bunch: the bunch splits.
		{"split", []fdb.KeyValue{benchBunch(ss, 1000, textIndexBunchSize)}, 1007},
	} {
		key, value := benchMessageKey(c.seq), []int{1, 2, 9}
		if c.name == "middle" || c.name == "split" {
			// Between two entries: seqs step by one, so take the part.
			key = tuple.Tuple{"writer-0042", c.seq, int64(1)}
		}
		b.Run(c.name, func(b *testing.B) {
			tx := &benchBunchTx{kvs: c.kvs}
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := m.Put(tx, ss, key, value); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkTextBunchSerializer(b *testing.B) {
	ss := subspace.Sub("idx", "token")
	kv := benchBunch(ss, 1000, textIndexBunchSize)
	s := TextIndexBunchedSerializerInstance()
	key, err := s.DeserializeKey(kv.Key, len(ss.Bytes()), len(kv.Key)-len(ss.Bytes()))
	if err != nil {
		b.Fatal(err)
	}
	entries, err := s.DeserializeEntries(key, kv.Value)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("deserialize", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.DeserializeEntries(key, kv.Value); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("serialize", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.SerializeEntries(entries); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkCompareTuples(b *testing.B) {
	x, y := benchMessageKey(1004), benchMessageKey(1005)
	b.ReportAllocs()
	for b.Loop() {
		if compareTuples(x, y) >= 0 {
			b.Fatal("order")
		}
	}
}

func BenchmarkDefaultTextTokenizer(b *testing.B) {
	var sb strings.Builder
	for i := range 60 {
		fmt.Fprintf(&sb, "The assistant replied with step %d of the plan, citing file_%d.go and the user's request. ", i, i%7)
	}
	text := sb.String()
	t := DefaultTextTokenizerInstance()
	b.Run("map", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := t.TokenizeToMap(text, GlobalMinVersion, TokenizerModeIndex); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("iterate", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			it, err := t.Tokenize(text, GlobalMinVersion, TokenizerModeIndex)
			if err != nil {
				b.Fatal(err)
			}
			for it.HasNext() {
				it.Next()
			}
		}
	})
}
