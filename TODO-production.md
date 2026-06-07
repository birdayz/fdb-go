# TODO — Production Readiness

Derived from `review_2026-06-07.md`. Ordered by criticality so the most important
work is done first. Target use case: **SaaS control plane.**

**Criticality scale**
- **P0 — Blocker.** Do not run in production until fixed. Safety, legal, or data risk.
- **P1 — High.** Fix before relying on it at scale / for real workloads.
- **P2 — Medium.** Maturity and operability; fix before a stable v1 / external adopters.
- **P3 — Low.** Polish.

**Effort:** S = hours · M = 1–3 days · L = >3 days / multi-session.

**Won't-fix (acknowledged):** Bus factor of one (single author, 0 external
contributors). Structural; mitigate via the "own-your-fork" items in P1/P2, not by
eliminating it.

Legend: `[ ]` open · `[~]` in progress · `[x]` done.

---

## P0 — Blockers (before any production use)

### [ ] P0.1 — Add a LICENSE (legal blocker) · S
**Why:** No LICENSE file exists anywhere (`git ls-files | grep -i licen` → empty), yet
README:281 links `[LICENSE]`. Unlicensed code is "all rights reserved" — cannot legally
be deployed. This is a derivative of Apache-2.0 `fdb-record-layer`, so the license must
be Apache-2.0-compatible.
**Do:**
- Add `LICENSE` (Apache-2.0).
- Add `NOTICE` attributing Apple/FoundationDB Record Layer + the Apache protos.
- Decide internal vs OSS distribution; if dual-licensing, add the second file.
**Done when:** `LICENSE` + `NOTICE` are tracked; README link resolves; legal sign-off.

### [~] P0.2 — Panic/error discipline: errors for the reachable, assert for the impossible, two trust-boundary recovers
**Decided policy (2026-06-07, revised after bradfitz review) — scoped fail-stop:**
- **Reachable from user input or external/untrusted data** → **returned error** with the
  right code. Never a panic. Covers: SQL eval (`1/0`, overflow, CAST, type mismatch),
  malformed records, malformed wire frames, bad DDL, unsupported user values.
- **Genuine fundamental invariant** ("our code is broken / storage state may be corrupt")
  → **assert (panic), fail-stop.** Crashing loudly is correct — never serve from a state
  you can't trust. Record-layer + fdbgo asserts stay fail-stop.
