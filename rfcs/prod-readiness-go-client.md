# RFC: Production-readiness assessment — pure-Go FoundationDB client (`pkg/fdbgo`)

**Status:** Assessment (snapshot at HEAD after PR #299). Not a change proposal — a state-of-the-client
audit with an actionable punch-list. Supersedes the client-specific portions of
`docs/review_2026-06-07.md` (the 4.5/10 whole-project SaaS-control-plane review; 6.5/10 for the
client) and the stale `TODO_client.md` (a 2026-06-01 source-audit, 13/16 items since fixed).
**Spec:** FoundationDB C++ `libfdb_c` at tag **7.3.75** (the `foundationdb` pin in `MODULE.bazel`).
C++ is the spec; the differential oracle is the Apple CGo binding over `libfdb_c`. Wire compatibility
is the hard line — Go and C/Java apps share a cluster and read/write each other's data.

**Scope:** `pkg/fdbgo` only (the pure-Go FDB client: `client/`, `transport/`, `fdb/`, `wire/`).
NOT the record layer, SQL engine, or planner.

---

## Verdict

**~7.5/10 for the pure-Go client, up from the 6.5/10 the 2026-06-07 review gave it.** The data-plane
wire path is production-trustworthy on the common path and unusually well-tested (though the gold
differential gate runs nightly, not per-PR); what remains is bounded *operational* edges and
*honesty-of-surface* issues, not common-path correctness defects.

> **Production-usable for the common transactional path with known, documented caveats — not yet a
> blind drop-in `libfdb_c` replacement.** The thing that is hardest to retrofit and most dangerous to
> get wrong on a shared cluster — wire/data correctness — is the strongest part. The gaps are an
> availability gap requiring stable coordinators (cluster-file rotation strands the client), a
> transaction-timeout gap (only a `ctx` deadline bounds a hung read; `SetTimeout` doesn't), and a set
> of silently-inert option setters that can surprise a migrator.

This is a living signal, not a coronation: PR #299 (this work-stream) fixed a *real* cross-client wire
bug in tenant metadata that had shipped green — proof both that the differential method works and that
the client is young enough to still harbor wire bugs in less-traveled corners.

## Method

Four dimension surveys against HEAD, each reading the actual source/tests and the 7.3.75 C++ spec
(not the docs, which drift). Every load-bearing claim below is cited `file:line`. The four axes:
wire correctness, API/feature completeness, operational robustness, test maturity.

## Scorecard

| Dimension | Grade | One-line |
|---|---|---|
| **Wire correctness** | Strong (common path) | 54 go-vs-cgo differential functions pin the hard-line surface byte-for-byte; no divergence produces wrong data on the common path. The one known committed-byte divergence (#3) is confined to a pathological `BYPASS_UNREADABLE` case and is deliberate. |
| **API completeness (hot path)** | Strong | ~95–98% of the transact-closure surface is real wire traffic, not stubs. |
| **Operational robustness** | Good, 2 gaps | Faithful `libfdb_c` failure-handling port; `CRASH_BUG.md` resolved; no panic *found* reachable on wire input (fuzzed) + RFC-110 backstop. |
| **Test maturity** | Strong | ~995 test funcs, real testcontainers (no mocks), 72% client coverage, bindingtester 200+ seeds 0 failures, `-race` PR-gating. |

---

## 1. Wire correctness — STRONG (common path)

The differential suite (`pkg/fdbgo/bench/`, 25 files, **54 `Test/FuzzDifferential*` functions** + the
`libfdbc/differential_test.go` record-layer gold gate) runs the same operation through the pure-Go
client and the real `libfdb_c` CGo binding against ONE shared FDB testcontainer and asserts
byte-identical persisted state. Both clients pinned at API 730; reads pinned to one shared read version
with a re-pin-on-1007 loop so MVCC-window staleness never yields a false mismatch. Many tests also pin
the C++-spec *value*, so a "both clients agree but both wrong" regression is still caught.

Pinned axes: tuple/key codec byte-identity (all type codes, int boundaries, big.Int, float sign-bit,
nil-escape, versionstamp offsets) · all 16 atomic fold semantics (operand/base width × missing/empty/
present-empty) · RYW reads and writes · getKey selectors over pending writes (the former RFC-056
divergence, now green) · getKey conflict-set subtraction · GetRange (streaming-mode invariance, limit,
reverse, selector bounds) · conflict ranges · versionstamp key/value offsets + multi-op same-stamp ·
`accessed_unreadable` (1036) semantics (RFC-098) · size/error codes (2101/2102/2103/2004) · cancel
lifecycle (1025) · RYW-disable poisoning (2000) · snapshot-RYW counter · **tenants** (cross-client CRUD
interop, metadata codec, tenant-prefixed versionstamp +8 offset) · metadataVersionKey write-validation
· **watches** (fire on change/delete/create, RYW-disabled → 1034).

**Open divergences — one (#3) is a deliberate committed-byte divergence in a pathological case; the rest are
correctness-neutral:**

1. **GRV-cache throttle-cooldown gate omitted** (`client/grv.go:274`). The background refresher may start
   slightly earlier under ratekeeper throttle; the *serve* path still throttle-gates (`grv.go:79`), so no
   stale/wrong version is ever served. Timing only; documented in-code.
2. **getKey read-version staleness asymmetry** (RFC-056 item 2, `TODO.md:589`). Under CPU starvation Go's
   getKey can hit `transaction_too_old (1007)` marginally sooner than cgo on the same pinned version. Both
   correctly return 1007 once a version genuinely ages; perf/timing, not a wire divergence.
3. **`BYPASS_UNREADABLE` span-wipe** (`client/ryw.go:46`, comment at `:51`). **This IS observable in committed
   bytes** — the one known committed-byte divergence: C++ `addUnmodifiedAndUnreadableRange` *replaces* the
   write-map span (silently dropping a prior Set inside an SVK candidate range), so C++ never commits that Set
   while Go does. Confined to a pathological `BYPASS_UNREADABLE` write-then-SVK-over-it interleaving; Go's
   keep-the-write behavior is the deliberate, arguably-saner choice. Reviewed.
4. **RYW-disabled read overlapping a local write does not raise 2000** (`differential_getkey_conflict_test.go:273`).
   Pure option-semantics error-surfacing gap; conflict-safety is covered (Go uses the full span — over-conflicts,
   always safe).

**Stale prose, not a bug:** comments in `ryw.go`/`ryw_getkey.go` still describe versionstamp-pending reads as
"reads as ABSENT (our approximation)". RFC-098 closed that — the read paths now hit the unreadable gate and
throw 1036 first (`ryw.go:378`). Documentation hygiene.

**Unprobed axes (latent risk — not known-wrong, but no differential sentinel):**
`commit_unknown_result (1021)` idempotency/retry · `GetRangeSplitPoints`/`GetEstimatedRangeSizeBytes`
*result values* (only error-codes pinned) · locality `GetAddressesForKey` returned addresses ·
cross-shard range-merge. A future wire bug in any of these would not be caught by the current suite.

## 2. API / feature completeness — STRONG on the hot path

~95–98% of the transact-closure surface is present and backed by real wire traffic: Get/GetKey (4
selectors)/GetRange (reverse/limit/streaming) · Set/Clear/ClearRange · all 16 atomics + versionstamp
key/value · conflict ranges · commit/cancel/reset/onError/setReadVersion · GetReadVersion/CommittedVersion/
Versionstamp/ApproximateSize · `GetEstimatedRangeSizeBytes`/`GetRangeSplitPoints` (real
`WaitMetricsRequest`/`SplitRangeRequest`, `client/metrics.go`) · snapshot reads · watches · tenants ·
locality · TLS (`client/tls.go`, real `crypto/tls`) · full directory/subspace/tuple ports.

**Gaps are OUTSIDE the hot path:**

- **The entire NetworkOptions layer is absent** — no `StartNetwork`/network options (Apple exposes 48): no
  trace files, no knobs, no TLS-via-option-API. The single biggest surface gap, and `API_PARITY.md` never
  mentions it.
- **~40 transaction-option setters are silent no-ops** (`fdb/options.go`) that `return nil` and do nothing.
  Most are harmless (tracing/hints/priority). The dangerous subset *alters semantics yet fails silently*:
  `SetRawAccess` (`:214`), `SetAuthorizationToken` (`:303`), `SetAutomaticIdempotency` (`:223`),
  `SetSpecialKeySpace*` (`:206`/`:210`), the causal/durability knobs (`:235`–`:253`). A user migrating from
  the CGo binding gets *no error* when setting a meaningful option that is inert.
- **Special-key-space module absent** — `\xff\xff/status/json`, `\xff\xff/transaction/conflicting_keys`, etc.
  return nothing.
- **Multi-version / external client absent** — by design (pure-Go single-version).
- **`Database.RebootWorker` is a hard stub** (`fdb/database.go:462`, returns `errNotSupported`);
  `LocalityGetBoundaryKeys` ignores its `readVersion` arg (`fdb/database.go:446`); `GetMainThreadBusyness` absent.

`API_PARITY.md` overstates the picture ("full parity") by counting no-op setters as implemented and omitting
NetworkOptions. It should be split into "honored" vs "accepted-but-ignored" tables.

## 3. Operational robustness — GOOD, two gaps

Internals are a disciplined 1:1 port of `libfdb_c`'s failure handling: coordinator contact —
`client/database.go:480` races all coordinators in parallel, first success wins (a benign divergence from
C++'s *sequential* round-robin probe in `monitorProxiesOneGeneration`; same outcome, faster) · `ClientDBInfo`
topology + a 5s `topologyMonitor` poll
(`topology.go:8`) · lazy caller-driven reconnect with immediate in-flight failure (no hang) via
`failConnection`/`failAllPending` (`transport/conn.go:617`) · PING-based dead-connection detection
(`conn.go:860`) · handshake with a 10s deadline + ctx-cancel watcher (`conn.go:178`) · GRV batching + opt-in
cache (`grv.go`) · full C++ `QueueModel` power-of-two load balancer with per-server backoff (`loadbalance.go`)
· request hedging (`hedge.go`) · the `commit_unknown_result` self-conflict + `commitDummyTransaction`
idempotency dance (`commitpath.go:38`) · retryable set exactly matching C++ `fdb_error_predicate(RETRYABLE)`
(`fdb/error.go:436`) · size limits with correct codes (2101/2102/2103) and C++ check ordering.

**`CRASH_BUG.md`: RESOLVED** (reverse-scan `\xff\xff`-to-storage-server and inverted-range commit SIGSEGVs
fixed; all 8 crashing bindingtester seeds pass). **No `panic(` in non-test code has been *found* reachable on
network or untrusted wire input** — every panic is a `Must*` constructor, a tuple-encode-on-bad-caller-input
(matching Apple), or an internal invariant; the decode path (`wire/reader.go`) is exhaustively bounds-checked
and exercised by the 23 fuzz + 17 C++-oracle targets. The **RFC-110 panic backstop** (`client/panicbackstop.go`,
a faithful
`Net2::run` port) wraps every background goroutine. No goroutine leak found (`Close()` cancels ctx then
`wg.Wait()`; long-polls pair receive with `ctx.Done()`).

**SERIOUS gaps:**

1. **Cluster-file coordinator rotation is unobservable.** `db.clusterFile` is read once at open and never
   re-read or watched (`client/database.go:148`). A full coordinator-set change (`coordinators auto`, a coordinator
   replacement) strands the client on the stale list forever — no ctx rescues it. `libfdb_c` monitors and
   rewrites the cluster file. Single most consequential production gap. (Proxy/GRV-proxy changes *within* a
   fixed coordinator set are handled by the topology poll.)
2. **Transaction `SetTimeout` does not bound an in-flight read.** `checkTimeout` (`transaction.go:1829`) is a
   synchronous gate run only at op-entry/commit; `tx.timeout` is never threaded into an RPC wait ctx. Worse, a
   read whose RPC reply times out is *re-sent* up to `maxReadTimeoutRetries` (10) times at `readRPCTimeout` (the
   5s `DefaultRPCTimeout`) without re-running `checkTimeout` (`readpath.go:121,329,573`), so a single
   hung-but-alive Get/GetKey/GetRange can run for up to ~50s **regardless of the `SetTimeout` value — even a
   10s/30s timeout is exceeded.** Only the caller's `ctx` deadline reliably bounds a stuck read (it IS threaded
   into every `waitReply`); `SetTimeout` is honored only at op boundaries. C++'s `timebomb` cancels
   asynchronously. This makes a bounded `ctx` on every `Transact` mandatory, not optional.

**MINOR:** no internal max-retry — runtime loops and `bootstrap` block until success or ctx/db.ctx cancel
(matches C++; callers MUST pass bounded contexts) · location-cache `evictIfNeeded` re-sorts the full slice at
the 600k cap (`locality.go:284`, perf cliff under churn).

## 4. Test maturity — STRONG

~995 Test+Fuzz functions across `pkg/fdbgo`, **real FDB testcontainers, never mocks**; 72% client line
coverage. **23 fuzz targets** (wire reader/`ErrorOr`, 10 reply parsers, wire-type marshal round-trips, RYW
cache, tuple unpack, 3 differential fuzzers) + a **separate 17-target C++-oracle fuzzer** (`cmd/fdb-diff-oracle`)
fuzzing Go wire encode/decode against a linked C++ binary. FDB's **official Python bindingtester** runs against
a Go stacktester (`cmd/fdb-binding-stress`), documented at **200+ seeds × 1000 ops, 0 failures**. Wire compat
is enforced against the real **Java 4.11.1** server per-PR. **`-race` is now PR-gating** over the client
packages (`ci.yml:123`) and already caught a real race (`tx.hadRead` → `atomic.Bool`). `govulncheck` and
`SECURITY.md` exist. The differential suite is **flake-free by construction** — its one harness flake
(pinned-version reads drifting past the 5s MVCC window) was root-caused and pinned
(`differential_stalepin_test.go`), not waved away.

**Caveat — gating reach, not test quality:** the `libfdb_c` differential gold gate and most fuzz targets run
**nightly on a single self-hosted box**, not per-PR — so a fork cannot reproduce the gold gate, and a regression
between nightly runs lands without a per-PR sentinel.

## Movement since the 2026-06-07 baseline

Three of that review's client blockers are now essentially closed: **#2 crash-DoS** (RFC-110 goroutine panic
backstop, PR #297) · **#5 no `-race`** (now PR-gating) · **#6 no escape hatch** (libfdb_c build-tag backend,
RFC-109 / PR #295). Plus PR #298 (CreateTransaction option-default inheritance) and PR #299 (tenant cross-client
codec fix + tenant/watch differentials). The remaining open baseline item touching the client is **P0.4**
(retry/ctx bounds) — partially addressed (ctx now reaches the retry loop; no internal max-retry by design).

## Punch-list (prioritized, actionable)

**P0 — close before "drop-in `libfdb_c` replacement":**
1. **Cluster-file re-watch.** Monitor the cluster file for coordinator-set changes and re-bootstrap the
   coordinator list (port `libfdb_c`'s monitor-and-rewrite). Highest leverage; today a coordinator rotation is
   an unrecoverable strand.
2. **Thread `SetTimeout` into RPC wait contexts** so a transaction timeout cancels an in-flight RPC (port C++
   `timebomb` semantics). Today only a caller `ctx` deadline bounds a hung read; `SetTimeout` is honored only at
   op boundaries, and the read loop re-sends 10× the 5s RPC timeout in between.

**P1 — honesty of surface (cheap, high trust-impact):**
3. **Split `API_PARITY.md`** into "honored" vs "accepted-but-ignored" option tables, and list NetworkOptions as
   out-of-scope. Either make the semantically-meaningful silent no-ops (`SetAuthorizationToken`, `SetRawAccess`,
   `SetAutomaticIdempotency`, causal/durability) return an explicit "unsupported" error, or document them as
   inert — silence is the trap.
4. **Prune `TODO_client.md`** — it reads as a wall of open High-severity bugs but 13/16 are fixed and pinned; a
   future reader concludes the client is unsafe when it isn't.
5. **Document the bounded-context requirement** in godoc/README: with no internal max-retry, a `Transact`/`Open`
   against a down cluster blocks until the caller's `ctx` cancels — a real difference from `libfdb_c`'s internal
   timeouts. Migrators MUST pass bounded contexts.
6. **`LocalityGetBoundaryKeys` should honor its `readVersion`** (`fdb/database.go:446`): today it ignores the arg
   and returns current shard boundaries, a semantic divergence for MVCC-consistent boundary lookups — pin the
   locality read to the supplied version.

**P2 — close the latent gaps:**
7. **Differential sentinels for the unprobed axes:** `commit_unknown_result (1021)` idempotency, split-points /
   estimated-size *result values*, locality addresses, cross-shard range-merge.
8. **Promote the gold gates to per-PR** (or a fast subset): the `libfdb_c` differential + the highest-value fuzz
   targets, so regressions are caught before merge, not nightly.

## Recommendation

- **Use it now** for the read/write/atomic/versionstamp/tenant/range data plane — that surface is well-proven
  against both `libfdb_c` and Java and shares a cluster safely.
- **Operate it with** stable coordinators and a bounded `ctx` deadline on every `Transact` (works around the two
  P0 gaps), and keep the RFC-109 `libfdb_c` escape hatch available for the critical write path until the gold
  gates run per-PR.
- **Top 3 to close** for unqualified production: cluster-file re-watch, `SetTimeout`→RPC-ctx, and the
  `API_PARITY.md` honest-options split.
