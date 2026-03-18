package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

// textCursor wraps a BunchedMapMultiIterator to implement RecordCursor[*IndexEntry].
// Matches Java's TextCursor.
type textCursor struct {
	underlying *BunchedMapMultiIterator
	index      *Index
	closed     bool
	lastResult *RecordCursorResult[*IndexEntry]
}

func newTextCursor(underlying *BunchedMapMultiIterator, index *Index) *textCursor {
	return &textCursor{
		underlying: underlying,
		index:      index,
	}
}

// OnNext returns the next IndexEntry from the text index scan.
// The key contains: [groupingColumns..., token, primaryKeyColumns...]
// The value contains: Tuple(positionList) (or empty tuple if positions omitted).
func (c *textCursor) OnNext(_ context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.lastResult != nil && !c.lastResult.HasNext() {
		return *c.lastResult, nil
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

// Close releases resources.
func (c *textCursor) Close() error {
	c.closed = true
	c.underlying.Cancel()
	return nil
}

// Ensure textCursor implements RecordCursor[*IndexEntry].
var _ RecordCursor[*IndexEntry] = (*textCursor)(nil)
