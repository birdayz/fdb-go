package recordlayer

import (
	"context"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// textCursor wraps a BunchedMapMultiIterator to implement RecordCursor[*IndexEntry].
// Matches Java's TextCursor, which uses CursorLimitManager to enforce record scan,
// time, and byte scan limits.
//
// Note: ByteScanLimiter is not tracked here because BunchedMap entries are already
// deserialized by the time they reach this cursor. Accurate byte tracking would
// require deeper integration with BunchedMapMultiIterator to report raw FDB bytes
// read. ScannedRecordsLimit and TimeLimit are fully enforced.
type textCursor struct {
	underlying  *BunchedMapMultiIterator
	index       *Index
	scanProps   ScanProperties
	closed      bool
	lastResult  *RecordCursorResult[*IndexEntry]
	recordsRead int       // entries returned (for scan limit tracking)
	startTime   time.Time // for time limit enforcement
}

func newTextCursor(underlying *BunchedMapMultiIterator, index *Index, scanProps ScanProperties) *textCursor {
	return &textCursor{
		underlying: underlying,
		index:      index,
		scanProps:  scanProps,
		startTime:  time.Now(),
	}
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
		result := NewResultNoNext[*IndexEntry](ScanLimitReached, c.limitContinuation())
		c.lastResult = &result
		return result, nil
	}

	if c.closed || !c.underlying.HasNext() {
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

// Ensure textCursor implements RecordCursor[*IndexEntry].
var _ RecordCursor[*IndexEntry] = (*textCursor)(nil)
