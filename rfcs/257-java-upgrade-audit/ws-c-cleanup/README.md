# WS-C implementation finding: bounded terminal heartbeat cleanup

Addresses the cleanup finding in the NAK of tree
`2a4d33046f8640673643ea989709665f425cbbc5`; no final-tree ACK or CI result claimed.

Java reference: 4.14.2.0 `IndexingBase.clearHeartbeats`, removing only the current
indexer's keys and ignoring terminal cleanup errors. The accepted Go lifecycle
also requires an independent 30-second bound. `DB.Run` cannot provide that bound:
its dispatched commits deliberately detach from caller cancellation.

Cleanup now owns raw-clear transactions and their commit futures, with the
standard runner retry classification, attempt limit and backoff. Each attempt's
FDB timeout is the **remaining** context budget, never a fresh 30 seconds.
The readiness wait itself observes context cancellation and enters `Get` only
when `IsReady` is true. Every exit requests cancellation of the future and owned
transaction. This distinction is necessary: the pure-Go future's `Cancel` is a
no-op and its dispatched commit is detached from transaction cancellation and
timeout. No blocked `Get` goroutine is spawned or abandoned by cleanup. The
backend may finish its detached own-key clear after the caller returns. The
identity is one per OnlineIndexer (Java's single `indexerId`, WS-C addendum
revision 3), so the indexer's next attempt, `BuildIndex` or `MergeIndexes` writes
the same key; a late clear landing after that write would erase a live heartbeat.
The cleanup therefore reads each key before clearing it, so a late commit
conflicts with any write of the key since its read version, or is rejected as too
old (`OnlineIndexer bounded heartbeat cleanup`: "a cleanup clear that lands after
the next attempt wrote the heartbeat leaves it in place", with the commit held
past the deadline and landed after the next write). An earlier revision called
late clears harmless because each build minted a new UUID; that premise went
with the per-attempt identity. Unknown-outcome retries are idempotent own-key
clears. The caller retains its original build
result; expiry remains the fallback. No client/backend implementation changed.

The wall-clock `time.Until` site is explicitly allowed by the DST gate because it
compares a native `context.WithTimeout` deadline, not persisted timestamps. The
initial full verification correctly failed until this reason was registered.

## Initial evidence (before the detached-future correction)

The initial implementation relied on future/transaction cancellation to interrupt
`Get`. That was insufficient for a detached pure-Go commit. The initial four
specs used a cooperative cancellation barrier and therefore did **not** establish
the bound for a future ignoring cancellation. The following historical results
remain measurements of that earlier source, not final verification:

Raw logs: `/var/tmp/fdb-upgrade-recovery/ws-c/`.

* Four real-FDB regression specs in `OnlineIndexer bounded heartbeat cleanup`:
  deadline and explicit cancellation while waiting on a **real dispatched commit**,
  a real competing-write conflict followed by successful retry with non-increasing
  timeout budget and peer/legacy key preservation across two targets, and an
  already-cancelled build whose independent cleanup still executes and whose
  original cancellation remains the returned error.
* The commit barrier delays the wait on the real future; it does not invent commit
  results or simulate a network outage. The wrapper forwards the optional
  `CtxTransactor` capability so it preserves the real backend's cancellation path.
  These tests use the suite's pure-Go FDB handle; no C-backend execution claimed.
* `cleanup-target.log`: 9/3574 specs passing (four cleanup + five format-15 lifecycle).
* `cleanup-{cancel,timeout}-mutation.log`: removing commit-future cancellation or
  setting timeout to zero each failed the selected real-FDB regression. Both
  mutations were verified present, compiled and executed; source restored.
* `cleanup-final-full.log`: `just test`, **93/93**, **1 executed/92 cached**,
  118.040 seconds. The prior full run executed six targets and failed only the
  missing DST allowance; it was not green.
* `cleanup-race.log`: actual `--@rules_go//go/config:race`, uncached,
  **79/3574 Ginkgo specs** plus `TestBuildIndexTimeLimitUsesTheEnvClock` and both
  subtests, passing. Focus combines admission, heartbeat, batch fencing, mutual,
  bounded cleanup, and format-15 lifecycle. Not a full-suite race claim.
* Seven source hashes checked unchanged after both final full and race runs:
  `cleanup-source.sha256`.

## Detached-future correction

Source trace: `fdb.Transaction.Commit` returns a plain `futureNil`; its base
`Cancel` is a no-op. `PendingCommit.Resolve` reaches `commitAdmitted`, which
releases its incarnation lease and dispatches via `context.WithoutCancel`.
`commitInput` carries no transaction deadline. Its uncertain-delivery barrier
can wait for database shutdown. Transaction cancellation alone cannot bound
cleanup's wait on this path.

The barrier now holds **readiness**, optionally ignores cancellation, and counts
calls to `Get`. Deadline and explicit-cancel cases require zero `Get` calls while
the future is not ready. A successful released commit is collected exactly once.
`cleanup-detached-target.log`: 11/3598 specs passing (six cleanup + five format-15
lifecycle). `cleanup-detached-mutation.log` reverts to blocking `Get` plus
cancellation: it built and failed the selected ineffective-cancel regression.
The corrected implementation is included in the final **93/93 uncached** full
run, **183/3604** record-layer race specs plus ordinary Go tests/seeds, and
**9/1523** JVM race specs. Exact commands, populations, hashes, and logs:
`../ws-c-followup-liveness/README.md`.

The nested-Any and mutual-renewal findings have their own fixes/evidence in
`../ws-c-nested-any/` and `../ws-c-mutual-renewal/`. Multi-target follow-up
liveness is still under implementation. No commit, publication, CI trigger,
or merge performed.
