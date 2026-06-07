# TODO — Production Readiness

Derived from `review_2026-06-07.md`. Ordered by criticality so the most important
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

## P0 — Blockers (before any production use)

### [ ] P0.1 — Add a LICENSE (legal blocker) · S
**Why:** No LICENSE file exists (`git ls-files | grep -i licen` → empty), yet README:281
links `[LICENSE]`. Unlicensed = "all rights reserved" — cannot legally deploy. Derivative
of Apache-2.0 `fdb-record-layer`, so it must be Apache-2.0-compatible.
**Do:** add `LICENSE` (Apache-2.0) + `NOTICE` (Apple/FoundationDB + Apache protos).
**Done when:** both tracked; README link resolves; legal sign-off.

### [ ] P0.2 — Boundary recover + network-goroutine teardown (the hours-not-weeks crash fix) · S
**Do FIRST, before the P0.3 sweep** (Torvalds): don't run a multi-tenant process that
crashes on `SELECT 1/0` for the weeks the sweep takes. Build the net, then refactor behind
it. This is the minimal realization of the P0.3 policy:
- db/sql boundary recover at `connection.go:305,336` (catch-all → `debug.Stack()` → log
  SERIOUS w/ tenant context → generic internal error → keep serving).
- recover→`failConnection` in `readLoop`/`writeLoop`/`connectionMonitor` (`conn.go:265-267`)
  — a panic there is otherwise an uncatchable whole-host crash. Fix the false `conn.go:586`
  comment + the `exitErr` first-error-wins ordering.
**Done when:** a panicking query returns an error (process survives); a panic in a network
goroutine fails only that connection; tests for both.

### [~] P0.3 — Panic/error discipline: errors on the data path, assert internally, recover at every goroutine boundary
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
  Apple parity]; (3) the 3 network goroutines [new, P0.2].
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
- [ ] **P0.3-A1 — eval signature + plumbing, mechanical** (query-engine, **Graefe ACK**, `M`).
  `Value.Evaluate → (any, error)` + `QueryPredicate.Eval(ctx) TriBool → (TriBool, error)`.
  **Split per-package** (`values/` → `predicates/` → `executor/`), not one 500-site commit
  (Torvalds). Typed errors + SQLSTATE map already exist (`translateExecError`). Do the
  `ReportUnresolvedReference` global fix (`values.go:777`, P1.1) in this same sweep — don't
  touch values.go twice. **Verify Kleene short-circuit semantics** (`FALSE AND 1/0`→FALSE;
  `1/0 AND FALSE`→error): check `err` per-child before the TriBool switch; pin both orderings
  (Graefe).
- [ ] **GATE** — conformance + 1M stress + **`-race`**, **per-query/seeded** diff (an
  aggregate row-count diff is poisoned by the known nondeterminism, P1.x/TODO.md:54 —
  Graefe+Torvalds). **Pin the `executor.go:739`/`executor_new_plans.go:337` `keep=false`
  silent-row-drop bug BEFORE the gate** (it corrupts the baseline; real correctness bug).
- [ ] **P0.3-A2 — delete the 6 eval/cursor control-flow recovers** (separate commit).
- [ ] **P0.3-B — record/metadata/key-expr panics → errors** (`M`). `metadata.go:476`
  (unknown record type — real bug: DDL builder caller already expects nil), `key_expression.go:
  1133`. Audit `catalog/metadata.go:48/56/63` non-nil assumptions.
- [ ] **P0.3-C — network goroutines: ADD recover→failConnection** (`S`). *(Reversed per FDB
  C++ + Torvalds — was "no goroutine recover needed".)* Keep decode error-clean too (verified
  panic-free on bad bytes — belt + suspenders); fix the `conn.go:586` comment + `exitErr`
  ordering. (Lands with P0.2.)
- [ ] **P0.3-D — parser: KEEP the FFI recovers, expand fuzz** (`S`). *(Corrected: collecting
  listener already exists; recovers guard the ANTLR runtime — do NOT delete.)*
