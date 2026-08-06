# RFC-211: Post-execution query statistics

Status: Implemented

## Problem

`PlanGenerationLogger` (RFC-034) reports **planning**. It is the only
per-query telemetry the engine has, and its field set says so: `SQL`,
`PlanHash`, `PlanExplain`, `PlanningDuration`, `Cache`, `CacheNumEntries`,
`SlowQuery`, `Err` (`plan_logging.go:50-71`). The record is built entirely in
`planLogScope.finish` from the planning clock and the plan tree
(`plan_logging.go:142-166`), and the callback fires once per `Plan()` call.

Nothing reports what a statement then *did*: no rows returned, no records
scanned, no bytes read, no execution time, no retry count. A multi-tenant
operator cannot attribute cluster load to a tenant from the engine's own
telemetry — the numbers exist, they are simply never surfaced.

They really do exist. `ScanLimiterState`
(`pkg/recordlayer/scan_limiter_state.go:82-94`) already counts
`recordsScanned` and `bytesScanned` per execution attempt, because the
scanned-rows / scanned-bytes limits are checked against exactly those
counters. Seven leaf cursors charge them (`key_value_cursor.go:237,240`,
`index_scan.go:565-566`, `record_key_cursor.go:136,167`, `text_cursor.go:46,171`,
`count_index_maintainer.go:244,285`, `bitmap_value_index_maintainer.go:609-610`,
`multidimensional_index_maintainer.go:691-692,1119,1133`). The executor knows
rows returned (`paginatingRows.emitted`). The retry loop knows attempts. The
work is collection and delivery, not measurement.

## Investigation

### Java reference

Java's execution-side instrumentation is the per-**transaction**
`FDBStoreTimer` hung off `FDBRecordContext`
(`FDBRecordContextConfig.java:42`, getter `:101`, builder setter `:365`;
`FDBRecordContext.java:186` passes it to `FDBTransactionContext`, which holds
it at `FDBTransactionContext.java:49-64`). The execution-relevant members are
timed `Events` — `LOAD_RECORD` (`FDBStoreTimer.java:122`), `SCAN_RECORDS`
(`:124`), `QUERY_FILTER` (`:220`) — and `Counts`: `LOAD_KEY_VALUE` (`:530`),
`LOAD_RECORD_KEY` (`:534`), `LOAD_RECORD_KEY_BYTES` (`:536`),
`LOAD_RECORD_VALUE_BYTES` (`:538`), `BYTES_READ` (`:717`). Counters are read
back with `getCount(Event)` (`StoreTimer.java:658`), `getTimeNanos`
(`:646`), `getCounter` (`:150`); per-operation deltas need
`StoreTimerSnapshot.from(timer)` (`StoreTimerSnapshot.java:67`) plus
`StoreTimer.getDifference` (`StoreTimer.java:101`).

The scanned-record / scanned-byte accounting Go's `ScanLimiterState` ports is
Java's `ExecuteState` (`ExecuteState.java:44-48`) wrapping a
`RecordScanLimiter` + `ByteScanLimiter`, read back through
`getRecordsScanned()` (`ExecuteState.java:114`) and `getBytesScanned()`
(`:122`).

Two properties of that pair decide this design:

1. **The consumed count survives the limit.** The limiters are mutable objects
   owned by the caller's `ExecuteState`, so after `CursorLimitManager`
   (`cursors/CursorLimitManager.java:134-149`) either throws
   `ScanLimitReachedException` or stops the cursor with
   `NoNextReason.SCAN_LIMIT_REACHED`, `executeProperties.getState()
   .getRecordsScanned()` still reads the accumulated value. The count is
   attached to neither the exception (`ScanLimitReachedException.java:36` is a
   bare message-only `RecordCoreException`) nor the `RecordCursorResult`
   (which carries `get()`/`getContinuation()`/`getNoNextReason()` and no
   counters). Reading the counter object you already hold is therefore *the*
   Java-sanctioned way to report what a killed statement consumed.
