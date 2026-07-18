# RFC-182 — Generative differential row testing: the plan-soundness oracle

**Status:** Graefe ACK + Torvalds ACK (2026-07-18), both conditional on their findings being folded into this text — folded below; see §10. Ready for implementation (P1).
**Tracks:** the 2026-07-18 Cascades quality audit's #1 systemic recommendation.
**Relates to:** RFC-179/180/181 (correctness audit waves — hand-driven instances of what this automates), RFC-167 §2a (`WithPrimaryKeyIntersector` ordering gate), chaos `StoreModel` (`pkg/recordlayer/chaos/` — the storage-layer precedent for model-based oracles), `cmd/fdb-binding-stress` (seeded-stress conventions), `plandiff` (`pkg/relational/conformance/plandiff/runsql.go` — the Java `runSql` runner this reuses).

## 1. Problem

The 2026-07-18 audit confirmed a live wrong-rows bug on master: the pk-intersection
path drops residual predicates (`intersector_primary_key.go:143-147` hardcodes
`NoCompensation`; Java's `WithPrimaryKeyDataAccessRule.java:135-193` intersects leg
compensations and reapplies residuals). Repro:

```sql
-- items(id PK, category STRING, name STRING, price BIGINT), idx_category, idx_price
SELECT * FROM items WHERE category='electronics' AND price=120 AND name='Keyboard'
-- plan: Fetch(Intersection(IndexScan(IDX_CATEGORY,[=]), IndexScan(IDX_PRICE,[=])))
-- rows: [3, 5] — WRONG (id 5 fails name='Keyboard'). Correct: [3].
```

The suite that missed it is otherwise strong (~90k test LOC in the cascades tree,
1159 `TestFDB_*` functions, a 1642-query cross-engine corpus, ~56 fuzz targets).
Every layer shared one blind spot:

1. **Unit tests test the mechanism, not the contract.** `intersector_primary_key_test.go`
   verifies the intersector builds what it intends — never that the built alternative
   is row-equivalent to the group it joins.
2. **Corpora test factors, never the cross product.** `and_index_intersection.yaml`
   covers {2 indexed preds} and {1 indexed + 1 residual}; the killer combination
   {2 indexed + 1 residual} is absent. The plandiff corpus's `three_way_and` entry
   has the right predicate count on a table with **zero indexes**.
3. **Projection is an invisible axis.** Corpus house style is `SELECT id … ORDER BY id`;
   the bug only wins under `SELECT *` (covering-vs-fetch cost flips the winner).
4. **Every existing fuzz target asserts planner *stability*** (determinism,
   idempotence, no-panic, cost monotonicity) — none has a row oracle.
5. **The same defect class was found before** (bug-hunt AGG-RESIDUAL: aggregate
   data-access path dropping residuals) and fixed point-wise, not class-wise.

The class to kill: **plan unsoundness** — any winning plan whose row semantics
differ from the query's. Hand-written tests pin behaviors someone thought about;
this RFC adds the layer that checks behaviors nobody thought about.

## 2. Design overview

A seeded generator produces `(schema, data, query-family)` triples. Each query
executes through the FULL production path (sqldriver → Cascades → executor →
real FDB via testcontainers) and its rows are compared against one or both oracles:

- **Oracle M (model, always on):** brute-force in-memory evaluation over the
  generator's own authoritative row set — the planner analog of chaos `StoreModel`.
- **Oracle J (Java, nightly):** the same schema/data/query run on live Java
  4.12.11.0 via the conformance server's `runSql` step (`plandiff.NewJavaRunnerHTTP`).

Any mismatch fails loudly with the seed, the minimized repro, and both row sets.
Every confirmed finding gets pinned as a permanent regression (corpus entry or
dedicated test) — the harness is the discovery net; pins are the memory.

