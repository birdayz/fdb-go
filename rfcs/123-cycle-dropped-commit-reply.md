# RFC-123 — Cycle under a dropped commit reply (commit_unknown_result)

**Status:** Implemented (PR #321)
**Item:** TODO.md C3 ("Ride their test designs"), increment 4 — RFC-120 §7's commit-side-fault

> **Reviews.** FDB-C-dev **ACK** — DROP→1021 is the faithful model (1021 is client-minted from an
> ambiguous RPC: C++ `LoadBalance.actor.h:344/369` `request_maybe_delivered` → `tryCommit:6773`; the
> proxy never `sendError(commit_unknown_result)`); rejecting synthetic 1020 is correct (the apply/reject
> decision is upstream of the reply — no faithful reply-rewrite to 1020 exists; real 1020s already flood
> the workload); recovery preserves serializability (retry re-runs fn = fresh transposition, `checkData`
> is count-agnostic). Torvalds **ACK** — no-flakes holds, ring-survival airtight, runner generalization
> sound, 1020-rejection honest. **Both caught the same mandatory correction (folded in):** the draft
> wrongly bounded the dummy loop by `workCtx`; in fact `tx.commit`/`commitDummyTransaction` run under
> `context.WithoutCancel` (`transaction.go:1616`), so the dummy is bounded by a clean reply
> ((n-1)/n per attempt) + the outer ctx, while only the swap actor observes `workCtx`.
> **Impl conditions:** (1) outer ctx ≫ `window + worst-case-commit-tail` (pin in a comment); (2)
> `everyNthCommitReplyDrop` drops ONLY commit replies — GRV/read/locate verbatim — and
> `TestEveryNthCommitReplyDrop` pins GRV-passthrough as the anti-wedge regression; (3) the generalized
> `runCycleFaultPhase` preserves all three asserts (`injected>0`, `committed>0`, 3× disarmed `check`) +
> the error-identity `default` arm; (4) start n=8/window=30s/outer=180s, report observed
> injected/committed in the PR, keep `n` on the fewer-drops side, never tune to green.
follow-up. Builds on RFC-119 (oracle) + RFC-120/122 (the SimTransport fault harness). **Test-only —
zero production/wire impact** → differential-vs-libfdb_c gates N/A.
**Spec:** `fdbserver/workloads/Cycle.actor.cpp` @ 7.3.75 under Sim2 fault injection; Cycle's
`commitFailedRetries` counter (`:203-204`) exists precisely for commit-path faults.

---

## 1. Problem

RFC-120/122 fault the **read** path (inline `future_version`/`process_behind`/`wrong_shard` on storage
read replies) and prove the Cycle ring survives. The commit path is unexercised by injection. RFC-120
§7 deferred it: "Commit-side faults … Sim2 faults the commit path too (Cycle's `commitFailedRetries`
counter exists for exactly this)."

This increment injects the **faithful** commit-path wire fault — `commit_unknown_result (1021)` — and
asserts the ring stays one Hamiltonian cycle. This is the workload's real commit-recovery teeth: it
drives `commitDummyTransaction` + the `onError(1021)` self-conflicting retry **under concurrent load**,
proving they never corrupt the ring when a commit's outcome is genuinely ambiguous.

## 2. The faithful fault model — a DROPPED commit reply, not an injected error

How a real client gets each commit outcome (verified against the Go commit path + C++):

- **success** → proxy sends `CommitID{version≥0}` (`parseCommitReply`, `commitpath.go:461`).
- **`not_committed (1020)`** → the resolver REJECTED the commit (a real conflict); the proxy sends
  `CommitID{version=-1, conflictingKRIndices}` and the mutations were **never applied**
  (`commitpath.go:469-475`; C++ `CommitProxyServer.actor.cpp:2448-2466`).
- **`commit_unknown_result (1021)`** is **NOT something the proxy sends.** It is the *client's*
  classification of an **ambiguous** commit RPC: a lost/timed-out reply, a connection error, or a proxy
  change (`commitpath.go:71-78,80-88,38,61` — every arm mints `ErrCommitUnknownResult`). The commit may
  or may not have applied; the client cannot tell.

So the faithful SimTransport commit fault is **dropping the commit reply** (`simConn` `drop=true`): the
client's `waitReplyOrProxiesChanged` times out after `DefaultRPCTimeout` (5 s) → `commit_unknown_result`
→ `commitDummyTransaction` → return 1021 → `db.Transact`'s `onError` retries. This is exactly the
Sim2/real scenario (a dropped/delayed commit reply where the commit **did** reach and apply at the
proxy, but the client never learned the outcome).