2. **Java never accumulates across continuations.** `ExecuteState.reset()`
   (`:74`) returns a *new* limiter pair with the same original limit,
   discarding the accumulated counts, and `ExecuteProperties.resetState()`
   (`ExecuteProperties.java:317`) is how each continuation segment gets a
   fresh budget. Nothing in Java sums the segments.

### What fdb-relational surfaces — a hard negative

Java's SQL layer has **no per-query execution-metrics surface**. Verified by
search over the module a SQL/JDBC user compiles against:

- `fdb-relational-api/src/main/java` matches nothing for
  `metric|telemetry|statistic|stats|listener|callback|recordsScanned|bytesScanned`.
  Same for `fdb-relational-jdbc/src/main/java` and
  `fdb-relational-grpc/src/main` (including the `.proto` files).
  `RelationalConnection` (`RelationalConnection.java:54`) is
  `interface RelationalConnection extends java.sql.Connection` and nothing more.
- `grep -rn "getRecordsScanned\|getBytesScanned\|getExecuteState"
  fdb-relational-core/src/main/java` finds nothing. Relational *configures*
  scan limits (`EmbeddedRelationalConnection.java:518-525`, notably
  `.setFailOnScanLimitReached(false)` at `:523`) and never reads the consumed
  counts back.
- The one execution fact that escapes to a caller is a *reason*, never a
  number: `RecordLayerIterator.java:123-150` maps `NoNextReason` to
  `terminatedEarly()` plus a human string.

What relational *does* have is `MetricCollector`
(`fdb-relational-core/.../api/metrics/MetricCollector.java:42`), implemented by
`RecordLayerMetricCollector` (`.../recordlayer/metric/RecordLayerMetricCollector.java:46`),
whose own javadoc says it is "bound to a `FDBRecordContext` object and hence
scoped to a single transaction" (`:42`). It forwards to `context.increment` /
`context.record` and reaches the timer via `getUnderlyingStoreTimer()` (`:106`,
`@VisibleForTesting`). Its execution-side members are **timings only** —
`EXECUTE_RECORD_QUERY_PLAN` (`RelationalMetric.java:78`),
`CREATE_RESULT_SET_ITERATOR` (`:82`), `TOTAL_EXECUTE_QUERY` (`:88`),
`EXECUTE_QUERY_PLAN` (`:92`), `TOTAL_PROCESS_QUERY` (`:104`), clocked at
`AbstractEmbeddedStatement.java:76,112,135` and `query/Plan.java:99`. Every
`RelationalCount` (`RelationalMetric.java:128-138`) is a plan-cache or
continuation counter. **No rows scanned, no bytes read, no retries.** And the
collector is reachable only by downcasting to the concrete
`EmbeddedRelationalConnection` (field `:97`, getter `:279`) — impossible over
JDBC or gRPC. Its sink is a process-global Dropwizard `MetricRegistry`
(`MetricRegistryStoreTimer.java:46`, installed at
`RecordLayerTransactionManager.java:64`), not a per-query record.

Retry counts are worse: `FDBDatabaseRunnerImpl.RunRetriable.currAttempt`
(`FDBDatabaseRunnerImpl.java:168-170`) is a private field of a private inner
class, incremented at `:227` and escaping only into a log line (`:209-217`).
The sole programmatic trace is the timing of `Events.RETRY_DELAY`
(`FDBStoreTimer.java:250`, instrumented at `FDBDatabaseRunnerImpl.java:222`) —
an artifact, not an exposed count, and per-runner rather than per-query.

### Consequence for the design

The split is clean, and it is the split CLAUDE.md names:

- **The count source is a port.** Reading the scan counters off the execution
  state *after* execution — including after the limit killed the statement — is
  precisely Java's `executeProperties.getState().getRecordsScanned()`. Go
  already has the object; nothing new is measured.