```
seed ──► generator ──► schema+data (authoritative Go values)
                   │            │
                   │            ├─► CREATE/INSERT via sqldriver ──► Cascades+FDB rows ─┐
                   ├─► query ───┤                                                      ├─► diff
                   │            ├─► Oracle M: full-scan eval over Go values ───────────┘
                   │            └─► Oracle J (nightly): conformance-server runSql ──► 3-way triage
                   └─► EXPLAIN ──► plan-family histogram (coverage gate)
```

## 3. Oracle M — the in-memory model

**What it is.** The generator keeps every table's content as typed Go values
(`[]row`); rows are rendered to `INSERT` statements *from* that model, never
parsed back. Oracle M evaluates the query naively over the model: full scan →
predicate evaluation per row → projection → ORDER BY (stable sort) → LIMIT/
DISTINCT (when generated). No planner, no indexes, no FDB.

**Deliberate design point — shared scalar semantics.** Oracle M reuses the
engine's own `values`/`predicates` evaluation (`Value.Eval` over a synthesized
row/binding) rather than reimplementing SQL scalar semantics. Rationale:

- The class under test is *planner* soundness. Removing the planner while keeping
  the scalar interpreter isolates exactly that class: dropped/duplicated
  predicates, wrong index bounds, unsound intersections/unions, merge-cursor row
  drops, projection/covering mistakes.
- An independent reimplementation would drift from the engine on NULL semantics,
  type coercion, and collation, producing false positives that rot trust — and a
  *correct-looking* reimplementation that shares the author's misunderstanding
  catches nothing.
- The blind spot this creates (engine and oracle share a scalar-eval bug) is
  exactly what Oracle J covers: Java evaluates with its own interpreter.

**Comparison rules.**
- Row multiset equality on driver-visible values (same typed decode path the
  sqldriver returns). No epsilon: DOUBLE columns compare **bit-exact on the
  IEEE-754 pattern** — NaN payloads equal iff bit-identical, `-0.0 ≠ +0.0` —
  divergence is a finding, not noise (see OQ-3 for the Java-oracle phase gate).
- With ORDER BY: assert (a) multiset equality and (b) the engine's output is
  sorted by the ORDER BY key per the engine's documented NULLS default —
  tie groups stay unordered. Never assert full sequence equality on ties.
- **LIMIT k (Phase 2) makes the correct answer a *set* of valid row sets;
  the comparator must accept membership, not equality** (Graefe finding 3):
  - LIMIT without ORDER BY: engine output must be a `min(k, |M|)`-element
    sub-multiset of Oracle M's full result — when the full result has fewer
    than k rows the engine must return ALL of them, not "k elements".
  - LIMIT with ORDER BY: engine output must be a valid top-k — the first
    `k - |last tie group prefix|` rows are forced; rows drawn from a tie group
    straddling the cut may be any members of that group.
- **Projection cross-check:** every generated query body runs in ALL projection
  variants (`SELECT *`, narrow, reordered, pk-only). Each variant diffs against
  Oracle M independently, and variants cross-check row identity via pk — the
  axis that hid the intersection bug is tested by construction.
- Engine errors are never silently tolerated. A decline is acceptable only if it
  matches the documented unsupported-shape list (the conformance principle);
  any other error is a finding. This also catches over-rejection regressions.

## 4. Oracle J — Java differential

Reuses the wired plumbing: `plandiff.NewJavaRunnerHTTP(baseURL, clusterFile)`
POSTs to the Java conformance server's `runSql` step; the cross-engine A3 test
(`conformance/yamsql_cross_engine_conformance_test.go`) already established the
schema-adaptation gotchas (e.g. NOT NULL on PK columns).

- **Subset mode:** the generator has a Java-compatible profile that avoids
  documented Java 4.12.11.0 limitations (the A3 skip list: GROUP BY, DISTINCT,
  LIMIT, multi-col ORDER BY, IS TRUE/FALSE, …). Shapes Java can't run are
  Oracle-M-only — Go-side reach beyond Java is allowed (wire compat is the hard
  line; query reach is not) and still needs soundness coverage.
