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

## Java conformance-server determinism (the deeper fix)

Un-gating A3 surfaced that the Go↔Java cross-engine gate was *nondeterministic
across runs* — a different scenario failed each run. The root cause was pinned
with a direct experiment (`java_planner_warmth_proof_test.go`): the SAME query,
run as the first query on each of N freshly-spawned JVMs (cold) and again after
warm-up, is **12/12 deterministic** — `SELECT COUNT(*)` always succeeds,
multi-column `ORDER BY` always throws `UnableToPlanException`, etc. So there is
**no query-level, cold-start, or planner nondeterminism**, and
`UnableToPlanException` is itself deterministic for a fixed query + server state
(thrown only at `resultOrFail()`/`finalExpressions.isEmpty()` after full
exploration; task-budget exhaustion throws `RecordQueryPlanComplexityException`).
The earlier "tolerate Java planner nondeterminism" code was a misdiagnosis and is
removed.

Crucially, **there is NO cross-query JVM-state pollution** — the theory an earlier
pass reached for, and never pinned. It is directly refuted: a SINGLE shared Java
server runs all ~119 A3 scenarios (and SeedRunCorpus's ~1620 queries) with
byte-identical results across 3 independent runs each. If process-global state
poisoned later queries, a single JVM over hundreds of queries would be the first
casualty; it isn't. (The once-suspected mechanism — the ANTLR parser's static
`_decisionToDFA`/`PredictionContextCache` — is a performance cache that yields
identical parse trees cold or warm; it does not change results.) So the
"different scenario each run" symptom had exactly two real contributors plus a
reporting artifact — none of them "warming":

1. **Read-version (GRV) lag under CONCURRENT server spawning.** All servers share
   the one FDB testcontainer. Spawning a server *while another runs a query* makes
   that query's transaction race the spawn for the cluster: it can take a read
   version from BEFORE its own ephemeral-schema `CREATE` committed, see no table,
   and throw a SPURIOUS `UnableToPlanException`. (Proven: a query 12/12 OK with
   sequential spawning intermittently fails under concurrent spawning.) **Fix: the
   pool never spawns while a query runs** — and with the shared-reuse model below
   it barely spawns at all (one JVM for the whole suite by default).

Reporting artifact (failures were deterministic; the REPORT looked random):

