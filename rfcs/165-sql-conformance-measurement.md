# RFC-165: Measured SQL conformance tracking over the yamsql corpus

**Status:** Draft / Proposed (v2 — incorporates Graefe + Torvalds review)
**Gate:** Torvalds + codex + @claude (test-infra + docs; read-path only, no wire/format change, no Cascades/optimizer/cost/executor code → **no Graefe ACK for the tracking tooling itself**). The *ANSI features this tracking turns into a backlog* are separate: each is a Cascades query-engine change under the **full query-engine gate (Graefe + deep coverage)**, read-side only.
**Author:** cascades-bug-hunt shift
**Scope of THIS RFC:** the measurement/scoreboard (Phases 0–1), built entirely on the existing 319-file yamsql corpus. **No new test format.** The sqllogictest-format program (an optional reach/breadth driver) is severed into **RFC-166** and is not approved here.
**Relates:** RFC-164 (port-fidelity drift detection — the *parity-net* half); FEATURE_MATRIX.md generation pattern; retires the hand-typed numbers in `SQL_CONFORMANCE.md`.

---

## 1. Problem

We claim SQL conformance but cannot *measure* it. Both existing artifacts mislead:

- **`SQL_CONFORMANCE.md`** is hand-typed. Its "Yamsql Conformance" line reads **"115 scenario test suite. Current: 115/115 pass (100%)"** while the corpus on disk is **319** files — a literal nobody recomputed, already a year stale. Its summary carries `~60%`/`~85%`/`~95%` "ANSI coverage" estimates nobody derived from anything. That is marketing, not measurement, and it is the *exact* failure mode (a hand-typed status disconnected from any running test) this RFC must not reproduce.
- **`FEATURE_MATRIX.md`** is generated and drift-guarded (good) but only inventories *our own* corpus — its denominator is "things we already test," so it can never tell us what we're *missing* against the standard.

The user's ask: keep specific, verifiable track of which test cases pass and what percentage of SQL we support — and use that to drive toward ANSI SQL *beyond* the Java port. This RFC delivers the measurement; RFC-166 proposes the optional sqllogictest reach driver on top.

**Anti-rot principle (load-bearing, from review):** every number in every artifact here is either (a) *computed from the corpus by walking it*, or (b) a *pinned fact* (an ISO feature ID; the frozen Java 4.12.11.0 reference). **No status is hand-asserted and then trusted.** A "supported" claim must trace to a named, ANSI-tagged corpus case that the live FDB/cross-engine lane actually runs — "the file exists" is a fake checkbox by CLAUDE.md and is explicitly insufficient.

---

## 2. Positioning — two axes, and what each harness actually proves

CLAUDE.md names two axes; conflating them is what produced the stale estimates. Precisely:

- **Job 1 — semantic parity with Java (the conformance principle).** Does Go compute the *same answers and the same plan shapes* as the Java Record Layer 4.12.11.0? The harness is **yamsql** + the cross-engine A3 oracle. **What it proves: semantics, not the wire.** A row/plan diff says nothing about key/record/index/continuation *encoding* — wire compat is an FDB-level property tested in `pkg/recordlayer` integration tests, not here. (This correction matters for the `ext` rule in §4.) yamsql stays the **primary** harness: it is native to the dialect and carries `plan_contains`/`error_code` assertions a result-only format cannot express.

- **Job 2 — ANSI reach beyond the Java port.** How much of *standard* SQL do we do, and what's the gap? Per CLAUDE.md "wire compat is the hard line; query reach is not," net-new **read-side** capabilities Java lacks are welcome (wire-safe by construction; deep coverage required). **The driver of this axis is the ANSI feature ledger (§4), not any test format.** The ledger's `gap` rows are the roadmap. A standard *format* (sqllogictest) is a separate, optional question deferred to RFC-166.

### 2.1 Relationship to RFC-164 (the parity-net half)

RFC-164 (`port-fidelity-drift-detection`) and this RFC split along the two axes and share the `plandiff` backend. They are complementary, not competing.

| | **RFC-164** (parity nets) | **RFC-165** (this — measurement) |
|---|---|---|
| Question | Does Go match Java? (catch drift) | How much standard SQL, measured? |
| Output | Nets that catch drift (WS-1…WS-5) + pins | The scoreboard (Ledger A/B) + the gap backlog |
| Artifact kind | a stream of **catches** | a **coverage %** over a fixed corpus |

Three points of contact, corrected per review:

1. **WS-1 is an orthogonal drift net, not Ledger B's denominator.** RFC-164's WS-1 (generative random-SQL Go-vs-Java differential) produces *catches* — unbounded, non-reproducible inputs with no stable denominator. It **cannot** "compute Ledger B's percentage." The clean boundary: **Ledger B = static coverage over the curated corpus** (the FEATURE_MATRIX drift-guard pattern); **WS-1 feeds new `gap`/regression rows and pins *into* the corpus**, which then move the static number. Don't build a second differential; don't conflate the net with the scoreboard.

