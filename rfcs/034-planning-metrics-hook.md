# RFC-034: Planning-metrics hook for operational debuggability (P2.2)

Status: Implemented

## Problem

A production operator's first question when a query is slow or wrong is
*"what plan did the planner pick, and how long did planning take?"* Today the
Go engine answers neither. There is:

- no query logging,
- no slow-query log,
- no plan hash / plan text in any log or error,
- no planning-duration measurement,
- no cache hit/miss visibility.

`cascades.PlannerEventHandler` exists but is nil by default and only fires
per-task *inside* the optimizer (explore/transform/optimize callbacks) — it is
the wrong granularity for "log one line per query." The two option constants
`OptLogQuery` and `OptLogSlowQueryThresholdMicros` are defined
(`pkg/relational/api/options.go:55-56`) but **no code consumes them**. The only
way to recover a plan in production is to re-run with `EXPLAIN` under the same
data and parameters — often impossible after the fact.

## Investigation

### Java reference

Java funnels every query through `PlanGenerator.getPlan(query)`
(`fdb-relational-core/.../PlanGenerator.java:127-140`):

```java
public Plan<?> getPlan(String query) throws RelationalException {
    resetTimer();
    KeyValueLogMessage message = KeyValueLogMessage.build("PlanGenerator");
    Plan<?> plan = null; RelationalException exception = null;
    try {
        plan = planContext.getMetricsCollector()
            .clock(TOTAL_GET_PLAN_QUERY, () -> getPlanInternal(query, message));
    } catch (RelationalException e) { exception = e; throw e; }
    finally {
        RelationalLoggingUtil.publishPlanGenerationLogs(
            logger, message, plan, exception, totalTimeMicros(), options);
    }
    return plan;
}
```

`RelationalLoggingUtil.publishPlanGenerationLogs`
(`.../RelationalLoggingUtil.java:37-92`) adds these keys to the message and logs
it (error if exception, info if `LOG_QUERY` or duration exceeds
`LOG_SLOW_QUERY_THRESHOLD_MICROS`, debug otherwise):

- `totalPlanTimeMicros`
- `planHash` (via `PlanHashable.planHash(mode)`) — only for physical plans
- `plan` (the `explain()` text)
- `planCache` ∈ {`skip`, `hit`, `miss`, `inconclusive`}, plus
  `primaryCacheNumEntries` and `generatePhysicalPlanTimeMicros`

Key observations:
- Java does **not** log an absolute "estimated cost" scalar. The Cascades cost
  model is a *comparator* (16 criteria), not a number. The plan identity Java
  logs is `planHash` + `explain()`. **We match Java: no invented scalar cost.**
- The logger is *always* called in the finally block; the options gate the log
  *level*, not whether the record is produced.

### Go reference points

- Single SELECT funnel: `cascadesGenerator.planSelectCascades`
  (`cascades_generator.go:251`) — owns the plan cache lookup/put and the
  `planner.Plan(ref)` call.
- Single DML funnel: `cascadesGenerator.planDML` (`:542`) — calls
  `planner.Plan(ref)`, no cache.
- Plan hash already exists: `plans.PlanHash(p) uint64`
  (`plan_hash.go:15`), deterministic over the plan tree.
- `Explain()` already exists on every plan and is already computed on both
  the cache-hit and fresh-plan success paths.
- Cache hit/miss is decided inside `planSelectCascades`; `PlanCache` exposes
  `Stats()` but not a current-size accessor.

## Fix

Add a **planning-metrics hook** at the SQL-engine layer (Go analog of
`RelationalLoggingUtil` + the `PlanGenerator` finally block), nil by default
(silent, zero overhead). This is *not* the per-task `PlannerEventHandler`; it
is one callback per `Plan()` call.

### New file `pkg/relational/core/embedded/plan_logging.go`

```go
// PlanCacheEvent mirrors Java RelationalLoggingUtil.PlanCacheEvent.
type PlanCacheEvent int
const (
    PlanCacheInconclusive PlanCacheEvent = iota // zero value: errored before a cache decision
    PlanCacheSkip                               // not cacheable (LIMIT/OFFSET) or cache disabled
    PlanCacheHit
    PlanCacheMiss
)
func (e PlanCacheEvent) String() string // "inconclusive"/"skip"/"hit"/"miss"

// PlanGenerationInfo is the diagnostic record for one Plan() call.
// Field set mirrors the keys Java adds to its KeyValueLogMessage.
type PlanGenerationInfo struct {
    SQL              string        // truncated to MaxLoggedSQLLength
    PlanHash         uint64        // 0 when no physical plan was produced
    PlanExplain      string        // "" when no physical plan was produced
    PlanningDuration time.Duration
    Cache            PlanCacheEvent
    CacheNumEntries  int
    SlowQuery        bool          // duration exceeded the slow-query threshold
    Err              error         // non-nil on planning failure
}

// PlanGenerationLogger receives one callback per Plan() call. nil = silent.
// Sampling and log-level policy are the handler's responsibility (Java keeps
// the same split: the engine always emits, the logger/sampler decides volume).
type PlanGenerationLogger interface {
    LogPlanGeneration(ctx context.Context, info PlanGenerationInfo)
}

const MaxLoggedSQLLength = 1024
func truncateSQL(sql string) string // rune-safe truncation + "…(truncated)"
```

