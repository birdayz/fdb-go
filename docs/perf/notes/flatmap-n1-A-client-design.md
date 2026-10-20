# (A) fdbgo eager first range batch + commit `reading` barrier — design (fm-impl)
Base: origin/master AFTER PR #789 (fix/ryw-local-read-nogrv). Do not code against pre-#789.

## Spec (C++ 7.3.77, /tmp/fdbsrc)
- RYW getRange registers its future in `reading` at call time: ReadYourWrites.actor.cpp:1764-1767
  (also get :1690-1691, getKey :1706-1707, watch :1290, getAddressesForKey :1848-1849).
- Commit waits for every one of them first: ReadYourWrites.actor.cpp:1359-1360 (`wait(resetPromise || reading)`).
- Issue-time write view: readWithConflictRangeRYW builds `RYWIterator it(&ryw->cache, &ryw->writes)`
  before its first wait (:382); WriteMap::iterator pins `at(map->ver)` and bumps ver (WriteMap.h:158), the
  PTree is persistent, so later writes are invisible to an in-flight read.
- Conflict filtering uses that same issue-time iterator: addConflictRange(ryw, req,
  it.extractWriteMapIterator(), result) (:388) — independent writes at issue time suppress read conflicts.
- Apple Go binding issues the first batch inside GetRange: bindings/go/src/fdb/transaction.go:295-296
  (`f := t.doGetRange(r, options, snapshot, 1)`), Iterator() reuses that future (range.go:173-176); later
  batches are issued from Advance (range.go:232-252). Java binding does the same: RangeQuery.java:105-139
  (AsyncRangeIterator ctor -> startNextFetch); KeyValueCursor.build() calls getRange().iterator()
  (KeyValueCursor.java:77-81), which is why Java's FlatMap pipeline overlaps inner range reads.

## Go change (on top of #789)
1. Facade (pkg/fdbgo/fdb/range_result.go): GetRange starts batch 1 at call time (parity with Apple
   transaction.go:295), shared by Iterator()/GetSliceWithError (Apple range.go:173-176). Later batches stay
   at Advance with the then-current write view. Reuse WIP fdb-fix-ryw-local startBatch/rangeBatch shape.
2. Client: BeginGetRange-style issue that synchronously (under the RYW lock) captures #789's
   rywWriteSpan for [begin,end) (trivial selectors; whole-map capture for non-trivial ones) + unreadable cap,
   and registers the issue in the current readIncarnation. The goroutine runs the existing getRange with the
   captured span (merge, localRangeInSpan) and computes read conflicts against the span (C++ :388).
