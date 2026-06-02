# TODOs

FoundationDB Record Layer — Go Port. Java version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

Current state: 46 test targets, 639+ SQL tests passing, 270 yamsql scenarios, 508 cross-engine specs, 105 fuzz targets, ~65 Cascades rules, 41 plan types (36 executor-wired), 48 value types, 9 predicate types. Unified Cascades task stack (REWRITING + PLANNING). Winner-based plan selection with per-ordering properties.

---

## Known gaps

### [x] CORRECTNESS FIXED — re-enumerated indexed multi-way joins (was: NULL / 0 rows)

**Symptom (fixed).** A 3-way *indexed chain* join planned through the RFC-042 L3
index-NLJ re-enumeration path returned wrong results that depended on the
FROM-order: one order returned 200 rows all-NULL, the opposite order returned 0
rows (correct is 200 rows, all `t1.id = 1`). 2-way joins and non-indexed *star*
3-way joins (`TestFDB_ThreeTableFrom`) were always correct.

**Root cause (pointer-level instrumented).** `PartitionSelectRule` misrouted the
*spanning* join predicate (e.g. `t3.t2_id = t2.id`, one alias in each partition
half) into the **lower** partition. Java's classification keys on
`uppersDependingOnLowersAliases`, computed from `getCorrelationOrder()` —
**quantifier** correlations. Go's flat-seed join quantifiers are independent
scans with **no quantifier-level correlations** (the joins are plain predicates),
so `uppersDependingOnLowers` is *always empty* and the spanning predicate always
fell to the "can do in lower" branch. That yields a degenerate **Case-1
cross-product** partition whose lower result is a `{_0}` literal placeholder
(discarding the real columns) and whose pushed-down filter evaluates against
unbound upper aliases → wrong rows. The physical FlatMap then merges via
`JoinMergeResultValue`, which cannot resolve columns nested under `_0` → NULL.

