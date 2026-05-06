package recordlayer

import (
	"context"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// recordKeyCursor scans the records subspace and returns only primary keys,
// without deserializing record data. Split records (multiple KV pairs per
// primary key) are deduplicated so each PK is returned exactly once.
//
// Matches Java's FDBRecordStore.scanRecordKeys() which uses a
// DedupCursor over a KeyValueCursor mapped to primary keys.
type recordKeyCursor struct {
	store          *FDBRecordStore
	continuation   []byte
	scanProperties ScanProperties

	// Internal state
	iterator      *fdb.RangeIterator
	closed        bool
	keysReturned  int
	keysScanned   int
	bytesScanned  int64
	prefixLength  int
	startTime     time.Time
	lastPK        tuple.Tuple // for dedup of adjacent duplicate PKs
	peekedHasMore *bool       // non-nil when hasMore() has been called but result not consumed
}

func (c *recordKeyCursor) OnNext(ctx context.Context) (RecordCursorResult[tuple.Tuple], error) {
	if c.closed {
		return RecordCursorResult[tuple.Tuple]{}, fmt.Errorf("cursor is closed")
	}

	if c.iterator == nil {
		if err := c.initIterator(); err != nil {
			return RecordCursorResult[tuple.Tuple]{}, err
		}
	}

	ep := c.scanProperties.GetExecuteProperties()

	// Row limit check
	if ep.ReturnedRowLimit > 0 && c.keysReturned >= ep.ReturnedRowLimit {
		if c.hasMore() {
			return NewResultNoNext[tuple.Tuple](ReturnLimitReached, &BytesContinuation{bytes: c.continuation}), nil
		}
		return NewResultNoNext[tuple.Tuple](SourceExhausted, &EndContinuation{}), nil
	}

	// Time limit
	if ep.TimeLimit > 0 && c.keysScanned > 0 && time.Since(c.startTime) >= ep.TimeLimit {
		return c.noNextWithCont(TimeLimitReached), nil
	}

	// Scan limit
	if ep.ScannedRecordsLimit > 0 && c.keysScanned >= ep.ScannedRecordsLimit {
		return c.noNextWithCont(ScanLimitReached), nil
	}

	// Byte limit
	if ep.ScannedBytesLimit > 0 && c.keysScanned > 0 && c.bytesScanned > ep.ScannedBytesLimit {
		return c.noNextWithCont(ByteLimitReached), nil
	}

	recordsSubspace := c.store.subspace.Sub(RecordKey)

	for {
		hasNext := false
		if c.peekedHasMore != nil {
			hasNext = *c.peekedHasMore
			c.peekedHasMore = nil
		} else {
			hasNext = c.iterator.Advance()
		}
		if !hasNext {
			return NewResultNoNext[tuple.Tuple](SourceExhausted, &EndContinuation{}), nil
		}

		kv, err := c.iterator.Get()
		if err != nil {
			return RecordCursorResult[tuple.Tuple]{}, fmt.Errorf("record key cursor: get: %w", err)
		}

		c.bytesScanned += int64(len(kv.Key) + len(kv.Value))

		// Unpack the key relative to the records subspace: (pk..., suffix)
		keyTuple, err := fastSubspaceUnpack(kv.Key, len(recordsSubspace.Bytes()))
		if err != nil || len(keyTuple) < 2 {
			continue // skip unparseable keys
		}

		// Extract PK by stripping the suffix (last element)
		pk := tuple.Tuple(keyTuple[:len(keyTuple)-1])

		// Dedup: skip if same PK as last returned
		if c.lastPK != nil && tuplesEqual(pk, c.lastPK) {
			continue
		}

		c.keysScanned++
		c.lastPK = pk

		// Update continuation: raw key suffix after the records subspace prefix
		if len(kv.Key) > c.prefixLength {
			c.continuation = kv.Key[c.prefixLength:]
		}

		c.keysReturned++

		return NewResultWithValue(pk, &BytesContinuation{bytes: c.continuation}), nil
	}
}

func (c *recordKeyCursor) noNextWithCont(reason NoNextReason) RecordCursorResult[tuple.Tuple] {
	if c.continuation != nil {
		return NewResultNoNext[tuple.Tuple](reason, &BytesContinuation{bytes: c.continuation})
	}
	return NewResultNoNext[tuple.Tuple](reason, &StartContinuation{})
}

func (c *recordKeyCursor) hasMore() bool {
	if c.iterator == nil {
		return false
	}
	result := c.iterator.Advance()
	c.peekedHasMore = &result
	return result
}

func (c *recordKeyCursor) initIterator() error {
	recordsSubspace := c.store.subspace.Sub(RecordKey)
	c.prefixLength = len(recordsSubspace.FDBKey())

	beginKey, endKey := recordsSubspace.FDBRangeKeys()
	begin := beginKey.FDBKey()
	end := endKey.FDBKey()

	if c.continuation != nil {
		innerCont := unwrapContinuation(c.continuation)
		fullKey := append(recordsSubspace.FDBKey(), innerCont...)
		if c.scanProperties.IsReverse() {
			// Reverse: continuation caps the high end (exclude already-returned keys)
			end = fullKey
		} else {
			// Forward: continuation raises the low end (skip already-returned keys)
			begin = append(fullKey, 0x00)
		}

		// Initialize lastPK from continuation for split record dedup.
		// Continuation is tuple-packed (pk..., suffix) — strip the last element.
		if contTuple, err := fastUnpack(fdb.Key(innerCont)); err == nil && len(contTuple) >= 2 {
			c.lastPK = tuple.Tuple(contTuple[:len(contTuple)-1])
		}
	}

	rng := fdb.KeyRange{Begin: begin, End: end}
	options := fdb.RangeOptions{
		Mode:    c.scanProperties.CursorStreamingMode.ToFDB(),
		Reverse: c.scanProperties.IsReverse(),
	}

	tx := c.store.context.Transaction()
	var rangeResult fdb.RangeResult
	if c.scanProperties.ExecuteProperties.IsolationLevel == SnapshotIsolation {
		rangeResult = tx.Snapshot().GetRange(rng, options)
	} else {
		rangeResult = tx.GetRange(rng, options)
	}

	c.iterator = rangeResult.Iterator()
	return nil
}

func (c *recordKeyCursor) Close() error {
	c.closed = true
	return nil
}

func (c *recordKeyCursor) IsClosed() bool { return c.closed }
