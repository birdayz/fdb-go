// Portions derived from FoundationDB Record Layer (CursorLimitManager.java,
// FDBRecordStore.java, TimeScanLimiter.java, KeyValueCursorBase.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer/internal/tuplefast"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// recordKeyCursor maps each physical record key to its PK without loading data.
// ScanRecordKeys wraps suffixed layouts in Dedup and an outer returned-row limit.
type recordKeyCursor struct {
	store                   *FDBRecordStore
	continuation            []byte
	scanProperties          ScanProperties
	omitUnsplitRecordSuffix bool
	iterator                rangeIterator
	closed                  bool
	keysReturned            int
	prefixLength            int
	initialPass             bool
	scanState               *ScanLimiterState
	clock                   *ScanLimiterState
	terminal                *RecordCursorResult[tuple.Tuple]
}

func (c *recordKeyCursor) OnNext(ctx context.Context) (RecordCursorResult[tuple.Tuple], error) {
	if c.terminal != nil {
		return *c.terminal, nil
	}
	if c.closed {
		return RecordCursorResult[tuple.Tuple]{}, fmt.Errorf("cursor is closed")
	}
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[tuple.Tuple]{}, err
	}
	if c.iterator == nil {
		if err := c.initIterator(); err != nil {
			return RecordCursorResult[tuple.Tuple]{}, err
		}
	}
	if c.scanState == nil {
		c.scanState = resolveScanLimiterState(c.scanProperties.ExecuteProperties)
	}
	if c.clock == nil {
		c.clock = c.scanState
	}
	props := c.scanProperties.ExecuteProperties
	allowed, reason := tryScanLimits(c.scanState, c.clock, props, &c.initialPass)
	if !allowed {
		if props.FailOnScanLimitReached {
			return RecordCursorResult[tuple.Tuple]{}, &ScanLimitReachedError{Reason: reason}
		}
		return c.stop(reason), nil
	}
	// A finite physical scan, like Java's KV iterator, stops without a lookahead.
	if props.ReturnedRowLimit > 0 && c.keysReturned >= props.ReturnedRowLimit {
		return c.stop(ReturnLimitReached), nil
	}
	if !c.iterator.Advance() {
		if _, err := c.iterator.Get(); err != nil {
			return RecordCursorResult[tuple.Tuple]{}, fmt.Errorf("record key cursor: advance: %w", err)
		}
		return c.stop(SourceExhausted), nil
	}
	kv, err := c.iterator.Get()
	if err != nil {
		return RecordCursorResult[tuple.Tuple]{}, fmt.Errorf("record key cursor: get: %w", err)
	}
	c.scanState.AddBytesScanned(int64(len(kv.Key) + len(kv.Value)))
	key, err := tuplefast.SubspaceUnpack(kv.Key, c.prefixLength)
	if err != nil {
		cause := &IllegalArgumentError{Message: err.Error()}
		if c.omitUnsplitRecordSuffix {
			return RecordCursorResult[tuple.Tuple]{}, &RecordCoreArgumentError{
				Message: "unable to unpack key", Cause: cause,
				Key: append([]byte(nil), kv.Key...), Subspace: append([]byte(nil), kv.Key[:c.prefixLength]...),
			}
		}
		return RecordCursorResult[tuple.Tuple]{}, cause
	}
	pk := tuple.Tuple(key)
	if !c.omitUnsplitRecordSuffix {
		if len(key) == 0 {
			return RecordCursorResult[tuple.Tuple]{}, &RecordCoreStorageError{Message: "Missing record split number"}
		}
		// Tuple.getLong accepts every Number carrier, not just canonical integer suffixes.
		suffix, ok := javaNumberLongValue(key[len(key)-1])
		if !ok || suffix < recordVersionSuffix {
			return RecordCursorResult[tuple.Tuple]{}, &RecordCoreStorageError{Message: "Invalid record split number", KeyTuple: pk}
		}
		pk = pk[:len(pk)-1]
	}
	cont, err := wrapContinuation(kv.Key[c.prefixLength:])
	if err != nil {
		return RecordCursorResult[tuple.Tuple]{}, fmt.Errorf("wrap record-key continuation: %w", err)
	}
	c.continuation = cont
	c.keysReturned++
	return NewResultWithValue(pk, &BytesContinuation{bytes: cont}), nil
}

func (c *recordKeyCursor) stop(reason NoNextReason) RecordCursorResult[tuple.Tuple] {
	var cont RecordCursorContinuation = &StartContinuation{}
	if reason == SourceExhausted {
		cont = &EndContinuation{}
	} else if c.continuation != nil {
		cont = &BytesContinuation{bytes: c.continuation}
	}
	result := NewResultNoNext[tuple.Tuple](reason, cont)
	c.terminal = &result
	return result
}

func (c *recordKeyCursor) initIterator() error {
	recordsSubspace := c.store.subspace.Sub(RecordKey)
	c.prefixLength = len(recordsSubspace.FDBKey())
	beginKey, endKey := recordsSubspace.FDBRangeKeys()
	begin, end := beginKey.FDBKey(), endKey.FDBKey()
	if c.continuation != nil {
		fullKey := append(append([]byte(nil), recordsSubspace.FDBKey()...), unwrapContinuation(c.continuation)...)
		if c.scanProperties.IsReverse() {
			end = fullKey
		} else {
			begin = append(fullKey, 0x00)
		}
	}
	rng := indexSkipRange(fdb.KeyRange{Begin: begin, End: end}, c.scanProperties.ExecuteProperties.Skip, c.scanProperties.IsReverse())
	options := kvRangeOptions(c.scanProperties)
	tx := c.store.context.Transaction()
	var result fdb.RangeResult
	if c.scanProperties.ExecuteProperties.IsolationLevel == SnapshotIsolation {
		result = tx.Snapshot().GetRange(rng, options)
	} else {
		result = tx.GetRange(rng, options)
	}
	c.iterator = result.Iterator()
	return nil
}

func (c *recordKeyCursor) Close() error {
	c.closed = true
	return nil
}

func (c *recordKeyCursor) IsClosed() bool { return c.closed }