**Fix (shipped).** `PartitionSelectRule` now rejects the degenerate partition: a
predicate routed to the lower that references an UPPER alias cannot be evaluated
there, so the whole partition is skipped (`rule_partition_select.go`, "Reject
degenerate partitions" guard). The valid associativities — where the spanning
predicate stays at the join level — then win identically for every FROM-order.
Both orders now return 200 correct rows; deterministic; full suite green.
`multiway_join_index_probe_test.go` was a plan-shape-only fake checkbox (never
executed the query) — now retrofitted with **row-correctness** assertions for
both FROM-orders, which is the load-bearing check.

**Remaining (cost-optimality, NOT correctness) — RFC-042.** Under the big→small
FROM-order the re-enumerated `(t2⋈t3)` sub-product still prefers a cross-product
NLJ over the index probe (the index-probe alternative either loses on cost or
flows a sub-product result the parent predicate can't SARG), so that order
full-scans the 200-row T3 instead of index-probing it. Correct, just slower. Full
byte-identical FROM-order invariance for N≥3 (the `TestFDB_MultiwayJoinOrder_Probe`
goal) depends on closing this cost gap + FROM-order-deterministic winner selection.
Likely levers: the index-probe cardinality cost (criterion #2 — make the FlatMap
inner range over the index-scan wrapper so `maxDataAccessCardinality` reflects the
probe), and making re-enumerated sub-products flow a flat `JoinMergeResultValue`
so the index-probe variant is both cheaper AND resolvable.

### vs Java (correctness/feature parity)

- [x] **Correlated filter without index.** Fixed in 56874f23 — ImplementFilterRule sets innerAlias on RecordQueryPredicatesFilterPlan. All correlated paths (scalar subquery, EXISTS, JOIN) work without indexes. 14+ integration tests verify.
- [x] **RIGHT/FULL OUTER JOIN.** Done in RFC-036. (The old "only LEFT OUTER" note was stale — RIGHT already worked via operand-swap normalization in `cascades_translator.go`, pinned by `TestFDB_RightJoin`.) FULL OUTER added as a Go-only query extension: Java's SQL layer has **no** outer joins at all (`visitOuterJoin` is a no-op, zero tests), so LEFT/RIGHT/FULL are all read-path-only extensions with **zero wire-format impact** — Java apps still read/write the same records. FULL OUTER is implemented exclusively by the materialized NLJ cursor (`streaming_cursors.go`): LEFT-OUTER outer loop + a `matchedInner` bitmap + a drain phase emitting unmatched inner rows NULL-padded on the left. Routed away from the correlated FlatMap path (cannot observe global inner-match state); FULL+EXISTS rejected with a clear error. 9 FDB integration tests (all four row classes, NULL-key 3VL, many-to-many, large-inner hash+drain, WHERE-above-join, determinism, RIGHT NULL-key regression). Graefe+Torvalds ACK.
- [x] **Correlated scalar subquery shapes widened.** Non-aggregate (ORDER BY + LIMIT), multi-table inner FROM (JOINs), multi-column validation, deep-walk replaceScalarSubqueryRef. GROUP BY/HAVING rejected with clear errors (PredicatePushDownRule AliasMap conflict). CorrelatedExistsError propagation fixed.
- [ ] **No *general-purpose* window functions — and Java has none either.** Investigation (RFC-045): Java's relational layer has **no** general streaming window operator. The general `windowClause` is commented out in Java's grammar ("don't want to deal with them now"); `LAG`/`LEAD` are grammar tokens with **no** value class; `RankValue implements Value.IndexOnlyValue` (computable only from a rank/leaderboard index, never over a result set). The **only** working window function in Java is `ROW_NUMBER() OVER (... ORDER BY <distance>) <= K` via `QUALIFY`, used exclusively for **vector/HNSW K-NN search**. So "match Java's window functions" ≡ "finish the vector/HNSW relational parity" — tracked as **Phase 9** below. General windowing over plain tables would be a *Go-only extension Java lacks entirely* (allowed if wire-compat holds + deep tests), not parity — deferred, not in Phase 9.
- [x] **GROUP BY/HAVING in correlated scalar subqueries.** Done in RFC-047 — a Go-only read-side extension (Java rejects correlated scalar subqueries at the grammar level entirely; zero wire impact). The stale "PredicatePushDownRule AliasMap.Compose conflict" blocker no longer applies: GroupByExpression is already a push-down barrier (no case in `pushPredicateToExpression`) and the panicking `AliasMap.Compose` has no production callers. `buildCorrelatedScalar` now builds GROUP BY (+ HAVING) into the inner plan and caps with `LIMIT 1`; the scalar contract is FirstOrDefault (first group + LEFT-OUTER NULL-on-empty), NOT a runtime cardinality assertion (Graefe). Empty input → 0 groups → NULL falls out naturally (vs no-GROUP-BY COUNT → 0). Group keys + aggregate operands resolve via the semantic scope (`ResolveIdentifier`), scalar column named with the bare operand to avoid an embedded-`.` qualifier mis-parse. 42803 enforced via `validateGroupByProjection`; multi-column + EXISTS-in-HAVING + unresolvable-expr-arg/key rejected. 23 FDB integration probes (incl. EXPLAIN-pins-StreamingAgg, empty→NULL contrast, expression group key, join+GROUP BY, determinism 10×).
  - [ ] **Follow-up: `ORDER BY` over grouped output in a correlated scalar subquery.** Currently `ORDER BY` combined with `GROUP BY` in a correlated scalar subquery is **rejected** with a clear error (RFC-047) — ordering the *groups* to make the multi-group FirstOrDefault choice deterministic needs post-aggregation sort-key resolution this builder does not wire. The interim rejection is correct (no silent wrong results); closing this would make multi-group scalar subqueries deterministic. `ORDER BY` *without* `GROUP BY` (ordering rows before the `LIMIT 1`) already works.
  - [x] **Follow-up (single-source): expression/constant-argument aggregate that meets a *differing* aggregate via HAVING in a correlated scalar subquery.** DONE — the addendum unified producer and consumer on **one** canonicaliser (`canonicalAggName`, called by both `buildCorrelatedScalar` and `rewriteAggregateValue`), so the two name schemes can no longer drift; the prior fail-safe rejection is gone for single-source. The last silent-wrong corner (nested-arithmetic args like `SUM((amount+10)*2)` returning NULL → dropped groups) was a *separate* root cause — an inverted `!isArith` guard in `translateAggregate` that preferred a lossy text reparse over the resolved operand — fixed in RFC-048 (4dc3276c): the resolved `AggregateOperands[i]` is now always the source of truth. Works now (single-source): `SELECT COUNT(1) … HAVING COUNT(*)` both directions; `SELECT SUM(a*2) … HAVING SUM(a*3)`; decimal-literal args (`SUM(a*1.5)`); nested-arith args (`SUM((a+10)*2)`). `COUNT(DISTINCT 1)` correctly still rejected (DISTINCT unsupported here). Pinned by `quality_probes_test.go` (count_constant_with_having_works, expression_aggregate_in_having_works, decimal_literal_aggregate_arg_in_having, nested_arithmetic_aggregate_arg_in_having). **Residual (join only):** over a JOIN an expression-argument aggregate in HAVING is still rejected (the operand binds to the wrong quantifier through the parser round-trip) — pinned by `join_expression_aggregate_in_having_rejected`.
- [ ] **🚩 IN over an indexed column drops the outer projection (wrong result schema).** Found by RFC-048 W3.5 (plan-diversity oracle). `SELECT id FROM t WHERE a IN (1,7)` over a table with an index on `a` plans as `InJoin(IndexScan(IDX_A,[=]))` with **no** outer `Project([ID])`, so it returns columns `[ID, A]` instead of `[ID]`; the full-scan plan for the same query is `Project([ID], PredicatesFilter(Scan(T)))` and returns `[ID]`. The returned *rows* (id values) are correct — only the projection/output schema diverges — but `rows.Scan(&id)` then fails ("expected 2 destination arguments"). Root cause is NOT the InJoin rule (Go's `ImplementInJoinRule` faithfully mirrors Java: it only fires when the SELECT's result value is the inner QOV, so the `id` projection is a separate expression *above* the IN-Select); the outer `Project([id])` is simply not materialised over the InJoin physical alternative the way it is over the Scan path. Deterministic repro: indexed schema `t(id BIGINT PRIMARY KEY, a BIGINT)` + `CREATE INDEX idx_a ON t(a)`, insert a few rows, run `SELECT id FROM t WHERE a IN (1,7)` and compare column count vs the same query on an unindexed copy. Out of scope for the index-range correctness fix in RFC-048 (separate Project-over-InJoin planning subsystem); the W3.5 oracle excludes the IN shape until this is fixed.
- [x] **DML does not execute through Cascades (parallel pipeline).** Fixed as **P0.4** — all DML now executes through Cascades (`planDML`); the naive `execStatement` DML path is deleted. See P0.4.

### Beyond Java (Go-only improvements)

- [x] **Full Graefe Memo with cross-group merging.** Done in RFC-037 — union-find group merging (the Cascades-paper "merge two groups discovered to be one", §2 + §3.5), a Go-only extension beyond Java (which, like the pre-RFC Go memo, only interns at insertion time). `Reference` gains a monotonic `id` + `forwardedTo` + path-compressed `Canonical()`; every state-bearing method resolves the receiver to canonical, so a merged-away (loser) Reference transparently forwards — no in-flight task, Quantifier, or binding is rewritten. `GetRangesOver()` resolves at the single chokepoint (444 sites). `Memo.Integrate` hooks the REWRITING yield path: when a yielded expression equals a member of a different group, the two merge (survivor = lower id, deterministic), folding members + exploration state, repointing the topology index, invalidating correlation caches up the DAG, and recursively re-merging parents (paper's bottom-up recursion). Scoped to REWRITING (PLANNING winners/partial-matches embed raw References — guarded by `mergeable`); ancestor/descendant (idempotence) merges skipped to avoid DAG cycles. Wire compat untouched (read-path-only sharing). Merge fires through the real planner (`TestMemoMerge_FiresThroughRealPlanner`); 9 merge unit tests + determinism 50×/10×; 46/46 targets green; stress-1M unchanged. Graefe+Torvalds ACK (NAK'd v1 on in-flight-task stranding + cache staleness + index repoint + upward re-merge — all fixed in v2). **Reach caveat (honest):** the merge is correct and fires, but its practical reach is narrow today — the memo's interning/equivalence is alias-sensitive, and rule-rewritten equivalents mint fresh quantifier aliases, so equivalent sub-expressions intern to *different* child References and rarely surface as merge candidates (measured: exactly one merge on a K-branch equivalent UNION regardless of K; ~2% planner-time delta; no execution-time effect — same plan). Broad merging (and any real speedup / multi-way-join-order benefit) is **gated on alias-namespace unification (item 7.1 below)**; this PR lands the correct merge *infrastructure*, not a present-day perf win. Remaining (Future Work): **alias-normalized equivalence (7.1) — the lever**; reduction-rule-triggered merges (§3.6); PLANNING-phase merging; cost-model exploitation of shared sub-products for full N-way join-order optimality.
  - **PR-A landed the lever (RFC-038 epic / RFC-039 + RFC-040).** The reach caveat is now closed: the memo's structural-equivalence compare sites use alias-aware `expressions.MemoEqual` (faithful port of Java `Reference.isMemoizedExpression`) on top of the RFC-040 foundation (alias-aware `EqualsWithoutChildren` + alias-invariant `HashCodeWithoutChildren`). Rule-rewritten equivalents that differ only in fresh quantifier aliases now intern/merge into the SAME Reference — proven by `memo_activation_test` (K=6 alias-variant filters → 1 shared Reference, was K distinct). Zero plan-shape regression (plandiff conformance green), 10/10 deterministic, stress-1M before/after within noise. Graefe+Torvalds ACK. Still ahead in the epic: **PR-C** join-order enumeration (associativity/commutativity, capped) and **PR-D** cost selection + the e2e "multi-way join ordering proven" test (N-table join, EXPLAIN-pinned optimal order ≠ FROM-order, shared sub-products merged).
- [x] **Correlated scalar subqueries.** Go-only extension — Java rejects at grammar level. Implemented via FlatMap with JoinTypeLeftOuter.

---

## Production readiness (Graefe review, 2026-05-28)

The Cascades architecture is solid — task stack, two-phase REWRITING+PLANNING, 16-criteria cost model, match-candidate infra all well-ported. The production risks are all at the **boundaries**: planner↔executor, executor↔runtime, system↔operator. Priority tiers below.

### P0 — fix before deploying anywhere (correctness/availability)

- [x] **🚩 P0.4 DML executes through Cascades.** Fixed in RFC-035 — all DML (INSERT VALUES/SELECT, UPDATE, DELETE) routes through `planDML` → Cascades executor; `planOne` no longer branches on exec mode and the naive `execStatement` DML path (`execInsert`/`execUpdate`/`execDelete`/`execInsertSelect`, `pkPushdownCursor`) is deleted. INSERT VALUES reuses the Explode operator (RecordConstructor→Array→Explode→Insert) with plan-time arity/NOT-NULL/coercion; UPDATE SET RHS resolves to Values; DELETE/UPDATE WHERE gets EXISTS/scalar-subquery support via `upgradeDMLWhereWithCatalog`; INSERT…SELECT maps projection→target positionally and materializes (Halloween-safe). `IsUpdate()` derived from physical plan type; `RowsAffected` counted (Java's countUpdates); DML respects explicit transactions via `runInTx`. Fixed a latent non-correlated-EXISTS semi-join bug that also affected SELECT. QueryContext rejects update plans before executing (use Exec) — documented divergence in DIVERGENCES.md. Corner-case tests in `dml_cascades_fdb_test.go`. Graefe+Torvalds ACK (direction + implementation).
- [x] **P0.1 NLJ memory bomb.** Fixed in PR #203 — `CollectAllBounded` with configurable materialization limit (default 100K rows) on all 6 `CollectAll` sites. `MaterializationLimitExceededError` typed error. All cursor leaks on error path fixed. 11 regression tests. RFC-028.
- [x] **P0.2 Plan cache serves wrong plans.** Fixed in RFC-029 — cache keys on normalized SQL string directly (was uint64 FNV-64a hash with no text comparison on hit → collision = wrong plan). Scalar subquery staleness was a non-issue: `scalarSubqueryBinding` stores plans not results, re-evaluated per page fetch. `QueryHash` retained for tests only.
- [x] **P0.3 No context cancellation in executor.** Fixed in RFC-030 — `ctx.Err()` checks at the top of every cursor OnNext loop and drain function (44 sites across 19 files). `autoContinuingCursor` was the worst offender (created new FDB transactions on cancelled contexts). All cursor combinators, executor cursors, utility drains, DML drains, legacy query path drains, and iterator adapters now respect Go context cancellation. 24 unit tests verify prompt cancellation.

### P1 — fix before relying on the optimizer for real workloads (plan quality)

- [x] **P1.1 Wire statistics from FDB.** Fixed in RFC-031 — `fetchTableStatistics` was already wired (nightshift-100) but had two bugs: used read-write transactions for read-only stats (wasted commit), and fabricated equal-distribution counts for intermingled schemas. Fixed: `FDBDatabase.RunRead()` for no-commit snapshot reads, dropped intermingled fallback (returns nil → safe DefaultStatistics). E2E FDB integration test proves full pipeline: count maintenance → stats read → cost model → plan selection → execution.
- [x] **P1.2 Complete QOV-based FieldValue migration.** Fixed in RFC-032 — all 10 `stripAlias*` calls deleted (8 NLJ rule, 2 PushFilterBelowJoinRule). Predicates now retain `FieldValue(QOV(correlationId), "column")`; filters use `PredicatesFilterPlanWithAlias` so the executor binds each row under its correlation alias and resolves via `evaluateCorrelated` — zero string manipulation. `executePredicatesFilter` binds the inner alias whenever present (was gated on params only). Root cause exposed: `PartitionBinarySelectRule` (Java inner-join rule) fired on LEFT OUTER joins, pushing nullable-side predicates pre-join; guarded with `JoinInner`. `mergeRows` string qualification untouched (operates on executor row maps, not planner Values — separate concern). All 46 targets pass; determinism verified.

### P2 — fix before scaling operations

- [x] **P2.1 Plan cache LRU is O(n) per hit.** Fixed in RFC-033 — replaced the slice-based LRU order tracking (linear scan + slice splice in `promote()` on every hit, under the lock) with a `container/list` doubly-linked list + `map[string]*list.Element`. Promote-on-hit/update and eviction are now O(1), matching Java's Caffeine-backed cache. `RWMutex` downgraded to `Mutex` (the read path always mutated the list, so the read lock was a lie). `BenchmarkPlanCache_HitLargeCache` confirms position-independent O(1) hits at maxSize=1024; all existing LRU-semantics tests pass unchanged + new interleaved-eviction test.
- [x] **P2.2 Operational debuggability.** Fixed in RFC-034 — `PlanGenerationLogger` hook (nil = silent) emits one `PlanGenerationInfo` per Cascades planning call: SQL (truncated, rune-safe), plan hash (`plans.PlanHash`), plan explain, planning duration, cache event (hit/miss/skip/inconclusive), cache size, slow-query flag, error. Go analog of Java's `RelationalLoggingUtil` + `PlanGenerator` finally block; wired into `planSelectCascades` (real query) and `planDML` via a shared `planLogScope` with a named-return defer. EXPLAIN re-entry suppressed via `logMetrics bool`. No scalar "estimated cost" — the Cascades cost model is a comparator, not a number (matches Java; logs plan hash + explain instead). Threshold default sourced from the canonical `api.OptLogSlowQueryThresholdMicros` (single source of truth); `OptLogQuery` left intentionally unwired (no SLF4J level concept in Go — handler owns level + sampling), documented at `options.go:55`. Sampling is the handler's responsibility. 11 unit tests + 2 FDB integration tests (DML Skip event, SELECT miss-then-hit through the public driver). Graefe ACK, Torvalds ACK.

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

### 7.5 Structural interning key for re-enumerated join sub-products (RFC-043 follow-up)

`PartitionSelectRule.mergeQuantifierAlias` (RFC-043) synthesizes a **string** quantifier
alias (`$m_<len>:<name>...`, netstring-injective) so that identical merged join
sub-products reached from different bipartitions wrap under the same alias and intern to
one memo Reference. This string-alias-as-interning-key is a workaround for the absence of
true structural/alias-aware memo equality on the re-enumeration path — and it has already
produced two bugs (order-dependence, then non-injectivity on `_`-containing names; both
fixed + pinned). Graefe + Torvalds both flagged it as a non-blocking smell: the right key
is the memo's structural/hash equality over the live-alias SET (RFC-039/040 `MemoEqual` /
alias-invariant `HashCodeWithoutChildren`), not a hand-rolled serialization. Replace the
synthesized stable alias with structural interning when the ≥5-way enumeration-efficiency
work lands (it ties into RFC-039 broad memo merging). Until then: do NOT expand the string
scheme further. Scoped to the deferred ≥5-way path; 4-way is unaffected.

### 7.7 Retire `ImplementIndexScanRule` — unify on the data-access/`Compensation` path (RFC-045 follow-up)

Go reaches a physical index scan / filter via THREE producers that bypass `Compensation`: the
data-access/compensation match path (`predicate_multi_map.go`), the Go-only `ImplementIndexScanRule`
(a fusion of Java's `ImplementPhysicalScanRule` + candidate matching that iterates predicates
directly), and `ImplementFilterRule` (synthesizes a `RecordQueryPredicatesFilterPlan` over the inner
winner). Java has ONE path (`AbstractDataAccessRule` → `toEquivalentPlan`) and enforces "index-only
value can't be a residual" ONCE via `Compensation.isImpossible()`. Because Go's extra rules don't
route through `Compensation`, RFC-045 enforces the index-only compensatability guard at multiple
layers: `valueContainsUncompensatable` (match path) + the residual-skip loop in
`ImplementIndexScanRule.OnMatch` (implement-index path) + a final-plan validation
`validateNoIndexOnlyResidual` in `Planner.Plan` (the `ImplementFilterRule` leak can't be guarded at
the rule — removing its member collapses the filter Reference and breaks the data-access intersection
memo, so the leaking *final* plan is rejected with `UnplannableIndexOnlyResidualError` instead).
All are load-bearing and pinned (`TestVectorPlan_QualifyPlansToVectorScan`,
`TestImplementIndexScanRule_SkipsIndexOnlyResidual`, `TestVectorPlan_MetricMismatchDoesNotMatchVector`),
so there is **no live bug** — but the layering is a smell whose root is the duplicated paths. Root fix
(Graefe-endorsed): retire `ImplementIndexScanRule` and route `ImplementFilterRule`'s filter
implementation through a single data-access rule backed by `Compensation`, at which point the
implement-layer guard AND the final-plan validation delete themselves and the property is enforced
once, as in Java. See DIVERGENCES.md "ImplementIndexScanRule is a Go-only second index-scan path".
Not urgent.

### 7.6 Source-anchored field pull-up — retire `composeFieldOverJoinMerge` (RFC-044 follow-up)

Multi-source pull-up (pullup.go:71-78) produces **bare** `FieldValue`s with no source
anchoring, where Java produces `FieldAccessValue(QuantifiedObjectValue(alias), field)` to
disambiguate the owning quantifier. The Go-only `JoinMergeResultValue` +
`composeFieldOverJoinMerge` band-aid re-derives the anchoring after the fact and, absent
the side info, hard-codes the inner side. RFC-044 made that sound-by-invariant (the merge
is re-flowed under the inner alias, so only inner-side bare fields reach the rule) + added
a qualified-field fail-safe + an E2E sentinel (`TestFDB_JoinMerge_OuterColumn_NotDropped`),
so there is **no live bug**. The Java-aligned root fix is to anchor fields to their source
during pull-up so the opaque-merge ambiguity never exists, retiring the rule and the
bare/qualified distinction entirely. Graefe-endorsed; not urgent. Do this with (or before)
the typed join-result migration that replaces `JoinMergeResultValue` with a schema-bearing
record constructor à la Java.

---

## Phase 9: Vector / HNSW relational SQL parity (RFC-045)

**Context.** The record-layer / Cascades core of vector search is already ported and FDB-tested:
the HNSW graph (`hnsw.go`), the index maintainer (`vector_index_maintainer.go`), RaBitQ
quantization (`pkg/rabitq`), HNSW stats (`hnsw_stats.go`), `vec_math.go` / `fht_kac_rotator.go`,
chaos verification (`chaos/verify_vector.go`), and integration tests
(`vector_index_test.go`, `rabitq_test.go`, `hnsw_stats_test.go`, `bench/sift_benchmark_test.go`).
The Cascades *values* (`value_row_number.go` + `value_*_distance_row_number.go` seeds,
`value_row_number_high_order.go`), the match candidate (`vector_index_match_candidate.go`, 232 LOC),
and a `DistanceRank` comparison stub all exist. The SQL **grammar** is complete:
`vectorIndexDefinition` (`CREATE VECTOR INDEX … USING HNSW … PARTITION BY … OPTIONS(…)`),
`qualifyClause`, `overClause`, `windowSpec`, `nonAggregateWindowedFunction(ROW_NUMBER …)`.

**The gap = the relational front-end + Cascades wiring** (the "just not relational bits"):

**Status: DONE (RFC-045, Graefe+Torvalds ACK).** 9.1–9.4 all landed, tested, green. The full
SQL vector K-NN read path works end-to-end: a partitioned HNSW index +
`SELECT … WHERE <partition> QUALIFY ROW_NUMBER() OVER (PARTITION BY … ORDER BY
euclidean_distance(vec, q)) <= K` plans to a BY_DISTANCE vector index scan and executes
against real FDB returning the k nearest records (`TestFDB_VectorSearch_QualifyE2E`). Also
fixed a latent vector-scan PK-extraction bug. **Known follow-up:** an *unpartitioned* vector
index + WHERE-less QUALIFY does not yet match the candidate (Java's vector search is always
partitioned) — fails to plan rather than returning wrong results; revisit if needed.

- [x] **9.1 DDL: `CREATE VECTOR INDEX … USING HNSW … PARTITION BY … OPTIONS(…)`** → metadata vector
  `Index` (type `vector`, HNSW options). No `vectorIndexDefinition` handler exists in `pkg/relational`
  today. Wire-compat: the index metadata + on-disk HNSW format must match Java byte-for-byte (core
  already does; DDL must produce the same `Index` proto + options).
- [x] **9.2 Query front-end: `QUALIFY ROW_NUMBER() OVER (PARTITION BY … ORDER BY <distance>(vec, q)) <= K`.** Done — walk.go builds DistanceValue + RowNumberValue; predicates.TransformRowNumberDistanceRankMaybe ports transformComparisonMaybe; QUALIFY lowers to a DistanceRank ComparisonPredicate.
  No `qualifyClause` handling and no window-function→Value visitor exist (`grep QualifyClause` → 0 hits;
  `extractFunctionNameFromCall` only returns the *name* string). Build the distance-specialized
  `RowNumberValue` (Euclidean / Cosine / Dot-product / EuclideanSquare) from the parse tree, fleshing
  out the seed value classes; port `RowNumberValue.transformComparisonMaybe` so `ROW_NUMBER() <= K`
  rewrites into a `DistanceRankValueComparison(queryVector, k, efSearch, isReturningVectors)`.
- [x] **9.3 Cascades wiring + vector physical plan.** Done — (9.3a) tryVectorIndexCandidate enumerates the candidate + ExpandVectorIndex builds the distance placeholder + valuesMatchColumn matches it; (9.3b) ToScanPlan splits partition prefix from the DistanceRank binding; (9.3c) RecordQueryVectorIndexPlan + executeVectorIndexScan dispatch BY_DISTANCE; physicalVectorIndexScanWrapper + the index-only compensatability guard (valueContainsUncompensatable via values.IsIndexOnly on the match path + the residual-skip loop in ImplementIndexScanRule) make the vector scan the sole physical winner — the DistanceRank predicate, being index-only, is never lowered to a residual filter, exactly as Java's match-then-implement does. Three pieces (Torvalds catch — not a single
  branch): **(9.3a)** add a vector branch to the match-candidate enumeration (next to
  `NewValueIndexScanMatchCandidate` at `plan_context_builder.go:46` + the metadata-driven builder in
  the embedded layer) so a `vector`-type index yields the candidate; **(9.3b)** rework
  `VectorIndexScanMatchCandidate.ToScanPlan` (`vector_index_match_candidate.go:200`, today a generic
  `NewRecordQueryIndexPlan`) to split partition-equality `ComparisonRange`s from the single
  distance-rank comparison (which rides as an *equality-shaped* range, à la Java
  `toVectorIndexScanComparisons`); **(9.3c)** introduce a vector-aware physical plan that threads
  query-vector/k/`ef_search`/`isReturningVectors` and at execution dispatches **BY_DISTANCE** via
  `ScanIndexByType`/`ScanVectorIndex` → `ScanByDistance` (`index_scan.go:338-345`) — without it the
  plan does a BY_VALUE scan that errors at `index_scan.go:269`.
- [x] **9.4 E2E proof.** Done — `TestFDB_VectorSearch_QualifyE2E` (sqldriver, real FDB): builds a partitioned vector schema, inserts vectors, EXPLAIN-pins the BY_DISTANCE vector scan for the full QUALIFY SQL query, executes it, and asserts the top-2 nearest records. (yamsql port + `ef_search`/OR-of-two-KNN/`42F21`-in-WHERE coverage remain as nice-to-have follow-ups.) Original plan: Port Java's `window-function-documentation-queries.yamsql` (KNN top-K via
  `QUALIFY`, `ef_search` option, `<`/`<=`, OR-of-two-KNN) as the Go conformance/yamsql scenario, plus an
  FDB integration test that `EXPLAIN`-pins the vector index scan (not a full-scan fallback) and asserts
  row + distance correctness. Window-functions-in-`WHERE` must error (Java: `42F21`).

