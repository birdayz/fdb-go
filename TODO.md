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
- [ ] **GROUP BY/HAVING in correlated scalar subqueries.** Requires PredicatePushDownRule to treat GroupByExpression as a barrier (AliasMap.Compose conflict on correlation alias). Per Graefe: runtime cardinality assertions incompatible with continuation-based pagination — use Java's FirstOrDefault pattern.
- [ ] **PlanVisitor resolver lacks SubqueryPlanner.** Scalar subquery error messages don't propagate through step (2) projection resolution because the resolver has no SubqueryPlanner. workaround: errors propagate through upgradeProjectionValues (step 12) instead.
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

Go reaches a physical index scan via two paths: the data-access/compensation match path
(`predicate_multi_map.go`) and the Go-only `ImplementIndexScanRule` (a fusion of Java's
`ImplementPhysicalScanRule` + candidate matching that iterates predicates directly, bypassing
`Compensation`). Java has ONE path (`AbstractDataAccessRule` → `toEquivalentPlan`) and enforces
"index-only value can't be a residual" ONCE via `Compensation.isImpossible()`. Because Go's
implement rule doesn't route through `Compensation`, RFC-045 had to apply the index-only
compensatability guard at BOTH layers (`valueContainsUncompensatable` + the residual-skip loop in
`ImplementIndexScanRule.OnMatch`). Both are load-bearing and pinned (`TestVectorPlan_QualifyPlansToVectorScan`,
`TestImplementIndexScanRule_SkipsIndexOnlyResidual`), so there is **no live bug** — but the
duplication is a smell whose root is the duplicated path. Root fix (Graefe-endorsed): retire
`ImplementIndexScanRule` so the single data-access rule routes through `Compensation`, at which
point the implement-layer guard deletes itself and the property is enforced once, as in Java.
See DIVERGENCES.md "ImplementIndexScanRule is a Go-only second index-scan path". Not urgent.

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
- [x] **9.3 Cascades wiring + vector physical plan.** Done — (9.3a) tryVectorIndexCandidate enumerates the candidate + ExpandVectorIndex builds the distance placeholder + valuesMatchColumn matches it; (9.3b) ToScanPlan splits partition prefix from the DistanceRank binding; (9.3c) RecordQueryVectorIndexPlan + executeVectorIndexScan dispatch BY_DISTANCE; physicalVectorIndexScanWrapper + a cost criterion rejecting residual index-only DistanceRank make the vector scan win. Three pieces (Torvalds catch — not a single
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
