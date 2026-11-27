// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// UnionCursor.java, MergeCursorState.java, UnorderedUnionCursor.java,
// UnionCursorContinuation.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"fdb.dev/internal/recordlayer/query/plan/plans"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

type indexFetchCursor struct {
	inner     recordlayer.RecordCursor[*recordlayer.IndexEntry]
	store     *recordlayer.FDBRecordStore
	scanState *recordlayer.ScanLimiterState
	pipeline  recordlayer.RecordCursor[QueryResult]
}

// newIndexFetchCursor fetches each entry's record; scanState, when set, is
// charged the loaded bytes as Java's fetchIndexRecords charges its ExecuteState.
func newIndexFetchCursor(inner recordlayer.RecordCursor[*recordlayer.IndexEntry], store *recordlayer.FDBRecordStore, scanState *recordlayer.ScanLimiterState) *indexFetchCursor {
	c := &indexFetchCursor{inner: inner, store: store, scanState: scanState}
	c.pipeline = recordlayer.MapPipelined(inner, c.load, store.GetPipelineSize(recordlayer.PipelineOperationIndexToRecord))
	return c
}

func (c *indexFetchCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	return terminalOnError(c.pipeline.OnNext(ctx))
}

func (c *indexFetchCursor) load(entry *recordlayer.IndexEntry) recordlayer.PendingRead[QueryResult] {
	pk := entry.PrimaryKey()
	if pk == nil {
		return recordlayer.NewPendingRead(func() (QueryResult, error) { return c.finish(entry, nil, nil, nil) }, nil)
	}
	return recordlayer.MapPendingRead(c.store.LoadRecordAsync(pk, c.scanState), func(rec *recordlayer.FDBStoredRecord[proto.Message], err error) (QueryResult, error) {
		return c.finish(entry, pk, rec, err)
	})
}

func (c *indexFetchCursor) finish(entry *recordlayer.IndexEntry, pk tuple.Tuple, rec *recordlayer.FDBStoredRecord[proto.Message], err error) (QueryResult, error) {
	indexName := ""
	if entry.Index != nil {
		indexName = entry.Index.Name
	}
	if pk == nil {
		// A malformed index entry that resolves to no primary key is detectable
		// corruption, not a row to drop. Java resolves the PK before fetching
		// (IndexEntry.getPrimaryKey) and the ERROR orphan path below is loud for
		// any missing base record; a nil PK is the same class of fault, so raise
		// rather than silently continue.
		return QueryResult{}, &recordlayer.RecordCoreStorageError{
			Message:   "record not found from index entry",
			IndexName: indexName,
			IndexKey:  entry.Key,
		}
	}

	if err != nil {
		return QueryResult{}, fmt.Errorf("executor: loading record for index entry pk %v: %w", pk, err)
	}
	if rec == nil {
		// Port Java's IndexOrphanBehavior.ERROR (RecordQueryIndexPlan →
		// FetchIndexRecords.PRIMARY_KEY → FDBRecordStoreBase.loadIndexEntryRecord):
		// an index entry whose base record is missing means the index and the
		// records disagree — index corruption, an out-of-band delete, or a
		// maintainer bug. Query execution never uses SKIP here; silently
		// continuing would return fewer rows and hide the corruption. Raise the
		// same typed error Java throws, carrying its INDEX_NAME / PRIMARY_KEY /
		// INDEX_KEY log info.
		c.store.Context().Timer().Increment(recordlayer.CountBadIndexEntry)
		return QueryResult{}, &recordlayer.RecordCoreStorageError{
			Message:    "record not found from index entry",
			IndexName:  indexName,
			PrimaryKey: pk,
			IndexKey:   entry.Key,
		}
	}

	return FromStoredRecord(rec), nil
}

func (c *indexFetchCursor) Close() error { return c.pipeline.Close() }

func (c *indexFetchCursor) IsClosed() bool { return c.pipeline.IsClosed() }

// executeFetchFromPartialRecord executes a FetchFromPartialRecordPlan: it takes
// the partial records its child produces and resolves each to the FULL record
// by primary key. Mirrors Java's RecordQueryFetchFromPartialRecordPlan, which
// maps its child's rows through QueryResult::getIndexEntry and hands them to
// FetchIndexRecords.PRIMARY_KEY (RecordQueryFetchFromPartialRecordPlan:128-132).
//
// This used to be a PASS-THROUGH, on the premise that "in Go, the index scan
// executor already returns full records". That premise died with RFC-220: the
// access path now emits Fetch(Covering(IndexScan)) on every value-index match,
// so the child's rows are partial records built from index entries. Passing
// them through would hand partial rows to the fetch's consumer — silent wrong
// rows, not a slower plan.
func executeFetchFromPartialRecord(
	ctx context.Context,
	p *plans.RecordQueryFetchFromPartialRecordPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	inner := p.GetInner()
	if inner == nil {
		return recordlayer.Empty[QueryResult](), nil
	}
	innerCursor, err := ExecutePlan(ctx, inner, store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}
	return newFetchFullRecordCursor(innerCursor, store, props.ScanState), nil
}

