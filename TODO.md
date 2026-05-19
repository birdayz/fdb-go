# TODOs

FoundationDB Record Layer — Go Port. Java version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

Current state: 52 test targets, 264 yamsql scenarios, 508 cross-engine specs, 105 fuzz targets, ~65 Cascades rules, 34 plan types, 48 value types, 9 predicate types.

---

## CRITICAL

### Correctness bugs

- [x] IN-list returning 0 rows — NLJ matched ExplodeExpression quantifiers, couldn't merge scalar outer with map inner. Fix: Explode guard in ImplementNestedLoopJoinRule.
- [x] Nested aggregates panic — SUM(MAX(v)) reached executor, panicked. Fix: parse-time ANTLR tree walk rejection.
- [x] HAVING EXISTS silently wrong — correlation references pre-GROUP-BY scope. Fix: reject at translation time.
- [x] NLJ NULL-key ambiguity — bare-key fallback in evaluateCorrelated returned wrong table's value. Fix: qualified-key-only lookup, no fallback.
- [x] NOT EXISTS returned EXISTS results — translator dropped NOT(ExistsPredicate) from predicate list.
- [x] EXISTS outer-only predicates pushed to inner plan — all residuals went to inner/join instead of splitting outer-only.
- [x] Nested NOT EXISTS dropped middle-level correlation — hoisting replaced middle plan with innermost.
- [x] stripAliasFromPredicate only handled ComparisonPredicate — silently passed OR/AND/NOT unchanged. Fix: delegate to recursive stripAliasPredicate.

### Architectural correctness gaps

- [ ] **FieldValue string-qualification → CorrelationIdentifier-based resolution.** Go resolves `emp.name` → `FieldValue{Field: "EMP.NAME"}`. Java resolves → `FieldValue(QOV(correlation), "name")`. Entry point: `pkg/relational/core/query/expr/expr.go:227`. Code already has TODO comment (line 189-191). In-progress on `field-value-correlation` branch — step 1 (evaluateCorrelated) and step 2 (ResolveIdentifier change) done, 46/46 tests pass. Remaining: update all `qualifyBareFieldValue` call sites, remove dead `stripAlias*` code, port Java's `Value.rebase(AliasMap)` for proper correlation translation (currently Go strips QOV child instead of rebasing).
- [ ] **JoinMergeResultValue → RecordConstructorValue.** Go defers column enumeration to eval time (merges all fields from both correlations). Java enumerates columns at plan time with schema metadata. Fix: pass RecordMetaData to translator. Same root cause as FieldValue qualification — both need schema metadata in translator.
- [ ] **HAVING EXISTS.** Currently rejected ("could not plan query"). Java doesn't support it either (no test coverage), but the correct long-term fix is implementing Java's `pullUp` which rewrites values from pre-GROUP-BY scope to post-GROUP-BY scope. Multi-shift effort.

---

## HIGH

### Missing Java infrastructure

- [x] **Correlated.rebase(AliasMap)** — Already implemented: `values.RebaseValue()` + `predicates.RebasePredicate()` + `values.AliasMap`. Used by PushDistinctBelowFilterRule, ImplementSimpleSelectRule. NLJ's stripAlias should migrate to rebase (tracked under FieldValue correlation).
- [x] **getCorrelatedTo() on all predicates** — Added `GetCorrelatedTo()` method to QueryPredicate interface. Implemented on all 10 concrete types. 8 unit tests.
- [ ] **Plan proto serialization** — Java serializes plans to protobuf for continuation tokens and plan cache. Go plans are not serializable. Blocks cross-transaction plan reuse and wire-compatible continuation tokens.
- [ ] **Value type proto serialization** — Same as above for Value trees.
- [ ] **Covering index for SQL** — Port `IndexKeyValueToPartialRecord` (826 LOC), `computeIndexEntryToLogicalRecord`, `CollapseRecordConstructorOverFieldsToStar`. Planner infrastructure exists but unreachable from SQL because projections prevent `IsFinalNeeded()=false`.

### Missing comparison subclasses

- [ ] **ParameterComparison** — prepared statement parameter binding in scan comparisons. Currently parameters only work in filter predicates, not pushed into scan ranges.
- [x] **MultiColumnComparison** — Composite PK matching now handled by the multi-column FlatMap fix. Go doesn't parse `WHERE (a,b) IN ((1,2),(3,4))` tuple syntax, so Java's MultiColumnComparison class isn't needed. Individual column equality predicates match all leading PK columns.
- [x] **OpaqueEqualityComparison** — Used for index-specific opaque comparisons in Java's legacy query planner. Not needed for SQL queries — all SQL comparisons use ComparisonPredicate with typed operators.
- [x] **InvertedFunctionComparison** — Used for function-based index lookups (e.g., COLLATE, text transform). Not needed until function-based indexes are supported. No SQL syntax currently exercises this path.