### `EmbeddedConnection` wiring (`connection.go`)

Two new fields + setters:

```go
planLogger               PlanGenerationLogger // nil = silent
slowQueryThresholdMicros int64                // slow-query threshold
```

`New(...)` initializes `slowQueryThresholdMicros` from the **single canonical
default** already declared in the options package — it reads
`api.DefaultOptionValues()[api.OptLogSlowQueryThresholdMicros]` (= 2_000_000)
rather than re-hardcoding the literal, so there are not two sources of truth
for the threshold. `SetPlanLogger(PlanGenerationLogger)` and
`SetSlowQueryThresholdMicros(int64)` expose configuration.

### Why `OptLogQuery` stays unwired (deliberate, documented)

Java's `RelationalLoggingUtil` uses `LOG_QUERY` to pick the SLF4J **log level**
(info vs debug) of a record it always builds. Go has no SLF4J and no ambient
log-level concept: the engine emits every record to the hook, and the
*handler* is the policy — it decides level, sampling (the ~1% target), and
sink. So `OptLogQuery` (a level knob) has nothing to gate until a built-in
default logger exists. It therefore remains intentionally unconsumed pending
the options-plumbing work for the gRPC/REPL frontends (no `Options` instance
flows through `EmbeddedConnection` today). This RFC does **not** silently leave
the dead-constant smell it diagnosed: it converts `OptLogSlowQueryThresholdMicros`
into the live default source above, and records here that `OptLogQuery` is
parked by design, not oversight. A code comment at `options.go:55` will point
to this RFC.

### Shared scope helper (faithful port of Java's try/finally)

```go
type planLogScope struct {
    g     *cascadesGenerator
    ctx   context.Context
    sql   string
    start time.Time
    plan  plans.RecordQueryPlan
    cache PlanCacheEvent
}

// nil return = logging disabled → all methods no-op, zero overhead.
func (g *cascadesGenerator) beginPlanLog(ctx, sql) *planLogScope
func (s *planLogScope) setPlan(plans.RecordQueryPlan)
func (s *planLogScope) setCache(PlanCacheEvent)
func (s *planLogScope) finish(err error) // computes hash/explain/duration, emits
```

`finish` only computes `PlanHash` (a tree walk) when logging is enabled —
nil scope short-circuits, so a nil logger pays nothing beyond one nil compare.

### Call-site changes (named-return defer = Java's finally)

`planSelectCascades` gains a `logMetrics bool` parameter so the **EXPLAIN
path does not double-log**. `computeExplainText` (`cascades_generator.go:427`)
re-enters `planSelectCascades` purely to render plan text; Java's `getPlan`
funnel does not fire for EXPLAIN-internal planning, so we pass `false` there.
The real query path (`planSelect:218`) passes `true`.

```go
func (g *cascadesGenerator) planSelectCascades(
    ctx, q, md, logMetrics bool) (plan query.Plan, err error) {
    var ls *planLogScope
    if logMetrics {
        // canonicalTextOf recovers the original whitespace-preserved SQL from
        // the token interval; q.GetText() (still the cache key) would log
        // token-concatenated garbage like "SELECTid=1FROMorders".
        ls = g.beginPlanLog(ctx, canonicalTextOf(q)) // nil if logger unset
    }
    defer func() { ls.finish(err) }()
    // cache hit:        ls.setPlan(cachedPlan); ls.setCache(PlanCacheHit)
    // fresh + cached:   ls.setPlan(physPlan);   ls.setCache(PlanCacheMiss)
    // fresh, not cached (LIMIT/OFFSET, or g.cache == nil): setCache(PlanCacheSkip)
    // error before plan: scope stays PlanCacheInconclusive, PlanHash == 0
}
```

`setCache` is called on the success path **before** `g.cache.Put` so the event
is recorded even though the named-return defer fires after the `return`. The
three non-hit outcomes are enumerated explicitly: `Miss` (planned and stored),
`Skip` (planned but deliberately not cached — `LIMIT`/`OFFSET`, or no cache
configured), `Inconclusive` (errored before any cache decision; zero value).

`planDML`: same defer pattern; cache is always `PlanCacheSkip` (DML is never
cached). Only `planDML` itself logs — `planDMLExplainOnly` and the
`buildLogicalPlanFor*` EXPLAIN-DML paths never reach it.

