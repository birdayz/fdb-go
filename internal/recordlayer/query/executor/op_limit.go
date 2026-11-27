// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"
	"math"

	"fdb.dev/internal/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/recordlayer"
)

// executeLimit implements LIMIT/OFFSET. Go-only SQL extension — Java
// uses ExecuteProperties.ReturnedRowLimit set at the JDBC layer instead.
//
// Optimization: propagates the effective row limit (limit + offset)
// into the inner plan's ExecuteProperties so downstream scans stop
// reading from FDB after enough records are produced. This avoids
// reading the full table when only N rows are needed.
func executeLimit(
	ctx context.Context,
	p *plans.RecordQueryLimitPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	children := p.GetChildren()
	if len(children) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}

	limit := int(p.GetLimit())
	offset := int(p.GetOffset())

	// Runtime row cap (RFC-156 parameterized vector rank limit `... <= ?`):
	// evaluate the Value against the bound parameters. The Value already carries
	// the rank adjustment (K for rank<=K, K-1 for rank<K), so its result is the
	// final cap. A non-positive cap (e.g. K-1 with K=1, i.e. ROW_NUMBER() < 1)
	// yields 0 rows — the same EMPTY result a literal Limit(0) gives.
	if lv := p.GetLimitValue(); lv != nil {
		ev, err := lv.Evaluate(evalCtx)
		if err != nil {
			return nil, fmt.Errorf("limit value: %w", err)
		}
		n, ok := toLimitInt(ev)
		if !ok {
			return nil, fmt.Errorf("limit value is not an integer (%T)", ev)
		}
		if n < 0 {
			n = 0
		}
		limit = n
	}

	// RFC-128 §3.3: envelope the LIMIT continuation so the skip/limit state
	// survives the per-page transaction rollover that paginatingRows does.
	// The shared skipCursor/limitRowsCursor (cursor_combinators.go, driven by
	// applySkipLimit) forward the inner continuation with no
	// skip/limit bookkeeping — exactly like Java's SkipCursor/RowLimitedCursor
	// — so resuming them re-skips `offset` and resets `limit`. We therefore
	// keep those byte-identical and confine the envelope to THIS operator: a
	// LIMIT-specific continuation that records {inner continuation, remaining
	// offset, remaining limit}. On resume we decode it, drive the child from
	// the inner continuation, and continue skipping/limiting from where the
	// previous page stopped — never re-skipping, never resetting the cap.
	innerCont, remOffset, remLimit, decErr := decodeLimitContinuation(continuation, offset, limit)
	if decErr != nil {
		return nil, fmt.Errorf("invalid limit continuation: %w", decErr)
	}

	// Go-only extension: propagate the effective row limit to the inner plan
	// so downstream scans stop early. The child must produce remOffset rows to
	// skip PLUS however many this LIMIT may emit. Under an existing parent
	// returned-row cap (e.g. MAX_ROWS) the LIMIT emits at most that many
	// post-offset. Include any request skip in that emission budget, but apply
	// the request skip and cap OUTSIDE the semantic envelope so skipped rows
	// consume the semantic cap too. For a finite request cap the child budget
	// is remOffset + min(remLimit, parentSkip+parentCap)
	// — NOT min(remOffset+remLimit, parentCap), which would stop the child
	// before it skips the offset (`SELECT COUNT(*) FROM t LIMIT 1 OFFSET 1`
	// under MAX_ROWS=1 erroring on resume instead of returning 0 rows).
	innerProps := props.ClearSkipAndAdjustLimit()
	innerProps.ReturnedRowLimit = limitChildRowLimit(innerProps.ReturnedRowLimit, remOffset, remLimit)

	innerCursor, err := ExecutePlan(ctx, children[0], store, evalCtx, innerCont, innerProps)
	if err != nil {
		return nil, err
	}

	return applySkipLimit(newLimitEnvelopeCursor(innerCursor, remOffset, remLimit), props.Skip, props.ReturnedRowLimit), nil
}