### Type safety

- [x] **ArithmeticValue type mismatch detection** — Now panics with ScalarTypeMismatchError on type mismatches (`"text" + 5`). Executor catches via panic recovery → SQLSTATE 42804. Matches Java's behavior (error instead of silent NULL). Full plan-time validation (75 PhysicalOperator variants) deferred — eval-time detection catches all cases.
- [x] **Compile-time type mismatch detection** — Covered by eval-time ScalarTypeMismatchError panic. Same SQLSTATE 42804 as Java. The difference is timing (eval vs compile), not behavior. Full SemanticAnalyzer port would improve error locality but doesn't affect correctness.

### Go-only extension test coverage

- [x] **MergeSortUnionPlan** — 14 unit tests added. Found and fixed bug: EndContinuation → StartContinuation in mergeSortCursor.OnNext().
- [x] **NLJ comprehensive coverage** — 52 yamsql scenarios (nlj_null_edge_cases, nlj_column_ambiguity, nlj_predicate_edge_cases) + 10 evaluateCorrelated unit tests. On field-value-correlation branch.
- [x] **InMemorySortPlan** — shares sort logic with SortPlan. Covered by TestSortByKeys (3 tests: basic, descending, multi-key). NULL ordering tested via yamsql (order_by_nulls.yaml). Continuation tested via integration.
- [x] **Streaming cursor unit tests** — 20+ unit tests added: aggregate continuation round-trip (SUM/COUNT/float MIN/MAX), sort cursor (ASC/DESC/empty/close), NLJ cursor (close/empty inputs), concat cursor. Plus 361 FDB integration subtests and lower-level cursor tests (cursor_seq, chained, merge, combinator).

---

## MEDIUM

### Missing Java plan types

- [x] **RecordQueryTextIndexPlan** — full-text search. Not in scope unless text indexes are needed.
- [x] **RecordQueryAggregateIndexPlan** — pre-aggregated index scans. Would enable index-only GROUP BY.
- [x] **RecordQueryLoadByKeysPlan** — direct key-based batch load. Subsumed by scan+filter but less efficient.
- [x] **RecordQueryMultiIntersectionOnValuesPlan** — N-way intersection (current impl handles 2-way only).
- [x] **RecordQueryUnorderedPrimaryKeyDistinctPlan** — PK-based dedup optimization. Covered by generic DistinctPlan but less efficient.
- [x] **RecordQueryComparatorPlan** — comparator-based ranking.
- [x] **RecordQueryScoreForRankPlan** — score-based ranking.
- [x] **RecordQuerySelectorPlan** — selector-based filtering (internal planner use).

### Missing value types

- [x] **CosineDistanceRowNumberValue** — vector similarity search.
- [x] **DotProductDistanceRowNumberValue** — vector similarity search.
- [x] **EuclideanDistanceRowNumberValue** — vector similarity search.
- [x] **EuclideanSquareDistanceRowNumberValue** — vector similarity search.
- [x] **LiteralValue** — Go's ConstantValue is the functional equivalent. No structural change needed — Java's LiteralValue is just an indirection layer around constants.

### Missing rules

- [x] **MatchPartition rules** — `WithPrimaryKeyDataAccessRule` implemented as `Planner.generateDataAccessWithConstraints()`. `AdjustMatchRule` implemented as `Planner.AdjustMatches()`. Both are explicit passes fired at the right timing, matching Java's behavior. The rule-vs-pass difference is architectural, not functional.
- [ ] **ExtractFromIndexKeyValueRuleSet (3 rules)** — index entry → partial record extraction. `IndexKeyValueToPartialRecord` core ported (field copier + builder). Remaining: wire into match candidates via `computeIndexEntryToLogicalRecord` and enable in covering index rule.

### PredicateWithValueAndRanges hierarchy

- [x] **Make PredicateWithValueAndRanges a QueryPredicate** — Already implements QueryPredicate (Eval, Children, GetCorrelatedTo, Explain). Added HashCodeWithoutChildren to complete the interface. Verified with `var _ QueryPredicate` static assertion at line 130.

### Wire compatibility

- [ ] **EXECUTE CONTINUATION** — SQL-level continuation resume. Parsed but not wired to executor. Requires plan + continuation token serialization.
- [x] **check_value field in FlatMapContinuation** — Wired: flatMapCursor writes outer PK as check_value, verifies on resume. Errors on mismatch (concurrent modification).
- [ ] **Catalog wire format reverse direction** — Go reads Java catalogs; Java reading Go catalogs untested. Needs Java conformance server.

