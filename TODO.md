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
- [x] **🚩 IN over an indexed column drops the outer projection (wrong result schema).** Fixed in **RFC-070**. Root cause was two defects: (1) `MergeProjectionAndFetchRule`'s fallback dropped the projection when the fetch's child was an InJoin (not a coverable index scan), leaking a bare `InJoin` ([ID,A]) into the root projection group where it won on cost; (2) `physicalProjectionWrapper`/`physicalFetchFromPartialRecordWrapper` `WithChildren` didn't relink a compound-join inner during extraction (left `Project([id], InJoin(<nil>))` / `Fetch(<nil>)`), because of an `isLeafReplaceable` gate — same gate RFC-069 removed from the in-memory sort wrapper. Fix: fallback retains the projection; the two transparent caps relink unconditionally. `SELECT id FROM t WHERE a IN (1,7)` → `Project([ID], InJoin(IndexScan(IDX_A,[=])))`; `SELECT id+100 ...` (was 0 rows) → `{101,107}`. Pinned by `TestFDB_INProj_OuterProjectionOverInJoin` (indexed+unindexed, multi-column, expression-projection, 8× determinism). Graefe+Torvalds ACK.
  - [ ] **Follow-up (RFC-070): `pushValue`-into-covering-result-value modeling gap.** Java's `MergeProjectionAndFetchRule` yields a bare `fetchPlan.getChild()` because `RecordQueryFetchFromPartialRecordPlan.pushValue` rewrites the projected value into the covering plan's own result value. Go's `WithCovering` only sets a flag (the scan still flows the full partial record), so Go compensates with a thin outer `Project`. Pushing the value into the covering result value would let both rule branches collapse to a bare child yield, matching Java. Cosmetic/architectural — current behaviour is correct.
  - [ ] **Follow-up (RFC-070): other transparent unary wrappers over joins.** `Map`, `Distinct`, `Limit`, `TypeFilter`, `FirstOrDefault`, `DefaultOnEmpty` still gate `WithChildren` on `isLeafReplaceable` and could exhibit the same nil-inner-over-join bug if a rule ever builds them with a placeholder inner over a join. Not currently reachable via SQL (projections route through `LogicalProjectionExpression`, not `Map`); the **blanket** gate removal is unsafe — it regressed `TestFDB_AggregateIndexUsage` by dropping the eq-filter on aggregation/DML wrappers (which embed filter semantics in their own plan). Each wrapper needs individual analysis if/when reachable.
