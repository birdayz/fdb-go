# FDB client (pkg/fdbgo) bug-hunt — handover

Branch: `hunt/fdbgo-client-bughunt` (off `worktree-bughunt-2`).
Method: differential vs **libfdb_c 7.3.77** (C++ source at `/tmp/fdbsrc`, the spec). Two
multi-agent discovery workflows (22 axes total) → adversarial refute-verify → DFS-fix with
red→green proof. C++ is the spec; the FDB-C-dev + Torvalds + codex-review gauntlet is owed
before any PR/merge (see "Review owed").

Interrupted by a **session usage limit (resets 7:10pm Europe/Berlin)** mid-second-workflow.
13 of 22 axes still need a clean run. **Nothing here is merged.**

## Done this session (committed, red→green proven)

One commit, three independent libfdb_c divergences (all pinned, full pre-commit suite green):

1. **getKey rywDisabled result not clamped to maxReadKey** (HIGH, system-key leak).
   `transaction.go` GetKey: a `SetReadYourWritesDisable` GetKey returned storage's raw
   resolution; a forward selector off the end of the user keyspace resolved into `\xff/...`
   and leaked it. libfdb_c clamps the *returned* key to `getMaxReadKey` (ReadYourWrites.actor.cpp:182-183).
   Clamp the rywDisabled return AFTER the conflict-range add (conflict range stays on the
   unclamped key, matching NativeAPI getKeyAndConflictRange:5767).
   Pin: `bench/TestDifferential_GetKeyBoundaryRYWDisabled` (pre-fix go=`\xff\x02/fdbClientInfo/migrated/`, cgo=`\xff`).

2. **RYW getRange more=false at exactly-limit** (MEDIUM; also a spurious-1020 over-conflict).
   FDB forces `more = more || limits.isReached()` (ReadYourWrites.actor.cpp:799). Go derived
   `more` from residual-data presence → slow path (`ryw.go:620`) + cached fast path (`ryw.go:554`)
   returned `more=false` when the result exactly filled the limit. `rangeConflictExtent`
   (transaction.go:1213) keys off `more`, so the false `more=false` widened the read-conflict to
   the full `[begin,end)` → a concurrent write in the unread tail spuriously aborts (1020).
   Added `limitReached()`; pin: `client/TestGetRange_MoreOnExactlyLimit` (slow + cached + control).

3. **Atomic() accepted non-atomic op-codes** (MEDIUM, silent data deletion).
   C++ atomicOp throws `invalid_mutation_type` (2018) for any op outside ATOMIC_MASK
   (ReadYourWrites.actor.cpp:2234); libfdb_c's CATCH_AND_DIE aborts. Go buffered+committed the
   raw byte — most dangerously `Atomic(MutClearRange,...)` (deletes a range). Added `isAtomicOp`
   mask gate in `Atomic()` that rejects eagerly WITHOUT buffering, surfaced as deferred 2018 at
   Commit (`invalidAtomicOpErr`, cleared on Reset). Pin: `client/TestAtomic_RejectsNonAtomicOpCode`
   (ClearRange rejected + a,b survive + reset-reuse).
   - Fallout (fixed): `ryw_adversarial_test.go TestRYWAtomic_ChainedOps/Set_then_Add` drove its
     chain via `Atomic(MutSetValue,...)` to model a "Set" — now correctly rejected. Adapted to use
     the real `tx.Set` (same RYW fold, Site B); verified no PRODUCTION caller passes a non-atomic op
     (the fdb facade maps every method to a valid atomic op; stacktester forwards conformant codes).
   - **For review (Torvalds):** `applyAtomic`'s `case MutSetValue` (ryw.go:1201-1202) is now DEAD —
     it was only reachable via `Atomic(MutSetValue)`. Left in as defensive code; candidate for removal.

## Done — round 2 (committed, red→green proven)

4. **Watch goroutine vs Cancel/Reset data race + lost-cancellation leak** (HIGH). `watchCtx`/
   `watchCancel` accessed unsynchronized while the async WatchPoll fetched the context lazily in
   the goroutine (racing Cancel/reset; if cancelWatches won, the poll minted a never-cancelled
   context → leak). Fix: bind the watch context SYNCHRONOUSLY in WatchSetup (like readVersion/span)
   and thread it through WatchPoll, so cancelWatches always cancels the ctx the poll holds; guard
   both fields with `watchMu`. Pin: `TestWatch_GetWatchCtxCancelRaceFree` under `-race` (red→green:
   WARNING: DATA RACE pre-fix). Touches the fdb facade Watch signature.
5. **AddReadConflictRange/Key skip the RYW write-map filter** (MEDIUM, over-conflict → spurious
   1020). Both APIs now route through `conflictRangesLocked` / `addReadConflictForKeyRYW` when RYW
   is enabled (C++ updateConflictMap, ReadYourWrites.actor.cpp:1986); rywDisabled adds directly
   (:1979). Pin: `TestAddReadConflict_FiltersSelfWrite` (white-box, red→green).

## Done — round 3 (committed, red→green proven)

6. **OnError backoff not bounded by the SetTimeout deadline** (MEDIUM). A timed-out txn slept a
   full (growing) backoff and did one extra reset+retry before surfacing 1031. Fix: (a) an entry
   gate `checkTimeout()` in OnError (C++ RYWImpl::onError :1506 throws timed_out at entry); (b)
   `backoffSleepBounded` caps each backoff at the deadline → 1031 (C++ :1517 timebomb race). Pin:
   `TestOnError_RespectsTimeoutDeadline` + `TestBackoffSleepBounded_CapsAtDeadline` (red→green).
7. **Iterator() returns empty for Limit=-1 (ROW_LIMIT_UNLIMITED) and Limit<=-2** (MEDIUM, facade).
   `-1` is unlimited (return all), `<=-2` is range_limits_invalid (2012). Fix: `effectiveLimit`
   maps `-1`→MaxInt; `Iterator()` rejects `<-1` with 2012 (matching GetSlice/client + libfdb_c).
   Pin: `TestRangeIterator_RowLimitUnlimitedAndInvalid` (red→green; pre-fix Iterator(-1)→0 rows).

Also written: **RFC-170** (watch-at-committed-version design, Draft — needs FDB-C-dev ACK). (Renumbered
from 165 → 170 after merging master, whose 165–167 were taken by query-layer RFCs.)

## Done — round 4 (committed, red→green proven)

8. **Hedged read leaks the primary's QueueModel `startRequest` delta on `ctx.Done()`** (MEDIUM,
   two finders confirmed). The top-level `ctx.Done()` branch of `sendFrameWithHedge` returned a bare
   `{err}` (no addr/delta), so the caller's `if result.addr != ""` skipped `endRequest` → permanent
   LB skew. Fix: return the primary's accounting like `waitForReply`'s `ctx.Done`. Pin:
   `TestHedge_ContextCancellation_AccountsPrimary` (red→green; needs a non-nil secondary to reach
   the buggy branch — the existing test passed a nil secondary, hence the gap).
9. **`Watch()` skips legal-range + key-size validation** (MEDIUM). A normal (non-system) txn could
   register a watch on a `\xff` system key (C++ 2004) or an oversized key (2102). Fix: `WatchSetup`
   applies the same maxReadKey/key-size gates as Get, BEFORE the read (C++ RYW watch,
   ReadYourWrites.actor.cpp:2450-2456). Pin: `TestWatchSetup_RejectsSystemAndOversizedKeys`.