Constraints to mirror from Java's `VectorIndexScanMatchCandidate`: exactly one distance-rank per query;
the index MUST be partitioned and the query MUST supply partition keys; the SQL distance fn MUST match the
index `metric`; ORDER BY must be ascending; `ROW_NUMBER()` is INDEX-ONLY (refuse without a matching index).
`@API(EXPERIMENTAL)` in Java — landed Jan–Mar 2026, just before the 4.11.1.0 tag.

- [x] **9.5 Multi-partition vector scan (partial partition prefix).** Done in RFC-046 — `vectorMultiPartitionCursor` ports Java's `flatMapPipelined(prefixSkipScan, scanSinglePartition)`: `findNextPartition` skip-scans the distinct partition prefixes, `searchOnePartition` runs one HNSW search per partition, per-partition top-K concatenated, full cross-partition `FlatMapContinuation` resume. Planner: `ComputeBoundParameterPrefixMap` keeps the equality prefix + always the DistanceRank binding (no nil-query-vector on a partial prefix); `parametersRequiredForBinding={distanceAlias}` (the full-prefix guard dropped, matching Java's `VectorIndexExpansionVisitor`). Partition inequality left unconsumed → residual (documented; endpoint-into-skip-scan is a perf follow-up). Graefe+Torvalds ACK. Pinned by `TestVectorPlan_PartialPrefixPlansMultiPartition`, `TestVectorPlan_PartitionInequalityNotConsumedIntoPrefix`, FDB E2E `TestFDB_VectorSearch_MultiPartition_{Fanout,InequalityResidual,Pagination}`. DIVERGENCES.md "Vector scan multi-partition" closed.