2. **Ginkgo `Ordered` skip-after-failure masked a deterministic failure set.**
   All A3 scenarios are nested in one outer `Ordered` container (required to host
   the run-once pool `BeforeAll`). Ginkgo's default for `Ordered` is to SKIP every
   subsequent spec once one fails; combined with Ginkgo's randomized run order,
   this meant the *first* broken scenario in that run's order failed and **all
   other broken scenarios were skipped** — so each run reported exactly one
   failure, a different one each time, even though every one of those failures was
   individually deterministic (verified: each fails 10/10 in isolation). This was
   the dominant "looks nondeterministic" effect and sent the investigation chasing
   a planner ghost. **Fix: `ContinueOnFailure` on the outer container**
   (Torvalds-reviewed; the idiomatic Ginkgo decorator for "shared setup +
   independent specs"). Every scenario now runs regardless of earlier failures, so
   a single run surfaces the COMPLETE, order-independent failure set — which is
   what let the genuine Go-only-extension / stateful scenarios below be found and
   excluded in one pass instead of one-per-run whack-a-mole.

Server model + supporting fixes:

3. **Pooled, RE-USED Java servers (default: one shared server, no recycle).**
   Since there is no cross-query pollution, A3 servers are re-used across
   scenarios exactly like SeedRunCorpus's single shared server. `JavaServerPool`
   defaults to size 1 + `maxInvocations` 0 (never recycle), so the whole A3 suite
   runs on ONE JVM spawned once at startup — ~2× faster than fresh-per-scenario
   (~256s vs ~520s for A3) and far lighter on memory (one JVM, not a 16-deep
   buffer; the 16-JVM buffer was what pushed a constrained CI runner into GC
   thrash and a 900s test-timeout). `CONFORMANCE_A3_POOL_SIZE` and
   `CONFORMANCE_A3_MAX_INVOCATIONS` remain as knobs (parallelism; a recycle safety
   valve for a hypothetical future leak). Determinism proven: single-JVM,
   all-scenarios, no-recycle, 3×, byte-identical.
4. **Plan cache disabled** in the conformance server (`makeEngine(planCache = null)`
   — the canonical `Optional.empty()` "cache disabled" path), removing the one
   TTL-evicted, wall-clock-timing-dependent engine cache. Corpus queries are mostly
   distinct, so the lost cache hits cost ~nothing.
5. **JVM lifecycle.** Each server is spawned in its own process group (`Setpgid`);
   `Close()` kills the whole group + reaps (`Wait`) — the Bazel `java_binary`
   launcher is a wrapper script that forks the JVM as a child, so killing only the
   wrapper orphaned the JVM. A registry + an `AfterSuite` sweep guarantee no server
   outlives the suite; each server writes a unique temp cluster-file (not a shared
   `/tmp` path) so concurrent startup spawns can't race. Verified: zero zombies,
   zero orphans after.
6. **Genuine Java-incompatibilities excluded/fixed** — deterministic, confirmed in
   isolation, and (per "extensions where Go works but Java can't are good, not
   divergences") covered Go-only via the yamsql corpus:
   - `NOT NULL` on scalar columns (fdb-relational allows it only on ARRAY) dropped
     from 3 schemas; `unique_violation` reworked to seed its full state in `Setup`.
   - **Go-only read-side extensions Java's planner can't plan** (deterministic
     `UnableToPlanException` or semantic rejection), excluded with notes: multi-column
     `ORDER BY`; explicit `NULLS FIRST/LAST`; **positional `ORDER BY`** (`ORDER BY 2`,
     `ORDER BY 1` — Java can't plan it even over the PK, though `ORDER BY id` by name
     plans fine, so it's the positional reference it rejects); the whole
     `recursive_cte` scenario (the `TRAVERSAL ORDER` clause — absent from Java's
     grammar — plus recursive-CTE + outer `ORDER BY` → "order by is not supported in
     subquery", plus renamed-column recursion); and the whole
     `correlated_exists_advanced` scenario (DISTINCT + comma-join + correlated EXISTS
     + ORDER BY, and correlated NOT EXISTS + ORDER BY, both `UnableToPlan`).
   - **Stateful DML scenarios** whose SELECTs assert state between mutation steps,
     which the stateless A3 harness (schema+Setup+one-query) can't replay:
     `dml_with_null_safe` and `dml_subquery` (its DELETE→re-INSERT→UPDATE→DELETE
     chain ends at one final state, so per-step expectations can't be asserted;
     Go returns the correct final state, verified 10/10). `insert_select` was
     instead TRIMMED to its additive `BIGINT→BIGINT` INSERT…SELECT shapes (which
     both engines agree on), dropping the aggregate-into-`BIGINT` steps — see next.
   - **One real divergence found and filed (not papered over):** `INSERT INTO
     bigint_col SELECT AVG(v)` — Java types `AVG(BIGINT)→DOUBLE` and rejects the
     `DOUBLE→BIGINT` assignment at plan time (SemanticException 22000), accepting
     `SUM(BIGINT)→BIGINT`; Go's `AggregateValue.Type()` derives AVG/SUM from the
     operand (mistyping `AVG(BIGINT)` as `BIGINT`) while the accumulator yields
     `float64`. The correct fix is a Cascades type-derivation change (`AVG→DOUBLE`,
     SUM accumulator→int64) — RFC-gated, **Graefe**-reviewed — tracked in TODO.md,
     NOT masked by a coerce-on-write band-aid (an earlier such band-aid was
     reverted as it diverged Go further from Java).
7. **Tolerance removed.** `isJavaPlannerNondeterminism` deleted; `entryConforms`
   again treats "Java errors, Go succeeds" as a divergence requiring an explicit
   annotation; `divergenceHolds` re-asserts BOTH the annotation's Java premise and
   Go's pinned behaviour (incl. the `JavaIntermittentGoCorrect` arm now requiring
   Java success — codex); A3's `if javaRes.Err != nil { return }` removed.

Bazel caches the conformance test result, so the per-scenario cost is paid only
when an input actually changes — unchanged commits hit the cache.

## Tracked follow-ups (reviewer-requested, non-blocking)

- **Typed-NULL CASE typing** (Graefe): `commonBranchType` skips every
  `*values.NullValue`, including typed ones (`CAST(NULL AS BIGINT)`), so a CASE
  whose only type carriers are typed NULLs reports UNKNOWN where Java honors the
  cast type. Pre-existing gap (no worse than before); skip only untyped NULL.
- **Go-too-lenient fix-or-accept** (Graefe + Torvalds): `agg_in_where`
  (`WHERE COUNT(*)>0`), `type_mismatch_boolean_eq_int` (`bool = int`), and
  `cast_bigint_to_boolean` — decide reject (match Java) vs accept (extension),
  then un-lock.
- **Result-set type/name tail**: derived-table aggregate column types,
  integer-literal INTEGER typing, alias-on-aggregate threading — fix to shrink
  the lock toward empty.
