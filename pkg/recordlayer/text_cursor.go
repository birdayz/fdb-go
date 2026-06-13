package recordlayer

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// textCursor wraps a BunchedMapMultiIterator to implement RecordCursor[*IndexEntry].
// Matches Java's TextCursor, which uses CursorLimitManager to enforce record scan,
// time, and byte scan limits.
//
// ByteScanLimiter is tracked via a KVCallback on the BunchedMapMultiIterator.
// The callback fires per raw FDB key-value read (before deserialization) and
// accumulates key+value bytes into bytesScanned. This matches Java's approach
// of passing a Consumer<KeyValue> to scanMulti() that calls
// byteScanLimiter.registerScannedBytes(key.length + value.length).
type textCursor struct {
	underlying   *BunchedMapMultiIterator
	index        *Index
	scanProps    ScanProperties
	closed       bool
	lastResult   *RecordCursorResult[*IndexEntry]
	recordsRead  int       // entries returned (for scan limit tracking)
	bytesScanned int64     // raw FDB bytes read (accumulated via callback)
	startTime    time.Time // for time limit enforcement
}

// newTextCursorWithByteTracking creates a textCursor and a KVCallback that
// accumulates raw FDB bytes into the cursor's bytesScanned counter.
// The callback must be passed to NewBunchedMapMultiIteratorWithCallback.
func newTextCursorWithByteTracking(index *Index, scanProps ScanProperties) (*textCursor, KVCallback) {
	c := &textCursor{
		index:     index,
		scanProps: scanProps,
		startTime: time.Now(),
	}
	// Use atomic add so the callback is safe even though in practice
	// the iterator and cursor run on the same goroutine.
	callback := func(keyLen, valueLen int) {
		atomic.AddInt64(&c.bytesScanned, int64(keyLen+valueLen))
	}
	return c, callback
}

// setUnderlying sets the iterator after construction.
func (c *textCursor) setUnderlying(it *BunchedMapMultiIterator) {
	c.underlying = it
}

// OnNext returns the next IndexEntry from the text index scan.
// The key contains: [groupingColumns..., token, primaryKeyColumns...]
// The value contains: Tuple(positionList) (or empty tuple if positions omitted).
//
// Matches Java's TextCursor.onNext() which checks limitManager.tryRecordScan()
// before each entry. The "free initial pass" pattern allows at least one record
// before enforcing scan/time limits (matching CursorLimitManager.usedInitialPass).
func (c *textCursor) OnNext(_ context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.lastResult != nil && !c.lastResult.HasNext() {
		return *c.lastResult, nil
	}

	executeProps := c.scanProps.GetExecuteProperties()

	// Check byte scan limit BEFORE reading next entry (free initial pass for first record).
	// Matches Java's CursorLimitManager.tryRecordScan() which checks
	// byteScanLimiter.hasBytesRemaining() with usedInitialPass guard.
	if executeProps.ScannedBytesLimit > 0 && c.recordsRead > 0 && atomic.LoadInt64(&c.bytesScanned) >= executeProps.ScannedBytesLimit {
		result, err := noNextOrFail[*IndexEntry](executeProps, ByteLimitReached, c.limitContinuation())
		if err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		c.lastResult = &result
		return result, nil
	}

	// Check time limit BEFORE reading next entry (free initial pass for first record).
	// Matches Java's CursorLimitManager.tryRecordScan() which checks
	// timeScanLimiter with usedInitialPass guard.
	if executeProps.TimeLimit > 0 && c.recordsRead > 0 && time.Since(c.startTime) >= executeProps.TimeLimit {
		result := NewResultNoNext[*IndexEntry](TimeLimitReached, c.limitContinuation())
		c.lastResult = &result
		return result, nil
	}

	// Check scanned records limit BEFORE reading next entry (free initial pass).
	// Matches Java's CursorLimitManager.tryRecordScan() which checks
	// recordScanLimiter with usedInitialPass guard.
	if executeProps.ScannedRecordsLimit > 0 && c.recordsRead >= executeProps.ScannedRecordsLimit {
		result, err := noNextOrFail[*IndexEntry](executeProps, ScanLimitReached, c.limitContinuation())
		if err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		c.lastResult = &result
		return result, nil
	}

	if c.closed || !c.underlying.HasNext() {
		// Check for deserialization or I/O errors from the iterator.
		if err := c.underlying.Err(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		contBytes := c.underlying.GetContinuation()
		if contBytes == nil {
			// Truly exhausted — no more data.
			result := NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{})
			c.lastResult = &result
			return result, nil
		}
		// Stopped by limit — can resume with continuation.
		result := NewResultNoNext[*IndexEntry](ReturnLimitReached, &BytesContinuation{bytes: contBytes})
		c.lastResult = &result
		return result, nil
	}

	entry := c.underlying.Next()

	// Build the index entry key: subspaceTag (grouping key) + entry key (document ID)
	var k tuple.Tuple
	if entry.SubspaceTag != nil {
		k = make(tuple.Tuple, 0, len(entry.SubspaceTag)+len(entry.Key))
		k = append(k, entry.SubspaceTag...)
		k = append(k, entry.Key...)
	} else {
		k = entry.Key
	}

	// Value is Tuple.from(positionList) — position list elements as a nested tuple.
	positionTuple := make(tuple.Tuple, len(entry.Value))
	for i, v := range entry.Value {
		positionTuple[i] = int64(v)
	}
	valueTuple := tuple.Tuple{positionTuple}

	indexEntry := &IndexEntry{
		Index: c.index,
		Key:   k,
		Value: valueTuple,
	}

	c.recordsRead++

	cont := c.makeContinuation()
	result := NewResultWithValue(indexEntry, cont)
	c.lastResult = &result
	return result, nil
}

// makeContinuation creates a continuation from the iterator state.
func (c *textCursor) makeContinuation() RecordCursorContinuation {
	contBytes := c.underlying.GetContinuation()
	if contBytes == nil {
		return &EndContinuation{}
	}
	return &BytesContinuation{bytes: contBytes}
}

// limitContinuation returns the appropriate continuation when a scan/time limit
// is hit. If the iterator has a continuation, use it so scanning can resume.
// Otherwise, return StartContinuation (no position info but NOT end of iteration).
func (c *textCursor) limitContinuation() RecordCursorContinuation {
	contBytes := c.underlying.GetContinuation()
	if contBytes != nil {
		return &BytesContinuation{bytes: contBytes}
	}
	return &StartContinuation{}
}

// Close releases resources.
func (c *textCursor) Close() error {
	c.closed = true
	c.underlying.Cancel()
	return nil
}

func (c *textCursor) IsClosed() bool { return c.closed }

// Ensure textCursor implements RecordCursor[*IndexEntry].
var _ RecordCursor[*IndexEntry] = (*textCursor)(nil)
