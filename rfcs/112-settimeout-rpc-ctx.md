# RFC-112: Thread `SetTimeout` into RPC wait contexts (port C++ `timebomb`)

**Status:** Accepted (v2 — folds the FDB-C++ + Torvalds impl-review NAK: GRV bounding,
GetAddressesForKey/GetLocations bounding, dropped the redundant loop `checkTimeout`)
**Item:** `rfcs/prod-readiness-go-client.md` punch-list **P0.2** (one of the "top 3 to close").
**Spec:** FoundationDB C++ `libfdb_c` 7.3.75 (`/tmp/fdbsrc`). **Wire-compat impact:** none — pure
client-side cancellation semantics + an error code (1031). No bytes change.

## Problem

`SetTimeout` sets `tx.timeout`/`tx.deadline` (= `creationTime + timeout`, C++-faithful) and
`checkTimeout` (`transaction.go:1829`) is run at op-entry via `ensureReadVersion`
(`transaction.go:552`) and `Commit` (`:1228`). But **once a read RPC is in flight the deadline is
never enforced**: the reply-timeout retry loop (`readpath.go` `getValue`/`getKey`/`getRange`) re-sends a
slow read up to `maxReadTimeoutRetries` (10) × `readRPCTimeout` (5s `DefaultRPCTimeout`) **without
re-checking `checkTimeout` and without threading the deadline into the RPC wait**. So a single
hung-but-alive read can run ~50s **regardless of `SetTimeout`** — a 10s/30s timeout is exceeded. Only a
caller `ctx` deadline bounds it today (it IS threaded into every `waitReply`).

## C++ spec — `timebomb`

`ReadYourWritesTransaction::resetTimeout` (`ReadYourWrites.actor.cpp:1576`) arms
`timebomb(creationTime + timeoutInSeconds, resetPromise)` (`:1567`). `timebomb` waits to the deadline
then `resetPromise.sendError(transaction_timed_out())` (error **1031**, `error_definitions.h:58`).
Every read/commit does `wait(resetPromise.getFuture() || <op>)` (e.g. `:1360`, `:1405`), so when the
bomb fires the in-flight op is cancelled **asynchronously** and throws 1031. The deadline is measured
from creation/reset, shared across retries — exactly Go's `tx.deadline`.

## Proposed Go change

Go uses `context` for cancellation; the faithful analog is to bound every read RPC wait by
`tx.deadline` and surface 1031 when our deadline (not the caller's ctx) fires.

1. **`opContext(ctx)`** — when `tx.timeout > 0`, return `context.WithDeadline(ctx, tx.deadline)`;
   else `ctx` unchanged. This is **the fix**: threaded into every read RPC wait so a hung wait
   (`waitReply`, which already honors `ctx.Done()`, `rpc.go:64-66`) is cancelled at the deadline — the
   in-flight read is aborted, not left to run the 10×5s loop. The Go analog of the `resetPromise || op`
   race.
2. **`mapTimeout(parentCtx, err)`** — convert a deadline/cancel error caused by *our* timeout (the
   `parentCtx` is still live) into `transaction_timed_out` (1031); if `parentCtx` is itself done, it's
   the caller's cancellation — preserve it. Matches C++ raising 1031, not a generic cancel.

`opContext`+`mapTimeout` are threaded at **every read RPC entry**:
- `getValue`/`getKey`/`getRange` become thin wrappers (`opContext` → `*Impl` → `mapTimeout`).
- **`ensureReadVersion`** bounds the GRV — the *first* read RPC every transaction issues — matching
  C++ `RYWImpl::getReadVersion`'s `choose { getReadVersion() | resetPromise }`
  (`ReadYourWrites.actor.cpp:1537`). A hung-but-alive GRV proxy must not run past the timeout.
- **`GetAddressesForKey`/`GetLocations`** (C++ bounds `getAddressesForKey` by the timebomb too,
  `:1843-1848`).

`transaction_timed_out` (1031) is already correctly **non-retryable** (`onErrorRetryable`, matching
C++ `fdb_error_predicate`), so a timed-out read/GRV aborts the whole `Transact` rather than looping.
The reply-timeout retry loops are left unchanged — `opContext` cancels the in-flight wait, so a
separate loop `checkTimeout` would be redundant (and untested) belt-and-suspenders.

**Scope note: the commit RPC** is deliberately `ctx`-detached for `commit_unknown_result` idempotency
(RFC-093) and is bounded by `checkTimeout` at `Commit` entry; threading the bomb into the detached
commit RPC would reintroduce the idempotency hazard, so it stays as-is (a documented divergence — C++'s
timebomb does bound the in-flight commit, but Go lacks FDB's idempotent commit-retry actor).

## Executable spec (tests)

- **Deterministic (`dropReplyConn`)**: a read whose reply is dropped, with `SetTimeout(300ms)`, returns
  `transaction_timed_out` (1031) in ≈300ms — **not** ~50s. Revert-proof: without `opContext`+loop-check
  it runs to the `maxReadTimeoutRetries`×`readRPCTimeout` bound and returns `transaction_too_old`.
  Deterministic by construction (the reply is dropped, so the deadline always wins — no timer race,
  the RFC-288 lesson).
- **Caller-ctx precedence (unit)**: when the caller's `ctx` deadline is earlier and fires, the error is
  the caller's `ctx.Err()`, not 1031 (we only synthesize 1031 for our own timeout).
- **No-timeout unchanged (unit)**: `tx.timeout == 0` → `opContext` returns ctx unchanged; existing
  reply-timeout behavior (`transaction_too_old` on exhaustion) is preserved.
- **1031 non-retryable**: a timed-out read is not retried by `Transact`/`OnError`.
