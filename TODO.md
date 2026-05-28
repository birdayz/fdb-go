# TODOs

FoundationDB Record Layer — Go Port. Java version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

Current state: 46 test targets, 639+ SQL tests passing, 270 yamsql scenarios, 508 cross-engine specs, 105 fuzz targets, ~65 Cascades rules, 41 plan types (36 executor-wired), 48 value types, 9 predicate types. Unified Cascades task stack (REWRITING + PLANNING). Winner-based plan selection with per-ordering properties.

---

## Known gaps

### vs Java (correctness/feature parity)

- [x] **Correlated filter without index.** Fixed in 56874f23 — ImplementFilterRule sets innerAlias on RecordQueryPredicatesFilterPlan. All correlated paths (scalar subquery, EXISTS, JOIN) work without indexes. 14+ integration tests verify.
- [ ] **No RIGHT/FULL OUTER JOIN.** Only LEFT OUTER is implemented.
- [x] **Correlated scalar subquery shapes widened.** Non-aggregate (ORDER BY + LIMIT), multi-table inner FROM (JOINs), multi-column validation, deep-walk replaceScalarSubqueryRef. GROUP BY/HAVING rejected with clear errors (PredicatePushDownRule AliasMap conflict). CorrelatedExistsError propagation fixed.
- [ ] **No window functions.** No `ROW_NUMBER()`, `RANK()`, `LAG/LEAD`.
- [ ] **GROUP BY/HAVING in correlated scalar subqueries.** Requires PredicatePushDownRule to treat GroupByExpression as a barrier (AliasMap.Compose conflict on correlation alias). Per Graefe: runtime cardinality assertions incompatible with continuation-based pagination — use Java's FirstOrDefault pattern.
- [ ] **PlanVisitor resolver lacks SubqueryPlanner.** Scalar subquery error messages don't propagate through step (2) projection resolution because the resolver has no SubqueryPlanner. workaround: errors propagate through upgradeProjectionValues (step 12) instead.

### Beyond Java (Go-only improvements)

- [ ] **Full Graefe Memo with cross-group merging.** Neither Java nor Go implements the Cascades paper's cross-Reference equivalence class merging. Per-Reference dedup suffices for current workloads (CRUD, simple joins, aggregates). Full Memo would unlock optimal multi-way join ordering (5+ tables) and redundant subexpression elimination. Significant effort — the `B3` follow-on in codebase comments.
- [x] **Correlated scalar subqueries.** Go-only extension — Java rejects at grammar level. Implemented via FlatMap with JoinTypeLeftOuter.

---

## Production readiness (Graefe review, 2026-05-28)

The Cascades architecture is solid — task stack, two-phase REWRITING+PLANNING, 16-criteria cost model, match-candidate infra all well-ported. The production risks are all at the **boundaries**: planner↔executor, executor↔runtime, system↔operator. Priority tiers below.

### P0 — fix before deploying anywhere (correctness/availability)

- [x] **P0.1 NLJ memory bomb.** Fixed in PR #203 — `CollectAllBounded` with configurable materialization limit (default 100K rows) on all 6 `CollectAll` sites. `MaterializationLimitExceededError` typed error. All cursor leaks on error path fixed. 11 regression tests. RFC-028.
- [x] **P0.2 Plan cache serves wrong plans.** Fixed in RFC-029 — cache keys on normalized SQL string directly (was uint64 FNV-64a hash with no text comparison on hit → collision = wrong plan). Scalar subquery staleness was a non-issue: `scalarSubqueryBinding` stores plans not results, re-evaluated per page fetch. `QueryHash` retained for tests only.
- [ ] **P0.3 No context cancellation in executor.** Zero `ctx.Err()`/`ctx.Done()` checks in non-test executor code. `flatMapCursor.OnNext` passes ctx to children but never checks it → a cancelled request spins until the 5s FDB timeout or an incidental child error. Every Go service sets request deadlines via context.
  - **Fix:** check `ctx.Err()` at the top of every cursor `OnNext` loop iteration. Mechanical but critical.

### P1 — fix before relying on the optimizer for real workloads (plan quality)

