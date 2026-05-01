# TODOs

Strict execution order. Pick the next unchecked item whose gates are satisfied. No priority debate — phases run sequentially; items inside a phase run in parallel unless gated.

Java Record Layer version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

---

## Phase 1 — Parallel quick wins (no gates, start immediately)

- [x] **#1** Go-only cleanup: `SELECT DISTINCT` plain projection. **Closed obsolete (swingshift-64)**: empirical probe showed fdb-relational 4.11.1.0 accepts plain `SELECT DISTINCT col FROM T` (Cascades has a DISTINCT-projection rule). Java's `UnableToPlanException` only fires for DISTINCT + ORDER BY together — a shape-specific Cascades composition gap, not blanket DISTINCT non-support. Aligning Go would mean shape-detection (bolt-on `if X` per CLAUDE.md principle #10), not a clean removal. Leave Go's DISTINCT pipeline in place; revisit narrow shape alignment if cross-engine divergence surfaces in real corpora.
- [x] **#2** Go-only cleanup: scalar STRING family (UPPER / LOWER / LENGTH / CHAR_LENGTH / CHARACTER_LENGTH / OCTET_LENGTH / SUBSTRING / SUBSTR / TRIM / LTRIM / RTRIM / CONCAT / CONCAT_WS / REPLACE / LEFT / RIGHT / POSITION / REVERSE) — **landed swingshift-64**. Removed Go-side dispatch in `scalar_functions.go` (proto + map paths now both fall through to the byte-equal `Unsupported operator <NAME>` default arm); dropped STRING / LENGTH / OCTET_LENGTH from `inferScalarFunctionJDBCType`; rewrote 5 yamsql files (string_functions, trim_concat, select_no_from, scalar_subquery_advanced, scalar_subquery_types) and 17 sqldriver tests; pinned cross-engine via 3 plandiff corpus entries (string_upper_rejected, string_upper_in_cte_where_rejected, string_substring_rejected). `||` operator wasn't implemented Go-side; nothing to remove. Net diff: -198 LOC scalar_functions.go alone.
- [x] **#3** Go-only cleanup: scalar ARITHMETIC (ABS / SQRT / POWER / POW / FLOOR / CEIL / CEILING / ROUND / SIGN / PI / EXP / LN / LOG) + DATETIME function-call aliases (NOW / CURDATE / CURTIME / SYSDATE / UTC_TIMESTAMP / UTC_DATE / UTC_TIME) — **landed swingshift-64**. Removed Go-side dispatch in `scalar_functions.go`; both proto + map paths fall through to byte-equal `Unsupported operator <NAME>`. SQL-standard form (CURRENT_TIMESTAMP / CURRENT_DATE / CURRENT_TIME / LOCALTIME, no parens) intentionally NOT touched: Java's `BaseVisitor.visitSimpleFunctionCall` is `visitChildren(ctx)` (broken pass-through, no error) — Go's working impl is a Go-only correctness improvement, not a divergent rejection. Pinned cross-engine via `arith_abs_rejected`, `arith_power_rejected`, `datetime_now_rejected` corpus entries. Out-of-scope (separate cleanup): FLOOR / CEIL / CEILING / ROUND / SIGN / PI / EXP / LN / LOG / MOD function-form / date-part fns (YEAR/MONTH/...) — Java also rejects these but they weren't in the named scope and stay implemented Go-side for now (cf. #28 covers date-parts).
- [x] **#4** Go-only cleanup: `LIMIT N` → `setMaxRows` alignment — **landed swingshift-64**. Rejected `simpleTable.LimitClause()` at parse time in `extractFromSimpleTable` with byte-equal Java messages (`"LIMIT clause is not supported."` / `"OFFSET clause is not supported."`, ErrCodeUnsupportedQuery / 0AF00) — Java's AstNormalizer.visitLimitClause checks offset first so `LIMIT N OFFSET M` errors on OFFSET. Confirmed empirically via cross-engine probe. Pinned via `limit_clause_rejected` + `offset_clause_rejected` corpus entries. Test surface rewritten: 15 yamsql files, 4 sqldriver tests, 3 embedded internal tests; LogicalLimit operator infrastructure left in place for future Cascades / setMaxRows-routing consumption. SQL `LIMIT N` is now unreachable; pagination must go through a future `setMaxRows`-style API (not yet plumbed in Go's database/sql layer).
- [ ] **#5** Go-only cleanup: `col IN (SELECT ...)` → JOIN/EXISTS rewrite. ~14 file rewrite. (~1 shift) — **Deferred swingshift-64 pending upstream**: cross-engine probe shows Java crashes with `NullPointerException: Cannot invoke ExpressionsContext.expression() because expressionsContext is null` (parser visitor missing null-check for subquery-as-IN-source). NPE is a Java bug, not a clean rejection — bolting Go-side rejection to mirror it violates CLAUDE.md design principle #10 ("emergent behaviour over special-case checks"). EXISTS subquery works cleanly in both engines (verified). When Java fixes the NPE upstream and emits a clean `RelationalException`, revisit: align Go message verbatim and rewrite tests. Until then leave Go's IN-subquery impl in place as a Go-only correctness improvement over Java's broken NPE.
- [x] **#6** Go-only cleanup: FROM-less SELECT — **landed swingshift-64**. Java's `QueryVisitor.visitSimpleTable` line 225 has `Assert.notNullUnchecked(simpleTableContext.fromClause(), UNSUPPORTED_QUERY, "query is not supported")` — the gate is universal, fires inside CTE bases too (NOT just standalone). Empirical probe confirmed: `SELECT 1+1` and `WITH base AS (SELECT 1 AS n) SELECT n FROM base` both reject with the identical message. The TODO's premise about a CTE-base bypass was stale; no parser flag needed. Go's `extractFromSimpleTable` rejects when `simpleTable.FromClause()` is nil with byte-equal message + ErrCodeUnsupportedQuery (0AF00). Pinned via `probe_fromless_in_cte_base` corpus entry. Test surface: 3 yamsql files + 4 sqldriver tests + 1 embedded internal test. Also added cross-engine harness improvement: walks Go's *api.Error cause chain to find the deepest message (mirrors Java conformance server's root-cause traversal) so wrapped-by-CTE-context errors compare byte-equal at their inner-most rejection.
- [x] **#7** Go-only cleanup: `WHERE (bare-paren-boolean)` — **landed swingshift-64**. Java's parser treats `(...)` as a recordConstructor (single-element tuple); Expression.toUnderlyingPredicate's `Assert.castUnchecked(..., BooleanValue.class)` fails with byte-equal `"expected BooleanValue but got RecordConstructorValue"`. Go matches at the WHERE entry sites (`rejectTopLevelParenthesizedWhere` helper called from `evalPredicate` proto-path + `join.go`/`cte_scan.go` map-paths) — the check fires on the WHERE expression's TOP-LEVEL only, NOT on every recursive PredicatedExpression: empirical probe showed Java accepts `(a) AND (b)` (the LogicalExpression surface type is BooleanValue even with RecordConstructor leaves) while rejecting bare `(a)`. Pinned via `where_paren_top_level_rejected` corpus entry. Test surface: 1 yamsql file (boolean.yaml).
- [ ] **#8** A3 corpus expansion 290 → 1587 yamsql parity. Mechanical, surfaces ~1/3 real bugs, parallel-safe. (~4-6 shifts)
- [x] **#9** INFORMATION_SCHEMA decision — **KEEP, swingshift-64**. Probed empirically: fdb-relational 4.11.1.0 rejects `INFORMATION_SCHEMA.TABLES` with `RelationalException: Unknown reference INFORMATION_SCHEMA.TABLES`. Catalog isn't registered at all (no quoted form, no alternate path). Decision: keep Go's working Go-only impl (system_tables.go / system_rows.go) — SQL-standard feature, removing it is a user-visible regression — and document the divergence in the plandiff corpus.go comment block. #35 (A4 cross-engine byte-equivalence) stays gated on upstream. Open follow-up: write a feature proposal for fdb-relational upstream.