- **Three-way triage on disagreement:** compare Go, Java, and Oracle M.
  - Go ≠ M, Java = M → Go bug (file, fix, pin).
  - Go = M, Java ≠ M → Java upstream bug or a documented divergence → DIVERGENCES.md
    entry with a live probe, per existing practice.
  - all three differ → shared-semantics divergence (the Oracle-M blind spot
    working as designed) → highest-priority investigation.
- Runs nightly (needs the Java server + FDB), not in `just test`. A red nightly
  is always in scope — no freeze exempts it (CLAUDE.md).
- **Outcome classification is typed, not textual** (Torvalds finding 4). Every
  seed resolves to exactly one of:
  - `INFRA` — container timeout, dead Java server, transport error. Never
    counted as a row mismatch; the run reports it separately and fails only on
    an INFRA *rate* threshold, so a dead server can't masquerade as green or as
    a soundness finding.
  - `DECLINE` — engine rejected the query; acceptable only if it matches the
    shared decline authority (§9 OQ-4, resolved: extracted in P3), else a finding.
  - `MISMATCH` — genuine row divergence. Mismatches carry a **signature**
    (normalized plan family + divergence shape + triage arm). The nightly fails
    only on *new* signatures; known documented semantic divergences live in the
    same shared authority as declines, each entry required to reference a live
    probe (DIVERGENCES.md practice) so the list can't become a suppression
    dumping ground. **The ledger may only ever contain Go=M / Java≠M entries
    (documented Java-side divergences). A Go≠M mismatch has NO ledger path —
    it always fails, regardless of signature reuse.** (Torvalds delta note;
    P3 must enforce this structurally, not by convention.)
  The stress binary exits with distinct codes per class.

## 5. The generator

Seeded `*rand.Rand` only (no global rand, no time); the seed is the repro key.
Dimensions, phase-gated (§7):

- **Schema:** 1 table (Phase 1) → 2-3 (Phase 4). Column types BIGINT, STRING,
  BOOLEAN, DOUBLE (OQ-3), nullable mix; single and composite PKs; 0-4 indexes:
  single-column, composite, with deliberate overlap (two indexes on one column,
  index prefixes of the PK) — the shapes that feed intersections and winner ties.
- **Data:** 10-200 rows; adversarial values by construction: NULLs in indexed
  columns, heavy duplicates (so intersections have real work), boundary values
  (0, -1, MaxInt64, empty string), and a selectivity spread — some predicates
  match many rows, some few, some zero.
- **Query:** AND/OR trees (depth-bounded), operators `= < > <= >= <> BETWEEN
  IN IS NULL` (`LIKE` Phase 2), over a *biased* mix of indexed and unindexed
  columns — the generator explicitly over-weights "≥2 indexed columns bound AND
  ≥1 unindexed residual", the empty matrix cell that hid the bug. ORDER BY
  on/off, over indexed and unindexed keys. Projection variants per §3.
  Phase 2 adds LIMIT + continuation-resume; Phase 4 adds DISTINCT, GROUP BY /
  aggregates, joins.
- **Continuation dimension (Phase 2):** every query re-executes through a
  small-row-limit continuation loop; the reassembled rows must equal the
  one-shot rows. This is the net for the merge-cursor/continuation class
  (VECTOR-PARTITION-INTERSECTION-K>1 was exactly this).

**Plan-shape steering and the coverage gate.** Random SQL without steering
produces full scans. Coverage is enforced in two layers (Graefe finding 2 +
Torvalds finding 2 — they compose):

- **Hard gate — directed template seeds.** The smoke run reserves a small set
  of deterministic template seeds, each *constructed* to produce one required
  plan family (Intersection, IndexScan+residual, Covering, Union/InUnion, …)
  and *asserted per-seed* against the plan. A cost-model change that flips a
  template's family fails that seed deterministically with a clear message —
  a real signal (the family became unreachable or the template rotted), never
  a statistical flake. Random seeds carry no per-family gate **at smoke scale**;
  at **nightly** scale (≥10k seeds) an empty required-family bucket IS a hard
  failure — there it means generator drift, not sampling noise (templates prove
  a family is *reachable*; only the random stream proves it is *exercised*).