- [ ] **P1.1 Wire statistics from FDB.** `StatisticsProvider` exists but `DefaultStatistics` returns `1e6` for everything → cost model flies blind. Picks correctly for CRUD by accident (ordinal tie-breaking dominates); picks randomly for competing access paths of similar shape, and *wrong* when a small table has a large index (or vice versa). The count index already exists in Go (maintained on writes).
  - **Fix:** wire `StatisticsProvider` to read `RecordMetaData.getRecordCountIndex()` at plan time. ~1 FDB read per record type, ~1ms planning overhead. **Single highest-leverage change for plan quality.**
- [ ] **P1.2 Complete QOV-based FieldValue migration.** 9 `stripAlias*` calls in the NLJ rule; `mergeRows` (line 1171) does string-based key qualification (`outerQual + "." + COL`). Works today by accident on simple shapes; breaks on 3+ table joins / self-joins / correlated subqueries with ambiguous column names (wrong-prefix strip or no strip). Java uses `Value.rebase(AliasMap)` — structural `CorrelationIdentifier` retargeting, zero string manipulation.
  - **Fix:** every predicate/value carries `FieldValue(QOV(correlationId), "column")`; delete all `stripAlias*`. Highest-leverage *architectural* cleanup — eliminates a whole bug class.

### P2 — fix before scaling operations

- [ ] **P2.1 Plan cache LRU is O(n) per hit.** `PlanCache.promote()` linear-scans the LRU order slice under a write lock on every cache hit. Fine at 256 entries; `PLAN_CACHE_PRIMARY_MAX_ENTRIES=1024` makes it a concurrency contention point.
  - **Fix (20 min):** `container/list` doubly-linked list for O(1) promote.
- [ ] **P2.2 Operational debuggability.** No query logging, no slow-query log, no plan-in-error-message, no planning history. `PlannerEventHandler` exists but nil by default. First production question ("what plan did it pick?") requires re-running with EXPLAIN under the same data/params.
  - **Fix:** optional planning-metrics hook (nil = silent) logging SQL (truncated), plan hash, planning duration, cache hit/miss, estimated cost. Sample ~1% in production to structured output.

---

## Stress test 1M baseline (2026-05-27)

**Run command:** `bazelisk test //pkg/relational/sqldriver/stress:stress_test --test_output=streamed --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v"`

| Query | Rows | Time | Threshold |
|-------|------|------|-----------|
| pk_lookup_first | 1 | 1.5ms | <5ms |
| pk_lookup_middle | 1 | 1.5ms | <5ms |
| pk_lookup_last | 1 | 1.7ms | <5ms |
| index_customer_eq (8 rows) | 8 | 9.1ms | <10ms |
| index_amount_range (100K rows) | 100017 | 196ms | |
| index_status_count | 1 | 362ms | |
| full_scan_count | 1000000 | 3.1s | ~3s/1M |
| full_scan_filter | 1 | 534ms | |
| group_by_status | 4 | 5.25s | |
| group_by_status_count_only | 4 | 1.9ms | |
| sum_by_status | 4 | 2.0ms | |
| group_by_customer_having | 47271 | 107ms | |
| join_10_outer | 10 | 4.1ms | |
| order_by_pk_full (1M rows) | 1000000 | 3.33s | ~3s/1M |
| order_by_pk_index_filter | 8 | 3.4ms | |
| scan_all_narrow (1M rows) | 1000000 | 3.33s | ~3s/1M |
| scan_all_wide (1M rows) | 1000000 | 3.66s | ~3s/1M |
| in_list | 46 | 10ms | <10ms |
| needle_in_haystack_pk | 1 | 2.0ms | <5ms |
| needle_in_haystack_filter | 1 | 2.4ms | <5ms |
| full_scan_sparse_filter | 97 | 3.0s | ~3s/1M |
| update_by_index | 8 | 4.0ms | |
| delete_single_row | 1 | 2.3ms | |

All 23 subtests PASS. Total: 170.7s (incl. bulk insert ~2:28).

---

## Phase 8: Planner architecture cleanup (Graefe review findings)

