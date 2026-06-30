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

Also written: **RFC-165** (watch-at-committed-version design, Draft — needs FDB-C-dev ACK).

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

Also written: **RFC-166** (reset() must clear non-persistent options — closes the `txn-options-lifecycle`
HIGH + `snapshot-ryw` MEDIUM findings; Draft, needs FDB-C-dev ACK).

The 2026-06-30 clean discovery re-run also CONFIRMED (still open, in the PR table): Reset() option
preservation (HIGH, RFC-166), buffer-pool `sync.Pool` race on SendFrame error (LOW), SYSTEM_IMMEDIATE
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

Also written: **RFC-167** (getKey isBackward shard-location, Draft — needs multi-SS/SimTransport proof).

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
- **#22 sendGetValue fallback error-masking — UNCERTAIN, re-verify** before any change.

## Precise recipe — #15 buffer-pool race (LOW, ROOT-CAUSED, ready for a focused change)

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
- **[LOW, UNCERTAIN — verify] sendGetValue sequential fallback swallows genuine FDB reply errors**
  (`readpath.go:547`) — masks e.g. 1009 as all_alternatives_failed/1007.

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
