# RFC 010: Native fdb-go Client — Correctness Hardening & Systemic Prevention

## Status: PROPOSED (Torvalds-reviewed + C-client conformance-reviewed vs FDB 7.3.75; changes folded in)

## Problem

An external audit (Codex, 2026-06-01, captured in `TODO_client.md`) flagged 15 issues
across the pure-Go FDB client: `pkg/fdbgo/client`, `pkg/fdbgo/fdb`, `pkg/fdbgo/transport`,
`pkg/fdbgo/wire`. Every finding was independently re-verified against the actual code (not
the report's line numbers) before being accepted. **13 of 15 are real and actionable; 1 is a
false positive; 1 is cosmetic.** Several are genuine wire/correctness defects in code that is
on the critical path of every read and commit.

The headline isn't any single bug — it's the failure *mode*. The native client reimplements,
by hand and in several independent copies, four things that FDB's C/C++ client centralizes:
**(1)** protocol error classification, **(2)** read retry/load-balancing policy, **(3)** resource
lifecycle (reply registration, queue-model accounting, connection teardown), and **(4)** request
marshalling. Each hand-copy drifted. Worse, in at least one case (#2) the *test* encoded the
implementation's own mistake as its oracle, so CI stayed green while the behavior was broken.

This RFC is the plan to (a) fix the verified defects, prioritized by real blast radius, and
(b) make the four invariant classes above centralized, executable, and enforced — so the next
hand-copy can't silently drift.

## Verification summary

Each finding was checked by reading the cited code and, where the claim was non-obvious,
reproducing it. Severity is **recalibrated** from the report where verification disagreed.

| # | Finding | Verdict | Report sev | **Calibrated** | Note |
|---|---------|---------|-----------|----------------|------|
| 2 | `ErrWrongShardServer = 1062` (real code is 1001; 1062 = `change_feed_cancelled`) | **REAL** | High | **High** | + self-confirming test injects 1062 and asserts success |
| 3 | `Transaction.Get` pipelined path skips wrong-shard retry / hedge / cache-invalidate; falls back on *any* error not just `ErrNeedFullRYW` | **REAL** | High | **High** | default path for every public point read |
| 8 | `ReadErrorOr` shape heuristic misclassifies 1-field success replies; `GetRangeSplitPoints` silently drops real split points | **REAL** | Medium | **High ↑** | report *under*-rated; silent data loss, untested |
| 4 | Tenant commit builder mutates `tx.mutations` in place via unsafe alias; rebuild double-prefixes / double-adjusts versionstamp | **REAL** | High | **High** | tenant-scoped |
| 6 | Conn shutdown: writeLoop exits on ctx.Done without waking queued `errCh`; monitor declares dead without closing socket / failing pending | **REAL** (two bugs) | High | **High** | unbounded goroutine hang + fd/goroutine leak |
| 5 | Hedge loser's `startRequest` delta never paired with `endRequest`; timeout/cancel leaks **both** | **REAL** | High | **Medium** | slow-bleed load-model/selection bias, not data loss; 15-line fix |
| 11 | TLS advertised in README but never wired into DB dial; `DialWithTLS(useTLS=true, nil)` corrupts framing | **REAL** | Medium | **Medium** | no TLS path through public API |
| 10 | DB-default access-system-keys omits `FLAG_IS_LOCK_AWARE`; per-tx facade sets it | **REAL (fix direction flipped)** | Medium | **Medium** | conformance: in the C client `ACCESS_SYSTEM_KEYS` does **not** imply `LOCK_AWARE` (independent options). Conformant fix is to **decouple**, not couple — the existing facade coupling is itself a divergence |
| 13 | Reply channel never returned to pool on success; pool defeats its own purpose; doc comments are wrong | **REAL** | Low | **Low–Med** | hot-path alloc overhead |
| 15 | Forward range boundary `append([]byte(lastKey), 0)` aliases backing array; safe only by `cap==len` today; reverse path copies | **REAL** (latent) | Low | **Low** | also reachable **today** via RYW write-passthrough keys |
| 1 | Read parsers ignore inline `LoadBalancedReply.error` — the SS's **primary** wrong-shard/future-version channel for reads | **REAL** | High | **High** | conformance review (7.3.75) corrected this: SS *always* sets the inline field for read errors via `sendErrorWithPenalty`; Go reads it as a successful empty result → silent wrong-shard. **Co-equal with #2/#3.** An interim downgrade to Medium was *wrong* — see Wire Conformance Review |
| 7 | Public "concurrent-safe" contract vs unlocked reads/clears of `tx.mutations` | **REAL** | High | **Medium ↓** | idiomatic single-writer use is safe; needs concurrent Set-with-Commit. RYW sub-claim is a *different* mutex (`ryw.mu`, not `conflictMu`) |
| 14 | Full `writeCh` makes monitor short-circuit ping wait via closed `done` | **PARTIAL** | Low | **Low** | mechanism real; impact is "detection delayed ~1.5s/cycle", not "defeated" |
| 9 | `isSystemKey` matches `\xff\xff` only | **PARTIAL** | Medium | **Low ↓** | naming defect only; in FDB `\xff\xff`=special (bypasses resolver), `\xff`=system (should conflict) — current behavior is likely *correct*, function is mis-named |
| 12 | Location refresh panics on empty response | **FALSE** | Medium | **none** | `queryLocations` only returns `nil` error when `len(entries)>0`; `entries[0]` unreachable when empty. Add a defensive guard as cheap hardening, but there is no bug |

**Net:** the audit is high quality. 11 solid bugs, 2 real-but-narrower-than-claimed, 1 cosmetic,
1 false positive. The report under-rated #8 (real silent data loss) and over-rated #7. It had
**#1 right at High** — an interim downgrade to Medium during review was wrong and the wire
conformance review (below) reinstated it. #10 is real but its fix direction had to be flipped.

## Wire conformance review (vs FDB 7.3.75 C client)

Every proposed fix was checked against the real `apple/foundationdb` **release-7.3** source
(matching the Go client's pinned 7.3.75 codegen), since C++ is the spec. Result: the fixes for
**#2, #3, #4, #5, #8, #9 are CONFORMANT** (they match the C client exactly). Three corrections
came out of this review:

1. **#1 is REQUIRED for conformance and is High, not Medium.** Read replies (`GetValueReply`,
   `GetKeyReply`, `GetKeyValuesReply`) all derive from `LoadBalancedReply`
   (`StorageServerInterface.h`). The storage server's `sendErrorWithPenalty` overload for
   `LoadBalancedReply`-derived replies (`storageserver.actor.cpp:1855-1865`) sets
   `reply.error = err; promise.send(reply)` — the **inline** field — and `getValueQ`/`getKeyQ`/
   `getKeyValuesQ` route `wrong_shard_server`, `future_version`, `process_behind` through it
   (these are all in `canReplyWith`). The client throws it in `checkAndProcessResultImpl`
   (`LoadBalance.actor.h:337-366`). So the inline field is **the primary wrong-shard channel for
   reads**, not the `ErrorOr` root. The Go parsers never read it → a wrong-shard read reply is
   silently consumed as a successful empty result. My earlier reasoning ("wrong-shard arrives via
   the ErrorOr wrapper") was wrong. #1 is co-equal with #2/#3.

2. **#1's decode is NOT the same path as #8.** #8 parses the *root* `ErrorOr` union (uint8 tag at
   slot 0 = {1:Error, 2:success}, value RelOff at slot 1 — confirmed against `flat_buffers.h:948-956`).
   #1 must parse `Optional<Error>` *inside* the reply: a uint8 present-tag at `SlotError` + a
   RelativeOffset at `SlotError+1` to a nested **Error table** whose code is `uint16` at slot 0.
   The generated `Error []byte` field decoded via `ReadBytes` (length-prefixed) is the wrong shape
   for a table — the helper must `ReadNestedReader` + `ReadUint16`. The two fixes share the
   *uint16-code* insight, **not** the navigation. (The earlier "one decode path shared with #8"
   note was wrong and is corrected in Phase 0.)

3. **#10's fix direction is backwards.** In the C client, `ACCESS_SYSTEM_KEYS`/`READ_SYSTEM_KEYS`/
   `RAW_ACCESS` set **only** `rawAccess` (`NativeAPI.actor.cpp:7159-7171`); `LOCK_AWARE` is an
   independent option (`:7072-7080`), and C++ call sites that want both set them separately. So
   the conformant fix is to **decouple** — making `SetAccessSystemKeys` imply `lockAware` (the
   RFC's original #10 *and* the existing facade at `fdb/options.go:60`) is itself a divergence
   that Java/CGo apps sharing the cluster would observe (lock-aware lets writes bypass the
   database-locked check).

Two pre-existing divergences the audit didn't isolate, surfaced here:

- **#8 reader/writer disagree.** The Go ErrorOr *writer* (`erroror.go`) already emits the correct
  union tag (1=Error, 2=success); only the *reader* (`reader.go`) ignores it and infers from field
  count. The #8 fix is "make the reader adopt the tag the writer already uses" — the writer is the
  correct in-repo reference.
- **#2 is load-bearing, not "Layer 2 only."** With `ErrWrongShardServer = 1062`, the live read
  retry in `getValue`/`getKey`/`getRange` keys cache-invalidate-and-retry off `change_feed_cancelled`,
  so it never fires on real wrong-shard and would spuriously fire on a change-feed error. The
  "Layer 2 only" comment at `transaction.go:29` is misleading and should go.

## How the C/C++ client handles these (the spec anchor)

Per project doctrine, **C++ is the spec for the FDB client; Go divergence is a Go bug.** The
relevant upstream behaviors the Go port must match:

- **One error taxonomy.** `flow/error_definitions.h` is the single source of every code; there
  is exactly one mapping from code → name and one `fdb_error_predicate()` for retryability.
  Our three hand-tables (`fdb/error.go`, `wire/reader.go`, `client/transaction.go` constants)
  already drifted (#2): the description maps were fixed in a prior shift, the `ErrWrongShardServer`
  *constant* was not.
- **One `loadBalance()`.** All point/range/keyselector reads go through the same actor: locate
  → send (optionally with a hedged second request) → on `ErrorOr` error or inline
  `LoadBalancedReply.error`, update the queue model and retry the correct shard. Pipelining in
  C++ defers *waiting*, never the error/retry policy. Our port forked a second, semantics-poor
  path (`GetPipelined`/`PendingGet.Resolve`, #3).
- **RAII resource release.** C++ wraps the queue-model delta in a `ModelHolder` whose destructor
  releases it for **both** the winner and the cancelled loser of a hedge. Go has no RAII; the
  port wired release for the winner only (#5).
- **`applyTenantPrefix` is pure over a copy.** C++ builds the prefixed mutation set without
  scribbling the transaction's own buffer; ours aliases and mutates it (#4).

## Fix plan

Ordered by real blast radius, not by the report's numbering. Each item ships with the
regression test that pins the corrected behavior **and**, where a prior test encoded the bug,
the corrected test.

**Phasing rule (per Torvalds review):** correctness fires ship as *targeted* fixes with their
regression tests. The read-path consolidation (dedup the four copies of the
locate→hedge→endRequest→classify loop) is a **separate, non-gating** refactor — it must not
block the High fixes. An abstraction that gates the fires turns one-shift work into three-shift
work.

### Phase 0 — wire/correctness fires + the two cheap read fires (land first, small, independent)

- **#2 wrong-shard code.** Set `ErrWrongShardServer = 1001`. Rewrite the fault-injection test
  (`fault_test.go`) to inject **1001** and assert cache-invalidation+retry, and add a negative
  test proving **1062** (`change_feed_cancelled`) is *not* treated as wrong-shard. This is a
  one-line code change exposing a self-confirming test — the test rewrite is the real work.
- **#8 ErrorOr shape heuristic.** Replace `nfields<=1 ⇒ error` inference in `ReadErrorOr`/
  `ReadErrorOrInto` with real `ErrorOr` union tag/value parsing — read the uint8 tag at slot 0
  ({1:Error, 2:success}) and the value RelOff at slot 1, the convention the Go *writer*
  (`erroror.go`) already emits. Decode `Error.ErrorCode` as `uint16`. Pin `SplitRangeReply` with
  nil and non-empty `SplitPoints`, and `ErrorOrError` with non-zero padding around the 2-byte
  code. Tighten `multishard_test.go` to assert the *expected* split points, not just `len>0`.
- **#1 inline reply error — REQUIRED for wrong-shard conformance on reads (High, co-equal with
  #2/#3).** The storage server delivers `wrong_shard_server`/`future_version`/`process_behind` for
  reads through the **inline** `LoadBalancedReply.error`, not the `ErrorOr` root (see Wire
  Conformance Review). Add a shared `checkLoadBalancedReplyError` step to the three
  `LoadBalancedReply`-derived parsers (`parseGetValueReply`, `parseGetKeyReply`,
  `parseGetKeyValuesReply`) before reading success fields, and route the returned `*wire.FDBError`
  into the same classify→invalidate→retry as the root-error path. **Decode is its own path, not
  #8's:** `Optional<Error>` here is a uint8 present-tag at `SlotError` + a RelativeOffset at
  `SlotError+1` to a nested **Error table** (uint16 code at slot 0) — navigate it with
  `ReadNestedReader` + `ReadUint16`, *not* `ReadBytes`/`ReadInt32`. Apply only to these three
  replies — `WatchValueReply` is **not** a `LoadBalancedReply` and must not get the check. Test:
  inject an inline wrong-shard on each of Get/GetKey/GetRange and assert cache-invalidate+retry.
- **#5 hedge queue-model leak (targeted, ~15 lines).** Make `hedgeResult` carry per-RPC cleanup
  for **both** arms, or have `raceReplies`/`waitForReply` call `endRequest` on the loser, timeout,
  and cancel paths directly. Does **not** need the read state machine. Deterministic test asserts
  `smoothOutstanding` returns to baseline on winner/loser/timeout/cancel.
- **#3 pipelined read semantics (targeted, localized to the pipelined path).** Make
  `PendingGet.Resolve` run the same classify→cache-invalidate→retry as `getValue` for the single
  key it owns, and fall back to the full path **only** on `ErrNeedFullRYW`. Critically — the
  legal-key-range rejection must stay at **`GetPipelined` enqueue time, before `SendFrameDeferred`**,
  not in `Resolve` (by `Resolve` the illegal frame is already on the wire). Retrying an individual
  wrong-shard key is necessarily synchronous (locate→send→wait) — that's the rare slow path and
  matches C++. **Batching is preserved:** N `Get`s still queue N deferred frames and the first
  `Resolve` flushes them in one syscall. Pin *both* axes: the injected-error matrix (correctness)
  **and** a test asserting N pipelined Gets still produce exactly one flush (the perf feature
  didn't silently turn into N round-trips).

### Phase 1 — read-path consolidation (refactor, non-gating, behavior-identical)

- Dedup the four near-identical read loops (`getValue`/`sendGetValue`, `getKey`/`sendGetKey`,
  `getRange`/`sendGetRange`, `sendGetValueToServer`) into one internal read state machine
  (locate → send → decode → classify root+inline error → queue-model update → cache-invalidate →
  retry/return). Synchronous, pipelined, hedged, and fallback reads become *schedulers* over it;
  pipelining defers the *wait*, hedging sends a backup — neither owns a different error path.
- **Definition of done is "behavior-identical":** every read entrypoint passes the *same* shared
  injected-error matrix it passed after Phase 0, and the pipelining one-flush test still holds.
  This phase changes structure, not behavior — if a test changes, the refactor is wrong.

### Phase 2 — lifecycle & marshalling (#6, #4, #13)

- **#6** Single `failConnection(err)`: cancel ctx, close the socket, fail all pending replies,
  drain queued sync-write waiters with an error. `Close`, the readLoop error path, **and**
  `connectionMonitor` all route through it. The errCh waits in `SendFrame`/`Flush` gain a
  `<-c.ctx.Done()` escape. Tests: `Close` racing `SendFrame`/`Flush`; monitor-driven dead-conn
  cleanup wakes pending reads without waiting for RPC timeout.
- **#4** Tenant commit builder snapshots into a scratch `[]MutationRef` with copied slice headers
  before prefixing; the unsafe zero-copy alias is kept **only** for the no-tenant path. Test:
  build twice on the same tenant tx → no double-prefix, `tx.mutations` unmodified.
- **#13** Return the reply channel to the pool exactly once on the success path; fix the false
  doc comments. Guard against double-put across `Cancel`/`Release`.

### Phase 3 — narrower / hardening (#7, #10, #11, #15, #9, #12, #14)

- **#7 — honor the published contract, don't narrow it.** `fdb/transaction.go:24` *publishes*
  "individual methods are safe for concurrent use." Narrowing that doc to match the broken code is
  papering over by editing the spec — forbidden. Instead **make the methods that matter actually
  safe**, which is cheap: (a) snapshot `mutations`/`readConflicts`/`writeConflicts` under
  `conflictMu` at the top of `Commit`/`buildCommitTransactionRequest`/`GetApproximateSize` (one
  slice-header copy — noise next to a commit RPC), and (b) **move the `tx.mutations = tx.mutations[:0]`
  clears inside the existing `conflictMu` section** in `postCommitReset`/`reset` (strictly correct
  regardless). That honors the contract for `Set`/`Get`/`Commit`/`GetApproximateSize`. The **one**
  remaining sharp edge — the RYW atomic resolve-vs-`Set` lost update (`ryw.go:238`, under `ryw.mu`,
  not `conflictMu`) — requires holding `ryw.mu` across a server round-trip to fix properly; that's
  a real perf decision, so document *that specific method* as not-concurrent-safe rather than
  pretending the whole transaction is unsafe. `-race` tests for `Set`+`Commit`,
  `Set`+`GetApproximateSize`, and the RYW lost-update.
- **#10 — decouple, don't couple (conformance-corrected).** In the C client `ACCESS_SYSTEM_KEYS`
  sets only `rawAccess`, never `lockAware` (`NativeAPI.actor.cpp:7159-7171`); the two are
  independent. So the conformant fix is to make the two Go entrypoints agree by **removing** the
  facade's auto-`SetLockAware` (`fdb/options.go:60`), *not* by adding it to the client method. If
  a "system admin" convenience that sets both is wanted, expose it as an explicitly-named helper —
  the wire-facing option must match C semantics. Unit-test that both a per-tx and a DB-default
  access-system-keys tx commit with `FLAG_IS_LOCK_AWARE` **unset** unless lock-aware was set
  separately, matching the C client. (Caveat: this is a behavior change to the existing facade —
  call it out in the PR; any code relying on the implicit coupling must set `SetLockAware`
  explicitly, exactly as a Java/CGo app must.)
- **#11** Either wire TLS config through `ParseClusterString`/`ClusterFile` → `getOrDial`, or
  drop the README claim and make `DialWithTLS(useTLS=true, tlsCfg=nil)` reject rather than emit
  TLS-framed plaintext. (Pick: keep the claim, do the wiring — it's the spec'd capability.)
- **#15** `ri.begin = append(append([]byte(nil), lastKey...), 0)` — mirror the reverse path's
  defensive copy. Test with a `Key` whose `cap > len`. (Reachable today via RYW passthrough keys.)
- **#9** Rename `isSystemKey` → `isSpecialKey` (it tests `\xff\xff`); fix the comment. No behavior
  change — current resolver-conflict handling is correct.
- **#12** Add a defensive length guard in `locality.refresh` returning a typed error on empty
  entries. Not a live bug; cheap insurance against a future refactor breaking `queryLocations`'s
  non-empty invariant.
- **#14** On full `writeCh`, fall through to the bytes-received liveness check instead of
  short-circuiting via a closed `done`. Test with a saturated `writeCh`.

## Systemic prevention

Codex's `TODO_client.md` prevention plan (A–I) is directionally right on the four invariant
classes. It is **wrong on the enforcement mechanism**: regex test-name CI gates
(`go test -run 'Hedge|QueueModel|...'`) prove nothing — a gate that matches zero tests passes
green, which is the exact "green CI, latent bug" trap that produced #2 in the first place. This
section keeps the good structure and replaces the gates with mechanisms that fail loudly.

### P1. One error taxonomy, generated — kills #2, the #8 int-width nit, retry drift

- A single checked-in source of truth (`error_definitions` table) generates code→name,
  retryability, and the named constants. `client`, `fdb`, `wire` consume the generated table;
  none defines its own.
- **Enforcement that bites:** a test asserts every reused error *constant* equals its canonical
  table entry (`ErrWrongShardServer == codeFor("wrong_shard_server")`). Not a name regex — a
  value assertion that fails if a constant drifts again.

### P2. One read state machine — kills #1, #3, #5, future read drift

- Exactly one function classifies a reply (`ErrorOr` root error **and** inline
  `LoadBalancedReply.error`) and decides retry/invalidate. Pipelined/hedged/sync/fallback are
  *schedulers* over it, never parallel semantics.
- **Enforcement:** a shared injected-error matrix (wrong-shard, future-version, process-behind,
  all-alternatives-failed) runs against **every** read entrypoint — sync `Get`, pipelined `Get`,
  hedged `Get`, `GetKey`, `GetRange` — table-driven, same table. A new read path that doesn't
  route through the state machine fails the matrix because it won't retry.

### P3. Exactly-once resource ownership — kills #5, #6, #13

- Every acquired resource has one owner and one release path: each `startRequest` ends once;
  each prepared reply is delivered/canceled/failed once; each sync-write waiter receives once;
  every connection-failure path closes the socket and wakes waiters. Introduce scoped helpers
  (`inFlightRead`, `replyRegistration`, `queuedWrite`) so raw `ReplyHandle` + raw queue-model
  delta plumbing stops spreading by hand.
- **Enforcement:** (a) `go test -race` on `client`/`fdb`/`transport` is a **required** CI job —
  not a name regex; (b) a queue-model invariant test asserts outstanding returns to baseline
  after every hedge arm; (c) a teardown test asserts no goroutine/fd leak after `Close` and after
  monitor-declared-dead (goleak-style).

### P4. Marshalling purity — kills #4

- Request builders are pure over a transaction snapshot. A builder may never mutate
  `tx.mutations`, conflict buffers, the RYW cache, or options. Snapshot mutable state under lock
  first.
- **Enforcement:** every builder that transforms input (tenant prefix, versionstamp offset) has a
  build-twice test asserting byte-identical output and unmodified source buffers.

### P5. Differential conformance is the real backstop — catches what unit tests structurally can't

This is the one that would have caught #2, #3, and #8 without anyone knowing to look. Unit
tests against a hand-built fake reply can encode the same wrong assumption as the code (#2's
fault test did exactly this). The only oracle that can't is the **real binding**.

This is the *expensive* prevention item and therefore the one most likely to never get built —
which would be a tragedy, since it's the only one that catches the bug class that motivated this
RFC. So it is **funded with a concrete minimal scope in this RFC**, not left aspirational:

- **Minimal P5 (in scope for this RFC):** against the existing testcontainer FDB, run the same
  operations through the **native Go client** and the **official `apple/foundationdb` CGo binding**,
  asserting identical `(value, errorCode)` pairs for: point reads, key selectors, a wrong-shard
  scenario (the #2/#3 oracle), and a tenant-prefixed commit (the #4 oracle). That alone would have
  caught #2, #3, and #8. It goes in the Definition of Done with teeth.
- **Full P5 (follow-up RFC, explicitly out of this scope):** all range streaming modes, watches,
  commit-unknown-result, system/special key access, retry-behavior parity. Listed here so it
  isn't silently dropped, but **not** a gate on this batch.
- Any divergence is captured as a minimal reproducer **before** the fix (the regression net),
  per CLAUDE.md "fix bugs as you find them."

### P6. The lesson the prevention plan must encode: no self-confirming tests

#2's fault test injected the wrong code and asserted the wrong-keyed handler fired — it pinned
the bug. The reviewable rule: **a fault-injection test's injected value must come from the
canonical taxonomy (P1), never from the constant the code-under-test uses.** Inject
`codeFor("wrong_shard_server")`, not `ErrWrongShardServer`. This goes in the native-client PR
checklist below.

### Native-client PR checklist (`client`/`fdb`/`transport`/`wire`)

- New protocol parser or request builder → where is the malformed/error test? Builder also has a
  build-twice purity test?
- Starts a request / registers a reply / enqueues a write / takes a pooled resource → where is
  exactly-once cleanup proven?
- New read path → routes through the read state machine and passes the shared injected-error matrix?
- Touches transaction state → which lock or snapshot protects it; does `-race` cover it?
- Changes error handling → which value-equality taxonomy test covers it?
- Fault-injection test → does the injected code come from the canonical table, not the
  code-under-test's own constant?
- Allocation/aliasing/`unsafe` optimization → where is the purity/aliasing regression test?
- Claims C-binding parity → which conformance/differential test pins it?

## Definition of done

Not production-ready until all hold:

- All **High** findings (**#1**, #2, #3, #4, #6, #8) fixed with regression tests; the #2 fault test
  injects 1001 and a negative test rejects 1062.
- **#1 inline wrong-shard** on Get/GetKey/GetRange triggers cache-invalidate+retry (conformance fire).
- **#5 hedge leak** (Medium, but lands in Phase 0): outstanding returns to baseline on all arms.
- Pipelined `Get` and sync `Get` pass the **same** injected-error matrix.
- **N pipelined `Get`s still produce exactly one flush** (batching survived the #3 fix).
- Legal-key-range rejection for pipelined `Get` happens at enqueue, before the frame is on the wire.
- `go test -race ./pkg/fdbgo/client ./pkg/fdbgo/fdb ./pkg/fdbgo/transport` green in CI as a
  required job.
- Connection-close/monitor tests prove no stranded `SendFrame`/`Flush`/pending-reply goroutines
  and no fd leak.
- Tenant commit building proven non-mutating (build-twice).
- The error-constant value-equality test passes, and the **minimal P5 differential run** (point
  reads, key selectors, wrong-shard, tenant commit; native vs CGo binding) passes. (Full P5 is a
  follow-up RFC and is *not* a gate on this batch.)

## Resolved decisions

Both former open questions are resolved here — a plan should not ship with load-bearing unknowns.

1. **#1 inline error — resolved by the conformance review: it is High and REQUIRED.** The open
   question ("does the SS set the inline field for reads?") is answered: **yes, always** — read
   replies derive from `LoadBalancedReply` and the SS routes wrong-shard/future-version/
   process-behind through the inline `reply.error` (`storageserver.actor.cpp:1855-1865`,
   `LoadBalance.actor.h:337-366`), not the `ErrorOr` root. The Go parsers ignore it, so wrong-shard
   reads are silently consumed as empty results. Fixed in Phase 0, co-equal with #2/#3, with its
   *own* nested-Error-table decode (not #8's root-union decode). The minimal P5 differential pins
   it against the C binding.
2. **#7 — resolved: honor the published contract, don't narrow the doc.** The false binary was
   "snapshot everything vs. narrow the doc." The real answer (above): cheaply honor the contract for
   `Set`/`Get`/`Commit`/`GetApproximateSize` via the lock-moves + snapshot, and document only the
   single genuinely-expensive edge (RYW atomic resolve-vs-`Set`, which needs `ryw.mu` held across a
   server round-trip) as not-concurrent-safe. Editing the spec to match broken code is forbidden by
   project doctrine.