2. **The bug hunt is this RFC's empirical motivation.** RFC-164's post-mortem — *"the test gap is dimensional, not volumetric; the bug lives in the negative space between tested features"* (one test even *pinned* a bug) — is this RFC's thesis with 9 corpses attached. It also vindicates the harness choice: 6 of 9 are shared-surface parity bugs a standard-vs-ANSI suite would not catch (Go-diverged-from-Java, not from the standard), which is why yamsql + the Go-vs-Java oracle stay primary.

3. **Convergence items.** **NULLS-ORDER** (RFC-164 §5: `RequestedSortOrder` dropped the NULLS axis) is both a port-fidelity bug *and* an ANSI feature (NULLS FIRST/LAST = optional F855). One fix flips both. The 6 already-fixed hunt bugs become **evidence pins** for specific subfeatures once their regression scenarios are ANSI-tagged (§4): CAST-ROUND→F201, AGG-RESIDUAL→F131/E051-02, HAVING-PUSHDOWN→E051-06, COUNT-COL→E091-02 (COUNT; E091-01 is AVG), DISTINCT-UNIONALL→E051-01+E071. (Note: this is *subfeature* evidence, not whole-feature `parity` — see the E091 worked example in §4.)

**Guardrail inherited from RFC-164 §WS-2:** plan assertions claim *a specific operator fired* (`AggregateIndexScan`), never an unsound structural invariant (distinctness legitimately arises from a unique index/PK/streaming-agg/intersection with no Distinct operator; ordering⇒NULL-placement is runtime, not structural).

---

## 3. Ledger B — generated corpus coverage (`SQL_COVERAGE.md`)

Pure FEATURE_MATRIX pattern: a library function walks `testdata/*.yaml`, classifies each `Test` by its **typed outcome metadata** (never by string-matching SQL), renders Markdown with computed counts, and a drift guard byte-compares. **Static, no Docker** — runs in every CI/pre-commit pass.

Classification (grounded in the actual corpus: 268 `error_code` asserts, distribution measured):

| Bucket | Rule | Meaning |
|---|---|---|
| **supported** | `Test.Rows` present (positive result assertion) | feature works, rows verified |
| **unsupported (correctly rejected)** | `Test.ErrorCode ∈ {0A000, 0AF00, 0AF01, 42883}` (53+23+15+… cases) | feature not implemented; we cleanly reject it — an honest pin |
| **error-path conformance** | any other `ErrorCode` (42703, 22003, 23505, 42804, 42803, …) | correct *rejection/constraint* semantics (bad SQL / overflow / unique violation), **not** a feature gap |

Output: per-feature-area (reuse `categoryFor`) and total `supported / unsupported / error-path` counts and a real percentage, plus the explicit list of every `unsupported` pin (no silent caps). This is the **measured corpus number**. The drift guard is `TestSQLCoverageUpToDate` (peer of `TestFeatureMatrixUpToDate`).

---

## 4. Ledger A — ANSI conformance (`SQL_ANSI_CONFORMANCE.md`): derived, two-axis, exercised

This is the artifact the reviews reshaped most. The fatal first-draft mistake was a hand-authored 4-state `Status` behind a file-existence guard — i.e. `SQL_CONFORMANCE.md`'s rot with fresh paint. The corrected model:

### 4.1 What is hand-authored vs derived

- **Hand-authored = pinned facts only:**
  - The **ISO roster**: `Identifier | Core? | Name`, pinned to **SQL:2023 Core (177 mandatory features)**. Feature IDs/names are facts (not copyrightable; reproduced from PostgreSQL Appendix D, permissive). They don't rot.
  - The **`Java?` column** is a fact about the **frozen 4.12.11.0 reference** — it only changes on a deliberate, tracked Java-pin bump (the `upgrade-versions` skill), never silently. Where the A3 cross-engine lane covers a tagged feature, `Java?` is **derived** from real Java results and the lane catches any mislabel; elsewhere it is a pinned fact cross-checked as A3 coverage grows.
