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
- [~] **P0.3-F — fuzz net** (`M`) — substantially DONE; one acknowledged-shallow gap. The
  eval/value layer (where panics actually hide) is well-fuzzed: `embedded_test.go` has
  `FuzzApplyMathOp`, `FuzzApplyBitOp`, `FuzzCompareValues`, `FuzzCastValue`, `FuzzLikePrefixStrinc`,
  `FuzzLikePatternToPrefix`; `values/` has `FuzzSimplifyValue_ArithmeticTree`/`_CastChain`,
  `FuzzAndOrValue_Kleene3VL`, `FuzzCaseExpression_FirstMatchWins`, etc. **Gap:** no e2e
  *SQL-string + seed-rows → `QueryContext` → no-panic* target. Honest caveat (kept): e2e fuzz
  needs a container → shallow — which is exactly why the boundary recover stays the real backstop.
  Not "proven by fuzz"; fuzz reduces, the recover backstops.
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

### [x] P1.5 — `govulncheck` + supply-chain hygiene in CI · S — DONE
Added a **Vulnerability scan** job to `ci.yml` (`govulncheck` over the shipped `pkg/...`,
excluding `pkg/testcontainers`), a `SECURITY.md` (private reporting + scope + dep policy).
The first scan found **7 called vulns**: 5 in `golang.org/x/crypto@v0.48.0` → **bumped to
v0.52.0** (fixed; transitively pulled x/text v0.37, x/tools v0.44; bazel build green, no
MODULE.bazel.lock drift), and 2 in `github.com/docker/docker` (test-infra only, **Fixed in:
N/A** upstream) — excluded from the production scan + documented in SECURITY.md. Post-bump
scan: production-clean (only the 2 docker N/A remain, test-only).

### [ ] P1.6 — Own-your-fork CI gates (bus-factor mitigation) · M
Pin/vendor a commit; run conformance (`//conformance`), the libfdb_c differential suite
(`pkg/fdbgo/bench`), and the 1M stress test (`//pkg/relational/sqldriver/stress`) as *your*
required gates (today: stress is in no workflow; differential is nightly-only on 8 of 109 fuzz
targets). Caveat (Torvalds): this gives *detection*, not *repair* — see the won't-fix note.

### [~] P1.7 — Reconcile contradictory docs · S — README done
README's "Not yet supported" listed **6 features; 5 were already implemented** (verified
against the yamsql corpus + DIVERGENCES.md): LEFT/RIGHT/FULL OUTER JOIN, LIMIT/OFFSET,
subqueries-in-WHERE (EXISTS / IN (SELECT) / correlated scalar), mixed ASC/DESC, scalar
functions (UPPER/LOWER). The "no physical sort operator / ORDER BY needs an index" claim
was also stale (Go has a Go-only `RecordQueryInMemorySortPlan`). Rewrote the README SQL
section to the accurate surface + a dated pointer to the authoritative source (yamsql corpus
+ DIVERGENCES.md), keeping only genuine gaps (CTE-in-UNION-branch, DML `IN (SELECT)`, general
window functions, synthetic record types). **Remaining:** a generated/maintained
`FEATURE_MATRIX.md` (deferred — the dated README pointer is the interim source of truth).

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
