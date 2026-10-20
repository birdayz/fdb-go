# RFC (draft): allocation-free read cancellation context

Status: draft, design only. Base: origin/master `33c13686e`. Spec: libfdb_c 7.3.77 (`/tmp/fdbsrc`).
Go stdlib cites are for the toolchain on the box (go1.27.1, `$GOROOT/src/context/context.go`).

## 1. Problem

Every top-level read op goes through `Transaction.opContext`
(`pkg/fdbgo/client/read_incarnation.go:144-185`). It builds a fresh `cancelCtx` child of
the caller's ctx, links it to the incarnation's own `cancelCtx` with `context.AfterFunc`,
wraps it in a `valueCtx` that carries a `*readOperation`, and returns a closure that tears
all of this down. The pipelined binding read (`fdb.Transaction.Get` → `GetPipelined` +
`PendingGet.Resolve`, `pkg/fdbgo/fdb/transaction.go:50`) pays for it twice: once in
`GetPipelined` (`transaction.go:924`) and again in `Resolve` (`pending_get.go:50`).
The baseline heap profile (`diagnostics-v3-control-v4/.../go-heap-get1000-alloc-objects`)
puts `context.AfterFunc`, `withCancel`, `WithValue`, `WithCancelCause`,
`cancelCtx.Done`, `propagateCancel`, `opContext`, and `enterReadState` among the top
flat sites. Together they make up about 20 to 26 % of read-path objects.

## 2. Allocation inventory (source-derived; §7 test T1 confirms it)

Path: `fdb.Transaction.Get` on a server read with a `context.Background()` parent, which
is what `Database.Transact` passes (`fdb/database.go:373-380`).

| # | where | object | what it provides |
|---|---|---|---|
| 1 | `opContext:163` → `context.withCancel` (ctx.go:278) | `*cancelCtx` (per op) | one Done channel per op that merges the user's ctx with incarnation failure |
| 2 | `opContext:163` → `WithCancelCause` (ctx.go:271) | `CancelCauseFunc` closure | the cancel handle; carries the cause (but see note 1 below) |
| 3 | `opContext:164` → `AfterFunc` (ctx.go:327) | `*afterFuncCtx` | registers the op on `inc.ctx` (tx Cancel, Reset, timeout) |
| 4 | `opContext:164` | the `func(){cancel(Cause(inc.ctx))}` closure | forwards the incarnation's cause into the op ctx |
| 5 | `AfterFunc` (ctx.go:331) | `stop` closure | lets the op deregister from the incarnation at op end |
| 6 | `enterReadState` (`transaction_lifetime.go:62`) | `*executionLease` | pins the incarnation against turnover (Reset/OnError drain, the C++ `reading` barrier) |
| 7 | `opContext:179` → `WithValue` (ctx.go:738) | `*valueCtx` | carries the op to nested calls |
| 8 | `opContext:179` | `*readOperation` | {inc, parent, lease, entryErr}: the captured incarnation, the deferred-error verdict, nested-borrow detection |
| 9 | `opContext:180` | release closure | ends the op: stop, then `cancel(nil)`, then `lease.release` |
| 10 | first `ctx.Done()` (ctx.go:458), from the selects in `enqueueOwned` and `PendingGet.resolve` | `chan struct{}` | the op's Done channel |
| 11 | `Resolve` → `enterReadState` | `*executionLease` | the resolve phase takes its own lease (the send phase released the first one at `transaction.go:929`) |
| 12 | `Resolve` → `WithValue` (`pending_get.go:50`) | `*valueCtx` | rebinds the op to the new lease |
| 13 | `Resolve` | `*readOperation` | the same data as #8, with the new lease |