// limitChildRowLimit computes the read budget below a semantic LIMIT. Offsets
// have been validated by plan construction or continuation decoding. Unlike a
// semantic skip, this budget may saturate: reaching it yields a resumable page
// boundary. Wrapping it negative would silently disable the finite read budget.
func limitChildRowLimit(parentCap, remOffset, remLimit int) int {
	emit := remLimit // <0 == unbounded (OFFSET-only)
	if parentCap > 0 && (emit < 0 || parentCap < emit) {
		emit = parentCap
	}
	if emit < 0 {
		return parentCap
	}
	if remOffset > math.MaxInt-emit {
		return math.MaxInt
	}
	return remOffset + emit
}

// limitEnvelopeCursor performs RFC-128's LIMIT/OFFSET (skip `remOffset`, then
// emit at most `remLimit`) over its inner cursor, AND wraps each result's
// continuation in a LimitContinuation so a cross-page resume continues from the
// exact skip/limit position instead of re-skipping/re-limiting. It deliberately
// re-implements skip-then-limit inline (rather than reusing the shared
// SkipCursor/RowLimitedCursor) so it can observe each skip and emit and record
// the remaining counts — the shared combinators are kept byte-identical to Java
// because executor operators drive them generically via applySkipLimit.
type limitEnvelopeCursor struct {
	inner     recordlayer.RecordCursor[QueryResult]
	remOffset int
	remLimit  int
	// unbounded means "no row cap, OFFSET only" — the LIMIT was negative
	// (LogicalLimit.Limit < 0). SQL `OFFSET n` without a LIMIT is not valid in
	// this grammar, so this is currently unreachable from SQL; the guard keeps
	// a negative limit from collapsing the result to empty if a future caller
	// ever produces one.
	unbounded bool
	// terminal caches a sticky no-next result (matching RowLimitedCursor's
	// cached-terminal behavior): once set, every further OnNext returns it.
	// Holds BOTH the exhaustion terminal and an out-of-band stop — Java caches
	// every no-next result, so a re-call replays verbatim and never re-pulls
	// the inner.
	terminal *recordlayer.RecordCursorResult[QueryResult]
	closed   bool
}

func newLimitEnvelopeCursor(inner recordlayer.RecordCursor[QueryResult], remOffset, remLimit int) *limitEnvelopeCursor {
	if remOffset < 0 {
		remOffset = 0
	}
	return &limitEnvelopeCursor{
		inner:     inner,
		remOffset: remOffset,
		remLimit:  remLimit,
		unbounded: remLimit < 0,
	}
}

func (c *limitEnvelopeCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.terminal != nil {
		return *c.terminal, nil
	}

	// Limit fully consumed (also covers LIMIT 0 and a resume with remLimit==0):
	// the operator is EXHAUSTED — it will never emit another row — so it stops
	// with SourceExhausted+EndContinuation. This is the correct terminal in the
	// paginatingRows architecture: an end continuation ends the page drain (a
	// resumable continuation here would loop forever, since each fresh page
	// would rebuild an already-empty window). Java's RowLimitedCursor returns
	// the in-band RETURN_LIMIT_REACHED and lets the client driver stop; in our
	// page-drain driver the equivalent "nothing more, ever" signal is end.
	if !c.unbounded && c.remLimit <= 0 {
		return c.exhaust(), nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if !result.HasNext() {
			reason := result.GetNoNextReason()
			if reason == recordlayer.SourceExhausted {
				// Inner drained: the LIMIT is genuinely exhausted, no resume.
				return c.exhaust(), nil
			}
			// Inner stopped out-of-band (page/scan boundary): envelope the
			// inner continuation with the CURRENT remaining offset/limit so the
			// next page resumes mid-window (the next request opens a fresh
			// cursor from this continuation). Cached in terminal so a
			// contract-violating re-call on THIS instance replays it verbatim
			// (Java's cached no-next result) instead of re-pulling the inner.
			res := recordlayer.NewResultNoNext[QueryResult](
				reason, &limitEnvelopeContinuation{inner: result.GetContinuation(), remOffset: c.remOffset, remLimit: c.remLimit},
			)
			c.terminal = &res
			return res, nil
		}

		if c.remOffset > 0 {
			c.remOffset--
			continue // OFFSET: skip this row.
		}

		// Emit this row. After the emit one fewer row remains in the window;
		// the envelope records the inner continuation positioned PAST this row
		// plus remOffset==0 and the decremented remLimit, so a resume neither
		// re-skips nor re-emits it. (Unbounded: remLimit stays negative, the
		// cap never fires — OFFSET-only stream.)
		if !c.unbounded {
			c.remLimit--
		}
		return recordlayer.NewResultWithValue(result.GetValue(), &limitEnvelopeContinuation{
			inner: result.GetContinuation(), remOffset: 0, remLimit: c.remLimit,
		}), nil
	}
}