**Why NOT inject `not_committed (1020)`.** Rewriting a commit reply to the `version=-1` shape would tell
the client "the resolver rejected this" — but the intercept can only rewrite the *reply*, by which time
the proxy has already run the resolver and (if accepted) **applied** the mutations. A synthetic 1020 on
an *applied* commit asserts a state a real cluster never produces (committed-but-told-rejected). And the
1020→retry path is **already exercised**: the concurrent Cycle workload (RFC-119, no faults) produces a
flood of *genuine* resolver-conflict 1020s. So a synthetic 1020 adds an unfaithful scenario and no new
coverage. The faithful, novel commit-path fault is the dropped reply → 1021.

**Why NOT a root-`ErrorOr` 1021 rewrite.** The proxy never `sendError(commit_unknown_result)` — 1021 is
client-minted. Rewriting to a root-1021 error would be synthetic. The drop is the honest model.

## 3. The recovery under test — why the ring survives whether or not the commit applied

A swap that gets 1021 retries via `db.Transact`'s `onError` loop, which **re-runs the swap fn from
scratch**: re-reads the *current* ring at a fresh read version and computes a **fresh valid
transposition** of whatever it read (the RFC-119 §2 invariant). It does NOT replay the old mutations.
So:
- If the dropped-reply commit **did not apply** → the retry just performs the swap normally.
- If it **did apply** → the ring already advanced by one valid transposition; the retry reads that
  advanced ring and applies *another* valid transposition. Two valid transpositions of a Hamiltonian
  cycle is still one Hamiltonian cycle. `committed` under-counts (the applied-but-unknown commit isn't
  counted), but the **ring invariant holds** — which is the only thing `check` asserts.

`commitDummyTransaction` (`commitpath.go:113`) is the synchronization barrier: after 1021 it commits a
dummy that read+write-conflicts the original's first conflict key, so when the dummy commits the
original is provably no longer in-flight at the proxy; then `onError` copies write→read conflicts so the
retry is self-conflicting (if the original applied, the retry conflicts on it → 1020 → retry again →
reads the applied state). This is the idempotency machinery; this test proves it preserves the ring
**under concurrent load with real dropped commits**, not just in isolation.

