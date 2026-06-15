# RFC-110: Network/background-goroutine panic backstop — match `Net2::run`, not "add recover"

**Status:** Draft
**Item:** Production-readiness audit (`docs/review_2026-06-07.md`) Blocker #2 residual — the
crash-safety hardening (TODO-production P0.2) recovered only "the 3 network goroutines"
(the `transport/conn.go` read/write/monitor loops) and left the pure-Go client's other
long-lived/background goroutines as uncaught process-abort surfaces.
**Spec:** FoundationDB C++ `libfdb_c` at tag **7.3.75** (`/tmp/fdbsrc`, the `foundationdb`
pin in `MODULE.bazel`). C++ is the spec; every claim below is cited `file:line`.

---

## Problem

In Go, **an unrecovered panic in *any* goroutine aborts the whole process** — uncatchable by
any caller. For a shared/multi-tenant control plane, one latent client-side bug taking down the
host is a real availability hazard. The 2026-06-07 audit flagged this as Blocker #2 ("a single
malformed frame on a long-lived network goroutine takes down the process").

Two corrections, both load-bearing, from reading the source:

1. **The audit's stated trigger — a malformed network reply — is FALSE for the pure-Go client.**
   Every reply decoder bounds-checks and returns errors (`wire/reader.go` scalar/var-length
   readers guard `off+N > len` and return zero/nil; `ReadVectorCount` caps the count,
   `reader.go:429-438`); the transport validates frame length (`16 ≤ n ≤ 100 MiB`) and an
   XXH3-64 checksum and **drops the connection on a bad frame** (`transport/framing.go:101-129`),
   then `failConnection` reconnects (`conn.go:697-718`). A hostile/corrupt/truncated reply
   becomes a returned error or a dropped connection — it never reaches a `panic`. Both
   `refreshTopology` (`topology.go:54-60`) and `backgroundRefresher` (`grv.go:460-472`) already
   swallow *errors* non-fatally. So there is **zero network-input-reachable panic** on these paths.

2. **The real gap is that Go currently aborts the process where C++ deliberately survives.**
   The only reachable panic on the topology/GRV paths is an *encode-side invariant*
   (`wire/writer.go:73` "vtable not in template closure") — a latent self-bug, practically never
   fires. But *if* it (or any future panic — a nil-deref, an index bug) ever fires on one of
   these goroutines, Go kills the host. The C++ client, on the identical class of failure, **logs
   loudly and keeps running** (see below). That divergence — Go-aborts-where-C++-survives — is the
   defect, and it is broader than the two goroutines the audit named.

This RFC settles the question the audit's "add a `defer recover()`" hand-wave skipped: **does the
C++ client recover here, and if so, with exactly what semantics?** The answer is yes, and the
semantics matter — a recover that silently swallows or that kills the goroutine would *not* match
C++ and *would* be papering-over.

---

## The spec: what the C++ client does on the network thread

### It recovers ordinary failures — `Net2::run`'s catch-all backstop

`flow/Net2.actor.cpp:1603-1611` — every task the network thread executes is wrapped:

```cpp
try {
    ++tasksSinceReact;
    (*task)();
} catch (Error& e) {
    TraceEvent(SevError, "TaskError").error(e);
} catch (...) {
    TraceEvent(SevError, "TaskError").error(unknown_error());
}
```

The network thread catches **any** exception escaping a task, logs `SevError "TaskError"`, and
keeps draining tasks. This is the recover-equivalent. `ASSERT(c)` *throws* `internal_error`
(`flow/include/flow/Error.h:126-131`), it does not abort; and `g_crashOnError` is **`false`** in
the client (`flow/Error.cpp:28`; flipped `true` only by `fdbserver --crash` /
`fdbbackup`, `fdbserver.actor.cpp:1583`), so the `Error` ctor's `crashAndDie()` gate
(`Error.cpp:124`) is never taken. A blown invariant on the client network thread is therefore
**log-loud-and-continue**, not crash.

### It crashes in exactly four narrow classes

The C++ client intentionally terminates the process **only** here (full audit in §"Verification"):

1. **OOM through FDB's own allocators** / the `--memory` resident-set monitor →
   `platform::outOfMemory()` → `criticalError(FDB_EXIT_NO_MEM, …)` → `flushAndExit`
   (`flow/Platform.actor.cpp:3190,3288,3381-3428`; `flow/SystemMonitor.cpp:545`).