## Native fdbgo client — conformance & differential testing (RFC-010 Phase 1+)

RFC-010 Phase 0 (the wire-correctness fires: #1 inline reply error, #2 wrong_shard_server code,
#3 pipelined retry, #5 hedge queue-model leak, #8 ErrorOr union parse) landed. These three items
close the testing/conformance gaps its prevention plan (P5/P7) calls for.

### RFC-010 audit findings (the original 15 — correctness fires)

The execution list for the Codex source audit (`TODO_client.md`); full detail + C-conformance
reasoning per item in `rfcs/010-native-client-correctness.md`. **12 landed, 2 open, 1 false positive.**
Each open item is gated by *why it isn't trivially done*, not by another item.

- [x] **#1** inline `LoadBalancedReply.error` decoded on read parsers (Phase 0)
- [x] **#2** `ErrWrongShardServer` 1062 → 1001 + anti-self-confirming fault test (Phase 0)
- [x] **#3** pipelined `Get` shares full classify→invalidate→retry; 1006 surfaced correctly (Phase 0)
- [x] **#4** tenant commit builder uses a scratch `[]MutationRef` — no in-place mutation of `tx.mutations`, no double-prefix on rebuild (build-twice regression; Torvalds + FDB-C ACK)
- [x] **#5** hedge loser/timeout/cancel QueueModel deltas released (Phase 0)
- [x] **#6 — HIGH.** Conn shutdown — fixed in RFC-050. One `failConnection(err)` path (`sync.Once`: cancel ctx + close socket + `failAllPending`) is the single teardown, used by `Close`, `connectionMonitor` death, and `readLoop` read errors. **(1)** `SendFrame`/`Flush` now wait on `errCh` **or** `ctx.Done()` (and deliberately don't pool `errCh` on the `ctx.Done()` path — audit #13 stale-value hazard), so a sender whose frame is still queued when `writeLoop` exits no longer hangs forever. **(2)** `connectionMonitor` death now calls `failConnection` — adding the missing `conn.Close()` that unblocks `readLoop`'s blocking `Read` (the old bare `cancel()` leaked the fd + goroutine until the 10 s TCP keepalive). Single-delivery to a pending reply still comes from the pending-map + `pendingMu` + delete-as-you-go; `closeOnce` only guarantees the meaningful error wins. SimTransport scope: built the in-process `net.Pipe` fake-server test harness #6 needs (handshake + stall / go-silent / abrupt-close modes) and made the monitor cadence injectable (unexported `withMonitorCadence` on an unexported `dialWith`; public signatures unchanged); the full seeded multi-mode SimTransport is deferred to C4 (YAGNI). 6 deterministic in-process `-race` tests (the two core ones verified failing on the pre-fix code: stranded-sender hang + monitor-no-socket-close leak). FDB-C + Torvalds ACK.
- [x] **#7 — MEDIUM.** Honor the "methods safe for concurrent use" contract — fixed in RFC-049. Writers already appended under `conflictMu`; the unprotected readers/clears now do too: `Commit` validation + read-only check snapshot `mutations`/`len(writeConflicts)` under the lock and **thread that validated snapshot into the marshal** (so a `Set` racing `Commit` can't ship an *unvalidated* mutation to the proxy — FDB-C catch); `buildCommitTransactionRequest`/`commitDummyTransaction` snapshot the conflict headers under the lock (append-only + `conflictBuf`-only-grows ⇒ snapshot-and-release is race-free for them); `GetApproximateSize` iterates **under** the lock (not a released snapshot — it can race `Commit`'s in-contract auto-reset, which `[:0]`-reuses the backing arrays); `mutations[:0]` clears moved inside `conflictMu` in reset/postCommitReset; `addWriteConflict*` moved the `nextWriteNoConflict`/`writeConflictsDisabled` gate inside the lock (the one-shot flag is read+cleared on the `Set` path → two concurrent `Set`s raced). `Set`/`Clear`/`ClearRange`/`Atomic` now publish the mutation + its write-conflict range **atomically** under one `conflictMu` acquisition (codex catch — the old two-lock split let a `Commit` snapshot ship a mutation *without* its conflict range → a missed conflict; this also subsumes the `nextWriteNoConflict` fix and drops `Set` from two locks to one). Contract doc narrowed: option setters (`SetXxx`) + `Reset` are configure-before-use, not concurrent-safe (matches `fdb_transaction_set_option`); RYW lost-update stays documented-not-safe. 6 deterministic concurrency tests (verified failing on the pre-fix code) + tenant no-alias sentinel + validated-snapshot pin + Set-atomicity invariant. FDB-C + Torvalds + codex review.
- [x] **#8** `ReadErrorOr` parses the union tag (not field count); error code uint16 (Phase 0)
- [x] **#9** rename `isSystemKey` → `isSpecialKey` (tests `\xff\xff` special-key space; behavior unchanged)
- [x] **#10** decoupled `ACCESS_SYSTEM_KEYS` from `LOCK_AWARE` in `fdb/options.go` (C sets them
  independently — confirmed NativeAPI 7159 / RYW 2557 / TenantManagement). Facade no longer
  auto-sets lock-aware; each `fdb/database.go` tenant call site sets the exact C++ options (writes
  ACCESS+LOCK_AWARE; OpenTenant READ_SYSTEM_KEYS+READ_LOCK_AWARE; ListTenants
  READ_SYSTEM_KEYS+LOCK_AWARE). Behavior change: external callers
  relying on the implicit coupling must set `SetLockAware` explicitly (as a Java/CGo app must) — only
  observable on a *locked* DB; wire-safe (lock-aware is a commit flag, not persisted bytes).
  Pinned by `TestSetAccessSystemKeys_DoesNotImplyLockAware` (facade unit test, fails if the coupling returns).
