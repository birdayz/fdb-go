# RFC-082: Conformance suite is red — join column-label divergence + miswired Go-only scalar-subquery scenarios; gate conformance on CI

Status: Draft

## Problem

`//conformance:conformance_test` (the Go↔Java cross-validation, tagged
`conformance_java`) is **excluded from both `just test` and PR CI**
(`--test_tag_filters=-conformance_java,-stress` in `justfile:67` and
`.github/workflows/ci.yml:41`). It runs only in two nightlies. Un-gating it
locally and running it surfaced **2 real failures on master** that have been
green on every PR:

1. `RunSql Harness … SeedRunCorpus` entry **`inner_join`**: column metadata
   diverged. `SELECT u.name, o.total FROM Users u, Orders o WHERE u.uid = o.uid`
   — Java labels the columns `[NAME, TOTAL]`; Go labels them `[U.NAME, O.TOTAL]`.
2. `yamsql cross-engine equivalence (A3)` scenario **`scalar_subquery`** test #0:
   `SELECT (SELECT MAX(v) FROM t) FROM t WHERE id = 1` — Java throws `42601`
   syntax error; Go executes it.

For a project whose entire thesis is wire/behavior compatibility with Java, the
one suite that proves it being outside the merge gate is the headline problem.

## Investigation

Ground truth was obtained by probing the **real Java conformance server**
directly (a temporary spec, since deleted) rather than reasoning from the log.

### (1) Join column labels — a real Go bug on the shared surface

Probed Java vs Go column labels across shapes:

| Query | Java | Go |
|---|---|---|
| `SELECT u.name, o.total` (join, qualified ref) | `[NAME, TOTAL]` | **`[U.NAME, O.TOTAL]`** |
| `SELECT name, total` (join, bare ref) | `[NAME, TOTAL]` | `[NAME, TOTAL]` |
| `SELECT u.name AS un` (join, explicit alias) | `[UN]` | `[UN]` |
| `SELECT u.name FROM Users u` (single table) | `[NAME]` | `[NAME]` |

So the leak is surgical: **only an unaliased `alias.field` projection over a
multi-source (join) input.** Two code paths produce it (verified by
instrumentation):

1. **Projection** (`SELECT u.name, o.total`): `deriveColumnsFromProjection`
   (`cascades_generator.go:1760`). The projection `FieldValue` for a join carries
   its qualifier *inside* `fv.Field` itself — `fv.Field == "U.NAME"`,
   `fv.Child == nil` — whereas a single-source field has `fv.Field == "name"`.
   The derived `name` (used for `ColumnDef.Name`) is therefore `U.NAME`.
2. **`SELECT *` / `a.*`** (`SELECT * FROM a, b`): `deriveColumnsFromJoin` →
   `qualifyAndMergeColumns` (`cascades_generator.go:1942`), which sets
   `qual.Name = alias + "." + col` and never sets a display `Label`.

The RFC-077 7.6 source-anchored-join rework (commits
`1c0fd31b`/`01837863`/`5aa8819b`, 2026-06-05) is the likely regression origin.

`ColumnDef.Name` is *also* the datum-map lookup key and the type-resolution key,
and a join legitimately needs the qualifier to disambiguate same-named columns
across legs (`descriptorForColumn(name, descs)`; `SELECT *` over a join keeps a
duplicate `uid`). So `Name` must stay qualified. The user-visible column name
comes from `cascadesRows.Columns()` / `paginatingRows.Columns()`, both of which
return `ColumnLabel` = `Label`-or-`Name`. `Label` is purely the display name.
Fix: set `Label` to the **unqualified** field (`parseColRef(field).bare()`) on
both paths; leave `Name` (datum key) untouched.

### (2) Scalar subquery — a Go-only grammar extension, miswired into cross-engine

`scalar_subquery.yaml`'s own header documents it as a "Grammar extension from
nightshift-39 (`subqueryExpressionAtom` in `expressionAtom`)". Probing Java
confirms it rejects **every** subquery-as-value-expression form — projection,
WHERE, arithmetic, aliased, zero-row — with `42601` syntax error. Java
fdb-relational 4.11.1.0 has no subquery-expression-atom at all.

