# RFC-138 — `enforcedPostTransactionDelay` indexer throttle option (Java 4.12, RFC-135 §4 R2b)

**Status:** Draft (v5 — wiring corrected over four codex rounds: gated-behind-retries / before-range (v2);
replace-rps / skip-final-range (v3); time-limit checked-before-and-accounting-for the delay (v4);
context-aware sleep + clamp negative delays (v5))
**Item:** RFC-135 §4 **R2 part (b-i)** — port Java 4.12's `setEnforcedPostTransactionDelay` online-indexer
throttle option (commit `298cf786f` #4229). (The typed-records range-preset optimization, #4244, is a
separate change — R2c.)
**Reviewers:** Torvalds + codex + @claude. Record-layer (online-indexer throttling), **no Graefe gate**.

---

## 1. Problem (verified real)

Java 4.12 added `OnlineIndexOperationConfig.setEnforcedPostTransactionDelay(long ms)` (#4229): an
alternative to the records-per-second throttle. When set to a positive value, the indexer waits exactly
that many milliseconds after **each** build transaction, *instead of* the records-per-second–based
delay. `DEFAULT_ENFORCED_POST_TRANSACTION_DELAY = 0` (disabled → records-per-second behaviour, as
today). In Java's `IndexingThrottle.Booker.waitTimeMilliseconds()` the new branch is first:

```java
final long enforcedPostTransactionDelay = common.config.getEnforcedPostTransactionDelay();
if (enforcedPostTransactionDelay > 0) {
    return enforcedPostTransactionDelay;   // unconditional fixed delay per transaction
}
// else: records-per-second computation
```

Go's `indexingThrottle` (`pkg/recordlayer/indexing_throttle.go`) has the records-per-second limiter
(`waitForRateLimit`, `:127`) but **no** enforced-post-transaction-delay option — a real 4.12 parity gap.

## 2. Investigation (Go shape)

Go's `waitForRateLimit` (`:127-160`) returns immediately when `recordsPerSecond <= 0` or no records were
scanned since the last reset, else computes `expectedMs = 1000*scanned/recordsPerSecond`, subtracts
elapsed, caps at 999 ms, and sleeps. The throttle is built by `newIndexingThrottle(initialLimit,
maxRetries, recordsPerSecond)` (`:28`), constructed from the `OnlineIndexer` at Build time; the
`OnlineIndexer`/`OnlineIndexerBuilder` carry `recordsPerSecond` (`SetRecordsPerSecond`) already. So the
enforced-delay value threads the same way: builder → `OnlineIndexer` → `newIndexingThrottle`.

## 3. Fix

1. Add `enforcedPostTransactionDelay int` (milliseconds, default 0 = disabled) to `indexingThrottle`
   (+ `newIndexingThrottle` param) and to `OnlineIndexer`, with builder
   `SetEnforcedPostTransactionDelay(ms int)` (matches Java `setEnforcedPostTransactionDelay`).
2. **Apply it POST-transaction, independent of retries** (corrected after review). A v1 attempt put the
   branch in `waitForRateLimit`, but that method (a) is called only when `maxRetries > 0`
   (`buildRangeWithRetries` short-circuits to `buildFn` otherwise — so the knob was a no-op in the
   **default** config) and (b) is called *before* `buildFn` (so a fixed delay would fire before the first
   range, not post-transaction). Instead: a dedicated `indexingThrottle.applyEnforcedPostTransactionDelay()`
   (sleeps if `> 0`) is called in `buildRangeWithRetries` **after** each successful `buildFn`. The loop now
   runs whenever a throttle exists (not gated by `maxRetries`); the records-per-second `waitForRateLimit`
   stays gated behind `maxRetries > 0` and before `buildFn` (a **pre-existing** Go limitation — Java always
   throttles — tracked separately, not entangled with this knob). `handleSuccess` in the `maxRetries == 0`
   path is benign (it only raises the limit when `recordsLimit < initialLimit`, which never happens without
   failures).
3. **Replace rps, not stack with it, and skip the final range** (v3). The enforced delay *replaces* the
   records-per-second throttle (Java returns one or the other), so when it is set the rps
   `waitForRateLimit` is skipped (`if oi.maxRetries > 0 && !enforcedDelay`) rather than both firing. And it
   is a *between-transactions* delay: it is applied only when the committed range returned `hasMore == true`
   — the final range (`hasMore == false`) is skipped (the build is done; no next transaction to pace),
   giving Java's / the RFC's `(chunks−1)·D`.
4. **Check the time limit before (and accounting for) the delay** (v4). Java's
   `validateTimeLimit(toWait)` (`IndexingBase.java:532,541-551`) runs *before* the wait and throws
   `TimeLimitException` if `startingTime + limit < now + toWait` — i.e. it never sleeps a delay that
   would push past the deadline. So the delay is applied in the build loop via a new
   `OnlineIndexer.throttleBetweenRanges(startTime)` which (a) returns `TimeLimitExceededError` *without
   sleeping* when `elapsed + enforcedDelay >= timeLimit`, then (b) applies the delay. It replaces the
   loops' prior plain time-limit check (equivalent when no delay is set), and is called only between
   ranges (the final range, `!hasMore`, breaks before it). `buildRangeWithRetries` no longer applies the
   delay.
5. **Context-aware + clamp** (v5). The sleep is `select { case <-ctx.Done(): case <-timer.C: }`
   (`applyEnforcedPostTransactionDelay(ctx)`), so a cancelled/deadline-hit build is not blocked for the
   full delay between ranges. A non-positive `enforcedPostTransactionDelay` is clamped to 0 in the
   time-limit math (`throttleBetweenRanges`) so a *negative* value behaves exactly like "disabled" and
   cannot subtract from `elapsed` and loosen the deadline.
6. Wire `OnlineIndexer.enforcedPostTransactionDelay` into `newIndexingThrottle` at Build time +
   `SetEnforcedPostTransactionDelay` builder.

## 4. Performance

Off by default (0 → no behaviour change). When enabled, it deliberately adds a fixed inter-transaction
delay (a throttling feature). No effect on the records-per-second path.

## 5. Wire / behaviour impact

None on persisted bytes. A build-time throttling knob; default preserves current behaviour exactly.

## 6. Test plan

- Unit: a throttle with `enforcedPostTransactionDelay = D > 0` waits ~D ms in `waitForRateLimit`
  regardless of `recordsPerSecond` / records-scanned (including the `recordsScannedSinceForcedDelay == 0`
  case, where the records-per-second path would return immediately) — measure elapsed ≥ D. With the
  option 0, behaviour is byte-identical to today (records-per-second path).
- Builder: `SetEnforcedPostTransactionDelay` threads the value to the throttle; default is 0.
- **FDB integration test (revert-proof for the wiring):** a build with `SetEnforcedPostTransactionDelay(D)`
  and a small `SetLimit` (multiple chunks) but **no `SetMaxRetries`** (default 0) takes ≥ ~(chunks−1)·D —
  proving the delay fires under the default config and per committed transaction. Without the corrected
  wiring it finishes in ~ms (the codex-found no-op).

## 7. Scope

One commit on the RFC-135 branch (PR #336): the throttle field + apply branch + builder + wiring + tests.
R2c (typed-record range preset) and R3–R8 remain separate.