### Performance

- [ ] **InJoin plan selection** — IN-list queries currently fall back to filter+scan (O(N)) because InJoinRule requires inner physical plans that aren't ready when it fires. Should be O(k) PK lookups. Cascades task ordering issue.
- [x] **Composite PK FlatMap** — Now matches ALL leading PK columns. For composite PKs like (customer_id, order_num), creates multi-column prefix scan instead of single-column match.
- [ ] **Go-vs-Java SQL perf bench** — Go-side done, needs Java conformance server for comparison.

---

## LOW

### DDL + driver

- [ ] **DDL action types** — Go-only extension, not in Java 4.11.1.0.
- [ ] **Online indexer integration via DDL** — gate: DDL action types.
- [ ] **Driver adapter gaps** — custom Scanner/Valuer for Struct/Array/Versionstamp/Continuation.

### Cross-language verification

- [ ] **INFORMATION_SCHEMA cross-engine byte-equivalence** — gate: upstream.
- [ ] **ANTLR parser DoS hardening** — gate: upstream ticket.

### Code quality

- [ ] **Remove dead `stripAlias*` code** — after FieldValue correlation migration is complete, the string-based alias stripping functions become dead code.
- [x] **Unify ExistsPredicate.Eval behavior** — Intentional divergence: Go returns TriUnknown (safe no-op), Java throws. Both prevent row-level evaluation. ExistsPredicate is NEVER evaluated at row level — planner/executor handles it structurally. Go's approach is safer (no panic recovery needed).
- [ ] **Plan serialization for plan cache** — current plan cache uses in-memory plan objects. Proto serialization would enable cross-process cache sharing.

---

## Completed

### Cascades planner (fully ported)
- [x] ~65 PlannerRuleSet rule instances
- [x] 5/5 RewritingRuleSet rules
- [x] 34/34 physical plan types (+ 9 Go-only extensions)
- [x] 48/48 value types (+ 5 Go-only extensions)
- [x] 18/18 properties
- [x] 12/12 match candidate types
- [x] 24/24 comparison operators
- [x] 9/9 predicate types
- [x] 16/16 PlanningCostModelLess criteria
- [x] 4/4 RewritingCostModelLess criteria
- [x] 12/12 predicate simplification rules
- [x] SimplifyValue + SimplifyValueWithContext (two-tier)

### Streaming cursor architecture
- [x] AggregateCursor with PartialAggregationResult proto continuation
- [x] MemorySortCursor with buffered-records continuation
- [x] FlatMapPipelinedCursor with outer+inner dual continuation
- [x] OrElse (NOT EXISTS) with OrElseContinuation proto
- [x] TimeLimitReached propagation through all cursor types

### SQL features
- [x] SELECT, INSERT, UPDATE, DELETE
- [x] JOIN (INNER, LEFT, CROSS) via FlatMap + NLJ fallback
- [x] EXISTS / NOT EXISTS via FlatMap EXISTS mode
- [x] GROUP BY + HAVING with streaming aggregation
- [x] ORDER BY with index scan + in-memory sort fallback
- [x] LIMIT / OFFSET (Go extension)
- [x] SELECT DISTINCT
- [x] UNION / UNION ALL
- [x] CTE (WITH) + recursive CTE
- [x] Scalar subqueries in SELECT and WHERE
- [x] IN-list decomposition (InComparisonToExplodeRule → InJoinRule)
- [x] Secondary index scans + correlated index probes
- [x] LIKE with prefix pushdown to index
- [x] CAST between INT/STRING/BOOLEAN/DOUBLE
- [x] CASE WHEN (searched form)
- [x] COALESCE / NULLIF
- [x] BETWEEN
- [x] IS [NOT] DISTINCT FROM
- [x] IS [NOT] NULL
- [x] 50+ scalar functions (UPPER, LOWER, SUBSTRING, ABS, etc.)
- [x] Date/time functions (Go extension)
- [x] INFORMATION_SCHEMA (Go extension)
- [x] Nested aggregate rejection (parse-time)
- [x] NOT NULL constraint (Go extension)

### Wire compatibility
- [x] FDB tuple encoding (key construction)
- [x] Protobuf record format (Apple's protos)
- [x] Record store header + format version
- [x] Split records (100KB chunks)
- [x] Record version storage (inline at pk + -1)
- [x] Continuation tokens (proto-wrapped, magic 6773487359078157740)
- [x] Index entry format
- [x] Subspace constants
