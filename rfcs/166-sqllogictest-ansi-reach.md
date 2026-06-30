# RFC-166: sqllogictest as an optional ANSI breadth/legibility driver (severed from RFC-165)

**Status:** Draft / Proposed (parked behind RFC-165 Phases 0–1; not on the critical path)
**Gate:** Torvalds + codex + @claude for the harness; each ANSI feature it surfaces is a Cascades change under the full Graefe gate + read-side wire guard.
**Relates:** RFC-165 (the measurement program — the *driver* of the ANSI roadmap is the ledger there, not this format); RFC-164 (WS-1 differential).

---

## 0. Why this is a separate RFC (Torvalds review of RFC-165)

The RFC-165 review correctly refused to bless a full sqllogictest harness inside a "minimal-first" measurement RFC. Severing it forces the honest question: **once corpus import is rejected (it is — see §2) and yamsql's assertions are strictly stronger (`plan_contains`/`error_code`, which sqllogictest cannot express), what does adopting a second format actually buy?** This RFC must answer that on merits before any code. If the answer is "not enough," it stays unimplemented — and that is an acceptable outcome.

### What it buys (the honest, narrow case)

1. **External breadth we would not hand-author.** The permissively-licensed SQLite `test/evidence/*` files are organized by SQL-language clause and probe edge cases (NULL propagation, predicate corners, basic-aggregation boundaries) we are unlikely to think to write. Importing a curated slice through a dialect shim is *gap discovery* against the RFC-165 ledger — it finds `gap` rows faster than hand-authoring does.
2. **Standard-format legibility / external comparability.** Conformance expressed in the lingua franca (SQLite/DuckDB/CRDB/DataFusion all speak it) is legible to outside contributors and comparable to other engines.

### What it does NOT buy (stated plainly, so we don't over-sell)

- It is **not** the ANSI roadmap driver — the **ledger (RFC-165 §4)** is. The `# ansi:` tag + gap derivation produces the backlog without any second format.
- It is **weaker** than yamsql on assertions (no plan-shape, coarse errors), so imported cases are correctness-only and second-class.
- It does **not** replace the cross-engine differential — that is RFC-164 WS-1 / the A3 oracle, reused, not reinvented.

**Decision rule:** implement RFC-166 only if, after RFC-165 Phase 1, the hand-authored ledger has obvious breadth gaps that a curated SQLite-`evidence` import would cheaply fill. Otherwise leave it parked.

---

## 1. Candidate survey (verified)

### 1.1 sqllogictest format (SQLite) — the format to adopt if any

Flat record format authored for cross-dialect result verification: `statement ok|error`, `query <T/I/R> <nosort|rowsort|valuesort> <label>`, `----` separator, MD5 hashing above `hash-threshold`, and **`skipif`/`onlyif <db>`** dialect gating. **License (verified from `sqllogictest.c`):** multi-licensed, reuser elects one of GPL/BSD/MIT/CC0 — we elect **MIT or CC0** (Apache-compatible). The format is an uncopyrightable method (a dozen independent reimplementations exist). Corpus files carry no restrictive per-file headers. Scale: 500+ files, the `test/evidence/` subset (~13 files) is the feature-organized, reusable part.

### 1.2 CockroachDB logictest — format yes, corpus NO

Superset format (`subtest`, `statement count`, `let`, config topologies). **License trap (verified):** master/≥24.3.0 is the proprietary CockroachDB Software License — *cannot* vendor. Pre-24.3 BSL converts to Apache only after each version's Change Date; `v23.1.0` converted 2026-04-01, so as of 2026-06-30 that *exact tag* is Apache-2.0 with notices retained — but the dialect is PostgreSQL/CRDB and mostly will not run (§3). Per-file headers were rewritten at relicense and prove nothing. **Do not bulk-import.**

### 1.3 DuckDB — MIT, thin donor

`.test/.slt` under verbatim MIT (no per-file overrides) → vendorable with the MIT notice + upstream attribution chain. Useful as a transpilation donor for a dialect-neutral slice; don't import its extended directives (`require`, `__TEST_DIR__`).

### 1.4 NIST FIPS 127-2 — avoid

Frozen at SQL-92 (1996), and **not cleanly public domain** — NCC Ltd (UK) + Computer Logic R&D (Greece) hold copyright in parts. Not worth the audit.

### 1.5 Licensing matrix

| Source | Format | Test data | Vendor into Apache-2.0? |
|---|---|---|---|
| sqllogictest (SQLite) | uncopyrightable; runner multi-licensed (elect MIT/CC0) | project-level grant, no per-file headers | **Yes** — elect MIT/CC0 |
| CockroachDB logictest | superset (borrow ideas) | master/≥24.3 proprietary; pre-24.3 BSL→Apache after Change Date | **No** from master; only converted tags + notices — *and dialect mostly won't run* |
| DuckDB | extends format | MIT | **Yes** — retain MIT + upstream attribution |
| NIST FIPS 127-2 | n/a | mixed PD + NCC + Computer Logic R&D; SQL-92 | **Avoid** |
| ISO 9075 Annex F | n/a | text paywalled; **IDs/names are facts** | IDs/names yes (already used by RFC-165 §4) |

