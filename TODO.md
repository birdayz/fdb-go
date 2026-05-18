# TODOs

Java Record Layer version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

---

## CRITICAL — Streaming cursor architecture (DONE)

**Resolved.** Replaced `CollectAll`-based blocking operators with Java-aligned streaming cursors. Every blocking operator (aggregation, sort, NLJ) is now a cursor that processes records one-by-one, propagates `TimeLimitReached`, and serializes partial state into protobuf continuations (`AggregateCursorContinuation`, `MemorySortContinuation`). The pagination layer recreates the cursor hierarchy per transaction from the composite continuation. Hash aggregation removed — streaming only (Java-aligned). 10K: 22/22 tests pass. 100K: 16/19 pass (3 remaining are intermittent FDB Docker issues + planner limitation).

Go's blocking operators call `CollectAll(ctx, innerCursor)` which drains the cursor into a `[]QueryResult` and **discards the stop reason**. When the leaf cursor hits the 4s time limit, `CollectAll` gets partial rows, doesn't know they're partial, and the operator produces wrong results. The `TimeLimitReached` signal is swallowed at the `CollectAll` boundary.

**What Java does per operator:**

| Operator | Java cursor | State in continuation |
|---|---|---|
| Streaming aggregation | `AggregateCursor` | partial accumulator (running SUM, current group key) |
| Sort | `MemorySortCursor` | all buffered records + inner continuation |
| FlatMap/NLJ | `FlatMapPipelinedCursor` | outer position + inner continuation + correlation bindings |

Each processes records one-by-one, detects `TimeLimitReached` from the inner cursor, serializes partial state, and returns `TimeLimitReached` to the caller. Zero data loss.

**Fix:** Replace `CollectAll`-based blocking operators with cursor implementations that:
1. Process inner records incrementally (not drain-then-process)
2. Detect `TimeLimitReached` from the inner cursor after each record
3. Serialize partial operator state (accumulator, sorted buffer) into the continuation
4. On resume: deserialize state, resume inner cursor, continue processing

**Affected operators and their Java equivalents:**
- `executeAggregation` → port `AggregateCursor` + `StreamGrouping` with `PartialAggregationResult` proto
- `executeSort` / `executeInMemorySort` → port `MemorySortCursor` with buffered-records continuation
- `executeNestedLoopJoin` → port `FlatMapPipelinedCursor` with outer+inner dual continuation
- `executeIntersection` → port incremental intersection with per-side continuations

See `RFC_TRANSACTION_PAGINATION.md` and `STRESS_RELATIONAL.md` for full analysis.

### Fixed: silent FDB timeout truncation

~~Queries return partial results without errors when the 5s FDB transaction limit is hit.~~

**FIXED.** `keyValueCursor.nextKV()` now propagates FDB errors. `paginatingRows` replaces the single-transaction `cascadesRows` with cross-transaction pagination (`TimeLimit=4s` per page, continuation-based resume). Leaf scans (SELECT, ORDER BY PK, index scans) now paginate correctly at 100K+. Blocking operators still get partial input — see above.

### Fixed: PK lookups scale O(N)

~~Single-row PK lookup: 250ms at 10K, 6 seconds at 100K.~~

**FIXED.** Registered `PrimaryScanMatchCandidate` in `GetMatchCandidates()`. Fixed `ImplementIndexScanRule` to handle `RecordQueryScanPlan`. Fixed `ToScanPlan()` to use `queriedRecordTypes`. PK lookup at 100K: **1ms** (was 6s).

### NLJ JOIN predicates not pushed into plan

`SELECT ... FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 10` takes **42s** at 10K. The Cascades planner produces `Filter(predicates, NLJ(Scan, Scan))` — the NLJ has **zero predicates** and materializes the full cross-product (10M pairs). Join conditions live in a separate Filter above the NLJ.

**Fix:** `ImplementNestedLoopJoinRule` must absorb predicates from the parent `LogicalFilterExpression` into the NLJ plan. Then the existing hash join infrastructure in the executor activates (equi-join key extraction, hash index build). Port Java's `FlatMapPlan` with correlation bindings for correlated joins (EXISTS subqueries).

---

## CRITICAL — FlatMap Java alignment (MOSTLY DONE)

Current `flatMapCursor` uses `mergeRows` to combine outer+inner — this is NOT how Java does it. Java evaluates a `resultValue` expression with both outer and inner bound as correlations. The `mergeRows` hack breaks for same-column-name joins and doesn't match Java's data flow.

**What Java does (RecordQueryFlatMapPlan.executePlan):**
1. Binds outer result as `CORRELATION` under `outerQuantifier.getAlias()`
2. Executes inner plan with correlated context + innerContinuation
3. For each inner result: binds BOTH outer AND inner as correlations → evaluates `resultValue` → produces output row
4. `resultValue` is a Value tree (RecordConstructorValue) that explicitly selects fields from both correlations

**What Go currently does (wrong):**
1. Binds outer datum as correlation ✓
2. Executes inner plan ✓
3. Calls `mergeRows(outer, inner, aliases)` — Go-specific hack that breaks for ambiguous columns