**Cancellation detail (load-bearing for §4.1's no-wedge argument).** `tx.commit` — and therefore
`commitDummyTransaction` and the dummy's own commits — run under `context.WithoutCancel(ctx)`
(`transaction.go:1616`): the window deadline is **stripped**, so `commitDummyTransaction`'s
`if ctx.Err() != nil` guard (`commitpath.go:143`) and its `backoffSleep` observe the **outer** test ctx
(~180 s), NOT the `workCtx` window. So the dummy barrier is NOT bounded by window-close; it is bounded
by **a clean reply getting through** (every-`n`-th drop ⇒ `P(clean)=(n-1)/n` per attempt ⇒ a bounded
number of dummy retries in expectation) and ultimately by the outer ctx. The **swap-actor loop** is
what observes `workCtx` (`database.go` Transact guards + `OnError` backoff run on the live `workCtx`),
so the actor exits at window close even if a barrier is mid-flight; a detached dummy goroutine then
finishes on its next clean reply against the outer ctx. This is why the outer ctx must exceed
`window + worst-case-commit-tail` with margin (§4.1).

## 4. Proposed Go change (test-only)

In `pkg/fdbgo/client/cycle_workload_test.go`:

1. **Generalize the runner.** RFC-122's `runCycleInlineReadFaultPhase(... code ...)` becomes
   `runCycleFaultPhase(t, ctx, db, sd, w, faultName, makeIntercept func(*atomic.Int64) frameIntercept,
   actors, window)` — the body (arm → run A actors → disarm → assert `injected>0 && committed>0 &&
   check==nil`, all with fresh per-phase counters) is unchanged; only the intercept is parameterized.
   The read-fault tests pass `func(c){ return everyNthInlineReadError(4, code, c) }`.
2. **`everyNthCommitReplyDrop(n, counter)`** — drops every n-th **commit** reply (`isCommitReplyBody`:
   the composed `ErrorOr<CommitID>` envelope fileID `commitReplyEnvelopeFileID`, already defined in
   RFC-120), passing reads / GRV / locate verbatim (content-discrimination, RFC-120 §3.2). `counter`
   advances only on an actual drop.
3. **`TestCycle_SurvivesDroppedCommitReply`** — runs `runCycleFaultPhase` with
   `everyNthCommitReplyDrop`, asserting the ring survives.
4. **`TestEveryNthCommitReplyDrop`** (no FDB, the determinism floor) — feed crafted frames; assert every
   n-th **commit** reply is dropped, **read and GRV replies pass verbatim** (the content-discrimination
   regression — a misfilter that dropped reads would wedge), runts pass, and `counter` advances only on
   an actual commit drop.

### 4.1 Fault rate + window — flake-free under the 5 s drop cost

Unlike a read fault (re-read in ~ms), a dropped commit costs `DefaultRPCTimeout` (5 s) before the client
gives up → 1021, plus the `commitDummyTransaction` barrier (its own commits, some of which may also be
dropped — it retries with backoff). So commit-fault throughput is far lower than read-fault throughput.
**Progress is still guaranteed and flake-free**, by these mechanisms (corrected from a draft that wrongly
attributed the dummy bound to `workCtx`):
- **Every drop is recoverable** — 1021 is `onErrorRetryable` (`commitpath.go:237`), and a window close
  surfaces to the swap actor as a raw `context` error, never a terminal `transaction_timed_out` (no
  per-tx timeout is set).
- **The swap-actor loop is `workCtx`-bounded** (Transact guards + `OnError` backoff run on the live
  `workCtx`), so each actor exits promptly at window close.
- **The `commitDummyTransaction` barrier is NOT `workCtx`-bounded** — it runs under
  `context.WithoutCancel` (`transaction.go:1616`, see §3), so it terminates by a **clean reply**
  (`P(clean)=(n-1)/n` per attempt ⇒ bounded dummy retries in expectation), backstopped by the outer ctx.
- **The dummy's GRV/reads must pass through.** `commitDummyTransaction` → `dummy.Commit` →
  `ensureReadVersion` issues a GRV; `everyNthCommitReplyDrop` drops **only commit replies**
  (content-discrimination, §4 / RFC-120 §3.2), so the dummy's GRV always completes and the barrier
  cannot wedge on a GRV stall. `TestEveryNthCommitReplyDrop`'s **GRV-passthrough case is the anti-wedge
  regression**, not optional polish.

**Outer-ctx margin (a hard requirement).** Because the commit/barrier tail runs on the outer ctx, the
test's outer ctx MUST exceed `window + worst-case-commit-tail` with comfortable margin (mirroring
RFC-120/122's 120–180 s outer vs 20 s window). A comment pins this; shrinking the outer ctx toward
`window + ε` would wedge the tail into a real `db.ctx` deadline and flake.

**Pinned values.** Start `n = 8`, `window = 30 s`, `actors = 16`, outer ctx 180 s. Over the window
`committed` lands in the dozens–hundreds (orders above the `>0` floor) and `injected` (drops) in the
tens. **`n` and `window` are pinned to the empirically-observed headroom (documented in the test), NOT
tuned to green** — a *higher* `n` (fewer 5 s drops) is the safe direction; shrinking `n` toward 1 or
stretching `window` to scrape a passing `committed` is forbidden. If `committed` ever approached 0 that
is a commit-recovery bug to root-cause. The implementation reports the observed injected/committed and
the committed PR records them.

## 5. Executable spec — what it proves

1. **`TestCycle_SurvivesDroppedCommitReply`** (real FDB via SimTransport): every n-th commit reply
   dropped; after disarm + quiescence assert `injected > 0` (the fault fired — anti-vacuity),
   `committed > 0` (the workload recovered *through* dropped commits), `check == nil` (the ring is still
   exactly one Hamiltonian cycle despite the ambiguous-commit retries). A swap-actor `default: t.Errorf`
   arm fires if the client ever surfaced a non-retryable error from a dropped commit (a real bug).
2. **`TestEveryNthCommitReplyDrop`** (no FDB): the intercept determinism floor — commit-reply dropped on
   the n-th, read/GRV verbatim, counter accuracy. The control-plane-discrimination regression (a
   misfilter that dropped a read/GRV would wedge the workload).
3. **Control:** RFC-119/120/122 tests (no commit faults) remain the baselines.

## 6. Wire-compat impact

**None.** Test-only; the fault is a *dropped* reply (no bytes injected), the most faithful possible model
of a lost commit reply. No production code changes.

## 7. Follow-ups (unchanged from RFC-120 §7)

- The remaining C3 workloads (AtomicOps / ConflictRange / Serializability / FuzzApi gaps), each its own
  increment.
- `dropAll`-pulsed read-reply *loss* (the read-reply timeout re-send path under load), if wedge-free —
  distinct from this commit-reply drop.
