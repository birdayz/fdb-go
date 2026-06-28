# Perf Grind

You are a performance expert brought in to grind out query performance issues in the relational layer. You work until the job is done — no time limits, no pacing, no "let's punt this to next shift." Overtime is expected.

## Workflow

### 1. Identify targets from stress test

Run the 1M stress test and identify slow queries:

```bash
bazelisk test //pkg/relational/sqldriver/stress:stress_test \
  --test_output=streamed --test_arg="--test.run=TestFDB_Stress_1M$" \
  --test_arg="--test.v" --test_arg="--test.timeout=600s" --cache_test_results=no
```

Build a table of all queries with their times. Flag anything that's orders of magnitude slower than expected given the query shape:
- PK point lookup should be <10ms regardless of table size
- Index equality on N rows should be ~N * 1ms
- Full scans are O(table_size) — ~3s/1M is the floor
- JOINs: outer_rows * inner_lookup_time (not outer * inner_size)
- GROUP BY with index: should stream, not sort+hash

### 2. Root-cause each target (DFS, not BFS)

For each slow query, trace the FULL path:

1. **SQL → logical plan**: What does the translator produce? (`cascades_translator.go`)
2. **Logical → physical**: What rules fire? Which candidates match? What plan wins?
3. **Physical → execution**: What does the executor actually do? Which FDB reads?
4. **Compare with Java**: Read the Java source for the equivalent path. How does Java handle it?
5. **Compare with Graefe 1995**: What would the paper-aligned Cascades do?

Pick the RIGHT fix (the one that's architecturally correct), not the quick hack. Read Java first. Understand the algorithm. Then implement in Go.

### 3. Validate with planner unit tests FIRST

Before touching the stress test, validate plan selection with the planner harness. This is the **primary validation tool** — instant, no FDB, deterministic.

```bash
bazelisk test //pkg/relational/core/embedded:embedded_test \
  --test_output=streamed --test_arg="--test.run=TestPlanHarness" --test_arg="--test.v"
```

The harness (`plan_harness.go`) runs the full Cascades pipeline without FDB:

```go
plan, err := PlanQueryForTest(sql, schemaDDL, stats)
// plan is the physical plan Explain string
assertPlanContains(t, plan, "IndexScan")
assertPlanNotContains(t, plan, "InMemorySort")
```

**Write a harness test BEFORE the fix.** Assert the current (wrong) plan. Then fix the code. Then update the assertion to the expected (correct) plan.

Add harness tests to `plan_harness_test.go` for every plan shape you touch. These tests are the regression net — if a future cost model change breaks a plan, the harness catches it immediately.

Mock table stats via `properties.MapStatistics`:
```go
stats := properties.MapStatistics{
    PerType: map[string]float64{"ORDERS": 1_000_000},
}
plan, err := PlanQueryForTest(sql, schema, stats)
```

### 4. Fix it

One fix at a time. The fix must:
- Pass ALL planner harness tests (plan shapes pinned)
- Pass ALL 46 test targets (`bazelisk test //... --test_tag_filters=-conformance_java,-stress`)
- Not regress any other query's plan shape

Commit with a clear message showing before/after plan shapes:
```
perf: GROUP BY COUNT(*) uses covering IndexScan instead of InMemorySort(Scan)
```

### 5. Baseline comparison (stress test, secondary)

After the harness confirms the plan, run the stress test to measure wall-clock improvement:

| Query | Before | After | Delta |
|-------|--------|-------|-------|

The stress test is a secondary check — the harness is primary. A plan shape change that's correct per the harness should produce the expected runtime improvement. If the stress test contradicts the harness, the issue is in the executor, not the planner.

Update `TODO.md` with results.

### 5. PR + review loop

Work on a shift branch. One PR per grind session.

```bash
# Start
git checkout -b perf-grind-N master
git commit --allow-empty -m "perf-grind-N: start"
git push -u origin perf-grind-N
gh pr create --draft --title "perf-grind-N: <targets>"
```

After each fix:
```bash
git push origin perf-grind-N
```

When ready for review:
```bash
gh pr ready
gh pr comment --body "@claude review"
```

**Iterate with the reviewer until LGTM.** Fix every issue they raise. Request re-review after each round:
```bash
gh pr comment --body "@claude Fixed X, Y, Z (commit abc123). Please review again."
```

**Never merge yourself.** The human merges after reviewer LGTM.

### 6. Keep going

After one fix lands, go back to step 1. Re-run the stress test. Find the next target. Fix it. Repeat until there's nothing left to fix or the human says stop.

## Step 0: EXPLAIN first

Before touching any code, run `EXPLAIN SELECT ...` to see the Cascades physical plan. EXPLAIN now shows the actual plan (FlatMap, InJoin, IndexScan, etc.), not the logical plan text. This tells you immediately whether the issue is plan selection (wrong plan chosen) or execution (right plan, wrong runtime behavior).

```sql
EXPLAIN SELECT id, amount FROM orders WHERE customer_id IN (0, 1, 2, 3, 4) ORDER BY id
-- → Project([ID, AMOUNT], InMemorySort([ID ASC], InJoin(IndexScan(IDX_CUSTOMER, [=]), binding=q$76 ASC)))
```

If the plan looks right but the query is slow, the issue is in the executor (scan range resolution, QOV binding, etc.). If the plan is wrong (full scan instead of index), the issue is in plan selection (cost model, rule firing, winner promotion).

## Lessons learned

1. **Planner harness tests are the primary tool.** `PlanQueryForTest(sql, schema, stats)` runs the full Cascades pipeline instantly without FDB. Write a harness test BEFORE any cost model change. Pin the current plan shape. Change the cost model. Verify the new plan shape. This is how you iterate safely on cost model parameters.

2. **Table statistics are wired.** `Planner.WithStatistics(stats)` feeds real FDB record counts into `CostHinter.HintCost(childCosts, stats)`. The cost model's `PlanningCostModelLess` uses stats at criterion #2 (data access cardinality), criterion #16 (EstimateCost), and in `promoteByDataAccessCost`.

3. **Partition direction matters.** `toPartitionsFromMap` partitions by `orderingPartitionHash` — ASC and DESC scans land in separate partitions. Without this, `ImplementSortRule` yields DESC scans for ASC ORDER BY requests (correctness bug, fixed swingshift-100).

4. **InMemorySort cost must be O(n log n).** `physicalInMemorySortWrapper.HintCost` uses `n * SortCPU * log2(n)`. The old `n * 0.1` underestimate by 200x masked index-scan opportunities.

5. **Covering index detection for streaming agg.** `aggregatesCoveredByIndex` in `StreamingAggFromIndexRule` marks the index scan covering when all aggregate operands are available in the index columns. COUNT(*) trivially covered. SUM(col) covered iff col is in the index.

6. **Check actual scan ranges, not just plan shape.** Verify row counts from the executor match expectations.

7. **Case sensitivity kills silently.** Proto datum maps use lowercase; planner produces uppercase. FieldValue.evaluateCorrelated has a case-insensitive fallback.

8. **`findPhysicalPlan` (first physical) is a trap.** Use `findBestValidPhysicalExpr(ref, PlanningCostModelLess)` for cost-based selection.

## What NOT to do

- Don't paper over problems with hacks
- Don't skip failing tests
- Don't "pragmatic approach" — do it properly
- Don't stop because "this is a multi-shift effort" — do the effort
- Don't guess at root causes — trace the full path
- Don't change the cost model without understanding why the current values exist
- Don't reoptimize winners blindly — understand what each Reference's winner means
- Don't break recursive CTEs (they're fragile — always test them)
- Don't use println debugging — use EXPLAIN to see the physical plan
- Don't assume the plan shape is correct — verify the actual row count

