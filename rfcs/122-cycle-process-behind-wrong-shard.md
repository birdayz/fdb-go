# RFC-122 — Cycle under injected process_behind + wrong_shard faults (SimTransport)

**Status:** Draft
**Item:** TODO.md C3 ("Ride their test designs"), increment 3 — RFC-120 §7's first two follow-ups
(`process_behind (1037)` row + `wrong_shard (1001)` under load, its own increment).
**Spec:** `fdbserver/workloads/Cycle.actor.cpp` @ 7.3.75 under Sim2 fault injection; SimTransport
(RFC-118) is our wire-level analog. Builds directly on RFC-119 (the oracle) + RFC-120 (the
read-fault harness). **Test-only — zero production/wire impact** → differential-vs-libfdb_c gates N/A.

---

## 1. Problem

RFC-120 landed `TestCycle_SurvivesInjectedReadFaults`: the Cycle ring survives `future_version (1009)`
injected on every 4th storage read, proving the client's classify→`onError`→reset→re-read loop
absorbs a retryable read fault under concurrent load without breaking serializability. It explicitly
deferred two faults (RFC-120 §7):

- **`process_behind (1037)`** — "same fixed-delay path as 1009; a one-line table row."
- **`wrong_shard (1001)` under load — its own increment.** A *different* recovery mechanism
  (invalidate location cache → re-locate → re-read), where "a buggy resume could drop/dup," deserving
  "its own ring-survival assertion," not folded into the 1009 test as "just another retryable inline."

This RFC closes both. The injection shape is identical (a faithful inline `ErrorOr<reply>` read error
via `everyNthInlineReadError`, already wire-validated by RFC-118/C4) — only the **client recovery path
under test** differs, which is exactly the point: 1009/1037 exercise the fixed-delay re-read;
1001 exercises relocate+invalidate.

## 2. The C++ design (cited)

`cycleClient` (`Cycle.actor.cpp:153-211`) retries **every** error via `wait(tr.onError(e))` (`:200-205`)
— Sim2 injects `future_version`/`process_behind`/`wrong_shard`/`transaction_too_old` indiscriminately,
and the single-cycle invariant must hold after recovery because each attempt **re-reads the current
ring** and computes a fresh valid transposition. 1037 and 1001 are part of the same Sim2 fault set the
workload already runs under; this RFC adds them to our SimTransport analog. (Identical framing to
RFC-120 §2 — the Go `db.Transact` retry loop IS `onError`, and `swapOnce` re-reads inside `fn` every
attempt.)

## 3. Proposed Go change (test-only)

In `pkg/fdbgo/client/cycle_workload_test.go`, extract the RFC-120 test body into a shared phase runner
`runCycleInlineReadFaultPhase(t, ctx, db, sd, w, code, codeName, actors, window)` — arms
`everyNthInlineReadError(4, code, &injected)` + `armAll()`, runs `actors` swap actors for `window`,
disarms, and asserts (all non-timing-dependent): `injected > 0`, `committed > 0`,
`w.check(ctx, db) == nil`. (No new fault primitive — `everyNthInlineReadError` already takes the code.)

- **`TestCycle_SurvivesInjectedReadFaults`** (extends the RFC-120 test): runs the runner for **1009
  then 1037 sequentially on one ring/container** — they drive the identical fixed-delay path
  (`isFutureVersionOrProcessBehind`, `readpath.go:930` → constant `futureVersionDelay`), so 1037 is the
  promised "one-line table row." Each phase starts from the valid ring the previous phase's disarmed
  `check` confirmed.
- **`TestCycle_SurvivesInjectedWrongShard`** (new, own container + own assertion): runs the runner for
  **1001**. This is the "own increment" — a distinct recovery path with its own ring-survival proof.

### 3.1 Why 1001 is flake-free (the no-flakes hard line)

`wrong_shard (1001)` on a read drives `getValueImpl`'s bounded relocate loop
(`readpath.go:401-449`): `isWrongShardServer` → `locCache.invalidate(key)` → re-locate → re-read, up
to `MaxWrongShardRetries`. **The exhaustion path returns a RETRYABLE `transaction_too_old (1007)`**
(`readpath.go:444-449`, matching libfdb_c never propagating `all_alternatives_failed` to the app), NOT
a terminal error. So under sustained 1/4 injection:
- a single read either recovers within the budget (re-locate hits the same single-shard container and
  the re-read lands clean — `P(clean) = 3/4` per read), or
