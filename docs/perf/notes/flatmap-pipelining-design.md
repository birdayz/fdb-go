# FlatMap / nested-loop inner pipelining — design notes (fp-impl)
Java: FlatMapPipelinedCursor (pipelineSize 5 for FlatMapPlan, IN_JOIN 10) creates up to N inner
cursors ahead but only onNext()s the head. The overlap comes from KeyValueCursor building an FDB
AsyncRangeIterator at construction, which issues the first range fetch immediately.
Go today: range reads lazy (fdb/range_result.go goRangeIterator.Advance -> doRange -> client
getRangeDir; keyValueCursor.initIterator on first OnNext). Point Gets are already pipelined
(client GetPipelined deferred frames) — PR #798 uses that for index->record fetch.
Needed for parity (3 parts):
1. fdbgo: eager first batch for a range read, with C++ issue-time RYW semantics. rywCache.getRange
   (client/ryw.go:653) decides hasWrites/hasClears/unreadable under c.mu, then serverGetRange.
   Eager variant: take that decision synchronously at issue; only when no local writes/clears/
   unreadable overlap and serverCache not fullyKnown, start the server read async (goroutine under
   opContext/trackReadOperation, or a deferred-frame PendingRange like PendingGet); else stay lazy.
   Must handle commit drain, Reset, cancellation, limits/byteTarget/reverse, cache update.
   Opt-in API (e.g. RangeIterator prefetch) so other range reads stay unchanged.
2. recordlayer keyValueCursor: start the iterator at construction when prefetch is requested
   (check initIterator dependencies: continuation, scan limiter, ctx).
3. Window of pre-opened inner cursors: recordlayer FlatMapPipelined (IN-join) and executor
   flat_map_cursor.go (nested loop). Only the head is advanced; continuations unchanged.
Rejected: concurrent inner OnNext (ExecuteState/ScanLimiterState/buffers not goroutine-safe;
pkg/simfdb simTxn not goroutine-safe; breaks DST determinism; executor zero-goroutine invariant).
Findings so far:
- keyValueCursor.initIterator (key_value_cursor.go:819-930) depends only on construction-time state
  (range, continuation, scanProperties, recordsRead=0), so building the iterator at construction is
  safe at the cursor level; the I/O eagerness must come from fdbgo.
- fdbgo eager range: rywCache.getRange's no-write fast path = server read + cacheServerResult; an
  async start taken only when no writes/clears/unreadable overlap at issue gives C++ issue-time
  semantics. The async read MUST be drained by Commit (C++ RYW commit waits for outstanding reads
  so their read-conflict ranges are recorded; fdbgo drains PendingGets the same way) and must obey
  Reset/readIncarnation. Verify against /tmp/fdbsrc ReadYourWrites.actor.cpp before coding.
- Optional-interface opt-in (RangeIterator prefetch) keeps simfdb/libfdbc/decorators lazy+serial.
- C++ commit waits for ALL outstanding reads: ReadYourWrites.actor.cpp:1359 `wait(resetPromise || reading)`,
  every get/getRange does `reading.add(success(result))` (:1691,:1767). fdbgo Commit
  (client/transaction.go ~1824-1841) drains only tx.pendingReads (PendingGets); a concurrent
  goroutine read (e.g. an async range prefetch) is NOT waited for, so its read-conflict range could
  miss the commit. Any async range read must register for this drain (or fix the gap generally:
  commit waits for in-flight read leases of the current incarnation, like `reading`).
- RYW issue-time: decide no-overlap synchronously at issue; a write landing in the range between
  issue and the async server read must not be merged (C++ RYWIterator snapshots the write map).
  Factor rywCache.getRange fast path (ryw.go:653+) into a server-only call for the async arm.
