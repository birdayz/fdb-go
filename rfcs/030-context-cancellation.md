# RFC-030: Context Cancellation in Executor Cursors

**Status:** Implemented
**P0.3** — fix before deploying anywhere

## Problem

Zero `ctx.Err()` checks exist in production cursor `OnNext` methods. When a Go HTTP handler cancels its context (deadline exceeded, client disconnect), every cursor in the plan tree ignores the cancellation and continues pulling rows until either:

1. The 5s FDB transaction timeout kills an underlying range read, or
2. An incidental child error surfaces.

For cursors that loop over in-memory data (NLJ hash probe, filter over large scans, memorySortCursor buffer, intersection merge), FDB never gets involved — the cursor spins indefinitely on a cancelled request. Every Go service sets request deadlines via context; ignoring them is a production availability risk.

## Investigation

### Go state

51 `OnNext` implementations across the codebase. Zero check `ctx.Err()` or `ctx.Done()`. The only context check is in `runner.go:102-104` (a select in the top-level driver, not in any cursor).

### Java comparison

Java uses `close()` + `volatile boolean closed` with checks at entry points and decision branches. `FlatMapPipelinedCursor` checks `if (closed) return ALREADY_CANCELLED;` at the top of `tryToFillPipeline()`. CompletableFuture cancellation propagates through the async pipeline. Go's `ctx.Err()` is the direct equivalent.

Java does NOT check on every single async step — it checks at cursor entry points and before starting new work. This is the right granularity: frequent enough to stop promptly, infrequent enough to add zero measurable overhead.

## Fix

Add `ctx.Err()` checks at the **top of every internal loop** in cursors that loop, and at the **entry point** of leaf cursors that do FDB I/O.

Return `context.Canceled` / `context.DeadlineExceeded` as errors (non-continuable — no `NoNextReason` or continuation attached). A cancelled request is dead, not resumable. Callers must not extract a continuation from a cancellation error and resume on a new transaction.

Pattern:
```go
func (c *filterCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
    for {
        if err := ctx.Err(); err != nil {
            return RecordCursorResult[T]{}, err
        }
        result, err := c.inner.OnNext(ctx)
        ...
    }
}
```

### Cursors requiring changes (loop internally)

| File | Cursor | Loop type | Risk |
|------|--------|-----------|------|
| `cursor_combinators.go` | `autoContinuingCursor` | tx-retry loop | **CRITICAL** — creates new FDB txns on cancelled ctx |
| `cursor_combinators.go` | `filterCursor` | filter loop | spins on large non-matching scans |
| `cursor_combinators.go` | `skipCursor` | skip-N loop | bounded by N |
| `cursor_combinators.go` | `flatMapCursor` | outer+inner loop | spins on outer+inner |
| `key_value_cursor.go` | `keyValueCursor.readNextRecord` | version-skip loop | foundational FDB leaf cursor |
| `key_value_cursor.go` | `keyValueCursor.collectSplitRecord` | chunk-collect loop | bounded by split count |
| `record_key_cursor.go` | `recordKeyCursor` | PK-dedup loop | skips split-record duplicates |
| `dedup_cursor.go` | `dedupCursor` | dedup loop | spins skipping duplicates |
| `index_scan.go` | `indexRecordCursor` | resolve-entry loop | FDB I/O per iteration |
| `multidimensional_index_maintainer.go` | `rtreeScanCursor` | R-tree iterator loop | skips filtered points |
| `multidimensional_index_maintainer.go` | `prefixSkipScanCursor` | prefix iteration loop | iterates sub-cursors |
| `merge_cursor.go` | `unionCursor` | child advance loop | multi-child advance |
| `merge_cursor.go` | `intersectionCursor` | merge-intersection loop | converge loop |
| `executor/executor.go` | `indexFetchCursor` | null-PK skip loop | FDB I/O per iteration |
| `executor/executor.go` | `filterResultCursor` | filter loop | spins on non-matching rows |
| `executor/executor.go` | `CollectAll` (function) | drain loop | materializes entire cursor |
| `executor/executor.go` | `CollectAllBounded` (function) | drain loop | materializes up to limit |
| `executor/executor.go` | `executeDelete` (function) | DML drain loop | deletes per row |
| `executor/executor.go` | `executeInsert` (function) | DML drain loop | inserts per row |
| `executor/executor.go` | `executeUpdate` (function) | DML drain loop | updates per row |
| `cursor_util.go` | `ForEach` (function) | drain loop | applies fn to all rows |
| `cursor_util.go` | `AsListWithContinuation` (function) | drain loop | collects page |
| `cursor_util.go` | `GetCount` (function) | drain loop | counts all rows |
| `cursor_util.go` | `Reduce` (function) | drain loop | folds all rows |
| `cursor.go` | `Seq` (function) | iterator loop | Go iter.Seq adapter |
| `cursor.go` | `Seq2` (function) | iterator loop | Go iter.Seq2 adapter |
| `cursor.go` | `SeqWithContinuation` (function) | iterator loop | Go iter.Seq2 adapter |
| `executor/flat_map_cursor.go` | `flatMapCursor` | outer+inner loop | spins on outer+inner |
| `executor/streaming_cursors.go` | `aggregateCursor` | accumulate loop | scans entire group |
| `executor/streaming_cursors.go` | `memorySortCursor` | materialize loop | materializes all rows |
| `executor/streaming_cursors.go` | `customSortCursor` | materialize loop | materializes all rows |
| `executor/streaming_cursors.go` | `nljCursor` | outer+inner loop | hash probe + outer loop |
| `executor/executor_new_plans.go` | `mergeSortCursor` | peek-buffer fill loop | multi-cursor advance |
| `executor/executor_new_plans.go` | `concatCursor` (executor) | cursor-switch loop | iterates cursor array |
| `embedded/in_list_pushdown.go` | `pkCompositeInListCursor` | cursor-switch loop | iterates value list |
| `embedded/in_list_pushdown.go` | `secondaryIndexCompositeInListCursor` | cursor-switch loop | iterates value list |
| `embedded/in_list_pushdown.go` | `secondaryIndexInListCursor` | cursor-switch loop | iterates value list |
| `embedded/in_list_pushdown.go` | `pkInListCursor` | cursor-switch loop | iterates value list |

### Cursors NOT requiring changes

- **Pure passthrough** (single `inner.OnNext(ctx)` call): `limitRowsCursor`, `mapResultCursor`, `mapErrCursor`, `orElseCursor`, `coveringIndexCursor`, `errCheckCursor`, `prependResultCursor`, `coveringCursor`, `aggregateIndexCursor`, `multiIntersectionMergeCursor`, `concatCursor` (recordlayer — two-cursor concat, max one recursive call) — cancellation propagates through inner cursor.
- **In-memory only** (no loop, no I/O): `emptyCursor`, `errorCursor`, `listCursor`, `sortResultCursor`, `singleResultCursor` — return immediately.
- **Generator-based**: `chainedCursor` — single generator call, no loop.

## Performance

`ctx.Err()` is a single atomic load (sync.Mutex-free in Go's runtime). Cost: <1ns per call. Even on a 1M-row full scan, the overhead is ~1ms total — unmeasurable against FDB I/O latency.

## Test plan

1. Unit test: create a cursor over a large in-memory dataset, cancel context after N rows, verify cursor returns `context.Canceled` promptly (not after exhausting the dataset).
2. Integration test: start a long-running query via SQL driver, cancel the context, verify the query returns within a short deadline (not 5s FDB timeout).
3. All existing tests pass (ctx.Err() returns nil for non-cancelled contexts — zero behavioral change on the happy path).
