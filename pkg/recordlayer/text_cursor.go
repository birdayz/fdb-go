package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
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
	underlying  *BunchedMapMultiIterator
	index       *Index
	scanProps   ScanProperties
	closed      bool
	lastResult  *RecordCursorResult[*IndexEntry]
	recordsRead int // entries returned (also this cursor's own free-initial-pass gate)

	// scanState is the (possibly shared) scanned-records/scanned-bytes/time
	// counter set — see ScanLimiterState's doc comment. Never nil.
	scanState *ScanLimiterState
}

// newTextCursorWithByteTracking creates a textCursor and a KVCallback that
// accumulates raw FDB bytes into the cursor's scanState.
// The callback must be passed to NewBunchedMapMultiIteratorWithCallback.
func newTextCursorWithByteTracking(index *Index, scanProps ScanProperties) (*textCursor, KVCallback) {
	c := &textCursor{
		index:     index,
		scanProps: scanProps,
		scanState: resolveScanLimiterState(scanProps.ExecuteProperties),
	}
	// Called synchronously from within textCursor.OnNext's own call into the
	// underlying iterator — never from a separate goroutine (see
	// ScanLimiterState's Concurrency doc: the whole leaf-cursor call chain is
	// single-threaded, pinned by pkg/recordlayer/query/executor's
	// package_invariant_test.go).
	callback := func(keyLen, valueLen int) {
		c.scanState.AddBytesScanned(int64(keyLen + valueLen))
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
func (c *textCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.lastResult != nil && !c.lastResult.HasNext() {
		return *c.lastResult, nil
	}

	// Honor a statement deadline / cancellation (RFC-106a) so a text scan is
	// bounded by the per-request timeout, not only by the per-page scan limits.
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}

	executeProps := c.scanProps.GetExecuteProperties()

	// Check the returned-row limit FIRST (RFC-106a): a MAX_ROWS/LIMIT-bounded
	// text scan must stop cleanly with ReturnLimitReached before the scan-record
	// backstop below can turn a satisfied row cap into a 54F01 (match index_scan).
	if executeProps.ReturnedRowLimit > 0 && c.recordsRead >= executeProps.ReturnedRowLimit {
		result := NewResultNoNext[*IndexEntry](ReturnLimitReached, c.limitContinuation())
		c.lastResult = &result
		return result, nil
	}

	// Check byte scan limit BEFORE reading next entry (free initial pass for first record).
	// Matches Java's CursorLimitManager.tryRecordScan() which checks
	// byteScanLimiter.hasBytesRemaining() with usedInitialPass guard.
	if executeProps.ScannedBytesLimit > 0 && c.recordsRead > 0 && c.scanState.BytesScanned() >= executeProps.ScannedBytesLimit {
		result, err := noNextOrFail[*IndexEntry](executeProps, ByteLimitReached, c.limitContinuation())
		if err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		c.lastResult = &result
		return result, nil
	}

	// Check time limit BEFORE reading next entry (free initial pass for first record — unconditional;
	// Java's time branch never suppresses it, even in fail mode). Matches Java's
	// CursorLimitManager.tryRecordScan(). noNextOrFail (not a raw NewResultNoNext): Java throws on
	// ANY halted limit under failOnScanLimitReached, not just the scanned-records one
	// (CursorLimitManager.java:141-144).
	if executeProps.TimeLimit > 0 && c.recordsRead > 0 && c.scanState.Elapsed() >= executeProps.TimeLimit {
		result, err := noNextOrFail[*IndexEntry](executeProps, TimeLimitReached, c.limitContinuation())
		if err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		c.lastResult = &result
		return result, nil
	}

	// Check scanned records limit BEFORE reading next entry (free initial pass, EXCEPT under
	// FailOnScanLimitReached — Java's record-scan branch never grants the free pass in fail mode,
	// CursorLimitManager.java:135-136). Threshold is the (possibly shared) scanState so every leg of
	// an IN-join/IN-union over a text index charges the same aggregate budget instead of resetting
	// per leg.
	if executeProps.ScannedRecordsLimit > 0 &&
		(c.recordsRead > 0 || executeProps.FailOnScanLimitReached) &&
		c.scanState.RecordsScanned() >= executeProps.ScannedRecordsLimit {
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
	c.scanState.AddRecordScanned()

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