---

## 2. The dialect reality — why a vanilla corpus cannot run (verified vs grammar 4.12.11.0)

fdb-relational is not generic SQL:
1. **No bare `CREATE TABLE`** — only `CREATE SCHEMA TEMPLATE`/`CREATE DATABASE`/`CREATE SCHEMA WITH TEMPLATE`; tables live in a template; every table needs a `PRIMARY KEY` (implicitly NOT NULL, leading).
2. **Closed type set** — `{BOOLEAN, INTEGER, BIGINT, FLOAT, DOUBLE, STRING, BYTES, UUID, DATE, TIMESTAMP, VECTOR}` + `<type> ARRAY`. **`STRING` not `VARCHAR`/`CHAR`/`TEXT`; no `DECIMAL`/`NUMERIC`/`REAL`/`INT`.**
3. **No string/math scalar functions** — `UPPER`/`SUBSTR`/`ABS` parse but are rejected at planning (42883), not parse time. Allowed: `COALESCE`/`NULLIF`/`GREATEST`/`LEAST`/`CARDINALITY`/`CAST`/`CASE` + Go datetime.
4. **No `COUNT(DISTINCT)`; no `UNION`-distinct/`INTERSECT`/`EXCEPT`; no `IN (subquery)`.** `LIMIT/OFFSET` works on the **outer** SELECT (rejected only nested/in DELETE — the `SQL_CONFORMANCE.md` "rejects at parse time" line is stale).

**Consequence:** every imported file needs a schema rewrite (bootstrap + synthesized PK), a type-keyword map, and feature-gating of banned constructs. Adoption = parser + dialect shim + curated corpus, never a drop-in.

---

## 3. Design — a `.slt` runner as a `plandiff` front-end (no new engine)

Reuse, verified: `plandiff.SetupRunner.RunWithSetup(ctx, schemaTemplate, setup[], query) RunResult`; `NewGoSQLSetupRunner` (Go-only, wired); `NewJavaRunnerHTTP` + the Java conformance server `runSql`/`runWithSetup` (cross-engine, wired); `JavaServerPool` + 1020-retry + the A3 triple-assertion model. New code is a **front-end only**:
- **Parser** — `.slt` records (typed; no SQL string-matching beyond the format grammar).
- **Adapter/decomposer** — fold the flat statement/query stream into `RunWithSetup` (DDL→template, DML→setup, each `query`→one call). Flag interleavings the folding can't express; don't mangle.
- **Dialect shim** — bootstrap wrap + synthesized `__ROWID` PK; type map (`VARCHAR/TEXT→STRING`, `INT→INTEGER`, `REAL→DOUBLE`, `DECIMAL/NUMERIC`→flag-and-drop); feature-gate banned constructs into expected-error pins (never silent drops).
- **Comparator** — `nosort`→ordered, `rowsort/valuesort`→`Unordered` multiset; canonical `T/I/R`+`NULL`/`(empty)`/`%.3f` rendering; MD5 path (default `hash-threshold 0` for authored files).
- **Engine registry** — `fdbrl` (+ `fdbrl-go`/`fdbrl-java`) for `skipif`/`onlyif`.

Cross-engine mode reuses the A3 pool (the `bazelrunfiles` context, pre-spawn, conflict-retry); error-path cross-engine inherits A3's current gate (Java planner stalls on some error shapes).

---

## 4. Corpus seeding (only if §0's decision rule says go)

Each `.slt` carries `# ansi: <ID>` tags (same convention as RFC-165 §4) + `# source:`/`# license:` provenance; a `TestCorpusLicenseHeaders` guard enforces provenance. Priority: (1) hand-author ANSI-gap-targeted `.slt` mapped to ledger rows; (2) transpile SQLite `evidence/*` through the shim; (3) thin DuckDB (MIT) slice; (4) optional dialect-neutral CRDB `v23.1.0` files with notices.

## 5. Phasing

Strictly downstream of RFC-165 Phase 1. Phase A: parser + adapter + shim + Go-only runner + fuzz; Phase B: ANSI-gap-targeted authored `.slt`; Phase C: cross-engine via A3; Phase D: curated SQLite/DuckDB import for breadth + hardening (hash mode, shared-template optimization). Each ANSI feature it surfaces is implemented under the full Graefe gate (RFC-165 routes it).

## 6. Decision requested

Approve in principle as the *home* for the sqllogictest reach driver, **parked behind RFC-165 Phases 0–1**, to be implemented only if the post-Phase-1 ledger shows breadth gaps a curated permissive import would cheaply fill. Adopt the **format** (elect MIT/CC0); **reject** CockroachDB corpus import. If the breadth case proves weak, this RFC stays unimplemented — an acceptable outcome.
