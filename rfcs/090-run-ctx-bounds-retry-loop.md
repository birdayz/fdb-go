# RFC-090 — `Run(ctx)` doesn't bound the retry loop / backoff (delayed-cancellation, uncancellable hot-conflict)

Part of TODO-production.md **P0.4**. Reviewed-by: **FDB C++** (ACK-with-conditions, folded
in below), **Torvalds** (code quality, pending).

## Problem

`FDBDatabase.Run(ctx, fn)` (`pkg/recordlayer/database.go:145`) captures `ctx` into
`recordCtx.ctx`, so cursor ops inside `fn` honor cancellation (RFC-030). But the **retry
loop and backoff** run on `context.Background()`:

- The facade `fdb.Database.Transact(f)` (`pkg/fdbgo/fdb/database.go:215`) calls
  `db.d.inner.Transact(db.d.ctx, …)` with `db.d.ctx = context.Background()` (`:217`) and sets
  `t.ctx = db.d.ctx` (`:222`). The caller's `ctx` is dropped.
- `client.Database.Transact(ctx, f)` (`pkg/fdbgo/client/database.go:512-536`) IS ctx-aware —
  top-of-loop `ctx.Err()` (`:515`), `OnError(ctx,…)`→`backoffSleep(ctx,…)`
  (`transaction.go:1211,1264`), `tx.Commit(ctx)` (`:527`). The only defect is the facade
  substituting Background.

Consequences:
- Cancellation is observed only when `fn` next runs a `recordCtx.ctx`-aware op. A `fn` that is
  write-only / compute-then-commit and hits a persistent retryable error (hot 1020) is
  **genuinely unbounded** — the loop check (Background) never fires and backoff (Background)
  never wakes. (When `fn` does touch a ctx-aware op between retries, cancellation is merely
  *delayed* by one backoff interval — still wrong for a control plane.)
- Default retry limit is unlimited (libfdb_c parity, `RETRY_LIMIT=-1`) — fine *only if `ctx`
  bounds the loop*, which today it does not.
- Three divergent retry paths: `client.Database.Transact` (ctx-aware), the facade (Background,
  used by `Run`), and `FDBDatabaseRunner.RunWithRetry` (`runner.go:94`, bounded + ctx-checked,
  unused by `Run`).

## Constraint

- C++ is the spec: keep unlimited-retry default; the caller's `ctx` is the Go-idiomatic bound.
- `fdb.Database.Transact(f func(Transaction)(any,error))` is Apple-binding-compatible — must
  not change/break.
- `fdb.Transactor` is implemented by `Database`, `Tenant`, `Transaction`,
  `chaos.ChaosTransactor`, and a test spy.

## Commit cancellation — RESOLVED (FDB C++ review): do NOT cancel a dispatched commit

The first draft proposed `t.ctx = ctx` everywhere, including the commit. The C++ review showed
that is **wrong and a regression**:

- A ctx-cancel mid-commit makes `commit()` map the wait error → `commit_unknown_result` and
  call `commitDummyTransaction(ctx)` — which **immediately returns at `commitpath.go:141`
  (`if ctx.Err()!=nil {return}`)**, so the maybe-committed idempotency barrier becomes a
  **no-op**. Today (`t.ctx=Background`) the barrier always runs; Option A silently breaks it.
- libfdb_c semantics: a `transaction_timeout`/cancel mid-commit surfaces as **1031/1025**
  (not 1021) and runs **no** dummy barrier; the dummy barrier exists only for a genuine
  `commit_unknown_result` from a degraded commit RPC. So "ctx-cancel is handled idempotently"
  was based on C++ behavior that does not exist.
- A dispatched commit is **already bounded** independent of `ctx` by `DefaultRPCTimeout = 5s`
  (`transaction.go:52`, applied in `commitpath.go:71`). Not cancelling it on `ctx` costs at
  most ≤5s past the deadline for one in-flight commit to resolve to a *known* outcome.

**Decision (Option B):** `ctx` bounds the retry-loop check, `OnError` backoff, and reads/GRV.
The dispatched commit RPC **and** `commitDummyTransaction` run on `context.WithoutCancel(ctx)`
(Go 1.26) so a dispatched commit always resolves to a known outcome with the barrier intact.
This matches libfdb_c's invariant ("a dispatched commit resolves; the barrier protects the
retry").

## Fix

- **Optional capability interfaces** (not a `Transactor` widening — avoids a meaningless
  `Transaction.TransactCtx` and an interface break):
  ```go
  type CtxTransactor interface {
      TransactCtx(ctx context.Context, f func(Transaction)(any,error)) (any,error)
  }
  type CtxReadTransactor interface {
      ReadTransactCtx(ctx context.Context, f func(ReadTransaction)(any,error)) (any,error)
  }
  ```
  `recordlayer.Run`/`RunWithVersionstamp`/`RunWithWeakReads` type-assert
  `d.transactor.(fdb.CtxTransactor)` and use `TransactCtx(ctx,…)`, falling back to
  `Transact(…)` otherwise. `RunRead` does the same with `CtxReadTransactor`.
- `fdb.Database.TransactCtx(ctx,f)`: like `Transact`, but threads `ctx` to
  `db.d.inner.Transact(ctx,…)` and sets `t.ctx = ctx` (reads honor ctx).
  `Database.Transact(f)` = `TransactCtx(db.d.ctx, f)` (Apple-compat, Background).
  Same for `Tenant`. `ReadTransactCtx` mirrors for the read path (fixes `RunRead`, whose
  `ReadTransact` loop is currently Background-bound except the entry `ctx.Err()` check at
  `database.go:191`).
- `chaos.ChaosTransactor`: implement `TransactCtx`/`ReadTransactCtx` — forward to
  `inner.(CtxTransactor)` if present, else fall back to `inner.Transact` (it wraps an
  arbitrary `fdb.Transactor`). `Transaction` and the test spy need nothing (capability
  absent → fallback).
- **Commit detachment** (the core safety change), in `client.Database.Transact`
  (`database.go:527`): commit on `context.WithoutCancel(ctx)` so the commit RPC + barrier are
  not cancelled by the caller's ctx. Verify `commit()` threads that detached ctx to
  `commitDummyTransaction`. (`commitDummyTransaction` then loops bounded only by
  `DefaultRPCTimeout` per attempt — matches libfdb_c's barrier; deliberate.)

## Verification

- Unit (no FDB): already-cancelled `ctx` → `TransactCtx` returns promptly, zero retries; a
  spy transactor forcing repeated retryable errors under a deadline exits at the deadline.
- **FDB integration (required — the crux):** (a) cancel `ctx` mid-commit (fault-inject a
  withheld/slow commit reply); assert the dummy barrier RUNS (not skipped) and the outcome is
  deterministic; (b) a write that committed just before cancel returns success (caller lost
  the race), not a spurious error; (c) a hot-1020 loop under a `ctx` deadline exits at the
  deadline, not forever; (d) reads under a cancelled ctx return canceled promptly (not after
  the 5s per-RPC timeout).
- Apple-compat: `Transact(f)`/`ReadTransact(f)` unchanged (Background path); chaos/spy
  transactors compile + pass. Determinism + full `just test` green; stress-1M within noise.

## Out of scope

- Default `transaction_timeout` (behavior-change risk — could abort legitimate long
  transactions; separate P0.4 follow-up).
- Wire/format: none (read-path + client retry only).