2. **Low-level VM/host-API failure** the process can't survive — guard-page `mmap`/`mprotect`
   failure (`std::abort`, `Platform.actor.cpp:2125,2135`), `clock_gettime(CLOCK_MONOTONIC)`
   failure at init (`criticalError`, `:3665-3669`).
3. **A throw escaping a C-API "infallible" op** — the `CATCH_AND_DIE` set
   (`bindings/c/fdb_c.cpp:137-146`): `fdb_transaction_set/atomic_op/clear/clear_range/
   set_read_version/cancel/reset`, `fdb_future_cancel/destroy/release_memory`, the destroy paths.
4. **A violated invariant or overflow in a destructor** where throwing is unsafe —
   `ASSERT_ABORT` (`Error.h:132-138`) in `~DatabaseContext` (`NativeAPI.actor.cpp:1932`),
   `~Thread`; and `~BaseTraceEvent` calls ungated `crashAndDie()` (= `std::abort`,
   `Platform.h:684`) on `TracedTooManyLines` overflow (`Trace.cpp:1374`) — a client-reachable
   destructor abort that does **not** route through the `Net2::run` catch. Go has no
   trace-line-overflow analog, so it is moot for the port, but the class exists.

*Examined and excluded* (so the next reader doesn't re-flag them): the `FastAlloc.cpp:483-516`
`abort()`s are `#if FAST_ALLOCATOR_DEBUG`-only; `FlowTransport.actor.cpp:1220` `flushAndExit` is
guarded `if (!FlowTransport::isClient())` (server-only, rethrows on the client); the
`flow/flow.cpp` and simulation `ASSERT_ABORT(g_network->isSimulated())` are simulation-only.

Notably **not** crashes (all log + close/retry, never fatal): transport checksum/length
corruption (`FlowTransport.actor.cpp:1291-1296,1338-1350` → `connectionKeeper` close+reconnect,
only `actor_cancelled` rethrows, `:914,1004,1021-1023`); **incompatible protocol version**
(SevWarn + close, `:1507,1529-1538`); trace-file write/open failure (degrade + retry); raw `new`
`std::bad_alloc` (no custom `set_new_handler`/`set_terminate` → propagates into the catch-all).

### The monitor self-heal semantics (the loops we're porting)

- **`monitorProxies` / `monitorProxiesOneGeneration`** (`MonitorLeader.actor.cpp:840-1005`) — the
  production topology monitor, the analog of Go `topologyMonitor`. Uses `tryGetReply` (returns
  `ErrorOr`, never throws): a failed coordinator round logs `SevInfo "MonitorProxiesConnectFailed"`
  (`:972-974`), advances round-robin to the next coordinator with **no** delay, and sleeps
  `COORDINATOR_RECONNECTION_DELAY = 1.0s` (`ClientKnobs.cpp:52`) **only after a full failed sweep**
  (`:975-979`), then loops forever. Never crashes, never exits; only ctx-cancel
  (`actor_cancelled`) and a conn-string forward unwind it.
- **`backgroundGrvUpdater`** (`NativeAPI.actor.cpp:1283-1331`) — the analog of Go
  `backgroundRefresher`. Two-level try/catch: the inner catch (`:1305-1319`) catches *every*
  error, logs `SevInfo "BackgroundGrvUpdaterTxnError"`, applies `tr.onError(e)` + an exponential
  `Backoff` (0.01→1.0s, ×2, jittered; `DatabaseContext.h:852-865`), and loops; the **stale cached
  read version is left intact** (the cache write is success-only and monotonic, `:363-382,7409`),
  so readers keep using it until it ages past `MAX_VERSION_CACHE_LAG = 0.1s` and transparently
  fall back to a live proxy GRV. The outer catch (`:1327-1330`) only rethrows on cancellation
  (DB teardown). Never crashes on a fetch error.

---

## The alignment insight

`recover()` and `Net2::run`'s `catch` cover **the same class**, and the things each lets through
are **the same class**:

| C++ network thread | Go goroutine | Outcome |
|---|---|---|
| Thrown `Error` / `internal_error` (blown `ASSERT`) | ordinary `panic(...)` | **caught** → log + continue |
| OOM via FDB allocators, VM/host failure | fatal runtime error (true OOM, stack overflow) — **bypasses `recover()`** | **process exits** |
| `CATCH_AND_DIE` infallible C-API op | synchronous facade mutation-buffer write (caller's goroutine) | caller-visible; not a background goroutine |
| `ASSERT_ABORT` in destructor | n/a (no destructors) | — |

So "add `recover()` at every background-goroutine boundary, log loud, take the layer-appropriate
non-fatal action" is **exactly C++ parity**: Go's runtime already hard-exits on the conditions
C++ hard-exits on (Go `recover()` does not catch `runtime.Error` fatal throws like OOM), and a
plain `panic` is precisely the `internal_error`-throw that C++ catches and logs. This is not a
tolerance gate (CLAUDE.md #9) — it is porting `Net2::run`'s documented, deliberate
client-availability policy, with the panic sites left in place and the recovery made **loud and
counted** (more observable than a bare process death in a multi-tenant host).

---

## Proposed Go change

Apply a uniform backstop across every background/long-lived goroutine in `pkg/fdbgo`, with the
action matched to the C++ analog. (Already-correct: the 3 `transport/conn.go` loops recover →
`failConnection`, the FlowTransport `connectionKeeper` close+reconnect analog.)

**Class A — long-lived control loops → recover, treat the panic as the loop's *error path*
(backoff + rate-limited log + counters), *continue the loop* (do not exit):**
- `client/topology.go:18` `topologyMonitor` (launched `database.go:610`)
- `client/grv.go:426` `backgroundRefresher` (launched `grv.go:285`)

  Continuing-the-loop is the `monitorProxies`/`backgroundGrvUpdater` self-heal. But "continue"
  is **not** "re-run immediately": a *deterministic* panic (a real bug, e.g. `writer.go:73`)
  would otherwise re-fire every iteration. The C++-faithful fix is exactly what
  `backgroundGrvUpdater` already does on *any* error: apply an exponential `Backoff` (0.01→1.0s,
  ×2, jittered; `NativeAPI.actor.cpp:1305-1319`, `DatabaseContext.h:852-865`), and `monitorProxies`
  delays `COORDINATOR_RECONNECTION_DELAY=1.0s` after a failed sweep
  (`MonitorLeader.actor.cpp:975-979`, `ClientKnobs.cpp:52`). So **a recovered panic enters the same
  backoff path the loop uses for errors**, capping the re-panic rate at ≤1/s; for `topologyMonitor`
  that means dropping out of rapid-poll to the steady 5s (or the 1s sweep floor). The stale GRV
  cache stays usable across the panic, matching `backgroundGrvUpdater`'s success-only monotonic
  cache write.

  Logging is **rate-limited** (the bare-`slog.Error`-every-interval death Torvalds flagged): log
  the first occurrence immediately with full stack, then at most once per minute carrying a
  suppressed-count — the analog of C++ `TraceEvent` suppression / `monitorNominee`'s
  `.suppressFor(1.0)` (`MonitorLeader.actor.cpp:519-523`). Discoverability comes from the
  counters, not the log volume (next section).

**Class A-batch — the GRV request batcher (`grv.go:309` `time.AfterFunc(b.flush)`) → recover MUST
complete the popped batch with an error *before* returning (then log):** `flush` pops
`batch := b.pending; b.pending = nil` under `b.mu` (`grv.go:317-319`) and only delivers results to
each `req.reply` at the very end (`grv.go:371-373`). A panic anywhere in between (in
`sendGRVRequest`, `applyGRVReply`, or the adaptive-window math) would orphan the popped batch:
every waiter blocked on `select { case <-req.reply; case <-ctx.Done() }` (`grv.go:303-308`) with a
non-canceling ctx **hangs forever** (codex P1). The `batch` slice is local to `flush`, so a
`defer recover()` inside `flush` can — and must — deliver `grvResult{err: <panic-as-error>}` to
**every** `req.reply` in `batch` before returning. This is a one-shot per-batch timer callback, not
a standing loop: the next queued request arms a fresh timer, so there is nothing to "re-arm" and no
tight spin — a deterministic flush panic fails each batch with an error (callers get the error,
never hang) at the rate requests arrive, with the same rate-limited logging.

  **The recover must not leave `b.mu` held (codex P2a — a deadlock hazard).** `flush` takes
  `b.mu` with explicit `Lock()`/`Unlock()` in the adaptive-window math (`grv.go:362-369`); a panic
  *inside* that locked region does not auto-release the mutex, so a top-level `defer recover()`
  that completes the batch and returns would leave `b.mu` permanently locked — and every later GRV
  request blocking on `b.mu.Lock()` (`grv.go:296`) hangs, defeating the whole point. So each
  `b.mu.Lock()` in `flush` must be paired with a **deferred** unlock (a closure-scoped critical
  section) so a panic unwinds the lock before the recover completes the popped batch.

**Class B — one-shot dial/RPC workers → recover, deliver the failure through the existing
result channel / call, never crash:**
- `client/database.go:359` `dialAndPool` → `transport.Dial` (`:360`) runs **before** the normal
  `db.connMu.Lock(); delete(db.dialing, addr)` cleanup (`:362-363`). A panic in `Dial` therefore
  leaves the in-flight `dialCall` in `db.dialing[addr]` with `call.done` **never closed** — every
  later caller in `getOrDialConn` coalesces onto it (`database.go:325-330`) and blocks until *its
  own* ctx expires, and no fresh dial ever starts for that addr: a permanently poisoned
  singleflight entry (codex P2). The recover MUST, under `connMu`: `delete(db.dialing, addr)`, set
  `call.err`, and `close(call.done)` — wake the waiters with an error and let the next caller start
  a clean dial.
- `client/database.go:497` `tryOneCoordinator` (the per-coordinator worker) → put the recover
  **here, on the unit of work**, converting a panic to a returned error — not only on the fan-out
  goroutine. `tryAllCoordinators` calls `tryOneCoordinator` two ways: the parallel fan-out
  (`database.go:479`) **and a direct call on the caller's goroutine when there is a single
  coordinator** (`database.go:466-468`, the common test/dev shape). A recover only on the fan-out
  goroutine would miss the single-coordinator fast path entirely (codex P3). Recovering inside
  `tryOneCoordinator` covers both paths uniformly (emergent fix, CLAUDE.md #10): the fan-out leg
  forwards the returned error via the buffered `ch` (matching a failed `quorum(ok,1)` leg), and the
  single-coordinator direct call returns the error to its caller.

**Class C — facade-future helpers → recover, store the panic as `f.err`, still `close(f.done)`
(fail this op only):**
- `fdb/future.go:83,176,221,266,311`, `fdb/transaction.go:471`

  These run `fn()` (client read/encode) on a detached goroutine; `panicToError`
  (`transaction.go:508`) does **not** cover them — it runs on the *caller's* goroutine after the
  future already resolved. This is the same recover-into-an-error-pointer idiom as
  `recoverErrorPanic` (`libfdbc/backend.go:353`).

**No real surface (trivial `select`s, no user code) — out of scope:** `transport/conn.go:238`,
`conn.go:972`, the `libfdbc` cancel-watcher.

**Where Go still crashes (intentionally, to match C++):** nothing extra to do — Go's runtime
already aborts on true OOM / stack-overflow (these bypass `recover()`), matching C++ classes 1–2;
the synchronous facade mutation ops are caller-visible (class 3 analog); class 4 has no Go
counterpart.

### Mechanics

**Decision: duplicate the ~6-line recover shape per layer; share only the logging sink.** The
three actions are genuinely different (`failConnection` / send `result{err}` to `ch` / store
`f.err`+`close(done)`), so a cross-package helper would need an action callback *and* would force
exporting `transport`'s internals — a half-abstraction that buys nothing (Torvalds' call, and the
right one). What *is* worth sharing is the test-capturable logging sink: lift `seriousLog`
(`conn.go:631`, a `var`-indirected `slog.Default().Error` with a `stack` attr) into a tiny shared
shim (e.g. `pkg/fdbgo/internal/diag`) so `client` and `fdb` log recovered panics through the same
rate-limited, test-observable path. `transport`'s `recoverLoop` (→ `failConnection`) stays as-is.

**Observability — the discoverability mechanism, since the log is rate-limited.** Add to
`client.ClientMetrics` (RFC-097): (a) `recoveredPanics` — a monotonic total, per goroutine label;
(b) `consecutivePanics` — a per-loop **gauge** reset to 0 on any successful iteration, so a
dashboard/alert reads "this loop has re-panicked N× in a row" — the signal that distinguishes a
one-off from a loop stuck in a deterministic-bug backoff spin. A recovered panic is the analog of
C++'s `SevError "TaskError"` (`Net2.actor.cpp:1604-1611`) plus its `countConn*` counters: loud,
counted, and alertable — not a silent process death in a multi-tenant host. This is the line
against CLAUDE.md #9: the recovery is auditable (an operator can tell the loop is degraded), the
panic sites stay as fail-loud invariants, and the fatal class still crashes.

---

## Executable spec (what the tests prove)

A deterministic test hook injects a `panic` inside each class's work function (e.g. a
`panicOnNextRefresh` var the goroutine calls, à la the existing fault-injection dialers), and the
test asserts, against real FDB (testcontainers):

1. **Class A (standing loops — `topologyMonitor`, `backgroundRefresher`):** a test hook (a
   `panicOnNextRefresh` var, same fault-injection idiom as the `fault_test.go` dialers) panics once
   inside `refreshTopology` / the `backgroundRefresher` refresh body. Assert: the process survives;
   `recoveredPanics`/`consecutivePanics` increment; the panic logged once at `slog.Error` with a
   stack (captured via the shared sink). The "goroutine keeps running" assertion is
   **signal-driven, not sleep-based** — the hook closes a channel on the *next* real iteration and
   the test waits on it (no `time.Sleep`-and-hope; CLAUDE.md forbids the flake). A *repeated*
   deterministic panic asserts the re-fire rate is **backoff-bounded** (≤1/s) and the log is
   rate-limited (1 immediate + suppressed-count), `consecutivePanics` climbing. For
   `backgroundRefresher`: the previously-cached read version stays usable across the panic.
1b. **Class A-batch (GRV flush — distinct from Class A; codex P2b):** inject a panic in `flush`
   *after* it pops `b.pending` (incl. while holding `b.mu` in the adaptive-window math). Assert:
   **every** waiter on the popped batch receives an `err` result (none hangs) — wait on the
   `req.reply` channels, signal-driven; a **subsequent** GRV request succeeds (proving `b.mu` was
   *not* left locked — the deadlock guard); counters increment; log rate-limited. This path is
   **not** backoff-bounded and **not** re-armed — it fails each batch at request-arrival rate, so
   the test asserts waiter-errors, *not* a ≤1/s re-fire bound.
2. **Class B:** panic inside `tryOneCoordinator` → `tryAllCoordinators` returns a normal error (the
   race sees one failed leg), the other coordinators still resolve, process survives. Panic inside
   `dialAndPool` → the dial caller gets an error, not a crash.
3. **Class C:** panic inside a facade future's `fn()` → `future.Get()` returns an error, the host
   survives.
4. **Crash-still-happens guard:** prove the fatal class is *not* swallowed — via a **subprocess
   helper** (re-invoke the test binary with an env flag) that triggers a genuine Go runtime-fatal
   (concurrent map write / stack overflow / true OOM — these call `fatalthrow` and **bypass**
   `recover()`), asserting the child exits non-zero with `fatal error:`. NOTE (codex P2): a
   nil-map assignment or nil-pointer deref is an *ordinary, recoverable* panic — the backstop
   *would* catch it — so it is **not** a valid example for this guard.
5. **Revert-prove:** back out each backstop, confirm the matching test panics the test binary
   (process dies), restore. `-race` the touched packages, `--runs_per_test=10`.

---

## Wire-compat impact

**None.** No bytes change — no key/record/index/continuation/atomic-mutation encoding is touched,
no error code is remapped, no retry predicate changes. This is purely process-liveness behavior on
goroutine boundaries. The differential vs `libfdb_c` is unaffected.

---

## Non-goals

- Not converting the encode-side invariant panics (`wire/vtable.go`, `writer.go:73`) into error
  returns — they stay as fail-loud invariants (≙ C++ `ASSERT`); we only stop them from aborting
  the host. If `writer.go:73` ever actually fires it is a codegen bug to fix at the source, and the
  loud `slog.Error` + counter is how we'd find out.
- Not changing the transport loops (already correct).
- Not the `libfdbc` cgo backend's own crash policy (it inherits `libfdb_c`'s).

---

## Verification (how the spec claims were produced)

- C++ at `/tmp/fdbsrc`, `git describe --tags` → **7.3.75**.
- `Net2::run` backstop, `g_crashOnError`, `ASSERT`/`ASSERT_ABORT` read directly
  (`Net2.actor.cpp:1603-1611`, `Error.cpp:28,124`, `Error.h:126-138`).
- Deliberate-crash enumeration by grepping `crashAndDie|criticalError|flushAndExit|ASSERT_ABORT|
  abort()|_exit|outOfMemory` across the tree and filtering to client-reachable, non-simulation
  paths (`Platform.actor.cpp`, `fdb_c.cpp`, `NativeAPI.actor.cpp`, `SystemMonitor.cpp`).
- Monitor self-heal from `MonitorLeader.actor.cpp:840-1005` and `NativeAPI.actor.cpp:1283-1331`
  (+ `ClientKnobs.cpp:52,131-135`, `DatabaseContext.h:852-865`).
- Go decode bounds-checking, transport framing/checksum, and the complete goroutine inventory
  audited against `pkg/fdbgo` HEAD (`wire/reader.go`, `transport/framing.go`, `transport/conn.go`,
  `client/{topology,grv,database}.go`, `fdb/{future,transaction}.go`).