- [x] **DML does not execute through Cascades (parallel pipeline).** Fixed as **P0.4** — all DML now executes through Cascades (`planDML`); the naive `execStatement` DML path is deleted. See P0.4.
- [x] **🚩 TODO 7.6-union-remap — aggregate UNION branch with a mismatched output alias drops rows (pre-existing executor gap).** Fixed for STREAMING aggregates in **RFC-078**: (1) `executeUnorderedUnion` (executor_new_plans.go) now remaps later branches' columns to the first branch's names by position — it previously concatenated branch cursors with NO normalization at all (unlike the ordered `RecordQueryUnionPlan`/`executeUnionStreaming`); (2) `planColumnNamesWithMD` (executor.go) reports a `RecordQueryStreamingAggregationPlan`'s output names (group keys + alias-or-canonical) instead of descending through `GetInner()` to the input scan. `SELECT u.x FROM (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b) u` now returns both counts (was `[2, NULL]`). Pinned by `TestFDB_UnionAggregateColumnRemap`. Graefe + Torvalds ACK.
  - [x] **Follow-up (RFC-078) c — FIXED in RFC-080: re-enable the union-as-join-leg / derived-table aggregate case for UNGROUPED aggregates.** The gate's `LogicalAggregate` case is hit only by a *bare* aggregate branch (no Project). Graefe's review caught that a bare aggregate can be GROUPED (an unaliased, all-visible `SELECT g, COUNT(*) FROM t GROUP BY g` skips `buildSelectShell`'s stripping Project). Only the UNGROUPED sub-shape is safe to normalize: an ungrouped aggregate produces **no** aggregate-index candidate (`tryAggregateIndexCandidate` returns nil when `groupingCount == 0`, `cascades_generator.go`), so it always plans as StreamingAgg, which flows every aggregate under its alias (RFC-078). So `unionBranchNormalizable`'s `LogicalAggregate` arm relaxed from `false` to `len(Aggregates) >= 1 && len(GroupKeys) == 0`. `TestFDB_UnionJoinLeg` case (3) flipped clean-error→correct-rows. Pinned by `TestFDB_UnionScalarAggregateAlias` (single + multi ungrouped unions read by name + no-AggregateIndex invariant), `TestFDB_UnionGroupedAggregateStillGated` (grouped union, which DOES plan as AggregateIndex, stays gated), `TestUnionBranchNormalizable_AggregateArity`. plandiff byte-identical. Graefe + Torvalds ACK.
    - [x] **Follow-up (a) — GROUPED bare aggregate union by name — FIXED in RFC-081.** A bare GROUPED aggregate union branch (`SELECT g, COUNT(*) FROM a GROUP BY g UNION ALL …` read by name) plans as `AggregateIndex` (single agg) or `MultiIntersection`/`StreamingAgg` (multi agg). The fix was *reporting*, not cursor changes: the AggregateIndex and MultiIntersection cursors already write rows keyed by their output names (group cols + canonical aggregate name; a bare aggregate is always unaliased, so no alias to carry). Added `RecordQueryAggregateIndexPlan.OutputColumnNames()` + `planColumnNamesWithMD` arms for AggregateIndex (group cols + `CanonicalAggColumnName`) and MultiIntersection (result-value field names, verbatim), then dropped the `len(GroupKeys) == 0` clause → gate is now `len(Aggregates) >= 1`. `TestFDB_UnionGroupedAggregate` (single + multi grouped union join legs, mismatched group-key names → correct rows; EXPLAIN-pins AggregateIndex), `TestPlanColumnNames_{AggregateIndexReportsOutputSchema,MultiIntersectionReportsResultValueNames}`, `TestAggregateIndexPlan_OutputColumnNames`, gate unit test grouped→true. plandiff byte-identical. Graefe + Torvalds ACK.
      - [ ] **Sub-follow-up (codex): DIVERGENT-NAMED aggregate union branches.** A bare aggregate whose output name differs between the logical leg schema (`aggregateOutputColumns`, raw text) and the physical row key (`aggResultName`/AggregateIndex canonical) NULLs when union-remapped by name. Divergent forms: qualified operand (`SUM(t.c)`→`SUM(C)`), constant (`COUNT(1)`/`COUNT(NULL)`→`COUNT(*)`), expression (`SUM(a*b)`), DISTINCT. RFC-081 GATES all of them via `aggregateNamesStableForUnion` (whitelist `COUNT(*)`/`FUNC(bare-col)`; clean error, `TestFDB_UnionQualifiedAggregateGated` + `TestFDB_UnionGroupedCountConstantGated`). To OPEN them: unify aggregate output naming so the logical schema and the physical row key agree for every form (strip qualifier consistently + reconcile count-star normalization between StreamingAgg and AggregateIndex), then relax the whitelist. NOTE: a separate pre-existing bug — `SELECT u.*` star-expansion over an aggregate union join leg mis-derives the aggregate column name (NULL) even for ALIASED aggregates (Project-topped) — is orthogonal to the gate and also needs fixing.
  - [x] **(b) ordered-union projection-alias — FIXED in RFC-079.** A UNION branch projecting a post-aggregate EXPRESSION with an alias (`SELECT COUNT(*)+1 AS x FROM a UNION ALL SELECT COUNT(*)+1 AS y FROM b`, read by name) returned `[NULL,NULL]` — the legacy `buildSelectShell` builder (the UNION-branch path) built the post-agg projection with `nil` aliases, dropping the `AS x`. Fixed by extracting the projection-building loop into one shared `buildPostAggregateProjection` helper called by both `visitSelectGroupBy` (modern) and `buildSelectShell` (legacy) — one source of alias truth. Pinned by `TestFDB_UnionAggregateExprAlias` + `TestBuildLogicalPlan_PostAggExprAlias_CarriesAlias`. Modern path plandiff byte-identical. Graefe + Torvalds ACK.
  - [ ] **Cleanup (RFC-079 follow-up b): unify the SimpleTable logical builder onto `visitSelectGroupBy`.** The "one query path" endgame (CLAUDE.md "no parallel pipelines"). `buildSelectShell`/`buildLogicalPlanForSelect` is a second SELECT builder reached by plain-table SELECTs, derived tables, AND UNION branches; it has repeatedly drifted from the modern `visitSelectGroupBy` (the RFC-079 alias bug was one such drift). Route ALL of its callers through `PlanVisitor.visitSelectGroupBy` and delete the legacy builder. Larger than a single-bug fix (multiple callers, full regression surface) — Graefe's condition: must unify the WHOLE SimpleTable builder, not graft a special case onto the union entry.

### Beyond Java (Go-only improvements)

- [x] **Full Graefe Memo with cross-group merging.** Done in RFC-037 — union-find group merging (the Cascades-paper "merge two groups discovered to be one", §2 + §3.5), a Go-only extension beyond Java (which, like the pre-RFC Go memo, only interns at insertion time). `Reference` gains a monotonic `id` + `forwardedTo` + path-compressed `Canonical()`; every state-bearing method resolves the receiver to canonical, so a merged-away (loser) Reference transparently forwards — no in-flight task, Quantifier, or binding is rewritten. `GetRangesOver()` resolves at the single chokepoint (444 sites). `Memo.Integrate` hooks the REWRITING yield path: when a yielded expression equals a member of a different group, the two merge (survivor = lower id, deterministic), folding members + exploration state, repointing the topology index, invalidating correlation caches up the DAG, and recursively re-merging parents (paper's bottom-up recursion). Scoped to REWRITING (PLANNING winners/partial-matches embed raw References — guarded by `mergeable`); ancestor/descendant (idempotence) merges skipped to avoid DAG cycles. Wire compat untouched (read-path-only sharing). Merge fires through the real planner (`TestMemoMerge_FiresThroughRealPlanner`); 9 merge unit tests + determinism 50×/10×; 46/46 targets green; stress-1M unchanged. Graefe+Torvalds ACK (NAK'd v1 on in-flight-task stranding + cache staleness + index repoint + upward re-merge — all fixed in v2). **Reach caveat (honest):** the merge is correct and fires, but its practical reach is narrow today — the memo's interning/equivalence is alias-sensitive, and rule-rewritten equivalents mint fresh quantifier aliases, so equivalent sub-expressions intern to *different* child References and rarely surface as merge candidates (measured: exactly one merge on a K-branch equivalent UNION regardless of K; ~2% planner-time delta; no execution-time effect — same plan). Broad merging (and any real speedup / multi-way-join-order benefit) is **gated on alias-namespace unification (item 7.1 below)**; this PR lands the correct merge *infrastructure*, not a present-day perf win. Remaining (Future Work): **alias-normalized equivalence (7.1) — the lever**; reduction-rule-triggered merges (§3.6); PLANNING-phase merging; cost-model exploitation of shared sub-products for full N-way join-order optimality.
  - **PR-A landed the lever (RFC-038 epic / RFC-039 + RFC-040).** The reach caveat is now closed: the memo's structural-equivalence compare sites use alias-aware `expressions.MemoEqual` (faithful port of Java `Reference.isMemoizedExpression`) on top of the RFC-040 foundation (alias-aware `EqualsWithoutChildren` + alias-invariant `HashCodeWithoutChildren`). Rule-rewritten equivalents that differ only in fresh quantifier aliases now intern/merge into the SAME Reference — proven by `memo_activation_test` (K=6 alias-variant filters → 1 shared Reference, was K distinct). Zero plan-shape regression (plandiff conformance green), 10/10 deterministic, stress-1M before/after within noise. Graefe+Torvalds ACK. Still ahead in the epic: **PR-C** join-order enumeration (associativity/commutativity, capped) and **PR-D** cost selection + the e2e "multi-way join ordering proven" test (N-table join, EXPLAIN-pinned optimal order ≠ FROM-order, shared sub-products merged).
  - **PR-C scope corrected (RFC-074).** PR-C was framed as "efficient ≥5-way enumeration via sub-product interning (collapse the dual merge values)." **Measurement falsified the premise:** collapsing `JoinMergeResultValue`/`JoinMergeAllValue` to one canonical type does NOT reduce `distinctRefs`/`tasksRun` (N=5 stays 127k tasks / 171 refs) — the duality is a ~2× constant, not the exponential. The exponential is that logically-equivalent join sub-products land in SEPARATE memo References (even identical scans fork ×3) and never coalesce: `mergeable` (memo_merge.go:84) refuses once a group `HasWinnersOrMatches`, and `OptimizeGroup` interleaves `SetWinner` with `Integrate` yields, so a group holds a winner before its equivalent twin is born. RFC-074 now ships ONLY the **merge-value collapse** — a correct Go-only-divergence removal + prerequisite for single-type interning, **behavior-preserving (NOT a budget fix)**. Graefe re-ruled.
  - **PR-C2 — the actual ≥5-way budget fix (NEW, separate RFC).** Java does NOT solve the blowup by merging-under-winners (RFC-037 broad merge is a Go-only extension Java lacks); it **bounds the bipartition lattice at the source** via `shouldDeferCrossProducts` + `shouldJoinRightDeep` (Java `PartitionSelectRule.java:92,122`) and builds legs in a canonical interning-friendly form. Port/enable that pruning into `rule_partition_select.go` (the hooks exist — `ShouldJoinRightDeep`/`ShouldDeferCrossProducts` — verify defaults + why a pure connected chain isn't bounded). 1:1 Java-aligned. Do NOT decouple exploration from optimization (Java interleaves identically) and do NOT extend broad-merge-under-winners. Graefe-ruled.
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
- [x] **P2.3 Intersection cursors don't resume mid-stream (codebase-wide).** Fixed in **RFC-071**. `DecodeIntersectionContinuation` (exact inverse of `buildIntersectionContinuation`) splits the per-child `IntersectionContinuation` proto into START/MID/END resume states; `executeIntersection` and `executeMultiIntersection` create each child cursor accordingly (fresh / resume-from-bytes / `Empty`) via the shared `buildIntersectionChildCursors`, then use `IntersectionResume`/`IntersectionMultiResume`. `started` is now tracked **per child** in `mergeChildState` (matching Java's `KeyedMergeCursorState`, not derived from cursor-level state) and seeded from the decode, so a resumed mid-stream child can't be re-encoded as START. The loud guard is dropped. Also fixed a latent continuation-timing bug the paged test caught: both cursors captured the continuation *after* the post-match advance, losing every other match on resume (`[2,4,6]`→`[2,6]`) — now captured before. Pinned by white-box paged-resume tests (no dup/loss, asymmetric exhaustion, no-common, 3-child, both cursors) + decode round-trip/error/nil tests in `intersection_resume_test.go`. Graefe+Torvalds+@claude+codex ACK (v1 NAK'd — Graefe caught a limit-before-first-row child silently terminating the intersection + held-match loss on out-of-band stops, driving the full Java `MergeCursorState` consume-model port; @claude caught `intersectionMultiCursor` returning bare END on a limit instead of checkpointing; codex caught a decode child-count validation gap for 1/2-child tokens). Surfaced by @claude + codex on PR #249; landed as PR #252.
  - [ ] **Follow-up (RFC-071, Go-only optimization beyond Java): skip re-scanning discarded non-matching rows on intersection resume.** Because the cached per-child continuation sits at the last *consumed* (matched) position (faithful to Java `MergeCursorState`), an out-of-band stop resumes a child from there and re-scans the non-matching rows discarded since its last match (bounded by the inter-match gap; the whole prefix-to-first-match for a never-matched child). Correct (no dup/no loss) and Java-faithful, but for very sparse intersections under a tight per-page limit the re-scan is wasted work and — pathologically — could fail to make progress within one page. Tracking the position just *before* the currently-held candidate (so resume re-reads only it) would eliminate the re-scan; this diverges from Java's model, so it's a Go-only read-path optimization, not parity. Flagged by codex on PR #252.
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