**Fix (must be done as a unit, no intermediate states):**
- [x] `RecordQueryFlatMapPlan` carries `resultValue values.Value` + `inheritOuterRecordProperties`
- [x] `flatMapCursor` binds BOTH outer and inner as correlations, evaluates `resultValue` — no `mergeRows`
- [x] `JoinMergeResultValue` produces merged map with qualified keys from both correlation bindings
- [x] Multi-predicate support: absorb equi-join into correlated scan, residual predicates above or inside inner
- [x] Same-column-name joins — `deriveColumnsFromFlatMap` handles qualified keys
- [x] `FlatMapContinuation` proto: wired (outer + inner position serialized/deserialized)
- [x] Secondary index FlatMap: correlated index scans via MatchCandidate
- [x] EXISTS/NOT EXISTS FlatMap mode with multi-predicate support (alias-stripped inner filter)
- [x] LEFT OUTER FlatMap with correct NULL row emission
- [x] Outer-only predicate push-down below FlatMap (alias-stripped)
- [x] Result value ownership aligned with Java: translator creates JoinMergeResultValue, rule passes through sel.GetResultValue() (nightshift-97)
- [ ] Replace `JoinMergeResultValue` with proper `RecordConstructorValue` (requires translator to produce field-level resultValue for joins — needs schema metadata at translation time)
- [ ] `check_value` field in FlatMapContinuation (concurrent-modification detection between transactions) (implemented in generic FlatMapPipelinedWithCheck; executor-level cursor does not use it — documented)

### Test coverage gaps vs Java (from audit of RecordCursorTest.java + JoinWithLimitTest.java)

- [x] **Multi-step FlatMap continuation under TIME_LIMIT**: testFlatMapReasons (5×5 grid, 6 cycles), pipelineWithInnerLimits, pipelineWithOuterLimits — both out-of-band and row-limit variants. Fixed two continuation bugs: priorOuterCont nil on resume, pending inner dropped on outer stop. (swingshift-96)
- [x] **OrElse (NOT EXISTS) under TIME_LIMIT**: Ported all 4 Java tests. Added OrElseWithContinuation with OrElseContinuation proto serialization. (swingshift-96)
- [x] **Inner/outer limit grid tests**: Both out-of-band (TimeLimitReached) and in-band (ReturnLimitReached) variants ported via iterateGrid helper. (swingshift-96)
- [ ] **JOIN continuation resume at SQL level**: Requires EXECUTE CONTINUATION implementation (parsed but not wired). Multi-shift effort.
- [x] **EXISTS + 3-way join + ORDER BY plan shape**: (covered by join_exists_self.yaml, correlated_exists_advanced.yaml, flatmap_exists_coverage.yaml, exists.yaml, correlated_subquery_probes.yaml + TestFDB_CorrelatedExistsCrossJoin, TestFDB_NestedCorrelatedExists)

---

## DONE — SQL LIMIT/OFFSET extension (swingshift-95)

Shipped. Parse in PlanVisitor.visitLimit → LogicalLimit in logical tree → Cascades translator skips it → paginatingRows applies post-execution. Tests: LIMIT 3, LIMIT 2 OFFSET 1, yamsql offset.yaml.

---

## Active work

### Bytes IN-list Ginkgo harness flake (491→492/492)

1 remaining cross-engine conformance failure. `bytesAdvancedScenario` query #2: `SELECT id FROM t WHERE payload IN (X'DEADBEEF', X'CAFEBABE') ORDER BY id` returns 0 rows in the Ginkgo shared-container context. Same code passes in 4 independent test contexts. Needs Java conformance server to diagnose.

### Correlated NOT EXISTS bugs — RESOLVED (nightshift-97)

Two bugs fixed:
1. Translator dropped NOT(ExistsPredicate) from predicate list → rule couldn't detect negation → NOT EXISTS acted as EXISTS.
2. Outer-only WHERE predicates were pushed inside the inner plan (or passed as NLJ join predicates) → they never matched → wrong rows emitted.

### NormalizePredicatesRule existential guard — RESOLVED (swingshift-96)

Fixed. Removed the existential quantifier guard and replaced with hash-based dedup to prevent the infinite normalization loop. Now fires on all SelectExpressions matching Java.

### DecorrelateValuesRule — RESOLVED (swingshift-96)

Push-down-into-child infrastructure added. All 29/29 Java tests ported (pushIntoChildSelect, pushIntoChildFilter, partitionValuesByChild, pushIntoExpressionsWithVariations).

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

All Cascades planner subsystems ported: ~65 rules, 34 plan types, 48 value types, 18 properties, 12 match candidates, 24 comparison operators, 9 predicates. Phase 1–4 complete. Partial Phase 5 (#26–#28, #31–#32) and Phase 6 (#38, #99). 6,568+ tests, 106 fuzz targets, 491/492 cross-engine conformance specs, 1754 yamsql scenarios.
