# RFC-093 — Thread the live ctx to the commit-path GRV for write txns

**Status:** Draft
**Item:** TODO-production.md "Client robustness" — *Thread live ctx to the commit-path GRV
for write txns (Commit-internal)* (codex + FDB C++).
**Domain:** pure-Go FDB client (`pkg/fdbgo`). Reviewer gate: **FDB C++ client developer +
Torvalds** (skill `fdb-client-review`). Spec: C++ source 7.3.75 at `/tmp/fdbsrc`.

## Problem

The low-level retry loop is `(*Database).Transact` (`client/database.go:512`); the fdb-layer
`FDBDatabase.Run` reaches it via `runTransactCtx` → `client.Database.Transact`. That loop
dispatches the commit under `tx.Commit(context.WithoutCancel(ctx))` (`client/database.go:560`).
RFC-090 introduced that `WithoutCancel` to protect the **commit RPC + its
`commit_unknown_result` idempotency barrier** from a late caller cancel tearing a commit in
half (ambiguous-write hazard).

But `Commit` does more than the commit RPC. For a write txn with no prior read version it
runs an internal **GetReadVersion** first — `ensureReadVersion(ctx)` at `transaction.go:1106`
(C++ `tryCommit` calls `startTransaction(CAUSAL_READ_RISKY)` to guarantee a read version
before commit). Because the *whole* `Commit` is invoked with a stripped ctx, that GRV is
**uncancellable**: a caller cancel arriving while the commit-path GRV is in flight is
ignored, and the GRV blocks until it resolves (or its own RPC timeout). The pre-commit
`ctx.Err()` check in the `Transact` loop (`database.go:534`) catches a cancel *before*
`Commit`, not one *during* the GRV. So a `db.Transact(ctx, …)` writer cannot abort a commit
that is stuck waiting for a read
version — exactly the availability footgun P0.4 set out to remove.

This is a **sub-RPC window and non-hazardous** (a stale-but-durable commit — no data
hazard; FDB C++ review). It is an availability/correctness-of-cancellation gap, not a wire
divergence: no bytes change.

A prior attempt fixed it in the `Transact` loop (prefetch the GRV separately under the live
ctx) and **regressed the read-only/no-op fast path** by forcing a GRV the C++ skips (codex
P2 — reverted; the lesson is recorded in the `database.go:538-549` NOTE). The correct home is
*inside* `Commit`, after the fast-path return.

## Investigation

`Commit` (`transaction.go`) has exactly two ctx-uses after its read-only fast-path return
(`:1094`, returns when `len(muts)==0 && nWriteConflicts==0`, before any GRV):

1. `tx.ensureReadVersion(ctx)` — `:1106` — the commit-path GRV (a *read*; correct to bound
   by the caller ctx/deadline).
2. `tx.commit(ctx, muts)` — `:1115` — `commitpath.go:28`, the "commit RPC + barrier" that
   owns `commitDummyTransaction` (the `commit_unknown_result` re-confirm). RFC-090 requires
   this to be detached.

Today both inherit the `WithoutCancel` applied at the call site. The fix detaches **only**
#2, leaving #1 live.

C++ correspondence (`/tmp/fdbsrc`, 7.3.75): in `fdbclient/NativeAPI.actor.cpp` the GRV
(`getReadVersion`) is an ordinary cancellable read future bounded by the transaction's
deadline; only the commit RPC + the `commitDummyTransaction` re-confirmation
(`NativeAPI.actor.cpp:~4225`) get the special "don't abandon a maybe-applied commit"
treatment. Go's split must mirror that boundary: **GRV cancellable, commit barrier not.**
The FDB C++ reviewer should confirm the exact GRV cancellability and barrier line.

## Fix

Two lines.

1. `client/database.go:560`
   `tx.Commit(context.WithoutCancel(ctx))` → `tx.Commit(ctx)`
   Pass the live ctx into `Commit`.

2. `client/transaction.go:1115`
   `tx.commit(ctx, muts)` → `tx.commit(context.WithoutCancel(ctx), muts)`
   Re-apply `WithoutCancel` to *only* the commit RPC + barrier.

Net: `ensureReadVersion(ctx)` (`:1106`) now runs under the live caller ctx; the commit RPC
and `commit_unknown_result` barrier (`:1115` → `commitpath.go`) stay detached, preserving
RFC-090 idempotency.

### Comment changes (each comment must stay true after the move — Torvalds)

Three comment sites describe the *old* "GRV is also detached" behavior and must change in the
**same diff**, or they ship a lie:

1. **`database.go:527-536`** (the `if ctx.Err()` block *before* `Commit`) currently says the
   `WithoutCancel` "**also detaches the commit path's pre-commit GRV (ensureReadVersion) — so
   once we call Commit, a new read version can be fetched and a commit issued even on an
   already-expired ctx**." After the fix this is **false** — the GRV honors the live ctx.
   Rewrite it: this early `ctx.Err()` check is now a fast-abort (skip creating the commit at
   all on an already-dead ctx); the commit-path GRV inside `Commit` is *also* ctx-bounded, and
   only the commit RPC + barrier are detached.