The A3 harness (`yamsql_cross_engine_conformance_test.go`) asserts BOTH engines
succeed and agree. Its own header already states the established pattern:
"Java-unsupported features (GROUP BY, DISTINCT, LIMIT, multi-col ORDER BY)
trigger Java errors and stay on the not-yet-wired list." Per CLAUDE.md, net-new
read-side extensions Java lacks entirely are allowed when wire compat holds and
they have deep Go-only coverage — they must NOT sit in a cross-engine
*equivalence* harness. The four scalar-subquery scenarios
(`scalarSubqueryScenario`, `scalarSubqueryTypesScenario`,
`scalarSubqueryAdvancedScenario`, `scalarSubqueryDmlScenario`) were added to
`crossEngineScenarios()` in error.

Go-only coverage already runs in `just test` and is unaffected:
`pkg/relational/sqldriver/scalar_subquery_cte_test.go`,
`quality_probes_test.go`, and the yamsql `testdata/scalar_subquery*.yaml` corpus.

## Fix

1. **Join column labels** — set the display `Label` to the unqualified column on
   both paths, leaving `Name` (datum/type key) qualified:
   - `deriveColumnsFromProjection`: introduce a display label distinct from the
     alias-override; for an unaliased `FieldValue` set
     `Label = ToUpper(parseColRef(fv.Field).bare())`. Aliased and
     non-field-expression cases unchanged (`Label = alias` / `Label = _i`).
   - `qualifyAndMergeColumns`: when qualifying `Name`, set `Label` to the bare
     column if it has none yet (propagates through nested joins).
2. **Scalar subquery**: remove the four Go-only scalar-subquery scenarios from
   `crossEngineScenarios()`, with a comment explaining Java's grammar lacks
   subquery-expression-atoms and pointing at the Go-only coverage.
3. **Gating**: align `justfile:67` and `ci.yml:41` to drop `-conformance_java`
   so conformance runs in `just test` (pre-commit) and on every PR. `-stress`
   stays excluded (separate heavy 1M tier).

## Performance

None. (1) touches result-set metadata construction only (one extra
string-upcase per projected column, off the row hot path). (2) removes test
scenarios. (3) adds ~250s (cached Java artifacts) to CI on self-hosted runners —
acceptable for the project's core correctness gate.

## Test plan

