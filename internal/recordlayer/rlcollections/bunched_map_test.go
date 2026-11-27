package rlcollections

import (
	"bytes"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

type recordingTimer struct {
	writes     [][3]int
	deletes    [][2]int
	rangeReads [][]fdb.KeyValue
}

func (r *recordingTimer) OnWrite(keyLen, valLen, oldValLen int) {
	r.writes = append(r.writes, [3]int{keyLen, valLen, oldValLen})
}

func (r *recordingTimer) OnDelete(keyLen, oldValLen int) {
	r.deletes = append(r.deletes, [2]int{keyLen, oldValLen})
}

func (r *recordingTimer) OnRangeRead(kvs []fdb.KeyValue) {
	r.rangeReads = append(r.rangeReads, kvs)
}

func TestBunchedMapHelpers(t *testing.T) {
	t.Parallel()
	if got := joinBytes(); len(got) != 0 {
		t.Fatalf("joinBytes() = %x", got)
	}
	if got := joinBytes([]byte{1}, nil, []byte{2, 3}); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("joinBytes = %x", got)
	}
	if !tupleEqual(tuple.Tuple{int64(42), "hello"}, tuple.Tuple{int64(42), "hello"}) {
		t.Fatal("tupleEqual same failed")
	}
	if tupleEqual(tuple.Tuple{int64(42), "hello"}, tuple.Tuple{int64(42), "world"}) {
		t.Fatal("tupleEqual diff failed")
	}
	if !tupleEqual(nil, tuple.Tuple{}) {
		t.Fatal("tupleEqual nil vs empty failed")
	}
	if !positionListsEqual([]int{1, 2}, []int{1, 2}) || positionListsEqual([]int{1, 2}, []int{1, 3}) || positionListsEqual([]int{1}, []int{1, 2}) {
		t.Fatal("positionListsEqual failed")
	}
	if !positionListsEqual(nil, []int{}) {
		t.Fatal("positionListsEqual nil vs empty failed")
	}
	if boolToInt(true) != 1 || boolToInt(false) != 0 {
		t.Fatal("boolToInt failed")
	}
	elems := TupleToElements(tuple.Tuple{int64(1), "hello", []byte{0xff}})
	if len(elems) != 3 || elems[0] != int64(1) || elems[1] != "hello" {
		t.Fatalf("TupleToElements = %v", elems)
	}
	if bunchedMapMaxValueSize != 10_000 {
		t.Fatalf("bunchedMapMaxValueSize = %d", bunchedMapMaxValueSize)
	}
}

func TestBunchedMapTimerInstrumentation(t *testing.T) {
	t.Parallel()
	nilBM := NewBunchedMap(5)
	if nilBM.bunchSize != 5 || nilBM.serializer == nil || nilBM.timer != nil {
		t.Fatalf("unexpected NewBunchedMap: %+v", nilBM)
	}
	nilBM.instrumentWrite([]byte{1}, []byte{2}, nil)
	nilBM.instrumentDelete([]byte{1}, []byte{2})
	nilBM.instrumentRangeRead([]fdb.KeyValue{{Key: []byte{1}, Value: []byte{2}}})

	timer := &recordingTimer{}
	bm := NewInstrumentedBunchedMap(10, timer)
	bm.instrumentWrite([]byte{1, 2}, []byte{3, 4, 5}, nil)
	bm.instrumentWrite([]byte{1, 2}, []byte{3, 4, 5}, []byte{6, 7, 8, 9})
	bm.instrumentDelete([]byte{1, 2, 3}, nil)
	bm.instrumentDelete([]byte{1, 2}, []byte{1, 2, 3, 4, 5})
	bm.instrumentRangeRead([]fdb.KeyValue{{Key: []byte{1, 2}, Value: []byte{3}}})

	if len(timer.writes) != 2 || timer.writes[0] != [3]int{2, 3, -1} || timer.writes[1] != [3]int{2, 3, 4} {
		t.Fatalf("writes = %v", timer.writes)
	}
	if len(timer.deletes) != 2 || timer.deletes[0] != [2]int{3, -1} || timer.deletes[1] != [2]int{2, 5} {
		t.Fatalf("deletes = %v", timer.deletes)
	}
	if len(timer.rangeReads) != 1 || len(timer.rangeReads[0]) != 1 {
		t.Fatalf("rangeReads = %v", timer.rangeReads)
	}
}