- **Telemetry — histograms.** Every query's plan is bucketed into families and
  the per-run histogram is printed so coverage drift is visible and generator
  weights are tuned against it; additionally an *advisory* rule-firing
  histogram from the planner's event hooks reports rules that never fired —
  a rule invisible in plan shapes is invisible to EXPLAIN buckets.

Buckets are derived from the **typed physical plan tree** (plan-type switch),
never by string-matching EXPLAIN text — the repo's no-text-matching rule
applies to the harness itself.

## 6. Delivery form

Execution needs real FDB (no mocks), so the harness is a seeded stress family,
not a `go fuzz` target:

- **`cmd/sql-diff-stress`** — the binary, mirroring `cmd/fdb-binding-stress`
  conventions: `-seeds N -seed-start M -oracle model|java|both -profile
  default|java-compat`, one FDB container shared across seeds (one database/
  schema path per seed, `t.Parallel()`-safe), container setup under
  `context.WithTimeout(…, 2*time.Minute)`. Repro: `bazelisk run
  //cmd/sql-diff-stress -- -seeds 1 -seed-start NNN`.
- **`TestFDB_SQLDiffStress_Smoke`** — bounded run wired into `just test`,
  Oracle M only, directed template seeds + telemetry histograms. **Per-seed
  workload is specified, and the budget is measured, not asserted** (Torvalds
  finding 3): one schema, ≤100 rows, ~8 query bodies × up to 4 projection
  variants ≈ 32 executions per seed. Seed count is set from a measured P1
  timing run so the smoke fits ≤~2 min *excluding* container startup; the smoke
  shares the sqldriver suite's container (one database path per seed), so it
  adds no startup cost of its own. The measured seeds/sec goes in the P1
  landing note.
- **Nightly** — ≥10k seeds Oracle M + ≥1k seeds Oracle J (java-compat profile),
  sharded by seed range.
- **Minimizer:** on mismatch, greedy shrink (drop predicates → drop projection
  variants → drop rows → shrink values), re-checking the diff persists at each
  step; emit the minimal schema+data+query as a ready-to-pin test snippet.
  **Bounded and flake-safe** (Torvalds finding 5): hard cap on shrink
  iterations and wall time; if the mismatch vanishes mid-shrink (flake, or a
  shrink that removed the trigger) the minimizer emits the last-known-failing
  form and stops — it never loops and never discards the original repro.
- **Pin workflow:** every confirmed finding lands a permanent regression before
  or with its fix (corpus entry in the relevant yamsql/plandiff corpus, or a
  dedicated `TestFDB_*`), same as today's culture. The stress harness itself is
  never the pin.
- **yamsql emitter (ships with the P2 minimizer; P1 failure output is already
  copy-pasteable).** The minimized repro serializes directly as a DRAFT yamsql
  scenario: generated DDL → `schema_template`, generated INSERTs → `setup`,
  query text → `query`, **Oracle M's rows** (the correct answer — never the
  engine's) → `rows` with `unordered: true`, so the emitted scenario is born
  red on the buggy engine and goes green with the fix. Rules:
  - No `plan_contains` in emitted pins — the only plan observed at emission is
    the buggy one; the fixer adds the plan assertion after the fix determines
    the correct shape.
  - Values that can't round-trip YAML faithfully (NaN payloads, `-0.0`, exotic
    strings) fall back to an emitted `TestFDB_*` Go snippet.
  - Shapes whose correct answer is a *set* of valid results (LIMIT without
    ORDER BY, tie-straddling top-k) are not exact-rows-pinnable: the shrinker
    prefers dropping the LIMIT; else the Go-snippet fallback asserts the §3
    membership property.
  - Emitted files are drafts a human names and curates into the corpus —
    auto-generated permanence is how junk accumulates.

## 7. Phases