- Regression for (1): the existing `inner_join` corpus entry now passes;
  additionally an `sqldriver` FDB test asserting `Rows.Columns()` for a
  join with unaliased qualified projections returns unqualified labels, plus the
  single-table / bare-ref / `AS`-alias shapes (lock all four rows of the table
  above so the qualifier can't creep back in on any axis).
- (2): the full conformance suite goes green; scalar-subquery Go-only tests
  still pass under `just test`.
- Whole-suite proof: `just test` (now including `conformance_java`) is green
  end to end — this is the gate that was missing.

## Expanded scope: the gate was hiding a much larger red

Un-gating conformance and fixing the join-colname leak advanced `SeedRunCorpus`
past its first-divergence (`inner_join`) and exposed that the suite has been
**broadly red for a long time, unwatched** (it runs only nightly, off the merge
path). A full categorized enumeration (temporary probe, deleted) of the 1616-entry
corpus through both engines, after the join-colname fix:

| Class | Count | Meaning | Disposition |
|---|---|---|---|
| `OK` | 997 | exact match | pass |
| `JERR_GERR_MSGMATCH` | 97 | both reject, same root message | pass |
| (pre-annotated `Divergence`) | 63 | already documented | skipped |
| **`COLS_NAME_ANON`** | **278** | Java `_0`/`_1`, Go descriptive label, **types match** | **RELAX harness** |
| **`COLS_TYPE`** | **101** | column TYPE differs (names match) | **FIX Go type derivation** |
| **`JERR_GOK`** | **34** | Java errors, Go succeeds | **ANNOTATE extensions / FIX `agg_in_where`** |
| **`JERR_GERR_MSGDIFF`** | **25** | both reject, wording differs | **ANNOTATE `BothErrorMessagesDrift`** |
| **`GERR_JOK`** | **18** | Go errors, Java succeeds | **FIX Go gaps** |
| **`ROWS`** | **2** | row values differ | **FIX (correctness)** |
| **`COLS_NAME_REAL`** | **1** | non-anonymous name mismatch | **FIX** |

~459 entries currently fail. Per-class decisions (Graefe + Torvalds reviewed each):

1. **`COLS_NAME_ANON` (278) — RELAX, tightly.** A column label for an unaliased
   projection is presentation metadata, not wire format; Java synthesizes `_N`
   from ordinal position because it has no source name, Go derives a descriptive
   name from the Value tree. Neither is "more correct"; the *type* is. Allowed
   read-side improvement (CLAUDE.md "query reach is not the hard line").
   Implemented via `plandiff.ConformColumns`: assert arity + per-column TYPE
   unconditionally; relax the NAME **only** when Java's matches `^_\d+$` and Go's
   is non-empty. Never suppresses a type or non-anonymous name mismatch.
2. **`COLS_NAME_REAL` (1) — FIX.** `SELECT COUNT(*) AS cnt` → Go drops the `AS`
   alias (`COUNT(*)` vs Java `CNT`). Explicit alias must win on the aggregate
   branch. Same family as the join-colname fix.
3. **`COLS_TYPE` (101) — FIX Go (Java is right).** Go runs no expression-type
   derivation for several Value shapes: arithmetic returns `BIGINT` instead of the
   numeric-promotion result (`DOUBLE`), `CASE` returns `UNKNOWN` instead of the
   branch common-supertype, UUID `UNKNOWN` vs Java `OTHER`. Bytes `BYTES` vs Java
   `BINARY` is a Type-enum spelling skin over the same wire type → align Go to
   `BINARY` (do NOT relax type comparison). Query-engine change → Graefe-gated.
4. **`GERR_JOK` (18) — FIX all (Go is the buggy side).** IEEE float division
   (`x/0.0`→±Inf, `0.0/0.0`→NaN) must not raise `22012` for floating types
   (integer `/0` still errors); derived-table/CTE expr-alias resolution and
   correlated-EXISTS-over-CTE visibility are scope-resolution bugs; UUID equality
   "could not plan" is the same typing gap as (3) in the planner. Large planner
   gaps that can't land in this PR get annotated `JavaSucceedsGoRejects` with a
   tracked TODO rather than a wrong result.
5. **`JERR_GOK` (34) — ANNOTATE extensions, FIX one bug.** LIMIT/OFFSET, in-memory
   sort of an unindexed arithmetic ORDER BY, parenthesized/`CASE`-boolean WHERE
   predicates are legitimate Go read-side reach Java lacks → `DivergenceJavaErrorsGoCorrect`,
   each with a one-line proof that Java genuinely rejects (no laundering).
   **Exception: `agg_in_where` (`WHERE COUNT(*) > 0`) is a Go correctness bug, not
   an extension** — an aggregate in WHERE is ill-formed SQL (no group to evaluate
   over); Java's rejection is correct. Go must be **FIXED to reject**.
6. **`JERR_GERR_MSGDIFF` (25) — ANNOTATE `BothErrorMessagesDrift`.** Both reject
   correctly; message-only wording drift is acceptable. Pin a cause-specific
   `GoErrorContains` substring so the annotation proves Go rejects for the right
   reason.
7. **`ROWS` (2) — FIX (highest severity).** `WHERE pred OR EXISTS(...)` returns
   empty. Root cause (Graefe): Go hoists the `EXISTS` quantifier out of the `OR`
   into the FlatMap join graph as an unconditional semi-join, losing the
   disjunction; in Java the existential stays inside the `OrPredicate` as a boolean
   sub-expression. Fix where Go lowers `ExistsPredicate`/correlated quantifiers
   under an `OrPredicate`. Pin both corpus rows.

**Packaging.** Torvalds recommends splitting (correctness / type-derivation /
relax+annotate+gate) into separate PRs; per explicit request this ships as ONE PR
with cleanly-ordered commits — correctness → type derivation (Graefe-gated) →
harness relax + annotations → CI gate **last** (only after the rest is green).
Reviewers: Graefe, Torvalds, @claude, codex.

## Progress (this PR)

Cross-engine SeedRunCorpus failures cut from **459 → a small tracked tail**:

| Fix | Mechanism | Cleared |
|---|---|---|
| Anonymous-label relax | `ConformColumns` (arity+types+named-cols asserted; `_N` label relaxed) | 278 |
| Join column-label leak | `parseColRef().bare()` display label; `Name` stays qualified | (join entries) |
| CASE / COALESCE / GREATEST / LEAST / IF type | `commonBranchType` / `polymorphicResultType` | ~50 |
| Arithmetic result type | numeric promotion via descriptor (`arithTypeNameViaDesc`) | ~ |
| IEEE float `/0` → ±Inf/NaN | `evalFloat` (integer `/0` still errors) | 2 |
| UUID type → OTHER + value → canonical string | `protoFieldTypeName` + executor `uuidMessageToString` | ~6 |
| EXISTS-under-OR | reject (was silent empty rows); inline-EXISTS-under-OR is future work | 2 (now Go-rejects) |
| 74 divergence annotations | `rfc082Divergences` (32 JavaErrorsGoCorrect, 18 JavaSucceedsGoRejects, 24 BothErrorMessagesDrift) | 74 |

### Tracked tail (not yet green)

- **bytes BINARY type-name** (8): Go reports `BYTES`, Java `BINARY` (same wire type). Rename ripples through ~6 driver type-name sites + tests; deferred.
- **derived-table type propagation** (4): `(SELECT sum(v) AS t FROM ...) AS s` → `s.t` reports UNKNOWN; the derived column type isn't propagated through the sub-select.
- **integer-literal typing** (3): Go types `42` / `GREATEST(1,5,3)` as BIGINT; Java as INTEGER.
- **alias-on-aggregate** (1): `COUNT(*) AS cnt` drops the alias (bare aggregation has no projection layer to carry it).
- **Go-too-lenient (3, withheld from annotation — fix-or-accept decision for reviewers):**
  `agg_in_where` (`WHERE COUNT(*)>0` accepted — should reject), `type_mismatch_boolean_eq_int` (`bool = int` returns empty — should reject), `cast_bigint_to_boolean` (Go allows the cast Java disallows — defensible extension vs. bug).
- **A3 `union_columns_extended`**: Java deterministically rejects `... UNION ALL ... ORDER BY id, v` ("non existing column V"); needs the A3 harness to treat it as Java-unsupported.

The CI gate (`justfile` + `ci.yml`) flips to include `conformance_java` as the **final** step, only once the tail closes and the suite is fully green.

## Regression lock (the actual un-gate)

You can't flip a green gate while the tail is red, and you must NOT annotate real
Go gaps as xfail-green (laundering). The third option (Torvalds): a regression
**lock**. `conformance_java` runs in CI; the SeedRunCorpus harness asserts the
diverging set is EXACTLY `rfc082KnownRed`:

- a non-annotated entry that diverges but is **not** locked → fails the suite (a
  regression caught at the merge gate — the disease this PR cures);
- a locked entry that silently starts **passing** → fails the suite, forcing its
  removal (the lock only shrinks).

`entryConforms` is the per-entry predicate (matching root error, or conforming
columns + equal rows); annotated entries keep their pinned assertions. This lets
the gate flip (`-stress` only) with the tail still red — without faking green and
without leaving new breakage unwatched. When `rfc082KnownRed` reaches empty it
becomes a plain all-green gate.

## Final state

`conformance_java` now runs in `just test` and PR CI (`-stress` only excluded).
The SeedRunCorpus regression lock is GREEN with **28 known-red entries** in
`rfc082KnownRed` (the withheld Go-too-lenient cases, the result-set type/name
derivation tail — derived-table types, integer-literal INTEGER, alias-on-
aggregate — plus pre-existing CAST/recursive-CTE/`SELECT *`-mixed-types
divergences and seven inline annotations that had silently drifted while the
suite was ungated). The cross-engine SeedRunCorpus went from 459 raw divergences
to 28 tracked-and-locked. The lock fails on any new divergence (a regression) or
any locked entry that starts passing (forcing the list to shrink). When
`rfc082KnownRed` reaches empty, the gate is plain all-green.