func TestTextSubspaceSplitterWhiteBox(t *testing.T) {
	t.Parallel()
	indexSS := subspace.FromBytes(tuple.Tuple{"idx"}.Pack())
	splitter := NewTextSubspaceSplitter(indexSS, 2)
	ss, err := splitter.SubspaceOf(indexSS.Pack(tuple.Tuple{"group1", "token", "pk"}))
	if err != nil || !bytes.Equal(ss.Bytes(), indexSS.Sub("group1", "token").Bytes()) {
		t.Fatalf("SubspaceOf = %v, %v", ss, err)
	}
	if _, err := splitter.SubspaceOf(indexSS.Pack(tuple.Tuple{"onlyone"})); err == nil {
		t.Fatal("expected error for short key")
	}
	tag, err := splitter.SubspaceTag(indexSS.Sub("group1", "token"))
	if err != nil || len(tag) != 2 || tag[0] != "group1" || tag[1] != "token" {
		t.Fatalf("SubspaceTag = %v, %v", tag, err)
	}
}

func TestBunchedMapIteratorsWhiteBoxState(t *testing.T) {
	t.Parallel()
	s := TextIndexBunchedSerializerInstance()

	// BunchedMapMultiIterator
	it := &BunchedMapMultiIterator{serializer: s}
	it.Cancel()
	if !it.done || it.HasNext() || it.Next() != nil {
		t.Fatal("canceled multi iterator should be done")
	}
	if (&BunchedMapMultiIterator{serializer: s}).Err() != nil {
		t.Fatal("expected nil Err by default")
	}
	if (&BunchedMapMultiIterator{serializer: s, iterErr: &BunchedMapException{Message: "test error"}}).Err() == nil {
		t.Fatal("expected sticky Err")
	}
	lastKey := tuple.Tuple{"mytoken"}
	suffix := []byte{0xAB, 0xCD}
	stopped := &BunchedMapMultiIterator{
		serializer: s, lastKey: lastKey, currentSubspaceKey: []byte{0x01, 0xAB, 0xCD},
		currentSubspaceSuffix: suffix, done: true, limit: 5, returned: 5,
	}
	if want := append(append([]byte{}, suffix...), s.SerializeKey(lastKey)...); !bytes.Equal(stopped.GetContinuation(), want) {
		t.Fatalf("multi cont = %x, want %x", stopped.GetContinuation(), want)
	}
	exhausted := &BunchedMapMultiIterator{
		serializer: s, lastKey: lastKey, currentSubspaceKey: []byte{0x01, 0xAB, 0xCD},
		currentSubspaceSuffix: suffix, done: true, limit: 0, returned: 5,
	}
	if exhausted.GetContinuation() != nil {
		t.Fatal("exhausted multi iterator should have nil continuation")
	}

	// BunchedMapMultiIterator Next
	entry := &BunchedMapScanEntry{Key: tuple.Tuple{"cached"}, Value: []int{10, 20}}
	mit := &BunchedMapMultiIterator{serializer: s, nextEntry: entry, limit: 1}
	got := mit.Next()
	if got == nil || !mit.done || mit.returned != 1 || mit.nextEntry != nil {
		t.Fatalf("multi Next unexpected state: got=%v done=%v ret=%d", got, mit.done, mit.returned)
	}

	// BunchedMapIterator
	sit := &BunchedMapIterator{serializer: s, lastKey: tuple.Tuple{"bar"}, done: true, limit: 5, returned: 5}
	if !bytes.Equal(sit.GetContinuation(), s.SerializeKey(tuple.Tuple{"bar"})) {
		t.Fatalf("single cont mismatch")
	}
	sitExhausted := &BunchedMapIterator{serializer: s, lastKey: tuple.Tuple{"foo"}, done: true, limit: 10, returned: 3}
	if sitExhausted.GetContinuation() != nil {
		t.Fatal("single exhausted should have nil continuation")
	}
	sNext := &BunchedMapIterator{serializer: s, nextEntry: &bunchedEntry{Key: tuple.Tuple{"last"}, Value: []int{5}}, limit: 1}
	if sGot := sNext.Next(); sGot == nil || !sNext.done || sNext.returned != 1 {
		t.Fatalf("single Next unexpected state")
	}
}

func TestBunchedErrorsFormatting(t *testing.T) {
	t.Parallel()
	e1 := &BunchedMapException{Message: "signpost mismatch"}
	if !strings.Contains(e1.Error(), "signpost mismatch") {
		t.Fatalf("unexpected error: %v", e1)
	}
	e2 := &BunchedSerializationError{Message: "corrupt prefix", Data: []byte{0xDE, 0xAD}}
	if !strings.Contains(e2.Error(), "corrupt prefix") || !strings.Contains(e2.Error(), "data len=2") {
		t.Fatalf("unexpected error: %v", e2)
	}
	e3 := &BunchedSerializationError{Message: "short"}
	if strings.Contains(e3.Error(), "data len") {
		t.Fatalf("unexpected data len in %v", e3)
	}
}
