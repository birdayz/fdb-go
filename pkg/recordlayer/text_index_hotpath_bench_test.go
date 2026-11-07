package recordlayer

import (
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Benchmarks isolate TEXT update CPU without I/O, using exporter (writer, seq, part) keys
// so most BunchedMap puts append rather than insert randomly.

// benchBunchTx answers a put's snapshot range read with canned key-values and
// drops its writes and conflict ranges.
type benchBunchTx struct {
	fdb.WritableTransaction
	kvs []fdb.KeyValue
	// byMap, when set, answers a read by the map its key is in: the first
	// prefixLen bytes of the begin selector's key.
	byMap     map[string][]fdb.KeyValue
	prefixLen int
	// walkDepth is the depth of the stack a read uses, as the client's RYW
	// walk does; 0 uses none.
	walkDepth int
}

type benchBunchSnapshot struct {
	fdb.ReadTransaction
	tx *benchBunchTx
}

type benchBunchRange struct {
	kvs       []fdb.KeyValue
	walkDepth int
}

func (tx *benchBunchTx) Snapshot() fdb.ReadTransaction { return benchBunchSnapshot{tx: tx} }

func (s benchBunchSnapshot) GetRange(r fdb.Range, _ fdb.RangeOptions) fdb.RangeResult {
	kvs := s.tx.kvs
	if s.tx.byMap != nil {
		begin, _ := r.FDBRangeKeySelectors()
		kvs = s.tx.byMap[string(begin.FDBKeySelector().Key.FDBKey()[:s.tx.prefixLen])]
	}
	return benchBunchRange{kvs: kvs, walkDepth: s.tx.walkDepth}
}

// benchWalk uses about 560 bytes of stack per level.
//
//go:noinline
func benchWalk(n int) byte {
	var frame [512]byte
	frame[n%len(frame)] = byte(n)
	if n == 0 {
		return frame[0]
	}
	return benchWalk(n-1) + frame[n%len(frame)]
}

func (r benchBunchRange) GetSliceWithError() ([]fdb.KeyValue, error) {
	if r.walkDepth > 0 {
		benchWalk(r.walkDepth)
	}
	return r.kvs, nil
}
func (r benchBunchRange) GetSliceOrPanic() []fdb.KeyValue { return r.kvs }
func (r benchBunchRange) Iterator() fdb.RangeIterator     { panic("not used") }

func (*benchBunchTx) Set(fdb.KeyConvertible, []byte)                  {}
func (*benchBunchTx) SetBytes([]byte, []byte)                         {}
func (*benchBunchTx) Clear(fdb.KeyConvertible)                        {}
func (*benchBunchTx) ClearBytes([]byte)                               {}
func (*benchBunchTx) AppendIfFits(fdb.KeyConvertible, []byte)         {}
func (*benchBunchTx) AppendIfFitsBytes([]byte, []byte)                {}
func (*benchBunchTx) AddReadConflictKey(fdb.KeyConvertible) error     { return nil }
func (*benchBunchTx) AddReadConflictKeyBytes([]byte) error            { return nil }
func (*benchBunchTx) AddWriteConflictKey(fdb.KeyConvertible) error    { return nil }
func (*benchBunchTx) AddWriteConflictKeyBytes([]byte) error           { return nil }
func (*benchBunchTx) AddReadConflictRange(fdb.ExactRange) error       { return nil }
func (*benchBunchTx) AddReadConflictRangeBytes([]byte, []byte) error  { return nil }
func (*benchBunchTx) AddWriteConflictRange(fdb.ExactRange) error      { return nil }
func (*benchBunchTx) AddWriteConflictRangeBytes([]byte, []byte) error { return nil }

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

// BenchmarkTextBunchedMapPipelinedPuts overlaps 128 token-map reads in issue order,
// finishing oldest first; deep reads mimic the client RYW stack.
func BenchmarkTextBunchedMapPipelinedPuts(b *testing.B) {
	const tokens, pipeline = 128, 128
	m := NewBunchedMap(textIndexBunchSize)
	index := subspace.Sub("idx", "tenant", "user", int64(3))
	maps := make([]subspace.Subspace, tokens)
	byMap := make(map[string][]fdb.KeyValue, tokens)
	for i := range maps {
		maps[i] = index.Sub(fmt.Sprintf("tok%04d", i))
		byMap[string(maps[i].Bytes())] = []fdb.KeyValue{benchBunch(maps[i], 1000, 10)}
	}
	key, value := benchMessageKey(1010), []int{1, 2, 9}
	for _, depth := range []int{0, 48} {
		name := "shallow"
		if depth > 0 {
			name = "deep"
		}
		b.Run(name, func(b *testing.B) {
			tx := &benchBunchTx{byMap: byMap, prefixLen: len(maps[0].Bytes()), walkDepth: depth}
			pending := make([]pendingBunchedPut, 0, pipeline)
			finishOldest := func() {
				if _, _, err := pending[0].finish(); err != nil {
					b.Fatal(err)
				}
				pending = pending[1:]
			}
			b.ReportAllocs()
			for b.Loop() {
				pending = pending[:0]
				for _, ss := range maps {
					if len(pending) == pipeline {
						finishOldest()
					}
					pending = append(pending, m.startPut(tx, ss, key, value))
				}
				for len(pending) > 0 {
					finishOldest()
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
