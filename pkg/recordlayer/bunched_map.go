// Portions derived from FoundationDB Record Layer (BunchedMap.java,
// BunchedMapIterator.java, StoreTimer.java, KeyRange.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"

	"fdb.dev/internal/recordlayer/rlcollections"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

type (
	BunchedMap              = rlcollections.BunchedMap
	BunchedMapIterator      = rlcollections.BunchedMapIterator
	subspaceSplitter        = rlcollections.SubspaceSplitter
	textSubspaceSplitter    = rlcollections.TextSubspaceSplitter
	bunchedMapScanEntry     = rlcollections.BunchedMapScanEntry
	kvCallback              = rlcollections.KVCallback
	bunchedMapMultiIterator = rlcollections.BunchedMapMultiIterator
	bunchedEntry            = rlcollections.BunchedEntry[tuple.Tuple, []int]
	pendingBunchedPut       = rlcollections.PendingBunchedPut
	pendingBunchedRemove    = rlcollections.PendingBunchedRemove
)

type storeTimerBunchedMapAdapter struct{ timer *StoreTimer }

func (a storeTimerBunchedMapAdapter) OnWrite(keyLen, valLen, oldValLen int) {
	a.timer.Increment(CountSaveIndexKey)
	a.timer.IncrementBy(CountSaveIndexKeyBytes, int64(keyLen))
	a.timer.IncrementBy(CountSaveIndexValueBytes, int64(valLen))
	if oldValLen >= 0 {
		a.timer.IncrementBy(CountDeleteIndexValueBytes, int64(oldValLen))
	}
}

func (a storeTimerBunchedMapAdapter) OnDelete(keyLen, oldValLen int) {
	a.timer.Increment(CountDeleteIndexKey)
	a.timer.IncrementBy(CountDeleteIndexKeyBytes, int64(keyLen))
	if oldValLen >= 0 {
		a.timer.IncrementBy(CountDeleteIndexValueBytes, int64(oldValLen))
	}
}

func (a storeTimerBunchedMapAdapter) OnRangeRead(kvs []fdb.KeyValue) {
	for _, kv := range kvs {
		a.timer.Increment(CountLoadIndexKey)
		a.timer.IncrementBy(CountLoadIndexKeyBytes, int64(len(kv.Key)))
		a.timer.IncrementBy(CountLoadIndexValueBytes, int64(len(kv.Value)))
	}
}

// NewBunchedMap creates a new BunchedMap with the given bunch size.
func NewBunchedMap(bunchSize int) *BunchedMap { return rlcollections.NewBunchedMap(bunchSize) }

// NewInstrumentedBunchedMap creates a BunchedMap with instrumentation enabled.
// Matches Java's InstrumentedBunchedMap which wraps BunchedMap with timer hooks.
func NewInstrumentedBunchedMap(bunchSize int, timer *StoreTimer) *BunchedMap {
	if timer == nil {
		return rlcollections.NewBunchedMap(bunchSize)
	}
	return rlcollections.NewInstrumentedBunchedMap(bunchSize, storeTimerBunchedMapAdapter{timer: timer})
}

// newTextSubspaceSplitter creates a splitter for TEXT index scans.
func newTextSubspaceSplitter(indexSubspace subspace.Subspace, groupingColumns int) *textSubspaceSplitter {
	return rlcollections.NewTextSubspaceSplitter(indexSubspace, groupingColumns)
}

// newTextSubspaceSplitterForRange derives logical suffix bounds from tuple
// endpoints, before an exclusive bound's strinc can make its bytes undecodable.
func newTextSubspaceSplitterForRange(indexSubspace subspace.Subspace, groupingColumns int, scanRange TupleRange) *textSubspaceSplitter {
	s := newTextSubspaceSplitter(indexSubspace, groupingColumns)
	if len(scanRange.Low) < groupingColumns || len(scanRange.High) < groupingColumns {
		return s
	}
	prefix := scanRange.Low[:groupingColumns]
	if !bytes.Equal(prefix.Pack(), scanRange.High[:groupingColumns].Pack()) {
		return s // the range covers more than one token map
	}
	full, bounded := TupleRangeAllOf(prefix).ToFDBRange(indexSubspace), scanRange.ToFDBRange(indexSubspace)
	if bytes.Compare(bounded.Begin.FDBKey(), full.Begin.FDBKey()) < 0 || bytes.Compare(bounded.End.FDBKey(), full.End.FDBKey()) > 0 {
		return s // a token prefix spans maps; a suffix prefix stays within one
	}
	rlcollections.SetTextSubspaceSplitterLogicalBounds(s, bytes.Compare(bounded.Begin.FDBKey(), full.Begin.FDBKey()) > 0 || bytes.Compare(bounded.End.FDBKey(), full.End.FDBKey()) < 0)
	return s
}

// newBunchedMapMultiIterator creates a multi-map iterator.
// Streams from FDB lazily — only one bunch in memory at a time.
func newBunchedMapMultiIterator(
	tx fdb.ReadTransaction, parentSubspace subspace.Subspace, splitter subspaceSplitter,
	beginBytes, endBytes, continuation []byte, limit int, reverse bool, serializer *TextIndexBunchedSerializer,
) *bunchedMapMultiIterator {
	return rlcollections.NewBunchedMapMultiIterator(tx, parentSubspace, splitter, beginBytes, endBytes, continuation, limit, reverse, serializer)
}

// newBunchedMapMultiIteratorWithCallback creates a multi-map iterator with a
// per-KV callback for byte tracking. The callback fires once per raw FDB
// key-value pair read, before deserialization. Matches Java's scanMulti()
// with Consumer<KeyValue> callback parameter.
func newBunchedMapMultiIteratorWithCallback(
	tx fdb.ReadTransaction, parentSubspace subspace.Subspace, splitter subspaceSplitter,
	beginBytes, endBytes, continuation []byte, limit int, callback kvCallback, reverse bool, serializer *TextIndexBunchedSerializer,
) *bunchedMapMultiIterator {
	return rlcollections.NewBunchedMapMultiIteratorWithCallback(tx, parentSubspace, splitter, beginBytes, endBytes, continuation, limit, callback, reverse, serializer)
}
