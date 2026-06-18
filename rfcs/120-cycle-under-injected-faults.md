# RFC-120 — Cycle under injected wire faults (SimTransport)

**Status:** Accepted (v2 — folds in the RFC-stage review)
**Item:** TODO.md C3 ("Ride their test designs"), increment 2 — RFC-119 §7's first follow-up.

**Reviews (RFC stage):** FDB C++ maintainer — **NAK'd v1** (caught the fatal control-plane-scoping
flaw: the single-process container collocates storage/commit/GRV on one connection, so the v1 frame-
index-blind intercept would corrupt `CommitID`/`GetReadVersionReply` replies → flaky red). v2 fixes it
with **content-discrimination** (§3.2: fault only read-reply fileIDs, pass commit/GRV/locate verbatim)
+ a mandatory commit-passthrough regression. Torvalds — ACK with conditions, all folded in: N=4 pinned
with proven headroom (the "raise N if it bites" flake-hiding escape hatch is deleted), rigorous
progress guarantee, ship 1009 alone (defer 1001 — different relocate path), mandatory intercept unit
test. Test-only, zero wire impact → differential-vs-libfdb_c gates N/A.
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

var injected atomic.Int64
sd.setIntercept(everyNthInlineReadError(4, ErrFutureVersion, &injected))  // see §3.1
sd.armAll()                              // content-discrimination scopes the fault, NOT the addr (§3.2)