## Phase 2 — Cascades core machinery (sequenced)

- [ ] **#10** B3 full Memo: cross-Reference equivalence-class merging, partial-match propagation, cost-driven extraction. Gates everything below. (~2 shifts)
- [ ] **#11** B6 planner driver: per-rule task granularity (TransformTask / ImplementTask split). Retire FixpointApply legacy callers. Gate: #10. (~1 shift)
- [ ] **#12** B5 Batch A: index rules — `MergeFetchIntoCoveringIndex`, `IndexEquality`, `IndexRange`, `InComparisonToExplode` + IndexAccessHint / MatchCandidate ports. Covers swingshift-44's 11-branch pushdown chain. Gate: #10. (~2 shifts)
- [ ] **#13** B7 correctness tests for Phase 2 rules. Interleave with #12. Gate: #12. (~1 shift)

## Phase 3 — Cascades rule batches B+C

- [ ] **#14** B5 Batch B1 — data-access rules: `AbstractDataAccessRule`, `AggregateDataAccessRule`. Gate: #12. (~2 shifts)
- [ ] **#15** B5 Batch B2 — implementation rules: `ImplementNestedLoopJoinRule`, `ImplementRecursiveDfsJoinRule`, `ImplementStreamingAggregationRule`. Unblocks JOIN + aggregate + CTE. Gate: #14. (~2 shifts)
- [ ] **#16** B5 Batch B3 — decomposition + optimization: `DecorrelateValuesRule`, `PushPredicateThroughDistinctRule`, `MergeFetchIntoTypeFilterRule` family. Gate: #15. (~2 shifts)
- [ ] **#17** B5 Batch C — finalization: `FinalizeExpressionsRule` + remaining ~30 rules. Gate: #16. (~2 shifts)
- [ ] **#18** B7 correctness tests for Phase 3 rules. Interleave with #14-17. (~2 shifts)
- [ ] **#19** Physical-wrapper cleanup — retire `physicalScanWrapper` / `physicalFilterWrapper` / `physicalSortWrapper` / `physicalDistinctWrapper` / `physicalTypeFilterWrapper` once Memo is plan-aware. Gate: #10. (~0.5 shift)