Also written: **RFC-171** (reset() must clear non-persistent options — closes the `txn-options-lifecycle`
HIGH + `snapshot-ryw` MEDIUM findings; Draft, needs FDB-C-dev ACK).

The 2026-06-30 clean discovery re-run also CONFIRMED (still open, in the PR table): Reset() option
preservation (HIGH, RFC-171), buffer-pool `sync.Pool` race on SendFrame error (LOW), SYSTEM_IMMEDIATE
+GRV-cache (LOW), atomic op-code precedence (LOW — the edge flagged in the round-1 atomic fix),
oversized system-key Clear silently dropped (LOW). 3 candidates were REFUTED by the adversarial verify.

## Done — round 5 (committed, red→green proven)

10. **`too_many_watches` (1032) enforcement** (MEDIUM). `SetMaxWatches` was a no-op and no
    outstanding-watch counter existed, so a Go app could register unlimited pending watches.
    Added a per-Database `outstandingWatches`/`maxWatches` counter (default 10000 =
    DEFAULT_MAX_OUTSTANDING_WATCHES); `WatchPoll` acquires a slot (1032 over the cap) and releases
    on every exit path; `Database.SetMaxWatches` + the facade `DatabaseOptions.SetMaxWatches` are
    wired. C++ increaseWatchCounter (NativeAPI.actor.cpp:5694,2175). Pin:
    `TestDatabase_OutstandingWatchLimit`.
11. **`StreamingModeExact` + no row limit → `exact_mode_without_limits` (2210)** (LOW, facade). The
    explicit-Exact Iterator returned all rows; libfdb_c rejects EXACT with no limit/byte-target
    (fdb_c.cpp:996-998). Fix: `Iterator()` guard. Pin:
    `TestRangeIterator_RowLimitUnlimitedAndInvalid` (Exact sub-cases).

## Done — round 6 (committed, red→green proven)

12. **Atomic invalid op-code precedence** (LOW). The round-1 fix poisoned with invalid_mutation_type
    (2018) eagerly, preempting C++'s legal-range/metadataVersionKey checks. C++ atomicOp order
    (ReadYourWrites.actor.cpp:2226-2234): metadataVersionKey (2000) / legal-range (2004) BEFORE
    op-validity (2018). Fix: Atomic() sets the poison matching that precedence. Pin:
    `TestAtomic_InvalidOpCodePrecedence`.
13. **Oversized system-key Clear silently dropped** (LOW). `Clear()` size-clamped (dropped) an
    oversized key BEFORE the legal-range check, so an oversized `\xff` system key was swallowed
    instead of `key_outside_legal_range` (2004). C++ checks legal-range first (RYW:2419-2424). Fix:
    only size-drop a key WITHIN the legal range; an illegal one is buffered → commit reports 2004.
    Pin: `TestClear_OversizedSystemKey` (key must exceed SYSTEM_KEY_SIZE_LIMIT=30000 to exercise it).

Also written: **RFC-169** (getKey isBackward shard-location, Draft — needs multi-SS/SimTransport proof).

## Final disposition of the remaining low-value findings (engineering judgment)

- **#19 commitDummyTransaction jitter ±10% vs C++ U[0,1) — ACCEPTED (not a bug to fix).** Pure
  internal-timing of the `commit_unknown_result` synchronization barrier; **zero wire/data effect**.
  The Go ±10% jitter (`jitterBackoff`) is a deliberate thundering-herd design (spreads coordinated
  retries). "Wire compat is the hard line; query reach is not" — a non-wire internal timing
  distribution is an acceptable divergence; forcing the C++ law would churn 3 funcs + 2 tests for no
  observable benefit. Leave as-is.
- **#16 SYSTEM_IMMEDIATE + USE_GRV_CACHE — NEEDS FDB-C-DEV ADJUDICATION, do not rush.** Go
  INTENTIONALLY makes IMMEDIATE bypass the GRV cache (`grv.go` "SYSTEM_IMMEDIATE must always contact
  proxy", documented). The finder says C++ NativeAPI:7484/7504 serves cached for IMMEDIATE+useGrvCache.
  Don't "fix" a documented intentional deviation without the FDB-C-dev confirming C++ is right here.
- **#21 api<520 versionstamp suffix — REAL wire divergence but ~zero blast radius** (only API 13-519,
  FDB < 6.0). Recipe: in `Atomic()`, for SVK/SVV when `apiVersion < 520`, append `\x00\x00` (key) /
  `\x00\x00\x00\x00` (value) BEFORE offset parsing (C++ RYW:2251-2261), then the existing offset path
  works. Delicate (threads through versionstampKeyRange + validateVersionstampOffset); test by opening
  at API 510. Focused follow-up — wire-compat hard line says fix it, but no real app runs API<520.
- **#22 sendGetValue fallback error-masking — VERIFIED real but NARROW; fix designed + reverted
  (needs a fragile multi-server test).** Full analysis + recipe under "Findings NOT yet fixed" below.

## #15 buffer-pool race — FIXED (round 23)

**Fixed:** the 5 SendFrame callers that owned a POOLED body and `Put` it back on the SendFrame ERROR
path (commitpath.go commit + readpath.go getKey/getValue×2/getKeyValues) now DROP the buffer on error
instead of Put-ting it — because SendFrame's post-enqueue `ctx.Done` return (conn.go:454) leaves
`writeLoop` still reading `body` in WriteFrame, so returning it to the pool races a concurrent reuse.
The success-path `Put` stays (WriteFrame copied body before errCh fired). The other SendFrame callers
(coordinator/grv/locality/metrics) use fresh `MarshalFDB()` allocations (no pool), and the watch path
Puts nothing on error — so those 5 were the whole exposure. Deterministic pin:
`TestSendFrame_PostEnqueueCtxDone_TransportStillOwnsBody` (transport) proves the enqueued writeReq
still references `body` after the error return (the contract the fix rests on); passes under `-race`.
Conservative: at worst one un-pooled buffer per rare send-error.

### Original recipe (for reference)

`SendFrame` (`transport/conn.go:431`) has TWO return paths: (a) via `errCh` (line 451) — the
writeLoop ran `WriteFrame`, which **copies `body` into `c.wbuf`**, so `body` is safe to reuse
afterwards; (b) via `<-c.ctx.Done()` (line 442/455, returns `errConnClosed`) — the writeLoop **may
still hold `req.body`** (the enqueued slice) and write it AFTER `SendFrame` returns. Callers that
own a POOLED `body` and `Put` it back **on the error path** (`commitpath.go:57`
`marshalBufPool.Put`, and `readpath.go` `sendGetValueToServer`/the `makeSender` closures
`getValueBufPool.Put` etc. on SendFrame error) hand the buffer back to the pool while the writeLoop
may still reference it → a concurrent commit/read draws the same buffer, overwrites it, and the
writeLoop writes corrupted bytes. **Data race**, `-race`-detectable.

