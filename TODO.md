# TODOs

Strict execution order. Pick the next unchecked item whose gates are satisfied. No priority debate — phases run sequentially; items inside a phase run in parallel unless gated.

Java Record Layer version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

---

## Phase 1 — Parallel quick wins (no gates, start immediately)

- [x] **#1** Go-only cleanup: `SELECT DISTINCT` plain projection. **Closed obsolete (swingshift-64)**: empirical probe showed fdb-relational 4.11.1.0 accepts plain `SELECT DISTINCT col FROM T` (Cascades has a DISTINCT-projection rule). Java's `UnableToPlanException` only fires for DISTINCT + ORDER BY together — a shape-specific Cascades composition gap, not blanket DISTINCT non-support. Aligning Go would mean shape-detection (bolt-on `if X` per CLAUDE.md principle #10), not a clean removal. Leave Go's DISTINCT pipeline in place; revisit narrow shape alignment if cross-engine divergence surfaces in real corpora.
- [ ] **#2** Go-only cleanup: scalar STRING family (UPPER / LOWER / LENGTH / SUBSTRING / SUBSTR / TRIM / LTRIM / RTRIM / CONCAT / `||` / REPLACE / LEFT / RIGHT / POSITION / REVERSE). ~25 file rewrite. (~1-2 shifts)
- [ ] **#3** Go-only cleanup: scalar ARITHMETIC (ABS / SQRT / POWER) + DATETIME (CURRENT_TIMESTAMP / NOW). ~8 file rewrite. (~1 shift)
- [ ] **#4** Go-only cleanup: `LIMIT N` → `setMaxRows` alignment. Dozens of files but mechanical. (~1 shift)
- [ ] **#5** Go-only cleanup: `col IN (SELECT ...)` → JOIN/EXISTS rewrite. ~14 file rewrite. (~1 shift)
- [ ] **#6** Go-only cleanup: FROM-less SELECT (with CTE-base flag in parser). Parser plumbing + ~5 file rewrite. (~1 shift)
- [ ] **#7** Go-only cleanup: `WHERE (bare-paren-boolean)`. Parser tweak + ~3 file rewrite. (~0.5 shift)
- [ ] **#8** A3 corpus expansion 290 → 1587 yamsql parity. Mechanical, surfaces ~1/3 real bugs, parallel-safe. (~4-6 shifts)
- [ ] **#9** INFORMATION_SCHEMA decision: keep + propose upstream / remove until upstream. Unblocks #35. (~0.5 shift)

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