// exhaust builds and caches the sticky terminal SourceExhausted result. Once the
// LIMIT window is fully emitted (or empty), the operator is done for good — the
// page-drain driver stops on the EndContinuation.
func (c *limitEnvelopeCursor) exhaust() recordlayer.RecordCursorResult[QueryResult] {
	res := recordlayer.NewResultNoNext[QueryResult](
		recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
	)
	c.terminal = &res
	return res
}

func (c *limitEnvelopeCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *limitEnvelopeCursor) IsClosed() bool { return c.closed }

// LimitContinuation is the RFC-128 §3.3 envelope for a RecordQueryLimitPlan's
// continuation: the inner cursor's continuation plus the skip/limit window left
// to apply. It is Go-only and INTERNAL to executeLimit — it never becomes a SQL
// resume token or a wire/Java-interop continuation (no .proto, no
// magic-6773487359078157740 surface). The encoding is a hand-rolled
// length-prefixed blob, sufficient because it round-trips only within this
// process across paginatingRows' per-page transaction rollover.
//
// Layout (all integers big-endian):
//
//	[1]   version byte (limitContVersion)
//	[8]   remaining offset (int64)
//	[8]   remaining limit  (int64)
//	[4]   inner continuation length (uint32; 0xFFFFFFFF == "nil/no inner")
//	[...] inner continuation bytes
const limitContVersion byte = 1

// limitContNilInner marks an absent inner continuation (start-from-begin),
// distinct from a present-but-empty inner continuation (length 0).
const limitContNilInner uint32 = 0xFFFFFFFF

// limitEnvelopeContinuation snapshots the window without serializing its child.
// In particular, a sort's continuation owns the remaining rows: encoding it on
// every emission would repeatedly serialize the same tail. Like Java's
// RowLimitedCursor and SkipCursor, retain the immutable continuation object
// until a consumer requests bytes, independently of cursor advancement/closure.
type limitEnvelopeContinuation struct {
	inner     recordlayer.RecordCursorContinuation
	remOffset int
	remLimit  int
}

func (c *limitEnvelopeContinuation) ToBytes() ([]byte, error) {
	return encodeLimitContinuation(c.inner, c.remOffset, c.remLimit)
}

func (c *limitEnvelopeContinuation) IsEnd() bool { return false }

func encodeLimitContinuation(innerCont recordlayer.RecordCursorContinuation, remOffset, remLimit int) ([]byte, error) {
	var innerBytes []byte
	haveInner := false
	if innerCont != nil && !innerCont.IsEnd() {
		b, err := innerCont.ToBytes()
		if err != nil {
			return nil, err
		}
		// ToBytes returns nil for Start/End-like positions; treat a nil byte
		// slice as "no inner continuation" so resume starts the child fresh.
		if b != nil {
			innerBytes = b
			haveInner = true
		}
	}

	buf := make([]byte, 0, 1+8+8+4+len(innerBytes))
	buf = append(buf, limitContVersion)
	buf = appendInt64BE(buf, int64(remOffset))
	buf = appendInt64BE(buf, int64(remLimit))
	if haveInner {
		buf = appendUint32BE(buf, uint32(len(innerBytes)))
		buf = append(buf, innerBytes...)
	} else {
		buf = appendUint32BE(buf, limitContNilInner)
	}
	return buf, nil
}