**Fix:** on a SendFrame ERROR, do NOT return the pooled buffer to the pool — drop it (GC reclaims it
once the writeLoop's reference, if any, is gone). The SUCCESS-path `Put` stays (body was copied).
Conservative: at worst one un-pooled buffer per (rare) send-error. Sites: `commitpath.go` commit
(error branch) + every `readpath.go` SendFrame caller that `Put`s on error. **Test:** a loopback
fake server + a goroutine that cancels the conn ctx mid-send, run under `-race`, asserting no race
on the pooled backing array. Multi-site + a fake-transport `-race` test → its own focused commit.

## Review gauntlet — RAN + ITERATED (2026-06-30)

All three reviewers ran on `master..HEAD`. **Outcome: ACK on 11/13, converged NAK on one** (the
too_many_watches 0-cap, fixed round 7). On the **delta re-review codex found two MORE real P2s**
(round 8) — the critical-gate value:
1. **OnError entry-gate position (transaction.go:2016):** the timeout gate ran BEFORE the
   `errors.As(*wire.FDBError)` branch, so a non-FDB application error (a `Transact` callback's
   `errors.New(...)`) past the deadline was replaced by 1031. Moved the gate AFTER the non-FDB
   return (FDB errors only). Pin: the non-FDB-escape assertion in `TestOnError_RespectsTimeoutDeadline`
   (red-proven: gate-before → 1031). A non-retryable FDB error past deadline still → 1031.
2. **invalidAtomicOpErr data race (transaction.go):** the fix-#3 poison field was a plain `error`
   written by `Atomic()` (a concurrent-safe data op) and read by `Commit` → race. Converted to
   `atomic.Pointer[wire.FDBError]` (CAS keeps the first invalid op). Pin:
   `TestAtomic_InvalidOpPoison_RaceFree` under `-race`.
   FDB C++ dev re-confirmed the 0-cap fix → **full ACK**; Torvalds' two conditions addressed.

**Round 9 — codex's 2nd `--supersede` re-review found two MORE** (P2 + P3), both fixed red→green:
- **P2 SetMaxWatches out-of-range (options.go/database.go):** clamped a negative to 0, so
  `SetMaxWatches(-1)` "succeeded" then failed every watch with 1032. C++ `extractIntOption(v, 0,
  ABSOLUTE_MAX_WATCHES=1e6)` THROWS `invalid_option_value` (2006) on out-of-range and leaves the cap
  UNCHANGED — it does NOT clamp (NativeAPI:2092-2102; the FDB-C-dev's earlier "clamps" was the
  approximation, codex read the source). `SetMaxWatches` now returns 2006 for `<0`/`>1e6`, cap
  untouched. Pin: `TestSetMaxWatches_RejectsOutOfRange`.
- **P3 invalid-Atomic precedence (transaction.go):** the fix-#3 poison was checked at Commit entry
  before the buffered-mutation loop, so a bad Atomic AFTER a system-key `Set` masked the Set's 2004
  with 2018. C++ throws the FIRST illegal op eagerly — extracted the per-mutation validation into a
  pure `validateMutation`, and the bad-op poison now defers to an earlier illegal buffered mutation.
  Pin: `TestAtomic_InvalidOp_DefersToEarlierIllegalMutation` (Set-before-Atomic → 2004;
  Atomic-before-Set → 2018; red-proven). Extraction verified by the versionstamp-order differential.

**CI flake (a396d8cc): `TestWithKnob_AppliedToProcess`** — pre-existing testcontainers one-shot
`ps aux` knob check raced `configure new`'s recovery restart; hardened to poll `/proc/PID/cmdline`
(the sibling multi-process test was already fixed this way). NOT an fdbgo-code failure (all client
tests were green in CI).

**Round 10 — codex's 3rd `--supersede` re-review found two MORE** (both P2), both addressed:
- **Facade error-type leak (options.go):** `DatabaseOptions.SetMaxWatches` returned the internal
  `*wire.FDBError` for the 2006 reject path instead of a public `fdb.Error` like every sibling
  setter. Wrapped in `convertError`. Pin: `TestSetMaxWatches_FacadeConvertsError`.
- **Poison re-check race (transaction.go):** the invalid-Atomic poison was read (lock-free) at Commit
  ENTRY, before the `conflictMu` mutation snapshot — a concurrent `Atomic(badOp)` that stores the
  poison (under `conflictMu`) AFTER that entry load but BEFORE the snapshot was missed, so the commit
  could succeed despite the invalid atomic. Fix: re-read the poison UNDER the same `conflictMu` as
  the `muts` snapshot, linearizing poison-vs-commit with mutation-vs-commit. **Correct by
  construction** (the re-check and the bad-op Store share `conflictMu`); a deterministic regression
  needs read-barrier-park fault injection (hold a pipelined GetValue reply via the simDialer
  intercept so Commit parks in the barrier past the entry check, inject `Atomic(badOp)`, release,
  assert 2018; revert-prove by removing the re-check). **FOLLOW-UP: write that fault test** — the fix
  is landed + commented, this pins it against a future snapshot refactor dropping the re-check.

**Round 11 — codex's 4th `--supersede` re-review found one more** (P2), fixed red→green:
- **Watch cap charged in the async poll (readpath.go):** `tryAcquireWatch` ran inside the async
  `WatchPoll` goroutine, so two `Watch()` calls under `MAX_WATCHES=1` raced — the first-registered
  watch could lose the slot to the second. C++ `Transaction::watch` charges `increaseWatchCounter`
  SYNCHRONOUSLY at watch() time (NativeAPI:5694), releasing via `decreaseWatchCounter` in the async
  actor (catch on setup error :5679, completion :5683). Moved the acquire to `WatchSetup` (sync,
  registration order, after the malformed-key rejects); release on a post-acquire setup error there
  (matching the C++ catch) and in `WatchPoll`'s defer on the success path (eager future → always
  runs). Pin: `TestWatchSetup_ChargesSlotAtRegistrationOrder` (second setup → 1032 deterministically
  — only satisfiable if WatchSetup charges; pre-fix it returned nil).

**Round 12 — codex's 5th `--supersede` re-review found two MORE** (both P2 concurrent-single-txn
contract edges, both second-order effects of earlier gauntlet fixes), both fixed:
- **Watch-ctx cancellation leak (readpath.go):** round-11 moved the slot acquire to WatchSetup but
  bound `getWatchCtx` AFTER the blocking GRV/value read. A `Cancel()` during that read was missed by
  `cancelWatches` (no watchCancel yet) → WatchPoll polled a fresh never-cancelled ctx and HELD the
  slot. Moved the bind to right after the acquire, BEFORE the read (C++ binds the watch's cancellable
  future at registration); a Cancel during the read now cancels the bound ctx → WatchPoll drains +
  releases. Removed a redundant explicit `checkCancelled` (ensureReadVersion's leading check at :622
  already covers the before-bind case).
- **Non-atomic filtered conflict append (transaction.go):** the round-2 RYW filter splits an explicit
  `AddReadConflictRange` into sub-ranges appended under SEPARATE `conflictMu` acquisitions — a
  concurrent `Commit` could snapshot a prefix and drop the rest of the caller's conflict. Added
  `addReadConflicts` (one lock, all-or-none) and used it in all three filter loops
  (AddReadConflictRange, addGetKeyConflictRange, getRange).

Pinned deterministically: `TestWatchSetup_CancelledTxnDoesNotLeakSlot` (round-11 release-on-cancel).

