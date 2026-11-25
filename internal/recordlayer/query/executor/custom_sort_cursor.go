// Portions derived from FoundationDB Record Layer (AggregateCursor.java,
// StreamGrouping.java, FlatMapPipelinedCursor.java,
// NumericAggregationValue.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer"
)

// SortBufferExceededError is returned when an in-memory sort
// materializes more rows than the configured limit. Prevents OOM
// on unbounded ORDER BY without LIMIT.
type SortBufferExceededError struct {
	Rows  int
	Limit int
}

func (e *SortBufferExceededError) Error() string {
	return fmt.Sprintf("sort buffer exceeded: %d rows materialized (limit %d); add LIMIT or use an ordered index", e.Rows, e.Limit)
}

// ---------------------------------------------------------------------------
// customSortCursor — streaming sort with pluggable comparator
// ---------------------------------------------------------------------------

// customSortCursor implements RecordCursor[QueryResult] for ORDER BY.
// Two phases: LOAD (pull from inner into buffer) and EMIT (return
// sorted records one-by-one). When the inner cursor hits a limit
// during LOAD, the buffer and limit are propagated upward via the sort
// continuation so the next transaction can continue loading into the
// same buffer. Mirrors Java's MemorySortCursor; the comparator is a
// pluggable sortFn evaluating the plan's sort-key Values against each
// row's positional row.

type customSortCursor struct {
	inner  recordlayer.RecordCursor[QueryResult]
	sortFn func([]QueryResult) error

	buf     []QueryResult
	loaded  bool
	emitIdx int
	closed  bool
	maxBuf  int                       // 0 = use DefaultMaxSortBufferRows
	st      *recordlayer.ExecuteState // RFC-130 statement memory budget
	// lastNoNext replays a fill-phase out-of-band stop on a contract-violating
	// re-call (Java's cached no-next result). Without it a re-call re-enters
	// the fill loop and pulls MORE inner rows into a buffer whose contents were
	// already serialized into the reported continuation — skewing the resume.
	// The emit phase needs no cache: loaded=true makes its exhaustion terminal
	// idempotent without touching the inner.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
	// chargedBytes is what THIS cursor has charged against st, released on
	// Close. The statement budget bounds LIVE buffered bytes: the buffer
	// round-trips through the page continuation and is re-charged at resume
	// injection (chargeResumedBuffer), so without the teardown release a
	// statement-wide ExecuteState would re-accumulate the same buffer once
	// per page and fail a compliant query for crossing page boundaries.
	chargedBytes int64
}

// DefaultMaxSortBufferRows is the maximum number of rows the in-memory
// sort cursor will materialize before returning an error. Prevents OOM
// on queries that sort unbounded result sets without LIMIT. Override per
// cursor via the maxBuf field.
const DefaultMaxSortBufferRows = 5_000_000

func newCustomSortCursor(
	inner recordlayer.RecordCursor[QueryResult],
	sortFn func([]QueryResult) error,
	st *recordlayer.ExecuteState,
) *customSortCursor {
	return &customSortCursor{
		inner:  inner,
		sortFn: sortFn,
		st:     st,
	}
}

func (c *customSortCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf("cursor is closed")
	}
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	if c.loaded {
		return c.emitNext()
	}
	limit := c.maxBuf
	if limit <= 0 {
		limit = DefaultMaxSortBufferRows
	}
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if !result.HasNext() {
			reason := result.GetNoNextReason()
			if reason == recordlayer.SourceExhausted {
				if sortErr := c.sortFn(c.buf); sortErr != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, sortErr
				}
				c.loaded = true
				return c.emitNext()
			}
			contBytes, encErr := encodeSortContinuation(
				result.GetContinuation(), c.buf, false,
			)
			if encErr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, encErr
			}
			res := recordlayer.NewResultNoNext[QueryResult](
				reason, recordlayer.NewBytesContinuation(contBytes),
			)
			c.lastNoNext = &res
			return res, nil
		}
		v := result.GetValue()
		// RFC-130: charge each row's bytes against the statement memory budget
		// before keeping it in the sort buffer.
		if c.st.HasMemLimit() {
			n := estimateQueryResultBytes(v)
			if err := c.st.ChargeMemory(n); err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
			c.chargedBytes += n
		}
		c.buf = append(c.buf, v)
		if len(c.buf) >= limit {
			return recordlayer.RecordCursorResult[QueryResult]{}, &SortBufferExceededError{
				Rows:  len(c.buf),
				Limit: limit,
			}
		}
	}
}

func (c *customSortCursor) emitNext() (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.emitIdx >= len(c.buf) {
		return recordlayer.NewResultNoNext[QueryResult](
			recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
		), nil
	}
	row := c.buf[c.emitIdx]
	c.emitIdx++
	// The emitted row's continuation is the REMAINING sorted buffer plus the
	// inner-EXHAUSTED marker (emit only starts once the inner is exhausted):
	// a resume restores the remainder and goes straight to emit
	// (executeInMemorySort sets loaded=true), never re-running the inner.
	// buf is append-only during fill and immutable during emit, so the
	// sub-slice stays valid for the lazy encode.
	return recordlayer.NewResultWithValue(row, &sortEmitContinuation{remaining: c.buf[c.emitIdx:]}), nil
}

// sortEmitContinuation lazily encodes the sort EMIT-PHASE continuation at
// ToBytes() time (the flatMapCursorContinuation pattern — an encode failure
// propagates instead of being swallowed): (inner-EXHAUSTED marker, remaining
// sorted rows) via the F53 typed sort codec. Go extension — Java's Cascades
// has no physical sort; Java's own MemorySortCursor instead re-scans the input
// filtered by minimum_key into a key-deduped map, which a slice buffer cannot
// mirror without duplicating rows.
type sortEmitContinuation struct {
	remaining []QueryResult
}

func (w *sortEmitContinuation) ToBytes() ([]byte, error) {
	return encodeSortContinuation(nil, w.remaining, true)
}

func (w *sortEmitContinuation) IsEnd() bool { return false }

// chargeResumedBuffer charges rows restored from a sort continuation against
// the statement memory budget (RFC-180 H4): a resumed buffer is memory like
// any other, and for a FRESH ExecuteState this is its only accounting. For the
// statement-wide state shared across pages the matching release happens in
// Close — see chargedBytes.
func (c *customSortCursor) chargeResumedBuffer(rows []QueryResult) error {
	if !c.st.HasMemLimit() {
		return nil
	}
	for _, r := range rows {
		n := estimateQueryResultBytes(r)
		if err := c.st.ChargeMemory(n); err != nil {
			return err
		}
		c.chargedBytes += n
	}
	return nil
}

func (c *customSortCursor) Close() error {
	if !c.closed && c.chargedBytes > 0 {
		c.st.ReleaseMemory(c.chargedBytes)
		c.chargedBytes = 0
	}
	c.closed = true
	return c.inner.Close()
}

func (c *customSortCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*customSortCursor)(nil)
