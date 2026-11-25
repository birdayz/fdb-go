// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// UnionCursor.java, MergeCursorState.java, UnorderedUnionCursor.java,
// UnionCursorContinuation.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

// Package executor bridges RecordQueryPlan trees (Cascades planner
// output) and the FDBRecordStore scanning API to produce
// RecordCursor[QueryResult] streams. Mirrors Java's
// RecordQueryPlan.executePlan dispatching to
// FDBRecordStoreBase.scanRecords.
//
// The executor is a standalone visitor (not a method on
// RecordQueryPlan) to avoid circular dependencies between the plans
// package and the recordlayer package.
package executor

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

type RecursiveCTEDepthExceededError struct {
	MaxDepth int
}

func (e *RecursiveCTEDepthExceededError) Error() string {
	return fmt.Sprintf("recursive CTE exceeded maximum depth of %d", e.MaxDepth)
}

// AggregateTypeMismatchError is returned when MIN or MAX is applied to
// a non-numeric column. Java's fdb-relational rejects this with
// "VerifyException: unable to encapsulate aggregate operation due to
// type mismatch(es)" — the function registry only installs numeric
// MIN/MAX overloads.
type AggregateTypeMismatchError struct {
	Message string
}

func (e *AggregateTypeMismatchError) Error() string {
	return e.Message
}

type NumericRangeOverflowError struct {
	Value    any
	Column   string
	TypeName string
}

func (e *NumericRangeOverflowError) Error() string {
	return fmt.Sprintf("value %v out of range for %s column %q", e.Value, e.TypeName, e.Column)
}

// SumOverflowError reports a SUM/AVG accumulation leaving its operator's
// integer domain. Int32 selects the SUM_I/AVG_I lane's message: Java's
// Math.addExact(int, int) throws ArithmeticException("integer overflow")
// where the long overload throws "long overflow", and the messages surface
// verbatim through the driver (both under SQLSTATE 22003).
type SumOverflowError struct{ Int32 bool }

// UnsupportedContinuationError reports a resume attempt on a cursor shape
// that has no continuation support yet (RFC-180 WS-A follow-ups). The driver
// maps it to SQLSTATE 0A000 — a typed decline, never a silent wrong start
// (a since-deleted buffered union once fed the PARENT's continuation to every
// child; a raw-key scan child consumed it as a scan position).
type UnsupportedContinuationError struct {
	Shape string
}

// FilteredIndexPlanError reports a physical query plan that attempts to read a
// sparse/filtered index without carrying a predicate-implication proof. The
// proof must match the current metadata predicate; missing or stale proofs
// are rejected. Executing an unproved scan would read
// only the predicate-selected subset and could silently omit records.
//
// Low-level record-store APIs deliberately remain able to scan filtered
// indexes for maintenance and diagnostics. This is a query-plan invariant.
type FilteredIndexPlanError struct {
	IndexName string
}

func (e *FilteredIndexPlanError) Error() string {
	return fmt.Sprintf(
		"executor: filtered index %q cannot be used by a query plan without a predicate-implication proof",
		e.IndexName,
	)
}

func (e *UnsupportedContinuationError) Error() string {
	return "unsupported continuation: " + e.Shape + " cannot resume from a continuation"
}

func (e *SumOverflowError) Error() string {
	if e.Int32 {
		return "integer overflow"
	}
	return "long overflow"
}

// ExecutePlan executes a RecordQueryPlan tree against a store,
// returning a cursor over the results. Recursive — child plans are
// executed first, then the parent operator is applied.
func ExecutePlan(
	ctx context.Context,
	plan plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	cursor, err := executePlanUnwrapped(ctx, plan, store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}
	return attachProvidedOutputLayout(plan, cursor)
}