**CONCURRENCY TEST-DEBT (3 correct-by-construction linearizations needing fault-injection regressions
— a focused follow-up; the fixes are landed + commented, these PIN them against future regressions):**
1. **Poison re-check (round 10, transaction.go Commit snapshot):** ✅ DONE —
   `TestCommit_RechecksInvalidAtomicPoison_SetDuringReadBarrier` (poison_recheck_fault_test.go): drops
   the pipelined barrier read's reply so Commit's Resolve re-drives and parks on the HELD re-send
   reply (past the entry poison check), injects `Atomic(badOp)`, releases → asserts 2018. Deterministic
   (3/3). Revert-proof: without the re-read, Commit succeeds despite the invalid atomic.
2. **Watch-ctx-early (round 12, readpath.go):** ✅ DONE — `TestWatchSetup_CancelDuringValueRead_
   ReleasesSlot` (watch_ctx_fault_test.go): holds the WatchSetup value-read reply via the simDialer
   intercept, Cancel()s mid-read, releases → asserts the slot is freed (2nd watch under cap=1
   succeeds). Revert-proof: with getWatchCtx bound late, the watch long-polls forever holding the
   slot and Watch never drains (the wait times out).
3. **Conflict atomicity (round 12, transaction.go):** CORRECT-BY-CONSTRUCTION, not deterministically
   testable. The race is a pure in-memory `conflictMu` interleave (a Commit snapshot landing BETWEEN
   two sub-range appends) — there is no network park point for the sim intercept, and a `-race` test
   is vacuous (both accesses are already `conflictMu`-serialized, so the linearization is not a memory
   race). The `addReadConflicts` one-lock batch makes the append atomic by construction; forcing a
   partial would require a hot-path conflictMu-interleave hook (a code smell not worth adding for this
   edge). Left as-is: the fix is landed + commented; the two testable items (#1, #2) are pinned.

**Round 13 — codex's 6th `--supersede` re-review found two MORE** (P2 + P3), both on the round-12
watch-ctx change, both fixed red→green (deterministically — no fault injection needed):
- **Stale watchCtx poisons the next watch (P2, readpath.go):** round 12 bound `getWatchCtx` before
  the read, but a setup that FAILS (per-call ctx cancelled/timed-out during GRV/value read) left the
  per-txn `watchCtx` pointing at that cancelled child → a later watch on the same active txn reused
  it → `context.Canceled`. `getWatchCtx` now returns `created`; a failed setup that MINTED the ctx
  clears it (cancelWatches), leaving a pre-existing active watch's ctx alone. Pin:
  `TestWatchSetup_FailedSetupDoesNotPoisonNextWatch` (pre-cancelled per-call ctx → next watch's ctx
  is live).
- **Cap masks cancellation (P3, readpath.go):** the slot acquire ran before ensureReadVersion's
  checkCancelled, so a Cancel()ed txn with a full/0 cap returned 1032 instead of 1025. Added a
  pre-acquire `checkCancelled` (1025 out-ranks the cap). Pin:
  `TestWatchSetup_CancellationOutranksWatchCap`.

**⚠ ARCHITECTURAL FLAG — watch-ctx design:** rounds 11, 12, 13 ALL surfaced edges in the ONE-shared-
`watchCtx`-per-txn design (round 4/7). Each fix is correct, but the shared context is the root
fragility — a per-WATCH cancellable context (with cancelWatches iterating a list) would close the
whole class (failed/cancelled-watch cross-poisoning, concurrent-setup clear races). Deferred as a
focused redesign (risk: it underpins the round-4/7 watch-race fix); flagged for the next watch-area
change. If codex round 14 surfaces another watch-ctx edge, do the redesign.

**Round 14 — codex's 7th `--supersede` re-review found one more** (P2, watch-area again — 4th round):
the slot acquire ran before the caller-ctx cancellation / txn-SetTimeout could be observed, so a
full/0 cap masked the real terminal error (context.Canceled / 1031) with 1032. Added the caller
`ctx.Err()` + `checkTimeout` gates before the acquire (with the round-13 `checkCancelled`), in
mapTimeout precedence. Pin: `TestWatchSetup_TerminalErrorsOutrankCap`.

**⚠ SHARPENED ARCHITECTURAL FLAG — watch-setup structure (rounds 11-14):** the recurring edges split
into two structural fragilities, each with a decisive fix:
- **Acquire ordering (rounds 13, 14):** the cap-charge vs terminal-error ordering keeps producing
  edges. Decisive fix: **acquire LAST** — after ensureReadVersion + the value read — so EVERY terminal
  error (cancel/ctx/timeout/read-failure) surfaces before the cap is touched and a doomed setup never
  transiently holds a slot. Removes the pre-acquire gate duplication. Minor divergence: the cap then
  counts setup-COMPLETE watches, not in-setup ones (client-side limit, not wire — acceptable).
- **Shared watchCtx (round 13):** one ctx per txn → failed/cancelled-watch cross-poisoning. Decisive
  fix: **per-watch cancellable context** (cancelWatches iterates a list; each watch owns its cancel).
If codex round 15 surfaces ANOTHER watch edge, do BOTH restructures together (one reviewed change) —
they're the root, and incremental patching (4 rounds) is not converging on this area.

**Round 15 — codex's 8th `--supersede` re-review found one more** (P3, NOT watch — the rounds-11–14
watch patches HELD): the invalid-atomic poison's Commit-ENTRY early return left `tx.state` active,
while the round-10 snapshot re-check marks it `txStateErrored` — so a manual caller (not routing
through OnError) could keep issuing ops after a failed `Atomic(badOp);Commit()` depending on timing.
Added `tx.state.Store(txStateErrored)` to the entry check (rywPoisonErr deliberately NOT changed — it
is a per-op 2000 poison, and erroring it would turn subsequent ops into "not active" instead of
2000). Pin: `TestCommit_InvalidAtomicMarksErrored`.

**Round 16 — codex's 9th `--supersede` re-review found two MORE** (both P2, watch-area again — so the
whack-a-mole is NOT converged; round 15 was just a one-round detour), both fixed red→green:
- **Terminal state vs key validation (readpath.go):** the terminal checks (cancelled/ctx/timeout,
  rounds 13-14) ran AFTER the legal-range/key-size validation, so `Cancel();WatchSetup(illegalKey)`
  returned 2004 instead of 1025. Moved them to the TOP of WatchSetup (C++ entry-timebomb precedence).
  Pin: `TestWatchSetup_CancellationOutranksKeyValidation`.
- **Watch slot leak on terminal abort (transaction.go) — A REAL BUG, not just ordering:** a watch
  registered in Transact whose txn then fails non-retryably → OnError returned WITHOUT
  reset/cancelWatches, so the long-poll kept the acquired slot until the key changed; under
  MAX_WATCHES=1 one failed txn starved all future watches. Added a `defer` in OnError that
  cancelWatches on any non-nil (abort) return (the retry path already does via reset). Pin:
  `TestOnError_TerminalAbortCancelsWatches`.

