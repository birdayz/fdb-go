// Portions derived from FoundationDB Record Layer (CursorLimitManager.java,
// KeyValueCursorBase.java, AtomicMutationIndexMaintainer.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"context"
	"encoding/binary"
	"fmt"

	"fdb.dev/pkg/recordlayer/internal/tuplefast"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// countKVCursor scans an aggregate index and returns IndexEntry values.
// By default, decodes values as little-endian int64 (for COUNT/SUM/MIN_EVER_LONG/MAX_EVER_LONG).
// Set tupleValues=true to decode values as tuple-packed bytes (for MIN_EVER_TUPLE/MAX_EVER_TUPLE).
type countKVCursor struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.WritableTransaction
	tupleRange    TupleRange
	continuation  []byte
	scanProps     ScanProperties

	iterator     rangeIterator
	closed       bool
	returned     int
	initialPass  bool
	clock        *ScanLimiterState
	bounds       kvScanBounds
	prefixLength int
	lastCont     []byte
	lastNoNext   RecordCursorResult[*IndexEntry]
	hasNoNext    bool
	tupleValues  bool // if true, decode values as tuple-packed bytes

	// scanState is the (possibly shared) scanned-records/scanned-bytes/time
	// counter set for RFC-106a resource limits — see ScanLimiterState's doc
	// comment. Never nil.
	scanState *ScanLimiterState
}

func (c *countKVCursor) withContext(rc *FDBRecordContext) *countKVCursor {
	c.clock = transactionClock(rc, c.scanProps.ExecuteProperties, c.clock)
	return c
}

// newCountIndexCursor creates a cursor that scans a COUNT index.
// Each entry's Value is the count decoded from the little-endian int64 FDB value.
func newCountIndexCursor(index *Index, indexSubspace subspace.Subspace, tx fdb.WritableTransaction,
	scanRange TupleRange, continuation []byte, scanProperties ScanProperties,
) *countKVCursor {
	bounds := newKVScanBounds(indexSubspace, scanRange.Low, scanRange.High, scanRange.LowEndpoint, scanRange.HighEndpoint)
	return &countKVCursor{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		tupleRange:    scanRange,
		continuation:  continuation,
		scanProps:     scanProperties,
		bounds:        bounds,
		prefixLength:  bounds.prefixLength,
	}
}

// newTupleValueIndexCursor creates a cursor that scans an aggregate index with tuple-packed values.
// Each entry's Value is decoded from tuple-packed bytes (for MIN_EVER_TUPLE/MAX_EVER_TUPLE).
func newTupleValueIndexCursor(index *Index, indexSubspace subspace.Subspace, tx fdb.WritableTransaction,
	scanRange TupleRange, continuation []byte, scanProperties ScanProperties,
) *countKVCursor {
	bounds := newKVScanBounds(indexSubspace, scanRange.Low, scanRange.High, scanRange.LowEndpoint, scanRange.HighEndpoint)
	return &countKVCursor{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		tupleRange:    scanRange,
		continuation:  continuation,
		scanProps:     scanProperties,
		bounds:        bounds,
		prefixLength:  bounds.prefixLength,
		tupleValues:   true,
	}
}

func (c *countKVCursor) initIterator() error {
	// scanState is resolved lazily here (not at construction) so the nil-fallback private
	// instance's StartTime anchor is captured at first USE, matching the pre-fix
	// c.startTime = time.Now() that used to live in this exact spot — resolving it eagerly at
	// construction would tighten the effective time budget for a cursor built well before its
	// first OnNext (resolveScanLimiterState is a no-op passthrough for the shared, non-nil case:
	// that pointer's StartTime was already fixed when the caller minted it).
	c.scanState = resolveScanLimiterState(c.scanProps.ExecuteProperties)
	rng, err := c.bounds.keyRange(c.continuation, c.scanProps.IsReverse())
	if err != nil {
		return err
	}
	options := kvRangeOptions(c.scanProps)
	r := indexSkipRange(rng, c.scanProps.ExecuteProperties.Skip, options.Reverse)

	// Honor the requested isolation level, exactly as Java's KeyValueCursorBase does:
	// transaction = context.readTransaction(scanProperties.getExecuteProperties()
	// .getIsolationLevel().isSnapshot()) (KeyValueCursorBase.java:358), which is the
	// transaction every aggregate-index scan reaches through
	// AtomicMutationIndexMaintainer.evaluateAggregateFunction ->
	// scan(BY_GROUP, ...) -> StandardIndexMaintainer.scan
	// (AtomicMutationIndexMaintainer.java:209-217).
	//
	// A snapshot request that reads non-snapshot is not merely conservative: it adds a
	// read conflict on the index range, so a transaction that counts via this path,
	// races an index update and then writes anything at all fails with not_committed
	// (1020) — for a read whose whole contract is that it does not conflict.
	//
	// This is deliberately the OPPOSITE of the store's records-emptiness probe
	// (recordsRangeEmpty in store_builder.go), which is intentionally non-snapshot so a
	// concurrent insert DOES invalidate its "empty, so build the index inline" decision.
	// That probe wants the conflict; this read must not have one. Do not unify them.
	var rangeResult fdb.RangeResult
	if c.scanProps.ExecuteProperties.IsolationLevel == SnapshotIsolation {
		rangeResult = c.tx.Snapshot().GetRange(r, options)
	} else {
		rangeResult = c.tx.GetRange(r, options)
	}
	c.iterator = rangeResult.Iterator()
	return nil
}

