# RFC-120 — Cycle under injected wire faults (SimTransport)

**Status:** Draft
**Item:** TODO.md C3 ("Ride their test designs"), increment 2 — RFC-119 §7's first follow-up.
**Spec:** `fdbserver/workloads/Cycle.actor.cpp` @ 7.3.75, run under Sim2 fault injection (the C++
test's normal mode); SimTransport (RFC-118) is our wire-level analog of Sim2's network faults.

---

## 1. Problem

RFC-119 landed the Cycle serializability oracle: N keys forming one Hamiltonian cycle, concurrent
swap txns transpose adjacent ring edges, a check asserts the ring stays one cycle. It runs against a
healthy testcontainer — real FDB conflict detection is the chaos source.

But FDB does not run Cycle on a healthy cluster. It runs it under **Sim2 fault injection** — machine
reboots, network partitions, dropped/delayed packets, `future_version`/`wrong_shard` storms — and the
**same single-cycle invariant must still hold** after the client's retries recover. That is the
workload's real teeth: it proves the *client's* retry/recovery paths (read-reply timeout re-send,
inline-error classify→retry, relocate-on-wrong-shard, GRV/version aging, commit retry) never produce a
**wrong** result under fault, only a slower one. The C3 item text says so explicitly: "hammer it
concurrently **(+faults)** … verify the ring stays unbroken … (and later **SimTransport**)."

We cannot join FDB's Sim2 (hermetic, no external socket). **SimTransport (RFC-118) is our substitute**:
`simDialer.dial` (`simtransport_test.go:112`) dials the **real** testcontainer and `simConn.proxyLoop`
(`:59`) intercepts the storage server's **reply** frames — dropping them or rewriting them to a
faithful inline `ErrorOr<reply>` error (`types.MarshalErrorOrInlineError`, the exact byte shape
`storageserver.actor.cpp sendErrorWithPenalty` produces). So we can run the **real, data-bearing**
Cycle workload against real FDB while injecting wire faults on storage reads, and assert the ring
survives.

**Test-only — zero production/wire impact**, like RFC-119. The review question is fault-model fidelity
+ no-flakes discipline (the fault must perturb without wedging, and every assertion must be
timing-independent).

## 2. The C++ design (cited)

`cycleClient` (`Cycle.actor.cpp:153-211`) wraps the read-chain + swap + commit in
`try { … } catch (Error& e) { … wait(tr.onError(e)); }` (`:200-205`) — **every** error (including the
injected `future_version`/`process_behind`/`wrong_shard`/`transaction_too_old` Sim2 produces) is
retried via `onError`, which classifies retryability and backs off. The `tooOldRetries`/
`commitFailedRetries` counters (`:201-204`) just *count* the faults; correctness comes from
**re-reading the current ring state on every retry** and computing a fresh valid transposition, so a
committed swap is always a valid permutation of whatever it read — the single-cycle invariant is
preserved regardless of how many faults/retries preceded the commit. `cycleCheck` (`:295-335`) itself
retries `transaction_too_old` up to many times (`:322-331`) — the check, too, runs under fault.

Our Go analog: `db.Transact`'s retry loop IS `onError` (RFC-119 established this), and each swap txn
re-reads the chain inside `fn` on every attempt — so the same fault-tolerance property holds for free.
This RFC adds the *fault injection* that exercises it.

## 3. Proposed Go change (test-only)

New test(s) in `pkg/fdbgo/client/cycle_workload_test.go` (extending RFC-119's `cycleWorkload`; reuses
its `setup`/`swapOnce`/`check`/`checkData` verbatim — the workload is identical, only the transport is
faulted) using the in-package SimTransport harness:

```go
db, sd := newSimTestDB(t, ctx)          // real container, client dials through the fault proxy
w := &cycleWorkload{nodeCount: N, prefix: …}
w.setup(ctx, db); w.check(ctx, db)       // ring valid pre-fault
storageAddr := storageAddrFor(t, db, ctx, w.key(0))  // arm only storage reads (commits/GRV/locate go to the proxy/coordinator — unarmed)

var injected atomic.Int64
sd.setIntercept(everyNthInlineError(faultEveryN, code, &injected))  // see §3.1
sd.armAddr(storageAddr)

… run A actors swapping for a bounded window (RFC-119's loop, through the faulted db) …

sd.setIntercept(nil)                     // disarm: the final check reads fault-free
… assert injected > 0, committed > 0, w.check(ctx, db) == nil …
```

### 3.1 The fault model — perturb without wedging (no pulsing, no sleep)

The intercept injects a **retryable** inline error on a deterministic **fraction** of armed storage
read replies — `everyNthInlineError(n, code, counter)`: for armed non-PING frames, every `n`-th
(by the per-conn frame `idx` the `proxyLoop` already tracks) is rewritten to
`inlineError(code, penalty)`; the other `n-1` pass through unchanged. This is strictly better than
`dropAll()`-with-pulsing:
- **No wedge / no deadlock:** `(n-1)/n` of reads always succeed, so every actor makes progress; no
  arm/disarm orchestration, no `time.Sleep` (the no-flakes rule), no risk of an actor livelocking the
  whole window on a fully-dropped server.
- **Deterministic firing:** the fault fires by frame index, not the clock — `injected > 0` is a
  non-timing-dependent proof the fault actually happened (anti-vacuity for the fault itself).
- **Exercises the real retry path:** `code = future_version (1009)` drives the QueueModel-backoff
  read-retry (the exact path C4/`TestSimInlineFutureVersion` pins) **under concurrent load**;
  `process_behind (1037)` and `wrong_shard (1001)` are the same shape (1001 adds a relocate). First
  increment pins **1009**; 1037/1001 are trivial table rows to add.

`armAddr(storageAddr)` scopes the fault to **storage reads only** — commit (commit proxy), GRV, and
locate (`GetKeyServerLocations`, coordinator) replies flow on **different** addrs and are never armed,
so the control plane is never corrupted (RFC-118's armAddr-not-armAll discipline). The injected error
rides the read reply exactly as a real storage server's `sendErrorWithPenalty` would.

## 4. Executable spec — what it proves

1. **`TestCycle_SurvivesInjectedReadFaults`** (real FDB via SimTransport): ring of N nodes, A actors
   swapping for a bounded `workCtx`, `future_version` injected on every `faultEveryN`-th storage read.
   After disarm + quiescence, assert **all three** (all non-timing-dependent):
   - `injected.Load() > 0` — the fault actually fired (else the test is vacuous — passing because no
     fault occurred, the classic fault-injection footgun);
   - `committed.Load() > 0` — the workload made progress *through* the fault (the client recovered, it
     didn't just wedge);
   - `w.check(ctx, db) == nil` — **the ring is still exactly one Hamiltonian cycle** despite the
     injected read faults + the client's retries. This is the serializability-under-fault proof.
   Swap-actor error handling keys on error identity (RFC-119): a `context` error = window close; a
   **non-retryable** error surfacing from `db.Transact` (i.e. the client failed to absorb a *retryable*
   injected fault) → `t.Errorf`. So if the client ever mis-classified the injected 1009 as terminal,
   the test fails loudly — that's a real finding, not a flake.
2. **Teeth / control:** RFC-119's `TestCycle_SerializableUnderConcurrency` (no faults) is the control —
   same workload, healthy transport, must also pass. The delta between them is the fault path.
   `everyNthInlineError`'s injection counter (`injected > 0`) is the proof the fault fired; a unit test
   pins `everyNthInlineError` itself (every n-th frame rewritten, others verbatim, PINGs untouched) so
   the intercept logic is deterministically covered without FDB.
3. **(stretch, same PR if clean) a code table** over `{1009, 1037, 1001}` proving each retryable inline
   read error is absorbed and the ring survives — one `t.Run` per code.

The teeth are item 1's `injected > 0 && check == nil`: a serializability oracle under fault is only
meaningful if (a) the fault demonstrably fired and (b) the ring still checks out. A pass with
`injected == 0` would be a fake checkbox (faulted nothing).

## 5. Wire-compat impact

**None.** Test-only; the injected bytes are a faithful `ErrorOr<reply>` inline error
(`types.MarshalErrorOrInlineError`, already wire-validated by C4/RFC-118), delivered on a real reply
frame — it exercises the client's *decode + classify* of a shape a real storage server emits. No
production code changes; differential-vs-libfdb_c gates N/A.

## 6. Risks (the no-flakes hard line)

- **Wedge / livelock.** Mitigated by the fraction model (§3.1): `(n-1)/n` reads always succeed, so
  progress is guaranteed; `workCtx` bounds the window; the final check runs disarmed (fault-free), so
  it is reliable. No `time.Sleep`, no rate assertion — assertions are pure counters + the ring walk.
- **QueueModel backoff stacking.** Sustained 1009 adds per-address backoff; with `n ≥ 3` the
  pass-through majority keeps throughput positive over a ~20 s window (committed > 0 has wide margin).
  If backoff proves too aggressive at small `n`, raise `n` — it only changes fault density, never the
  invariant.
- **Arming the wrong conn.** `armAddr(storageAddr)` must hit the storage server, not the proxy, or the
  fault would corrupt commits/GRV. `storageAddrFor` (`:182`, warmed by a prior read) returns exactly
  `Servers[0]` for the key — the RFC-118-blessed way to scope storage-only faults.
- **Single-process container ⇒ one storage server.** `armAddr` on it arms all storage reads (fine —
  that's the intended blast radius); commits/GRV/locate still flow on other addrs. No multi-shard
  assumption.

## 7. Follow-ups

- 1037/1001 code-table rows (if not in this PR), and a `dropAll`-pulsed variant (reply *loss*, not
  just inline error — exercises the read-reply *timeout* re-send path under load) if it can be made
  wedge-free.
- The remaining C3 workloads (AtomicOps / ConflictRange / Serializability / FuzzApi gaps), each its own
  increment.