**⚠ WATCH-SETUP RESTRUCTURE IS NOW OVERDUE** — rounds 11, 12, 13, 14, 16 (five) all watch edges. The
incremental patching is genuinely not converging on this area. NEXT watch finding → STOP patching and
do the comprehensive restructure in ONE reviewed change: (1) terminal checks first, (2) validation,
(3) per-watch cancellable context (not one-shared), (4) reads, (5) acquire LAST (after reads), (6)
cancel-this-watch on setup error, (7) OnError/abort cancels all. Closes every edge class at once.

**Round 17 — codex's 10th `--supersede` re-review found two MORE** (a watch one — 6th round — and a
non-watch one), both fixed red→green:
- **Watch future Cancel() was a no-op (fdb/transaction.go) — 6th watch round:** the facade Watch
  returned `newFutureNil`, whose Cancel() is a base no-op, so an app freeing an unneeded watch by
  cancelling its future never cancelled watchCtx / reached releaseWatch → the cap kept counting it.
  Added `newFutureNilCancel` (a FutureNil with a Cancel hook) + exported `client.Transaction.
  CancelWatches`, and wired Watch's future Cancel → CancelWatches. Pin:
  `TestNewFutureNilCancel_CancelRunsHook`. LIMITATION: watchCtx is per-txn shared, so this cancels
  ALL the txn's watches — the per-watch-context restructure (below) scopes it to the one future.
- **OnError caller-cancel vs txn-timeout (transaction.go):** the round-8 checkTimeout gate returned
  1031 before observing a done caller ctx, so a TransactCtx caller with BOTH deadlines expired got
  1031 instead of their context.Canceled. Added the ctx.Err() check inside the gate (mapTimeout
  precedence). Pin: `TestOnError_CallerCancelOutranksTxnTimeout`.

**Codex caught 18 real issues across 11 review rounds the persona reviewers missed** — critical-gate
value, fully borne out. **The watch area (rounds 11-14, 16, 17 = SIX) needs the restructure now.**

**Round 18 — codex's 11th re-review found the multi-watch over-cancel** (P2, 7th watch round): the
round-17 future `Cancel()` → `CancelWatches` (txn-wide) cancels UNRELATED watches; codex requires
per-watch cancellation. This is the exact limitation documented on the round-17 fix. **There is no
minimal patch — round 18 REQUIRES the per-watch-context restructure.**