- **Remove the 8 control-flow recovers.** Keep exactly **two trust boundaries**:
  1. **Parser FFI guard** (`parser.go:39,99,121`, all 3 entry points). The parser already
     uses a collecting error listener; these recovers exist to contain the **ANTLR
     generated runtime**, which panics on adversarial input by design (dozens of panic
     sites in the runtime; can't fix without forking). Deleting them turns caught syntax
     errors into host crashes. This is an FFI boundary, NOT control-flow recover. **Keep.**
  2. **One defense-in-depth recover at the `database/sql` boundary**
     (`connection.go:305,336` ExecContext/QueryContext) — logs a SERIOUS error + stack,
     returns an internal error, does **not** kill the process. Rationale (bradfitz): the
     classification asserts ~134 planner panics are unreachable-by-argument, on a Cascades
     optimizer with known open bugs; in multi-tenant, tenant A's pathological query must
     not drop tenant B's in-flight txns. Bounds the blast radius when the classification
     is wrong on a query shape. Scopes fail-stop to storage-state invariants only.
- **Net:** safe only if untrusted-input paths are exhaustively error-returning — enforced
  by fuzz. The boundary recover is belt-and-suspenders for what fuzz inevitably misses.

**Current state:** 158 panics, 11 recovers. Eval signals user errors *by panicking*
(`values.go:1700` `1/0` → `panic`); the `database/sql` boundary catches nothing today;
the network goroutines (`conn.go:265-267`, etc.) have zero recover and the `readLoop`
comment (`conn.go:586`) falsely claims panic safety (latent process-killer). Full
classification in `docs/panic-audit.md`. **Only ~24 panics are user-reachable; ~134 are
legitimate asserts / by-design `Must*` API.**

**Blast radius (corrected — bradfitz):** the `Value.Evaluate(ctx) any → (any, error)`
change is **~500 sites incl. tests**, not 140: 63 impls + 125 non-test call sites + 334+
call sites in the values tests alone (they won't compile otherwise). Precedent next door:
`KeyExpression.Evaluate` already returns `([][]any, error)`. **Reject** error-in-context /
error-accumulator / sumtype alternatives (out-of-band side channels lose "error here,
propagate now"). Do the boring `(T, error)` sweep.

**Phased worklist (STAGED — mechanical first, net-removal second, with gates between):**
- [x] **P0.2-CLASSIFY** — done. `docs/panic-audit.md`.
- [ ] **P0.2-A1 — eval signature + error plumbing, mechanical** (query-engine, **Graefe
  ACK**, `M`). `Value.Evaluate → (any, error)` and `QueryPredicate.Eval(ctx) TriBool →
  (TriBool, error)`; every panic site becomes `return …, &TypedErr{}`; every call site
  threads the error. Typed errors + SQLSTATE map already exist
  (`cascades_generator.go:1135 translateExecError`) — this is plumbing, not new taxonomy.
  Codes: overflow→22003, div0→22012, cast→22F3H, type-mismatch→22000/42804 (keep
  existing mappings). **One commit, no behavior change beyond panic→error.**
  *(Do the `ReportUnresolvedReference` global fix — `values.go:777`, P1.1 — in this same
  sweep; don't touch values.go twice.)*
- [ ] **GATE** — run conformance + 1M stress, diff row counts vs baseline. Catches latent
  bugs the recovers were masking before the net is removed.
- [ ] **P0.2-A2 — delete the 6 eval/cursor control-flow recovers** (`executor.go:734,918,
  2505`, `executor_new_plans.go:337`, `values.go:416`, `simplifier_value.go:218`),
  separate commit. **Pin the `executor.go:739` `keep=false` bug first** — an unexpected
  panic there silently *drops result rows* (projection path errors at :929; filter path
  doesn't). Real correctness bug; regression test per CLAUDE.md.
- [ ] **P0.2-B — record/metadata/key-expr panics → errors** (`M`).
  `metadata.go:476` (unknown record type — real bug: DDL builder caller already expects
  nil return), `key_expression.go:1133` (unsupported literal). Both callers already return
  error; audit `catalog/metadata.go:48/56/63` non-nil assumptions.
- [ ] **P0.2-C — wire/transport stays error-clean; kill the dangerous comment** (`S`).
  Decode path already returns errors on bad bytes (verified) → no goroutine recover
  needed. Delete the false `conn.go:586` comment (latent process-killer, not just stale);
  fix `exitErr` so pending callers get the real cause; confirm with the existing wire fuzz.
- [ ] **P0.2-D — parser: KEEP the FFI recovers, expand fuzz** (`S`). *(Corrected: the
  collecting listener already exists; the recovers guard the ANTLR runtime — do NOT
  delete.)* Document them as the FFI boundary; add parser fuzz seeds for adversarial input.
- [ ] **P0.2-E — `Must*` convention** (`S`). Keep `future.MustGet()` as a documented
  `Must` API; switch the 8 internal callers to `.Get()` (`directory/directoryLayer.go:449/
  594/595/610`, `directory/node.go:63`, `keyspace/fdb_resolver.go:65/72/148`); THEN delete
  `transaction.go:509 panicToError` (safe only after the callers switch).
- [ ] **P0.2-F — fuzz net** (`M`). Build the missing targets: executor-package fuzz, and
  an **e2e SQL-string + seed-rows → `QueryContext` → assert no-panic** target. Honest
  caveat: e2e fuzz needs a container so it's *shallow* — which is exactly why P0.2's
  db/sql boundary recover stays.
- [ ] **P0.2-G — resolve `tuple.Pack()` reachability** (was a footnote; promoted). Does
  the record→`ComparisonKeyFunc`→`tuple.Pack` path (recovered at `merge_cursor.go:24`)
  ever see a user-data type that's unencodable? If yes → needs an error-returning encode
  variant (own blast radius; the tuple-encode "asserts" aren't all invariants). If no →
  the recover is dead code; delete it, asserts stay. **Resolve before deleting
  `merge_cursor.go:24`.**

**Done when:** only the 2 trust-boundary recovers remain; no panic reachable from
user/external input (proven by fuzz); remaining panics are documented invariant asserts;
conformance + stress green across the staged commits.

---

## P1 — High (before relying on it at scale)

### [ ] P1.1 — `-race` on the SQL layer in CI (+ fix what it finds) · M
**Why:** Main CI runs `//...` with no race detector (`ci.yml:41`); nightly `-race`
covers only 5 targets and **excludes all of `//pkg/relational/...`** (plan cache, driver,
planner) — exactly what `database/sql` drives concurrently. "Looks locked" ≠ "proven."
**Do:**
- Add a required CI job running `-race` on `//pkg/relational/...` (and broaden nightly
  `race-detector` toward `//...` minus stress).
- Fix `ReportUnresolvedReference` global (`values.go:777`) → `atomic.Pointer` or
  set-once; it will trip `-race` immediately.
- Root-cause and fix any race surfaced (no skips, per CLAUDE.md).
**Done when:** `-race` on the relational layer is green in required CI.

### [ ] P1.2 — Observability: pluggable logger · M
**Why:** Zero logging surface — no slog, no logger interface (grep non-test → none). No
way to get online-indexer progress, retry, or conflict diagnostics out of the library.
**Do:** add a pluggable `*slog.Logger` (nil = silent) on the DB/store/runner; emit
online-indexer progress + retry/conflict events. (Builds on the existing
`PlanGenerationLogger` hook from RFC-034.)
**Decision needed:** interfaces-only (no new core deps) vs direct OTel/Prometheus.
**Done when:** a consumer can capture structured logs for index builds and retries.

### [ ] P1.3 — Observability: conflict/retry metrics + export hook · M
**Why:** `StoreTimer` is in-memory `atomic.Int64` only (`store_timer.go`) with **no
export** and **no commit-conflict/retry counters**. Can't answer "how many 1020s are we
taking" or "why is this slow."
**Do:**
- Add `commit`, `commit_conflict (1020)`, `retry`, `commit_unknown_result` events to
  `StoreTimer` (`store_timer.go:17-52`).
- Add a `Snapshot()`→sink export hook; ship a Prometheus/OTel adapter as a separate
  optional package + example.
**Done when:** conflicts/retries are countable and exportable to a metrics backend.

### [ ] P1.4 — Bound default retries + propagate `ctx` (hang/forever-retry) · M
**Why:** `FDBDatabase.Run(ctx, …)`'s `ctx` never reaches the retry loop/backoff (runs on
`context.Background()`, `fdb/database.go:108,197`); default retry limit is **unlimited**
→ a hot conflict (1020) retries forever. `OpenWithConnectionString` /
`OpenDatabaseFromConfig` block forever on an unavailable cluster (bootstrap uses
`context.Background()`, `fdb/database.go:119,133`) while `OpenDatabase` caps at 60s.
**Do:**
- Thread the caller `ctx` into `Transact`'s retry loop + `backoffSleep`.
- Set sane default retry limit + transaction timeout (or unify `Run` onto the bounded,
  ctx-aware `FDBDatabaseRunner` in `runner.go` — there are currently two divergent retry
  paths).
- Make bootstrap timeout consistent across all `Open*` entry points.
**Done when:** a cancelled ctx aborts retries promptly; unavailable cluster fails within a
bounded time on every Open path; tests for both.

### [ ] P1.5 — `govulncheck` + supply-chain hygiene in CI · S
**Why:** No vulnerability scanning anywhere (`grep govulncheck` → none). No SECURITY.md.
**Do:** add `govulncheck ./...` as a CI job; add `SECURITY.md` (disclosure contact);
document dep-update policy.
**Done when:** CI fails on known vulns; SECURITY.md present.

### [ ] P1.6 — Own-your-fork CI gates (bus-factor mitigation) · M
**Why:** Can't remove bus factor, but can de-risk depending on it. The conformance +
differential + stress suites are the real safety net; make them *your* merge gates.
**Do:**
- Pin/vendor a specific commit; document the pin.
- Run conformance (`//conformance`), the libfdb_c differential suite (`pkg/fdbgo/bench`),
  and the 1M SQL stress test (`//pkg/relational/sqldriver/stress`) in your own CI as
  required gates. (Today: stress is in **no** workflow; differential is nightly-only on 8
  of 109 fuzz targets.)
**Done when:** your CI re-runs conformance + differential + stress on every upgrade of
the pin.

### [ ] P1.7 — Reconcile contradictory docs · S
**Why:** README "Not yet supported: LEFT/RIGHT OUTER JOIN, subqueries in WHERE,
LIMIT/OFFSET" (README:116-123) contradicts TODO.md/DIVERGENCES.md where these are
implemented. Can't assess stability when docs disagree.
**Do:** fix the README sections; create one `FEATURE_MATRIX.md` as the single source of
truth; date it.
**Done when:** README, TODO, DIVERGENCES, and the feature matrix agree.

---

## P2 — Medium (before stable v1 / external adopters)

### [ ] P2.1 — Releases, semver, CHANGELOG · S
**Why:** `git tag` empty; no releases, no CHANGELOG, no `/vN` — consumers can only pin a
`v0.0.0-pseudo` version. No stability contract.
**Do:** add `CHANGELOG.md`; cut `v0.1.0`; publish a stability statement (which surfaces
are stable vs experimental: record layer vs SQL engine vs pure-Go client vs vector).
**Done when:** a consumer can pin a tagged version and read what's stable.

### [ ] P2.2 — libfdb_c escape hatch · L
**Why:** Record Layer hard-imports `pkg/fdbgo/fdb` in 53 files; the Apple CGo binding is
test-only. If you hit a pure-Go-client divergence in prod, there's no fallback. The
pure-Go client is young and churned 40+ fixes in the week before review (and once crashed
the FDB server — fixed, `CRASH_BUG.md`).
**Do:** define a `Database`/`Transaction` interface; provide a libfdb_c-backed
implementation; let the Record Layer run on either via build tag/config.
**Done when:** the Record Layer test suite passes on both backends; switching is a config
flag.
**Note:** large; defer unless bet-the-company writes are needed before the pure-Go client
settles.

### [ ] P2.3 — Close known SQL-engine correctness gaps · L
**Why:** Open items in TODO.md. Edge cases, but "wrong/NULL/nondeterministic" in a
datastore is the scariest class. Each routes through the query-engine skill + Graefe.
**Items:**
- Join-enumeration **row-count nondeterminism** on 3-way/arithmetic joins (TODO.md:54) —
  *correctness flake, highest of these.*
- INSERT…SELECT qualified-aggregate-operand NULL (TODO.md:70).
- 0-row type-coercion misses: `SELECT double_col→BIGINT` empty source; `UPDATE int_col =
  <double-expr>` (TODO.md:73).
- Bare GROUP BY-aggregate INSERT…SELECT + possible PK-mapping anomaly (TODO.md:74).
- Divergent-named aggregate union branches NULL; `SELECT u.*` over aggregate union leg
  (TODO.md:79).
- `GetIndexTypeName` hardcodes MIN_EVER_LONG/MAX_EVER_LONG; needs MIN_EVER_TUPLE
  (TODO.md:75).
**Done when:** each pinned with a passing regression; nondeterminism item proven stable
over N runs.

### [ ] P2.4 — Broaden fuzz coverage in CI · S
**Why:** ~101 of 109 fuzz targets run in no workflow; nightly fuzzes only 8.
**Do:** rotate the full fuzz corpus through nightly with a time budget; publish crash
corpus.
**Done when:** all fuzz targets are exercised on a schedule.

### [ ] P2.5 — Pin FDB image version in tests · S
**Why:** README targets 7.3.75 but the getting-started snippet uses 7.3.63; no single
pinned FDB version in CI.
**Do:** pin one FDB testcontainer version; reconcile README.
**Done when:** one documented FDB version across CI + docs.

---

## P3 — Low (polish before v1 promise)

### [ ] P3.1 — Idiomatic Go API pass · M
Java-style accessors, mutable internal maps returned from getters, builder chains. Make
`go doc` read like a Go library. (Per `PRODUCTION_READINESS.md` P3.)

### [ ] P3.2 — Quickstart + realistic examples that compile in CI · S
Minimal Linux/macOS quickstart; examples for save/load, indexes, online build, retry,
pagination, Java/Go interop; ensure they compile in CI.

### [ ] P3.3 — De-duplicate the two retry predicates · S
`fdb/error.go:IsRetryable` vs `client/commitpath.go:200 isRetryable` — not a bug today
(latter is dummy-tx-only) but a drift risk. Single source of truth.

### [ ] P3.4 — Operator guide · M
FDB cluster file handling, retry behavior, tx limits, online index lifecycle, index-state
transitions, schema-evolution safety, backup/restore expectations, observability hooks.

---

## Suggested execution order

1. **P0.1** (license — unblocks everything, minutes).
2. ~~**P0.2-CLASSIFY**~~ ✓ done (`docs/panic-audit.md`).
3. **P0.2-A1** (eval `(any,error)` sweep + plumbing + the values.go race fix — query-engine, Graefe ACK).
4. **GATE** (conformance + 1M stress, diff row counts vs baseline).
5. **P0.2-A2** (pin the keep=false bug, then delete the 6 control-flow recovers).
6. **P0.2-B/C/D/E/G** (record/metadata → errors; wire comment; parser FFI keep+fuzz; MustGet→.Get(); tuple.Pack resolution).
7. **P0.2-F** (executor + e2e fuzz net).
8. **P1.4** (retry/ctx bounds).
9. **P1.1** (race in CI — may surface real bugs; fix them, no skips).
10. **P1.5 + P1.7 + P2.1** (govulncheck, doc reconciliation, release — cheap, parallel).
11. **P1.2 + P1.3** (observability).
12. **P1.6** (own-your-fork gates); **P2.3** (SQL correctness, Graefe-gated); **P2.2** (libfdb_c, if needed).