2. **`database.go:538-549`** (the NOTE: "a tighter refinement … was tried and reverted … The
   correct home is inside Commit … **Tracked as a follow-up**"). This RFC **is** that
   follow-up — drop the "tracked as a follow-up / residual window" framing. Keep the codex-P2
   lesson (why the fix lives *inside* `Commit`, not as a prefetch in this loop) as the reason
   the call site now just passes the live ctx.
3. **`database.go:551-559`** (the RFC-090 `WithoutCancel` rationale) **moves to**
   `transaction.go:1115`, the new `WithoutCancel` site — stating that *only* the commit RPC +
   `commit_unknown_result` barrier are detached (so the barrier can't no-op on a cancelled
   ctx, `commitpath.go`'s `if ctx.Err()!=nil {return}`), while the preceding GRV at `:1106` is
   deliberately live. (FDB C++ nit + Torvalds: the asymmetry must be explained at `:1115` or
   the next reader mistakes it for a bug.) The call site at `database.go:560` keeps a one-line
   pointer ("commit RPC + barrier are detached inside Commit — see transaction.go:1115").

## Safety (every path)

- **No-op / read-only txn:** returns at `transaction.go:1100` *before* `ensureReadVersion` —
  never issues a GRV. **No forced-GRV regression** (the exact bug the reverted P2 attempt
  caused). The read-only fast path is untouched.
- **Cancel during the commit-path GRV:** `ensureReadVersion(ctx)` returns `ctx.Err()` (a
  non-`*wire.FDBError`). `Commit` returns it; `Transact` calls `tx.OnError(ctx, err)`, which
  at `transaction.go:1243-1245` takes the non-FDBError branch → `txStateErrored` →
  **non-retryable**, returns the error. No infinite loop, prompt abort.
- **Cancel during commit RPC / barrier:** unaffected — `tx.commit` runs under
  `WithoutCancel`, so the in-flight commit and `commitDummyTransaction` re-confirm complete
  (bounded by their per-RPC timeout). RFC-090 idempotency intact.
- **Retryable error surfaced by the GRV** (e.g. 1007/1020): `OnError`'s `backoffSleep(ctx)`
  (`:1270/1283/…`) honors the cancel — a cancel during backoff returns `ctx.Err()`, no spin.
- **`context.Background()` callers** (no deadline/cancel to strip): `WithoutCancel` is
  observably inert, behavior unchanged.
- **Deadline (not just cancel):** the caller's deadline now *also* bounds the commit-path
  GRV — correct (a GRV is a read; bounding a read by the txn deadline matches C++). The
  commit RPC itself stays deadline-free under `WithoutCancel`.

## Performance

Zero overhead — no new allocation, no new RPC, no changed hot path. `WithoutCancel` moves
from one call site to another; `ensureReadVersion` already took a ctx. No plan/cost impact
(client-only).

## Test plan (no fake checkboxes — e2e + revert-proof)

Project rule: a real end-to-end test that fails pre-fix and passes post-fix.

**New fault infra** (extend `client/fault_test.go`): a frame-level **GRV-reply-blocking**
dialer, modeled on `wrongShardConn`/`wrongShardDialer` (`fault_test.go:229-331`). Instead of
*replacing* the next non-PING response body, the armed proxy **holds** the next response
frame on a `releaseCh` (blocking `WriteFrame` until the test signals), then forwards it. This
makes the in-flight GRV reply stall deterministically. **Cleanup must `close(releaseCh)` (via
`t.Cleanup`) to unwedge a still-held `proxyLoop`** — a held `WriteFrame` otherwise leaks the
proxy goroutine + the conn (Torvalds). `close` is safe even after a normal release (use a
`sync.Once` or a separate done flag so a double-signal can't panic).

**`TestFDB_CommitPathGRV_HonorsCtxCancel`:**
1. Start FDB through the blocking dialer; create a txn; issue a `Set` (write) so `Commit`
   skips the fast path and *must* GRV. Do **not** pre-fetch a read version (the commit path
   must issue the GRV during the fault window — opposite of the existing wrong-shard tests
   which pre-fetch to quiesce).
2. Run `db.Transact(ctx, …)` in a goroutine; arm the dialer so the GRV reply is held; once
   the GRV request is in flight, `cancel()` the ctx.
3. **Assert (post-fix):** `Transact` returns `context.Canceled` promptly (bounded, not on the
   `releaseCh`); the written key is **absent** (no commit happened).
4. **Revert-proof:** with the two-line change backed out, the same test hangs on the GRV
   until `releaseCh` is released (then commits) — i.e. the cancel is ignored. Capture the
   red locally; commit the test green with the fix. (Belt-and-suspenders: a bounded
   `select`/timeout in the test asserts "returned within Xs" so the pre-fix hang is a clean
   FAIL, not a test-timeout.)

**Regression for the fast path** (guard against re-introducing the reverted P2 bug):
`TestFDB_CommitReadOnlyNoForcedGRV` — a read-only/no-op `Commit` issues **no** GRV during
the fault window (arm the blocker, commit a no-mutation txn, assert it returns immediately,
never touching `releaseCh`).

**Gates:** `just test` green; `-race` on `//pkg/fdbgo/client`; loop the new test 10×
(`--nocache_test_results`) for timing determinism.

## Reviewer asks

- **FDB C++ dev:** confirm against `/tmp/fdbsrc` (7.3.75) that (a) the commit-path GRV is a
  cancellable read bounded by the txn deadline, and (b) only the commit RPC +
  `commitDummyTransaction` re-confirm get the no-abandon treatment — i.e. the
  GRV-live / commit-detached boundary is exactly where C++ draws it.
- **Torvalds:** the comment relocation is honest (no lie about what's detached); the test is
  a real e2e with a revert-proof (not a plan-shape fake); the new blocking-dialer doesn't
  leak goroutines/conns (`t.Cleanup`).