3. Commit barrier: generalize the PendingGet drain (#789 transaction.go:1853-1861) to an incarnation
   issued-read count (WIP read_issue.go waitIssuedReads); Reset/OnError retire the incarnation.
4. Avoid WIP defects: (MEDIUM) no whole-cache copy-on-write view per read (#789 span capture is O(overlap));
   (HIGH) watch setup must filter its conflict against ITS issue-time view, not a later live map.

## Proof
- client fault dialer that holds getKeyValues replies until N requests are seen: serial = timeout (red),
  eager = green; Commit-before-reply test: commit must not return until the eager read lands and its
  conflict range is included (C++ :1359).
- differential (bench): GetRange; write into range; iterate -> rows match libfdb_c (issue-time view);
  reuse WIP differential_read_order_test.go TestDifferential_RangeFirstBatchCallOrder.

## (B) status
Local commit 2d01b14fa on perf/flatmap-n1 (base aca6175b4), fast lane green. No compile dependency on (A):
B opens record/index/scan-range-set leaf range reads at construction and keeps a 5 (FlatMap) / 10 (IN-join)
inner window; I/O overlap appears once (A) makes facade GetRange issue batch 1 eagerly. Rebase B onto A.
Adjacent gap (not in scope): Java AsyncRangeIterator also prefetches the NEXT chunk on first next() of a chunk
(RangeQuery.java:305-306); Apple Go binding does not (range.go:232-252), so Go later batches stay serial.

## (C) next-batch prefetch — spec (after A, then B rebase)
Spec = Apple Go binding iterator over libfdb_c; do not exceed it (Java's earlier chunk prefetch,
RangeQuery.java:305-306, is NOT the target).
- Apple RangeIterator.Get hands out kvs[index], index++, and when index == len(kvs) calls fetchNextBatch
  (bindings/go/src/fdb/range.go:259-272): batch N+1 is ISSUED as the last row of batch N is handed out,
  before the caller's next Advance. Advance only waits on that future (range.go:212-230).
- fetchNextBatch issues nothing when !more or the row limit is used up (range.go:232-236); it shrinks the
  limit by rows taken, moves the selector past the last key (FirstGreaterThan / reverse FirstGreaterOrEqual),
  and bumps iteration (range.go:238-252).
- GetSliceWithError issues the next batch right after taking each batch (range.go:146-153); mode EXACT when
  Limit != 0, else WANT_ALL (range.go:139-143).
- libfdb_c itself has no read-ahead: each fdb_transaction_get_range is one call; the ITERATOR byte target per
  iteration is iteration_progression (fdb_c.cpp:1000-1029). C++ RYW issue-time view applies to each batch
  at its own issue (ReadYourWrites.actor.cpp:382, WriteMap.h:158); each is in `reading` (:1767).
Go change: goRangeIterator starts batch N+1 via (A)'s eager issue path when the last buffered row is first
handed out by Get (pure-Go Get is idempotent; trigger once per batch); Advance waits on it. Consequence to
keep (Apple parity): a batch issued at the boundary counts as an issued read (commit waits; conflict added)
even if the caller never advances again.
Measurement recipe draft: pkg/fdbgo/bench long-scan Go vs CGo iterator, StreamingModeIterator, >=50k keys
x ~100B values in one txn, with and without per-row work (prefetch only overlaps work done on the last row).

## (B) rework plan after fp-code NAK (start only once A exists)
- H1: probe must count getKeyValues requests SENT before the first reply wait (client-level hook /
  frame-counting dialer), not GetRange calls; fix probe comment (pkg/fdbgo is lazy pre-A).
- M1: eager first-leaf open only for non-blocking KeyValueCursor-shaped leaves (record/index range scans),
  declared by the caller of newScanRangeSetCursor; vector partition leaves stay lazy — Java wraps the
  partition search in a LazyCursor (VectorIndexMaintainer.java scanSinglePartition, VectorIndexMaintainer.java:226).
  Restore TestScanRangeSetCursorWholeRangeGateFailMode wantOpened 0 for the lazy factory.
- Docs: pipelineSize counts the head (cursor_combinators.go, flat_map_cursor.go).
- Double close: executor head inner closed on non-exhaustion stop then again in Close(); nil it/track.
- Optional: close-on-early-termination test at width 5; kept-armed test at production width.

## fp-spec Lows folded into (B) rework
(1) IN-join innerFactory errors: surface at fill (Java throws in tryToFillPipeline), not via errResultCursor
    at the head (executor_new_plans.go:1788-1792) — change the generic factory to return (cursor, error).
(2) executor fill: advance lastOuterContinuation / consume the armed inner only after openInner succeeds
    (Java keeps outerNextFuture until the entry is added, FlatMapPipelinedCursor.java:221-222).
(3) double close — see above. (5) memory budget of opened-ahead inners and (6) nil tx at construction: noted.
(4) Closing an opened-ahead, never-advanced inner — C++ does NOT retire the read:
    Java iterator cancel (RangeQuery.java:334-337) -> fdb_future_cancel (fdb_c.cpp:180-182) ->
    ThreadSingleAssignmentVar::cancel cancels only the onMainThread wrapper actor
    (ThreadHelper.actor.h:443-454, doOnMainThread :816-831). The RYW read future is still referenced by
    `reading.add(success(result))` (ReadYourWrites.actor.cpp:1767), and a SAV is cancelled only when its last
    future ref drops (flow.h:863-869). So the read runs to completion, adds its read-conflict range
    (:384-388) and commit waits for it (:1359-1360). Go (A): Close of an eager range read does not retire it;
    it stays registered for the commit barrier and records its conflict on completion.
Root ruling (#705): (2) is a correctness fix, unconditional. Regression: openInner fails, OnNext again,
the outer row must not be skipped (red on 2d01b14fa). (4) analysis accepted: Go matches C++.
From #798 final review (1c08f1b04), fold into (B) rework after rebasing on master post-#798:
(1) charge record-version bytes to the byte limit like Java size info (SplitHelper.java:246-248,316-318 inline;
    :446 via gets); Go loads the version separately (store.go LoadRecordAsync/finish ~:410-414). Regression on a
    versioned store.
(2) cursor_pipelined.go:83 OnNext after Close must return the closed/SourceExhausted result other cursors use,
    not context.Canceled. (3) cursor_pipelined.go:44 MapPipelined doc -> 1-2 lines of why.
Owner ruling (#805): pre-release, public API changes OK when an improvement; add a CHANGELOG line, don't ask.
Applies to: FlatMapPipelined innerFactory -> (cursor, error); facade GetRange issuing batch 1 eagerly (A);
iterator next-batch prefetch (C).

## (A) rework bench (BenchmarkGetRangeFacade, fdb_test, -benchmem, count=3), BEFORE = 2bc0e471e
local-hit: 11911/12113/13070 ns/op, ~9.05 KB/op, 94 allocs/op
remote:    85231/83462/81511 ns/op, ~4.05 KB/op, 57 allocs/op
AFTER (rework): local-hit 9256/9458/9286 ns/op, ~8.5 KB/op, 88 allocs; remote 80516/81277/81716 ns/op, ~3.79 KB/op, 52 allocs