// executePlanUnwrapped dispatches one physical operator. ExecutePlan wraps the
// returned cursor with the operator's provided output layout exactly once, so
// recursive child calls publish their own layouts before a parent consumes
// them and the parent then replaces/preserves that property as declared.
func executePlanUnwrapped(
	ctx context.Context,
	plan plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	switch p := plan.(type) {
	case *plans.RecordQueryScanPlan:
		return executeScan(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryIndexPlan:
		return executeIndexScan(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryTextIndexPlan:
		return executeTextIndexScan(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryVectorIndexPlan:
		return executeVectorIndexScan(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryTypeFilterPlan:
		return executeTypeFilter(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryFilterPlan:
		return executeFilter(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryLimitPlan:
		return executeLimit(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryDistinctPlan:
		return executeDistinct(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryUnorderedPrimaryKeyDistinctPlan:
		return executeUnorderedPrimaryKeyDistinct(
			ctx,
			p,
			store,
			evalCtx,
			continuation,
			props,
		)
	case *plans.RecordQueryUnionPlan:
		return executeUnion(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryIntersectionPlan:
		return executeIntersection(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryNestedLoopJoinPlan:
		return executeNestedLoopJoin(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryStreamingAggregationPlan:
		return executeAggregation(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryExplodePlan:
		return executeExplode(p, evalCtx, continuation, props)
	case *plans.RecordQueryDeletePlan:
		return executeDelete(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryInsertPlan:
		return executeInsert(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryUpdatePlan:
		return executeUpdate(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryTempTableScanPlan:
		return executeTempTableScan(p, evalCtx, continuation, props)
	case *plans.RecordQueryTempTableInsertPlan:
		return executeTempTableInsert(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryTableFunctionPlan:
		return executeTableFunction(p, evalCtx, continuation, props)
	case *plans.RecordQueryValuesPlan:
		return executeValues(p, evalCtx, continuation, props)
	case *plans.RecordQueryRecursiveLevelUnionPlan:
		return executeRecursiveLevelUnionPlan(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryRecursiveDfsJoinPlan:
		return executeRecursiveDfsJoin(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryUnorderedUnionPlan:
		return executeUnorderedUnion(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryPredicatesFilterPlan:
		return executePredicatesFilter(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryMapPlan:
		return executeMap(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryFirstOrDefaultPlan:
		return executeFirstOrDefault(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryDefaultOnEmptyPlan:
		return executeDefaultOnEmpty(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryInJoinPlan:
		return executeInJoin(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryMergeSortUnionPlan:
		return executeMergeSortUnion(ctx, p, store, evalCtx, continuation, props)
	case *plans.RecordQueryInUnionPlan:
		return executeInUnion(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryFlatMapPlan:
		return executeFlatMap(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryCoveringIndexPlan:
		return executeCoveringIndexScan(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryCoveringIndexValuePlan:
		return executeCoveringIndexValueScan(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryFetchFromPartialRecordPlan:
		return executeFetchFromPartialRecord(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryAggregateIndexPlan:
		return executeAggregateIndexScan(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryMultiIntersectionOnValuesPlan:
		return executeMultiIntersection(ctx, p, store, evalCtx, continuation, props)

	case *plans.RecordQueryLoadByKeysPlan:
		return executeLoadByKeys(ctx, p, store, evalCtx, continuation, props)

	// --- Go extensions (no Java equivalent) ---
	case *plans.RecordQueryInMemorySortPlan:
		return executeInMemorySort(ctx, p, store, evalCtx, continuation, props)

	default:
		return nil, fmt.Errorf("executor: unsupported plan type %T", plan)
	}
}

func executeRecursiveLevelUnionPlan(
	ctx context.Context,
	p *plans.RecordQueryRecursiveLevelUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	if p.IsDistinct() {
		// UNION DISTINCT recursion (Java runs it as UNION ALL, a Java
		// upstream bug in DIVERGENCES.md) carries a cross-level
		// seen-set that IS the emission history, so it cannot ride a
		// streaming continuation; it resumes by deterministic replay
		// (position + boundary-row integrity, loud on drift).
		replayBase := func() (recordlayer.RecordCursor[QueryResult], error) {
			return executeRecursiveLevelUnion(ctx, p, store, evalCtx, nil, props.ClearSkipAndLimit())
		}
		cur, err := newPositionReplayCursor(ctx, replayBase, continuation)
		if err != nil {
			return nil, err
		}
		return applySkipLimit(cur, props.Skip, props.ReturnedRowLimit), nil
	}
	// UNION ALL streams with the Java-shape RecursiveUnionCursor
	// continuation (phase + scan-frontier PTempTable + child position).
	cur, err := newRecursiveUnionCursor(ctx, p, store, evalCtx, continuation, props)
	if err != nil {
		return nil, err
	}
	return applySkipLimit(cur, props.Skip, props.ReturnedRowLimit), nil
}

// Fetch failures keep reporting an exhausted end result beside the error.
func terminalOnError(res recordlayer.RecordCursorResult[QueryResult], err error) (recordlayer.RecordCursorResult[QueryResult], error) {
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	return res, nil
}

type errCheckCursor struct {
	inner recordlayer.RecordCursor[QueryResult]
	err   *error
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (Java's cached no-next result) — never re-pulls the inner.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *errCheckCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	if *c.err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, *c.err
	}
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return result, err
	}
	if *c.err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, *c.err
	}
	if !result.HasNext() {
		c.lastNoNext = &result
	}
	return result, nil
}

func (c *errCheckCursor) Close() error { return c.inner.Close() }

func (c *errCheckCursor) IsClosed() bool { return c.inner.IsClosed() }

// enforceSerializable is QueryPlanUtils.enforceSerializable: a
// data-modification plan refuses SNAPSHOT isolation before opening its child.
func enforceSerializable(props recordlayer.ExecuteProperties, plan string) error {
	if props.IsolationLevel != recordlayer.SerializableIsolation {
		return &recordlayer.RecordCoreArgumentError{Message: "Cannot execute plan at SNAPSHOT isolation level", Plan: plan}
	}
	return nil
}

func newEmptyCursor[T any]() recordlayer.RecordCursor[T] {
	return &emptyCursor[T]{}
}

type emptyCursor[T any] struct{ closed bool }

func (c *emptyCursor[T]) OnNext(context.Context) (recordlayer.RecordCursorResult[T], error) {
	return recordlayer.NewResultNoNext[T](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
}

func (c *emptyCursor[T]) IsClosed() bool { return c.closed }

func (c *emptyCursor[T]) Close() error { c.closed = true; return nil }

// errResultCursor yields a stored error on the first OnNext. Used to defer a
// cursor-construction error through a factory that cannot itself return an error
// (e.g. OrElse's primary/alternative factories, which have signature
// func([]byte) RecordCursor[QueryResult]). Mirrors recordlayer.errorCursor.
type errResultCursor struct {
	err error
}

func (c *errResultCursor) OnNext(context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	return recordlayer.RecordCursorResult[QueryResult]{}, c.err
}

func (c *errResultCursor) Close() error { return nil }

func (c *errResultCursor) IsClosed() bool { return false }