## Phase 4 — Query Executor (integration phase, sequential)

- [ ] **#20** C1 PlanGenerator complete — full text→Value parser threading (arithmetic, function calls, qualified refs, exponent, escapes), LogicalLimit / LogicalAggregate / LogicalJoin / LogicalValues equivalents. (~1 shift)
- [ ] **#21** **C2 QueryExecutor — execute `RecordQueryPlan` against `FDBRecordStore`, return `RecordCursor`. Eliminates today's ad-hoc executor. SINGLE HIGHEST-LEVERAGE SHIFT.** Gate: #11, #12, #20. (~2 shifts; prototype 1-shift spike first)
- [ ] **#22** C3 RecordLayerResultSet — wraps cursor, implements `api.ResultSet`. Gate: #21. (~1 shift)
- [ ] **#23** C4 Continuation support — match Java encoding. Gate: #22. (~1 shift)
- [ ] **#24** C5 Prepared parameter binding via `cascades.Value.Evaluate`. Replaces textual `substituteParams`. Gate: #21. (~1 shift)
- [ ] **#25** ORDER BY JOIN/CTE/UNION fallback removal — falls out mechanically once C2 routes through Cascades. Gate: #21. (~0.5 shift)

## Phase 5 — DDL + cache + driver completion

- [ ] **#26** B0 type hierarchy: DATE / TIMESTAMP completion (TypeDate / TypeTimestamp + promotion). (~1 shift)
- [ ] **#27** D2 DDL types: DATE / TIMESTAMP / ARRAY / JSON column types. Gate: #26. (~2 shifts)
- [ ] **#28** Date-part Go-only cleanup (deferred from Phase 1) — keep / remove decision now that Java alignment is feasible. Gate: #27. (~0.5 shift)
- [ ] **#29** D1 DDL action types — `CreateTableAction` / `CreateIndexAction` / `DropTableAction` / `DropIndexAction` / `SetStoreStateAction`. Gate: #27. (~2 shifts)
- [ ] **#30** D3 Online indexer integration via DDL — CREATE INDEX triggers background build. Gate: #29. (~1 shift)
- [ ] **#31** B8 plan-cache-key diff — RFC-024 Go-internal cache key. Gate: #11, #21. Gates #32. (~1-2 shifts)
- [ ] **#32** D4 Plan cache (Phase 7) — `RelationalPlanCache` 3-tier + TTL + async eviction. Gate: #31. (~3 shifts)
- [ ] **#33** D5 driver adapter gaps — `Stmt` / `Rows` column-type / `Tx` / custom scanner-valuer (Struct / Array / Versionstamp / Continuation). Gate: #22. (~2 shifts)

## Phase 6 — Cross-language verification + perf

- [ ] **#34** E1 Go-vs-Java SQL perf bench — simple SELECT, secondary-index, INSERT, aggregate, prepared statement. Gate: #21. (~1 shift)
- [ ] **#35** A4 INFORMATION_SCHEMA cross-engine byte-equivalence. Gate: #9 + upstream. (~1 shift)
- [ ] **#36** Catalog wire format reverse direction (Go writes → Java reads). (~1 shift)
- [ ] **#37** E2 ANTLR parser DoS hardening — coordinate Go-side fix with upstream. Gate: upstream ticket. (~0.5 shift)
- [ ] **#38** CI test report drops `//pkg/relational/...` results. `.bazelrc:18` sets a single `--build_event_json_file=.bazel-bep.jsonl`; `.github/workflows/ci.yml` runs two `bazelisk test` invocations and the second (race-detector subset, no relational) overwrites the first BEP. Fix: per-invocation BEP files + teach `cmd/test-report` to merge multiple BEPs (or reorder so full suite runs second). (~0.5 shift)