- [ ] **P0.3-E — `Must*`: keep `panicToError`, switch internal callers to `.Get()`** (`S`).
  *(Corrected per FDB C++ — do NOT delete `panicToError`; it's the `Must*` boundary.)* Switch
  the 8 callers (`directory/directoryLayer.go:449/594/595/610`, `directory/node.go:63`,
  `keyspace/fdb_resolver.go:65/72/148`) to `.Get()` for hygiene; the recover stays.
- [ ] **P0.3-F — fuzz net** (`M`). Build executor-package fuzz + an e2e **SQL-string +
  seed-rows → `QueryContext` → no-panic** target. Honest caveat: e2e fuzz needs a container →
  shallow — which is exactly why the boundary recover stays. Drop any "proven by fuzz"
  done-criteria overclaim (Torvalds): fuzz reduces, the recover backstops.
- [ ] **P0.3-G — resolve `tuple.Pack()` reachability** (`M`). `multiIntersectionCompKeyFunc`
  (`executor_new_plans.go:144-156`) packs the raw `Evaluate` result with NO coercion (panics →
  caught only by `merge_cursor.go:24`); `mergeSortCursor.extractKey` coerces. Prove the
  comparison-key path is scalar-only (then the recover is dead → delete) or make it
  error-returning; fix the inconsistency. `extractKey`'s `%T:%v` fallback is itself a
  correctness bug (Go-format ordering ≠ tuple order). Route via Graefe. Resolve before
  deleting `merge_cursor.go:24`.

**Done when:** the library never panics to a caller (boundary recovers in place); data-path
panics are errors; remaining panics are documented internal asserts; conformance + stress +
`-race` green across the staged commits.

**RFC-091 status (2026-06-07):** Step 1 (twins) + fold-fix + 3 uncovered eval sites + A1
(executor migration) + A2 (delete all 6 control-flow recovers) **landed, both gates ACK'd**
(Graefe: "Cascades-correct, safe to merge"; Torvalds: "no NAK, the fix is real"). Eval errors
now return everywhere (div0→22012 etc.); the keep=false silent-row-drop is fixed + pinned.
**Follow-ups (tracked):**
- [ ] **Collapse** (Torvalds a/b): delete the dead old `Evaluate`/`Eval` panic-wrappers, rename
  `EvaluateErr`→`Evaluate` (`EvalErr`→`Eval`), and migrate the remaining old-method callers —
  5 plan-time rule sites (`rule_implement_in_join.go:469`, `rule_implement_in_union.go:95`,
  `rule_in_to_explode.go:96`, `rule_simplify.go:381`, `physical_vector_index_scan_wrapper.go:96`)
  **plus 6 `value_range.go` helpers** (`EvaluateAsStream`/`Cardinality`, :90-133). Dual eval
  methods must not ossify.
- [x] **GATE completed**: conformance + FDB green at every commit; `-race` on
  `//pkg/relational/...` run (found + fixed the `hadRead` client race — see P1.1); 1M stress
  green (all subtests pass, durations consistent with the baseline — no bulk-path regression).
- [x] **P0.2 gap CLOSED:** `paginatingRows.Next` + `cascadesRows.Next` (cascades_generator.go)
  now `recover()` → `recoveredPanicError` — the db/sql boundary recover spans the FULL query
  path (planning + first page via QueryContext/ExecContext, later pages via Rows.Next). A panic
  during later-page iteration becomes a generic internal error, not a host crash; user eval
  errors already return cleanly via the sweep. (`-race` job into CI still tracked under P1.1.)

### [ ] P0.4 — Bound retries + propagate `ctx` (promoted from P1 — availability blocker) · M
**Why (Torvalds: this is P0 for a control plane, not P1):** `FDBDatabase.Run(ctx,…)`'s ctx
never reaches the retry loop (`Database.Transact` takes no ctx, runs on `context.Background()`
— `database.go:108,197`); default retry limit unlimited → a hot 1020 retries forever and the
caller **cannot cancel it**. `OpenWithConnectionString`/`OpenDatabaseFromConfig` block forever
on an unavailable cluster (`database.go:119,133`) vs `OpenDatabase`'s 60s cap.
**Do (FDB C++):** route `Run`/friends through the already-correct `client.Database.Transact(ctx,
…)`, collapsing the 3 retry paths to one; keep unlimited-retry default (libfdb_c parity:
`RETRY_LIMIT=-1`) **but ship a sane default transaction timeout** for the control-plane target
— don't ship ctx-severed *and* timeout-off *and* retry-unlimited together; make all `Open*`
bootstrap bounded/consistent.
**Done when:** a cancelled ctx aborts retries promptly; unavailable cluster fails bounded on
every Open path; tests for both.

---

## P1 — High (before relying on it at scale)

### [ ] P1.1 — `-race` on the SQL layer in CI (+ fix what it finds) · M
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
**Still to do for P1.1:** wire the `-race` job into CI (not just this one-off run); the
`ReportUnresolvedReference` global (`values.go:777`) did NOT surface in this run but remains a
latent landmine to convert to `atomic.Pointer`/set-once.

### [ ] P1.2 — Observability: pluggable logger · M
Zero logging surface (no slog, no logger interface). Add a pluggable `*slog.Logger` (nil =
silent) on DB/store/runner; emit online-indexer progress + retry/conflict events. Builds on the
`PlanGenerationLogger` hook (RFC-034). **Decision:** interfaces-only (no new core deps) vs direct
OTel/Prometheus.

### [ ] P1.3 — Observability: conflict/retry metrics + export hook · M
`StoreTimer` is in-memory `atomic.Int64` only, no export, no commit-conflict/retry counters.
Add `commit`/`commit_conflict(1020)`/`retry`/`commit_unknown_result` events; a `Snapshot()`→sink
export hook; ship a Prometheus/OTel adapter as a separate optional package + example.

### [ ] P1.4 — ~~retry/ctx bounds~~ → **PROMOTED to P0.4.**

### [ ] P1.5 — `govulncheck` + supply-chain hygiene in CI · S
No vuln scanning anywhere; no SECURITY.md. Add `govulncheck ./...` (parallel with P0.3,
Torvalds); add `SECURITY.md` + dep-update policy.

### [ ] P1.6 — Own-your-fork CI gates (bus-factor mitigation) · M
Pin/vendor a commit; run conformance (`//conformance`), the libfdb_c differential suite
(`pkg/fdbgo/bench`), and the 1M stress test (`//pkg/relational/sqldriver/stress`) as *your*
required gates (today: stress is in no workflow; differential is nightly-only on 8 of 109 fuzz
targets). Caveat (Torvalds): this gives *detection*, not *repair* — see the won't-fix note.

### [ ] P1.7 — Reconcile contradictory docs · S
README "Not yet supported: LEFT/RIGHT OUTER JOIN, subqueries, LIMIT" (README:116-123)
contradicts TODO.md/DIVERGENCES.md. Fix README; create one dated `FEATURE_MATRIX.md`.

### [ ] P1.8 — CI reproducibility / supply chain · M
CI runs on a **self-hosted personal Hetzner box** (`runs-on: [self-hosted, linux, x64,
hetzner]`) — for any external adopter the green signal is unreproducible and depends on one
person's hardware (bus-factor + supply-chain). Provide a reproducible runner (ephemeral/cloud)
or document the requirement; pin tool/image versions with checksums.

### [ ] P1.9 — Resource limits / backpressure (multi-tenant noisy-neighbor) · M
**Why (Torvalds — missed by the review):** nothing caps a single query's intermediate
materialization, row count, or wall-clock. One tenant can OOM or wedge a shared host. P0.4
covers retry/ctx but not query resource bounds.
**Do:** statement timeout, max-rows / result-size cap, query memory budget (the
`MaterializationLimitExceededError` from RFC-028 is a start — extend to streaming intermediates
+ a per-query budget). Surface as errors, not crashes.

---

## P2 — Medium (before stable v1 / external adopters)

### [ ] P2.1 — Releases, semver, CHANGELOG · S
`git tag` empty; no releases/CHANGELOG/`/vN`. Add `CHANGELOG.md`; cut `v0.1.0`; publish a
stability statement (record layer vs SQL engine vs pure-Go client vs vector).

### [ ] P2.2 — libfdb_c escape hatch · L
**86 non-test files** (corrected from 53) import `pkg/fdbgo/fdb`; the Apple CGo binding is
test-only. No fallback if the young, recently-churning pure-Go client diverges in prod (it once
crashed the FDB server — fixed, `CRASH_BUG.md`). Define a `Database`/`Transaction` interface; a
libfdb_c-backed impl; switch via config. **Torvalds: mandatory for any bet-the-company *write*
path**, not "defer unless needed."

### [ ] P2.3 — Close known SQL-engine correctness gaps · L (query-engine, Graefe-gated)
Open items in TODO.md. **The row-count nondeterminism (TODO.md:54) is PROMOTED — see P1.x note;
treat as P0/P1 correctness** (a datastore returning different row counts across runs, shelved by
excluding the scenario instead of root-causing it, against the project's own no-skips rule —
Torvalds). Others: INSERT…SELECT qualified-agg NULL (70), 0-row coercion (73), bare GROUP BY
INSERT…SELECT (74), divergent-named aggregate union NULL (79), `GetIndexTypeName` MIN/MAX_EVER
(75).

### [ ] P2.4 — Broaden fuzz coverage in CI · S
~101 of 109 fuzz targets run in no workflow; nightly fuzzes 8. Rotate the full corpus nightly;
publish crash corpus.

### [ ] P2.5 — Pin FDB image version in tests · S
README targets 7.3.75 but the snippet uses 7.3.63; no single pinned version in CI. Pin one;
reconcile.

---

## P3 — Low (polish before v1 promise)

### [ ] P3.1 — Idiomatic Go API pass · M
Java-style accessors, mutable internal maps from getters, builder chains. Make `go doc` read
like a Go library.

### [ ] P3.2 — Quickstart + realistic examples that compile in CI · S

### [ ] P3.3 — De-duplicate the two retry predicates · S
`fdb/error.go:IsRetryable` vs `client/commitpath.go:200 isRetryable` — drift risk; single source.

### [ ] P3.4 — Operator guide · M
Cluster file, retry, tx limits, online index lifecycle, index-state transitions, schema-evolution
safety, backup/restore, observability hooks.

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