… run A actors swapping for a bounded window (RFC-119's loop, through the faulted db) …

sd.setIntercept(nil)                     // disarm: the final check reads fault-free
… assert injected > 0, committed > 0, w.check(ctx, db) == nil …
```

### 3.1 The fault model — perturb without wedging (no pulsing, no sleep)

The intercept injects a **retryable** inline error on a deterministic **fraction** of READ replies —
`everyNthInlineReadError(n, code, counter)`: for armed non-PING frames whose body is a **read reply**
(§3.2), every `n`-th *read reply* is rewritten to `inlineError(code, penalty)`; all other read replies
pass through, and **every non-read frame (commit / GRV / locate) passes through verbatim**. This
perturbs without wedging:
- **No wedge / no deadlock:** a swap does 3 reads; with `n = 4` the per-read clean probability is
  `3/4`, so `P(all 3 clean) = (3/4)³ ≈ 42%` on the *first* attempt, and every fault is **fixed-delay**
  retryable (`future_version` ⇒ `futureVersionDelay`, a constant — `transaction.go:1808`, NOT growing
  backoff), so each actor lands a commit in O(tens of ms) even in the bad case. **Progress guarantee
  (rigorous):** the per-attempt clean-read probability is bounded below by a positive constant and
  every fault is fixed-delay-retryable ⇒ each actor commits infinitely often in expectation within the
  window — livelock-free, no hand-waving. Over a 20 s window × 16 actors, committed lands in the
  **thousands** (3+ orders of magnitude above the `> 0` floor), so `committed > 0` is flake-proof *by
  construction*. **N is pinned at 4 for that proven headroom — it is NOT a knob to tune to green: if
  committed ever approached 0, that is a client retry bug to root-cause (the prime directive), never a
  reason to raise N.**
- **Deterministic firing:** the fault fires by read-reply count, not the clock — `injected > 0` is a
  non-timing-dependent proof the fault actually happened (anti-vacuity for the fault itself). *Which*
  logical read faults varies run-to-run (16 actors multiplex frames over the shared conn), but no
  assertion depends on which — only that some did (`injected > 0`) and the ring survived
  (`check == nil`). That nondeterminism never reaches an assertion.
- **Exercises the real retry path:** `code = future_version (1009)` drives the read-retry +
  classify→`onError`→reset→re-read loop (the path C4/`TestSimInlineFutureVersion` pins the *front* of,
  but does NOT drive to completion) **under concurrent load**. First increment ships **1009 alone**;
  `process_behind (1037)` is the same fixed-delay path (a one-line table row, this PR or next).
  **`wrong_shard (1001)` is DEFERRED to its own increment** — it is a *different* recovery mechanism
  (relocate + cache-invalidate, where a buggy resume could drop/dup, a distinct failure mode deserving
  its own ring-survival assertion), not "just another retryable inline."

### 3.2 Control-plane scoping is by CONTENT, not address (the RFC-119-review NAK fix)

The single-process testcontainer (`startProxyFDB`) collocates **storage server, commit proxy, and GRV
proxy on one address:port** (empirically confirmed by the FDB-C-dev review), and the client's
connection pool is keyed by address (`database.go`), so **all roles multiplex over ONE connection**.
Therefore `armAddr` does **not** isolate the control plane — reads, commits (`CommitID`), GRV
(`GetReadVersionReply`), and locate replies all flow through the same armed `simConn`. A frame-index-
blind intercept would eventually rewrite a `CommitID`/`GetReadVersionReply` frame into a read-shaped
inline-error body → `parseCommitReply`/`parseGetReadVersionReply` decode garbage → a *falsely-committed
swap* or corrupt read version → a broken ring for a reason that is **not** a client bug (a flake).

So the fault is scoped by **reply content**, not address: `everyNthInlineReadError` reads the fileID
from `body[4:8]` (FDB flatbuffer header, `writer_direct.go:178`) and faults **only** read replies.
**Important wire detail (found during implementation):** a load-balanced read reply travels as
`ErrorOr<T>`, and the real server stamps the **composed envelope** fileID `(2<<24)|T_fileID` (C++
`flow.h:137` `class ErrorOr : ComposedIdentifier<T,2>`; `FileIdentifier.h:79`
`file_identifier = (B<<24) | FileIdentifierFor<T>`), **not** `T`'s own fileID — verified empirically
(a real GetValueReply read arrives as `0x2150A71 = (2<<24)|GetValueReplyFileID`). (The harness's
`MarshalErrorOr*` stamps the inner fileID as a placeholder — `erroror.go:295`, "NOT the per-RPC
fileID the real server would send"; the client tolerates it because `ReadErrorOrInto` does not
validate the fileID.) So the discriminator matches the **composed** read envelopes
`(2<<24)|{GetValueReplyFileID, GetKeyReplyFileID, GetKeyValuesReplyFileID}`, passing commit
(`CommitID`), GRV (`GetReadVersionReply`), locate, and anything else through **verbatim** (each is a
distinct composed envelope, disjoint from the read set). The
`GetValueReply`-shaped inline error (`MarshalErrorOrInlineError`) is correctly parsed by *all three*
read parsers (C4 pins this across getValue/getKey/getKeyValues — the inline `LoadBalancedReply.error`
field is at a shared offset), so a single inline-error shape covers all read replies. `armAll()` is
used (not `armAddr`) precisely because content-discrimination — not address — is the scoping
mechanism; arming everything and faulting only read-reply bodies is the honest model.

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
3. **`TestEveryNthInlineReadError` — MANDATORY unit test (no FDB), the determinism floor.** Pins the
   intercept itself: feed it a sequence of crafted frames and assert (a) every `n`-th **read-reply**
   body is rewritten to the inline error, the other `n-1` pass verbatim; (b) **a `CommitID` frame and a
   `GetReadVersionReply` frame pass through UNTOUCHED** (the control-plane-passthrough dimension the
   existing suite never probes, because those tests pre-warm GRV/cache before arming) — this is the
   regression that would have caught the review NAK; (c) PINGs are never counted; (d) the `injected`
   counter advances only on actually-faulted read replies. Without this, `injected > 0` could pass for
   the wrong reason (e.g. an off-by-one faulting a commit frame).

The teeth are item 1's `injected > 0 && check == nil`: a serializability oracle under fault is only
meaningful if (a) the fault demonstrably fired and (b) the ring still checks out. A pass with
`injected == 0` would be a fake checkbox (faulted nothing).

## 5. Wire-compat impact

**None.** Test-only; the injected bytes are a faithful `ErrorOr<reply>` inline error
(`types.MarshalErrorOrInlineError`, already wire-validated by C4/RFC-118), delivered on a real reply
frame — it exercises the client's *decode + classify* of a shape a real storage server emits. No
production code changes; differential-vs-libfdb_c gates N/A.

## 6. Risks (the no-flakes hard line)

- **Control-plane corruption (the review NAK) — fixed by content-discrimination (§3.2).** The single-
  process container multiplexes storage/commit/GRV on one conn, so the fault MUST be scoped by reply
  content (fileID), never by address. `everyNthInlineReadError` faults only `GetValue/GetKey/
  GetKeyValues` reply bodies and passes `CommitID`/`GetReadVersionReply`/locate verbatim. Pinned by the
  mandatory `TestEveryNthInlineReadError` unit test (commit/GRV passthrough is an explicit case).
- **Wedge / livelock.** Mitigated by the fraction model (§3.1): the per-attempt clean-read probability
  is bounded below by a positive constant and every fault is fixed-delay retryable ⇒ commits infinitely
  often in-window; `workCtx` bounds it; the final check runs disarmed (fault-free). No `time.Sleep`, no
  rate assertion — assertions are pure counters + the ring walk. **N=4 is pinned for proven headroom,
  not tuned to green** (raising N to dodge a near-zero committed count is forbidden — that hides a real
  retry bug).
- **`future_version` retry is fixed-delay, not stacking.** The read-surfaced 1009 → `onError` path uses
  a constant `futureVersionDelay` (`transaction.go:1808`), so retries do not compound into a latency
  blowup that could starve the window. (1037 is the same path; 1001 is deferred — different mechanism.)
- **Connection pool re-dial mid-window.** `simDialer.dial` arms any conn dialed to an armed addr
  (`armAll` arms all), and `injected` is shared-atomic across conns, so a re-dial mid-window keeps
  faulting and counting correctly — no fresh unarmed conn escapes the fault.

## 7. Follow-ups

- **`process_behind (1037)`** — same fixed-delay path as 1009; a one-line table row (this PR if clean,
  else next).
- **`wrong_shard (1001)` under load — its own increment.** The relocate + cache-invalidate recovery is
  a distinct failure mode (a buggy resume could drop/dup), deserving its own ring-survival assertion.
- **Commit-side faults (`not_committed` / `commit_unknown_result` inline on the `CommitID` reply).**
  Sim2 faults the commit path too (Cycle's `commitFailedRetries` counter, `Cycle.actor.cpp:203-204`,
  exists for exactly this); this read-only increment is a strict subset of Sim2's perturbation. The
  content-aware intercept (§3.2) is the prerequisite — it already discriminates `CommitID`, so the
  commit-fault variant just inverts the filter. Natural next increment after 1001.
- **`dropAll`-pulsed reply *loss*** (not just inline error — exercises the read-reply *timeout* re-send
  path under load) if it can be made wedge-free.
- The remaining C3 workloads (AtomicOps / ConflictRange / Serializability / FuzzApi gaps), each its own
  increment.