- [x] **#11 — MEDIUM.** TLS wired end-to-end — fixed in RFC-051. `ParseClusterString` parses the `:tls` coordinator suffix (faithful to C++ `NetworkAddress::parse`: strip `(fromHostname)` then `:tls` when len>4; uniform-cluster, mixed rejected) → `ClusterFile.UseTLS`; `database` carries `tlsConfig *tls.Config` and `getOrDialConn` dials TLS; `resolveTLSConfig` loads `FDB_TLS_{CERTIFICATE,KEY,CA}_FILE` (→ `/etc/foundationdb/{cert,key}.pem` default) into a standard config, C++-precedence-faithful. **Go-idiomatic user-facing API (bradfitz review):** `transport.Dial(ctx, addr, *tls.Config, dialFn)` — the non-nil config is the *only* "use TLS" signal (nil = plaintext), so the silent-downgrade footgun is gone by construction (the `useTLS bool` + `DialWith`/`DialWithTLS` overloads + bespoke `transport.TLSConfig` are deleted); `fdb.OpenDatabase(clusterFile, WithTLSConfig(*tls.Config), WithDialFunc(...))` functional options, precedence explicit > `FDB_TLS_*` env > default; `upgradeTLS` clones + fills `ServerName`/`MinVersion` only if unset. 6 deterministic tests incl. a real in-process mutual-TLS handshake (FDB ConnectPacket inside the tunnel) + wrong-CA/missing-client-cert rejects. FDB-C + Torvalds + bradfitz ACK. Follow-ups: per-address TLS flag (dual-listen), `FDB_TLS_VERIFY_PEERS` rule DSL, `FDB_TLS_PASSWORD`/encrypted keys, FDB-TLS testcontainer e2e.
- [ ] **#13 — LOW (concurrency-sensitive).** Reply channel never returned to the pool on the success path (both `PrepareReply`/`Release` and `Send`/`SendAndWait`) — hot-path alloc, and the line-23 "readLoop returns it after dispatch" comment is false. Fix must return the channel **only when no send can race** (success: caller received; Cancel: only when it won the `delete`) to avoid a stale-buffered-value bug on the timeout/delivery race; leave the rare race-loser leaked. Best validated with `SimTransport`.
- [x] **#14 — LOW.** Monitor ping on a saturated `writeCh` — fixed in RFC-052. The send was already non-blocking (`select … default`), but the drop path returned a **closed** `done`, which the monitor read as `case <-replyCh:` "PING reply arrived → connection alive" — so a *stuck* connection (writeLoop blocked on an undrained socket ⇒ `writeCh` saturates) falsely passed as alive and never reached the `bytesReceived` liveness check (the one state where the monitor must act). Fix: the drop path returns **nil** (never selected in the monitor's `select`) so it falls through to the timer → `bytesReceived` kill — faithful to C++ `connectionMonitor`, whose liveness verdict is solely bytes-received (the ping-reply arm only restarts the cycle; C++ `Peer::send` is an unbounded buffer with no "couldn't send" path). Pinned by `TestSendPingWithReply_DropsToNilOnFullWriteCh` (verified failing on the pre-fix closed-`done`); the sent-path kill stays covered by `TestConn_MonitorDeathClosesSocket`. FDB-C + Torvalds ACK.
- [x] **#15** range-iterator next-begin via `keyAfter` helper that copies (no alias/scribble of `lastKey`); spare-capacity unit pin
- **#12 — FALSE POSITIVE.** Locality never panics (invariant guarantees non-empty); add a defensive guard at most.