- **P1 — the killer-class net.** Generator (single table, AND/OR, projection
  variants, ORDER BY) + Oracle M + `cmd/sql-diff-stress` + smoke test +
  template-seed gate. **Acceptance (weights frozen first — Torvalds finding 1,
  Graefe finding 5):** freeze the generator weights by commit *before* the
  acceptance run; then run against a tree with the pk-intersection residual
  bug present (`fb2dead8e`, or the fix commit's parent if the fix lands first)
  with a budget of **≤500 random seeds** (nightly-scale; the smoke's random
  slice is not required to hit it). The harness must find the bug unaided —
  no weight tuning after a miss; tuning restarts the acceptance clock. The
  landing note records commit, weights hash, first hitting seed, and the
  **hit rate** across the 500 (the harness's sensitivity baseline).
- **P2 — execution dimensions.** Continuation-resume loop, LIMIT (comparison
  rule in §3), `IN`, `BETWEEN`, `IS NULL`, `LIKE`, the minimizer, and the
  **forced-alternative mode** (Graefe finding 1): re-execute each query under
  planner configurations that disable dominant plan families (no-full-scan,
  no-covering, no-single-index), promoting losing memo alternatives to winners
  so they get row-checked — soundness is a property of every alternative, and
  the cost model *hides* violations until something flips the winner (exactly
  how the intersection bug hid behind `SELECT id`). Non-correlated EXISTS/IN
  enter here if feasible pre-joins (Graefe finding 4).
- **P3 — Java oracle.** `runSql` wiring, java-compat profile, three-way triage,
  outcome classification + signature ledger (§4), nightly target + red-nightly
  runbook entry. **Includes, as a requirement, extracting the shared
  decline/divergence authority** consumed by both this harness and the A3
  cross-engine test (resolves OQ-4; Torvalds finding 6 — the RFC must not
  ship the dual list it warns about).
- **P4 — surface growth.** DISTINCT, GROUP BY/aggregates, multi-table joins
  incl. **correlated EXISTS/IN** (Graefe finding 4 — compensation under
  correlation is where matching is subtlest, and this port already shipped a
  non-correlated-EXISTS bug), then vector/rank families (K>1 partition shapes —
  the other confirmed-bug class). **Honesty note on §3's principle** (Torvalds
  finding 7): aggregate evaluation in Oracle M is a *reimplementation* —
  aggregation is not `Value.Eval` over a row. P4's aggregate coverage therefore
  leans on Oracle J as the semantics authority: aggregate query shapes ship in
  the java-compat profile FIRST (three-way triage active) and Oracle-M-only
  aggregate shapes are marked as carrying the reimplementation-drift risk §3
  otherwise forbids.

Each phase is a milestone per the review cadence: implement with green tests
per commit, one joint Graefe+Torvalds lap at phase completion, codex at the
same granularity.

## 8. Non-goals

- **Not a plan-quality oracle.** It never asserts which plan should win — only
  that the winner is sound. Plan-shape pinning stays in yamsql `plan_contains`
  and EXPLAIN tests (the audit's separate recommendation to grow those stands).
- **Not EXPLAIN parity with Java.** plandiff owns plan-tree comparison.
- **Not a replacement for hand-written corpora.** Pins are the memory; this is
  the net.
- **No mocks, no in-memory execution shortcut for the engine side.** The engine
  path under test is always the full production path.

## 9. Open questions

- **OQ-1 — package home.** `pkg/relational/conformance/rowdiff/` (beside
  plandiff, sharing its Runner plumbing) vs a new `pkg/relational/difftest/`.
  Leaning rowdiff-beside-plandiff: same runners, same corpus-pinning targets.
- **OQ-2 — one container or per-process.** Proposed: one container per stress
  process, one database path per seed (matches sqldriver suite practice and the
  `--local_test_jobs=4` container cap). Nightly shards by seed range.
- **OQ-3 — DOUBLE columns vs Oracle J.** Go↔Java double formatting/rounding
  divergences are known (BIGINT-vs-DOUBLE narrowing entry in DIVERGENCES.md).
  Phase-gate DOUBLE to Oracle M until a documented comparison rule exists for J.
- **OQ-4 — RESOLVED (P3 requirement, see §7).** The "acceptable decline" list
  and the documented-divergence signature ledger are ONE shared authority
  extracted from the A3 skip list, consumed by this harness and the
  cross-engine conformance test alike. Each divergence entry must reference a
  live probe. No second copy.
- **OQ-5 — RESOLVED (see §7 P1).** Permanent sensitivity: the comparator and
  minimizer get unit tests over synthetic mismatches; the P1 acceptance run is
  recorded in the landing note as (commit, weights hash, first hitting seed,
  hit rate over the 500-seed budget); no fault-injection hooks in production
  planner code.

## 10. Review findings ledger (2026-07-18)

Graefe: ACK conditional on findings 1–4 (+5 minor) — folded: §7 P2
forced-alternative mode (1), §5 two-layer coverage + typed-tree buckets +
advisory rule-firing histogram (2), §3 LIMIT/tie + DOUBLE bit-pattern rules
(3), §7 P2/P4 correlated + non-correlated EXISTS/IN named (4), §7 P1 hit-rate
baseline (5).

Torvalds: ACK conditional on findings 1–5 (+6, 7) — folded: §7 P1 frozen
weights / ≤500-seed budget / landing-note record (1), §5 directed template
seeds as the hard gate + histogram demoted to telemetry (2), §6 per-seed
workload + measured budget + shared container (3), §4 INFRA/MISMATCH/DECLINE
classification + new-signature nightly policy (4), §6 bounded flake-safe
minimizer (5), OQ-4 resolved as P3 requirement (6), §7 P4 aggregate-oracle
honesty note (7).

Delta re-confirmation (2026-07-18, folded head): **Graefe ACK-DELTA** — two
refinements folded (§5 nightly-scale empty-bucket hard gate; §3 LIMIT
`min(k, |M|)`). **Torvalds ACK-DELTA** — one constraint folded (§4 ledger is
Go=M/Java≠M-only; Go≠M always fails). RFC is implementation-ready.

## 11. P1 landing note (2026-07-18)

Shipped: `pkg/relational/conformance/rowdiff/` (generator, Oracle M, runner,
templates, comparator sensitivity tests), `cmd/sql-diff-stress`,
`TestFDB_RowDiff_Smoke` (sqldriver suite, shared container), and the
`embedded.PlanPhysicalForTest` typed-plan helper for family bucketing.

**Acceptance run (§7 P1) — PASSED.**
- Target tree: `fb2dead8e` (the pk-intersection residual bug live on master;
  the fix landed as its child on this branch). Harness overlaid as new files
  only — no planner code from the fix present in the target tree.
- Frozen weights: `gen.go` sha256 prefix `41d46411683d3a50`, byte-identical
  in the acceptance worktree and the landing commit. Zero tuning after the
  run.
- Budget/result: 500 random seeds (start 1), templates DISABLED, 11,976
  comparisons in 1m45s (4.8 seeds/s). **23 distinct seeds mismatched
  (hit rate 4.6%), first hit seed 36, 0 infra.** Every sampled mismatch is
  the target class (winning intersection dropping the unindexed residual;
  e.g. seed 36: `WHERE (c IS NULL) AND (s='') AND (b<=6)` over
  idx_c+idx_s+idx_a — engine 3 rows, oracle 2).
- Histogram comparison (measured, 500 seeds each): Intersection bucket 123
  on the buggy tree vs 109 on the fixed tree — directionally consistent with
  the audit's cost-rung analysis (the under-filtered intersection costed
  cheaper and won more often), though the bulk of the 23 wrong-rows seeds
  sit inside intersections that win on BOTH trees and merely dropped their
  residual on the buggy one.
- Cross-check: the same 500 seeds + all templates on the FIXED tree run
  clean (0 mismatches, 0 infra; 11,996 comparisons in 1m51s).

Smoke budget (§6): measured 4.1–4.8 seeds/s; 25-seed smoke ≈ 6s + template
seeds ≈ 0.4s, far inside the ≤2 min budget, sharing the sqldriver suite
container (no extra startup).