There are also per-incarnation costs, which a one-read transaction pays per read:
- `readIncarnationLocked` (`read_incarnation.go:39-40`) allocates the `readIncarnation`, `inc.ctx`'s `cancelCtx` and its closure.
- `inc.ctx` gets a Done channel (created by #3's `propagateCancel`) and a `children` map (ctx.go:500).
- **Totals today: 10 objects on the send side, 3 on resolve, and 5 per incarnation.**
- **Strict context machinery** (#1, #2, #3, #4, #5, #7, #10) is the "~7 per read" the profile shows.
- A synchronous `tx.Get` (`transaction.go:891`) pays #1 to #10, so 10 per read.

If the user's ctx is cancellable (`TransactCtx` with a deadline), #1 also inserts into the
parent's `children` map. A non-stdlib parent ctx that has a Done channel makes
`propagateCancel` start a watcher goroutine for every op (ctx.go:521-528).

Notes:
1. **No code reads `context.Cause` of an op ctx.** The only `Cause` call is
   `read_incarnation.go:164`, and its result is forwarded into ctx #1 where no one reads
   it. Error mapping always reads `op.inc.cause` under `readErrMu`
   (`readLifetimeError:211`, `mapReadError:229`, `PendingGet.interruption:56`). The
   cause is therefore already stored on the transaction side. #2's cause plumbing is dead
   weight.
2. **Consumers of op contexts only use `Done`, `Err`, `Deadline` (the handshake,
   `transport/conn.go:362`) and `Value(readContextKey)`.** No goroutine started on the
   read path outlives its op while holding the op ctx:
   - hedging is synchronous (`hedge.go`);
   - dials are bound to `db.ctx` (`database.go:588`);
   - the handshake watcher is joined (`conn.go:378-391`).
   The cold-path `context.WithTimeout(ctx, …)` derivations (`locality.go:549`, `metrics.go`)
   are cancelled by their callers' `defer`.

## 3. C++ semantics to preserve (7.3.77)

- **One `resetPromise` per RYW incarnation.** Every read races its result against it:
  `choose { when(read…) ; when(wait(ryw->resetPromise.getFuture())) }`
  (ReadYourWrites.actor.cpp:354-395, 1196-1213). When the promise fails, the losing read
  actor is cancelled, which drops the NativeAPI/loadBalance wait. The promise's error is
  thrown into the read, so `internal_error` is unreachable. **Go analog: one shared
  Done signal per incarnation, with the cause stored once on the incarnation.**
- **Who fails it, and with what.** Every sender is guarded by `if (!resetPromise.isSet())`,
  so **the first error wins**:
  - `cancel()` sends `transaction_cancelled` 1025 (:2730-2733);
  - the destructor sends 1025 (:2771-2773);
  - `resetRyow()` sends 1025 to the *old* promise only if it is unset, after installing a
    fresh one (:2699-2728). `reset()` reaches it through :2735-2756, and onError through :1505-1532;
  - the timebomb sends `transaction_timed_out` 1031 (:1567-1574);
  - `checkUsedDuringCommit` sends `used_during_commit` (:2781-2786).
  So a read that timed out stays 1031 after a later reset or cancel.
- **Timeout option.** `TIMEOUT` → `resetTimeout()` (:2570-2573, 1576-1579) replaces
  `timeoutActor`, which cancels the old timebomb. A timebomb that already fired has set
  the promise, and nothing but reset clears it. Go already models this (`configureReadTimeout`,
  `armReadTimeoutLocked`, `read_incarnation.go:78-139`); this RFC does not change it.
- **Entry check.** Every read entry checks `if (resetPromise.isSet()) return
  resetPromise.getFuture().getError();` synchronously (:1679-1680, 1700-1701, 1736-1737, …),
  before `key_outside_legal_range`. Go: `readLifetimeError` (`read_incarnation.go:211`),
  where the incarnation's cause wins over an already-cancelled caller ctx at entry.
- **Drain.** `reading.add(success(result))` (:1691) is the commit barrier that waits for
  outstanding reads. Go: lease `users` and `drained` (`transaction_lifetime.go:17-31`, 145-186).
- **Per-future cancel.** `fdb_future_cancel` (bindings/c/fdb_c.cpp:180-182) →
  `ThreadSingleAssignmentVar::cancel` (flow/include/flow/ThreadHelper.actor.h:443) →
  `operation_cancelled` 1101 (ThreadHelper.actor.h:732-736). This cancels only that
  future, never the transaction. Go's analog is the caller's `ctx` (Go-only, RFC-090):
  - at completion, `mapReadError` lets `parent.Err()` win over `inc.cause`
    (`read_incarnation.go:233-237`);
  - at entry, `inc.cause` wins.
  **Both orders must stay exactly as they are.**
- **Delivery is atomic in flow.** `sendError` runs the callbacks on the network thread in
  one step. Go splits it into "record the cause under `readErrMu`" and "deliver". The
  current code guards the gap with "read `ctx.Err()` before reading the cause"
  (`read_incarnation.go:213-215`), and with the admission re-check that delivers an
  already-recorded cause (`:165-171`). That re-check is also the *only* delivery for a
  timeout that is already past due at creation (`armReadTimeoutLocked:117-118` records
  1031 but never cancels).

## 4. Design

### 4.1 `readOperation` *is* the op context

`readOperation` becomes a `context.Context` implementation and absorbs objects #1, #2, #5,
#6, #7, #8, #9 and #11 to #13 into a single object:

```go
type readOperation struct {
    tx       *Transaction
    parent   context.Context
    inc      *readIncarnation
    lease    *executionLease  // nil = entry refused; else &leaseSlot or a borrowed parent lease
    leaseSlot executionLease  // embedded: no separate allocation
    entryErr *wire.FDBError
    merged   context.Context  // nil unless parent.Done() != nil (§4.3)
    mergedCancel context.CancelFunc
    prev, next *readOperation // inc.linked list, guarded by readErrMu (§4.3)
}
func (o *readOperation) Deadline() (time.Time, bool) { return o.parent.Deadline() }
func (o *readOperation) Done() <-chan struct{}  { if o.merged != nil { return o.merged.Done() }; return o.inc.done }
func (o *readOperation) Err() error {
    if o.merged != nil { return o.merged.Err() }
    select { case <-o.inc.done: return context.Canceled; default: return nil }
}
func (o *readOperation) Value(k any) any {
    if k, ok := k.(readContextKey); ok && k.tx == o.tx { return o }
    if o.merged != nil { return o.merged.Value(k) }
    return o.parent.Value(k)
}
func (o *readOperation) AfterFunc(f func()) (stop func() bool) // §4.4; cold path only
```

- `tx.readOperation(ctx)` stays `ctx.Value(readContextKey{tx})`, so the ~20 call sites and
  every lease-identity user keep working unchanged: `publishCommit`, `resetExpected`,
  versionstamp, and the `op.lease == nil` checks.
- `opContext` returns `(ctx context.Context, op *readOperation)`. Callers write
  `defer op.end()`, which is nil-safe and a no-op on a borrow. This removes the release
  closure (#9). It is a mechanical change to the ~30 call sites listed by
  `rg 'tx.opContext\('`.
- The `context.Cause` wiring is removed (§2 note 1). `Err()` returns `context.Canceled` on
  incarnation failure, which is exactly what the current cancelCtx returns. So
  `mapReadError`, `isReplyTimeout` and `errors.Is` callers see identical errors.

### 4.2 One Done channel per incarnation, closed when the cause is recorded

`readIncarnation` drops `ctx` and `cancel` and gains `done chan struct{}`, created eagerly
in `readIncarnationLocked`. That is 1 alloc per incarnation, replacing 4 today: the
cancelCtx, its closure, the done channel and the children map.

`recordReadCauseLocked` closes `inc.done` in the same critical section that sets
`inc.cause`. `close` cannot re-enter (it runs no callbacks; it only wakes waiters), so it
is safe under the leaf `readErrMu`. RFC-256's rule ("cancellation delivery … outside")
exists because stdlib `cancel` propagates into children and AfterFunc callbacks, and
§4.3/§4.4 keep exactly that part outside the lock.

Consequences:
- For an incarnation-only op (case A), "recorded" and "delivered" are one step, as in
  flow. `Err() != nil` holds exactly when `inc.cause != nil`. The ordering trick in
  `readLifetimeError` still holds, now trivially.
- An incarnation created past its deadline (`armReadTimeoutLocked:117-118`) is born with
  `done` closed. The admission re-check (`opContext:165-171`) is no longer needed for case A.
  It stays for case B as `deliverLinked`.
- `failCapturedIncarnation`, `configureReadTimeout`, `fireReadTimeout` and `turnoverLocked`
  replace `inc.cancel(cause)` with `tx.deliverLinked(inc)` (§4.3), still called after
  unlocking. Pending retirement and timer stop are unchanged.
- First-error-wins is unchanged: only the `inc.cause == nil` arm closes the channel, so
  a double close is impossible by construction.

### 4.3 Case A and case B: wiring the user's ctx only when it can fire

When the op is created (outside `readErrMu`), opContext checks `parent.Done()`:

- **Case A: `parent.Done() == nil`.** This covers Background, TODO, WithoutCancel, and
  `Transact()`. Set `merged = nil`, so `Done()` is `inc.done`. **The per-op cost is
  0 objects beyond the `readOperation` itself.** A parent with a nil Done channel can
  never be cancelled (the `context.Context` contract), so nothing is lost.
- **Case B: `parent.Done() != nil`.**
  1. Create `merged, mergedCancel = context.WithCancel(parent)`. The stdlib registers it in
     the parent's children map with no goroutine when the parent is stdlib-derived. That
     costs 3 objects: the cancelCtx, its CancelFunc and, lazily, its channel.
  2. Link `o` into `inc.linked` under `readErrMu`. This intrusive list costs 0 allocs and
     replaces the `AfterFunc` objects #3 to #5.
  3. If `inc.cause != nil` at link time, do not link. Call `mergedCancel()` after unlocking.
  - `deliverLinked(inc)` detaches the list under the lock and marks it as drained, using
    a sentinel. After unlocking it calls each `mergedCancel()`.
  - `op.end()` unlinks under the lock (O(1)), unless the list is already drained, then
    calls `mergedCancel()`. This removes the registration from the parent's children map
    at op end, exactly as today.

  `merged` overrides `Value(&cancelCtxKey)` and its done channel equals `o.Done()`.
  Because of that, stdlib children derived from `o` (`WithTimeout(o, …)`) attach directly
  to `merged`'s children map through `parentCancelCtx` (ctx.go:383-397), with no
  goroutine and no AfterFunc.

### 4.4 Children of a case-A op

In case A, `o.Done()` is `inc.done`, which no stdlib `cancelCtx` owns. A stdlib child
would otherwise fall back to spawning a goroutine (ctx.go:521). `readOperation`
implements the `afterFuncer` interface (ctx.go:343-345, used at ctx.go:507-519):

- `AfterFunc(f)` appends `{f}` to a per-incarnation callback list under `readErrMu`
  (1 alloc, cold paths only: locate miss, metrics).
- If `inc.cause` is already set, it runs `go f()`, matching the `AfterFunc` contract.
- The returned `stop` removes the entry; that is called by the child's `cancel`/`removeChild`.
- `deliverLinked` also detaches this list and runs each `f` in its own goroutine, after unlocking.

### 4.5 Pipelined path: the op lives inside the `PendingGet`

`GetPipelined` allocates the `PendingGet` at entry and uses `&p.op` as the op ctx. It is
1 object, which the server path allocates today anyway.
- On a RYW hit or an early error the shell is dropped, so the cost is 1 object versus 10 today.
- On a borrow, where a parent op exists with an active lease, `p.opRef` points at the parent op, as today.

`Resolve` no longer builds a new ctx:
1. It takes the lease into a stack `executionLease`. `enterReadState` changes to
   `enterReadState(ctx, inc, wait, out *executionLease) bool`, which does not retain
   `out`, so the lease stays on the stack.
2. It keeps the current order: lease first, then `p.mu` (RFC-256 lock order).
3. Under `p.mu`, if the future is not done, it copies the lease into `p.op.leaseSlot` and
   sets `p.op.lease = &p.op.leaseSlot`.
4. It sets `p.op.entryErr = nil`. Today Resolve's fresh op has a nil `entryErr`; a
   `PendingGet` only exists if entry was clean, so this is the same behaviour.
5. A losing concurrent resolver (Commit drain versus `future.Get`) sees `p.done` and
   releases its stack lease. This removes #11 to #13.

`completeLocked` calls `p.op.end()` instead of `p.cancel()`.

### 4.6 Synchronous ops

`Get`, `GetKey`, `getRange` and the rest pay 1 object: the `readOperation`, which escapes
as an interface.

Optional Phase 2: a one-slot free list on `Transaction` (`atomic.Pointer[readOperation]`;
`Swap(nil)` to take, `CompareAndSwap(nil, o)` after `end()` to return) makes sequential
synchronous reads 0 alloc. It is gated on three things:
- an escape audit showing no reference outlives `end()`; the PendingGet-embedded ops are never pooled;
- a `seq` field checked in race builds;
- T1 showing a measurable win.

### 4.7 Optional Phase 2: share case B per incarnation

Ops in one incarnation usually pass the *same* ctx; the binding passes `t.ctx` to every
read. A per-incarnation `{done chan, merged, cancel, refs}` slot keyed by
`parent.Done()` lets overlapping case-B ops share one `merged`:
- the key is a channel, so it is always comparable; comparing contexts directly would
  panic on non-comparable implementations;
- `refs` counts live ops; the last `end()` cancels `merged`.

This amortises the 3 objects across a pipelined batch. It gives **no leak guarantee
weakening**: no registration outlives the last op that uses it, the same bound as today.
An unbounded "until incarnation end" variant is rejected: an abandoned transaction under a
long-lived cancellable ctx would leak one `cancelCtx` into that ctx's children map,
because Go has no `~RYW` (ReadYourWrites.actor.cpp:2771) to retire it.

## 5. Races, ordering and leaks

**Which error wins (unchanged, pinned by T3):**
- **Entry.** `readLifetimeError` reads `ctx.Err()` and then `inc.cause`, and the cause wins.
  - Case A: `Err()` is derived from `inc.done`, which closes inside the cause's critical
    section, so this cannot observe "done but no cause".
  - Case B: `merged` is cancelled only after the cause was recorded, or by the parent; the
    parent case is handled by `mapReadError`, where the parent wins at completion.
- **Completion.** `mapReadError` maps only `Canceled`/`DeadlineExceeded`, then
  `parent.Err()`, then `inc.cause`. That is unchanged. The parent is `o.parent`, never
  `merged`. `merged` can be cancelled by the incarnation, so consulting it would report
  `Canceled` instead of 1025/1031.
- **Cause.** `recordReadCauseLocked`'s nil guard gives 1025 versus 1031 versus commit
  outcomes first-wins, which mirrors the `isSet()` guards in §3. Reset records 1025 on the
  old incarnation only if it has no cause, as `resetRyow` does at :2726-2727.
- **Turnover.** It records the cause and closes `old.done` (old-incarnation reads wake)
  before the drain wait in `turnoverLocked`. A new incarnation gets a fresh `done`, so a
  reset never wakes post-reset ops, and an old op never observes the new channel.
  `opContext` captures `inc` once and stores it in `o.inc`, which is the same as today.
- **Reply-ready versus cancel.** `PendingGet.resolve`'s `TakeReadyOrCancel` after
  `ctx.Done()` is unchanged (`TestReadReplyReadyOutranksCancellation`).

**Admission races:**
- Case A: none. The op reads `inc.done` whatever its state.
- Case B: link-or-cancel happens under `readErrMu`, against `deliverLinked`'s
  detach-and-mark-drained. Each op is therefore cancelled exactly once, by whichever side
  wins. `mergedCancel` is idempotent.

**Leaks:**
- **Goroutines.** None are spawned for stdlib parents or for derived children (§4.3, §4.4).
  A non-stdlib parent with a Done channel and no `AfterFunc` method still gets one watcher
  goroutine per op from `WithCancel`, exactly as today. It exits at `op.end()`.
- **Registrations.** None outlive the op:
  - the parent's children map entry goes at `end()`;
  - the `inc.linked` node goes at `end()` or at drain;
  - the AfterFunc entries go at `stop` or at drain.
  `inc.done` is unreferenced garbage once the incarnation and its ops are unreachable.
  There is no cleanup needed for abandoned transactions, which is strictly better than
  today's `inc.ctx.children` map.

**Behaviour change: an op ctx is no longer Done after `end()` in case A.** Today
`cancel(nil)` closes the op's channel at op end. Nothing depends on it (§2 note 2), and the
two internal holders past `end()` are covered:
- `PendingGet` is memoised;
- borrow paths never reach `end()`.

Guard against a future holder with T5.

## 6. Projected allocations per read

| path | today | Phase 1 | Phase 2 |
|---|---:|---:|---:|
| binding `Get`, server read, Background (send + resolve, beyond `PendingGet`) | 12 (13 including `PendingGet`) | **0** (1 including `PendingGet`) | 0 |
| binding `Get`, RYW hit, Background | 10 | 1 | 1 |
| sync `tx.Get`, Background | 10 | 1 | 0 (free list) |
| binding `Get`, cancellable ctx | 12 + a parent-map insert | 3 | ~3 per pipelined batch, amortised |
| sync `tx.Get`, cancellable ctx | 10 + a parent-map insert | 4 | 3 (or ~0 when overlapping) |
| per incarnation | 5 | 1 (+1 if a timeout timer) | 1 |

- For the get1000/Background shape this projects **−12 objects per pipelined read**, and
  **−4 per transaction** from the incarnation.
- Strict context machinery goes from **7 per read to 0**.
- Phase 1 alone should remove the whole ~20 to 26 % slice. This is a projection; T1 must
  measure it.

Adjacent, not in scope: `enterState` heap-allocates an `executionLease` on every public
call (pprof-breakdown.md #7: 90.6 allocs per transaction on batchset50). The same
caller-storage `enterReadState` change (§4.5), applied to `enterState`, makes that 0. It
should be a sibling PR.

## 7. Test plan (Bazel only: `just test`; heavyweight targets tagged `test-full`)

- **T1: alloc pins.** Add `client/read_context_alloc_test.go` with `testing.AllocsPerRun`
  over the in-process fake-storage harness used by `read_incarnation_fdb_test.go`. Skip
  under `-race`, where alloc counts are not meaningful. Pins:
  - (a) GetPipelined + Resolve, Background: delta versus a bare `PendingGet` ≤ 0;
  - (b) RYW hit ≤ 1;
  - (c) sync Get ≤ 1, plus the reply decode baseline, measured first and pinned as an exact number;
  - (d) cancellable parent ≤ 3/4;
  - (e) borrowed nested op 0.
  Pin exact numbers, not ranges, so regressions fail loudly. Also add a binding-level pin
  in `fdb/`.
- **T2: unchanged suites stay green.** `read_incarnation_test.go`,
  `read_incarnation_fdb_test.go` (all 15),
  `transaction_lifetime_test.go`, `versionstamp_*`, `watch_ctx_fault_test.go`,
  `deferred_error_test.go`.
  `beforeReadFailureDelivery` now runs before `deliverLinked`. For case A the window it
  probes is gone by design, so `TestReadIncarnation_EntryCancellationObservationKeepsCause`
  must run both with a Background parent and with a cancellable one.
- **T3: precedence matrix.** Table-driven, against a held storage reply:
  - the trigger is one of {Cancel, Reset, OnError, timeout fires, `SetTimeout` shrink to
    past, `SetTimeout(0)` after fire, parent cancel, parent deadline};
  - the trigger is crossed with {before entry, in flight, after reply-ready}, and with
    case A and case B;
  - assert the exact error: 1025, 1031, `Canceled`, `DeadlineExceeded` or the value;
  - also assert that a second trigger never changes the first error.
- **T4: race and stress.** Run the admission-versus-delivery loops under `-race`
  (CI/nightly lane):
  - N goroutines `GetPipelined`, `Resolve`, `Get` concurrently with Cancel, Reset and
    `SetTimeout` churn;
  - after each step, assert `Err()!=nil ⇒ readIncarnationCause!=nil`;
  - assert no double close: no panic, under both case A and case B.
- **T5: leak checks.**
  - `runtime.NumGoroutine` is flat after 10k case-A and case-B ops with
    `WithTimeout(opCtx)` children (§4.4);
  - a test hook reports `len(inc.linked)==0` and `len(inc.afterFuncs)==0` after the ops end;
  - a parent `cancelCtx` survives 10k ops without growth, checked through a test-only
    custom parent that counts `AfterFunc` registrations;
  - a static check finds no `PendingGet` or op ctx reachable after `completeLocked`.
- **T6: libfdb_c differential.** Extend `bench/differential_cancel_test.go` (`test-full`):
  - read in flight, then Cancel → 1025 on both;
  - Reset, then the old future → 1025 and the new op succeeds;
  - timeout mid-read → 1031;
  - timeout, then Reset → the old future stays 1031;
  - Cancel after the timeout fired → 1031;
  - entry after Cancel → 1025 synchronously;
  - Go-only parent-cancel rows are asserted on Go alone (the C++ analog, `fdb_future_cancel`
    → 1101, is not mapped).
  - Run through the `hunt-divergences` loop.

## 8. Rollout

1. **PR 1.** Add `readOperation`-as-Context, `inc.done` closed under the record, and
   `deliverLinked`. Change `opContext` to return the op, and update the call sites. Covers
   cases A and B and the AfterFunc registry. Tests T1 to T5.
2. **PR 2.** Embed the op in `PendingGet` and switch `Resolve` to stack leases. Adds T1(a)
   and the binding pin.
3. **PR 3.** T6 differential rows.
4. **Optional PR 4.** Phase 2: the free list and shared case-B link, only if T1 or the
   benchmark shows the remaining objects matter.
5. **Sibling PR.** The `enterState` stack lease.

Reviewers: FDB C++ client maintainer, citing the §3 lines; Torvalds, on the reduced
surface and lock order.

## 9. Open questions

1. Should the case-B `merged` derive from `parent` or from `context.WithoutCancel`?
   Deriving from `parent` is the only way to inherit cancellation for free. Keep it.
2. `inc.done` is eager. Could it be lazy (created on first `Done`)? On the hot path every
   op calls `Done`, so lazy creation saves nothing and adds a lock. Keep it eager.
3. `Cause(opCtx)` returns `Canceled` rather than 1025/1031. No caller depends on it.
   Should the type document "use `mapReadError`, never `Cause`"? Yes; add it to the
   type comment.