We **cannot** run FDB's deterministic simulation: Sim2 is a hermetic single-threaded Flow event
loop with an in-memory network and no external socket, so a real Go client can't join it, and
server-side BUGGIFY edge-case injection exists only inside Sim2. But three of FDB's real,
externally-usable artifacts CAN be exercised against a testcontainer cluster our Go client
mutated. (Determinism for our OWN retry/LB/wire-error paths — `PendingGet.Resolve`'s
flush/transport/timeout arms, the codex 1006 drop-between-dial-and-send race, transparent
wrong-shard retry — comes from a seeded in-process `SimTransport` fake server behind
`transport.DialFunc`, extending the existing `wrongShardConn`; tracked as a separate Phase-1 item.)

- [x] **C1. Ride their oracle — FDB `ConsistencyCheck` after Go-client writes.** DONE
  (`pkg/fdbgo/conformance/consistencycheck_test.go`). `RunCluster(3, double, ssd)` →
  pure-Go client writes 1000 keys → wait replication-healthy → run FDB's one-shot
  `fdbserver -r consistencycheck` role → parse its JSON trace and assert it completed
  (`ConsistencyCheck_FinishedCheck`), examined data, and emitted **no** Severity-40
  inconsistency/`TestFailure` event. **Double redundancy is required** — under single
  redundancy the checker's replica comparison is a no-op (one copy per shard). Anti-vacuity:
  require `GetKeyValuesStream` reads (one per replica per shard) **>** `FirstValidServer`
  baselines (one per shard) — i.e. some single shard was read on ≥2 replicas, which a bare
  "≥2 reads total" count can't prove (N single-replica shards defeat it). `FirstValidServer`/
  `CheckCustomReplica` fire even under single redundancy and do NOT prove a comparison. The
  process exit code is unreliable (exits 0 even on inconsistency), so detection is by trace
  event: any Sev40 `ConsistencyCheck_*` (catch-all), the SevInfo `InconsistentStorageMetrics`,
  and Sev40 `TestFailure` reasons containing "inconsistent". Detection logic pinned by a
  deterministic unit test (`TestParseConsistencyTrace`) since the live run is always clean.