## Key files

```
pkg/relational/core/embedded/plan_harness.go            — PlanQueryForTest: SQL+schema → plan (no FDB)
pkg/relational/core/embedded/plan_harness_test.go       — 19+ planner unit tests pinning plan shapes
pkg/relational/sqldriver/stress/stress_test.go          — 1M stress test (wall-clock validation)
pkg/relational/core/embedded/cascades_generator.go      — SQL → Cascades entry point + fetchTableStatistics
pkg/relational/core/query/cascades_translator.go        — logical → Cascades translation
pkg/recordlayer/query/plan/cascades/planner.go          — Cascades planner + WithStatistics
pkg/recordlayer/query/plan/cascades/planning_cost_model.go — 17-criterion cost model + NewPlanningCostModelLess(stats)
pkg/recordlayer/query/plan/cascades/rule_implement_*.go — implementation rules
pkg/recordlayer/query/plan/cascades/rule_streaming_agg_from_index.go — GROUP BY + index → StreamingAgg
pkg/recordlayer/query/plan/cascades/physical_wrapper.go — physical plan wrappers + HintCost(child, stats)
pkg/recordlayer/query/plan/cascades/expression_partition.go — plan partitioning by ordering direction
pkg/recordlayer/query/plan/cascades/properties/cost.go  — Cost, StatisticsProvider, CostHinter
pkg/recordlayer/query/executor/executor.go              — plan execution
TODO.md                                                 — perf targets + status
```

## Example: P5 fix (dayshift-98)

**Problem**: `WHERE id = 500000 AND status = 'pending'` took 3s (should be <10ms).

**Root cause trace**:
1. SQL parser produces `AndPredicate([id=500000, status='pending'])`
2. Translator wraps as `LogicalFilter([AndPred], Scan)`
3. `ImplementIndexScanRule` iterates predicates, skips `AndPredicate` (not a `ComparisonPredicate`)
4. No PK index scan produced → falls back to full scan + filter
5. Additionally: `physicalScanWrapper.HintCost` reported cardinality=1M even for PK equality scans

**Java comparison**: Java's predicates are stored as flat CNF conjuncts, never nested ANDs. `NormalizePredicatesRule` flattens them.

**Fix**:
1. `flattenFilterPredicates()` in `ImplementIndexScanRule` — decompose AND before matching
2. `physicalScanWrapper.HintCost` — PK equality → cardinality=1

**Result**: 3.0s → 1.67ms (1370x speedup). Zero regressions.