### 7.5 + 7.6 (HOLISTIC — RFC-077): Source-anchored join result + structural interning

**Bundled per maintainer decision (2026-06-04):** 7.5 (structural interning key) and 7.6
(source-anchored field pull-up) are two facets of ONE change — retire the opaque, name-keyed
join-merge apparatus (`JoinMergeResultValue`/`JoinMergeAllValue`, `composeFieldOverJoinMerge`,
the string `mergeQuantifierAlias`) for **anchored access**: the translator + re-enumeration emit
`RecordConstructorValue` of `FieldValue(QOV(legAlias), col)`, resolved by the existing
`composeFieldOverConstructor`. RFC-073 GATED 7.6 on 7.5 (a circular "anchor only the binary join =
split-brain"); doing them as one migration breaks that deadlock, and **7.5's structural interning
falls out for free** — the anchored RC is canonical (one type, alias-set-keyed), so it interns
structurally via RFC-039/040 `MemoEqual`, retiring the synthetic string `mergeQuantifierAlias`
(measured load-bearing today *because* the merge is opaque; anchoring removes that).

**Design unlock (RFC-077):** Go's `RecordConstructorValue.Evaluate` produces a NAME-keyed map
(`values.go:2148`), so Go uses **name-based anchored resolution** — NOT Java's full ordinal-substrate
machinery (`FieldValue.ofOrdinalNumber`). Smaller, cleaner, Go-adapted (the sanctioned
"diverge when strictly better + clean" path). `composeFieldOverConstructor` simplifies field
accesses at plan time so the RC rarely survives to runtime; consumers reading the old
bare+`ALIAS.COL` keys (`cascades_generator.go:1890` column derivation, `executor.go:1434 mergeRows`,
`streaming_cursors.go`) move to the anchored RC's field keys. This addresses Torvalds' RFC-073
NAK (the Evaluate-shape change) via the name-keyed-map + compile-time-simplification design.

7.5/7.6 history (the prior split, RFC-073's deferred analysis, the Graefe direction + Torvalds NAK)
is preserved in `rfcs/073-source-anchored-join-result.md`; RFC-077 supersedes it as the holistic
plan.

**Status update (2026-06-05):** F3 split the bundle (Graefe ruling: 7.5 now, 7.6 deferred on column
threading). 7.5 IMPLEMENTED — and the documented root-cause was CORRECTED by an implementation spike:
the interning was NOT defeated by an alias-sensitive candidate-narrowing hash (the hash is already
alias-invariant, RFC-074; `memoizeNonLeaf` already uses alias-aware `MemoEqual`). The real
alias-sensitive sites are `Reference.Insert`/`InsertFinal`, which dedup alias-IDENTITY only — a
Go-vs-Java divergence (Java's `containsInMemo` is alias-aware). Fix: a GATED alias-aware `MemoEqual`
dedup tier in `Insert`/`InsertFinal`, opted into via `SelectExpression.InternsAliasAware()` (merge
re-enumeration selects only — gating avoids over-deduping CTE column-rename selects, which silently
read NULL when collapsed because Go's column derivation resolves some references by quantifier-alias
IDENTITY, unlike Java's ordinal/group model; this is the RESOLUTION-model axis, NOT alias-namespace
naming, which 7.1 already unified). `mergeQuantifierAlias` +
`mergeAliasPrefix` deleted; the merge quantifier now gets a plain `uniqueId`. Verified by a
deterministic chain task-count gate (±2%, pinned 3-chain 8999 / 4-chain 30593; naive un-gated uniqueId
DOUBLES the 4-chain to 60044) + full suite green + 5× determinism. The opaque-type retirement
(JoinMergeAllValue/Seed/composeFieldOverJoinMerge) and anchored RC remain 7.6, deferred on column
threading (F3). See RFC-077 "Precise root-cause — CORRECTED".

**7.6 DONE (2026-06-05, RFC-077 v4):** column threading landed in the 7.6 core (#259); this follow-up
(a) anchors EVERY reachable join-leg shape — correlated scalar subqueries (incl. dotted scalarCol),
derived tables / aggregate subqueries / CTE references as join legs, recursive-CTE legs (outer +
recursive-branch self-reference), Sort/Distinct/Union/Aggregate legs — and (b) DELETES the opaque
`JoinMergeAllValue`/`JoinMergeSeedValue`/`Seed`/`composeFieldOverJoinMerge`, migrating all consumers
to the source-anchored `RecordConstructorValue`. Decisive root-cause: the core's `tableColumns` was
case-SENSITIVE while the SQL path upper-cases table names, so the core's anchoring was DORMANT
(`resolveRecordType` now case-insensitive). Proven no-fallback by a panic-probe over the entire SQL
production surface; chain budget gate unchanged (anchored interns identically); plandiff
byte-identical. See RFC-077 v4.

- [x] **7.5 + 7.6 (RFC-077) — DONE.** 7.5 merged (#258), 7.6 core merged (#259), 7.6 retirement
  (anchor-all + delete opaque types) on `feat/7.6b-retire-opaque-merge`.

### 7.7 Retire `ImplementIndexScanRule` — unify on the data-access/`Compensation` path (RFC-045 follow-up)

- [x] **DONE (RFC-076 v5, 2026-06-05).** `ImplementIndexScanRule` + both registrations + its 3 test
  files deleted; shared helpers extracted to `scan_match_helpers.go`. Sequence: 3b template-aware
  costing → 3a constraint-pass activation + stub-chain costing → deletion + **data-access compensation
  materialization** (the v3/v4 premise missed that the data-access path never materialized its residual
  `Compensation.apply` LOGICAL filter into a physical plan during PLANNING, so the index scan was
  dropped to a full scan for the indexed-eq + non-indexed-residual shape; `pushDataAccessTasks` now
  realizes the unambiguously-safe simple residual as a physical filter, guarded against IN / correlated
  / index-only / vector-or-aggregate-inner / join-leg shapes — see `isSimpleResidualCompensation` +
  `refHasCorrelatedMatch`). `validateNoIndexOnlyResidual` KEPT (still load-bearing). Full suite green,
  plandiff byte-identical, determinism 5×. The data-access/`Compensation` path is now the sole scan
  producer, as in Java. Original analysis retained below.
- [ ] **Follow-up (Graefe v5 ACK condition): replace the `isSimpleResidualCompensation` allowlist with
  Java's exploratory-yield re-optimization.** Java yields data-access compensations via
  `FinalYields.yieldUnknownExpression` — a non-`RecordQueryPlan` lands in the *exploratory* set and is
  re-optimized by the normal PLANNING loop, so EVERY compensation shape is realized uniformly. Go's
  `pushDataAccessTasks` only `InsertFinal`s, so `implementDataAccessCompensation` + the
  `isSimpleResidualCompensation` allowlist stand in for that primitive. The allowlist is correct and
  each exclusion is pinned today, but it will rot the moment a new compensation shape appears with no
  allowlist arm (falls through to the dead-final-member path → silent no-plan). The honest fix is a Go
  `yieldUnknown`/exploratory-insert that re-optimizes all compensations and shrinks the allowlist to
  nothing — BLOCKED on Go's compensation re-optimization correctly handling IN-explode / correlated /
  index-only shapes (a naive exploratory-insert re-breaks them today, which is why the allowlist exists).

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
  - **RFC-076 v3 ACK'd (Graefe + Torvalds), committed `75bf8d17`. v2's leaf-matching diagnosis was
    FALSIFIED by empirical reproduction.** Disabling `ImplementIndexScanRule` + tracing shows the
    match infra fires correctly (leaf scan↔scan `EqualsWithoutChildren=true`; `matchSingleSourceAgainstSelect`
    binds the predicate to the candidate Placeholder; `pushDataAccessTasks` fires) — the gap is that
    every seed-match path builds its MatchInfo with `maxMatchMap=nil`, so `PartialMatch.PullUp`
    (`partial_match.go:117`) returns nil → `CompensateCompleteMatch` → `ImpossibleCompensation` →
    `DataAccessForMatchPartition` skips → ZERO scans. `ImplementIndexScanRule` is the SOLE producer.
    `ComputeMaxMatchMap` (`max_match_map.go:167`) exists but is never called by the seeds.
  - **WIP STASHED (`git stash list` → top of stack on this branch).** Implemented the data-access
    completion per the Graefe-confirmed Java recipe: wire `ComputeMaxMatchMap` into the seed paths
    (leaf uses an identity map over the candidate result value; intermediate uses query/candidate
    result values + `NewAliasMapValueEquivalence`), residual compensation (re-apply unmatched
    predicates as filters via `OfPredicateCompensation` — Java produces the match even when fully
    residual), an IN-sargable guard (an IN comparison is NOT a contiguous range — leave it to the
    explode/InJoin path), and per-ref `AdjustPartialMatchesForRef` in `pushDataAccessTasks` (matches
    are seeded in PLANNING exploration, after the dead phase-start `AdjustMatches`, so ordering parts
    are only computed at consume time). **Validated:** full cascades unit suite GREEN with the rule
    enabled; 12/16 cited shape tests green with the rule disabled.
  - **REMAINING (multi-shift, per-feature vs Java — bigger than v2 stated):** broad `just test`
    exposes that the new (Java-correct) matches diverge from the rule's plans: (1) Go cost/Pareto
    pruning lets a non-unique index beat the unique one + breaks index intersection (`plangen`
    `UniqueIndexPointLookupPreferred`, `EndToEnd_IndexIntersection`); (2) `wrapScanPlanWithCoverage`
    (`abstract_data_access_rule.go:345`) doesn't propagate the candidate `unique` flag that
    `OrderedIndexScanRule` sets; (3) vector index-only-residual: a metric mismatch no longer raises
    `UnplannableIndexOnlyResidualError` (4 `TestVectorPlan_*`); (4) **DELETE over-deletes** →
    `TestFDB_DeleteOldAndLowValue` panic (correctness); (5) sort-elim ordering parts now computed but
    the satisfaction→ordered-scan→`RemoveSort` chain is incomplete (4 `TestSortElim_*`); (6) covering
    full-index-scan vs table scan (`TestPlanHarness` covering/range). Grind each rule-disabled,
    red-first, aligned to Java/plandiff; do NOT one-off guess (a `boundCount==0` guard diverged from
    Java and broke a Java-aligned unit test). THEN retire the rule + guard + final-plan validation.
    `ImplementFilterRule` STAYS (faithful Java port). Separate PR from RFC-077.
  - **RFC-076 v4 (2026-06-04): step 1 DONE (5 correctness fixes, Graefe+Torvalds ACK), full retirement
    in progress.** The data-access path is now correct for every FDB-tested shape (dual-correlation
    joins, simple joins, aggregate eq-filter, vector residuals). Full rule retirement needs: (3a)
    activate the dormant ordering-constraint pass (`constraintOnly` never set true → `PushRequestedOrderingThrough*`
    inert); (3b) template-aware costing (a nil-inner `Fetch` shell hides its inner from the cost model
    → join-order flip on `TestFDB_JoinSelPred_Repro`). See RFC-076 "v4 amendment" for the sequenced plan
    + the ref-resolving (not magic-constant) 3b. `validateNoIndexOnlyResidual` STAYS (now load-bearing
    via the DistanceRank residual). **Step-2 cleanup TODO (file/do during retirement, by the retirement
    PR): stop SEEDING `AggregateIndexMatchCandidate` partial matches onto non-GroupBy refs** in the
    leaf/intermediate match rules, so the agg-skip type-switch — currently duplicated 4× (`planner.go:465`
    data-access boundary [new], `rule_implement_index_scan.go` [dies with the rule], `rule_streaming_agg_from_index.go`,
    `rule_aggregate_data_access.go`) — collapses to one. Torvalds flagged the boundary guard as a
    defensible transition shim, NOT the permanent design; the don't-seed fix is the root cause.

### 7.6 — MERGED into 7.5+7.6 (RFC-077)

7.6 (source-anchored field pull-up / retire `composeFieldOverJoinMerge`) is no longer a separate
item: it is the same change as 7.5 (anchored RC retires the opaque merge → structural interning
falls out). See the holistic **7.5 + 7.6 (RFC-077)** entry above. RFC-073's deferred analysis is
the historical record.

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
- [x] **#13 — LOW (concurrency-sensitive).** Fixed in **RFC-072**. The reply channel is now returned to the pool exactly on the no-send-can-race paths: `Release()` pools it on the success path (caller received, no `Cancel`); `Cancel()` pools iff it won the `delete` race and nils `h.ch` so `Release` never double-pools; `SendAndWait` pools on success and via `cancelPending` (delete + pool-iff-won) on timeout, leaving the rare race-loser to GC (it may hold a stale buffered value). The false "readLoop returns it after dispatch" comment is corrected — readLoop only delivers. Pinned by `reply_pool_test.go` (won/lost-race + success + no-double-pool, `-race`-clean) via a `putReplyChannel` seam (deterministic, not `sync.Pool`-reuse-dependent). Full multi-goroutine timeout-vs-delivery race coverage awaits `SimTransport` (C4). FDB-C + Torvalds ACK.
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
  (2) a go-vs-cgo read-version
  staleness asymmetry (go=`transaction_too_old(1007)` while cgo succeeds on the SAME pinned read
  version near the 5s MVCC edge). **Characterized (RFC-056 #235): PERF/TIMING, not a wire/
  behavioral divergence** — both clients correctly return 1007 once a read version genuinely ages
  past the 5s window; go just reaches that edge sooner under CPU starvation because its getKey
  does more per call (the materializing `buildSegmentsLocked` vs libfdb_c's lazy iterator), and
  the differential pins one version then issues 28 selectors on it. So behavioral identity HOLDS;
  the real fix is the lazy iterator (continuation item 1 below), which reduces the per-call work
  at the source. The differential is already robust (retries the transient 1007 with a fresh
  version via the canonical `gofdb.IsRetryable` predicate — `differential_getkey_ryw_test.go`).
  REMAINING: profile go-getKey 1007-rate vs cgo to confirm item-1 closes it. See rfcs/055.
  - [x] **(1) `Transaction.GetKey` ignores pending writes** — FIXED (RFC-056): faithful port of
    C++ `resolveKeySelectorFromCache` over a merged segment view (`pkg/fdbgo/client/ryw_getkey.go`:
    `rywSegmentIterator`/`buildSegmentsLocked` + `getKeyRYW`'s unknown-range server-read-remerge
    loop), wired into `Transaction.GetKey` (+ the base↔resolved RANGE read-conflict, fixing the
    old single-key conflict) and `Snapshot.GetKey` (writes visible by default via
    `includeWrites=!snapshotRYWDisabled`). A merged-GetRange shortcut was verified-WRONG on
    `{orEqual, offset>1}` — not used. Pinned by `ryw_getkey_test.go` + the
    `TestDifferential_GetKeyRYW` differential (pending Set/Clear/ClearRange vs libfdb_c) + corpus
    seeds. **Two deferred sub-edges, same root** (the rywCache doesn't preserve per-key op-type
    — it eagerly folds resolved atomics into plain entries and moves a matched CompareAndClear
    into the cleared list; faithfully closing either needs a write-map that retains op-type, like
    C++'s):
    (a) **phantom offset slot** — a PENDING atomic that resolves to no value (CompareAndClear, or
    an atomic on a locally-cleared range) is modeled as absent; libfdb_c keeps it as a "phantom"
    is_kv slot COUNTED in the offset walk. The getKey differential is scoped to non-atomic pending
    writes until then.
    (b) **conflict-range filtering** — C++ `updateConflictMap` SUBTRACTS independent-write/cleared
    segments from the getKey read-conflict (no DB read there). Go keeps the FULL base↔resolved
    range: it OVER-conflicts on those segments (extra retries, always SAFE) rather than risk a
    missed conflict on a folded dependent atomic (an UNSAFE under-conflict — a naive
    `!hasAtomics` filter was attempted and reverted after codex showed it drops the conflict for a
    Get-folded atomic). The full range is strictly better than the old single-key conflict (which
    under-conflicted). Exact filtering deferred with the op-type preservation above.
  - [x] **RYW applyAtomic on present-empty values** — FIXED: the chain conflated `nil` (absent)
    with present-empty, so a V2 op after `Xor(k,"")` took the absent→operand path (`Min(k,"0")`
    → 0x30 vs libfdb_c 0x00). The get/merge chains now keep present-empty non-nil (nil reserved
    for absent), mirroring C++ `Optional.present()`. Pinned by
    `TestRYWGetRange_V2AtomicOnPresentEmpty`.
  - (3) **versionstamped-pending read = unreadable.** A SetVersionstampedKey/Value pending on a
    key reads as ABSENT in Go pre-commit (Get→nil, GetRange→omit); C++ marks it `is_unreadable`
    and THROWS `accessed_unreadable`. Go has no unreadable state — approximated as absent,
    consistently across ALL base states: storage-absent, locally cleared, a pending plain Set,
    and a non-nil storage value the pending stamp shadows. `atomic()` refuses to eager-fold a
    versionstamp into a plain entry, and `resolveAtomics` short-circuits the chain to
    `unresolved` (terminal, dominant over cleared) so both read paths exclude the key and drop
    any stale storage value. Pinned by `TestRYW_VersionstampedAbsentNoPhantom` +
    `TestRYW_VersionstampedOverClearedOrPlainNoPhantom`. Full C++ parity (THROW on read) still
    needs an explicit unreadable concept — part of the RFC-056 audit.

- [ ] **RFC-056 continuation — ordered, ONE AT A TIME (do 1, then 2, then 3).** After the merged
  getKey-RYW core (#235), three follow-ups remain. Both 1 and 2 WILL be done (sequentially, not
  batched); 3 is the ongoing hunt.

  1. **[x] DONE (RFC-057).** Lazy `rywSegCursor` replaced the materializing
     `buildSegmentsLocked`: getKey cost is now FLAT in cache size — **57 ms / 39 MB →
     1 µs / 816 B at N=100k (55,437×)**, measured before/after (Torvalds's "no benchmark =
     no merge" gate). Behavior-identical: a 4000-state equivalence property-test oracled
     against the retained materializer + the RFC-056 differential + a 94k-exec fuzz burst,
     all green. `next`/`prev` are a single merged-boundary `skip` (no view desync). The
     original plan:
     **Lazy/windowed segment iterator for getKey-RYW.** `buildSegmentsLocked`
     (`pkg/fdbgo/client/ryw_getkey.go`) MATERIALIZES the whole merged-segment partition of
     [allKeysBegin, maxKey) — O(writes + cacheKeys) per resolution attempt — whereas libfdb_c's
     `RYWIterator` is LAZY (a steppable zip of the write-map + snapshot-cache sub-iterators).
     Port the lazy cursor (skip/next/prev computing each segment on demand, no full
     materialization), so getKey cost is bounded by the walk distance, not the cache size. This
     ALSO shrinks **item (2)** below: less work per getKey under heavy parallel-container load →
     less likely to drift past the 5s MVCC window mid-loop → fewer transient
     `transaction_too_old(1007)`. Validate with a profiling probe: go-getKey wall-clock +
     1007-rate vs libfdb_c, before/after; confirm resolution stays byte-identical
     (`TestDifferential_GetKeyRYW` + unit tests green). Then this de-flakes item (2) at the source
     rather than only via the differential's retry.

  2. **[x] DONE (RFC-058).** Op-type-preserving write-map closed BOTH sub-edges. Added `absent`
     (phantom) + `dependent` (DEPENDENT_WRITE, carried unchanged through folds like C++
     `isDependent()` reading the immutable stack bottom) to `rywEntry`; a matched CompareAndClear
     now stays a write-map entry (never moved to `cleared`). The differential **disproved the
     original framing of (a)**: getKey is a limit-1 range read in C++ (`read(GetKeyReq)` =
     `getRangeValue`/`getRangeValueBack`), so a phantom is COUNTED in the offset walk but SKIPPED
     at the landing — not "counted and landed on." Modeled as `segPhantom` (count-in-walk +
     directional skip-at-landing); the old `segEmpty` under-counted for offset>1, a naive `segKV`
     wrongly landed on it. Also fixed a pre-existing fold-path bug the same differential caught
     (`doMax(_,"")`→nil misread as absent by a later CompareAndClear). (b) Ported `updateConflictMap`
     (ReadYourWrites.actor.cpp:335) as `conflictRangesLocked` — the getKey read-conflict now
     SUBTRACTS INDEPENDENT writes + cleared ranges (safe now that op-type is preserved; the naive
     `!hasAtomics` filter codex NAK'd on #235 is impossible here). Proof: getKey differential
     re-enabled for pending CAC/atomics + 92k-exec fuzz (sub-edge a); a deterministic commit-order
     `TestDifferential_GetKeyConflict` whose INDEPENDENT/CLEARED cases FAIL without the filter and
     pass with it (sub-edge b). FDB-C-dev + Torvalds ACK on the RFC. Original (a)/(b) text:
     (a) **phantom-slot offset counting** — a PENDING atomic that resolves to no value
         (CompareAndClear, or an atomic on a locally-cleared range) is an `is_kv` "phantom" slot
         COUNTED in the getKey offset walk in libfdb_c, but Go currently models it as absent. With
         op-type preserved, count it. (Re-enable pending-atomic shapes in the getKey differential.)
     (b) **exact `updateConflictMap` conflict filtering** — getKey's read-conflict should SUBTRACT
         independent-write + cleared segments (no DB read there); Go currently keeps the
         conservative FULL base↔resolved range (safe over-conflict). With op-type preserved, the
         subtraction is safe (a naive `!hasAtomics` filter was UNSAFE — it dropped the conflict
         for a Get-folded dependent atomic; codex caught it on #235 → reverted). Port
         `updateConflictMap` (ReadYourWrites.actor.cpp:335) faithfully and pin with a conflict
         differential (concurrent write inside the range must conflict identically in both clients).

  3. **Fresh differential axes (`/hunt-divergences`).** Probe axes still uncompared vs libfdb_c:
     atomic-op edge cases across ALL of `Atomic.h` (empty / missing / present-empty operand per
     op); error-code + option semantics (RAW_ACCESS / ACCESS_SYSTEM_KEYS / snapshot-RYW); key
     encoding / tuple packing / versionstamp-offset validation. Each closed axis is more "absolute
     proof we're identical to the C client."
     - [x] **[RFC-059 — MERGED #238] RYW-disable-after-op poison.** Differential characterization
       corrected the earlier (imprecise) framing: NOT a per-read overlap check, NOT an
       option-set-time error. libfdb_c's `setOption(READ_YOUR_WRITES_DISABLE)` after any read or
       write throws `client_invalid_operation` deferred via `deferredError`, so the option call
       succeeds but EVERY subsequent op (regular + snapshot reads/GetKey, GetRange, GetReadVersion,
       GetEstimatedRangeSizeBytes, GetRangeSplitPoints, Commit) returns 2000 — the whole txn is
       poisoned. Go was silently permissive (returned 0). RFC-059 ports the poison
       (`Transaction.rywPoisonErr` set on disable-after-op, gated uniformly at `ensureReadVersion` +
       the metrics path; a `hadRead` signal covers the GetPipelined non-caching read). Pinned by
       `TestDifferential_RYWDisableAfterOp` + `TestCommit_RYWPoisonBeatsTimeout`. Reviewed by
       FDB-C++ dev + Torvalds + codex + @claude.
     - [x] **[RFC-060 — MERGED #239] tuple-codec byte-identity differential.** The tuple/key encoding is the wire
       hard line but has ZERO differential coverage vs libfdb_c's codec. `pkg/fdbgo/fdb/tuple` is a
       near-verbatim port (core encode/decode byte-identical by inspection) but adds go-only
       hot-path helpers (`PackWithPrefix`/`Pack1WithPrefix`/`Pack1ConcatWithPrefix`/
       `PackConcatWithPrefix`/`Packer.AppendInto`/`packerPool`) absent from libfdb_c that build the
       actual index/record keys on the wire. Prove `gotuple.Pack() == cgotuple.Pack()` across all
       type codes + boundaries (int size-limit boundaries, big.Int >8 bytes + leading-0xff
       zero-fill, float/double sign-bit flip, nil-escaping in bytes/strings/nested, versionstamp
       offset), the go-only helpers vs canonical `cgotuple.Pack()`, cross-client Unpack, and an
       end-to-end FDB wire round-trip. cgotuple is itself pinned to the cross-language
       `tuples.golden`, so this transitively pins go to the golden vectors.
     - [x] **[RFC-061 — MERGED #240] SNAPSHOT_RYW_ENABLE/DISABLE counter.** Found via the
       transaction-option-semantics survey, confirmed differentially: libfdb_c models snapshot
       RYW as an integer counter (ENABLE++, DISABLE--, bypass iff <=0, default 1), but Go used a
       boolean with `SetSnapshotRywEnable()` a no-op — so `disable→enable` left snapshot reads
       stuck bypassing RYW (go silently too permissive). Fixed: `snapshotRYWDisableCount int`
       (zero-value-safe inverse: DISABLE++, ENABLE--, bypass iff >0; preserved across reset as a
       persistent option). Pinned by `TestDifferential_SnapshotRYWReenable` (10 sequences, 3
       red→green + a counter-vs-boolean discriminator + negative-count axis + RYW-disable
       dominance).
     - [x] **[RFC-062 — MERGED #241] atomic-fold width/edge differential.** Atomic fold semantics
       are the wire hard line; the existing differential only used 8-byte operands on missing keys.
       Added a differential across operand/base widths + edge operands for all 12 ops. KEY finding
       (teeth-check): tx.Set/tx.Atomic ship RAW mutations (server folds at commit), so Go's
       client-side fold (doAdd/doMin/…) runs ONLY on in-txn reads — a commit-then-read-back test
       passed even with doAdd broken. Restructured to read WITHIN the txn (exercises the fold) +
       committed read-back (server fold/wire). Verify-and-pin (fold is a faithful port); teeth
       confirmed on doAdd (6 rows) + doByteMin (4 rows). Found+fixed a test-isolation bug (go/cgo
       shared a key → missing-key fold saw the other's committed value).
     - [x] **[RFC-063 — MERGED #242] versionstamp-mutation differential.** SetVersionstampedKey/Value
       were excluded from the fuzz differential; only a Go-only interop check + an offset-0 Value
       case existed. Added masked (10-byte stamp zeroed) go-vs-cgo differentials: VersionstampedKey
       (offset 0 / after-prefix / mid-key / binary), VSValueOffsets (non-zero offsets), tuple
       PackWithVersionstamp (offset + 2-byte user-version preservation), GetVersionstamp parity
       (10-byte, == materialized stamp), error/boundary (tight-valid offset+10==body vs off-by-one
       reject, negative, past-body, too-small, empty → 2000 go==cgo), multi-op. Mask offset is
       template-derived + length/surround/non-zero guards (Torvalds). Teeth: loosening
       validateVersionstampOffset by 1 reddens offbyone_reject. The differential CORRECTED a
       reviewer assumption: two versionstamped ops in one txn get the SAME stamp (txn-level, not
       per-op batch id; user differentiates via tuple user version).
       - [ ] **Follow-up (tenant +8 offset):** differentially test the tenant-prefix offset
         adjustment (`commitpath.go:354-363`, +8 for the 8-byte tenant prefix). Needs tenant
         harness setup in `pkg/fdbgo/bench` (OpenTenant on both clients; cluster tenant_mode must
         be enabled). Open a tenant, write a versionstamped key in the tenant txn, read back within
         the tenant, mask, compare — verify go/cgo adjust the offset by the prefix length identically.
     - [x] **[RFC-064 — MERGED #243] explicit conflict-range API differential.** AddReadConflictRange/
       Key + AddWriteConflictRange/Key feed the resolver (isolation) but had no differential coverage
       (RFC-058 covered only getKey-DERIVED conflict ranges). Empirically NO divergence — edges
       (inverted→2005, empty→accept, oversized→accept) match go==cgo (the C++ NativeAPI source has no
       release inverted-check, but the C binding cgo uses returns 2005 — the differential is the spec,
       not the source). Pinned the conflict OUTCOME: read-conflict range/key (A fails 1020 iff probe
       inside, half-open r0 incl / r9 excl), write-conflict range/key (a concurrent reader fails iff
       inside A's write-conflict), snapshot-read-no-conflict, self-write+read-conflict. Reused RFC-058
       pinning (both A+B SetReadVersion(vSetup), transient→retry, fresh prefix/attempt, bounded) →
       flake-free (5 runs). Teeth: empty key-conflict range → key_exact_r5 diverges. Oversized
       committed-truncation is unobservable (keys > maxKeySize are unwritable).
     - [x] **[RFC-065] getKey boundary resolution — REAL BUG FIXED.** The existing
       getKey differentials cover the keyspace INTERIOR + clamp off-prefix results, masking the
       EDGES. A boundary probe found a real divergence: a BACKWARD selector (lastLess*) at/past
       maxReadKey (\xff) wrongly returned \xff itself instead of the greatest key < \xff. Root
       cause: resolveKeySelectorFromCache (ryw_getkey.go) short-circuited EVERY off-end seek to
       readThroughEnd, ignoring direction; C++ it.skip() clamps to the last segment and only sets
       readThroughEnd after the walk for offset>1. Fix: direction-aware off-end branch — forward
       keeps readThroughEnd; backward repositions onto the last segment and resolves backward.
       Pinned by TestDifferential_GetKeyBoundary (pinned-version differential: lastLess*(maxReadKey)
       asserted < maxReadKey, empty/large-offset/past-max edges). Teeth: re-introducing the
       unconditional shortcut reddens LLT/LLE_maxReadKey. Only the RYW path had it; rywDisabled
       delegates to the server.
     - [x] **[RFC-067 — MERGED #247] error-CODE differential → TRANSACTION_SIZE_LIMIT + 4 linked fixes.**
       A fresh error-CODE differential (`TestDifferential_ErrorCodes`, comparing the FDB error code
       each client returns for the same size/legal-range triggers) found a REAL write-path divergence:
       the Go client did NOT enforce `TRANSACTION_SIZE_LIMIT` by default — it committed >10 MB txns
       that libfdb_c rejects client-side with `transaction_too_large` (2101). C++ defaults every txn's
       sizeLimit to the 10 MB knob (NativeAPI:6133); Go's `0=disabled` default left no enforcement.
       Fix: default to the knob. Four more linked fixes surfaced via review + differential: (2) online-
       indexer lessen-work codes (Torvalds — wrong numbers, missing 2101, made latent-live by the
       limit; now matches Java `IndexingThrottle.lessenWorkCodes` 1:1); (3) commit-validation ORDER
       (codex — read-only fast path + per-mutation-before-size; then the full eager-vs-deferred model:
       key/value-size + versionstamp-offset are EAGER first-invalid-op-wins, txn-size DEFERRED; pinned
       by `TestDifferential_VersionstampValidationOrder`, 8 cases); (4) `metadataVersionKey` write
       contract (codex+FDB-C+++Torvalds — a blanket `continue` silently committed every illegal mvk
       mutation where libfdb_c returns 2000/2004; replaced with the exact C++ gate; pinned by
       `TestDifferential_MetadataVersionKey`, 8 cases); (5) size the VALIDATED snapshot not the live
       buffer (codex — a Set racing Commit could fail a small commit for an unshipped mutation; pinned
       by `TestApproximateCommitSize_SizesSnapshotNotLiveBuffer`). Also fixed a pre-existing
       differential-harness flake: pinned-version range reads now retry the transient 1007 (stale pin
       under parallel-container load) instead of `t.Fatalf` (pinned by
       `TestDifferential_PinnedRangeRetriesStaleVersion`). Reviewed clean by FDB-C++ dev + Torvalds +
       codex (per-commit deltas + full review) + @claude.

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