- [x] **C2. Ride their client — differential vs the official C binding (`libfdb_c`).** Landed in
  **RFC-053 (PR #231)**. Differential harness in `pkg/fdbgo/bench` (reuses the dual-client fixture):
  L2 write battery (byte-identical persisted state — Set shapes incl. exactly-VALUE_SIZE_LIMIT, every
  atomic on a missing key pinning the Min→MinV2/And→AndV2 upgrade, SetVersionstampedValue offset,
  key-at-KEY_SIZE_LIMIT boundary) and L3 read parity (GetRange chunking-invariance across
  StreamingModes/limits/reverse + GetKey selector parity, read-version-pinned). Proven to have teeth
  (reverting Min→MinV2 fails it byte-exactly). **Surfaced & fixed FOUR real client divergences**, each
  pinned with a fail-pre-fix test: SetVersionstampedKey spurious write-conflict range; client-side
  key/value size-limit enforcement (set/atomic reject at commit, clear clamps/drops); raw-access key
  limit set by ACCESS_SYSTEM_KEYS/READ_SYSTEM_KEYS (not just RAW_ACCESS); raw-access slack gated off
  for tenant txns. Reviewed by FDB-C-dev + Torvalds + codex (3 P2s) + @claude.
  **Follow-up RFC-054: `FuzzDifferential`** — random op sequences through both clients,
  byte-identical persisted state (RYW coalescing, atomic accumulation, clear/overwrite
  ordering); 40s burst = 8068 execs, 0 mismatches.
  **Follow-up RFC-055: RYW-read differential (Get/GetRange)** — found+fixed a getRange
  merge bug that dropped empty-value pending keys.
  <details><summary>original spec</summary>
  The C
  binding is the client FDB simulation-tests on every CI run, so matching it is the closest we get
  to inheriting that coverage (RFC-010 prevention P5, corrected). Run the SAME operations through
  our Go client and `libfdb_c` against the same testcontainer cluster. **CRITICAL: compare at the
  DATA plane, never the wire.** Request frames are legitimately NOT byte-identical — reply-promise
  UIDs, read/committed versions, trace/span IDs, GRV batching, mutation/conflict ordering, and
  range chunk boundaries all vary per client. So:
    - **Writes → byte-exact on PERSISTED bytes.** Write the same logical mutation via each client,
      read the raw keys/values back out of FDB, assert byte-identical: key/tuple encoding, value &
      record format, index entries, version at `pk+\xff`, split chunking, continuation-token bytes
      + magic `6773487359078157740`. This is the cross-client compatibility hard line — where
      byte-identity is both *required* (Java/Go share a cluster) and *achievable* (the persisted
      format is spec-fixed; control-plane randomness never touches it).
    - **Reads → semantic, control-plane excluded.** Same key/range + a pinned read version →
      compare returned value / merged KV set + order / error CODE (not message). Ignore reply
      tokens; don't compare the literal version number (compare the data it produced); merge range
      chunks before comparing. Under deliberate concurrency, compare error CLASSES, not exact codes.
    - **Continuations → mutually resumable** (a Go-produced continuation resumes correctly when fed
      back; byte-equal where the format is fully spec-pinned). Any *data-plane* byte difference is a
      real wire-compat bug, NOT a tolerance to normalize away.
  </details>

- [ ] **C2-followup. RYW key-selector + read-version correctness audit (RFC-056).** Remaining
  RYW read-resolution divergences from libfdb_c surfaced by the RFC-055 differential:
  (1) `Transaction.GetKey` resolves selectors against storage ONLY and does not merge pending
  writes — needs a faithful port of C++ `resolveKeySelectorFromCache` (removeOrEqual + offset
  walk over the merged write-map via a RYWIterator segment model; a merged-GetRange shortcut
  was tried and verified-WRONG on `{orEqual, offset>1}`); (2) a possible go-vs-cgo read-version
  staleness asymmetry (one differential run showed go=`transaction_too_old(1007)` while cgo
  succeeded on the SAME pinned read version near the 5s MVCC edge — reproduce + compare go vs
  cgo SetReadVersion/window handling; may be a real divergence or a test-pin robustness issue).
  Land the held `FuzzRYWRead` (Get/GetRange/GetKey + atomics) to drive these. See rfcs/055
  "Findings & scope".
  - [x] **RYW applyAtomic on present-empty values** — FIXED: the chain conflated `nil` (absent)
    with present-empty, so a V2 op after `Xor(k,"")` took the absent→operand path (`Min(k,"0")`
    → 0x30 vs libfdb_c 0x00). The get/merge chains now keep present-empty non-nil (nil reserved
    for absent), mirroring C++ `Optional.present()`. Pinned by
    `TestRYWGetRange_V2AtomicOnPresentEmpty`.
  - (3) **versionstamped-pending read = unreadable.** A SetVersionstampedKey/Value pending on a
    key reads as ABSENT in Go pre-commit (Get→nil, GetRange→omit); C++ marks it `is_unreadable`
    and THROWS `accessed_unreadable`. Go has no unreadable state — approximated as absent
    (consistent across Get/GetRange, no phantom; pinned by `TestRYW_VersionstampedAbsentNoPhantom`).
    Full C++ parity (throw on read) needs an unreadable concept — part of the RFC-056 audit.

- [ ] **C3. Ride their test designs — port FDB workloads as scenario + invariant specs.** FDB's
  `fdbserver/workloads/*.actor.cpp` (Cycle, AtomicOps, ConflictRange, Serializability,
  FuzzApiCorrectness, …) are unrunnable for us (Sim2-only), but each scenario + invariant is
  language-agnostic. Port the adversarial designs — e.g. Cycle: maintain a ring of pointer K/Vs,
  hammer it concurrently (+faults), verify the ring stays unbroken — to drive our client against
  testcontainers (and later `SimTransport`). Reimplement the harness; reuse the proven scenarios.
  Extends the existing `pkg/recordlayer/chaos` model-based approach + `cmd/fdb-binding-stress`.

- [ ] **C4. Deferred Phase-0 test gaps (need `SimTransport` / faithful inline-error injection).**
  A coverage audit (codex) found these error/edge dimensions; the cheap deterministic ones were
  closed inline, these need infra and fold into the `SimTransport` build:
    - **Inline `LoadBalancedReply.error` on the `parseGetKeyReply` / `parseGetKeyValuesReply`
      parsers specifically.** The decode helper (`wire.ReadInlineReplyError`) and the slot
      constants are unit-pinned, but no test feeds those two parsers a *faithful* reply carrying an
      inline error, because the generated writer mis-marshals `Optional<Error>` (as length-prefixed
      bytes, not a nested Error table) — and the current fault harness injects ROOT `ErrorOr` errors,
      whereas real FDB delivers read wrong-shard via the INLINE field. `SimTransport` (or a
      hand-built fixture) must emit a correct nested-Error inline reply.
    - **`PendingGet.Resolve` flush-error arm** (needs a conn whose `Flush()` errors).
    - **Range wrong-shard across a partial continuation / `more=true`** (inject `1001` on the 2nd
      `GetKeyValues` frame mid-scan; assert no dup/drop, correct `more`; forward + reverse).
    - **`future_version` (1009) / `process_behind` (1037) read-path QueueModel backoff wiring**
      (assert `failedUntil` advances and the address is deprioritized).
