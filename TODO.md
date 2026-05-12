# TODOs

Java Record Layer version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

---

## CRITICAL — scalability bugs (dayshift-93 stress test)

Surfaced by `just stress` (10K/100K row tests). These are data correctness and architectural issues.

### Silent FDB timeout truncation

Queries return **partial results without errors** when the 5s FDB transaction limit is hit. At 100K rows: GROUP BY returns 2/4 status groups, ORDER BY returns 47K/100K rows, JOINs return 0 rows. The caller has no way to know the result is incomplete.

**Root cause:** the executor's scan cursor exhausts the FDB transaction timeout mid-scan and treats the end-of-transaction as end-of-data. No continuation is attempted across transaction boundaries for SQL queries (the record-layer cursor has `TimeScanLimiter` + continuation support, but the SQL executor doesn't use it).

**Fix:** the SQL executor must either (a) paginate across FDB transactions using continuation tokens (matching Java's `executeState` loop in `PhysicalQueryPlan`), or (b) detect the timeout and raise an explicit error instead of silently truncating.

### PK lookups scale O(N)

Single-row PK lookup: 250ms at 10K rows, **6 seconds** at 100K rows. Should be O(log N) via index seek.

**Root cause:** `scanComparisonsToTupleRange` likely produces a prefix range instead of a point key for equality comparisons on PK columns. The FDB range scan reads all records with the prefix, which is the entire table when the PK is the only key component. Needs investigation in the executor's `scanComparisonsToTupleRange` → `IndexFetchCursor` path.

**Stress test data:**
| Rows | PK lookup | COUNT(*) | ORDER BY (all rows) |
|---|---|---|---|
| 10K | 250ms | 240ms | 276ms |
| 100K | 6.0s | 3.8s | 3.7s (47K/100K truncated) |

### NLJ JOIN is O(N×M) — no correlated index probes

NLJ does a full inner table scan for every outer row. `SELECT ... FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 10` takes **37 seconds** at 10K orders × 1K customers — even though `c.id` is the customers PK.

**Root cause:** the NLJ executor materializes the entire inner table per outer row and filters post-hoc. Java uses `FlatMapPlan` with correlation bindings that allow the inner scan to use index lookups. Go's `RecordQueryNestedLoopJoinPlan` has no equivalent — it calls `executePlan(innerPlan)` unconditionally.

**Fix:** either (a) port Java's `FlatMapPlan` with correlated index probes, or (b) add hash-join / merge-join physical operators, or (c) at minimum, implement inner-result caching for uncorrelated inner scans (partially done in swingshift-92 for the uncorrelated case, but correlated JOINs still do full scans).

**Stress test data:**
| Operation | 10K (1K customers) | 100K (10K customers) |
|---|---|---|
| JOIN 10 outer rows | 37.6s | 4.0s (0 rows!) |
| JOIN 100 outer rows | 36.1s | skipped |
| EXISTS subquery | 11.8s | skipped |

---

## Active work

### Bytes IN-list Ginkgo harness flake (491→492/492)

1 remaining cross-engine conformance failure. `bytesAdvancedScenario` query #2: `SELECT id FROM t WHERE payload IN (X'DEADBEEF', X'CAFEBABE') ORDER BY id` returns 0 rows in the Ginkgo shared-container context. Same code passes in 4 independent test contexts. Needs Java conformance server to diagnose.

### NormalizePredicatesRule existential guard

Go skips NormalizePredicatesRule for SelectExpressions with Existential quantifiers. Java fires on all. Root cause (dayshift-93): removing the guard causes planner non-convergence (MaxTasks cap hit) due to cascading rule interactions creating an infinite exploration loop. Fix requires rule deduplication infrastructure. Documented in DIVERGENCES.md.

### DecorrelateValuesRule test gap (25/29 Java tests)

4 remaining Java tests need push-down-into-child infrastructure (pushIntoChildSelect, pushIntoChildFilter, partitionValuesByChild, pushIntoExpressionsWithVariations).

### Covering index for SQL (multi-shift)

Port `IndexKeyValueToPartialRecord` (826 LOC), `computeIndexEntryToLogicalRecord`, `CollapseRecordConstructorOverFieldsToStar`. Root cause fully mapped in DIVERGENCES.md.

---

## Remaining TODO items

### Phase 5 — DDL + cache + driver completion

- [ ] **#29** D1 DDL action types — Go-only extension, not in Java 4.11.1.0. Low priority.
- [ ] **#30** D3 Online indexer integration via DDL. Gate: #29.
- [ ] **#33** D5 driver adapter gaps — custom Scanner/Valuer for Struct/Array/Versionstamp/Continuation. Low priority.
- [ ] **#34** E1 Go-vs-Java SQL perf bench — Go-side done, needs Java conformance server.

### Phase 6 — Cross-language verification + perf

- [ ] **#35** A4 INFORMATION_SCHEMA cross-engine byte-equivalence. Gate: upstream.
- [ ] **#36** Catalog wire format reverse direction. Needs Java conformance server.
- [ ] **#37** E2 ANTLR parser DoS hardening. Gate: upstream ticket.
- [ ] **#39** Go-only GROUP BY — keep as Go extension.

---

## Completed (summary)

All Cascades planner subsystems ported: ~65 rules, 34 plan types, 48 value types, 18 properties, 12 match candidates, 24 comparison operators, 9 predicates. Phase 1–4 complete. Partial Phase 5 (#26–#28, #31–#32) and Phase 6 (#38, #99). 6,553+ tests, 105 fuzz targets, 491/492 cross-engine conformance specs, 1754 yamsql scenarios.
