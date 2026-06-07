# RFC-090 — A3 conformance flake: Java server doesn't retry `transaction_too_old`

**Status:** Draft — Torvalds + bradfitz reviewed; design revised per their NAK
(retry the *not-committed* class only — see Fix).

**Found:** With RFC-089 making conformance CI failures diagnosable, #269's CI
finally showed the real failure (not the join-enum nondeterminism we assumed):

```
[FAILED] scenario "where_literal_on_left" test #2 query "SELECT id FROM t WHERE 10 > n":
  Java errored on its pooled server …
  *plandiff.JavaError: java FDBException: Transaction is too old to perform reads or be committed
  { ExceptionClass: "FDBException", … }
```

That's **FDB error 1007 `transaction_too_old`** — the transaction's read version
aged past FDB's 5-second window before the read/commit completed.

## Problem

A `SELECT id FROM t WHERE 10 > n` over a **3-row** table cannot take 5s on its
own. The 1007 comes from CI-box saturation: RFC-082 parallelized A3 into a pool
of Java conformance servers **plus** parallel Go precompute workers, so the
shared JVM thread running fdb-relational's (plan-cache-disabled, so always-fresh)
Cascades plan + execute gets starved >5s, and the transaction's read version
goes stale. The tell is that an *arbitrary subset* of sibling comparisons fails
(`10 > n`, `10 = n` but not `10 < n` / `10 <= n` / `10 >= n` / `10 != n`) —
pure timing, not a query-specific divergence.

`transaction_too_old` is the canonical **retryable** FDB error: it's exactly
what `FDBDatabase#run()` / the record layer's runner loop absorbs by restarting
the operation on a fresh transaction. The conformance server bypasses that loop —
it runs queries through raw JDBC `executeQuery` on an auto-commit
`EmbeddedRelationalConnection` (`conformance/sql_plan_steps.java#runQuery`),
which surfaces 1007 straight to the client. **A conformance oracle that doesn't
retry retryable FDB errors is buggy** — it manufactures false negatives under load.

## Investigation

- `EmbeddedRelationalConnection` defaults `autoCommit = true` (its source,
  field init line 102) and the conformance server never disables it. So **every
  statement is its own transaction**.
- The FDB binding ships the exact predicate we need: `FDBException`'s
  `isRetryableNotCommitted()` — true for the **retryable AND definitely-not-
  committed** class (1007 `transaction_too_old`, 1020 `not_committed`, 1009
  `future_version`, 1037 `process_behind`, …) but **false for 1021
  `commit_unknown_result`**. The broader `isRetryable()` / `FDBExceptions.isRetriable`
  *includes* 1021 — a maybe-committed write — which is the trap: blindly replaying
  a setup INSERT / CREATE DDL that *did* commit would duplicate a row (spurious
  23505) or silently double-insert and corrupt the SELECT. Restricting to the
  not-committed class keeps the retry idempotent on the write paths too.
- The server's own error handler (`conformance_server.java:200`) walks
  `getCause()` to the root and reaches the `FDBException`, and the record layer's
  `FDBExceptions.getFDBCause` does the same — proving the cause chain is intact,
  so the predicate sees the FDB error.

## Fix

Add a `withFdbRetry(SqlSupplier<T>)` helper in `sql_plan_steps.java` that runs a
**single** auto-commit statement and, on a retryable-and-not-committed FDB error,
retries on a fresh transaction with jittered exponential backoff (base 50→800ms,
additive jitter, 6 attempts / 5 sleeps, up to ~3s total). Predicate: the FDB cause's
`isRetryableNotCommitted()` (falling back to `FDBExceptions.isRetriable` only for
record-core retriable *wrappers* — conflict / lock-taken — which carry no raw
`FDBException` and are not-committed by construction).

Wrap each statement execution **individually** (not the whole op — a 1007 on
setup INSERT #3 must not replay #1–#2): the three `CREATE TEMPLATE/DATABASE/SCHEMA`
DDLs, each `runWithSetup` setup INSERT in its loop, the query (`runQuery`), and
EXPLAIN (`runExplain`). Teardown DROPs are **not** wrapped — they're best-effort,
already `catch`-and-ignored, on unique-UUID paths, so a transient 1007 there just
leaks one ephemeral DB; a retry loop on the cleanup path buys nothing.

**Why this can't mask a real divergence:** the predicate is true *only* for
genuine infra/timing errors. A real Go-vs-Java semantic divergence (wrong plan,
type mismatch, parse error) surfaces as a **non-retryable** `RelationalException`
with a SQLState — the predicate returns false, the error is rethrown immediately,
and the spec still fails loudly. Retry absorbs the CI-load timeout, nothing else.

**Backoff holds a pooled handler thread** for up to ~2.4s; the jitter decorrelates
the wakeups of pool threads that all hit 1007 in the same load spike, and sleeping
during a spike sheds load rather than hammering a saturated box.

**Why determinism is preserved:** the plan cache stays disabled (the existing
determinism guarantee, `sql_plan_steps.java:130`). A retry re-plans the *same*
query from scratch → identical plan → identical rows. Retry changes only whether
a transient infra error is absorbed vs surfaced; it never changes a query result.

~35 lines, conformance harness only. No production code, no test-logic change.

## Test plan

A real 1007 needs CI saturation, which isn't reproducible on demand, so we pin
the contract **deterministically** with a test-only conformance step,
`runWithSetupInjectingFaults` (`sql_plan_steps.java`): it runs setup + query
through the production `withFdbRetry`, but injects N genuine `FDBException`s of a
chosen code before the SELECT executes (genuine → the native predicate
classifies them exactly as live errors; the countdown is a method-local
`AtomicInteger`, so it's isolated per request). `fault_inject_retry_conformance_test.go`
asserts the full contract:

- 1007 ×2 (within budget) → query **recovers**, returns the correct row.
- 1020 ×3 → recovers (the other not-committed code).
- 1007 ×7 (> `MAX_FDB_RETRIES`) → **surfaces** a typed `FDBException` (fails loud,
  doesn't spin forever).
- **1021 ×1 → surfaces immediately, NOT retried** (the maybe-committed write
  must never be replayed — the reviewer-caught hole).
- 1000 ×1 → surfaces immediately (non-retryable).

Plus: rebuild the Java server, run `//conformance:conformance_test` green
(`where_literal_on_left` included). The fix lands first; #269 (GROUP BY over
join, RFC-088) rebases onto it and its CI goes green — the flake that blocked it
is gone.

## Out of scope

- Tuning A3 precompute parallelism (RFC-082). Capping workers to NumCPU would
  reduce contention but is a perf knob, not a correctness fix — a GC pause or
  scheduler hiccup can still starve a thread >5s. Retry is the correctness fix;
  parallelism tuning is complementary and measured separately.