### 8.1 Evaluate `pushDataAccessTasks` as CascadesRule — RESOLVED (keep procedural)

Graefe flagged this as procedural code that should be a rule. After investigation: **the procedural approach is architecturally correct.** `pushDataAccessTasks` operates on Reference-level PartialMatch state, not expression types — CascadesRules require expression-type pattern matching. Java uses explicit `TransformMatchPartition` task types for the same reason: this is task-level logic, not rule-level. Go's direct method call in `ExploreExprTask.Run()` is simpler and equivalent. No change needed.

### 8.2 Verify `promoteByDataAccessCost` heuristic absorbed — VERIFIED

`promoteByDataAccessCost` was deleted in eb94291a (dead code cleanup). Its heuristic (prefer lower-cardinality data access) IS absorbed into `PlanningCostModelLess` at `planning_cost_model.go:191–208` — Criterion #2: `maxDataAccessCardinality`, lower wins. This fires via `stampOrderingWinners(ref, costModel)` after every data access insertion. The cost model uses the same `findExpressionsByType` + `maxDataAccessCardinality` comparison. No heuristic was dropped.

### 8.3 Document `maxRoundsPerRef = 10` cap — DONE

Added comment at `unified_tasks.go:59` explaining: prevents divergence from rule cycles (A→B→A) that produce distinct-but-equivalent members. Java relies on memo dedup for fixpoint; Go's per-Reference dedup is weaker, so pathological rule interactions can produce new members indefinitely. 10 rounds >> typical 2–3 needed, safely under MaxTasks budget.

---

## Phase 7: Cascades alignment — close remaining Java divergences

### 7.1 Unify alias namespaces — DONE

Quantifier aliases now match table aliases at creation. Three band-aids removed: `rightAliasSet`, `planContainsJoin`, `collectPlanAliases` (−114 lines). Root-cause fix in `mergeRows`: bare inner keys overwrote qualified keys from nested joins (missing `!exists` guard). 46/46 tests, 15/15 determinism.

### 7.2 Port matching infrastructure for index intersections — DONE

`IndexIntersectionRule` deleted (Go-only REWRITING-phase rule). Replaced with match-based PLANNING-phase intersection via `WithPrimaryKeyIntersector` in `intersector_primary_key.go`. Wired into `pushDataAccessTasks` with guards: candidate cap (4), match cap (8), restricted-scan filter, idempotency via `hasIntersectionFinal`. Two regressions found and fixed: IS NULL correctness (zero-coverage matches created incorrect intersections, fixed by filtering to restricted scans only) and MaxTasks (intersection logic ran N times per Reference, fixed by idempotency guard). 46/46 tests, 10/10 determinism.

### 7.3 Convert remaining predicateReferencesAlias sites — DONE

All 8 `predicateReferencesAlias` calls in the NLJ rule converted to `GetCorrelatedToOfPredicate` correlation-set checks. Function deleted. Root-cause fix: `qualifyBareFieldValue` in EXISTS builder now produces QOV-based FieldValues instead of flat strings. `walkPredicateFieldValues`/`fieldValueAliasAndCol` survive in push-filter/push-projection rules (handle both QOV and flat FieldValues for unit test compatibility).

### 7.4 FlatMap wrapper correlation propagation — NOT NEEDED (Graefe confirmed)

Graefe confirmed: `GetCorrelatedToWithoutChildren()` returning empty is correct for BOTH joins AND correlated subqueries. Correlations flow through quantifier children in both cases. `JoinMergeResultValue.Children()` does NOT need QOV nodes.

For correlated scalar subqueries (Go-only extension, Java rejects at grammar level), the correct Cascades architecture is:
1. `ForEachNullOnEmpty` quantifier (already exists: `ForEachNullOnEmptyQuantifier`)
2. `RecordQueryFirstOrDefaultPlan` with NULL default (already exists)
3. Correlated `BuildScalar` fallback (needs: full inner plan with outer scope, correlation predicate extraction)
4. NLJ rule: detect NullOnEmpty → wrap inner with FirstOrDefault + FlatMap

NLJ wrapper correlation propagation (walks predicates) is already correct and active.