**Reachability note (honest scope).** `planDML` (the Cascades DML planning
step) is entered only via `QueryContext`. `ExecContext` sets `execMode` and
routes DML through the non-Cascades `execStatement` path (`planOne:125`), which
this hook does not cover. And `QueryContext` plans the DML — firing the hook —
then rejects the resulting update plan (`connection.go:359`, "only SHOW and
SELECT"). So today `planDML`'s output is a throwaway: its plan is never
executed. Instrumenting it is still correct and minimal: it is a genuine
planning funnel, consistent with the SELECT path, and becomes live for free
once the DML execution path is unified onto Cascades (CLAUDE.md's "one query
path" direction). The hook is *not* claimed to make DML execution observable —
only the Cascades DML *planning* step.

Note: the `defer func() { ls.finish(err) }()` closure is allocated even when
`ls == nil`, so "zero overhead" is one nil-compare in `finish` plus a cheap
closure alloc, not literally nothing. Negligible against a planner run; called
out for honesty rather than overclaiming.

### `PlanCache.Len()`

Add an O(1) size accessor for `CacheNumEntries` (mirrors Java's
`primaryCacheNumEntries`).

## Performance

- Logger nil (production default and every existing test): `beginPlanLog`
  returns nil after one comparison; every scope method is a nil-guarded no-op;
  `finish` returns immediately. No `PlanHash`, no `time.Now`, no allocation.
- Logger set: one `time.Now()`/`time.Since`, one `plans.PlanHash` tree walk,
  reuse of the already-computed `Explain()` string, one `PlanCache.Len()`
  (O(1)), one struct passed by value. Negligible against planning itself.
- Sampling lives in the handler, so a 1%-sampled production logger pays the
  `PlanHash` cost only on emitted queries if it chooses to (it can early-return
  in `LogPlanGeneration`); the default no-op path never reaches it.

## Test plan

White-box unit tests in `package embedded`, no FDB needed.
`NewExplainOnlyGeneratorWithSchema` is **not** a valid vehicle: it builds a
session with `sess.DB == nil`, which routes SELECT through
`planSelectExplainOnly` (`:178`), bypassing `planSelectCascades` entirely.
Instead the tests build `md` from DDL via `buildSchemaTemplateFromDDL(...).Underlying()`,
parse the SQL to an `IQueryContext`, construct an `EmbeddedConnection{sess:
{Schema:"s"}, planLogger: capture}` with a live `PlanCache`, and call
`g.planSelectCascades(ctx, q, md, true)` directly. `fetchTableStatistics`
already no-ops to nil default stats when `sess.DB == nil`
(`cascades_generator.go:1058`), so the real planner + cache + logger run
without FDB. The capturing logger records each `PlanGenerationInfo`.

1. **Miss then hit**: same SELECT twice; first event `Cache==Miss`, second
   `Cache==Hit`, identical non-zero `PlanHash`, non-empty `PlanExplain`,
   `PlanningDuration > 0`, `CacheNumEntries == 1`.
2. **Skip (LIMIT)**: a `LIMIT`/`OFFSET` query reports `Cache==Skip`, not cached.
3. **Skip (no cache)**: a generator with `cache == nil` reports `Cache==Skip`.
4. **Error**: an unplannable query emits one event with `Err != nil`,
   `Cache==Inconclusive`, `PlanHash==0`.
5. **Slow-query flag**: threshold `1` (µs) flips `SlowQuery==true`; a huge
   threshold keeps it false.
6. **Nil logger**: queries plan normally; the nil-scope no-op path is exercised
   (regression guard that the defer is safe with a nil scope).
7. **EXPLAIN no double-log**: calling `planSelectCascades(..., logMetrics:false)`
   emits **zero** events even with a logger set — proves the EXPLAIN re-entry
   at `:427` is silent.
8. **Truncation**: SQL longer than `MaxLoggedSQLLength` is truncated, rune-safe.

DML (`planDML`) logging is covered by two FDB integration tests in the
`sqldriver` package, which reach the planner through the public driver and
install the logger via `sql.Conn.Raw` → `EmbeddedConnection.SetPlanLogger`:
- `TestFDB_PlanLogging_DML`: a `DELETE` via `QueryContext` fires `planDML`,
  asserting one event with `Cache==Skip` and a valid plan hash. The query then
  hits the expected `QueryContext` update-plan rejection (see reachability
  note); the test tolerates that and asserts the captured event. (`DELETE` not
  `INSERT` — the Cascades INSERT path has a separate pre-existing extraction
  gap, out of scope here.)
- `TestFDB_PlanLogging_SelectMissThenHit`: a `SELECT` run twice on the same
  pinned connection asserts miss-then-hit through the public driver, proving
  the SELECT funnel logs end-to-end (not just via the white-box harness).

All 46 existing targets must stay green (nil logger = unchanged behavior).