**UPDATE — the restructure LANDED (commit `6a76e4d70`, RFC-168 → status IMPLEMENTED).** The per-watch
context restructure: `watchCtx`/`watchCancel` → `watchCancels map[uint64]context.CancelFunc`;
`getWatchCtx` → `newWatchCtx` (returns ctx + a SCOPED cancel); `WatchSetup` returns the scoped cancel
(6th value), threaded to `WatchPoll` (deferred self-cleaning deregister) + the fdb facade (the future's
`Cancel()` scopes to ONE watch, not txn-wide). Closes round-13 poisoning, round-17 future-Cancel, and
round-18 over-cancel at once. Verified: `TestNewWatchCtx_PerWatchScoped` (cancel one, sibling survives)
+ `TestWatch_NewWatchCtxCancelRaceFree` under `-race`, the watch integration suite, both concurrency
fault tests, **binding-stress 100/100 pass 0 deaths**, and the full-suite hook (53/53). Owes the
codex/persona gauntlet on this HEAD (codex re-review #12 in flight).

**Round 19 — conflict-range oversized-key CLAMP (new finding #23, LOW, wire-bytes divergence).**
`AddReadConflictRange`/`AddWriteConflictRange` had the maxReadKey/maxWriteKey legal-range check (2004)
but NOT the C++ RYW oversized-key CLAMP: a non-system key >10 KB is < `\xff`, so it passes the
legal-range gate and reaches the clamp in libfdb_c (`ReadYourWrites.actor.cpp:1958-1976` read /
`:2474-2492` write) — each endpoint truncated to `getMaxReadKeySize+1` (== `getMaxClearKeySize`, 10009
non-system / 30001 system) and the range DROPPED if the clamp collapses it to empty. Go shipped the
FULL oversized key to the resolver (wire + tx-size-accounting divergence; outcome-equivalent since no
stored key exceeds the max, but the bytes/size differ). Ported the clamp (mirrors the existing
`ClearRange` clamp template) + red→green regression `TestAddConflictRange_ClampsOversizedKeys` (4
subtests: read/write × clamp-both-endpoints/drops-when-empty; revert-proven — all 4 fail without the
clamp). NOTE the single-key `AddReadConflictKey`/`AddWriteConflictKey` variants also miss this (+ the
2004 legal-range check) but their fix needs the API-shape decision (no error return to surface 2004) —
kept as a separate follow-up, not fixed piecemeal.

**Round 20 — codex re-review #12 (on the restructure HEAD `6a76e4d70`) found the setup-read slot
leak** (P2, watch-area again — the restructure's own edge). `WatchSetup` minted `watchCtx` but the
blocking setup reads still ran on the CALLER `ctx`: `ensureReadVersion(ctx)` + `ryw.get(ctx, key)`
(readpath.go:1127,1154). A Cancel()/reset() during a stuck value read cancels `watchCtx` (via
`cancelWatches`) but NOT the caller ctx, so the read stayed parked and the reserved slot stayed charged
until the caller ctx / RPC timeout — a starve under a low `MAX_WATCHES`. C++'s watch actor wraps its
setup waits in `catch{ cx->decreaseWatchCounter(); throw; }` (NativeAPI.actor.cpp:5637-5682), so
cancelling the actor on a txn reset releases the counter AT ONCE. Fix: thread `watchCtx` (a child of
the caller ctx, so caller cancellation still propagates) through both setup reads + a `watchSetupErr`
helper that maps a txn Cancel to transaction_cancelled (1025), matching the entry-check precedence.
Regression `TestWatchSetup_CancelUnblocksStuckSetupRead` — holds the value-read reply FOREVER, Cancels,
asserts Watch returns 1025 + the slot frees, all while the reply is still held (proves the CANCEL, not
the reply, unblocked the read — the dimension `TestWatchSetup_CancelDuringValueRead_ReleasesSlot`
missed by releasing the reply). Revert-proven: without the fix it FAILS at 24.7s ("Watch did not return
after Cancel"). Passes under `-race` with the whole watch suite. This is the restructure's own
follow-up edge; re-review #13 owed on the new HEAD.

**Round 21 — the round-20 HEAD's gauntlet (codex #13 + FDB-C-dev + Torvalds), all findings addressed.**
Both personas ACK'd the restructure + clamp + watchCtx commits vs C++ 7.3.77 (FDB-C-dev cited the exact
RYW/NativeAPI file:line for each). Follow-ups fixed in this round:
- **codex #13 [P2] Cancel ordering** (readpath.go:1180 / transaction.go:1797): `Cancel()` stored
  `txStateCancelled` AFTER `cancelWatches()`, so a setup read unblocked by cancelWatches could hit
  `watchSetupErr`→`checkCancelled` before the state flipped → `context.Canceled` instead of 1025 (a
  latent flake in the round-20 test). Fixed by storing the state FIRST; the store is now
  sequenced-before the ctx cancellation whose Done()-close is a happens-before edge to the read's
  `state.Load()` — correct by construction. (No isolated red→green: the race is benign-timing, Store
  wins ~always; a test asserting the intra-Cancel window passes both ways = a fake checkbox, so it was
  removed. The reliable end-to-end 1025 is the fault test.)
- **Torvalds #1 masking** (readpath.go:1179): `watchSetupErr` re-checked `checkCancelled` for ANY read
  error → a genuine FDBError on a cancelled txn was masked to 1025. Guarded to remap ONLY a context
  cancellation. Deterministic red→green: `TestWatchSetupErr_MapsCancelWithoutMaskingGenuineError`
  (`genuine_error_never_masked_on_cancelled_txn` FAILS without the guard — 1009 masked to 1025).
- **Torvalds #2** WatchPoll `defer watchCancel()` nil-guarded (defensive).
- **Torvalds #3** clamp RYW-enabled path now covered (`read_clamps_on_ryw_enabled_path`).
- **FDB-C-dev note (non-blocking, deferred):** Go acquires the watch slot BEFORE the setup value read;
  C++ RYWImpl::watch charges `increaseWatchCounter` AFTER the read (so C++ holds no slot during the
  read window). Pre-existing (finding #11 charge-at-registration), narrow, and commit 3 already narrows
  it (prompt release on Cancel). Candidate follow-up: move the acquire to after the value read while
  keeping it synchronous (before the async poll) to match RYWImpl::watch exactly. Queued, not in scope.

**Round 22 — grv-cache refresher Close barrier (new finding #25, LOW/robustness, Go-intrinsic).**
The lazy GRV-cache background refresher's one-shot `db.wg.Add(1)` (grv.go) could race Close's
`db.wg.Wait()` (database.go): the topology monitor keeps the wg counter ≥1 while alive, but Close's
`cancel()` makes it Done to 0, and a concurrent USE_GRV_CACHE opt-in launching the refresher in that
window Adds at a zero counter with a waiter registered → `sync: WaitGroup misuse: Add called
concurrently with Wait` panic (a crash). Fix: `registerBackgroundGoroutine()` reserves the wg slot
under `closeMu` gated on a `closed` flag that Close sets (under the same lock) BEFORE `cancel()`/
`Wait()` — so the check-and-Add is atomic w.r.t. the store: either the Add lands before `closed` (the
topology monitor still holds the counter ≥1, so no zero-counter Add; Close's Wait sees the slot) or
`closed` is set first (the refresher skips, falling through to a real GRV — correct during shutdown).
Deterministic regression `TestRegisterBackgroundGoroutine_SkipsAfterClose` (revert-proven: without the
`closed` check it reserves a slot after Close) + `TestRegisterBackgroundGoroutine_ConcurrentCloseNoMisuse`
(300-iteration concurrent register||Close stress, no panic). Owes its own client gauntlet.

**Round 24 — fresh-axis discovery sweep (3 parallel finders) + finding #26.** After the direct-fixable
findings were exhausted, ran 3 read-only finder agents on lightly-explored axes:
- **RYW getRange overlay merge — FAITHFUL** (traced vs `getRangeValue`/`RYWIterator`); the one behavioral
  difference is the SVK candidate-range Set-preservation, which is a DELIBERATE, documented, *saner* Go
  choice (C++ `addUnmodifiedAndUnreadableRange` silently drops a prior `Set`; Go keeps it). Not a bug.
- **getKey / key-selector RYW resolution — FAITHFUL** (exhaustively traced offset-count, orEqual,
  clear-skip, boundary, maxReadKey-clamp, rywDisabled). No divergence.
- **versionstamp ops → finding #26 (LOW, wire-request-byte, FIXED).** Go's committed
  `SetVersionstampedKey` mutation shipped the user's RAW zero placeholder; libfdb_c/Java commit the key
  TRANSFORMED with the cached-read-version min-bound stamp at the placeholder (C++ captures
  `getCachedReadVersion().orDefault(0)`, mutates k in place `ReadYourWrites.actor.cpp:2276`, stores it in
  the write map `:2295`, and the commit flush ships the write-map key `:2059`). The commit proxy
  overwrites `[pos,pos+10)` with the ASSIGNED stamp + strips the 4-byte offset, so the STORED record is
  byte-identical (the wire-compat HARD LINE held) — but the commit-REQUEST bytes diverged from both
  reference clients, and a code comment (transaction.go:1444) *incorrectly* claimed the raw-key commit
  matched C++. Fixed: `Atomic()` now buffers the min-bound-transformed key into `tx.mutations` (the same
  key feeds the RYW read model), with the comment corrected. Deterministic red→green
  `TestAtomic_SVKCommitsMinBoundTransformedKey` (revert-proven: raw key → all-zero placeholder) +
  `TestAtomic_SVKWriteOnlyTxnKeepsZeroPlaceholder` (orDefault(0) edge).
  **codex #17 caught a P2 regression in the #26 fix** (both personas ACK'd, missed it — codex the
  critical gate again): buffering the TRANSFORMED key meant `validateMutation` no longer saw the raw
  key, so a `\xff` placeholder at offset 0 (a raw system key on a non-system txn) bypassed the
  legal-range check (the transform hides the leading `\xff` behind the stamp). Fixed by gating the
  transform on `key < maxWriteKey` (only a legal raw key is transformed; an out-of-range one stays raw
  so `validateMutation` reports 2004 — matching C++ getVersionstampKeyRange's eager reject). Pin
  `TestAtomic_SVKSystemKeyPlaceholderStaysRawForValidation` (revert-proven: drop the guard → the
  `\xff` key is transformed to a `0x01…` stamp and passes validation). Re-gauntlet owed.

**Round 25 — 2nd discovery sweep (idempotency + size-limit finders).** Commit-idempotency finder →
**finding #27 (MEDIUM, open):** Go omits C++ `makeSelfConflicting()` (NativeAPI.actor.cpp:5952-5959,
called at :6858) — the ephemeral `\xFF/SC/<randomUID>` range added to the read+write conflict sets of
(virtually) every commit. Consequences: (a) the `CommitTransactionRequest` conflict-range bytes diverge
from libfdb_c (NOT persisted → no cross-engine record corruption, hard line holds); (b) the post-1021
`commitDummyTransaction` barrier synchronizes over a REAL user key (`writes[0].Begin`, commitpath.go:268)
instead of the SC key → a concurrent reader of that hot key can get a spurious `not_committed` (1020)
during a 1021 storm. Fix recipe: port `makeSelfConflicting` (inject `\xFF/SC/<UID>` into read+write
conflict ranges when `!causalWriteRisky` and ranges don't already intersect) so the dummy barrier uses
the ephemeral key. Idempotency-ID tagging is NOT a divergence (`automaticIdempotency` defaults false, so
libfdb_c also omits them — Go matches). Needs a deterministic UID → deferred (Date/rand unavailable in
some contexts; use a per-txn counter/uid source).

Size-limit finder → **finding #28 (HIGH, open — architectural, MEASURED vs real libfdb_c):** Go sizes
`transaction_too_large` (2101) AND ships the commit against the UNFOLDED append-only `tx.mutations` log
(transaction.go:1264/1306/1447 append; :1714 2101 gate; :1752 ship), whereas libfdb_c COALESCES same-key
writes in the RYW WriteMap (`writes.mutate`/`clear`, ReadYourWrites.actor.cpp:2295/2402/2435) and
materializes the FOLDED map at commit (:1392) before sizing (`Transaction::getSize` NativeAPI:6818 vs
sizeLimit :6835). Measured: 200× `Set(sameKey)` → Go rejects at sizeLimit<22000, cgo at <110 (~200×); at
the 10 MB default, ~150k increments of ONE counter key → **Go throws 2101 while Java/C on the same
cluster commit fine** (1 folded mutation). `GetApproximateSize` itself MATCHES cgo (both unfolded — the
user API is faithful); the divergence is at the 2101 boundary + the shipped commit content + resolver
load. Final stored KV state is identical (last-write-wins/atomic fold) → NOT data corruption, but a real
cross-client commit-behavior divergence that breaks ported counter/hot-key/retry-accumulation code. The
existing `TestDifferential_TransactionSizeLimit` uses only DISTINCT keys, so the folding axis was never
probed. **Fix (architectural, deferred — needs an RFC + careful atomic-coalescing/clear/set-then-clear
semantics):** commit from a coalesced write map (Go already folds in the RYW model `tx.ryw` but ships the
unfolded `tx.mutations`; C++ coalescing is a RYW-ENABLED feature — readYourWritesDisabled does NOT
coalesce in either client, so scope the fold to the RYW-enabled path). It touches the commit path — a
rushed change risks a lost/misordered-mutation regression, so it is the correct focused follow-up, not a
session-tail fix.

**NOTE on the 2nd finder sweep:** the size-limit finder (a general-purpose, write-enabled agent) wrote a
scratch test into `pkg/fdbgo/bench/` mid-investigation, which gazelle picked up and failed a commit hook.
Cleaned up. Future discovery sweeps should use the read-only `Explore` agent, or run in a worktree, to
keep the tree clean.

Also independently verified FAITHFUL this round (no divergence): snapshot read conflict-skipping + RYW
visibility, atomic-op RYW fold (absent/present-empty V2 gating), commit conflict-range assembly,
Transaction shared-field synchronization, `IsRetryable` (byte-perfect vs `fdb_error_predicate`), and the
retry backoff params (10ms/2.0/1s vs C++ ClientKnobs). The codebase is meticulous — remaining bugs are
architectural (RFCs) or dimensional.

**Codex caught 22 real issues across 18 review rounds the persona reviewers missed** — critical-gate
value, fully borne out. The latest (codex #17): a P2 REGRESSION in the finding-#26 fix itself
(SVK-transform hiding the raw key from legal-range validation) that BOTH persona reviewers ACK'd and
missed. This is the recurring lesson of the whole hunt: codex is load-bearing; never merge a
query/client change it hasn't cleared on the exact HEAD.

## Session close-out (this shift)

**Fixed + gauntlet-cleared (8 fix commits):** RFC-168 per-watch restructure; #23 conflict-range
oversized clamp; #24 watch-setup-reads-on-watchCtx; round-21 (codex #13 Cancel-order + Torvalds' 3);
#25 grv-cache Close barrier + deterministic Cancel test; #15 SendFrame pooled-body-on-error;
#26 SVK commit min-bound transform; #26-P2 SVK legal-range gate (codex #17). Each red→green + FDB-C-dev
+ Torvalds + codex ACK/clean.

**Verified FAITHFUL this shift (no divergence — ~10 axes):** RYW getRange merge, getKey/selector
resolution, snapshot conflict-skip + RYW visibility, atomic-op RYW fold (V2 absent/present-empty),
commit conflict-range assembly, Transaction shared-field sync, IsRetryable (byte-perfect), retry backoff
params, versionstamp (except #26), metadata-version.

**Documented + DEFERRED (architectural / verified-narrow — precise recipes above):** #22 getValue
fallback masking (narrow); #27 makeSelfConflicting (MEDIUM — commit self-conflict + dummy-barrier);
#28 write-map-folding size/2101 divergence (HIGH, MEASURED vs cgo — commit from a coalesced map). Plus
the pre-existing architectural set: #8 watch-at-committed (RFC-170), #9/#14 reset-clears-options
(RFC-171), #10 getKey isBackward (RFC-169), #16 SYSTEM_IMMEDIATE GRV-cache, #21 api<520 versionstamp.

**Signal for the next shift:** the direct-fixable client surface is exhausted; the remaining findings are
commit-path/architectural (need RFCs + careful implementation), and the #26→P2 sequence shows quick
commit-path changes carry real regression risk. #27 and #28 are the highest-value next fixes — both want
their own focused effort with a real cross-client differential test, not a tail-of-shift patch.

## Findings NOT yet fixed (all CONFIRMED unless noted) — priority order

### Architectural / needs design (write an RFC, route through FDB-C-dev first)
- **[HIGH] Watch registered at READ version, not COMMITTED version** (`readpath.go:1080`,
  facade eager goroutine `fdb/future.go:177`). When the watching txn also writes the watched key,
  the watch fires spuriously+immediately (SS reads the pre-commit value at a version in [RV,CV)).
  C++ registers post-commit at `getCommittedVersion()` (NativeAPI:6420, commitAndWatch:6909-6918).
  FIX = defer watch RPC to after commit, re-stamp with committedVersion (Java/C++ commitAndWatch
  shape). Single-container differential repro: seed k=A in a separate txn; `{Set(k,B); Watch(k)}`;
  no external change → cgo pending, Go fires. **RFC + FDB-C-dev ACK.**
- **[HIGH] Watch goroutine vs Cancel/Reset DATA RACE + lost-cancellation goroutine leak**
  (`transaction.go` getWatchCtx:~1863 / cancelWatches:~1852 — `watchCtx`/`watchCancel` are plain
  fields, no mutex). WatchPoll (in the async future goroutine) races Cancel()/Reset() (incl. the
  OnError retry path). Two harms: (1) `-race` data race; (2) if cancelWatches runs before the
  goroutine's first getWatchCtx (WatchSetup never sets the fields), cancel is a no-op and the
  goroutine then mints a fresh never-cancelled context → unbounded long-poll leak. FIX = guard the
  two fields with a mutex AND make getWatchCtx return an already-cancelled ctx after Cancel/Reset
  (or have WatchPoll observe tx state). Repro: `-race` with concurrent getWatchCtx||Cancel; +
  deterministic cancel-before-getWatchCtx leak. **Bounded fix but concurrency-careful; add a
  `-race` regression.**
- **[MEDIUM] rywDisabled GetKey ignores isBackward in shard location** (`readpath.go:179`,
  `locality.go` locate/lookupLocked — no reverse param). A backward selector on a cross-server
  shard boundary loops wrong_shard → 1007 (livelock). C++ threads `Reverse{k.isBackward()}`
  (NativeAPI:3788,1955,2022). NOT reproducible on single-container (needs multi-SS topology or
  SimTransport). FIX = thread isBackward through locate/lookup/buildGetKeyServerLocationsRequest.
  **RFC + multi-SS or SimTransport proof.**

### Bounded ports (fix inline, single-container differential or client regression)
- **[MEDIUM] too_many_watches (1032) never enforced; SetMaxWatches is a no-op**
  (`fdb/options.go:391`, `readpath.go:1069`). C++ caps outstanding watches per Database
  (NativeAPI:5694,2175-2179; default 1e4, ClientKnobs:120). FIX = outstanding-watch counter on the
  database, inc at registration → 1032 when over, dec on fire/cancel; wire SetMaxWatches.
- **[MEDIUM] OnError backoff not bounded by SetTimeout deadline** (`transaction.go:OnError`).
  C++ races the backoff `delay()` against the timebomb (ReadYourWrites.actor.cpp:1506,1517) and
  surfaces 1031 at ~deadline. Go sleeps the full (growing) backoff then retries, overshooting the
  declared timeout by up to one backoff (1s normal / 30s resource-constrained). FIX = check
  tx.deadline at OnError entry (return 1031 if passed) and bound backoffSleep by the deadline.
  Tight unit repro in the finding (no race needed).
- **[MEDIUM] Iterator() returns empty for Limit=-1 (ROW_LIMIT_UNLIMITED) and Limit<=-2**
  (`fdb/range_result.go:208`, effectiveLimit:64-69). -1 is unlimited (return all); <=-2 is
  range_limits_invalid (2012). Iterator bails `remaining<=0` → 0 rows + nil, contradicting its own
  GetSliceWithError AND libfdb_c. FIX = effectiveLimit maps -1→MaxInt, the Iterator path validates
  limit<-1→2012 like getRangeDir. Differential + internal-consistency test.
- **[MEDIUM, CONFIRMED ✅ vs C++] AddReadConflictRange/Key skip the RYW write-map filter**
  (`transaction.go:2595` AddReadConflictRange → `addReadConflict` directly; `:2612`
  AddReadConflictKey → `addReadConflictForKey` directly). **Verified against C++**: C++
  `addReadConflictRange` adds directly ONLY when `readYourWritesDisabled` (ReadYourWrites.actor.cpp:1977-1981);
  otherwise it runs `updateConflictMap(readRange, it)` (`:1983-1986`) — the write-map filter (334-351)
  that subtracts locally-written independent segments. Go always adds directly (no rywDisabled split,
  no filter) → over-conflict (spurious 1020). FIX = mirror the existing `addGetKeyConflictRange`
  pattern: when `!rywDisabled`, route through `conflictRangesLocked` (range) /
  `conflictForKeyLocked` (key); when rywDisabled, add directly (C++ :1979). Repro:
  A `Set(K); AddReadConflictKey(K)`; B `Set(K); commit`; A.commit → cgo commits, Go 1020.
  Differential via the existing conflict-outcome harness (`differential_getrange_conflict_test.go`).
- **[MEDIUM, UNCERTAIN — verify] Watch on system/special/oversized key not rejected**
  (`readpath.go:1069` WatchSetup). libfdb_c returns key_outside_legal_range (2004) / key_too_large;
  Go silently registers. FIX = WatchSetup applies the same maxReadKey/key-size gate as Get.
- **[MEDIUM, UNCERTAIN — verify] grv `db.wg.Add(1)` races `Close()`'s `wg.Wait()`**
  (`grv.go:295`) → "WaitGroup misuse: Add called concurrently with Wait" panic. **Verify the
  Add/Wait ordering; fix with the standard add-before-spawn or a closed flag.**
- **[MEDIUM, UNCERTAIN — verify] Hedge top-level ctx.Done() leaks the primary's QueueModel
  startRequest delta** (`hedge.go:99`) → permanent load-balancer skew. **Verify the delta
  accounting on the ctx.Done path; endRequest the started delta.**

### Trivial / niche
- **[LOW] commitDummyTransaction jitter ±10% (U[0.9,1.1)) vs C++ getBackoff U[0,1)**
  (`commitpath.go:186`/`206`). Timing-only, no wire/data effect. FIX = use `backoff * rand01()` in
  the dummy loop (the main OnError path already does). Check no other caller of jitterBackoff.
- **[LOW] StreamingModeExact + no row/byte limit should be exact_mode_without_limits (2210)**
  (`fdb/range_result.go:144`). Only the explicit-Exact Iterator path; GetSliceWithError unaffected.
- **[LOW] api<520 versionstamp offset-suffix transform unimplemented** (`transaction.go:1319`).
  C++ withSuffix `\x00\x00` (key) / `\x00\x00\x00\x00` (value) for apiVersion<520
  (ReadYourWrites.actor.cpp:2251-2261). FIRST verify the Go client's minimum supported API version —
  if it floors at >=520 this is N/A; else add the <520 branch + differential at API 510/500.
- **[LOW, VERIFIED real but NARROW — deferred for a proper multi-server test] #22 sendGetValue
  fallback masks a genuine reply error.** VERIFIED vs C++: `getValue`'s catch propagates any non-
  wrong_shard/all_alternatives error unchanged (`throw e`, NativeAPI.actor.cpp:3738), so a
  future_version (1009) must surface. **But the exposure is narrower than first stated:** the Go HEDGE
  path already surfaces a future_version reply directly via `parseGetValueReply` (readpath.go:563-565,
  `result.err==nil` for a reply-carried error → not the fallback). The masking is ONLY reachable in the
  fallback branch (`result.err != nil`), i.e. after the hedge CONN-FAILS or TIMES OUT **and** a
  fallback replica then returns future_version — a narrow double-failure. There, the loop drops the
  future_version and returns errReplyTimeout/1007 instead of 1009. **Fix (designed, not shipped):**
  remember an `isFutureVersionOrProcessBehind` error in the fallback loop and surface it with precedence
  version-err > timeout > 1007. **Why deferred:** a deterministic revert-prove needs a ≥3-server sim
  with mixed per-server behavior (2 hedge arms drop/conn-fail, 1 fallback replica replies
  `inlineErrorReply(1009)`) whose reachability depends on the container's replica count — a fragile,
  focused test disproportionate to a LOW, narrow divergence in the sensitive read-failover path.
  Shipping the fix without that test violates the no-untested-fix rule, so the code change was reverted;
  this is the precise recipe for the next focused implementation.

## Axes that NEVER ran (session limit) — re-run after 7:10pm
`size-limits`, `ryw-get`, `metadata-version`, `wire-encoding-parsers`, `grv-readversion` (partial),
`buffer-pools-overflow`, `txn-options-lifecycle`, `snapshot-ryw` (partial),
`conflict-ranges`/`special-system-keys`/`concurrency-grv-dial`/`readpath-resilience` (finder ran,
verifier failed → UNCERTAIN above; re-verify).

Re-run: edit `RERUN_ONLY` in the saved workflow script
`.../workflows/scripts/fdbgo-bughunt-discovery-wf_c21743ba-ae1.js` to the unrun keys, then
`Workflow({scriptPath: ...})`. Findings JSON saved at `shifts/scratch/fdbgo-findings*.json`.

## Review owed (before any PR/merge)
The 3 committed fixes need the client gauntlet: **FDB C++ client developer** (validate vs 7.3.77
file:line) + **Torvalds** + **codex-review** (`codex -s read-only -a never review --base <sha>`).
Re-request after every commit. The watch-CV and isBackward RFCs need FDB-C-dev ACK on design first.

## Calibration note
5 top axes were independently verified faithful before the hunt (atomic value semantics, retry
classification, read-path failover bounding, GRV cache constants, conflict-range addition) — this is
a meticulous codebase; remaining bugs are dimensional (unprobed axis) or Go-intrinsic
(race/leak/robustness), exactly as the findings show.
