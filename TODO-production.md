# TODO — Production Readiness

Derived from `docs/review_2026-06-07.md`. Ordered by criticality so the most important
work is done first. Target use case: **SaaS control plane.**

**Review status (2026-06-07):** reviewed by bradfitz (Go idiom — **governing reference
for the panic/error policy**), Graefe (query-engine, ACK w/ conditions), an FDB C++ dev
(client, ACK w/ conditions), Torvalds (prioritization, ACK w/ changes). See PR #272
comments. Resolutions folded in below.

**Criticality scale**
- **P0 — Blocker.** Do not run in production until fixed. Safety, legal, or data risk.
- **P1 — High.** Fix before relying on it at scale / for real workloads.
- **P2 — Medium.** Maturity and operability; fix before a stable v1 / external adopters.
- **P3 — Low.** Polish.

**Effort:** S = hours · M = 1–3 days · L = >3 days / multi-session.

**Won't-fix (acknowledged):** Bus factor of one — **100% one human** (3330 commits HEAD /
6378 all refs; the apparent "2nd author" is the same person, umlaut difference; zero
external contributors). Structural; mitigate via "own-your-fork" (P1.6) + a second human
owner (the real mitigation Torvalds names), not by eliminating it.

Legend: `[ ]` open · `[~]` in progress · `[x]` done.

---

## Verification pass — 2026-06-24

This doc was written 2026-06-07; a lot landed since. Every then-open item was re-checked against
the codebase (RFCs, CI workflows, code, tests) and the statuses below updated to reality. Headline:

- **Closed since the doc (now `[x]`):** the two continuation/pagination items (RFC-127 #325,
  RFC-128 #326); **P1.9** resource limits / backpressure (RFC-106a + RFC-130 + RFC-028);
  **P3.3** retry-predicate dedup (RFC-105).
- **Done (corrected — first pass was too pessimistic):** **P2.2** libfdb_c escape hatch (RFC-109
  full write-path backend + per-PR differential; **+ entry-point routing in `prod-stack/11`** so
  `-tags libfdbc` now flips the SQL driver + record-layer factory too); **P1.6** own-your-fork gates
  conformance + the bench differential already run per-PR via `bazelisk test //...`; only the 1M
  stress is nightly by design).
- **`[~]` (code landed, process remainder):** **P1.8** CI reproducibility — pinned + checksummed
  artifacts + hosted floor are in-tree, but RFC-108 is still **DRAFT** awaiting Torvalds + codex ACK.
- **Still genuinely OPEN:** **P2.3** the six SQL-engine correctness gaps (query-engine, Graefe-gated,
  several wire-sensitive), **P3.1** idiomatic-API pass (broad, low priority).
- **Deferred by owner:** **P2.1** — `CHANGELOG.md`, `RELEASE.md`, and the stability statement are
  all done (RFC-131/132); **only the `v0.1.0` git tag** is intentionally on hold. Do not cut a
  release tag yet. *(Corrects the first pass, which mis-reported the CHANGELOG as missing.)*
- **Closed by the `prod-stack/*` PRs (2026-06-24):** **P1.2** record-layer half (online-indexer
  progress events → `prod-stack/03`); **P1.7** generated `FEATURE_MATRIX.md` → `prod-stack/05`;
  **P0.3-F** SQL fuzz net (front-end + e2e targets → `prod-stack/06`); **P3.4** operator guide
  (`docs/operations.md` → `prod-stack/07`); **P3.2** `database/sql` example (`example/sql` →
  `prod-stack/08`); **P2.4** full nightly fuzz rotation (`engine-fuzz` job → `prod-stack/10`);
  **P2.2** escape-hatch entry-point routing + **P1.6** gate-placement decision → `prod-stack/11`.