// fetchFullRecordCursor resolves each incoming row to its full stored record by
// primary key. It is the QueryResult-shaped sibling of indexFetchCursor, which
// consumes raw index entries; a residual pushed below the fetch (Java's
// PushFilterThroughFetchRule does exactly this) means the fetch's child is a
// plan, not a scan, so its rows arrive as QueryResult.
//
// A row that ALREADY carries its stored record passes through untouched. That
// is idempotence, not a heuristic: fetching a row whose record is in hand
// returns the identical record, so the read would be pure cost. It is what
// keeps the covering scan's logical-shape fallback (see executeCoveringIndexScan)
// from costing two reads per row.
//
// Orphan behaviour is Java's IndexOrphanBehavior.ERROR, identical to
// indexFetchCursor's: an entry whose base record is missing means the index and
// the records disagree, and query execution never uses SKIP.
type fetchFullRecordCursor struct {
	store     *recordlayer.FDBRecordStore
	scanState *recordlayer.ScanLimiterState
	pipeline  recordlayer.RecordCursor[QueryResult]
}

func newFetchFullRecordCursor(inner recordlayer.RecordCursor[QueryResult], store *recordlayer.FDBRecordStore, scanState *recordlayer.ScanLimiterState) *fetchFullRecordCursor {
	c := &fetchFullRecordCursor{store: store, scanState: scanState}
	c.pipeline = recordlayer.MapPipelined(inner, c.load, store.GetPipelineSize(recordlayer.PipelineOperationIndexToRecord))
	return c
}

func (c *fetchFullRecordCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	return terminalOnError(c.pipeline.OnNext(ctx))
}

func (c *fetchFullRecordCursor) load(row QueryResult) recordlayer.PendingRead[QueryResult] {
	if row.Record != nil || row.PrimaryKey == nil {
		return recordlayer.NewPendingRead(func() (QueryResult, error) { return c.finish(row, nil, nil) }, nil)
	}
	return recordlayer.MapPendingRead(c.store.LoadRecordAsync(row.PrimaryKey, c.scanState), func(rec *recordlayer.FDBStoredRecord[proto.Message], err error) (QueryResult, error) {
		return c.finish(row, rec, err)
	})
}

func (c *fetchFullRecordCursor) finish(row QueryResult, rec *recordlayer.FDBStoredRecord[proto.Message], err error) (QueryResult, error) {
	if row.Record != nil {
		return row, nil
	}
	pk := row.PrimaryKey
	if pk == nil {
		// The fetch's contract is "make this partial row whole", and the only
		// handle it has is the primary key. A row reaching it with neither a
		// record nor a key is a planner fault — a fetch placed over a producer
		// that does not carry row identity — and it is raised rather than
		// passed through, because passing it through is precisely the silent
		// partial-row leak this cursor exists to prevent.
		return QueryResult{}, &recordlayer.RecordCoreStorageError{
			Message: "fetch from partial record: row carries neither a stored record nor a primary key",
		}
	}
	if err != nil {
		return QueryResult{}, fmt.Errorf("executor: loading record for partial row pk %v: %w", pk, err)
	}
	if rec == nil {
		// Java IndexOrphanBehavior.ERROR — see indexFetchCursor for the full
		// reasoning. Silently continuing would return fewer rows and hide index
		// corruption.
		c.store.Context().Timer().Increment(recordlayer.CountBadIndexEntry)
		return QueryResult{}, &recordlayer.RecordCoreStorageError{
			Message:    "record not found from index entry",
			PrimaryKey: pk,
		}
	}
	return FromStoredRecord(rec), nil
}

func (c *fetchFullRecordCursor) Close() error { return c.pipeline.Close() }

func (c *fetchFullRecordCursor) IsClosed() bool { return c.pipeline.IsClosed() }

func executeLoadByKeys(
	_ context.Context,
	p *plans.RecordQueryLoadByKeysPlan,
	store *recordlayer.FDBRecordStore,
	_ *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	keys := p.GetKeysSource().GetPrimaryKeys()
	if len(keys) == 0 {
		return recordlayer.Empty[QueryResult](), nil
	}
	// Java's shape (RecordQueryLoadByKeysPlan.executePlan): the KEY list
	// is the resumable source — RecordCursor.fromList(keys, continuation)
	// — with each key loaded lazily and missing records skipped. The
	// eager buffered form DROPPED the incoming continuation while still
	// emitting resumable-looking list tokens, so a resume replayed every
	// row from 0. Lazy loading also holds one record at a time, so no
	// statement-memory charge is needed (the RFC-130 buffer accumulated
	// every loaded record).
	return applySkipLimit(
		&loadByKeysCursor{
			keys:  recordlayer.FromListWithContinuation(keys, continuation),
			store: store,
		},
		props.Skip, props.ReturnedRowLimit,
	), nil
}

// loadByKeysCursor loads records for a resumable key-list cursor,
// skipping keys with no record (Java's .filter(Objects::nonNull)). Each
// emitted row carries the KEY cursor's continuation — the wire format is
// the plain 4-byte list-index token, byte-identical to Java's.
type loadByKeysCursor struct {
	keys   recordlayer.RecordCursor[tuple.Tuple]
	store  *recordlayer.FDBRecordStore
	closed bool
}

func (c *loadByKeysCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	for {
		r, err := c.keys.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if !r.HasNext() {
			return recordlayer.NewResultNoNext[QueryResult](r.GetNoNextReason(), r.GetContinuation()), nil
		}
		pk := r.GetValue()
		rec, err := c.store.LoadRecord(pk)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf("executor: LoadByKeys pk=%v: %w", pk, err)
		}
		if rec == nil {
			continue
		}
		return recordlayer.NewResultWithValue(FromStoredRecord(rec), r.GetContinuation()), nil
	}
}

func (c *loadByKeysCursor) Close() error {
	c.closed = true
	return c.keys.Close()
}

func (c *loadByKeysCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*loadByKeysCursor)(nil)
