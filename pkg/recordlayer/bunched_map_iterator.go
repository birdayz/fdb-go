package recordlayer

import (
	"bytes"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// SubspaceSplitter determines which sub-map a given FDB key belongs to.
// Used by BunchedMapMultiIterator to partition scans across multiple BunchedMaps.
// Matches Java's com.apple.foundationdb.map.SubspaceSplitter.
type SubspaceSplitter interface {
	SubspaceOf(keyBytes []byte) (subspace.Subspace, error)
	SubspaceTag(ss subspace.Subspace) (tuple.Tuple, error)
}

// TextSubspaceSplitter splits TEXT index keys by grouping columns.
// Given an FDB key in the index subspace, extracts the first groupingColumns
// elements as the subspace prefix for that token's BunchedMap.
// Matches Java's TextSubspaceSplitter.
type TextSubspaceSplitter struct {
	indexSubspace   subspace.Subspace
	groupingColumns int // textPosition + 1 (grouping columns + token column)
}

// NewTextSubspaceSplitter creates a splitter for TEXT index scans.
func NewTextSubspaceSplitter(indexSubspace subspace.Subspace, groupingColumns int) *TextSubspaceSplitter {
	return &TextSubspaceSplitter{
		indexSubspace:   indexSubspace,
		groupingColumns: groupingColumns,
	}
}

// SubspaceOf extracts the subspace for a given FDB key by taking the first
// groupingColumns elements of the unpacked tuple.
func (s *TextSubspaceSplitter) SubspaceOf(keyBytes []byte) (subspace.Subspace, error) {
	t, err := fastSubspaceUnpack(keyBytes, len(s.indexSubspace.Bytes()))
	if err != nil {
		return nil, &BunchedSerializationError{
			Message: fmt.Sprintf("TextSubspaceSplitter: unable to unpack key: %v", err),
			Data:    keyBytes,
		}
	}
	if len(t) < s.groupingColumns {
		return nil, &BunchedSerializationError{
			Message: fmt.Sprintf("TextSubspaceSplitter: key has %d elements, need at least %d", len(t), s.groupingColumns),
			Data:    keyBytes,
		}
	}
	prefix := make(tuple.Tuple, s.groupingColumns)
	copy(prefix, t[:s.groupingColumns])
	return s.indexSubspace.Sub(tupleToTupleElements(prefix)...), nil
}

// SubspaceTag returns the grouping key tuple for a subspace.
func (s *TextSubspaceSplitter) SubspaceTag(ss subspace.Subspace) (tuple.Tuple, error) {
	t, err := fastSubspaceUnpack(ss.Bytes(), len(s.indexSubspace.Bytes()))
	if err != nil {
		return nil, &BunchedSerializationError{
			Message: fmt.Sprintf("TextSubspaceSplitter: unable to unpack subspace tag: %v", err),
			Data:    ss.Bytes(),
		}
	}
	return t, nil
}

// tupleToTupleElements converts a Tuple to []tuple.TupleElement for subspace.Sub().
func tupleToTupleElements(t tuple.Tuple) []tuple.TupleElement {
	result := make([]tuple.TupleElement, len(t))
	for i, v := range t {
		result[i] = v
	}
	return result
}

// BunchedMapScanEntry is a single entry from a multi-map scan.
// Contains the entry data plus the subspace it belongs to.
type BunchedMapScanEntry struct {
	Subspace    subspace.Subspace
	SubspaceTag tuple.Tuple
	Key         tuple.Tuple
	Value       []int
}

// KVCallback is called for each raw FDB key-value pair read during iteration.
// Used for byte scan limiting — the callback receives the raw key and value
// lengths before deserialization. Matches Java's Consumer<KeyValue> callback
// passed to BunchedMapMultiIterator.
type KVCallback func(keyLen, valueLen int)

// BunchedMapMultiIterator iterates over multiple BunchedMaps within a parent subspace.
// Entries are returned sorted first by subspace, then by key within subspace.
// Streams from FDB lazily — only one bunch is in memory at a time.
// Matches Java's BunchedMapMultiIterator.
type BunchedMapMultiIterator struct {
	serializer *TextIndexBunchedSerializer
	splitter   SubspaceSplitter
	parentKey  []byte // parent subspace bytes
	reverse    bool
	limit      int
	callback   KVCallback // per-KV callback for byte tracking

	// FDB streaming iterator
	rangeIter rangeIterator
	iterErr   error // sticky error from Advance/Get

	// Continuation state
	continuation          []byte
	continuationSatisfied bool

	// Current subspace state
	currentSubspace       subspace.Subspace
	currentSubspaceKey    []byte
	currentSubspaceSuffix []byte
	currentSubspaceTag    tuple.Tuple

	// Current bunch state
	currentEntries []BunchedEntry[tuple.Tuple, []int]
	entryIndex     int

	// Iteration state
	lastKey   tuple.Tuple
	returned  int
	done      bool
	nextEntry *BunchedMapScanEntry
}

// NewBunchedMapMultiIterator creates a multi-map iterator.
// Streams from FDB lazily — only one bunch in memory at a time.
func NewBunchedMapMultiIterator(
	tx fdb.ReadTransaction,
	parentSubspace subspace.Subspace,
	splitter SubspaceSplitter,
	beginBytes, endBytes []byte,
	continuation []byte,
	limit int,
	reverse bool,
	serializer *TextIndexBunchedSerializer,
) *BunchedMapMultiIterator {
	return NewBunchedMapMultiIteratorWithCallback(
		tx, parentSubspace, splitter,
		beginBytes, endBytes, continuation,
		limit, nil, reverse, serializer,
	)
}

// NewBunchedMapMultiIteratorWithCallback creates a multi-map iterator with a
// per-KV callback for byte tracking. The callback fires once per raw FDB
// key-value pair read, before deserialization. Matches Java's scanMulti()
// with Consumer<KeyValue> callback parameter.
func NewBunchedMapMultiIteratorWithCallback(
	tx fdb.ReadTransaction,
	parentSubspace subspace.Subspace,
	splitter SubspaceSplitter,
	beginBytes, endBytes []byte,
	continuation []byte,
	limit int,
	callback KVCallback,
	reverse bool,
	serializer *TextIndexBunchedSerializer,
) *BunchedMapMultiIterator {
	it := &BunchedMapMultiIterator{
		serializer:            serializer,
		splitter:              splitter,
		parentKey:             parentSubspace.Bytes(),
		reverse:               reverse,
		limit:                 limit,
		callback:              callback,
		continuation:          continuation,
		continuationSatisfied: continuation == nil,
		entryIndex:            -1,
	}

	// Build the FDB range read based on continuation.
	var rangeResult fdb.RangeResult
	if continuation == nil {
		rangeResult = tx.GetRange(fdb.KeyRange{Begin: fdb.Key(beginBytes), End: fdb.Key(endBytes)},
			fdb.RangeOptions{Reverse: reverse})
	} else {
		continuationEndpoint := append(append([]byte{}, it.parentKey...), continuation...)
		if reverse {
			if bytes.Compare(continuationEndpoint, endBytes) < 0 {
				rangeResult = tx.GetRange(fdb.KeyRange{Begin: fdb.Key(beginBytes), End: fdb.Key(continuationEndpoint)},
					fdb.RangeOptions{Reverse: true})
			} else {
				rangeResult = tx.GetRange(fdb.KeyRange{Begin: fdb.Key(beginBytes), End: fdb.Key(endBytes)},
					fdb.RangeOptions{Reverse: true})
			}
		} else {
			if bytes.Compare(continuationEndpoint, beginBytes) < 0 {
				rangeResult = tx.GetRange(fdb.KeyRange{Begin: fdb.Key(beginBytes), End: fdb.Key(endBytes)},
					fdb.RangeOptions{Reverse: false})
			} else {
				// lastLessThan(continuationEndpoint) to firstGreaterOrEqual(endBytes)
				rangeResult = tx.GetRange(fdb.SelectorRange{
					Begin: fdb.LastLessThan(fdb.Key(continuationEndpoint)),
					End:   fdb.FirstGreaterOrEqual(fdb.Key(endBytes)),
				}, fdb.RangeOptions{Reverse: false})
			}
		}
	}

	it.rangeIter = rangeResult.Iterator()
	return it
}

// HasNext returns true if there are more entries to return.
func (it *BunchedMapMultiIterator) HasNext() bool {
	if it.done {
		return false
	}
	if it.nextEntry != nil {
		return true
	}
	it.advance()
	return it.nextEntry != nil
}

// Next returns the next entry and advances the iterator.
func (it *BunchedMapMultiIterator) Next() *BunchedMapScanEntry {
	if !it.HasNext() {
		return nil
	}
	entry := it.nextEntry
	it.lastKey = entry.Key
	it.nextEntry = nil
	it.returned++
	if it.limit > 0 && it.returned >= it.limit {
		it.done = true
	}
	return entry
}

// nextKV reads the next key-value from the FDB range iterator.
// Returns (kv, true) on success, (zero, false) when exhausted or on error
// (the error is recorded in iterErr and surfaced via Err()).
// Fires the KV callback if set.
func (it *BunchedMapMultiIterator) nextKV() (fdb.KeyValue, bool) {
	if !it.rangeIter.Advance() {
		// Advance()==false on exhaustion OR a transient FDB error (1007, timeout);
		// capture the stored Get() error so Err() surfaces it instead of looking like
		// clean end-of-data. This backs the live text-index scan (textCursor.Err()),
		// where swallowing it would silently truncate the result set.
		if _, err := it.rangeIter.Get(); err != nil {
			it.iterErr = err
		}
		return fdb.KeyValue{}, false
	}
	kv, err := it.rangeIter.Get()
	if err != nil {
		it.iterErr = err
		it.done = true
		return fdb.KeyValue{}, false
	}
	if it.callback != nil {
		it.callback(len(kv.Key), len(kv.Value))
	}
	return kv, true
}

// advance finds the next valid entry.
func (it *BunchedMapMultiIterator) advance() {
	for {
		// Try to get next entry from current bunch.
		if it.currentEntries != nil {
			idx := it.entryIndex
			if it.reverse {
				idx--
			} else {
				idx++
			}

			if idx >= 0 && idx < len(it.currentEntries) {
				it.entryIndex = idx
				e := it.currentEntries[idx]
				it.nextEntry = &BunchedMapScanEntry{
					Subspace:    it.currentSubspace,
					SubspaceTag: it.currentSubspaceTag,
					Key:         e.Key,
					Value:       e.Value,
				}
				return
			}
			// Exhausted this bunch — clear so we don't re-enter it.
			it.currentEntries = nil
		}

		// Need next KV from FDB range iterator (streaming).
		kv, ok := it.nextKV()
		if !ok {
			it.done = true
			return
		}

		// Check if this key is in the parent subspace.
		if !bytes.HasPrefix(kv.Key, it.parentKey) {
			cmp := bytes.Compare(kv.Key, it.parentKey)
			if (!it.reverse && cmp > 0) || (it.reverse && cmp < 0) {
				// Past the parent subspace — done.
				it.done = true
				return
			}
			// Haven't reached the subspace yet, skip.
			continue
		}

		// Determine which sub-subspace this key belongs to.
		nextSubspace, err := it.splitter.SubspaceOf(kv.Key)
		if err != nil {
			it.iterErr = err
			it.done = true
			return
		}
		nextSubspaceKey := nextSubspace.Bytes()
		nextSubspaceSuffix := make([]byte, len(nextSubspaceKey)-len(it.parentKey))
		copy(nextSubspaceSuffix, nextSubspaceKey[len(it.parentKey):])

		// Handle continuation.
		if !it.continuationSatisfied {
			if bytes.HasPrefix(it.continuation, nextSubspaceSuffix) {
				// This is the subspace we need to resume in.
				continuationKey, err := it.serializer.DeserializeKey(it.continuation, len(nextSubspaceSuffix), len(it.continuation)-len(nextSubspaceSuffix))
				if err != nil {
					it.iterErr = err
					it.done = true
					return
				}
				it.continuationSatisfied = true

				// Deserialize this bunch and find entries after the continuation key.
				boundaryKey, err := it.serializer.DeserializeKey(kv.Key, len(nextSubspaceKey), len(kv.Key)-len(nextSubspaceKey))
				if err != nil {
					it.iterErr = err
					it.done = true
					return
				}
				entries, err := it.serializer.DeserializeEntries(boundaryKey, kv.Value)
				if err != nil {
					it.iterErr = err
					it.done = true
					return
				}

				it.currentSubspace = nextSubspace
				it.currentSubspaceKey = nextSubspaceKey
				it.currentSubspaceSuffix = nextSubspaceSuffix
				tag, err := it.splitter.SubspaceTag(nextSubspace)
				if err != nil {
					it.iterErr = err
					it.done = true
					return
				}
				it.currentSubspaceTag = tag

				// Find the first entry strictly past the continuation key.
				startIdx := -1
				if it.reverse {
					for i := len(entries) - 1; i >= 0; i-- {
						if compareTuples(continuationKey, entries[i].Key) > 0 {
							startIdx = i
							break
						}
					}
				} else {
					for i := 0; i < len(entries); i++ {
						if compareTuples(continuationKey, entries[i].Key) < 0 {
							startIdx = i
							break
						}
					}
				}

				if startIdx >= 0 {
					it.currentEntries = entries
					it.entryIndex = startIdx
					e := entries[startIdx]
					it.nextEntry = &BunchedMapScanEntry{
						Subspace:    it.currentSubspace,
						SubspaceTag: it.currentSubspaceTag,
						Key:         e.Key,
						Value:       e.Value,
					}
					return
				}
				// No entries past the continuation key in this bunch.
				// Do NOT set currentEntries — the bunch is fully consumed.
				continue
			} else if bytes.Compare(nextSubspaceSuffix, it.continuation)*(boolToInt(!it.reverse)-boolToInt(it.reverse)) > 0 {
				// Past the continuation subspace, satisfied.
				it.continuationSatisfied = true
			} else {
				// Before the continuation subspace, skip.
				continue
			}
		}

		// Deserialize the bunch.
		boundaryKey, err := it.serializer.DeserializeKey(kv.Key, len(nextSubspaceKey), len(kv.Key)-len(nextSubspaceKey))
		if err != nil {
			it.iterErr = err
			it.done = true
			return
		}
		entries, err := it.serializer.DeserializeEntries(boundaryKey, kv.Value)
		if err != nil {
			it.iterErr = err
			it.done = true
			return
		}
		if len(entries) == 0 {
			continue
		}

		it.currentSubspace = nextSubspace
		it.currentSubspaceKey = nextSubspaceKey
		it.currentSubspaceSuffix = nextSubspaceSuffix
		tag, err := it.splitter.SubspaceTag(nextSubspace)
		if err != nil {
			it.iterErr = err
			it.done = true
			return
		}
		it.currentSubspaceTag = tag
		it.currentEntries = entries

		startIdx := 0
		if it.reverse {
			startIdx = len(entries) - 1
		}
		it.entryIndex = startIdx
		e := entries[startIdx]
		it.nextEntry = &BunchedMapScanEntry{
			Subspace:    it.currentSubspace,
			SubspaceTag: it.currentSubspaceTag,
			Key:         e.Key,
			Value:       e.Value,
		}
		return
	}
}

// GetContinuation returns a continuation token for resuming the scan.
// Returns nil if the scan is exhausted.
func (it *BunchedMapMultiIterator) GetContinuation() []byte {
	if it.lastKey == nil || it.currentSubspaceKey == nil {
		return nil
	}
	if it.done && (it.limit <= 0 || it.returned < it.limit) {
		// Exhausted the scan (not stopped by limit).
		return nil
	}
	// Continuation = subspaceSuffix + serializedKey
	return append(append([]byte{}, it.currentSubspaceSuffix...), it.serializer.SerializeKey(it.lastKey)...)
}

// Cancel stops the iterator.
func (it *BunchedMapMultiIterator) Cancel() {
	it.done = true
}

// Err returns the first error encountered during iteration, if any.
func (it *BunchedMapMultiIterator) Err() error {
	return it.iterErr
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