- **Per-PR codex sweep → consolidated fixes (`prod-stack/15`):** codex reviewed each PR against its
  own stacked base. 5 came back clean (#338/#341/#343/#346/#349); the rest surfaced real findings,
  fixed in `prod-stack/15`: libfdb_c `LocalityGetBoundaryKeys` negative-limit panic (#350); the
  online-indexer progress `delay_ms` reporting 0 for rps-throttled builds (#340); FEATURE_MATRIX
  substring miscategorization (#342); 5 operator-guide API inaccuracies / uncompilable snippets
  (#344); the SQL example not being re-runnable (#345); `pkg/rabitq` left out of the fuzz rotation
  (#347); and these TODO inaccuracies (P1.6 conformance/bench-are-per-PR, the statement-timeout knob,
  fuzz counts) (#339). codex also independently re-derived the `BeginTx`-under-libfdbc issue (#348)
  already fixed by #349 — validating that fix.
- **Bug found + fixed by codex review (`prod-stack/14`):** the full-stack codex pass caught a
  cross-module break the per-package reviewers couldn't see — `prod-stack/11` dropped the SQL driver's
  720 API-version pin (→ `fdbclient.Open` default 730), but `cmd/frl`'s catalog path still hard-pinned
  720, so in the `frl sql` REPL a SQL statement (→730) then `\d` (→720) hit FDB's "can't change the
  process-wide API version" error. Fixed by lifting `cmd/frl`'s `fdbAPIVersion` to 730 (completing the
  in-code "lift both to 730 together" TODO; the frl integration TestMain already pinned 730).
- **Bug found + fixed by the new fuzz net (`prod-stack/09`):** `FuzzSQLPlan` immediately surfaced a
  real Cascades planner panic — `values.EqualsWithoutChildren` hit its unhandled-type default on
  `*expr.predicateValue` (a predicate-as-value, e.g. `ORDER BY !amount`), which can't be added to the
  switch without an import cycle. Fixed by a `SelfEqualsWithoutChildren` interface (Java-faithful
  per-type equality); pinned by the fuzz crasher seed + a unit test. *(Query-engine change —
  Graefe + Torvalds gated.)*

---

## ✅ Known deadlock — ROOT-CAUSED + FIXED (was TOP open bug)

**[x] Load-dependent connection deadlock — FIXED.** Originally seen once: under the full
`bazelisk test //...` run (`--local_test_jobs=4`, 4 concurrent FDB containers) `chaos_test` hung
40 min with no progress, no dump; could not reproduce in isolation (0/6).
- **Root cause (found by static audit, then PROVEN):** the pure-Go client's connection handshake
  had **no deadline**. `dialWith` (`pkg/fdbgo/transport/conn.go`) honored ctx only for the TCP
  connect; the TLS upgrade + `WriteConnectPacket`/`ReadConnectPacket` ran with no read/write
  deadline, so `ReadConnectPacket`'s `io.ReadFull` (`handshake.go:126`) blocks **forever** if a
  peer accepts the socket but never sends its ConnectPacket (overloaded fdbserver / Docker-socat
  half-open under load — hence load-dependent + unreproducible solo). ctx cancellation cannot
  interrupt a blocking socket read. And the dial runs under the database's **global `connMu`
  lock**, so one wedged handshake froze *every* connection acquisition → total client wedge.
- **Fix:** bound the handshake with a deadline in `dialWith` (derived from ctx, falling back to
  `defaultHandshakeTimeout = 10s`), cleared before the I/O loops start. A stalled peer now fails
  the dial promptly; the lock is released; the client recovers/retries.
- **Pinned:** `conn_handshake_timeout_test.go` (`TestDial_HandshakeStallTimesOut` +
  `_DeadlineFromCtx`). **Proven:** with the deadline reverted the stall test hangs with
  `panic: test timed out ... handshake.go:126` (the exact `io.ReadFull`); with the fix it returns
  in ~0.4s.
- **Defense-in-depth kept:** `chaos_test` stays `size = "large"` (was `"enormous"`) so any future
  hang auto-dumps goroutines at 900s instead of running an hour. The four `timeout = "eternal"`
  targets still warrant an audit (bench/stress may legitimately need it; `recordlayer` likely
  does not).
- **Follow-up (latency, not deadlock):** the dial still holds `connMu` for the (now-bounded)
  handshake; under contention that serializes connection setup for up to the deadline. Dialing
  outside the lock + inserting under it is a separate concurrency refinement — filed, low priority.

### Client robustness — tracked follow-ups (non-hazardous, found during the review hunt)
- **[x] Honor ctx *cancellation* (not just deadline) during the handshake** (bradfitz + FDB C++) —
  **FIXED.** `dialWith` bounded the handshake with a deadline only, so a cancel-only ctx (no
  deadline) waited up to `defaultHandshakeTimeout` (10s) before aborting. Fixed with a cancellation
  watcher started before the TLS upgrade (`conn.go`): on `ctx.Done()` it pushes a past deadline onto
  the raw TCP conn (a stable handle the TLS wrapper delegates `SetDeadline` to), unblocking the
  in-flight TLS handshake AND ConnectPacket read immediately. The watcher is stopped+joined before
  the handshake deadline is cleared, so a late post-handshake dial-ctx cancellation can't disturb the
  now-live conn (its lifetime is `connCtx` from there). Pinned by
  `TestDial_HandshakeHonorsCancellation` (cancel-only ctx aborts in ~200ms vs the 10s default).
- **[x] Thread live ctx to the commit-path GRV for write txns (Commit-internal)** (codex + FDB C++).
  **DONE — RFC-093** (FDB C++ + Torvalds ACK on RFC + impl). The two-line split landed exactly as the
  executable spec called for: `database.go:548` passes the live ctx into `Commit` (was
  `context.WithoutCancel(ctx)`), and `transaction.go:1126` re-applies `WithoutCancel` to ONLY the
  commit RPC + `commit_unknown_result` barrier (`tx.commit`). Net: the commit-path GRV
  (`ensureReadVersion`, :1106) now honors the caller ctx (a cancel mid-GRV returns via
  `getReadVersion`'s `<-ctx.Done()` select, `grv.go:216` → non-`*wire.FDBError` → `OnError`:1243
  non-retryable), while the commit RPC + `commitDummyTransaction` stay detached (RFC-090 idempotency
  intact). The reverted-P2 forced-GRV regression is structurally avoided — the read-only/no-op fast
  path returns at :1100 before any GRV. **FDB C++ verified against 7.3.77** (`NativeAPI.actor.cpp:6578`
  GRV is a cancellable read; `:6750`/`:6306` `commitDummyTransaction` is the only no-abandon path — the
  Go split matches where C++ draws the line). Three stale comments at the old call site
  (`database.go:527-536` "also detaches the GRV", the `:538-549` "tracked as a follow-up" NOTE, and the
  RFC-090 rationale) were rewritten/relocated so none lie post-change. **Pinned** by
  `commit_path_grv_ctx_test.go` (`TestFDB_CommitPathGRV_HonorsCtxCancel` — a frame-level
  GRV-reply-blocking dialer holds the GRV reply, cancels mid-flight, asserts `context.Canceled` +
  key-absent; revert-proven to FAIL on the two-line revert — "GRV ignored the cancel"; and
  `TestFDB_CommitReadOnlyNoForcedGRV` — guards the fast path against a forced GRV). `-race` green;
  deterministic. Reviewer gate: the new **`fdb-client-review`** skill (FDB C++ dev + Torvalds), now
  the standing gate for `pkg/fdbgo` client/wire work and wired into `todo-worker` Step 1.
- **[x] `sendWatch` long-poll escape — AUDITED, safe (matches Java).** `sendWatch` (`readpath.go:855`)
  blocks in a `select` with TWO escapes: the watch reply on `replyCh`, and `ctx.Done()`
  (`readpath.go:905`). On connection teardown, `failAllPending` (`conn.go:744`) does a non-blocking
  send of the error — safe because `replyCh` is **buffered cap-1** (`replyChanPool`, `conn.go:32`), so
  the signal lands in the buffer even before `sendWatch` reaches the `select` (no dropped wake). The
  connectionMonitor (ping/pong 750ms / 2s timeout) tears down a stalled conn → `failAllPending` →
  retry next server. The only residual is a server that answers monitor pings yet silently drops the
  watch registration: the watch then waits on the caller's ctx — which is correct, because FDB
  watches are long-poll with no internal timeout *by design* (Java behaves identically; a watch with
  no caller deadline is meant to be long-lived). No divergence, no fix.

### Write-path / index-maintainer — hunt findings (verified against Java by FDB C++ review)
The Concatenate-index panic (the CRITICAL one) is FIXED above. The rest were all run by FDB C++
against Java — wire-compat is the hard line, so nothing was changed on unverified reasoning:
- **[x] VECTOR insert: silent-skip of an undecodable non-null vector — FIXED.** A non-null but
  undecodable vector (bad serialized bytes / non-numeric element) was saved UNINDEXED and silently
  (`tupleToVector` returned nil → `if vector == nil { continue }`), so a vector search missed the
  row. Java's `RealVector.fromBytes` throws and fails the write (FDB C++ confirmed). Fix:
  `tupleToVector` now returns `([]float64, error)` — `(nil,nil)` for an absent/null vector (still
  skipped, matches Java), an error for an undecodable non-null one (Update fails the write). Dead
  `extractVector` removed. Pinned by `TestTupleToVector` (proven: pre-fix the undecodable case
  returns `([], nil)` → unindexed; post-fix it errors).
- **[~] float32 leaderboard negate — VERIFIED NOT A DIVERGENCE; do not change.** `negateScore`'s
  `-float64(v)` matches Java exactly: `TupleHelpers.negate` Float/Double arm returns
  `-number.doubleValue()` (always a `Double` → 0x21). Changing Go to `-v` (float32 → 0x20) would
  *create* a cross-engine divergence. Closed.
- **[~] VECTOR `hnsw.Insert` no dimension check — NOT a Java-parity gap; closed.** Neither Java's
  `VectorIndexMaintainer`/`HNSW` validates `len(vector)==NumDimensions` on insert (only `Config`
  checks `numDimensions>=1`); both surface a mismatch later via distance `Preconditions`. Go matches
  Java on this axis. (Separate from the silent-skip above, which IS fixed.)
- **[~] SUM/MIN/MAX_EVER float→int64 truncation — VERIFIED matches Java; do NOT change (wire).**
  Checked against Java 4.11.1.0. Java's atomic mutations are LONG-only: the enum has `SUM_LONG`,
  `MAX_EVER_LONG`, `MIN_EVER_LONG` — no double/float variant (`AtomicMutation.java:123-135`). At write
  time `getMutationParam` casts the grouped key value to `Number` and calls `numVal.longValue()`
  (SUM_LONG `:187`, MAX/MIN_EVER_LONG `:199`) — `Double.longValue()` truncates toward zero, exactly
  like Go's `toInt64` (`atomic_index_helpers.go:203`, whose comment already says "matching Java's
  Number.longValue()"). And `AtomicMutationIndexMaintainerFactory.validate` (`:93-120`) checks only
  the GROUPING structure + version, never the field type — so Java *accepts* a SUM index on a DOUBLE
  field and truncates it; it does not reject it. Therefore Go matches Java 1:1: preserving the
  fraction (or erroring) would write different index bytes than Java for the same record — a wire
  divergence. Same verdict family as the float32-leaderboard-negate item above: not a bug, leave it.

### Continuation / pagination — hunt findings
- **[x] Iterator-error swallowing was a BUG CLASS, swept across all scan cursors — FIXED (PR after #272).**
  The `hasMoreKVs` fix generalized: every cursor that reported `SourceExhausted` on
  `iterator.Advance()==false` without checking `iterator.Get()` swallowed a transient FDB error
  (`*fdb.RangeIterator.Advance()` returns false on both clean exhaustion AND a fetch error; `Get()`
  then returns the stored error or nil). Fixed at both the row-limit boundary and the scan-termination
  site in `indexCursor`, `countKVCursor`, `bitmapKVCursor`, `recordKeyCursor`, plus the two BunchedMap
  iterators (`BunchedMapMultiIterator.nextKV` — backs the live text-index scan via `textCursor.Err()`
  — and `BunchedMapIterator.advance`). Each switched to the `rangeIterator` seam + white-box tests
  (both sites). Additive; no happy-path change.
- **[x] hnsw scan loops swallowed a mid-scan iterator error — FIXED.** `hnsw.go` had the same class in
  local iterators: the `for iter.Advance() {}` loops (`preloadLayer`, `loadNodeLayerInlining`,
  `preloadLayerInlining`) checked `Get()` only INSIDE the loop, so a mid-scan 1007/timeout that ended
  the loop was swallowed → a silently PARTIAL layer cache / neighbor list (corrupt-graph hazard that
  still commits); the `if ri.Advance()` probes (`findAnyNodeAtLayer`/`…Inlining`) reported the
  misleading "no nodes at layer" on a transient error. Added a post-loop / else `Get()` error check at
  all five sites. For deterministic testing without a live FDB, introduced an `hnswStorage.scan` seam
  (nil in production → real `tx.GetRange().Iterator()` via `scanIter`; tests inject a fake iterator);
  five white-box regressions pin each site, and the `preloadLayer` test was verified to fail pre-fix.
  **Caller propagation (codex P1):** surfacing the error at the leaf is necessary but not sufficient —
  the graph callers (`Delete` all-layers + entry-point, `searchLayerGreedy`, `searchLayerMulti`,
  `selectNeighborsHeuristic`, `pruneNeighbors`, `repairNeighbor`) treated EVERY load error as "node
  absent" and skipped it, so a transient error still produced a partial insert/delete/search that
  committed. Fixed with an `errHNSWNotPresent` sentinel: genuine not-found returns are wrapped with it,
  and each caller now does `if e := hnswFatal(err); e != nil { return e }` before the absent-case skip —
  propagating transient errors (tx aborts/retries) while still skipping genuinely-absent nodes. Pinned
  by an operation-level regression (`repairNeighbor` propagates a scan error vs skips an absent
  neighbor), verified to fail pre-fix.
- **[x] `hasMoreKVs` swallowed FDB iterator errors at the row-limit boundary — FIXED.** At the
  `ReturnedRowLimit` boundary, `hasMoreKVs` returned `iterator.Advance()` without checking
  `iterator.Get()` for the stored error — so a transient `transaction_too_old` (1007) / timeout
  landing exactly there was read as end-of-data → `SourceExhausted` → silent loss of all remaining
  rows. Fixed: `hasMoreKVs` now returns `(bool, error)` and the caller surfaces it (mirrors
  `nextKV`'s post-Advance error check). Additive (no happy-path change).
- **[x] Continuation serialization was `TO_OLD` (raw) vs Java 4.11.1.0 `TO_NEW` (proto+magic)**
  — **WIRE; FIXED.** FDB C++ verified (tag 4.11.1.0): `KeyValueCursorBase.Builder` defaults
  `serializationMode=TO_NEW` (line 284); no production path selects `TO_OLD`. So Java emits a
  proto-wrapped `KeyValueCursorContinuation{inner_continuation, magic_number=6773487359078157740}`,
  never raw. Go emitted the raw key suffix — byte-divergent (both engines were merely read-tolerant
  of each other) + a time-bomb (Java's raw-fallback is slated to throw). Fixed: `wrapContinuation`
  (`key_value_cursor.go:25`) now `MarshalVT`s the proto (nil suffix → nil, never wrap an end
  position; empty-but-present suffix → proto carrying the magic, wire-distinguishable from
  start/end). Same raw→proto fix applied to the second emitter, `record_key_cursor.go:114`. The
  dual-read `unwrapContinuation` still accepts legacy raw tokens, and index/bitmap/count scans
  already routed through `wrapContinuation`. Pinned by a cross-engine conformance regression
  asserting Go's continuation bytes are **byte-identical** to Java's for the same scan position
  (`continuation_conformance_test.go`), plus unit tests for the empty-suffix + round-trip cases.
- **[x] `executeLimit` skip/limit state not in the continuation — DONE (RFC-128, PR #326,
  Graefe + Torvalds ACK + @claude follow-ups).** The audit's literal P1 (executor re-skip on
  resume) was shielded/latent; RFC-128 went further and removed the post-execution LIMIT hoist,
  making LIMIT a uniform `RecordQueryLimitPlan` operator with a mandatory continuation envelope.
  `executeLimit` (now `executor/executor.go:800`) decodes a `LimitContinuation` (`{inner
  continuation, remaining offset, remaining limit}`) and the `limitEnvelopeCursor` resumes
  mid-window — never re-skipping `offset`, never resetting `limit`. Pinned by
  `limit_envelope_test.go` (`TestLimitEnvelope_ResumeAcrossPage_NoReSkip`, `_OffsetSpansPageBreak`,
  `_ZeroLimit`, round-trip + garbage-rejection). Verified green at HEAD.
- **[x] limit-before-first-row → `StartContinuation` read as "exhausted" — DONE (RFC-127, PR #325,
  Graefe + Torvalds ACK + @claude follow-up).** The old `fetchPage` inferred exhaustion from
  `ToBytes()==nil`, but a non-end `StartContinuation` is byte-identical to an `EndContinuation` →
  silent result-set truncation. Fixed by `pageContinuationState`
  (`embedded/cascades_generator.go:1411`): exhaustion is now decided by `IsEnd()`
  (≡ `SourceExhausted`), **never** by bytes — aligning Go with Java's `SOURCE_EXHAUSTED`-only
  invariant. The out-of-band+nil-bytes case raises `ScanLimitReachedError` (→ 54F01) instead of
  truncating; `LIMIT 0` (in-band `ReturnLimitReached`) is clean exhaustion.
  - **Reachability (per RFC-127's `/code-review` trace):** the data-loss path was **latent, not
    live** — every Go leaf cursor reports out-of-band only after `scanned>0` (→ `BytesContinuation`,
    which the old code resumed correctly), so no current cursor emits a no-next out-of-band+START.
    The fix is a correctness/invariant hardening (exhaustion from `IsEnd`, not bytes) + makes the
    `LIMIT 0` handling explicit — a latent landmine closed before any future Union/Intersection/
    MapWhile-style cursor can trip it. Pinned by `cascades_generator_pagination_test.go`
    (`TestPageContinuationState` decision table, `TestContinuationExhaustionByIsEndNotBytes`).
    Verified green at HEAD.
- **[~] `autoContinuingCursor` retry-after-limit-stop — ANALYZED, no duplicate rows (perf-only).**
  Re-read `onNextWithRetry`/`OnNext` (`cursor_combinators.go:780-847`) this round. On a retryable
  error the retry resumes from `lastContinuation()` = `c.lastResult` (the last RETURNED value's
  continuation, set only at `:813`). That is the CORRECT dedup-to-caller anchor: the caller has seen
  everything ≤ `lastResult`, so resuming after it can never re-emit a returned row — even when a
  limit-stop opened a new cursor from a position *ahead* of `lastResult` (e.g. a filtered scan that
  consumed but didn't return), since those skipped records were never returned. The only cost is
  re-scanning the `[lastResult, limit-stop]` gap on a retry — redundant work, not a correctness bug.
  Downgraded from "possible DUPLICATE rows" to perf-nit; no fix.

---

## P0 — Blockers (before any production use)

### [x] P0.1 — Add a LICENSE (legal blocker) · S — DONE
**Why:** No LICENSE file existed, yet README:281 links `[LICENSE]`. Unlicensed = "all rights
reserved" — cannot legally deploy. Derivative of Apache-2.0 `fdb-record-layer`.
**Done:** added `LICENSE` (verbatim Apache-2.0, "Copyright 2025 The fdb-record-layer-go
Authors") + `NOTICE` attributing the FoundationDB Record Layer port (Apache-2.0, tag
4.11.1.0), the `proto/apple/` protos, and the FoundationDB wire protocol. README link now
resolves. **Remaining (owner's call):** confirm the copyright holder name (currently "The
fdb-record-layer-go Authors") + obtain internal legal sign-off — the file is in place.

### [x] P0.2 — Boundary recover + network-goroutine teardown (the hours-not-weeks crash fix) · S — DONE
**Do FIRST, before the P0.3 sweep** (Torvalds): don't run a multi-tenant process that
crashes on `SELECT 1/0` for the weeks the sweep takes. Build the net, then refactor behind
it. This is the minimal realization of the P0.3 policy:
- [x] db/sql boundary recover spanning the FULL query path — `ExecContext`/`QueryContext`
  (`connection.go`) **and** `paginatingRows.Next`/`cascadesRows.Next` (`cascades_generator.go`):
  catch-all → `debug.Stack()` (first) → log SERIOUS server-side → generic internal error
  (panic value never leaked to the caller) → keep serving.
- [x] recover→`failConnection` in `readLoop` (inline, `conn.go:619`), `writeLoop` +
  `connectionMonitor` (`recoverLoop`, `conn.go:432,782`) — a panic there is otherwise an
  uncatchable whole-host crash. The false `conn.go` comment is corrected (`:612-615`) and the
  `exitErr` ordering is right (a real read error is preserved; the deferred recover overwrites
  only on an actual panic).
**Done when:** a panicking query returns an error (process survives); a panic in a network
goroutine fails only that connection; tests for both. — **MET:** `connection_recover_test.go`
(`TestRecoveredPanicError`: generic `ErrCodeInternalError` out, panic value logged SERIOUS but
NOT in the caller-visible message) + `conn_recover_test.go`
(`TestRecoverLoop_ContainsPanicAndFailsConnection`: containment proven, ctx cancelled, pending
replies carry the failure error, logged SERIOUS with loop + cause).

### [x] P0.3 — Panic/error discipline: errors on the data path, assert internally, recover at every goroutine boundary
**Status: policy fully realized; all sub-items closed (F closed 2026-06-24, `prod-stack/06`).**
CLASSIFY ✓ · A1 (eval→error) ✓ · GATE ✓ · A2 (delete 6 recovers) ✓ · B (metadata typed error) ✓ ·
C (network-goroutine recovers) ✓ · D (parser FFI recovers + fuzz) ✓ · E (resolver `.Get()`) ✓ ·
F (SQL fuzz net — `FuzzSQLPlan` front-end + `FuzzSQL_QueryContext` e2e) ✓ ·
G (intersection int32 coercion, RFC-092, Graefe+Torvalds ACK) ✓. The library never panics to a
caller; the data path returns errors; remaining panics are documented internal invariants /
`Must*`-API boundary.
**Governing policy (bradfitz — Go's "don't *leak* panics" convention, à la `encoding/json`):**
a library must never let a panic cross its API boundary; it need not, and should not, avoid
panic *internally*.
- **Data / runtime / external-reachable** (SQL eval `1/0`/overflow/CAST/type-mismatch,
  malformed records, malformed wire bytes, bad args, not-found, bad DDL) → **return errors.**
  Always.
- **Genuine "can't happen unless our code is broken" invariants** → **stay `panic`
  internally.** Do **NOT** thread `error` through the ~134 internal invariant sites
  (BiMap/AliasMap/memo/matchers/arity) — a non-error-returning helper's only alternative to
  panic is silent corruption, which is worse. Assertions stay assertions; hot paths stay clean.
- **Every goroutine boundary recovers** (this is what makes the library never-panic to
  callers — the `encoding/json` pattern): catch-all → `debug.Stack()` (first) → log SERIOUS
  → generic internal error / `failConnection`. Never splice the panic value into a
  tenant-visible message. Boundaries: (1) db/sql `connection.go:305,336` [new, P0.2];
  (2) FDB `transaction.go:508 panicToError` [**KEEP** — also the `Must*`-API boundary,
  Apple parity]; (3) the 3 network goroutines [new, P0.2]; (4) **the remaining client
  background goroutines — `topologyMonitor`, `backgroundRefresher`, the GRV-batch `flush`,
  `dialAndPool`/`tryOneCoordinator`, and the 6 facade futures — [RFC-110]**: P0.2 scoped
  recover to only "the 3 network goroutines", leaving these as process-abort surfaces; RFC-110
  closes that gap with `Net2::run`-faithful semantics (recover→backoff→continue for standing
  loops, fail-the-batch / fail-the-leg / fail-the-op elsewhere; Go runtime-fatals still crash,
  matching C++'s deliberate hard-exit classes).
- **No re-panic, no sentinel-panic taxonomy** (resolved against — bradfitz). It's Java/C++
  exception-taxonomy thinking: destroys the Go stack trace, is already defeated by the
  executor recovers, and the asserts are code bugs not corruption detectors (a panic during
  query exec rolls back the FDB txn → nothing corrupt persisted). A genuine on-disk
  corruption *detector*, if ever written, fail-stops un-recoverably at the storage layer
  (`os.Exit`), not as a boundary sentinel.
- **Init/wiring-time panics** (`MustCompile`-class: matcher constructors, rule-registry dup,
  `AliasMapOf` odd args) are fine — fire at startup/dev, never reachable by a tenant.

Caller's view: the library never panics. Internal view: assertions remain assertions.

**Current state:** 158 panics, 11 recovers. Only **~24 panics are user-reachable** (`docs/
panic-audit.md`); ~134 are legitimate asserts / by-design `Must*` API. **Keep** the 3 parser
FFI guards (`parser.go:39,99,121` — ANTLR runtime panics by design), `panicToError`, and the
new boundary recovers. **Delete only** the eval/control-flow recovers that substitute for
returning an error: `executor.go:734,918,2505`, `executor_new_plans.go:337`, `values.go:416`,
`simplifier_value.go:218`, `merge_cursor.go:24` (pending P0.3-G).

**Blast radius (bradfitz):** `Value.Evaluate(ctx) any → (any, error)` is **~500 sites incl.
tests** (63 impls + 125 non-test call sites + 334+ in the values tests). Precedent next door:
`KeyExpression.Evaluate` already returns `([][]any, error)`. Reject error-in-context /
accumulator / sumtype alternatives.

**Phased worklist (STAGED — mechanical first, gate, then net-removal):**
- [x] **P0.3-CLASSIFY** — `docs/panic-audit.md`.
- [x] **P0.3-A1 — eval signature + plumbing, mechanical** (query-engine, **Graefe ACK**, `M`) — DONE (RFC-091).
  `Value.Evaluate → (any, error)` + `QueryPredicate.Eval(ctx) TriBool → (TriBool, error)`.
  **Split per-package** (`values/` → `predicates/` → `executor/`), not one 500-site commit
  (Torvalds). Typed errors + SQLSTATE map already exist (`translateExecError`). Do the
  `ReportUnresolvedReference` global fix (`values.go:777`, P1.1) in this same sweep — don't
  touch values.go twice. **Verify Kleene short-circuit semantics** (`FALSE AND 1/0`→FALSE;
  `1/0 AND FALSE`→error): check `err` per-child before the TriBool switch; pin both orderings
  (Graefe).
- [x] **GATE** — conformance + 1M stress + **`-race`**, **per-query/seeded** diff — DONE
  (RFC-091; the `keep=false` silent-row-drop bug was pinned + fixed before the gate; `-race`
  surfaced + fixed the `hadRead` client race, see P1.1).
- [x] **P0.3-A2 — delete the 6 eval/cursor control-flow recovers** (separate commit) — DONE (RFC-091).
- [x] **P0.3-B — record/metadata/key-expr panics → errors** (`M`) — DONE (audited; one real
  fix, the rest are correctly-classified construction-time guards).
  - **`metadata.go:476` (`RecordMetaDataBuilder.GetRecordType`)**: confirmed the "caller expects
    nil" bug — `metadata/builder.go:270` guarded the call with `if rt == nil` but the method
    *panics* on a missing type, so the guard was dead. **Fixed** by pre-checking the
    nil-returning `GetRecordTypes()` map so `Build()` (already `(*X, error)`) returns the typed
    `ErrCodeInternalError` instead of a panic the boundary recover would flatten to a
    context-free "internal error". `GetRecordType`'s panic contract is **kept** — its only other
    callers are the catalog system-table fluent chains (`catalog/metadata.go:48/56/63`,
    `b.GetRecordType(X).SetPrimaryKey(...)`) where a missing constant-named system table is a
    genuine can't-happen invariant (programmer error, backstopped by the boundary recover).
  - **`key_expression.go:1133` (`Literal`)**: **keep panic** — it's a construction-time
    (`MustCompile`-class) type guard on a Go-API call building metadata, never reachable by a
    tenant data path. Converting it would violate the bradfitz policy (don't thread errors
    through can't-happen / programmer-error guards).
- [x] **P0.3-C — network goroutines: ADD recover→failConnection** (`S`) — DONE (landed with
  P0.2: `recoverLoop` on writeLoop/connectionMonitor, inline recover in readLoop, comment +
  `exitErr` ordering fixed, `conn_recover_test.go`).
- [x] **P0.3-D — parser: KEEP the FFI recovers, expand fuzz** (`S`) — DONE. The ANTLR-FFI
  recovers are kept (they guard the ANTLR Go runtime, which panics by design). Fuzz coverage
  exists: `parser/fuzz_test.go` has `FuzzParse`, `FuzzParseFunction`, `FuzzParseView`, plus
  `plangen/plangen_fuzz_test.go`. *(Corrected: collecting listener already exists; recovers
  guard the ANTLR runtime — do NOT delete.)*
- [x] **P0.3-E — `Must*`: keep `panicToError`, switch internal callers to `.Get()`** (`S`) — DONE
  (split by parity, not blanket-converted). *(Corrected per FDB C++ — do NOT delete
  `panicToError`; it's the `Must*` boundary.)*
  - **`keyspace/fdb_resolver.go:65/72/148` → switched to `.Get()`.** This is record-layer code
    mirroring Java's `LocatableResolver` (explicit `CompletableFuture` error handling), not an
    Apple-binding port. A routine transaction conflict (1020) on these reads is an expected,
    retryable event — flowing it back as an error for `db.Transact`/`ReadTransact` to retry is
    correct (principle #4: don't panic for expected conditions); the old `.MustGet()` panicked
    on every conflict and bounced through `panicToError`. Same outcome, no panic round-trip.
  - **`directory/directoryLayer.go:449/594/595/610`, `directory/node.go:63` → KEPT `.MustGet()`.**
    This package is a 1:1 port of Apple's Go directory layer, which deliberately uses `MustGet`
    + Transact-level recovery throughout (`node.go:63` is even a `bool` method — converting it
    would change the ported signature). Switching here would *diverge* from the Apple Go binding
    (a client-spec violation), so parity wins. `panicToError` is the documented boundary.
- [x] **P0.3-F — fuzz net** (`M`) — DONE. The eval/value layer (where panics actually hide) was
  already well-fuzzed: `embedded_test.go` has `FuzzApplyMathOp`, `FuzzApplyBitOp`,
  `FuzzCompareValues`, `FuzzCastValue`, `FuzzLikePrefixStrinc`, `FuzzLikePatternToPrefix`; `values/`
  has `FuzzSimplifyValue_ArithmeticTree`/`_CastChain`, `FuzzAndOrValue_Kleene3VL`,
  `FuzzCaseExpression_FirstMatchWins`, etc. The remaining **SQL-string → no-panic** gap is now
  closed by a complementary pair (2026-06-24):
  - **`FuzzSQLPlan`** (`pkg/relational/core/embedded`) — SQL string → parse → semantic analysis →
    Cascades SELECT planning, calling `planSelectCascades` **directly** (NOT through the boundary
    recover), so a planner/semantic panic is surfaced to the fuzzer, not swallowed. Container-free
    and fast (runs in CI fuzz rotation). **760k execs, 0 panics** on the first run.
  - **`FuzzSQL_QueryContext`** (`pkg/relational/sqldriver`) — the literal item: SQL string +
    seed rows → real `database/sql` → planner → executor → FDB, asserting **no panic escapes the
    db/sql boundary** (the never-panic-to-caller guarantee). Container-gated + shallow by nature
    (each input is a real transaction), so the seed corpus runs in CI and active fuzzing is opt-in.
  The boundary recover remains the production backstop; the front-end fuzz now finds the panics the
  recover would otherwise only mask.
- [x] **P0.3-G — comparison-key type coercion: make the 3 sibling builders consistent**
  (`M`, query-engine) — **DONE (RFC-092, Graefe ACK + Torvalds ACK).** Both intersection
  builders now widen `int32→int64` via a shared `widenInt32` helper; regression
  (`intersection_compkey_test.go`) proven to fail pre-fix (`unencodable element ... type int32`)
  and pass post-fix (byte-equal to `int64` encoding ⇒ order-preserving). Graefe verified the
  index key encoding already widens int32→int64 (`key_expression_compiled.go:117`), so the merge
  key matches the children's sort order and reproduces Java's uniform-`Long` semantics. uint32 is
  a documented symmetric, currently-unreachable follow-up. *Investigated 2026-06-07; framing
  corrected.* Findings:
  - `merge_cursor.go:24`'s recover is **already gone** — the executor `merge_cursor.go` was
    deleted in the A2 sweep; the real merge cursor is `pkg/recordlayer/merge_cursor.go`, whose
    `compareKeys` does `bytes.Compare(a.Pack(), b.Pack())` and **recovers** `tuple.Pack` panics
    into an error (pinned by `bug_bounty_test.go::TestBug2_UnionCursorMixedKeyTypesPanic`). So an
    unpackable key is an *error, not a crash* today.
  - The `extractKey` "%T:%v ordering bug" framing is **wrong**: ordering uses
    `compareValues` (semantic), `extractKey` is only the *dedup* key, and its `%T:%v` is
    consistent with `compareValues`' own `%v` fallback for any *single typed* sort column (the
    `%T` prefix can't break within-type equality). Not a live bug.
  - **The real item:** `intersectionCompKeyFunc` (`executor.go:1402`) and
    `multiIntersectionCompKeyFunc` (`executor_new_plans.go:149`) store the **raw** `Evaluate`
    result (`t[i] = v`), unlike `extractKey` and `streaming_cursors.go:233`, which coerce
    `int32→int64` and exotic→`%T:%v`. `int32` *is* produced by the value layer
    (`values.go:615,1790`), and `tuple` has no native `int32` — so an `int32`/exotic comparison
    key in an INTERSECTION currently makes the query **error out** (via `compareKeys`' Pack-recover)
    instead of returning rows. Narrow (int32-keyed intersection) but a real availability gap, and
    a 3-way inconsistency. **Fix:** factor `extractKey`'s coercion into a shared helper and use it
    in all three builders; pin with an int32-keyed intersection regression. Needs Graefe ACK
    (executor change).

**Done when:** the library never panics to a caller (boundary recovers in place); data-path
panics are errors; remaining panics are documented internal asserts; conformance + stress +
`-race` green across the staged commits.

**RFC-091 status (2026-06-07):** Step 1 (twins) + fold-fix + 3 uncovered eval sites + A1
(executor migration) + A2 (delete all 6 control-flow recovers) **landed, both gates ACK'd**
(Graefe: "Cascades-correct, safe to merge"; Torvalds: "no NAK, the fix is real"). Eval errors
now return everywhere (div0→22012 etc.); the keep=false silent-row-drop is fixed + pinned.
**Follow-ups (tracked):**
- [x] **Collapse** (Torvalds a/b) **DONE** — verified: **0** `EvaluateErr`/`EvalErr` twins and
  **0** dead non-error `Evaluate(...) any` wrappers remain in `pkg/`. The interfaces are the
  single error-returning forms (`values.go:130` `Evaluate(any) (any, error)`; `predicates.go:202`
  `Eval(any) (TriBool, error)`). All flagged callers migrated and thread the error: the 5
  plan-time rule sites (`rule_implement_in_join.go:469`, `rule_implement_in_union.go:95`,
  `rule_in_to_explode.go:96`, `rule_simplify.go:381`, `physical_vector_index_scan_wrapper.go:96`)
  all use `, err :=`; the `value_range.go` helpers (`EvaluateAsStream`/`Cardinality`) correctly
  degrade on error (`nil` / `(-1,false)` — the documented non-foldable / unknown-cardinality
  cost-model path, not a swallowed correctness bug). No dual eval methods left to ossify.
- [x] **GATE completed**: conformance + FDB green at every commit; `-race` on
  `//pkg/relational/...` run (found + fixed the `hadRead` client race — see P1.1); 1M stress
  green (all subtests pass, durations consistent with the baseline — no bulk-path regression).
- [x] **P0.2 gap CLOSED:** `paginatingRows.Next` + `cascadesRows.Next` (cascades_generator.go)
  now `recover()` → `recoveredPanicError` — the db/sql boundary recover spans the FULL query
  path (planning + first page via QueryContext/ExecContext, later pages via Rows.Next). A panic
  during later-page iteration becomes a generic internal error, not a host crash; user eval
  errors already return cleanly via the sweep. (`-race` job into CI still tracked under P1.1.)

### [x] P0.4 — Bound retries + propagate `ctx` (promoted from P1 — availability blocker) · M — DONE
**Why (Torvalds: this is P0 for a control plane, not P1):** `FDBDatabase.Run(ctx,…)`'s ctx
never reaches the retry loop (`Database.Transact` takes no ctx, runs on `context.Background()`
— `database.go:108,197`); default retry limit unlimited → a hot 1020 retries forever and the
caller **cannot cancel it**. `OpenWithConnectionString`/`OpenDatabaseFromConfig` block forever
on an unavailable cluster (`database.go:119,133`) vs `OpenDatabase`'s 60s cap.
**Done (RFC-090, FDB C++ ACK):**
- [x] ctx routed into the retry loop — `FDBDatabase.Run` → `runTransactCtx(ctx)` → the
  low-level `client.Database.Transact(ctx,…)`, which now checks ctx at every retry point
  (`database.go:347` backoff-select, `:515`/`:550` pre-attempt `ctx.Err()` returns). The loop
  that used to run on `context.Background()` now observes the caller's ctx.
- [x] commit-detach (Option B): commit + the `commit_unknown_result` barrier run under
  `context.WithoutCancel(ctx)` (`client/database.go:527`) so a late ctx cancel can't tear a
  commit in half (ambiguous-write hazard).
- [x] all `Open*` bootstrap bounded/consistent via `bootstrapContext` +
  `defaultBootstrapTimeout = 60s` (`fdb/database.go:96,102,117,147`).
- [x] **unlimited-retry default kept** (libfdb_c parity, `RETRY_LIMIT=-1`); the caller's ctx
  deadline is the bound.
- **Default transaction timeout — deliberately NOT shipped (parity decision, not a punt):**
  `d.timeout` stays `0 = disabled`, matching libfdb_c's default. Per the project's hard rule
  ("C++ is the spec for the FDB client — Go divergence is a bug in Go"), shipping a non-zero
  default would be a self-inflicted client divergence. The dangerous combination the item
  warned against (ctx-severed **and** timeout-off **and** retry-unlimited) is broken because
  ctx is no longer severed — ctx is now the cancellation mechanism. Callers wanting a hard
  per-tx cap set `DatabaseOptions.SetTransactionTimeout` / a ctx deadline (both honored).
**Done when:** a cancelled ctx aborts retries promptly; unavailable cluster fails bounded on
every Open path; tests for both. — **MET:** `transact_ctx_fdb_test.go`
(`TestFDB_TransactCtx_RetryLoopBoundedByCtxDeadline`: a permanently-retryable 1020 under a ctx
deadline surfaces an error after retrying, not an infinite loop;
`TestFDB_TransactCtx_CommitNotCancelledByCtx`: commit survives mid-tx cancel) +
`database_bootstrap_test.go` (`TestBootstrapContext`: bootstrap deadline bounded by
`defaultBootstrapTimeout`).

---

## P1 — High (before relying on it at scale)

### [x] P1.1 — `-race` on the SQL layer in CI (+ fix what it finds) · M — DONE
Main CI runs `//...` with no race detector (`ci.yml:41`); nightly covers 5 targets and
**excludes all of `//pkg/relational/...`** (plan cache, driver, planner). Add a required
`-race` job on `//pkg/relational/...`; fix `ReportUnresolvedReference` (`values.go:777`, in the
P0.3-A1 sweep); root-cause any race surfaced (no skips). **Pull this forward to run in parallel
with P0.3** (Torvalds) — a plan-cache race under `database/sql` concurrency is silent
corruption, worse than a loud panic.

**[x] First `-race` run on `//pkg/relational/...` done (RFC-091 GATE) — surfaced + FIXED a real
pre-existing client race.** `tx.hadRead` (pure-Go client `Transaction`) is written `true` from
concurrent pipelined-read resolution goroutines — `loadRecordStoreState` (store_state_cache.go:191)
issues `tx.Get` + `tx.Snapshot().GetRange` in flight together and their futures resolve on
separate goroutines, both setting the shared `bool`. 266 race instances across sqldriver +
plandiff, all the same field. Unrelated to RFC-091 (the eval sweep touches none of the client
read path) — exactly the class the audit predicted behind the no-`-race`-on-relational gap. Fix:
`hadRead` → `atomic.Bool` (build + copylocks-vet clean; re-ran `-race` on both targets → green).
**[x] CI job added:** `ci.yml` now has a gating **Race detector (SQL layer)** job —
`bazelisk test //pkg/relational/... --@rules_go//go/config:race --test_tag_filters=-stress`.
Confirmed green across the FULL layer post-`hadRead`-fix (18/18, 0 races).
**Residual (minor, tracked):** the `ReportUnresolvedReference` global (`values.go:777`) did NOT
surface under `-race` (tests set it only in `TestMain`/serially), but it's still a process-global
func pointer read on the eval hot path — convert to `atomic.Pointer`/set-once when convenient.

### [x] P1.2 — Observability: pluggable logger · M — DONE (both halves)
**Decision resolved: interfaces-only, no new core deps** (per the project's "simple code / no
heavy deps" principles; an OTel/Prometheus adapter ships as a separate optional package — see P1.3).
- [x] **Pluggable diagnostics via `log/slog`.** The SERIOUS panic-recovery logs (db/sql boundary
  `connection.go`, network goroutines `conn.go`) now route through `slog.Default().Error` instead
  of bare `log.Printf`. This makes the library's diagnostics pluggable via the *standard* Go
  mechanism — apps call `slog.SetDefault` with their own handler (JSON, levels, collector) — with
  zero record-layer-specific logging API to learn. (`seriousLogf` stays a test-capture seam.)
- [x] **Emit operational events** — DONE.
  - CLIENT half (RFC-097, with P1.3): retry events (per-code, incl. 1020) at Debug +
    `commit_unknown_result` at Warn flow through a per-handle logger (`client.WithLogger`, nil →
    `slog.Default()` — tests never mutate process globals), `Enabled`-guarded so the disabled-level
    hot path is one branch; same source as the counters.
  - RECORD-LAYER half (online-indexer progress events): the Go `OnlineIndexer`
    (`pkg/recordlayer/online_indexer.go`) now emits Java's per-range **"Indexer: Built Range"** INFO
    event, throttled by a 1:1 port of `IndexingBase.shouldLogBuildProgress` /
    `progressLogIntervalMillis` (default `-1` = off, matching Java `DEFAULT_PROGRESS_LOG_INTERVAL`),
    via a per-indexer `slog.Logger` (`SetLogger`, nil → `slog.Default()`). The `Enabled(INFO)` guard
    runs before the throttle so a disabled level advances no clock (Java's `isInfoEnabled() &&
    shouldLogBuildProgress()` short-circuit). Pinned by `online_indexer_progress_log_test.go`
    (throttle semantics, emission, disabled-level short-circuit) + an FDB Ginkgo test
    (`online_indexer_test.go` — events fire across a real multi-range build).

### [x] P1.3 — Observability: conflict/retry metrics + export hook · M — DONE (RFC-097)
`ClientMetrics` on every `Database` handle: the C++ `DatabaseContext` TransactionMetrics
subset with C++ names and 1:1 increment sites (commit started `commitMutations`-style —
read-only fast path NOT counted, matching `NativeAPI.actor.cpp:6800-6808`; per-code retry
counters at the OnError arms `:7749-:7772`; GRV completions per real batched reply,
cache hits excluded, `:7428-7440`) + a Go-only `transactionRetries` aggregate.
Export hook = `Database.Metrics()` poll (monotonic snapshot; Prometheus/OTel are
pull-based consumers of exactly this shape). Shipped adapter: `pkg/fdbgo/fdbmetrics` —
zero-dependency Prometheus **text-exposition** `http.Handler` + runnable example
(deliberately not a `prometheus.Collector`: no client_golang dep for 14 counters; a
Collector over `Metrics()` is documented as trivial). Pinned by counter-delta FDB tests
(clean commit, read-only zero-delta, forced conflict, dummy-barrier commits counted) —
conflict wiring revert-proven — plus the per-code mapping unit table, slog-event level
tests via a per-handle capturing handler, and `-race` green.

### [ ] P1.4 — ~~retry/ctx bounds~~ → **PROMOTED to P0.4.**

### [x] P1.5 — `govulncheck` + supply-chain hygiene in CI · S — DONE
Added a **Vulnerability scan** job to `ci.yml` (`govulncheck` over the shipped `pkg/...`,
excluding `pkg/testcontainers`), a `SECURITY.md` (private reporting + scope + dep policy).
The first scan found **7 called vulns**: 5 in `golang.org/x/crypto@v0.48.0` → **bumped to
v0.52.0** (fixed; transitively pulled x/text v0.37, x/tools v0.44; bazel build green, no
MODULE.bazel.lock drift), and 2 in `github.com/docker/docker` (test-infra only, **Fixed in:
N/A** upstream) — excluded from the production scan + documented in SECURITY.md. Post-bump
scan: production-clean (only the 2 docker N/A remain, test-only).

### [x] P1.6 — Own-your-fork CI gates (bus-factor mitigation) · M — DONE
*(Corrected 2026-06-25 per codex per-PR review — the gates are MORE complete than the first pass
recorded.)* The required gates already run per-PR; only the heaviest is nightly by design:
- **Conformance + the bench differential are per-PR** — `ci.yml`'s test job runs
  `bazelisk test //... --test_tag_filters=-stress`, and `tests(//...)` includes
  `//conformance:conformance_test` **and** `//pkg/fdbgo/bench:bench_test` (which owns the non-fuzz
  `differential_*_test.go`). So both are required on every PR (and conformance ALSO runs in the
  nightly race + coverage jobs — additive). The earlier "conformance nightly-only / bench in no
  workflow" claim was wrong.
- **libfdb_c gold differential** — additionally a dedicated per-PR gate (`nightly-libfdbc.yml` on
  `push`+`pull_request`) over `pkg/fdbgo/libfdbc` + `fdbclient` (byte-identical cross-client).
- **1M stress** — nightly (`nightly-stress.yml`), correctness-asserting; off-PR **by design**
  (RFC-107: too heavy for every PR) — the one gate the owner keeps nightly.
Caveat (Torvalds): detection, not repair — see the won't-fix note.

### [x] P1.7 — Reconcile contradictory docs · S — DONE (README + docs guard + generated FEATURE_MATRIX.md)
README's "Not yet supported" listed **6 features; 5 were already implemented** (verified
against the yamsql corpus + DIVERGENCES.md): LEFT/RIGHT/FULL OUTER JOIN, LIMIT/OFFSET,
subqueries-in-WHERE (EXISTS / IN (SELECT) / correlated scalar), mixed ASC/DESC, scalar
functions (UPPER/LOWER). The "no physical sort operator / ORDER BY needs an index" claim
was also stale (Go has a Go-only `RecordQueryInMemorySortPlan`). Rewrote the README SQL
section to the accurate surface + a dated pointer to the authoritative source (yamsql corpus
+ DIVERGENCES.md), keeping only genuine gaps (CTE-in-UNION-branch, DML `IN (SELECT)`, general
window functions, synthetic record types). **Remaining:** a generated/maintained
`FEATURE_MATRIX.md` (deferred — the dated README pointer is the interim source of truth).

**Update (RFC-131, PR — 2026-06-20):** the earlier SQL-section rewrite missed a separate README
contradiction — the client-maturity row (README:19) still claimed "no drop-in escape hatch to the C
client yet" while the build commands document the `-tags libfdbc` escape hatch. RFC-131 fixed that,
de-staled the hard-coded "accurate as of <date>" SQL-summary date, archived the six stale 2026-03-09
`reports/*.md` to `docs/archive/`, and added a `docs_consistency_test.go` guard (Java target anchored
to MODULE.bazel; README contradiction check). The doc-drift portion of this item is now closed.

**Update (2026-06-24):** the last deferred piece — a **generated `FEATURE_MATRIX.md`** — is now DONE.
`FEATURE_MATRIX.md` is generated from the yamsql corpus (one row per scenario: name, #cases, and the
scenario's own leading-comment description, grouped by feature area) via
`cmd/gen-feature-matrix` / `just feature-matrix`, and **cannot rot**: `TestFeatureMatrixUpToDate`
(`pkg/relational/conformance/yamsql`) regenerates and fails the build on drift (red→green proven).
README links it. **P1.7 is now fully closed.**

### [~] P1.8 — CI reproducibility / supply chain · M — implementation landed; RFC-108 still DRAFT
*(Verified 2026-06-24.)* The work is in-tree and verifiable: every downloaded runner artifact is
pinned **and SHA-256-verified** via `fetch_verified` (`infra/cloud-init.yaml`; `infra/main.tf`
`locals.versions` — `runner_sha256`, `bazelisk_sha256`, `just_sha256`, `mc_sha256`,
`fdb_clients_sha256`). FDB skew fixed to `7.3.77` (matches `MODULE.bazel`); GitHub runner pinned
(was `releases/latest`); ephemeral-runner opt-in (`runner_ephemeral`); stand-up documented
(`infra/README.md`). A GitHub-hosted reproducibility floor (`hosted-smoke.yml` on `ubuntu-latest`
— build+vet+pure unit tests) gives external adopters a Docker-free green signal. The self-hosted
box is retained for the heavy gates; bus-factor-of-one remains the acknowledged won't-fix.
**Remaining (process, not code):** `rfcs/108-ci-reproducibility.md` is still **DRAFT — awaiting
Torvalds + codex ACK**; mark `[x]` once accepted.

### [x] P1.9 — Resource limits / backpressure (multi-tenant noisy-neighbor) · M — DONE (RFC-106a + RFC-130 + RFC-028)
*(Verified 2026-06-24.)* All four query resource bounds exist and surface as errors (54F01 /
`StatementMemoryBudgetExceededError`), never crashes:
- **Statement timeout** — the whole-statement wall-clock deadline is `SetStatementTimeout` /
  `EmbeddedConnection.statementTimeout`, applied in `cascades_generator.go` via
  `context.WithTimeoutCause(ctx, statementTimeout, errStatementTimeout)`. (Distinct from
  `EXECUTION_TIME_LIMIT` / `OptExecutionTimeLimit`, which sets only the per-PAGE
  `ExecuteProperties.TimeLimit`, clamped to the 4s `txPageTimeLimit`.) Leaf enforcement in
  `key_value_cursor.go`. *(codex per-PR review corrected the earlier conflation of the two knobs.)*
- **Max-rows / result-size cap** — JDBC `MAX_ROWS` total cap (`paginatingRows.maxRows`),
  `pageRowBudget` → `WithReturnedRowLimit`; `OptMaxRows`.
- **Query memory budget** — RFC-130 extended RFC-028's row-only `MaterializationLimit` to a
  statement-wide **byte** budget over **streaming AND eager** intermediates
  (`ExecuteState.ChargeMemory` → `StatementMemoryBudgetExceededError`; streaming cursors carry
  `st` and charge per-row).
- **Scan / byte limits** — `ScannedRecordsLimit` / `ScannedBytesLimit` →
  `ScanLimitReachedError` (`FailOnScanLimitReached` → 54F01).

---

## P2 — Medium (before stable v1 / external adopters)

### [~] P2.1 — Releases, semver, CHANGELOG · S — docs DONE; only the tag cut is DEFERRED (owner's call)
*(Verified 2026-06-24 — correcting the first reconciliation pass, which wrongly reported "no
CHANGELOG".)* Everything except the actual tag is in place (RFC-131/132 era, 2026-06-20):
- **`CHANGELOG.md`** — Keep-a-Changelog format with an `[Unreleased]` section and a per-entry
  **Compatibility** block (wire / SQL / client-option / deps); guarded by `pkg/docscheck`.
- **`RELEASE.md`** — versioning + support policy: `v0.MINOR.PATCH`, Go API unstable pre-1.0, FDB
  wire format the stable hard line across every tag.
- **Stability statement** — `README.md` (pre-1.0 maturity row) + `RELEASE.md` per-layer policy +
  `SECURITY.md`.

**Remaining: only the `git tag` itself** — no `v0.x` cut yet, and **cutting `v0.1.0` is
intentionally on hold per the owner. Do NOT cut a release branch/tag yet.** When unheld, the cut is
mechanical (move `[Unreleased]` → `v0.1.0`, tag).

### [x] P2.2 — libfdb_c escape hatch · L — DONE (RFC-109 + entry-point routing, `prod-stack/11`)
*(Completed 2026-06-25.)* A build-tag-selectable libfdb_c backend, with the two real production open
paths now routed through it:
- **Seam:** `fdbclient.Open` chooses at build time — `pkg/internal/fdbclient/open_purego.go`
  (`//go:build !libfdbc`, default) vs `open_libfdbc.go` (`//go:build libfdbc`); the default build
  never imports/links cgo.
- **Backend:** `pkg/fdbgo/libfdbc/backend.go` (`//go:build cgo && libfdbc`) is a full
  `WritableTransaction` — the **write path IS covered** (`Set`/`Clear`/`Add`/`SetVersionstamped*`/
  conflict ranges/`Commit`), ctx-bounded retry, compile-time conformance assertions, `backend_stub.go`
  for `!cgo`. Wired via `recordlayer.NewFDBDatabaseWithBackend`.
- **Routing (this shift):** `FDBDatabaseFactory.GetDatabase` and `sqldriver/driver.go` now open via
  `fdbclient.Open` + `NewFDBDatabaseWithBackend`, so **`-tags libfdbc` flips the SQL driver and the
  record-layer factory too** — no source edit. Verified: default bazel build green; `go build -tags
  libfdbc` of `recordlayer` + `sqldriver` + `fdbclient` compiles. Default build is behavior-preserving
  — `NewFDBDatabaseWithBackend` keeps the pure-Go concrete handle, so CreateTransaction/locality are
  unaffected.
- **Gate:** `nightly-libfdbc.yml` runs a per-PR + nightly byte-identical cross-client differential.
- Build-tag selection (not runtime config) is deliberate — the libfdb_c network thread is
  once-per-process.

**Standalone transactions — now backend-agnostic (`prod-stack/12`).** The SQL engine's `database/sql`
**explicit transactions** (`BeginTx`/`COMMIT`, which span multiple driver calls and so can't use the
closure-based `Run` gold path) need a long-lived transaction handle. That used to be pure-Go-only
(`FDBDatabase.CreateTransaction()` returned the **concrete** `fdb.Transaction` and fail-fasted on a
non-pure-Go backend) — a self-inflicted narrow Go interface, NOT a C limitation (the C client creates
transactions just as well; the cgo backend already does so internally). Fixed per rule #2 (Go
divergence from C is a Go bug) by widening the abstraction: `BackendDatabase` gains
`CreateWritableTransaction() (WritableTransaction, error)` (the interface, not the concrete type),
implemented on both the pure-Go client and the libfdb_c backend (cgo handle is GC-finalized, so no
extra lifecycle). `FDBDatabase.CreateWritableTransaction` is backend-agnostic; the SQL engine
(`connection.go` `BeginTx`) and the `FDBDatabaseRunner` now use it, so **explicit transactions work on
libfdb_c too**. Pinned: `TestLibFDBC_CreateWritableTransaction` (real-FDB standalone tx on the C
backend, nightly-libfdbc CI) + the existing `TestFDB_BeginCommit`/`_BeginRollback`/`_SQLCommitRollback`
prove the pure-Go path through the new method.

**Locality / mutual indexing — now backend-agnostic (`prod-stack/13`).** `LocalityGetBoundaryKeys`
(the shard-boundary read the online MUTUAL indexer uses to partition the keyspace for concurrent
building) was the last pure-Go-only path. Closed the same way: `BackendDatabase` gains
`LocalityGetBoundaryKeys(...) ([]Key, error)`, implemented on the libfdb_c backend via Apple's
binding (a read of `\xff/keyServers`, byte-identical to the pure-Go client);
`FDBDatabase.LocalityGetBoundaryKeys` is backend-agnostic and the mutual indexer uses it, so mutual
indexing **parallelizes on libfdb_c** instead of degrading to a single fragment. Pinned by
`TestLibFDBC_LocalityGetBoundaryKeys` (a cross-client differential — both clients return byte-equal
boundaries).

**Net: nothing in `pkg/recordlayer` or `pkg/relational` is pure-Go-only anymore** — SQL (auto-commit
+ explicit transactions), record CRUD, regular + mutual index builds all work on either backend.
Tenants remain the declared v1 non-goal (the only acknowledged libfdb_c scope boundary).

### [ ] P2.3 — Close known SQL-engine correctness gaps · L (query-engine, Graefe-gated)
*(Verified 2026-06-24: all six still OPEN in TODO.md; no RFC ≤145 and no commit closes any.)*
- **Row-count nondeterminism** — the 3-way arithmetic-join probe scenario is still **excluded
  cross-engine** (`yamsql_cross_engine_conformance_test.go`, tracked under RFC-042); not yet proven
  deterministic. **Treat as P0/P1 correctness** (a datastore returning different row counts across
  runs, shelved by excluding the scenario instead of root-causing it — against the no-skips rule).
  Note: not currently flagged P0/P1 in TODO.md itself — it sits under the RFC-042 follow-up.
- **INSERT…SELECT qualified-agg NULL** (TODO.md ~1037) — `wrapBareAggregateInsertSource` still
  skips qualified-operand sources.
- **0-row coercion** (TODO.md ~1040, RFC-083 follow-up) — residual cases rely on the runtime converter.
- **bare GROUP BY INSERT…SELECT** (TODO.md ~1041, RFC-083 follow-up) — `LogicalAggregate` source guard defers.
- **divergent-named aggregate union NULL** (TODO.md ~1046) — gated via `aggregateNamesStableForUnion`.
- **`GetIndexTypeName` MIN/MAX_EVER** (TODO.md ~1042) — `values.go:2653-2656` still hardcodes
  `MIN_EVER_LONG`/`MAX_EVER_LONG`; needs a `_TUPLE` arm.

### [x] P2.4 — Broaden fuzz coverage in CI · S — DONE
*(2026-06-25; counts corrected per codex per-PR review.)* The tree has **140** `func Fuzz` targets.
Nightly active-fuzz coverage: diff-fuzz (18, `cmd/fdb-diff-oracle`) + client-fuzz (26, `//pkg/fdbgo`)
+ the new **`engine-fuzz`** job (`nightly-fuzz.yml`), which discovers and fuzzes the
`//pkg/relational` + `//pkg/recordlayer` **+ `//pkg/rabitq`** targets at 60s each — the previously
seed-replay-only set (was 8 actively-fuzzed at the 2026-06-07 baseline). With rabitq added (the lone
target codex caught outside the original scope), **every Go-native Fuzz target is now actively fuzzed
nightly.** Same DISCOVERY + valid-label + no-op-guard pattern as client-fuzz (a gazelle rename fails
loudly). Per-PR, `ci.yml` replays every `Fuzz*` seed corpus. Crash corpus **published as 30-day CI
artifacts** on failure. *(This net immediately earned its keep: `FuzzSQLPlan` found the
predicate-as-value planner panic fixed in `prod-stack/09`.)*

### [x] P2.5 — Pin FDB image version in tests · S — DONE
The test infra was already pinned to a single specific version, never `:latest`:
`foundationdb.Run`/`RunCluster` build `foundationdb/foundationdb:%s` from
`defaultOptions().version = fdbVersion()` = **7.3.77** (overridable via the `FDB_VERSION` env
that `.bazelrc:30` sets to `7.3.77` for every Bazel test). That matches `MODULE.bazel`
(7.3.77) and the README target table. The only drift was the README quickstart docker snippet
(`README.md:239`), which used `7.3.63` — **fixed to 7.3.77**. All FDB version references now
reconcile on 7.3.77.

---

## P3 — Low (polish before v1 promise)

### [ ] P3.1 — Idiomatic Go API pass · M
*(Verified 2026-06-24: untouched.)* Still pervasive — ~810 `GetX()` accessors in `pkg/recordlayer`,
54 in `pkg/fdbgo`; `WithX`/`SetX`/`AddX` builder chains throughout. Broad subjective polish, no
scoped plan. Constraint: wire/serialization accessors are load-bearing, so any rename pass must be
behavior-preserving. Low priority. Make `go doc` read like a Go library.

### [x] P3.2 — Quickstart + realistic examples that compile in CI · S — DONE
*(2026-06-24.)* Added `example/sql/main.go` (`//example/sql:sql`) — a realistic **`database/sql`**
quickstart: `fdbsql` DSN open, DDL (database/template/schema + a secondary index), parameterized
`INSERT` (`?` placeholders), a point query, and a `GROUP BY` aggregate over the index. It documents
the `-tags libfdbc` C-client switch in its header. Both `example/sql` and the existing record-layer
`example/getting_started.go` **compile in CI** (picked up by `bazelisk build //...`); README's
"Getting started" now links both runnable examples.

### [x] P3.3 — De-duplicate the two retry predicates · S — DONE (RFC-105)
*(Verified 2026-06-24.)* Investigation found the "two" were in fact **three intentionally
distinct C++ predicates**, not drift. `onErrorRetryable` (`client/commitpath.go`) is now the
single source for the `onError` set — both `Transaction.OnError` and `commitDummyTransaction`
call it. `fdb.IsRetryable` (12 `fdb_error_predicate` codes) and `fdb.IsOnErrorRetryable` (16
codes) are *correctly* different C++ predicates, each C++-pinned by tests; the cross-package
duplication is forced by an import cycle (the cgo backend can't import `client`) and documented at
the call site. Drift risk eliminated by construction + revert-proven tests.

### [x] P3.4 — Operator guide · M — DONE
*(2026-06-24.)* `docs/operations.md` written, covering all eight topics with the **real** Go API
surface (verified against the code, not invented): cluster file + DSN + the `-tags libfdbc` escape
hatch; retry/ctx (`TransactCtx`, unlimited-retry-bounded-by-ctx, `Database.Options()` knobs); tx
limits (5s/100KB/10MB/10KB + split records); online-index lifecycle (`OnlineIndexerBuilder` knobs +
defaults); index-state transitions (Disabled/WriteOnly/Readable/ReadableUniquePending); schema-
evolution safety (format version 14 + the wire-compat safe/unsafe list); backup/restore (none in the
layer — use `fdbbackup`/`fdbrestore`); observability (`Database.Metrics()`, `fdbmetrics` Prometheus
handler, slog, online-index progress events, `PlanGenerationLogger`). Linked from README.

---

## Suggested execution order

1. **P0.1** (license — minutes).
2. **P0.2** (boundary recover + network-goroutine teardown — hours; caps blast radius before the sweep).
3. **P0.4** (retry/ctx bounds — availability blocker).
4. **P0.3-A1** (eval `(any,error)` sweep, per-package, + the values.go race fix — Graefe ACK).
5. **GATE** (conformance + 1M stress + `-race`, seeded/per-query diff; pin the keep=false bug first).
6. **P0.3-A2** (delete the 6 control-flow recovers).
7. **P0.3-B/C/D/E/G** (record/metadata; goroutine recovers [w/ P0.2]; parser keep+fuzz; MustGet→.Get(); tuple.Pack).
8. **P0.3-F** (executor + e2e fuzz net).
9. **In parallel with P0.3:** **P1.1** (`-race` in CI), **P1.5** (govulncheck), **P1.7 + P2.1** (docs + release).
10. **P1.2 + P1.3** (observability); **P1.9** (resource limits); **P1.8** (CI reproducibility).
11. **P2.3** (SQL correctness — incl. promoted nondeterminism, Graefe-gated); **P2.2** (libfdb_c — mandatory for write path).