- **Derived from the corpus (cannot be hand-faked):**
  - **`Go?`** — derived from ANSI-tagged yamsql cases: a feature with a tagged case asserting a **positive** outcome ⇒ `yes`; only `0A000/0AF00/42883` pins ⇒ `no`; mix across subfeatures ⇒ `partial`. This *changes as we implement features*, computed by walking the corpus.
  - **Completeness** — `full`/`partial`/`none` from per-subfeature tagged coverage (the standard's rule: any missing hyphen-subfeature ⇒ parent `partial`).

### 4.2 Two independent axes (per Graefe) — who-supports × completeness

The first draft's single `parity/ext/gap/partial` enum collapsed *who supports it* with *how completely*, hiding the program's whole point. Keep them orthogonal, like `SQL_CONFORMANCE.md`'s `Java | Go` columns already do:

| `Java?` | `Go?` | Meaning | Routes to |
|:---:|:---:|---|---|
| yes | yes | **shared parity** | — (covered) |
| no | yes | **Go-only extension** (`ext`) — wire-safe by construction | credit; optional-features table |
| no | no | **shared ANSI gap** | **RFC-165 read-side backlog** (the roadmap) |
| **yes** | **no/partial** | **port-fidelity divergence** — Java has it, Go dropped it | **RFC-164 bug** (conformance-principle violation) |

That last row is the signal the whole program exists to surface, and the collapsed enum erased it. A `partial` is now always qualified by *which engine* misses the subfeature.

### 4.3 Evidence = exercised, not present (per both reviewers)

yamsql scenarios today carry no ANSI tag (`featurematrix.go` buckets by filename substring only). So:
- Extend the corpus with a `# ansi: <ID>[ subfeature]` leading-comment tag (the parser already extracts leading-comment tags — `featurematrix.go:205 extractDescription`).
- The drift guard `TestAnsiLedgerEvidenceExists` verifies, for every `Go?=yes/partial` row, that the cited scenario **exists, carries the matching `# ansi:` tag, and asserts a positive result (or the specific operator via `plan_contains`)** — so "evidence" means the feature is *exercised*, not that a file resolves. The live FDB/A3 lanes then prove those exact cases pass.
- The 6 hunt-bug regression scenarios get tagged, becoming the evidence pins of §2.1.3.

### 4.4 The `ext` rule, made concrete (resolves Graefe #1 / Torvalds #5)

An `ext` feature (`Java?=no, Go?=yes`) is **read-side and therefore wire-safe by construction**: it adds no write/DDL path, only lets Go *express* more over records Java still writes/reads identically. The evidence test proves *semantics*; wire-safety is the design invariant "introduces no new write path," **not** something a row diff can prove. The guard enforces the construction: an `ext` row's evidence must be a pure read (SELECT/query) introducing no DDL or write not already present in a shared-parity scenario. (The unfalsifiable "evidence proves Java reads identical records" bullet is deleted.)

### 4.5 Worked example — E091, done correctly

The first draft labeled **E091 (Set functions)** three different ways. Correct: COUNT/SUM/AVG/MIN/MAX (E091-01..05) work in **both** engines; only `COUNT(DISTINCT)` (E091-07) is missing in **both**. So E091 is **`Java?=partial, Go?=partial`, shared** — the COUNT-COL hunt-fix is evidence for E091-02 (COUNT; `parity`), and E091-07 (DISTINCT quantifier) is a **shared ANSI gap** → RFC-165 backlog (not a divergence, not whole-feature parity). The fact that one feature was classifiable three ways in the first draft is exactly why `Status` must be derived per-subfeature, not asserted.

### 4.6 Headline numbers (never one blended %)

The headline answers "how much does *this engine* (Go) do," so it is keyed on **`Go?` only** — *never* `(Java?=yes ∨ Go?=yes)`, which would count Java-only features as Go-supported (the exact "marketing, not measurement" failure §1 exists to kill). Over the 177 Core denominator the populations **partition by `Go?`**:
- **Go-supported** = `Go? ∈ {full, partial}` — the headline. Sub-split for context: **shared-parity** (`Java?` supports it too) vs **Go-`ext`** (`Java?=no`).
- **Not in Go** (`Go?=none`), routed by `Java?`:
  - **Shared gap** (`Java?=none`) → the ANSI roadmap size.
  - **Divergence** (`Java?=yes`) → a whole feature Java has and Go dropped → RFC-164 bug (should trend to 0).

`Go-full + Go-partial + shared-gap + divergence = 177` (every feature in exactly one bucket). A `Go?=partial` feature whose *missing subfeatures* Java does support also flags a **subfeature-level** divergence — surfaced in that row's comment, **not** double-counted in the headline. Report each population separately; never average them. Go-`ext` features also appear in a separate "Optional features beyond Core" table so reach gets credit without distorting the Core picture.

---

## 5. Implementation (mirror FEATURE_MATRIX exactly)

- **Library** (`pkg/relational/conformance/yamsql`): `GenerateCoverageReport(dir) (string,error)` (Ledger B) and `GenerateAnsiLedger(dir, roster) (string,error)` (Ledger A — joins the hand-authored ISO roster + `Java?` facts against the derived corpus tags). Rendering deterministic (sorted), pure/static, no Docker.
- **Roster source**: a Go data table `ansiCoreFeatures` (Identifier, Core, Name, Java? fact) — the only hand-authored input; reviewed as facts.
- **Tag extension**: `# ansi: <ID>` leading-comment convention on yamsql scenarios + a helper that maps scenario→IDs.
- **Generator**: `cmd/gen-sql-coverage` thin wrapper emitting both docs (model: `cmd/gen-feature-matrix/main.go`).
- **justfile**: `sql-coverage: go run ./cmd/gen-sql-coverage` (plain `go run`, like `feature-matrix`).
- **Drift guards** (peers of `featurematrix_test.go`, reuse `repoRootForMatrix`): `TestSQLCoverageUpToDate`, `TestAnsiLedgerUpToDate`, `TestAnsiLedgerEvidenceExists` (the exercised-not-exists check).
- **Bazel `data`**: add `//:SQL_COVERAGE.md` + `//:SQL_ANSI_CONFORMANCE.md` to the `yamsql_test` `data` list (load-bearing — else the guard passes vacuously under `bazel test`); add both to root `exports_files`. New `.go` files into the `yamsql` `go_library` srcs; `gazelle`.
- **Docs at repo root** (like FEATURE_MATRIX.md), not `reports/` (RFC-131 keeps it empty).
- **Retire the rot**: replace `SQL_CONFORMANCE.md`'s "115/115" + `~%` with pointers to the generated ledgers; fix its stale `LIMIT/OFFSET … reject at parse time` row (outer LIMIT works; only nested/DELETE LIMIT is rejected).

---

## 6. Phased plan (Phase 0/1 overlap fixed per Graefe)

- **Phase 0 — Ledger B + denominator + retire rot.** `GenerateCoverageReport` over the corpus (measured number today, honestly), the ISO roster denominator + `Java?` facts, the `# ansi:` tag infra and guard, `cmd`/justfile/Bazel wiring + drift guards, retire `SQL_CONFORMANCE.md`'s stale numbers. Ledger A renders with whatever is tagged so far (honest partial).
- **Phase 1 — tag the corpus + emit honest Ledger A.** Walk the 177 Core rows: tag existing yamsql scenarios with their ANSI IDs; the derived `Go?`/completeness then populate. File `Java?=no,Go?=no` rows as the ANSI read-side backlog in TODO.md (prioritized by Core-ness); file any `Java?=yes,Go?=no` rows as **RFC-164 port-fidelity bugs**. This is the roadmap output.

Each phase ships independently, CI green. (sqllogictest reach driver = RFC-166, Phase 2+, not in this RFC's scope.)

## 7. Test plan / proof obligations

- Ledger B + Ledger A generators: unit tests over a fixture corpus (classification buckets; roster×tag join; completeness/who-supports derivation; divergence detection).
- Drift guards regenerate byte-identically; `TestAnsiLedgerEvidenceExists` fails on a `Go?=yes` row whose cited scenario lacks the `# ansi:` tag or asserts no positive result (prove the exercised-not-exists check actually bites — a deliberate red fixture).
- The classification is from typed fields (`Rows`/`ErrorCode`/`PlanContains`), never SQL text (CLAUDE.md NO-TEXT-MATCHING).
- No `t.Skip` except the sanctioned Docker-absent guard. The generators need no Docker.

## 8. Risks & mitigations

- **Ledger A rot (the headline risk)** → `Go?`/completeness *derived* from tagged corpus; only ISO roster + frozen-Java facts hand-authored; evidence guard checks *exercised*, not *exists*. A "supported" claim cannot be free-typed.
- **Mislabeled `Java?`** → derived + A3-cross-checked where covered; pinned-fact-with-tracked-bump elsewhere.
- **Tag drift (scenario repurposed)** → the cited scenario is a live FDB/A3 test; a broken/repurposed case fails there, and the tag-match guard fails statically.
- **Over-claiming "measured" in Phase 0** → Ledger A is honest-partial until Phase 1 tagging completes; the *measured* number is Ledger B (fully corpus-derived) from day one.
- **Denominator gaming** → pinned edition (SQL:2023 Core, 177), subfeature-fails-parent, three populations never blended, `ext` in a separate table.

## 9. Decision requested

Approve **Phases 0–1 only**: (a) generated, drift-guarded **Ledger B** (measured corpus coverage); (b) **Ledger A** with `Go?`/completeness **derived** from `# ansi:`-tagged corpus + the A3 oracle, only the ISO roster + frozen-Java facts hand-authored, evidence guarded as *exercised*; (c) two-axis who-supports model that routes shared gaps to the RFC-165 backlog and Java-only-missing rows to RFC-164; (d) retire `SQL_CONFORMANCE.md`'s hand-typed numbers. **The sqllogictest format program is severed to RFC-166 and not approved here.**