- exhausts the budget → retryable 1007 → `db.Transact`'s `onError` retries the whole swap.

**Either way no non-retryable error ever surfaces to the swap actor**, so the actor's
`default: t.Errorf` arm (which fires only on a non-context error from `Transact`) cannot trip
spuriously. Progress is guaranteed by the same RFC-120 §3.1 argument: `P(3 clean reads) ≈ (3/4)³ ≈ 42%`
per attempt, fixed-delay retries, 16 actors × 20 s ⇒ `committed` lands in the thousands (orders above
the `> 0` floor). **N=4 stays pinned for that proven headroom — not a knob to tune to green.**

**No drop/dup for the cycle reads.** The drop/dup hazard RFC-120 §7 flags for 1001 is a *range-scan*
mid-continuation concern (pinned separately by C4's `TestSimRangeWrongShardMidScan`). Cycle reads are
**single-key** `Get`s — relocate → re-read the same key → the correct value; there is no continuation
to resume, so no drop/dup surface. The ring-survival assertion is the proof the relocate path returns
the right value under load.

### 3.2 Control-plane scoping unchanged (RFC-120 §3.2)

`everyNthInlineReadError` already faults **only** read-reply envelopes (`isReadReplyBody`, the composed
`ErrorOr<T>` fileIDs) and passes commit/GRV/**locate** verbatim. This is load-bearing for 1001: the
relocate fires a `GetKeyServerLocations` RPC whose reply must pass through untouched for the re-locate
to succeed — it does, because a locate reply is not in the read envelope set. No change needed.

## 4. Executable spec — what it proves

1. **`TestCycle_SurvivesInjectedReadFaults`** (1009 + 1037 phases): each phase asserts `injected > 0`
   (fault fired — anti-vacuity), `committed > 0` (recovered through it), `check == nil` (ring intact).
   1037 proves the process_behind row drives the same fixed-delay recovery as 1009.
2. **`TestCycle_SurvivesInjectedWrongShard`** (1001): same three assertions — but the `check == nil`
   here is the **relocate+invalidate** ring-survival proof (the distinct mechanism). A buggy relocate
   that dropped/dup'd or mis-classified 1001 as terminal → broken ring or a surfaced non-retryable
   error → loud failure.
3. **Control:** RFC-119's `TestCycle_SerializableUnderConcurrency` (no faults) + RFC-120's 1009 phase
   remain the baselines; the delta is the added fault codes.
4. The RFC-120 `TestEveryNthInlineReadError` unit test (determinism floor: every n-th read faulted,
   commit/GRV pass verbatim) already covers the intercept for any `code` — 1037/1001 reuse it unchanged.

## 5. Wire-compat impact

**None.** Test-only; the injected bytes are the same faithful `ErrorOr<reply>` inline error
(`types.MarshalErrorOrInlineError`) RFC-118/120 already validated, with a different error code. No
production code changes.

## 6. Risks

- **Flake under sustained 1001 → addressed in §3.1** (exhaustion is retryable, progress guaranteed,
  no terminal error reaches the actor).
- **Wedge/livelock → same fraction model as RFC-120 §3.1** (bounded-below clean probability,
  fixed-delay retries, `workCtx` bounds the window, final check disarmed).
- **Test duplication** — avoided by the shared `runCycleInlineReadFaultPhase` runner (one body, three
  phases across two tests), not three copy-pasted 70-line tests.

## 7. Follow-ups (unchanged from RFC-120 §7)

- **Commit-side faults** (`not_committed`/`commit_unknown_result` inline on the `CommitID` reply) —
  the content-aware intercept already discriminates `CommitID`; the commit-fault variant inverts the
  filter. Natural next increment.
- **`dropAll`-pulsed reply *loss*** (read-reply timeout re-send path under load), if wedge-free.
- The remaining C3 workloads (AtomicOps / ConflictRange / Serializability / FuzzApi gaps).