- **The per-statement aggregation and the delivery hook are a Go-only
  read-side extension**, because Java has neither (it discards counts at every
  `reset()`, and exposes nothing to a SQL caller at all). Wire compat is
  untouched: this reads counters that already exist and writes nothing.

## Fix

### Design decision: a second interface, not a second method

The brief's open question was whether to extend `PlanGenerationLogger` with a
post-execution callback or add a parallel hook. **Parallel hook**, for three
reasons:

1. **Extending is a breaking change with no Go escape hatch.** Java can add a
   method to `MetricCollector` because Java has `default` methods — and it
   uses them exactly so (`MetricCollector.java:78,89,99,107` are all
   `default`). Go has no equivalent: adding `LogExecutionStats` to
   `PlanGenerationLogger` breaks every existing implementer at compile time,
   including the `tenantPlanLogger` that `docs/mt-saas.md:499-509` tells
   operators to write.
2. **Java keeps the two separate.** Planning metrics go through
   `PlanGenerator`'s finally block into `RelationalLoggingUtil`; execution
   metrics go through `AbstractEmbeddedStatement`'s
   `metricCollector.clock(TOTAL_PROCESS_QUERY, …)` (`:76,112,135`). Two
   mechanisms, two lifetimes. One interface with two methods would assert a
   coupling neither engine has.
3. **The lifetimes genuinely differ.** A plan is planned once and may be
   executed many times (the plan cache is the point). One `Plan()` call maps to
   one planning record; one `Execute()` maps to one execution record. Binding
   them into one interface invites the assumption that they pair up 1:1, which
   the cache makes false.

So: `ExecutionStatsLogger`, `SetExecutionStatsLogger`, shaped in every other
respect exactly like RFC-034 — one record struct, one single-method interface,
nil is silent, the engine always emits and the *handler* owns level/sampling/
sink.

### New file `pkg/relational/core/embedded/execution_logging.go`

```go
type ExecutionStats struct {
    SQL               string        // truncated to MaxLoggedSQLLength
    PlanHash          uint64        // ties the record to its PlanGenerationInfo
    ExecutionDuration time.Duration // Java's TOTAL_EXECUTE_QUERY, per statement
    RowsReturned      int64         // rows handed to the caller (SELECT)
    RowsAffected      int64         // rows mutated (DML)
    RecordsScanned    int64         // summed ScanLimiterState.RecordsScanned
    BytesScanned      int64         // summed ScanLimiterState.BytesScanned
    Pages             int           // fetchPage calls
    Retries           int           // attempts beyond the first, summed over pages
    SlowQuery         bool          // ExecutionDuration exceeded the threshold
    Err               error         // nil on success
}

type ExecutionStatsLogger interface {
    LogExecutionStats(ctx context.Context, stats ExecutionStats)
}
```

`execLogScope` is the `planLogScope` analog: nil means disabled, every method
is nil-guarded, `finish` is idempotent.

### Collection points

**Scanned records / bytes — a delta, not a read.** `executeProps()`
(`cascades_generator.go:1756`) mints a fresh `ScanLimiterState` per call in
auto-commit but assigns the *transaction-scoped* state inside an explicit
transaction (`:1812-1814`, RFC-198 Decision 5). Reading the absolute counter at
page end would therefore be correct for auto-commit and cumulative — hence
quadratically over-counted — inside a transaction. The scope charges
`end - start` around each attempt instead, which is this attempt's true
contribution under both lifetimes. The charge is a `defer` registered
immediately after `props` is built, so it fires on **every** exit from the
attempt: clean completion, a translated execution error, and the 54F01 a scan
limit raises. That is what makes "a statement killed by
`EXECUTION_SCANNED_ROWS_LIMIT` still reports what it consumed" structural
rather than a special case — and it is the same guarantee Java gets from the
limiter object outliving `ScanLimitReachedException`.