// decodeLimitContinuation parses a LimitContinuation. An empty continuation
// (first page) yields (nil inner, fullOffset, fullLimit).
func decodeLimitContinuation(continuation []byte, fullOffset, fullLimit int) (innerCont []byte, remOffset, remLimit int, err error) {
	if len(continuation) == 0 {
		return nil, fullOffset, fullLimit, nil
	}
	if len(continuation) < 1+8+8+4 {
		return nil, 0, 0, fmt.Errorf("limit continuation too short: %d bytes", len(continuation))
	}
	if continuation[0] != limitContVersion {
		return nil, 0, 0, fmt.Errorf("unknown limit continuation version %d", continuation[0])
	}
	pos := 1
	ro := int64(readUint64BE(continuation[pos:]))
	if ro < 0 {
		return nil, 0, 0, &expressions.InvalidLimitOffsetError{Offset: ro}
	}
	pos += 8
	// Negative remaining limits are legitimate no-cap sentinels, unlike offsets.
	rl := int64(readUint64BE(continuation[pos:]))
	pos += 8
	innerLen := readUint32BE(continuation[pos:])
	pos += 4
	if innerLen == limitContNilInner {
		// Defence-in-depth: a nil-inner envelope must have no trailing bytes
		// (the encoder writes none). Reject a malformed continuation rather than
		// silently ignoring junk — symmetric with the length check below.
		if pos != len(continuation) {
			return nil, 0, 0, fmt.Errorf("limit continuation: %d trailing bytes after nil inner", len(continuation)-pos)
		}
		return nil, int(ro), int(rl), nil
	}
	if int64(pos)+int64(innerLen) != int64(len(continuation)) {
		return nil, 0, 0, fmt.Errorf("limit continuation inner length mismatch: want %d, have %d", innerLen, len(continuation)-pos)
	}
	inner := make([]byte, innerLen)
	copy(inner, continuation[pos:])
	return inner, int(ro), int(rl), nil
}

func appendInt64BE(b []byte, v int64) []byte {
	return appendUint64BE(b, uint64(v))
}

func appendUint64BE(b []byte, v uint64) []byte {
	return append(b,
		byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32),
		byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func appendUint32BE(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func readUint64BE(b []byte) uint64 {
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}

func readUint32BE(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// applySkipLimit wraps a cursor with skip/limit only when the values
// are meaningful. ReturnedRowLimit <= 0 means unlimited (matching
// DefaultExecuteProperties convention).
func applySkipLimit(cursor recordlayer.RecordCursor[QueryResult], skip, limit int) recordlayer.RecordCursor[QueryResult] {
	if skip > 0 {
		cursor = recordlayer.SkipCursor(cursor, skip)
	}
	if limit > 0 {
		cursor = recordlayer.LimitRowsCursor(cursor, limit)
	}
	return cursor
}

// filterResultCursor filters QueryResult items.
type filterResultCursor struct {
	inner  recordlayer.RecordCursor[QueryResult]
	pred   func(QueryResult) (bool, error)
	closed bool
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java FilterCursor's cached no-next result) — never re-pulls the inner.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *filterResultCursor) OnNext(ctx context.Context) (result recordlayer.RecordCursorResult[QueryResult], err error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	for {
		if err = ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		result, err = c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if !result.HasNext() {
			c.lastNoNext = &result
			return result, nil
		}
		keep, perr := c.pred(result.GetValue())
		if perr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, perr
		}
		if keep {
			return result, nil
		}
	}
}

func (c *filterResultCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *filterResultCursor) IsClosed() bool { return c.closed }
