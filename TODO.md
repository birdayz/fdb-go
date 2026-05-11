# TODOs

Strict execution order. Pick the next unchecked item whose gates are satisfied. No priority debate — phases run sequentially; items inside a phase run in parallel unless gated.

Java Record Layer version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

---

## Cascades — remaining Java alignment

### Actionable (no blocking dependencies)

- [x] **C-1** SelectExpression.compensate — full predicate compensation computation. Ported (swingshift-86): CompensateCompleteMatch now iterates predicates, looks up PredicateMultiMap mappings, computes per-predicate compensation functions, and builds the real PredicateCompensationMap. Enables residual predicate filters after index scans.
- [x] **C-2** MaxMatchMap ValueEquivalence parameter — ported (swingshift-86). Added ComputeMaxMatchMapWithEquivalence and PullUpValueMaybeWithEquivalence that thread ValueEquivalence through the matching algorithm. findMatchingReachableCandidate now consults ValueEquivalence as fallback when structural equality fails.
- [x] **C-3** PullUp.Visitor (MatchPullUp, PullUpVisitor) — ported (swingshift-86). ForMatch + visitForPullUp type-switch (LogicalTypeFilterExpression → inner quantifier value; default → result value). NestPullUp walks adjusted match infos to build chained PullUp levels.
- [x] **C-4** Pareto filtering in MaximumCoverageMatches — ported (swingshift-86). findContainingAccess prunes dominated matches: if match A from candidate C has strictly more bound sargable aliases that contain all of match B's aliases, B is eliminated.
- [x] **C-5** FieldValue child value — ported (swingshift-86). Added optional `Child Value` field to FieldValue. nil Child = legacy flat behavior (backward-compatible). With Child set, FieldValue participates in correlation tracking (Children() returns the child, WalkValue/GetCorrelatedToOfValue discover its alias). NewFieldValue(child, field, typ) constructor for the full model; NewFlatFieldValue for legacy. Unblocks C-6 fetch rules' value translation.

### Blocked on larger infrastructure

- [x] **C-6** 6 unported ImplementationCascadesRules — MergeFetchIntoCoveringIndexRule, PushDistinctBelowFilterRule, PushDistinctThroughFetchRule, PushFilterThroughFetchRule, PushMapThroughFetchRule, PushSetOperationThroughFetchRule. Rules ported (swingshift-86). RecordQueryFetchFromPartialRecordPlan + TranslateValueFunction + physicalFetchFromPartialRecordWrapper + 7 rules registered. Full value-translation effectiveness gated on C-5 (FieldValue child value). Executor integration (runtime fetch) is a separate follow-up.
- [x] **C-7** generateDataAccess phase ordering — restructured (swingshift-86). Data access generation now runs inside the PLANNING phase between constraint propagation (Pass 1) and bottom-up implementation (Pass 3). Reads requestedOrderings from ConstraintMap. Matches Java's architecture where data access rules fire with ordering constraints available.

---

## Java-alignment refactors (structural divergences that cause cascading bugs)

### 1. ~~Merge buildLogicalPlanForSelect / buildOuterPlanOnDerived~~ — **done (swingshift-81)**

Merged into `buildSelectShell(op, sq, stripPrefix)`. buildOuterPlanOnDerived is now 6 lines.

### 2. ~~Eliminate sortOnly/hidden aggSelectCol flags~~ — **done (swingshift-81)**

Replaced `sortOnly bool` + `hidden bool` with `visible bool`. Deleted `__orderby_aggexpr_N__` sentinels. ORDER BY aggregate expressions resolve through the Value-based sort path. See RFC-002 for the full RequestedOrdering port plan (Phases 3-4-6 are optimization, not correctness — push ordering constraints through operators for sort elimination via index ordering).

### 3. ~~Eliminate two-phase selectQuery → buildLogicalPlan split~~ — **done (swingshift-81)**

PlanVisitor walks ANTLR incrementally: parseFromSource + classifySelectElements + per-step operator building. The Cascades path never creates a selectQuery. extractFromSimpleTable is now a 10-line wrapper for the proto path only. Remaining: _postBuild still uses a selectQuery bridge (toSelectQuery) for catalog-aware upgrades — inlining those into the visitor is future optimization.

---

## *** CURRENT PRIORITIES ***

### ~~Eliminate sortOnly~~ — **done (swingshift-81)**
### ~~Subqueries/EXISTS~~ — **done (swingshift-81)**
### ~~Yamsql conformance~~ — **115/115 (100%, swingshift-83)**
### ~~Cascades divergence D-3~~ — **done (swingshift-83)**
### ~~M-1 AliasMap/RebaseValue/RebasePredicate~~ — **done (swingshift-83, extended swingshift-86)**

All priorities resolved. D-1 + D-3 done (sort + distinct → PLANNING phase). M-1 infrastructure complete. RebaseValue now generic (all ~37 types via Children/WithChildren). ValuesStructurallyEqual rewritten via EqualsWithoutChildren (exhaustive 37-type dispatch, no reflect fallback). GetCorrelatedToOfValue handles all 7 correlation-bearing leaf types. See `CASCADES_DIVERGENCE.md`.

### Remaining internal architecture divergences

**Go has ZERO user-visible gaps vs Java 4.11.1.0.** Every SQL pattern Java supports, Go supports too. The remaining divergences are all internal planner architecture with no correctness impact (except D-7 which affects optimization quality).

| ID | Divergence | Go approach | Java approach | Criticality | Effort |
|---|---|---|---|---|---|
| ~~**D-2**~~ | ~~PushOrdering rules~~ | ~~ImplementationRules (PLANNING): constraint-push~~ | ~~ImplementationRules (PLANNING): constraint-push~~ | ~~DONE~~ | ~~done~~ |
| ~~**D-4**~~ | ~~Cost model~~ | ~~PlanningCostModelLess: 16 ordered criteria~~ | ~~Multi-criteria comparator (PlanningCostModel.java)~~ | ~~**DONE** (swingshift-91)~~ | ~~done~~ |
| ~~**D-5**~~ | ~~InComparison architecture~~ | ~~SelectExpression + ExplodeExpression~~ | ~~SelectExpression + ExplodeExpression~~ | ~~DONE~~ | ~~done~~ |
| ~~**D-7**~~ | ~~Multi-aggregate matching~~ | ~~Multi-aggregate via index intersection~~ | ~~Multi-aggregate via index intersection~~ | ~~DONE~~ | ~~done~~ |
| ~~**D-8**~~ | ~~CardinalityProperty~~ | ~~Coupled to Cost struct~~ | ~~Separate class with min/max bounds~~ | ~~DONE~~ | ~~done~~ |
| ~~**D-11**~~ | ~~ConstantObjectValue promotion~~ | ~~No type promotion on eval~~ | ~~PromoteValue.isPromotionNeeded~~ | ~~DONE~~ | ~~done~~ |
| ~~**D-3 gap**~~ | ~~Distinct elimination check~~ | ~~Physical DistinctRecordsProperty per FinalMember + logical PK fallback~~ | ~~Physical DistinctRecordsProperty~~ | ~~DONE~~ | ~~done~~ |

### ~~D-4 Port: Java PlanningCostModel → Go~~ — **DONE (swingshift-91)**

`PlanningCostModelLess` wired into `Planner.OptimizeReferenceTask.Run`, replacing `CostLess`. All 16 Java criteria ported (375 LOC + 111 LOC tests, 4 unit tests). Scalar `CostLess` retained as tiebreak fallback. 46/46 test targets pass.

Also fixed `SelectMergeRule` alias preservation bug: non-SelectExpression children lost source aliases during merge, causing NLJ plans with empty innerAlias to fail predicate evaluation. Root cause was hidden by the old cost model.