func (c *countKVCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.hasNoNext && !c.closed {
		return c.lastNoNext, nil
	}
	result, err := c.advance(ctx)
	if err == nil && !result.HasNext() {
		// Java replays the first terminal result on subsequent calls.
		c.lastNoNext = result
		c.hasNoNext = true
	}
	return result, err
}

func (c *countKVCursor) advance(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.closed {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("cursor is closed")
	}

	// Honor a statement deadline / cancellation (RFC-106a).
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}

	if c.iterator == nil {
		if err := c.initIterator(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
	}

	executeProps := c.scanProps.GetExecuteProperties()

	clock := c.clock
	if clock == nil {
		clock = c.scanState
	}
	if allowed, reason := tryScanLimits(c.scanState, clock, executeProps, &c.initialPass); !allowed {
		return noNextOrFail[*IndexEntry](executeProps, reason, c.limitContinuation())
	}

	// Java reports the passed-down row limit even if it exactly exhausted the source.
	if executeProps.ReturnedRowLimit > 0 && c.returned >= executeProps.ReturnedRowLimit {
		if c.iterator.Advance() {
			return NewResultNoNext[*IndexEntry](
				ReturnLimitReached,
				c.limitContinuation(),
			), nil
		}
		// Advance() returns false on exhaustion OR error — check Get() for the stored
		// error so a transient transaction_too_old (1007) / timeout at the row-limit
		// boundary surfaces instead of being read as end-of-data (silent row loss).
		if _, err := c.iterator.Get(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index scan at row-limit boundary: %w", err)
		}
		return NewResultNoNext[*IndexEntry](
			ReturnLimitReached,
			c.limitContinuation(),
		), nil
	}

	if !c.iterator.Advance() {
		// Advance() returns false on exhaustion OR error — surface the stored Get()
		// error rather than reporting SourceExhausted (silent row loss).
		if _, err := c.iterator.Get(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index scan: %w", err)
		}
		return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
	}

	kv, err := c.iterator.Get()
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index scan: %w", err)
	}
	c.scanState.AddBytesScanned(int64(len(kv.Key) + len(kv.Value))) // RFC-106a byte accounting

	// Unpack key using fastUnpack for zero-alloc integer decode.
	prefixLen := len(c.indexSubspace.Bytes())
	if len(kv.Key) < prefixLen {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index key shorter than subspace prefix")
	}
	keyTuple, err := tuplefast.Unpack(kv.Key[prefixLen:])
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("unpack count index key: %w", err)
	}

	// Decode value based on index type
	var valueTuple tuple.Tuple
	if c.tupleValues {
		// TUPLE variants: decode value as tuple-packed bytes
		if len(kv.Value) > 0 {
			var err2 error
			valueTuple, err2 = tuplefast.Unpack(kv.Value)
			if err2 != nil {
				return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("unpack tuple value: %w", err2)
			}
		}
	} else {
		// COUNT/SUM/LONG variants: decode value as little-endian int64
		count := int64(0)
		if len(kv.Value) > 0 && len(kv.Value) < 8 {
			return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index %q: corrupted value: expected 8 bytes, got %d", c.index.Name, len(kv.Value))
		} else if len(kv.Value) >= 8 {
			count = int64(binary.LittleEndian.Uint64(kv.Value))
		}
		valueTuple = tuple.Tuple{count}
	}

	entry := &IndexEntry{
		Index: c.index,
		Key:   keyTuple,
		Value: valueTuple,
	}

	c.returned++

	cont, err := c.makeContinuation(kv.Key)
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}
	c.lastCont = cont

	return NewResultWithValue(entry, &BytesContinuation{bytes: cont}), nil
}

func (c *countKVCursor) limitContinuation() RecordCursorContinuation {
	if c.lastCont != nil {
		return &BytesContinuation{bytes: c.lastCont}
	}
	return &StartContinuation{}
}

func (c *countKVCursor) makeContinuation(key fdb.Key) ([]byte, error) {
	suffix, err := keySuffix(key, c.prefixLength)
	if err != nil {
		return nil, err
	}
	return wrapContinuation(suffix)
}

func (c *countKVCursor) Close() error {
	c.closed = true
	return nil
}

func (c *countKVCursor) IsClosed() bool { return c.closed }