**Retries.** `runInCapturedTx` (`connection.go:366-375`) either calls the
closure directly (explicit transaction, no retry loop) or hands it to
`DB.Run` → `runTransactCtx` → `Transact`, which re-executes it per attempt.
A local attempt counter incremented at the top of the closure, with
`attempts-1` added to the scope after the call returns, counts exactly the
retries and cannot go negative if the closure never ran.

Counting a failed attempt's scanned records is deliberate: the cluster served
those reads. A statement that scanned 5 records, conflicted, and rescanned 5
cost the cluster 10, and cost attribution that reported 5 would understate
precisely the tenants worth knowing about.

**Rows.** `RowsReturned` is `paginatingRows.emitted`, incremented in `Next`
after the egress byte cap (`:1622`) — rows actually handed back. DML never
goes through `Next` (`Execute` drains via `countAll` at `:1376`), so DML
reports `RowsAffected` and leaves `RowsReturned` zero.

### Emission point

`paginatingRows.Close()`. It is the one funnel every path reaches: the
`fetchPage` error path (`Execute:1366-1369`), the DML path (`:1375-1382`), and
`database/sql` closing an exhausted or abandoned result set. `Close` emits with
`r.statsErr`, which `Next` sets on any non-`io.EOF` error and the two `Execute`
paths set explicitly. `finish` is idempotent, so the repeated `Close` that
`database/sql` may issue emits once.

### Slow-query threshold on execution time

`slowQueryThresholdMicros` (`connection.go:77`) gains a second consumer.
`SlowQuery` on an `ExecutionStats` means the *execution* exceeded the
threshold, exactly as `SlowQuery` on a `PlanGenerationInfo` means planning did.
One knob, two independently-reported dimensions — an operator who sets 500 ms
now learns which half was slow instead of only ever hearing about planning.

## Performance

Logger nil (production default, every existing test): `beginExecLog` returns
nil after one comparison, the SQL text is never materialized (`execLogSQL`
short-circuits before `canonicalTextOf`), `PlanHash` is never walked, and every
scope method returns on its nil check. The per-attempt delta `defer` is
registered unconditionally — one closure allocation per page — and charges a
nil scope, which is two nil compares. Called out rather than claimed away, for
the same reason RFC-034 called out its own defer.

Logger set: two `time.Now()`, one `plans.PlanHash` walk per statement, four
integer adds per page, one struct by value. Negligible against the FDB round
trips the statement already paid for.

## Test plan

FDB integration tests, `t.Parallel()`, in
`pkg/relational/sqldriver/execution_stats_fdb_test.go`, driving the public
`database/sql` surface and installing the logger through the documented
`conn.Raw` → `SetExecutionStatsLogger` route.

Counts are pinned exactly, not bounded:

1. **Full scan** of an N-row table reports `RecordsScanned == N`,
   `RowsReturned == N`, `BytesScanned > 0`.
2. **Index equality** reports the index's selectivity — strictly fewer records
   scanned than the table holds, and equal to the matching rows — proving the
   number tracks the access path and is not a row-count echo.
3. **`LIMIT k`** reports `RowsReturned == k` alongside the true, larger
   `RecordsScanned` — the case where returned and scanned must not be the same
   number.
4. **`EXECUTION_SCANNED_ROWS_LIMIT` with `FailOnScanLimitReached`** reports the
   consumed amount alongside a non-nil `Err` carrying 54F01.
5. **Retries** pinned with the `retryOnceBackend` shape that
   `sim_sql_page_commit_retry_test.go` established: a forced retryable failure
   at a chosen transaction ordinal yields `Retries == 1`, and the scanned count
   reflects both attempts.
6. **DML** reports `RowsAffected` and zero `RowsReturned`.
7. **Slow-query on execution**: threshold 1 µs flips `SlowQuery`; a huge
   threshold keeps it false.
8. **Nil logger** leaves every path working (the nil-scope no-op guard).

Each dimension gets a mutation check: breaking that dimension's collection in
isolation must turn its assertion red.