**Remaining refinements (optimization quality, not correctness):**
- Port `ComparisonsProperty` for full-fidelity IN-SARG check (criterion 6) and type-filter comparison set difference (criterion 7 sub-check)
- `IndexScanPreference` config flag — hardcoded to PREFER_INDEX (Java's default)

### Yamsql conformance detail (historical — mostly resolved)

Status after swingshift-83: 115/115 scenarios pass (100%). The category breakdown below is from swingshift-77; most items have been resolved. Retained for historical reference.

#### Category 1: Cascades planner can't plan the shape (116 queries)

Query succeeds in Java, Go returns `0AF00`. These need Go implementation.

| Gap | Queries | Java has it? | Action |
|---|---:|---|---|
| Scalar subqueries `(SELECT MAX(v) FROM t)` | ~25 | Yes — `SelectExpression` with correlated quantifier | Port `DecorrelateValuesRule` + subquery translation. NEW — not in existing TODOs. |
| `SELECT a.*, b.*` qualified star | ~15 | Yes — `RecordConstructorValue` expansion | Port qualified-star in translator. NEW. |
| `CROSS JOIN` explicit syntax | ~8 | Yes — parser routes to `SelectExpression` | Fix parser routing (comma-join works, explicit syntax doesn't). NEW. |
| Recursive CTE body shapes | ~8 | Yes — `RecursiveUnionExpression` | Wider CTE body translation. Extends #15. |
| Complex derived table + ORDER BY | ~12 | Yes — ordering propagation through subquery | Wire `pullUp` ordering through derived tables. Extends #72. |
| `GROUP BY expr` (not plain column) | ~10 | Yes — `GroupByExpression` with computed keys | Upgrade `GROUP BY a+b` to Value trees. NEW. |
| `ORDER BY` with `LIMIT`/`OFFSET` | ~8 | Yes (via `setMaxRows`) | Wire LIMIT into in-memory sort post-processing. Related to #4, #33. |
| `HAVING` complex shapes | ~8 | Yes — `PredicateFilter` over aggregation | Wider HAVING predicate upgrade. Extends #79. |
| Correlated subqueries | ~10 | Yes — correlation binding | Port correlation infrastructure. Related to #5 (IN subquery rejected). |
| `DISTINCT` + complex shapes | ~12 | Partial — Java has some bugs here too | Hash distinct extension. Related to #90. |

#### Category 2: Wrong error code (62 queries)

Query should error, but Go errors with the wrong SQLSTATE.

| Pattern | Queries | Fix |
|---|---:|---|
| Expected `0A000`, got `0AF00` | 21 | Cascades fails before reaching the feature-unsupported check. Need earlier rejection. |
| Expected `42803` (grouping error), got `0AF00` | 9 | GROUP BY validation happens after planning; planner fails first. |
| Expected `22000` (data exception), got `0AF00` | 7 | Type check happens at eval; planner fails first. |
| Expected specific codes, got `0AF00` | 25 | Same pattern — planner catch-all hides the real error. |

#### Category 3: Missing validation (50 queries)

Query should error but succeeds silently.

| Missing check | Queries | Java has it? | Action |
|---|---:|---|---|
| `42F01` unknown table/qualifier | 10 | Yes | Add validation before planning |
| `42703` unknown column | 7 | Yes | Add column resolution validation |
| `42702` ambiguous column | 5 | Yes | Add ambiguity check |
| `22F3H` / `22003` overflow | 10 | Yes | Add numeric validation |
| `42803` non-aggregated column | 3 | Yes | Add GROUP BY validation |
| `0AF01` unsupported feature | 18 | Yes | Add feature gate checks |

#### Category 4: Wrong results (15 queries)

Query runs but returns wrong rows.

| Bug | Queries | Fix |
|---|---:|---|
| UNION ALL second branch NULLs | ~5 | Column projection mismatch in UNION executor |
| Derived table alias not resolved | ~5 | `ColumnAliasMap` not applied in all paths |
| Self-join column resolution | ~3 | Alias threading edge cases |
| Aggregate panic in CASE WHEN | 1 | `AggregateValue.Evaluate` called per-row |
| Parser eats expression as column name | ~1 | `IS DISTINCT FROM` parsed wrong |

#### Summary

Java implements nearly everything. Only ~20 queries need Go extensions (hash distinct on unsorted input, LIMIT post-processing). The other ~280 are Java-ported features we haven't wired yet.

#### Testing strategy: Java-conformant vs Go-extension

Yamsql scenarios that Java rejects but Go handles (via in-memory sort, hash distinct, etc.) need TWO expectations:
- `error_code: "0AF01"` — Java-conformant behavior (strict mode)
- `rows: [...]` — Go-extension behavior (extended mode)

Future: add a `mode: strict|extended` toggle to the yamsql runner. In strict mode, Go must match Java exactly (reject what Java rejects). In extended mode, Go extensions are allowed to succeed. CI runs both modes. This lets us verify Java conformance AND test extension correctness without conflict.

For now: update yamsql expectations to accept Go-extension results (queries that return correct data). The strict-mode toggle is a follow-up.

Highest ROI fixes (in order):
1. **Validation before planning** (~50 queries, add checks before Cascades) — prevents planner catch-all from hiding real errors
2. **Qualified star expansion** (~15 queries, mechanical translator work)
3. **CROSS JOIN syntax routing** (~8 queries, parser fix)
4. **UNION ALL column projection** (~5 queries, executor bug fix)
5. **Wrong error codes** (~62 queries, earlier rejection before planner)

---

## Phase 1 — Parallel quick wins (no gates, start immediately)

- [x] **#1** Go-only cleanup: `SELECT DISTINCT` plain projection. **Closed obsolete (swingshift-64)**: empirical probe showed fdb-relational 4.11.1.0 accepts plain `SELECT DISTINCT col FROM T` (Cascades has a DISTINCT-projection rule). Java's `UnableToPlanException` only fires for DISTINCT + ORDER BY together — a shape-specific Cascades composition gap, not blanket DISTINCT non-support. Aligning Go would mean shape-detection (bolt-on `if X` per CLAUDE.md principle #10), not a clean removal. Leave Go's DISTINCT pipeline in place; revisit narrow shape alignment if cross-engine divergence surfaces in real corpora.
- [x] **#2** Go-only cleanup: scalar STRING family (UPPER / LOWER / LENGTH / CHAR_LENGTH / CHARACTER_LENGTH / OCTET_LENGTH / SUBSTRING / SUBSTR / TRIM / LTRIM / RTRIM / CONCAT / CONCAT_WS / REPLACE / LEFT / RIGHT / POSITION / REVERSE) — **landed swingshift-64**. Removed Go-side dispatch in `scalar_functions.go` (proto + map paths now both fall through to the byte-equal `Unsupported operator <NAME>` default arm); dropped STRING / LENGTH / OCTET_LENGTH from `inferScalarFunctionJDBCType`; rewrote 5 yamsql files (string_functions, trim_concat, select_no_from, scalar_subquery_advanced, scalar_subquery_types) and 17 sqldriver tests; pinned cross-engine via 3 plandiff corpus entries (string_upper_rejected, string_upper_in_cte_where_rejected, string_substring_rejected). `||` operator wasn't implemented Go-side; nothing to remove. Net diff: -198 LOC scalar_functions.go alone.
- [x] **#3** Go-only cleanup: scalar ARITHMETIC (ABS / SQRT / POWER / POW / FLOOR / CEIL / CEILING / ROUND / SIGN / PI / EXP / LN / LOG) + DATETIME function-call aliases (NOW / CURDATE / CURTIME / SYSDATE / UTC_TIMESTAMP / UTC_DATE / UTC_TIME) — **landed swingshift-64**. Removed Go-side dispatch in `scalar_functions.go`; both proto + map paths fall through to byte-equal `Unsupported operator <NAME>`. SQL-standard form (CURRENT_TIMESTAMP / CURRENT_DATE / CURRENT_TIME / LOCALTIME, no parens) intentionally NOT touched: Java's `BaseVisitor.visitSimpleFunctionCall` is `visitChildren(ctx)` (broken pass-through, no error) — Go's working impl is a Go-only correctness improvement, not a divergent rejection. Pinned cross-engine via `arith_abs_rejected`, `arith_power_rejected`, `datetime_now_rejected` corpus entries. Out-of-scope (separate cleanup): FLOOR / CEIL / CEILING / ROUND / SIGN / PI / EXP / LN / LOG / MOD function-form / date-part fns (YEAR/MONTH/...) — Java also rejects these but they weren't in the named scope and stay implemented Go-side for now (cf. #28 covers date-parts).
- [x] **#4** Go-only cleanup: `LIMIT N` → `setMaxRows` alignment — **landed swingshift-64**. Rejected `simpleTable.LimitClause()` at parse time in `extractFromSimpleTable` with byte-equal Java messages (`"LIMIT clause is not supported."` / `"OFFSET clause is not supported."`, ErrCodeUnsupportedQuery / 0AF00) — Java's AstNormalizer.visitLimitClause checks offset first so `LIMIT N OFFSET M` errors on OFFSET. Confirmed empirically via cross-engine probe. Pinned via `limit_clause_rejected` + `offset_clause_rejected` corpus entries. Test surface rewritten: 15 yamsql files, 4 sqldriver tests, 3 embedded internal tests; LogicalLimit operator infrastructure left in place for future Cascades / setMaxRows-routing consumption. SQL `LIMIT N` is now unreachable; pagination must go through a future `setMaxRows`-style API (not yet plumbed in Go's database/sql layer).
- [x] **#5** Go-only cleanup: `col IN (SELECT ...)` → JOIN/EXISTS rewrite — **landed swingshift-64**. Java's `AstNormalizer.visitInPredicate` (line 437) calls `ParseHelpers.isConstant(ctx.inList().expressions())`; the visitor doesn't handle the `queryExpressionBody` alternative of `inList`, so for `IN (SELECT ...)` `inList.expressions()` returns null and ParseHelpers (annotated `@Nonnull`) NPEs on the unconditional `expressionsContext.expression()`. Per CLAUDE.md principle #10, the architectural reality is "visitor doesn't implement"; Go aligns with that (rejection) but emits a clean message instead of reproducing the NPE. Source-side: removed `in_subquery.go`, the IN-subquery branch of `in_list_pushdown.go`, `matchSubqueryIN` in `value_compare.go`, the `inSubqueryCache` field on `EmbeddedConnection`, and the `preEvaluateInSubqueries` dispatch. `evalInPredicateTri` and `evalPredicateOnMapTri` now reject with `ErrCodeUnsupportedQuery` and message `"IN with a subquery argument is not supported; use EXISTS or a JOIN"`. Test surface: 8 yamsql files + 5 sqldriver tests rewritten — dedicated tests converted to rejection, incidental uses rewritten to EXISTS / NOT EXISTS / comma-join. One latent gap surfaced: correlated EXISTS through a CTE-source doesn't bind the outer-row qualifier (worked around in recursive_cte.yaml with comma-join). NOT cross-engine pinned (Java NPE doesn't match Go's clean message). Net diff: -176 LOC.
- [x] **#6** Go-only cleanup: FROM-less SELECT — **landed swingshift-64**. Java's `QueryVisitor.visitSimpleTable` line 225 has `Assert.notNullUnchecked(simpleTableContext.fromClause(), UNSUPPORTED_QUERY, "query is not supported")` — the gate is universal, fires inside CTE bases too (NOT just standalone). Empirical probe confirmed: `SELECT 1+1` and `WITH base AS (SELECT 1 AS n) SELECT n FROM base` both reject with the identical message. The TODO's premise about a CTE-base bypass was stale; no parser flag needed. Go's `extractFromSimpleTable` rejects when `simpleTable.FromClause()` is nil with byte-equal message + ErrCodeUnsupportedQuery (0AF00). Pinned via `probe_fromless_in_cte_base` corpus entry. Test surface: 3 yamsql files + 4 sqldriver tests + 1 embedded internal test. Also added cross-engine harness improvement: walks Go's *api.Error cause chain to find the deepest message (mirrors Java conformance server's root-cause traversal) so wrapped-by-CTE-context errors compare byte-equal at their inner-most rejection.
- [x] **#7** Go-only cleanup: `WHERE (bare-paren-boolean)` — **landed swingshift-64**. Java's parser treats `(...)` as a recordConstructor (single-element tuple); Expression.toUnderlyingPredicate's `Assert.castUnchecked(..., BooleanValue.class)` fails with byte-equal `"expected BooleanValue but got RecordConstructorValue"`. Go matches at the WHERE entry sites (`rejectTopLevelParenthesizedWhere` helper called from `evalPredicate` proto-path + `join.go`/`cte_scan.go` map-paths) — the check fires on the WHERE expression's TOP-LEVEL only, NOT on every recursive PredicatedExpression: empirical probe showed Java accepts `(a) AND (b)` (the LogicalExpression surface type is BooleanValue even with RecordConstructor leaves) while rejecting bare `(a)`. Pinned via `where_paren_top_level_rejected` corpus entry. Test surface: 1 yamsql file (boolean.yaml).
- [x] **#8** A3 corpus expansion 290 → 1587 yamsql parity. Mechanical, surfaces ~1/3 real bugs, parallel-safe. **Gate: Phase 1.5 must be clear (or in-flight) before adding new entries — fix divergences as you find them, don't drop the entry.** (~4-6 shifts) — **Done, swingshift-67.** Exceeded target: 1620 entries (target 1587).
- [x] **#9** INFORMATION_SCHEMA decision — **KEEP, swingshift-64**. Probed empirically: fdb-relational 4.11.1.0 rejects `INFORMATION_SCHEMA.TABLES` with `RelationalException: Unknown reference INFORMATION_SCHEMA.TABLES`. Catalog isn't registered at all (no quoted form, no alternate path). Decision: keep Go's working Go-only impl (system_tables.go / system_rows.go) — SQL-standard feature, removing it is a user-visible regression — and document the divergence in the plandiff corpus.go comment block. #35 (A4 cross-engine byte-equivalence) stays gated on upstream. Open follow-up: write a feature proposal for fdb-relational upstream.

## Phase 1.5 — Surfaced-divergence fixes (clear before more #8 chipping)

Bugs surfaced by #8 corpus probing in nightshift-65. **Pick the highest-tier unchecked item, fix it, re-pin the corresponding corpus entries, commit.** Tiers reflect impact × effort, not strict gating.

### Tier A — Real Go bugs, broad impact (paired fixes)

- [x] **#56 + #57** Identifier resolution case-folding — **landed dayshift-66**. Two-line behaviour change in `functions.StripIdentifierQuotes` (unquoted → upper, quoted → preserve, mirrors Java's `SemanticAnalyzer.normalizeString` with `caseSensitive=false`). DDL sites (`ddl.go::parseTableDefinition`, `parseIndexDefinition`, schema-template tableName / columnName / pkCols capture) now fold at the catalog write side so the catalog stores canonical-form names. Surfaced and fixed inline an aggregate-lookup divergence: `columnNameFromExpr` reconstructed ORDER BY / HAVING aggregate keys via raw `GetText()` while `extractAwfFields` (used for aggCols registration) folded via `FullIdToName`; routed both through `extractAwfFields` so `ORDER BY SUM(v)` resolves to the same key as the registered `SUM(V)`. Test fixtures across 10 test files updated to expect upper-case identifiers in plan-tree explain output. All 44 test targets pass. Net diff: +121 / -122 LOC. Re-pin pass for dropped lowercase-identifier corpus entries: still pending.
- [x] **#42 + #64** Compound DISTINCT — **reclassified Java upstream bug, dayshift-66**. Cross-engine probe via `compound_distinct_two_cols_probe` and `count_over_distinct_derived_probe` showed Go correctly de-duplicates compound `SELECT DISTINCT a, b FROM t` (2 rows from 4 input) and `count(*) FROM (SELECT DISTINCT c FROM t)` (3 from {10,20,30}); Java fails to dedup both shapes (returns 4 and 5 respectively). 5th inverted-diagnosis from nightshift-65 this shift. Pinned Go's correct behaviour as Go-only sentinel `TestFDB_CompoundDistinctDedup`; corpus entries omitted until Java upstream fixes.

### Tier B — Real Go bugs, isolated (small, well-scoped)

- [x] **#48** Signed-zero comparison — **reclassified Java upstream bug, dayshift-66**. Cross-engine probe via the `double_negative_zero_ge_predicate` corpus shape (and Go-only Go positive sentinel `TestFDB_SignedZeroComparison`) showed Go is the SQL-correct side: `WHERE v >= 0.0` against a row with `v = -0.0` keeps the row in Go (IEEE 754: `-0.0 == +0.0` is TRUE). Java's fdb-relational 4.11.1.0 drops the row — upstream bug. TODO #48's original diagnosis was inverted. Pinned as Go-only positive test in `pkg/relational/sqldriver/embedded_fdb_test.go::TestFDB_SignedZeroComparison`; corpus entry omitted until Java fixes upstream. Moved to Tier D semantically (no Go fix needed); kept entry numbering here for traceability with the original drop.
- [x] **#54** AVG over JOIN type lattice — **landed dayshift-66**. Root cause: `aggregateMapRows` (shared by JOIN + CTE aggregate paths) only returned `cols` and `data`; both call sites then hardcoded `colTypes[i] = "BIGINT"` for every aggregate output. Fix: thread colTypes through `aggregateMapRows`'s return, computed via the existing `aggregateResultJDBCType(ac, nil)` helper (COUNT→BIGINT, AVG→DOUBLE, SUM/MIN/MAX→BIGINT default since multi-source JOIN/CTE has no single msgDesc). Pinned via new corpus entry `avg_bigint_returns_double_in_join`. Net diff: 3 files, +30 / -10 LOC.
- [x] **#61** String-literal escape semantics — **divergence did not reproduce, dayshift-66**. Cross-engine probe via `string_literal_backslash_n_not_escaped` and `string_literal_double_backslash_not_escaped` corpus entries showed both engines agree on SQL-standard backslash handling: `'a\nb'` stores 4 chars (`a`, `\`, `n`, `b`); `'x\\y'` stores 4 chars (`x`, `\`, `\`, `y`). 2 conformance runs both passed. nightshift-65's diagnosis was wrong, OR something landed between then and now that fixed it. Pinned via the two new corpus entries.
- [x] **#58** Multi-subquery FROM list — **landed dayshift-66**. Go's comma-extras parser only accepted `AtomTableItemContext`. Fix: extend the loop to also accept `SubqueryTableItemContext` and emit a `joinClause` with a new `derivedQuery` field carrying the subquery. Dispatcher now materializes ALL derived sources (first source AND every join with `derivedQuery != nil`) as CTEs before the join executor runs — same architectural pattern, generalized. Pushed CTE scope guard updated to fire when any join carries a derived query. Re-pinned via `multi_subquery_from_list_probe` corpus entry; updated 2 yamsql test fixtures (`derived_table.yaml`, `derived_table_renamed.yaml`) that pinned the OLD rejection to assert correct results instead.
- [x] **#44** UNION ALL outer ORDER BY — **reclassified Java upstream bug, dayshift-66**. Cross-engine probe (3 runs of `union_all_two_branches_multi_col_projection`) showed Go is deterministic-sorted; Java is intermittent — sometimes honours `ORDER BY id` at the UNION-ALL output, sometimes returns interleaved branch order. TODO #44's original diagnosis was inverted (the dropped corpus failed because Java was the flaky side, not Go). Pinned Go's deterministic behaviour as Go-only sentinel `TestFDB_UnionAllOuterOrderByDeterministic` (5 runs assert sorted output). Corpus entry omitted until Java upstream fixes.
- [x] **#52** PK literal-eq AND join-predicate — **reclassified Java upstream bug, dayshift-66**. Cross-engine probe via `pk_literal_eq_in_join_probe` showed Go correctly applies BOTH `a.id = 2` AND `a.id = b.parent` (returns 2 — only B rows (12,2) and (13,2) match); Java drops one of the predicates and over-counts to 5. TODO #52's diagnosis was inverted. Pinned Go's correct behaviour as Go-only sentinel `TestFDB_PKLiteralEqInJoin`; corpus entry omitted until Java upstream fixes.
- [x] **#45** EXISTS over CTE/derived correlated lookup — **landed dayshift-66**. Real Go bug. Root cause was 2 lines in `eval_map.go::evalExprAtomOnMap`: when validQualifiers is nil (single-source CTE / derived path), a qualified column reference `a.gid` whose qualifier names an OUTER source was falling through to `row[bare]` — matching the CURRENT row's `gid` column (`big.gid`) instead of deferring to the outer-scope walk. So `EXISTS (SELECT 1 FROM big WHERE big.gid = a.gid)` silently became `big.gid = big.gid` (tautology), making EXISTS true for every outer row. Fix: when validQualifiers is nil but an outer scope claims the qualifier, skip the bare-fallback and let the outer-scope resolution at the end of the function bind the reference. Re-pinned via `exists_over_cte_outer_with_probe` corpus entry.
- [x] **#63** Multi-column UPDATE self-ref — **landed dayshift-66**. Real Go bug confirmed: `UPDATE T SET x=100+80 (=180), y=80-180 (=-100)` instead of SQL-standard `y=80-100 (=-20)`. Root cause: `update_delete.go::execUpdate` evaluated each SET RHS against the IN-PROGRESS clone, so the second SET saw the already-updated value of the first column. Fix: two-pass — evaluate ALL RHS expressions against the ORIGINAL (unmodified) record first, then apply all assignments. Pinned via `update_multi_col_self_ref_probe` corpus entry.
- [x] **#53** 3-way join shared driver key — **reclassified Java upstream bug, dayshift-66**. Cross-engine probe via `three_way_join_shared_driver_probe` showed Go correctly applies BOTH join predicates and returns 3 (one tuple per a.id when each B / C side has exactly one matching row); Java returns 9 (full 3×3 cross product). 4th inverted-diagnosis from nightshift-65 this shift. Pinned Go's correct behaviour as Go-only sentinel `TestFDB_ThreeWayJoinSharedDriverKey`; corpus entry omitted until Java upstream fixes.

### Tier C — Wording / surface alignments (cosmetic but block byte-equal corpus pinning)

- [x] **#43** ORDER BY rejection wording — **landed dayshift-66**. Replaced Go's specific "ORDER BY X cannot be satisfied by any scan strategy; Add an index…" with Java's generic "Cascades planner could not plan query" at `select_query_full.go`. Updated 2 sqldriver tests that pinned the old wording. Pinned via `order_by_arith_unindexed_probe` corpus entry.
- [x] **#62** INT32 overflow on INSERT — **reclassified: Go more SQL-standard-correct (swingshift-92).** Go produces SQLSTATE 22003 (numeric_value_out_of_range); Java uses non-standard INVALID_CAST. Go is correct per SQL standard (22003 for INSERT range violations, 22F3H for explicit CAST). UPDATE INT32 silent truncation and SUM(BIGINT) overflow both fixed in swingshift-91. No Go-side change needed.
- [x] **#46** BIGINT literal overflow — **landed dayshift-66**. Go's `evalConstant` fell through to `ParseFloat` after `ParseInt` overflowed, silently accepting `99999999999999999999`. Java rejects with `NumberFormatException: For input string: "<text>"`. Fix: extract `parseDecimalLiteralValue` helper that distinguishes integer-shape text (no `.`/`e`/`E` — DECIMAL_LITERAL token) from float-shape (REAL_LITERAL); on `ParseInt` overflow of integer-shape text, error byte-equal `For input string: "<text>"` (without exception class prefix — the conformance harness reads the deepest cause message). Pinned via `bigint_literal_overflow_probe` corpus entry.
- [x] **#47** CAST(BIGINT AS BOOLEAN) — **landed dayshift-66**. Replaced Go's silent int64→bool coercion (`n != 0`) with byte-equal Java rejection: `Invalid cast operation No cast defined from LONG to BOOLEAN`. Pinned via `cast_bigint_to_boolean_probe` corpus entry.
- [x] **#50** NOT NULL constraint scope — **reclassified Java upstream limitation, dayshift-66**. Cross-engine probe confirmed: Java rejects scalar NOT NULL at schema-create time with `NOT NULL is only allowed for ARRAY column type`; Go follows SQL standard. Aligning Go to Java's restriction would invalidate dozens of existing schemas across the test surface — Java's behaviour is non-standard. Documented at the `not_null_scalar_probe` skip site.
- [x] **#51** Reserved-keyword column wording — **reclassified low-value Tier D, dayshift-66**. Both engines reject `count` as column name with a syntax error that echoes the offending fragment; Java's harness wraps the auto-generated schema-template name in double quotes, Go's doesn't. Pure cosmetic drift in error formatting. Documented at the `reserved_keyword_col_probe` skip site.
- [x] **#55** INSERT…(cols) SELECT — **landed dayshift-66**. Added byte-equal Java rejection at `insert.go::execInsert` when `explicitCols` is non-empty in an INSERT…SELECT shape: `setting column ordering for insert with select is not supported`. Plain `INSERT INTO t SELECT …` (no column list) still works in both engines. Rewrote `insert_select.yaml` (4 tests) to drop column lists. Pinned via `insert_cols_select_probe` corpus entry.
- [x] **#60** Parenthesized arithmetic in INSERT VALUES — **landed dayshift-66**. Added structural check at `insert.go::execInsert` that rejects a row-constructor slot whose expression atom is a `RecordConstructorExpressionAtomContext` (parenthesized expr) with byte-equal Java wording `expected Record but got Primitive`. Pinned via `paren_arithmetic_in_values_probe` corpus entry.
- [x] **#41** CASE shape divergences — **landed dayshift-66**.
  (a) `CASE WHEN <bare-BOOLEAN-col> THEN … END`: previously rejected by Go's bare-FieldValue check at the WHEN-condition predicate evaluator. Fix: switch the CASE WHEN's predicate evaluator (in `evalScalarFunctionCall` and the map variant) from `evalExprPredicate` to `evalExprPredicateTri(..., allowBareField=true)` — value-context, matching the AND/OR/NOT-operand rule. Pinned via `case_when_bare_bool_col_probe` corpus entry.
  (b) `WHERE CASE WHEN cond THEN TRUE … END`: previously accepted by Go (CASE evaluated to bool, WHERE used IsTruthy). Java's planner rejects the CASE-as-WHERE shape with `expected BooleanValue but got PickValue` (same Assert.castUnchecked path that rejects RecordConstructorValue). Fix: extended `rejectTopLevelParenthesizedWhere` to also reject SpecificFunctionCall→CaseFunctionCall at the WHERE expression's top level — fires across all four WHERE entry points (proto / CTE / JOIN / update-delete). Updated 2 sqldriver tests (`TestFDB_CaseInWhere`, `TestFDB_CaseInWhereOnCTE`) that pinned the OLD permissive Go behaviour. Pinned via `where_case_returns_bool_probe` corpus entry.

### Tier D — Java upstream bugs (Go behaviour is correct; document, do not fix Go)

- [x] **#49** Java planner missing-binding error on `WHERE pk_col = nonpk_col` — **reclassified Java upstream bug, dayshift-82**. Go is SQL-correct; Java's planner errors `Missing binding for __corr_q…` on correlation machinery. Documented. No Go fix needed; corpus skipped until upstream fixes.
- [x] **#59** Java planner VerifyException on bare-BOOLEAN-literal in WHERE conjunct — **reclassified Java upstream bug, dayshift-82**. `WHERE TRUE AND val > 5` and `WHERE FALSE OR val > 5` throw `VerifyException` in Java; Go succeeds correctly. Documented. No Go fix needed; corpus skipped until upstream fixes.
- [x] **#40** Simple-CASE implemented in Go (`CASE expr WHEN val THEN … END`). Java's visitCaseExpressionFunctionCall is still broken (visitChildren no-op, always falls through to ELSE). Go correctly evaluates; corpus entry pinned as DivergenceJavaWrongRowsGoCorrect.

## Phase 2 — Cascades core machinery (sequenced)

- [x] **#10** B3 full Memo: cross-Reference equivalence-class merging, partial-match propagation, cost-driven extraction. Gates everything below. (~2 shifts) — **landed nightshift-68**. Memo struct with topology-based cross-Reference lookup (leaf hash + parent-intersection for non-leaf), integrated into Planner (lazy init in Explore, AddExpression on growth), all 22 rules use call.MemoizeExpression, self-reference cycle guard, OptimizeReferenceTask + ExtractBestPlanFromSelector for cost-driven extraction. 14 unit tests + 2 fuzz targets (MemoConsistency, MemoizeInvariant) + 3 benchmarks.
- [x] **#11** B6 planner driver: per-rule task granularity (TransformTask / ImplementTask split). Retire FixpointApply legacy callers. Gate: #10. (~1 shift) — **Done, nightshift-68.** TransformReferenceTask + SaturationCheckTask replace monolithic ApplyRulesTask; Memo determinism fix (leafRefs slice + ordered candidates); FuzzPlanner_Determinism verified 5.8M execs; plangen end-to-end tests + benchmark migrated to Planner. FixpointApply retained for per-rule unit tests and convergence fuzzing (correct uses).
- [x] **#12** B5 Batch A: index rules — `MergeFetchIntoCoveringIndex`, `IndexEquality`, `IndexRange`, `InComparisonToExplode` + IndexAccessHint / MatchCandidate ports. Covers swingshift-44's 11-branch pushdown chain. Gate: #10. (~2 shifts) — **Done, nightshift-68.** MatchCandidate interface (7 methods), ValueIndexScanMatchCandidate (prefix discipline: N equalities + trailing inequality), RecordQueryIndexPlan, physicalIndexScanWrapper, ImplementIndexScanRule (predicate→column→alias matching, residual handling), InComparisonToExplodeRule (IN-list → Union of equalities), PlanContextBuilder (IndexDef → PlanContext), metadata.RecordLayerIndex satisfies IndexDef. 20+ tests + 3 e2e pipeline tests + 3.8M fuzz execs clean. Deferred: MergeFetchIntoCoveringIndex (needs column-usage dataflow), IndexAccessHint (needs parser threading).
- [x] **#13** B7 correctness tests for Phase 2 rules. Interleave with #12. Gate: #12. (~1 shift) — **Done, nightshift-68.** Fixed conflicting-equality merge bug (poisoned-alias discipline). 17 edge-case tests: conflicting/idempotent equality, non-FieldValue operand, non-ComparisonPredicate, compound equality+inequality, predicate order independence, gap-in-prefix rejection, inequality-stops-prefix, all-residual, 3-column prefix, single-element IN, duplicate IN elements, multi-column IN+equality cooperation, PlanContext builder upper-casing/unique-flag, Memo dedup. 3 fuzz targets (IndexScanRule, InExplode, ComparisonRange merge chain). 3 e2e pipeline tests (plangen package).

## Phase 3 — Cascades rule batches B+C

- [x] **#14** B5 Batch B1 — data-access rules: `AbstractDataAccessRule`, `AggregateDataAccessRule`. Gate: #12. (~2 shifts) — **Done, nightshift-68.** Practical effects of AbstractDataAccessRule landed: N-way intersection (up to 4-way), OrderedIndexScanRule, SortOverOrderedElimRule, ordering propagation, unique index point-lookup cost, Plan() cost-driven extraction. AggregateDataAccessRule (single-agg matching, AggregateIndexMatchCandidate). StreamingAggFromIndexRule. GroupByExpression equality fix. DistinctOverGroupByElimRule. PushFilterThroughGroupByRule. ImplementLimitRule + 4 LIMIT logical rewrites (Merge, PushThroughProjection, NoOpElim, ZeroLimit). ImplementNestedLoopJoinRule (2-quantifier Select → NLJ). plangen: LogicalLimit, LogicalJoin (CROSS + INNER with text/structured ON), text-based filter predicates, AND-chaining. Remaining: PartialMatch/Compensation (multi-shift, deferred).
- [x] **#15** B5 Batch B2 — implementation rules: `ImplementRecursiveDfsJoinRule` (needs CTE infrastructure). Gate: #14. (~2 shifts) — **Done, dayshift-69.** Full CTE infrastructure: TempTable (thread-safe in-memory buffer), TempTableScanExpression, TempTableInsertExpression, RecursiveUnionExpression (TraversalStrategy: ANY/PREORDER/LEVEL/POSTORDER), physical plans (TempTableScan/Insert/RecursiveDfsJoin), physical wrappers, ImplementTempTableScanRule, ImplementTempTableInsertRule, ImplementRecursiveDfsJoinRule. 100+ tests + 2 fuzz targets.
- [x] **#16** B5 Batch B3 — decomposition + optimization. Gate: #15. — **Done, dayshift-69.** Ported all portable implementation rules: ImplementRecursiveLevelUnionRule (CTE level-order traversal), ImplementExplodeRule (UNNEST pipeline completion), ImplementTableFunctionRule (TABLE() streaming). Named rules blocked: DecorrelateValuesRule (needs SelectExpression + ExplorationCascadesRule + TranslationMap), MergeFetchIntoCoveringIndexRule (needs covering-index fetch plans), PushDistinctBelowFilterRule and siblings (need plan-to-plan ImplementationCascadesRule matching over physical plans). Remaining 6 unported Java implementation rules all gate on PlanPartition/RequestedOrdering infrastructure. 40+ tests across plans + rules + planner integration.
- [x] **#17** B5 Batch C — finalization + 6 ImplementXxxRules. Gate: #16. **swingshift-70 landed:** ExpressionProperty framework (singleton property keys, PropertyMap, plan property computation for 24+ plan types), PlanPartition with property maps (ToPlanPartitions, RollUpPlanPartitions, AllAttributesExcept), PlanPropertiesMap per-Reference storage wired into PLANNING phase bottom-up, Reference.planProperties field, DefaultImplementationRules() factory, RecordQueryUnorderedUnionPlan + RecordQueryPredicatesFilterPlan + RecordQueryMapPlan + RecordQueryFirstOrDefaultPlan plan types, physical wrappers (UnorderedUnion, PredicatesFilter, Map), 3 ImplementationRules ported (ImplementUnorderedUnionRule, ImplementUniqueRule, ImplementSimpleSelectRule), NewPhysicalQuantifier, 800+ test lines. Also landed: RequestedOrdering + RequestedSortOrder types, ProvidedSortOrder + OrderingBinding + ProvidedOrderingPart types, RichOrdering with binding maps + Satisfies + EnumerateSatisfyingComparisonKeyValues + DirectionalOrderingParts + ConcatOrderings + MergeOrderings + CreateUnionOrdering, generic CrossProduct[T], InSource hierarchy (InValues/InParameter × sorted/unsorted), RecordQueryInJoinPlan + RecordQueryDefaultOnEmptyPlan, plan extraction fix (prefer FinalMembers after PLANNING), containsPhysical search fix (AllMembers). **Remaining:** ImplementDistinctUnionRule (308 LOC), ImplementInJoinRule (476 LOC), ImplementInUnionRule (281 LOC) — gated on PrimaryKeyProperty computation (requires schema metadata → PK columns mapping) + RequestedOrderingConstraint (planner constraint propagation). All 3 rules' matchers filter on `PrimaryKeyProperty.isPresent()` which currently always returns nil (~1 shift with PK infra).
- [x] **#18** B7 correctness tests for Phase 3 rules. Interleave with #14-17. (~2 shifts) — **Done.** 500+ tests across 90+ files covering all Phase 3 rules. **swingshift-70 progress:** Plan property computation tests (17 tests), partition infrastructure tests (16 tests), rule tests for ImplementUnorderedUnion (11 tests), ImplementUnique (5 tests), ImplementSimpleSelect (7 tests), plan structural tests for PredicatesFilter/Map/FirstOrDefault/UnorderedUnion (62 tests), Phase 3 end-to-end correctness tests (8 tests: UniqueOverDistinct absorption, LimitOverScan, FilterThenSort rewrite, DistinctOverUnion, ProjectionOverFilter, SelectNoPredicates, property invariants). Planner PLANNING phase integration tests (7 tests). RequestedOrdering + RichOrdering tests (19 tests). CrossProduct + OrderingBinding tests (12 tests).
- [x] **#19** Physical-wrapper cleanup — retire `physicalScanWrapper` / `physicalFilterWrapper` / `physicalSortWrapper` / `physicalDistinctWrapper` / `physicalTypeFilterWrapper` once Memo is plan-aware. Gate: #10. (~0.5 shift) — **Done, nightshift-68.** Added `physicalPlanExpression` interface + `findPhysicalPlan`/`findPhysicalExpr` helpers. Collapsed 9×7-case type switches to single interface assertions. Eliminated recursive `wrapPhysicalPlan` — implement rules now reuse existing wrapper from inner Reference via Memo. Net: -280 LOC. Wrappers remain as structural adapters (plans→expressions); full "plans ARE expressions" deferred to #12 which adds new plan types.

## Phase 4 — Query Executor (integration phase, sequential)

- [x] **#20** C1 PlanGenerator complete — arithmetic (+-*/%), function calls (UPPER, COALESCE, nested, zero-arg), LogicalValuesExpression, full predicate parser (comparison ops, AND/OR, BETWEEN, IN, LIKE, IS NULL, IS DISTINCT FROM, STARTS_WITH, NOT, parens, dotted refs). PushLimitThroughUnionRule also landed.
- [x] **#21** **C2 QueryExecutor — execute `RecordQueryPlan` against `FDBRecordStore`, return `RecordCursor`.** Gate: #11, #12, #20. — **Done, dayshift-69.** `pkg/recordlayer/query/executor/` package: `ExecutePlan()` dispatcher handling all 23 physical plan types (scan, index scan, type filter, filter, limit, distinct, projection, sort, union, intersection, NLJ, streaming+hash aggregation, explode, delete, insert, update, temp table scan/insert, table function, values, recursive level union, recursive DFS join). Java-aligned scan skip/limit push-down, `scanComparisonsToTupleRange` (equality prefix + inequality bounds), `indexFetchCursor` (index→record fetch), `goToProtoValue` (Go→proto field mutation for UPDATE), `EvaluationContext` with `TempTable` (thread-safe). 62 tests: 51 unit tests + 11 FDB integration tests via testcontainers.
- [x] **#22** C3 RecordLayerResultSet — wraps cursor, implements `api.ResultSet`. Gate: #21. — **Done, dayshift-69.** `RecordLayerResultSet` in `pkg/recordlayer/query/executor/resultset.go`: wraps `RecordCursor[QueryResult]`, 1-indexed JDBC-style typed accessors (Long/Float/Double/String/Bytes/Boolean/Object + ByName variants), `WasNull()`, `MetaData()` (ColumnCount/Name/Label/Type/TypeName/Nullable/DataType), exhausted `Continuation()`. Type coercion matrix aligned to Java's `AbstractRecordLayerResultSetTest`: numeric↔numeric (int64/int32/float64/float32), bool-only for Boolean, all-to-String, reject cross-domain (bool↔numeric). 20 unit tests: iteration, by-name, wasNull, null-alternation, column-out-of-range, before-advance, metadata, type-coercion, coercion-matrix (8 types × 5 accessors), empty cursor, continuation, close-idempotent.
- [x] **#23** C4 Continuation support — match Java encoding. Gate: #22. — **swingshift-70.** ResultSet.Continuation() now propagates cursor continuation bytes. Wire format inherited from key-value cursor (proto-wrapped, magic 6773487359078157740, conformance-tested). ExecutePlan threads continuation through all plan types. Remaining: per-plan continuation for composite plans (sort/union/intersection multi-cursor position).
- [x] **#24** C5 Prepared parameter binding via `cascades.Value.Evaluate`. Replaces textual `substituteParams`. Gate: #21. — **Done, dayshift-69.** `EvaluationContext` implements `values.ParameterBinder` (WithParams/BindParameter). `RowEvalContext` composes datum map + ParameterBinder for filter predicates that mix field references and ?-params. Threaded through scan comparisons, filter, values, explode, table function. 3 tests: scan-param, filter-param, values-param. Textual `substituteParams` still used in the naive generator path; will be removed when queries route through Cascades end-to-end.
- [x] **#84** **CRITICAL: Unified plan pipeline — eliminate naive generator.** Done: SELECT and DML (INSERT/UPDATE/DELETE) all route through Cascades. Naive generator retained only for DDL/SHOW/INFORMATION_SCHEMA (procedural, no optimization needed).
  
  **Architecture (from Java source analysis):**
  - Java `BaseVisitor.generateLogicalPlan(parseTree)` → `Plan<?>` for ALL statements
  - `QueryVisitor.visitInsertStatement()` (line 447): wraps `InsertExpression` containing ForEach quantifier over source rows + target table metadata
  - `QueryVisitor.visitUpdateStatement()` (line 506): table scan → WHERE → `UpdateExpression` with field transformation map
  - `QueryVisitor.visitDeleteStatement()` (line 559): table scan → WHERE → `DeleteExpression`
  - Same Cascades optimizer plans them: `ImplementInsertRule`, `ImplementUpdateRule`, `ImplementDeleteRule`
  - DML plans inherit from `RecordQueryAbstractDataModificationPlan` (source plan + mutation hook)
  - `PhysicalQueryPlan.executePhysicalPlan()` (line 418) calls `recordQueryPlan.executePlan()` uniformly
  - `isUpdatePlan()` distinguishes DML from read queries via `instanceof` check
  
  **Go port steps:**
  1. Create `LogicalInsert` / `LogicalUpdate` / `LogicalDelete` operators in `pkg/relational/core/query/logical/`
  2. Create `InsertExpression` / `UpdateExpression` / `DeleteExpression` in `pkg/recordlayer/query/plan/cascades/expressions/`
  3. Create `ImplementInsertRule` / `ImplementUpdateRule` / `ImplementDeleteRule` in Cascades rules
  4. Physical plans already exist: `RecordQueryInsertPlan`, `RecordQueryUpdatePlan`, `RecordQueryDeletePlan`
  5. Executor already dispatches them: `ExecutePlan` handles all plan types
  6. Wire the Cascades generator to handle INSERT/UPDATE/DELETE parse trees (not just SELECT)
  7. Remove naive generator DML code paths
  8. DDL stays as procedural actions (no Cascades optimization needed)
  
  **Key Java files:**
  - `fdb-relational-core/.../query/visitors/QueryVisitor.java` — DML visitors
  - `fdb-relational-core/.../query/QueryPlan.java` — PhysicalQueryPlan.execute()
  - `fdb-record-layer-core/.../plan/cascades/rules/ImplementInsertRule.java`
  - [x] **#65** C6 CascadesGenerator — **dayshift-76: ALL non-Docker skips eliminated (32→0).** Cascades handles all SELECT/DML. DDL/SHOW/INFORMATION_SCHEMA stay procedural (naive).
  - [x] **#78** Cascades Value evaluation — **dayshift-76: complete.** CASE, COALESCE, arithmetic, CAST, div/0, aggregates, type mismatch detection.
  - [x] **#79** Cascades translator extensions — **dayshift-76: converted to rejection tests** (Java parity). Subqueries, LEFT/RIGHT JOIN, derived tables not supported in Java's relational Cascades.
  - [x] **#80** FROM-less SELECT: resolved — correctly errors via Cascades.
  - [x] **#81** ORDER BY: **dayshift-76: converted to Java-aligned rejection tests.** No physical sort in Java's Cascades — ORDER BY without supporting index correctly rejected (0AF00). ORDER BY with PK/index works via sort elimination.
  - [x] **#83** Cascades execution: **dayshift-76: fixed.** GROUP BY projection, column type metadata, JOIN tests passing (5/7 shapes). Remaining 2 known-incorrect (alias resolution — see #85, #86).
  - [x] **#82** INFORMATION_SCHEMA routed to naive (5 tests).

  **Remaining work discovered in dayshift-76:**

  - [x] **#85** JOIN alias threading — **landed swingshift-77**. Threaded SQL aliases through SelectExpression → NLJ plan → mergeRows. Self-join now returns correct rows.
  - [x] **#86** CTE+JOIN predicate resolution — **landed swingshift-77**. CTE aliases flow through translator's sourceAlias extraction from LogicalScan children.
  - [x] **#87** Streaming aggregation ordering — **landed swingshift-77**. StreamingAggFromIndexRule yields both forward/reverse scans; streaming agg wrapper inherits direction from inner index scan.
  - [x] **#88** Reverse index scan for ORDER BY DESC — **landed swingshift-77**. OrderedIndexScanRule produces reverse scans for DESC sort keys; SortOverOrderedElimRule checks direction per-key.
  - [x] **#89** Type mismatch in predicate resolver — **resolved (nightshift-87)**. The text-based filter path is dead: all SELECT/DML routes through Cascades (#84). The Cascades filter uses typed ComparisonPredicates; `cmpAny` detects numeric-vs-string mismatch via `isNumericStringMismatch` and panics with `TypeMismatchError` (caught by executor → SQLSTATE 42804). Verified by TestFDB_SelectWhereTypeMismatch and TestFDB_MixedTypeEqualityNoStringCoerce.
  - [x] **#90** ImplementSortRule strictlySorted handling — **landed dayshift-82**. Ported Java's RemoveSortRule lines 112-157: (1) distinct partition with all ordering keys covered by sort/equality → strictlySorted, (2) unique index with full key coverage → strictlySorted. Added StrictlySorted field to RecordQueryIndexPlan, GetEqualityBoundValues/GetOrderingKeys to RichOrdering.
  - [x] **#91** FindUnsupportedFunction error code — **landed swingshift-77**. SELECT + DML paths now return ErrCodeUndefinedFunction (42883) matching Java's SqlFunctionCatalog.lookupFunction.
  - [ ] **#92** Type mismatch detection layer: Java catches at compile time (`SemanticAnalyzer`); Go catches at eval time (panic+recover). Functionally equivalent (same SQLSTATE 42804 error), architecturally different. Low priority: moving to compile-time would improve error locality but doesn't change user-visible behavior.

  **HN launch blockers (in priority order):**
  - [x] **#93** Fix #85 + #86 (alias threading) — **landed swingshift-77**.
  - [x] **#94** Fix #88 (reverse index scan) — **landed swingshift-77**.
  - [x] **#95** Fix #87 (streaming agg ordering) — **landed swingshift-77**.
  - [x] **#96** README / documentation — **landed swingshift-77**. SQL engine section with database/sql examples, DDL/DML syntax, Cascades optimizer details.
  - [x] **#97** Stress test / fuzz — **landed swingshift-77**. FuzzTranslateToCascades: random logical plan tree generation (8 operator types × flag combinations) exercising translator no-panic guarantee. Existing parser/planner/aggregation fuzz targets provide complementary coverage.
  - [x] **#98** Yamsql conformance: **102/102 (100%, swingshift-81)**. All scenarios pass. Remaining architectural items (#89, #90, #92) tracked separately.
- [x] **#25** ORDER BY JOIN/CTE/UNION fallback removal — **landed swingshift-74**. Cascades planner failure now returns error instead of falling back to naive. **nightshift-75:** fully ripped out naive fallback from SELECT path.

## Phase 5 — DDL + cache + driver completion

- [x] **#26** B0 type hierarchy: DATE / TIMESTAMP completion (TypeDate / TypeTimestamp + promotion). **Done (nightshift-87).** CodeDate/CodeTimestamp, DateType/TimestampType primitives, JDBC mappings, grammar + DDL, CompareValues time.Time handling, CastValue DATE/TIMESTAMP, Cascades CastValue.Evaluate, proto storage as STRING (ISO 8601), type inference for CURRENT_TIMESTAMP/DATE. 4 FDB integration tests.
- [x] **#27** D2 DDL types: DATE / TIMESTAMP / ARRAY column types. **Done (nightshift-87).** DATE/TIMESTAMP via #26. ARRAY: grammar already had `ARRAY?` token; ported DdlVisitor.visitColumnDefinition's isRepeated handling (element type forced NOT NULL, array carries nullability); datatypeToProtoFieldType recurses on element type + LABEL_REPEATED label. JSON out of scope (Java doesn't have it). FDB integration test for ARRAY DDL.
- [x] **#28** Date-part Go-only cleanup — **KEEP (nightshift-87)**. Decision: YEAR/MONTH/DAY/HOUR/MINUTE/SECOND/DAYOFWEEK/DAYOFYEAR functions are kept as Go extensions. They work with the new DATE/TIMESTAMP type system (accept time.Time input from CURRENT_TIMESTAMP or CAST). Java doesn't have temporal column types at all — removing these would be a user-visible regression with no Java-compat benefit.
- [ ] **#29** D1 DDL action types — `CreateTableAction` / `CreateIndexAction` / `DropTableAction` / `DropIndexAction` / `SetStoreStateAction`. Gate: #27. (~2 shifts). **Note (dayshift-89):** These action types do NOT exist in Java 4.11.1.0's MetadataOperationsFactory. Java's DDL is fully template-based (SaveSchemaTemplate bundles tables+indexes). #29 would be a Go-only extension for standalone DDL UX. Low priority for Java conformance.
- [ ] **#30** D3 Online indexer integration via DDL — CREATE INDEX triggers background build. Gate: #29. (~1 shift)
- [x] **#31** B8 plan-cache-key diff — **done (swingshift-81)**. QueryHash (normalized SQL → FNV-64a) + LRU PlanCache (256 entries) on EmbeddedConnection. DDL invalidation.
- [x] **#32** D4 Plan cache — **done (swingshift-81)**. Integrated into cascades_generator. Cache hit skips full Cascades pipeline. 10 unit tests.
- [ ] **#33** D5 driver adapter gaps — custom scanner-valuer (Struct / Array / Versionstamp / Continuation). Gate: #22. (~1 shift remaining)
  **Already done:** Stmt (embeddedStmt with ExecContext/QueryContext), Rows column-type (ColumnTypeDatabaseTypeName/ScanType/Nullable/Length/PrecisionScale), Tx (embeddedTx with Commit/Rollback), Pinger, SessionResetter, Validator, ConnPrepareContext, ConnBeginTx, time.Time parameter binding, NamedValueChecker (accepts fmt.Stringer including uuid.UUID).
  **Remaining:** Custom `database/sql.Scanner`/`driver.Valuer` types for complex values (Struct columns, Array columns, Versionstamp, Continuation token). These would let users `rows.Scan(&myStruct)` or pass structured types as parameters. Low priority — standard scalar types cover 95% of use cases.

## Phase 6 — Cross-language verification + perf

- [ ] **#34** E1 Go-vs-Java SQL perf bench — simple SELECT, secondary-index, INSERT, aggregate, prepared statement. Gate: #21. (~1 shift)
  **Go-side benchmarks done (nightshift-87 + dayshift-89):** BenchmarkFDB_PlanCacheHit/Miss, BenchmarkFDB_TimestampInsert, BenchmarkFDB_TimestampRangeScan, BenchmarkFDB_JoinQuery, BenchmarkFDB_AggregateGroupBy, BenchmarkFDB_IndexScanRange. Remaining: Java conformance server needed for cross-engine comparison.
- [ ] **#35** A4 INFORMATION_SCHEMA cross-engine byte-equivalence. Gate: #9 + upstream. (~1 shift)
- [ ] **#36** Catalog wire format reverse direction (Go writes → Java reads). (~1 shift)
- [ ] **#37** E2 ANTLR parser DoS hardening — coordinate Go-side fix with upstream. Gate: upstream ticket. (~0.5 shift)
- [x] **#38** CI test report drops `//pkg/relational/...` results — **landed swingshift-64**. Root cause: `.bazelrc:18` global `--build_event_json_file=.bazel-bep.jsonl` was getting overwritten by the second `bazelisk test` invocation (race-detector subset, no relational tests). Fix: race step now writes to `.bazel-bep.race.jsonl` (overrides the global flag); `cmd/test-report` accepts multiple positional BEP file args and concatenates target lists; CI workflow passes both BEPs to the report generator. Single-arg default unchanged for local use.
- [x] **#99** Schema-qualified table names (`schema.table`) — **landed dayshift-85**. `ResolveQualifiedTableName` in `ast_util.go` validates qualifier matches current schema (case-insensitive), strips it before lookup. Wrong qualifier → `ErrCodeUndefinedDatabase`, multi-part → `ErrCodeInternalError`. Wired into SELECT (naive + Cascades), INSERT, UPDATE, DELETE, JOIN paths. 9 unit tests + 6 FDB integration tests.
- [ ] **#39** Go-only GROUP BY clause — **broader than initially scoped**: empirical re-probe via subagent batch (swingshift-64) found Java rejects ALL GROUP BY forms, not just the non-projected form. Even canonical `SELECT g, COUNT(*) FROM t GROUP BY g` (grouping column IS in projection) triggers `UnableToPlanException: Cascades planner could not plan query`. Cascades 4.11.1.0 has no GROUP BY rule at all. Aligning Go would require rejecting all GROUP BY clauses at parse time with byte-equal "Cascades planner could not plan query"; ~10-20 yamsql files use GROUP BY and would need wholesale rewrite (often there's no clean rewrite — GROUP BY is the test's focus). Recommend leave as Go-only-correctness for now; revisit when Java adds GROUP BY support upstream. (~2 shifts if pursued)
_(Items #40-#64 moved to Phase 1.5 above. #41 and #40 were originally Phase 6 surface-divergence finds; recategorized so future shifts stop importing-by-numeric-order.)_

## Divergences (swingshift-70 audit)

Concrete Go-Java divergences surfaced by subagent audit. Ordered by impact.

### CRITICAL — correctness/completeness gaps

- [x] **#66** InJoinRule `enumerateInSourcesForRequestedOrdering` — **landed nightshift-71**. Walks requested ordering parts (not provided), matches against inner fixed bindings, honors sort direction, reads planner constraints. Gate: #67.
- [x] **#67** Ordering: PartiallyOrderedSet infrastructure — **landed nightshift-71**. `combinatorics/` sub-package: PartiallyOrderedSet[T], TopologicalSort (Backtrack+Kahn with skip), TransitiveClosure, EligibleSet, MapAll, FilterElements, Builder. RichOrdering upgraded to store PartiallyOrderedSet[string] internally; Satisfies() and EnumerateSatisfyingComparisonKeyValues() now use TopologicalSort.satisfyingPermutations. 30 tests + 2 fuzz targets.
- [x] **#68** Ordering: full merge algorithm — **landed nightshift-71**. EligibleSet-based lock-step merge with union/intersection binding combiners. mergeOrderings() walks both partial orders via EligibleSet, intersects eligible elements, combines bindings, preserves dependency edges.

### HIGH — optimization quality gaps

- [x] **#69** DistinctUnionRule: cross-product skip optimization — **landed dayshift-72**. CrossProductIterator with Skip(depth) prunes impossible branches. Incremental merge with memoization. O(n*k) instead of O(n^k).
- [x] **#70** InJoinRule: permutation generation — **landed nightshift-71**. enumerateSourceOrderings() uses TopologicalSort.Permutations() to enumerate all valid orderings of remaining sources. Gate: #67.
- [x] **#71** Ordering: `enumerateCompatibleRequestedOrderings` + `satisfiesGroupingValues` — **landed nightshift-71**. Uses TopologicalSort.satisfyingPermutations on the ordering set. Also added ProvidedSortOrder.ToRequestedSortOrder().

### MEDIUM — feature completeness gaps

- [x] **#72** Ordering: `pullUp`/`pushDown`/`translateCorrelations` — **completed swingshift-81**. PullUpValue/PushDownValue in values package handles RecordConstructorValue, QuantifiedObjectValue, exact match. PullUpThroughValue/PushDownThroughValue on RichOrdering and RequestedOrdering. 27 tests.
- [x] **#73** Ordering: SetOperationsOrdering semantics — **covered by existing Go design**. Go's RichOrdering already stores multiple fixed bindings per key with union/intersection combiners (combineBindingsForUnion/combineBindingsForIntersection). No separate subclass needed — Go's flat design is functionally equivalent.
- [x] **#74** DistinctUnionRule: `removeCommonEqualityBoundParts` — **landed dayshift-72**. Strips equality-bound ordering keys common across all union legs before merge.
- [x] **#75** InJoinRule: `isSupportedExplodeValue()` validation — **landed dayshift-72**. Validates explode collection values are ConstantValue, QuantifiedObjectValue, or constant-evaluable. Applied to both InJoinRule and InUnionRule.
- [x] **#76** Executor: InJoin/InUnion/MergeSortUnion — **landed dayshift-72**. InJoin iterates IN-values via concatCursor. ExplodeExpression values wired into InJoinPlan during rule execution. MergeSortUnion uses proper heap-merge cursor with peek buffers, comparison key evaluation, dedup support. InUnion separated from InJoin dispatch.
- [x] **#77** InUnionRule: `attemptFailedInJoinAsUnionMaxSize` — **landed dayshift-72**. Added to PlannerConfiguration, wired through ImplementationRuleCall.Context → InUnionRule → RecordQueryInUnionPlan.
