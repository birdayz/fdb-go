# RFC-124 — AtomicOps workload: atomic-op + companion-write transactional-consistency oracle

**Status:** Accepted
**Item:** TODO.md C3 ("Ride their test designs"), increment 5 — the **AtomicOps** workload (RFC-119 §7

> **Reviews.** FDB-C-dev **ACK** — `sum(log)==sum(ops)` is faithful and holds exactly under 1021 (log-set
> + atomicOp in one `t.mutations` commit, `NativeAPI.actor.cpp:5972/6002`; retry is a fresh op via the
> in-loop `state` decls); the same-op double-apply is confirmed faithful (libfdb_c de-dup is gated on
> `req.idempotencyId.valid()`, `:6751-6768`, default-invalid → `commitDummyTransaction` is a
> self-conflict barrier, not de-dup); `MutAddValue`+`Set` share the atomic commit boundary. Torvalds
> **ACK** — strictly stronger than `TestConcurrentAtomicAdd` (companion-log catches torn/lost commits a
> bare sum can't); proved no interleaving violates exact equality *given* the unique-per-attempt logKey.
> **Binding impl conditions:** (1) the op (`group/node/val`) is drawn from the RNG **inside** the
> `db.Transact` fn — fresh per retry (the 1021-replay hazard); (2) **`logKey` unique per ATTEMPT**
> (global monotonic counter bumped inside fn), NOT per `(actor,opNum)` — else a 1021-retry reuses the
> logKey and overwrites the maybe-applied original's entry while its `Add` already landed → spurious
> `ops>log` flake; (3) `sum(log)==sum(ops)` is the primary oracle; `sum(ops) ≥ lbsum` (lbsum = Σ
> definitely-committed vals) + `lbsum>0` + `injected>0` are the bound/anti-vacuity (no `ubsum` — Go's
> `db.Transact` hides the 1021 retries, but the exact server-vs-server equality is the real oracle);
> (4) `le64`/`fromLE64` self-consistent, sum raw uint64; AddValue-only scope; report injected/committed.
gap: "distributed idempotency-under-retry stress"). Builds on RFC-119/120/122/123 (the SimTransport
fault harness + the commit-drop intercept). **Test-only — zero production/wire impact** →
differential-vs-libfdb_c gates N/A.
**Spec:** `fdbserver/workloads/AtomicOps.actor.cpp` @ 7.3.75.

---

## 1. Problem

RFC-119 §7's coverage audit put AtomicOps's gap at "distributed idempotency-under-retry stress." The
existing `TestConcurrentAtomicAdd` sums N×M concurrent `Add`s against one key — but it has **no
companion-write cross-check** (it only verifies the final sum equals N·M·δ, which a *non*-atomic
read-modify-write could also satisfy under low contention) and **no fault stress**. FDB's AtomicOps
workload is a stronger oracle: it commits, in **one transaction**, an atomic op on an `ops` key **and**
a `set` of a unique `log` key to the same value, then checks `accumulate(log) == accumulate(ops)` per
group (`AtomicOps.actor.cpp:393,414`). Because the log-set and the atomic-op are in the same commit,
they are atomic together (both-or-neither) — so the sums match iff **every committed atomic op landed
exactly with its log entry, and none was lost or torn**, even across concurrent commits and retries.

## 2. The C++ design (cited, `AtomicOps.actor.cpp` @ 7.3.75)

**Worker** (`atomicOpWorker`, `:186-223`). Each attempt freshly randomizes `group ∈ [0,100)`,
`intValue`, `opsKey = ops{group}{nodeIndex}`, and a **unique** `logKey = log{group}{clientId}{opNum}`
(`logDebugKey`, `:135-140`). In one txn: `set(logKey, val)` + `set(debugKey, opsKey)` +
`atomicOp(opsKey, val, opType)` + `commit()` (`:199-202`). On **success**, `lbsum += intValue` and
`ubsum += intValue`; on **`commit_unknown_result (1021)`**, only `ubsum += intValue` (`:207-219`); then
`onError(e)` retries — **re-randomizing the next attempt** (the `state` decls at `:192-197` are inside
the retry loop). So a retry is a **fresh, different op**, never a re-application of the same op.

**Check** (`_check`, `:359-435`). Per group, accumulate the `log` keyspace and the `ops` keyspace under
the same op (`atomicOp` into `xlogResult`/`xopsResult`) and assert `xlogResult == xopsResult`
(`:393`); for `AddValue`, additionally assert `sum(log) == sum(ops)` (`:414`). The `lbsum`/`ubsum` are
logged on mismatch as a diagnostic bound (`:420-421`), not the primary assertion.

**Key property — why exact equality holds even under fault.** `set(logKey,val)` and
`atomicOp(opsKey,val)` are in **one** transaction, so each *committed* op contributes `val` to **both**
sums or **neither**. `commit_unknown_result` means the client doesn't *know* the outcome — but the
outcome is definite on the server (applied or not), and *both* the log entry and the atomic op reflect
that same definite outcome. The retry is a *fresh* op (new unique `logKey`), so it never
double-counts the original. Hence `sum(log) == sum(ops)` exactly, regardless of faults/retries. The
`lbsum`/`ubsum` bound the *client's* knowledge of its own contributions (`lbsum ≤ sum ≤ ubsum`), not
the log/ops relationship.

**Confirmed empirically (design probe, now removed).** A throwaway probe applied the *same* `Add(k,1)`
twice by retrying a fixed-key op under a dropped commit reply → `k=2` (double-apply). That is faithful
FDB behavior: with **no idempotency ID** (Go doesn't implement automatic idempotency IDs — a separate
known gap), `commit_unknown_result` + retry of the *same* op re-applies it. This is precisely why the
workload uses a **fresh unique `logKey` (and fresh op) per attempt** — so the invariant is "each
committed op's log+atomicOp are atomic," NOT "retrying an op is idempotent." The port MUST re-randomize
per attempt (the op generated *inside* the `db.Transact` fn, so each retry is fresh), or it would
spuriously double-count the same op (ops > log) and the exact-equality oracle would falsely fail.

## 3. Proposed Go change (test-only)

New `pkg/fdbgo/client/atomicops_workload_test.go`:

- **`atomicOpsWorkload`** — `groups`, key helpers `opsKey(group,node)`, `logKey(actor,opNum)` (unique
  per attempt), `le64`/`fromLE64`. `runOnce(ctx, db, actor, opNum)`: `db.Transact(fn)` where **fn**
  freshly picks `group`/`node`/`val` and does `tx.Set(logKey, le64(val))` + `tx.Atomic(MutAddValue,
  opsKey, le64(val))`. The op is generated INSIDE fn, so each retry is a fresh op (faithful re-random).
  Returns the committed `val` (for the client-side `lbsum`).
- **`check(ctx, db)`** — read each group's `log` and `ops` ranges; assert `sum(log) == sum(ops)` per
  group (the AddValue oracle), and the global `sum(ops) == sum(log)`.
- **`TestAtomicOps_LogConsistentUnderConcurrency`** (healthy): A actors × M ops, then `sum(log) ==
  sum(ops)`. The companion-log cross-check is the teeth the existing `TestConcurrentAtomicAdd` lacks.
- **`TestAtomicOps_LogConsistentUnderDroppedCommit`** (the gap): same workload under the RFC-123
  `everyNthCommitReplyDrop` fault. `sum(log) == sum(ops)` must STILL hold — proving the log+atomicOp
  commit atomically (both-or-neither) even when the commit outcome is ambiguous (`commit_unknown_result`
  → `commitDummyTransaction` + retry with a *fresh* op). This is the "idempotency-under-retry stress"
  oracle: a torn commit (log without op, or op without log) or a lost op → mismatch. Reuses
  `everyNthCommitReplyDrop` (no new intercept).
- **`lbsum ≤ sum(ops) ≤ ubsum`** client-bound assertion (anti-vacuity that the workload's own tracking
  is consistent) is a secondary check; `sum(log)==sum(ops)` is the primary oracle.

### 3.1 Flake-freedom

Same structure as RFC-123: bounded window, the `default: t.Errorf` arm fires only on a non-context
error (no per-tx timeout), commit-drop recovery is retryable (1021), `committed > 0` / `injected > 0`
are counter-identity assertions. The exact-equality check runs **disarmed** (after the fault window)
at a single read version. n/window pinned to observed headroom (reported in the PR).

## 4. Wire-compat impact

**None.** Test-only. No production code change.

## 5. Follow-ups

- ConflictRange (concurrent read/write race-detection) and FuzzApi (property-based multi-txn) gaps,
  each its own increment.
- Automatic idempotency IDs (the same-op re-apply the probe surfaced) is a separate, large known-gap
  feature — NOT this increment; this workload is faithful to the no-idempotency-ID behavior by design.
