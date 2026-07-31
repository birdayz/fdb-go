# RFC-201: The layered test corpus — reference, oracles, generation-and-commit

**Status:** Accepted (owner directive 2026-07-31: "super extensive suite that ensures
everything is tested, happy to have 100k and more tests"; amended same day: "we must
generate and commit — not a one-off job, generate real tests").

**Decision in one sentence:** build the suite as three layers — a vendored Java
reference corpus as the calibration anchor, oracle multiplication that turns every
query into many tests without hand-written expectations, and a **generation factory
whose blessed output is committed to the repository as permanent tests** — all
executing against real FoundationDB, with SimFDB adding only the fault schedules
real FDB cannot be steered into, and with growth governed by ratchets, counted
skips, and per-dimension coverage audit rather than raw test count.

---

## 1. Goal and non-goals

**Goal.** A suite that makes "everything is tested" an honest, measured statement:
every SQL feature, every feature *combination* the grammar can express, every
execution mode (continuations, prepared statements, plan-cache states, index
presence/absence), every fault schedule that changes observable behavior — with
100,000+ committed tests as the expected steady state, each one either carrying a
blessed expectation or a self-checking oracle.

**Non-goals.**
- Test *count* as a metric. The governing metrics are per-dimension coverage and
  the pinned-regression count (§8). 100k tests on one axis and zero on another is
  the failure mode this RFC exists to kill; the repo has shipped bugs under full
  green precisely because coverage gaps are dimensional, not volumetric
  (non-correlated `EXISTS` was wrong on master while every `NOT EXISTS` test was
  correlated).
- Mocked storage. Nothing in this RFC weakens the no-mocks rule. Real FDB is the
  default substrate for every correctness-bearing test (§6).
- A Java-format plan renderer. Java's `explain:` plan strings are a second
  planner-output language; Go asserts plan shape through its own `EXPLAIN` and the
  `explaindiff` golden instead (§3, ruling 2).

## 2. Standing principles

These are restated from CLAUDE.md because every mechanism below is shaped by them:

1. **Real FDB, no mocks.** Testcontainers, the same `foundationdb/foundationdb`
   image Java tests against. A corpus that does not touch real FDB tests our
   beliefs about the storage layer, not the storage layer.
2. **Every proof gets committed.** The generation factory (§5) is this rule at
   industrial scale: a generated query whose result two engines agree on is a
   proof, and it is committed, not discarded at the end of the run.
3. **Counted skips, never silent ones.** Every "cannot run" is a first-class
   ledger entry with a reason class (§8). A skipped `explain:` must never read as
   a pass.
4. **Dimensional coverage audit.** Gaps are found per feature-axis, not per file
   count (§8).
5. **A red nightly is always in scope.** The generative lanes are safety nets;
   a safety net that stays red is not a safety net.
6. **The suite IS the specification** (owner directive 2026-07-31): every test
   case documents expected behavior — a passing test is what "supported" MEANS,
   and behavior without a test is explicitly unspecified: not expected to work,
   not relied upon, and changeable without being a regression. There is no
   third state. Consequences are operational, not aspirational: the supported
   feature surface is *derived from* the passing corpus (the feature matrix is
   generated from tests, never hand-asserted); a documentation claim of support
   must cite the test that proves it; a new feature is not done until the
   corpus covers it (extending the existing e2e definition of done); and the
   skip ledger (§8) is therefore the precise, public statement of what is NOT
   yet supported — a skip class is a specification gap with a name, not an
   embarrassment to hide.

## 3. Layer 1 — the vendored Java reference corpus (the anchor)

Java's own acceptance suite, `fdb-record-layer/yaml-tests/src/test/resources/`:
**238 `.yamsql` files, 2,997 query stanzas** (root corpus 94 files / 2,691
queries; the rest are runner meta-tests, doc-queries, and format fixtures), each
root file with `.metrics.binpb`/`.metrics.yaml` companions. Measured by the
2026-07-31 scoping study; re-measure on every re-vendor.

**Standing rulings (made 2026-07-31, recorded here):**

1. **Vendor verbatim, never rewrite.** Files land under
   `third_party/apple/fdb-record-layer/yaml-tests/src/test/resources/` mirroring
   the upstream path, with a `VERSION` pin (currently 4.12.11.0), a NOTICE clause
   in the `proto/apple/` style, and a Bazel `filegroup` for data deps. Re-sync is
   a plain rsync from the tagged Java checkout; the corpus moves in lockstep with
   the pinned Java version. The `.metrics.*` companions are NOT vendored — they
   assert Java-Cascades-internal counters (task/transform/insert counts) that are
   meaningless for a different planner implementation and would only rot.
2. **No Java-format plan renderer.** `explain:`, `explainContains:` and
   `planHash:` directives become *counted skips* (§8). Precedent is Java's own:
   in multi-server mode Java degrades exactly these directives to no-ops because
   plan strings are not version-stable — Go is, in effect, another server
   version. Go-side plan-shape regression is owned by `explaindiff` (2,485
   queries baselined) and by `plan_contains` assertions in Go-authored scenarios.
3. **The polarity manifest lives in the Go tree.** Corpus files whose pass/fail
   polarity is encoded in Java *test classes* rather than in the data get a
   Go-side manifest recording polarity, fragment status, and per-file skip
   reasons, so re-vendoring stays a clean copy.

   *Measured (Phase 0 parser, Phase 1 execution; supersedes this ruling's
   original "33 negatives / 10 fragments", which was a coarse pre-implementation
   count):* **25 parse-level negatives, 20 execution-level negatives, 9
   fixed-version meta-tests, 2 include-only fragments.** The parse/execution
   split only became drawable once a parser existed. The 9 fixed-version
   meta-tests are the subtler correction: their polarity is defined solely
   against a version the Java test class pins (`SupportedVersionTest` and
   `InitialVersionTest` both pin 3.0.18.0), and no current-version stream runs
   them — so a single-current-version runner, which is what Go is, cannot
   evaluate them in either direction. `InitialVersionTest`'s own
   `shouldPassOnCurrent` / `shouldFailOnCurrent` streams are the authority for
   which files flip.
4. **Strict parsing.** Unknown block, directive, or tag is a parse error, and
   `TestCorpusParses` walks every vendored file asserting the full directive
   surface is understood, emitting a census (files, blocks, queries, configs,
   tags by type) so upstream format drift fails loudly and describably on a
   version bump.

**Phasing** (from the scoping study; each phase independently landable):

- **Phase 0 — vendor + parse.** No execution. The parser, the census gate, the
  polarity manifest. In flight at the time of writing.
- **Phase 1 — runner core, plan assertions suppressed.** Multi-document blocks,
  the `schema_template` lifecycle (already structurally identical to the Go
  harness's runner), `connect:` URI registry, `test_block` presets/modes/
  repetition/seed, `result`/`unorderedResult`/`count`/`error`, all result-side
  tags, version gates collapsed to the single Go version, `include:`.

  *This phase's original target of "~95 files green" is SUPERSEDED by
  measurement: **33 pass, 0 fail, 205 counted skips, 518 asserted queries.***
  The target was not missed so much as never grounded — it was formed before
  anyone had executed the corpus. Running it showed 42 files blocked by a single
  unmeasured DDL gap (a value index written as `CREATE INDEX … AS SELECT`,
  which turns out to be a bigger blocker than struct types at 39), and 54 files
  that are meta-tests of the runner rather than tests of the engine. The
  reachable ceiling for Phase 1 as scoped is ~87 files. The lesson generalises
  to the later phases' `+35` / `+16` / `41` / `44` figures: those are file
  counts derived from directive census, not from execution, and each should be
  re-stated as a measurement when its phase lands.
- **Phase 2 — `resultMetadata:` (+35 files) and per-page continuations /
  `EXECUTE CONTINUATION` (+16).** Runner- and driver-side; no planner risk. The
  continuation work is also the prerequisite for the ForceContinuations oracle
  (§4.2).
- **Phase 3 — struct types.** `create type as struct` + struct-typed columns:
  41 files, the single biggest engine gap. RFC-scale work of its own.
- **Phase 4 — SQL functions (`create function`, temporary functions; 44 files),
  views (11), enums (3).**
- **Phase 5 — parameter injection + prepared-statement mode (13), then the long
  tail** (proto-descriptor schemas, vector/semantic search, bitmap indexes,
  `copy_block`).
- **Parallel track, any time after Phase 1 — cross-engine wiring** (§4.1). This
  starts paying before any engine work lands: the Java leg has the full DDL
  surface, so every Phase-3/4 gap becomes a measured per-query divergence
  instead of an estimate.

Layer 1 tops out near ~3k queries. Its value is not volume: it is that every
query carries a *Java-blessed expectation*, making it the calibration standard
for the oracle layer and the blessing authority for the factory.

## 4. Layer 2 — oracle multiplication (the multiplier)

Oracles turn one query into many tests with **no hand-written expectations**.
Each oracle below names its mechanism and its instrument; an oracle without a
gated instrument is prose, not coverage.

### 4.1 Cross-engine differential
Every query executes on both engines — Go in-process, Java via the conformance
server (`plandiff.SetupRunner.RunWithSetup` is the existing entry) — and rows
must agree; where both engines error, the error class must agree (the
conformance principle: doesn't work in Java ⇒ doesn't work in Go, in the same
way). The vendored corpus plugs in behind Phase 1. Disagreement is a bug on one
side, found without anyone predicting the right answer.

### 4.2 Continuation equivalence
Every SELECT re-executes with forced `maxRows=1` pagination; the reassembled
pages must equal the one-shot result, row for row, order for order. This is
Java's `ForceContinuations` config ported as a Go execution mode. It roughly
doubles the effective suite and hammers the continuation token machinery —
which is wire-format-critical and therefore under the hard compatibility line.

### 4.3 Plan-diversity agreement
Cascades' memo holds alternative plans for each query. Execute the losers too:
every alternative that survives to a costed candidate must return identical
rows to the winner. This is the strongest planner-bug detector this repo has —
the wrong-window and signed-zero-DISTINCT defects were both of this class
(two plans, same query, different rows). Instrument: an executor-side harness
that enumerates the memo's plan set per corpus query, bounded per query, with
the per-run plan-pair count reported and floored.

### 4.4 Mode matrix
Prepared vs. simple statements; plan-cache cold vs. hit (Java's `check_cache`
precedent); index sets present vs. deliberately absent (forcing fallback access
paths — the plan changes, the rows must not); chaos fault injection at
transaction boundaries via the existing `ChaosTransactor`/`StoreModel` shadow;
DST seeds under SimFDB (§6).

**Arithmetic.** ~3k corpus queries × 2 engines × forced-continuations ×
prepared/simple × plan-alternatives ≈ **30–50k meaningful executions** from
Layer 1 alone, every one carrying a real oracle.

## 5. Layer 3 — the generation factory: generate and COMMIT (the volume)

**Owner ruling, verbatim intent: generation is not a one-off or nightly-only
fuzz job. The factory generates real tests, and blessed tests are committed to
the repository as permanent suite content.** Transient fuzzing continues to
exist (§5.6) but it is the open-ended search on top of the factory, not the
factory itself.

Why commit rather than regenerate: a committed test with a frozen expectation
survives tool churn — it keeps testing the engine even if the generator, the
oracle infrastructure, or the Java server is broken, replaced, or unavailable;
it is reviewable, bisectable, and shrinks with `git` rather than with tribal
knowledge; and it converts oracle agreement (a moment-in-time fact) into a
regression pin (a permanent one). This is "every proof gets committed" applied
to machine-written proofs.

### 5.1 Pipeline
Five stages, each with a measured output:

1. **Generate.** Grammar-driven query generation from the actual ANTLR grammar
   (synced from Java), weighted toward feature *combinations* — the
   join-under-EXISTS-over-UNNEST shapes nobody writes by hand — over seeded,
   generated schemas and data biased toward the nasty corners: NULLs, signed
   zeros, empty and 1-row tables, duplicate keys, deep struct/array nesting,
   Unicode, boundary integers. All generation is seeded and deterministic; the
   generator version + seed are recorded in every emitted test.
2. **Execute.** Run each candidate on real FDB through the Layer-2 oracles:
   cross-engine differential where the query is in Java's surface, metamorphic
   oracles (§5.5) where it is Go-only.
3. **Bless or file.** Oracle agreement ⇒ the observed rows become the frozen
   expectation. Oracle disagreement ⇒ a bug: auto-minimize (shrink query,
   schema, data while the disagreement reproduces) and file the minimal
   reproducer for immediate root-cause under the standing fix-now rules — the
   minimized case is committed as a pinned regression *with the fix*.
4. **Deduplicate.** Committing 90k variants of one shape is volume without
   coverage. Dedup keys: the plan fingerprint (physical plan shape hash) and
   the feature vector (which grammar productions / type combinations / mode
   flags the query exercises). A candidate is committed only if it adds a new
   point in that space, or replaces a strictly weaker test at the same point.
   The dedup census (candidates seen / points covered / committed) is part of
   every factory run's output.
5. **Materialize.** Committed tests are written in the Go yamsql scenario
   format (single-doc, strict schema — the existing runner executes them with
   zero new machinery), organized by feature-vector directory, in reviewable
   batches: one PR per factory run, carrying the run's census (seeds, counts,
   dedup stats, oracle stats) in the PR description. Generated files carry a
   header naming generator version, seed, blessing oracle, and date.

### 5.2 Growth policy
Per-run commit quotas (e.g. 1–5k per batch) keep PRs reviewable; the factory
runs on a cadence (nightly at first) until the feature-vector frontier flattens
— the steady state is a corpus of 100k+ committed tests reached over months,
not one 100k dump. When the frontier flattens, the factory's job shifts to
covering *new* engine features as they land (a new feature's booking is not
done until the factory has swept its combination space — this extends the
existing e2e definition of done).

### 5.3 Expectation maintenance
A committed expectation is frozen. When intended behavior changes (a planner
fix changes results — rare and always a bug-fix), the affected tests are
re-blessed through the same oracle pipeline in the fixing PR, never hand-edited;
the re-bless diff is part of the fix's review surface. Mass re-blessing outside
a behavior-changing fix is forbidden — it would be the tool overwriting the pin.

### 5.4 Determinism and reproducibility
Committed generators + committed seeds ⇒ any committed test can be regenerated
bit-identically; `TestFactoryDeterminism` pins a sample. Generation never uses
wall-clock or unseeded randomness.

### 5.5 Metamorphic oracles (for Go-only surface and extra depth)
Where the Java engine cannot serve as the oracle (Go's sanctioned read-side
extensions) or as an additional independent check everywhere else:
- **TLP** (ternary logic partitioning): rows of `WHERE p` ∪ `WHERE NOT p` ∪
  `WHERE p IS NULL` must reconstruct the unfiltered set — finds predicate and
  NULL-semantics bugs.
- **NoREC**: the optimized form vs. a deliberately unoptimizable rewrite must
  agree — finds planner bugs without a second engine.
- **Index-agreement**: same query with an index present vs. absent — access
  path changes, rows must not.
- **Aggregate-recompute**: aggregate-index answers vs. recomputation over the
  base scan.

### 5.6 The transient fuzz lane (kept, on top)
Open-ended fuzzing (the existing 105 fuzz targets, plus query-level fuzzing at
higher volume than the factory commits) continues nightly. Its failures follow
the same minimize-pin-fix path; its non-failing output is NOT committed — the
factory is the committing path precisely because it deduplicates and blesses,
which raw fuzz volume does not.

## 6. Execution substrates: real FDB, SimFDB, chaos-shadow

| What | Substrate | Why |
|---|---|---|
| Corpus, differential, oracles, factory generation & blessing | **Real FDB** (testcontainers) | wire format on real keys, real 5s tx limit, real splits, real continuations — the ground truth |
| Fault-schedule replays of the same corpora | SimFDB (RFC-199) | deterministic injection of `1007`/`1020`/`1021`/`commit_unknown` at chosen points; seed-reproducible; a fault space real FDB cannot be steered into |
| Random DML + fault programs | Real FDB + `StoreModel` shadow | storage truth with a model oracle at every step |
| Parser/format gates, factory dedup, census gates | No FDB | pure computation; keeps the fast CI tier fast |

Cost model for the real-FDB default: the dominant cost is per-container, not
per-query. One container per test package (existing pattern), schema-template
lifecycle for isolation, `t.Parallel()` throughout, `--local_test_jobs` capping
concurrent containers. Warm-container query cost is ~1–50ms; a 100k-query pass
is 1–3 hours on one box and parallelizes by seed range.

## 7. CI topology

Three tiers, mapped to the real capacity constraints (the CI runner is a
single-slot box; heavy lanes are already nightly there):

- **Per-PR (must stay fast):** build + parse/census gates + every pinned
  regression + a stratified sample of the committed corpus (stratified by
  feature vector, so every dimension appears in every PR run) + the Go-authored
  scenario suite.
- **Nightly:** the full committed corpus under all Layer-2 modes; the factory
  run (generate → bless → open the batch PR); the transient fuzz lane; SimFDB
  fault replays; cross-engine differential sweep.
- **Weekly / on-demand:** the 1M stress family; full plan-diversity enumeration
  at maximum bound; corpus re-vendor check against the pinned Java tag.

A red nightly is triaged same-day under the standing red-safety-net rule.

## 8. Governance — what makes 100k tests mean something

1. **Ratchets.** The corpus pass count, the feature-matrix coverage, and the
   per-dimension counts only go up; a change that shrinks any of them fails CI
   with the shrink named. (Same design as the docscheck ratchet: the count may
   rise honestly, never fall silently.)
2. **The skip ledger.** Every non-executed directive/file/query is a counted
   skip with a reason class (`unsupported-DDL:struct`, `plan-assertion`,
   `polarity:negative-meta`, …), extending the existing yamsql coverage
   classifier vocabulary. The ledger is printed per run and gated: a skip class
   growing is a visible event.
3. **Dimension audit.** Coverage is reported per feature axis (grammar
   production × type × mode), from the factory's feature-vector index and
   `bazelisk coverage` for code-level gaps. The audit's job is to answer "which
   dimension has zero tests" — the question raw counts cannot.
4. **The pinned-regression count** is the headline metric: it only grows, and
   each unit is a bug that cannot return.
5. **Instrument hygiene.** Every counter this RFC introduces is floored or
   asserted by a gate that fails when the counter goes dark (the dead-counter /
   proxy-metric / tautological-partition failure classes are this repo's
   measured bug surface — three ledger entries in the RFC-197 campaign were
   instrument failures).

## 9. Sizing and trajectory

- **Weeks:** Phase 0–1 — vendored corpus parsing + runner core, ~95 files
  green, skip ledger live, cross-engine wiring started.
- **1–2 months:** Phases 2 + oracles — 30–50k oracle-multiplied executions;
  factory pipeline online with first committed batches.
- **The quarter:** factory steady-state growth toward 100k+ committed tests;
  Phases 3–4 close the struct/function/view/enum gaps and the factory sweeps
  each newly-opened surface; SimFDB replay lane live.
- Throughout: the pinned-regression count and the dimension audit are the
  numbers reported, not the raw total.

## 10. Relationship to existing assets

Nothing here replaces an existing instrument; each is slotted:
Go-authored yamsql scenarios (342) — stay, and become the factory's output
format; cross-engine specs (508) + plandiff/rowdiff/explaindiff — become the
differential substrate at corpus scale; memoinvariant — joined by the
plan-diversity oracle; binding stress, chaos, 1M stress — unchanged, weekly
tier; 105 fuzz targets — the transient lane; conformance Java server — the
blessing authority for the factory and the differential's Java leg.

## 11. Sequencing and bookings

Phase 0 is in flight (branch `feat/yamsql-corpus-phase0`). Each subsequent
phase, the oracle harnesses, the factory pipeline, and the SimFDB replay lane
get numbered TODO.md bookings with their gates when the file is free (it is
owned by an in-flight branch at the time of writing); this RFC is the design
they cite. Engine-gap phases that touch the query engine (struct types above
all) carry the standing Graefe RFC + review gate individually.
