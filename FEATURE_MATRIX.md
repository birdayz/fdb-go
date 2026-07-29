# SQL Feature Matrix

<!-- GENERATED FILE — DO NOT EDIT BY HAND.
     Regenerate with `just feature-matrix` (or `go run ./cmd/gen-feature-matrix`).
     Source: pkg/relational/conformance/yamsql/testdata/*.yaml — the cross-engine
     conformance corpus. A drift guard (TestFeatureMatrixUpToDate) fails CI if this
     file is stale. -->

This is the **authoritative, exhaustive inventory** of the SQL surface exercised by the
yamsql conformance corpus — one row per scenario, generated directly from the corpus so
it never drifts. For the curated high-level summary see the SQL section of `README.md`;
for known gaps, Go-only extensions, and Java-divergence detail see `DIVERGENCES.md`.

**A case count is not a support count.** Every case is classified by its declared
outcome (typed fields only, never the SQL text) into one of three columns, so a pinned
rejection is never read as working support:
- **Supported** — a positive assertion: rows verified, empty result, or a DML step that must succeed.
- **Unsupported** — an explicitly-unsupported feature the corpus pins us cleanly REJECTING
  (SQLSTATE `0A000`/`0AF00`/`0AF01`/`42883`). E.g. every `x IN (SELECT ...)` case in the
  corpus is one of these — the feature is rejected, matching Java.
- **Error-path** — correct rejection/constraint semantics (unknown column, overflow, unique
  violation, type mismatch, …): supported behaviour, not a feature gap.

The same classifier drives `SQL_COVERAGE.md`, which reports the corpus-wide percentages.

**341 scenarios · 2736 query/assertion cases** across 18 feature areas — 2395 supported, 111 unsupported-feature pins, 230 error-path pins.

| Feature area | Scenarios | Cases | Supported | Unsupported | Error-path |
|---|--:|--:|--:|--:|--:|
| Aggregates & GROUP BY | 50 | 322 | 289 | 19 | 14 |
| Joins | 62 | 273 | 264 | 3 | 6 |
| Subqueries (EXISTS / IN / scalar) | 44 | 301 | 245 | 36 | 20 |
| CTEs | 12 | 105 | 73 | 9 | 23 |
| Set operations (UNION / INTERSECT / EXCEPT) | 11 | 61 | 52 | 5 | 4 |
| DML (INSERT / UPDATE / DELETE) | 25 | 210 | 191 | 1 | 18 |
| Ordering & pagination | 14 | 115 | 111 | 0 | 4 |
| Scalar functions & expressions | 33 | 377 | 323 | 22 | 32 |
| Predicates & WHERE | 12 | 104 | 102 | 0 | 2 |
| Column resolution & aliasing | 7 | 59 | 30 | 0 | 29 |
| NULL handling | 5 | 26 | 22 | 0 | 4 |
| NULL handling & boolean logic | 2 | 48 | 48 | 0 | 0 |
| Index usage | 9 | 164 | 162 | 0 | 2 |
| Types | 13 | 148 | 127 | 4 | 17 |
| Keys & primary keys | 5 | 133 | 128 | 0 | 5 |
| Error codes & validation | 4 | 37 | 8 | 2 | 27 |
| End-to-end scenarios | 3 | 20 | 20 | 0 | 0 |
| Other | 30 | 233 | 200 | 10 | 23 |
| **Total** | **341** | **2736** | **2395** | **111** | **230** |

## Aggregates & GROUP BY

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `aggregate_case_expression` | 3 | 3 | 0 | 0 | Aggregates over CASE expressions |
| `aggregate_distinct_count` | 2 | 0 | 2 | 0 | COUNT with DISTINCT values |
| `aggregate_edge_cases` | 10 | 10 | 0 | 0 | Edge cases for aggregate functions that match Java's behavior. |
| `aggregate_empty_table` | 9 | 9 | 0 | 0 | Empty table aggregate edge cases (Java's aggregate-empty-table.yamsql). |
| `aggregate_empty_table_java` | 12 | 12 | 0 | 0 | Aggregate behavior on empty tables |
| `aggregate_expr` | 21 | 19 | 2 | 0 | Aggregate functions accept arbitrary expressions as their argument |
| `aggregate_expression_select` | 16 | 16 | 0 | 0 | SELECT-list expressions that wrap aggregate function calls — |
| `aggregate_expressions_java` | 7 | 7 | 0 | 0 | Aggregates over expressions. |
| `aggregate_index_count_not_null` | 6 | 6 | 0 | 0 | COUNT(col) aggregate index with NULLs |
| `aggregate_index_count_star` | 2 | 2 | 0 | 0 | COUNT(*) aggregate index correctness |
| `aggregate_index_ddl` | 2 | 2 | 0 | 0 | Aggregate indexes via CREATE INDEX ... |
| `aggregate_index_delete` | 5 | 5 | 0 | 0 | Aggregate index correctness after DELETE |
| `aggregate_index_having` | 2 | 2 | 0 | 0 | Aggregate index with HAVING filter |
| `aggregate_index_multi_group` | 1 | 1 | 0 | 0 | Aggregate index with multi-column GROUP BY |
| `aggregate_index_sum` | 2 | 2 | 0 | 0 | SUM aggregate index via DDL |
| `aggregate_index_update` | 6 | 6 | 0 | 0 | Aggregate index correctness after UPDATE |
| `aggregate_null_edge` | 7 | 7 | 0 | 0 | Aggregate NULL edge cases |
| `aggregate_nulls` | 9 | 9 | 0 | 0 | SQL-spec aggregate NULL semantics hardened in swingshift-35 (c370213e): |
| `aggregate_order_by_java` | 19 | 19 | 0 | 0 | Aggregate queries with ORDER BY. |
| `aggregate_sum_large` | 2 | 2 | 0 | 0 | SUM with large values |
| `aggregate_with_null_groups` | 2 | 2 | 0 | 0 | Aggregates with NULL in group keys |
| `avg` | 3 | 3 | 0 | 0 | AVG over BIGINT returns DOUBLE (float) — matches Java's |
| `count_distinct` | 3 | 1 | 2 | 0 | COUNT(DISTINCT) is rejected by both engines. |
| `count_star_vs_col` | 5 | 5 | 0 | 0 | COUNT(*) vs COUNT(col) semantics |
| `distinct_aggregates` | 9 | 1 | 8 | 0 | All DISTINCT-aggregate forms (COUNT/SUM/MIN/MAX/AVG with DISTINCT) |
| `distinct_order_by` | 3 | 3 | 0 | 0 | DISTINCT with ORDER BY |
| `distinct_patterns_java` | 8 | 7 | 1 | 0 | SELECT DISTINCT patterns. |
| `distinct_streaming_ordered` | 1 | 1 | 0 | 0 | SELECT DISTINCT over an index-ordered |
| `dml_rowcount_java` | 12 | 11 | 0 | 1 | INSERT/UPDATE/DELETE row count semantics. |
| `empty_result_aggregate` | 4 | 4 | 0 | 0 | Aggregates over empty result sets |
| `go_extensions_group_by` | 5 | 5 | 0 | 0 | Go extensions: GROUP BY (Java rejects) |
| `group_by_case` | 1 | 1 | 0 | 0 | GROUP BY with CASE expression |
| `group_by_count_star` | 4 | 4 | 0 | 0 | GROUP BY with COUNT(*) edge cases |
| `group_by_derived_expr` | 4 | 4 | 0 | 0 | GROUP BY expressions through derived tables — regression guards for |
| `group_by_expression_key` | 2 | 2 | 0 | 0 | GROUP BY with expression-based keys |
| `group_by_having_complex` | 4 | 4 | 0 | 0 | Complex GROUP BY + HAVING patterns |
| `group_by_having_java` | 9 | 7 | 0 | 2 | GROUP BY + HAVING patterns from Java's |
| `group_by_multi` | 12 | 11 | 0 | 1 | Multi-column GROUP BY plus GROUP BY on arbitrary expressions. |
| `group_by_null` | 2 | 2 | 0 | 0 | swingshift-35 commit b059485e: groupByKey no longer uses fmt.Sprintf |
| `group_by_proj_expr` | 5 | 5 | 0 | 0 | SELECT projection of an EXPRESSION on group-by columns |
| `group_by_validation` | 30 | 19 | 2 | 9 | Java's groupby-tests.yamsql validates that SELECT columns must |
| `having` | 23 | 22 | 0 | 1 | HAVING filters grouped results (post-aggregate). |
| `having_avg` | 2 | 2 | 0 | 0 | HAVING with AVG aggregate |
| `limit_aggregate` | 3 | 3 | 0 | 0 | LIMIT with GROUP BY aggregates |
| `nested_aggregate_rejection` | 4 | 2 | 2 | 0 | Java's SemanticAnalyzer.validateGroupByAggregates rejects nested |
| `order_by_aggregate` | 3 | 3 | 0 | 0 | ORDER BY aggregate expressions |
| `select_count_where` | 5 | 5 | 0 | 0 | COUNT with various WHERE predicates |
| `select_distinct` | 7 | 7 | 0 | 0 | SELECT DISTINCT pins the dedup semantics. |
| `select_distinct_null` | 1 | 1 | 0 | 0 | SELECT DISTINCT with NULL values |
| `upstream_bug_distinct_compound` | 3 | 3 | 0 | 0 | Compound DISTINCT (Java upstream bug) |

## Joins

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `coalesce_in_join` | 2 | 2 | 0 | 0 | COALESCE in JOIN and aggregate contexts |
| `comma_join_exists` | 4 | 4 | 0 | 0 | Regression for the removed Go-only `eliminateRedundantCrossJoin` rewrite. |
| `composite_pk_join` | 3 | 3 | 0 | 0 | Joins on composite primary key tables |
| `count_distinct_join` | 3 | 1 | 2 | 0 | COUNT(DISTINCT col) against rows materialised from a JOIN or |
| `cross_join` | 5 | 4 | 1 | 0 | CROSS JOIN / comma-join / no-ON INNER JOIN all produce the |
| `cross_join_filter` | 3 | 3 | 0 | 0 | Cross join with various filter patterns |
| `cross_join_no_predicate` | 2 | 2 | 0 | 0 | CROSS JOIN without predicates |
| `cte_join` | 3 | 3 | 0 | 0 | CTE used in JOIN |
| `derived_table_join` | 3 | 3 | 0 | 0 | Derived tables (subqueries in FROM) with joins |
| `distinct_join` | 3 | 3 | 0 | 0 | SELECT DISTINCT on JOIN / comma-join results — dedup happens on |
| `distinct_join_exists` | 3 | 3 | 0 | 0 | DISTINCT with JOIN and EXISTS |
| `flatmap_empty_tables` | 4 | 4 | 0 | 0 | FlatMap with empty tables |
| `flatmap_exists_coverage` | 3 | 3 | 0 | 0 | EXISTS/NOT EXISTS via FlatMap |
| `flatmap_join_pagination` | 6 | 6 | 0 | 0 | FlatMap correlated join pagination |
| `flatmap_left_outer_coverage` | 3 | 3 | 0 | 0 | LEFT OUTER via FlatMap with PK match |
| `flatmap_multicolumn_pk` | 1 | 1 | 0 | 0 | FlatMap with composite primary keys |
| `flatmap_null_fk` | 2 | 2 | 0 | 0 | FlatMap with NULL foreign key values |
| `flatmap_one_to_many` | 3 | 3 | 0 | 0 | FlatMap with 1:N relationship (high fan-out) |
| `flatmap_regression` | 5 | 5 | 0 | 0 | FlatMap regression tests |
| `flatmap_secondary_index` | 5 | 5 | 0 | 0 | FlatMap via secondary index |
| `flatmap_three_way` | 1 | 1 | 0 | 0 | Three-way join (chained FlatMap) |
| `gr1_join` | 4 | 2 | 0 | 2 | SQL §7.10 GR1 with JOIN queries: the bare-col-with-aggregate without |
| `group_by_null_join` | 3 | 3 | 0 | 0 | GROUP BY on nullable JOIN column |
| `in_with_join` | 3 | 3 | 0 | 0 | IN-list combined with JOIN |
| `join_aggregate_having` | 3 | 3 | 0 | 0 | JOIN + aggregate + HAVING combined |
| `join_aggregate_java` | 6 | 6 | 0 | 0 | JOIN combined with GROUP BY/HAVING. |
| `join_aggregate_null` | 6 | 6 | 0 | 0 | stress joins + aggregation + NULL handling |
| `join_chained` | 3 | 3 | 0 | 0 | Multiple INNER JOINs chained in one FROM clause (Java's join-tests.yamsql). |
| `join_complex_patterns` | 6 | 6 | 0 | 0 | advanced join patterns |
| `join_error_codes` | 6 | 3 | 0 | 3 | Error code conformance for JOIN operations, aligned with Java's |
| `join_exists_self` | 4 | 4 | 0 | 0 | Self-join with EXISTS: exercises the NLJ existential path where |
| `join_four_tables` | 2 | 2 | 0 | 0 | Four-table join |
| `join_group_having_exists` | 6 | 6 | 0 | 0 | complex pipeline: JOIN + GROUP BY + HAVING + EXISTS |
| `join_index_correlation` | 6 | 6 | 0 | 0 | Join with correlated index probe |
| `join_left_right_symmetry` | 2 | 2 | 0 | 0 | LEFT/RIGHT join symmetry |
| `join_leg_name_pins` | 3 | 3 | 0 | 0 | same-named columns across join legs |
| `join_null_key` | 3 | 3 | 0 | 0 | SQL §7.6: NULL = NULL evaluates to UNKNOWN, so rows with NULL in a |
| `join_on_syntax` | 6 | 6 | 0 | 0 | JOIN ... |
| `join_optimization_probes` | 6 | 6 | 0 | 0 | — |
| `join_order_expression` | 3 | 3 | 0 | 0 | ORDER BY on join results with expressions |
| `join_pagination` | 7 | 7 | 0 | 0 | JOIN with paginated results (LIMIT+OFFSET) |
| `join_patterns_java` | 7 | 7 | 0 | 0 | JOIN patterns from Java test coverage. |
| `join_self_and_cross` | 2 | 2 | 0 | 0 | Self-join and cross-join combined |
| `join_three_way_predicate` | 4 | 4 | 0 | 0 | Three-way join with predicates |
| `join_with_or_predicate` | 2 | 2 | 0 | 0 | JOIN with OR predicates |
| `left_join_aggregate` | 1 | 1 | 0 | 0 | LEFT JOIN + GROUP BY on joined result |
| `left_join_exists_combo` | 2 | 2 | 0 | 0 | LEFT JOIN combined with EXISTS filter |
| `left_join_null_fk_comprehensive` | 4 | 4 | 0 | 0 | LEFT JOIN with NULL FK comprehensive |
| `left_join_star_null` | 2 | 2 | 0 | 0 | SELECT * with LEFT JOIN NULL propagation |
| `limit_join` | 3 | 3 | 0 | 0 | LIMIT with JOIN (Go extension combo test) |
| `multi_column_join` | 4 | 4 | 0 | 0 | Multi-column join predicates — joining on two columns simultaneously. |
| `multi_feature_join_agg_exists` | 3 | 3 | 0 | 0 | Combined JOIN + aggregate + EXISTS |
| `multi_table_where_join_java` | 6 | 6 | 0 | 0 | Multi-table FROM with WHERE join. |
| `nlj_column_ambiguity` | 17 | 17 | 0 | 0 | NLJ merged-row column ambiguity |
| `nlj_null_edge_cases` | 17 | 17 | 0 | 0 | NLJ (Nested Loop Join) NULL edge cases |
| `nlj_predicate_edge_cases` | 18 | 18 | 0 | 0 | NLJ predicate edge cases |
| `outer_join` | 10 | 10 | 0 | 0 | LEFT OUTER JOIN fills the right side with NULLs when there's no |
| `secondary_index_join` | 3 | 3 | 0 | 0 | Secondary index used in JOIN |
| `select_star_join` | 2 | 2 | 0 | 0 | SELECT * in joins |
| `self_join` | 6 | 5 | 0 | 1 | Self-joins — same table referenced twice in the FROM with distinct |
| `self_join_advanced` | 3 | 3 | 0 | 0 | Advanced self-join patterns |
| `update_with_join` | 2 | 2 | 0 | 0 | UPDATE with subquery-based conditions |

## Subqueries (EXISTS / IN / scalar)

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `case_exists_combo` | 2 | 1 | 1 | 0 | CASE WHEN + EXISTS combinations |
| `correlated_exists_advanced` | 2 | 2 | 0 | 0 | Advanced correlated EXISTS edge cases — regression guards for the fix |
| `correlated_subquery_probes` | 22 | 18 | 3 | 1 | Correlated subqueries reference outer-row columns. |
| `cte_exists` | 1 | 1 | 0 | 0 | CTE combined with EXISTS |
| `delete_with_subquery` | 2 | 2 | 0 | 0 | DELETE with subquery conditions |
| `derived_table` | 5 | 4 | 0 | 1 | Derived table: FROM (SELECT ...) AS alias. |
| `derived_table_group_by` | 9 | 7 | 0 | 2 | Java's groupby-tests.yamsql exercises several derived-table + GROUP BY |
| `derived_table_patterns_java` | 7 | 7 | 0 | 0 | Derived table (subquery in FROM) |
| `derived_table_renamed` | 2 | 2 | 0 | 0 | Derived table with column renaming via AS in the inner SELECT |
| `dml_not_exists` | 5 | 5 | 0 | 0 | DML with correlated NOT EXISTS + WHERE predicates |
| `dml_subquery` | 9 | 9 | 0 | 0 | UPDATE and DELETE with subqueries in WHERE. |
| `dml_subquery_residual` | 5 | 5 | 0 | 0 | Probes the DML correlated-EXISTS scan-loop rewrite when the correlation |
| `exists` | 8 | 7 | 1 | 0 | EXISTS / NOT EXISTS subquery predicates. |
| `exists_multi_table_inner` | 2 | 2 | 0 | 0 | EXISTS with multi-table inner query |
| `exists_subquery_java` | 8 | 8 | 0 | 0 | EXISTS and NOT EXISTS subquery patterns. |
| `exists_with_aggregate` | 6 | 5 | 1 | 0 | EXISTS subquery with aggregate |
| `exists_with_or` | 3 | 1 | 2 | 0 | EXISTS subqueries combined with OR predicates. |
| `having_not_exists` | 1 | 1 | 0 | 0 | HAVING with NOT EXISTS subquery |
| `in_list_advanced` | 10 | 8 | 0 | 2 | Advanced IN-list scenarios from Java's in-predicate.yamsql: |
| `in_list_comprehensive` | 8 | 8 | 0 | 0 | Comprehensive IN-list tests |
| `in_list_index_plan` | 6 | 6 | 0 | 0 | IN-list queries must use InJoin(IndexScan) |
| `in_list_null` | 4 | 1 | 0 | 3 | Java rejects NULL anywhere in the IN list with verbatim "NULL values |
| `in_list_pushdown` | 45 | 38 | 3 | 4 | IN-list pushdown: `WHERE pk_col IN (v1, v2, ...)` on a single-column |
| `in_list_with_order_by` | 3 | 3 | 0 | 0 | IN-list combined with ORDER BY |
| `in_subquery_decomposition` | 11 | 2 | 9 | 0 | `col IN (SELECT ...)` is REJECTED (0AF00) in every shape — this file |
| `insert_select_exists` | 5 | 5 | 0 | 0 | INSERT SELECT with EXISTS filter |
| `limit_exists` | 2 | 2 | 0 | 0 | LIMIT with EXISTS subquery |
| `nested_derived_table` | 16 | 16 | 0 | 0 | Nested derived tables (Java's null-operator-tests.yamsql): |
| `normalized_exists_predicates` | 3 | 2 | 1 | 0 | OR predicates combined with EXISTS subqueries that benefit from CNF |
| `not_exists_or` | 2 | 1 | 1 | 0 | NOT EXISTS combined with OR predicates |
| `not_exists_predicates` | 5 | 5 | 0 | 0 | NOT EXISTS with various predicate shapes |
| `scalar_subquery` | 8 | 6 | 0 | 2 | Scalar subquery: `(SELECT ...)` used as a value-returning expression. |
| `scalar_subquery_advanced` | 10 | 9 | 1 | 0 | Edge-case probes for the scalar-subquery feature added in nightshift-39. |
| `scalar_subquery_dml` | 8 | 6 | 2 | 0 | Scalar subquery on the right-hand side of UPDATE SET, in DELETE WHERE |
| `scalar_subquery_java` | 5 | 4 | 1 | 0 | Scalar subqueries in SELECT and WHERE. |
| `scalar_subquery_projection` | 3 | 3 | 0 | 0 | Scalar subquery in SELECT projection |
| `scalar_subquery_typed_gates` | 9 | 3 | 1 | 5 | — |
| `scalar_subquery_types` | 9 | 8 | 1 | 0 | Type-coverage probes for scalar subqueries: the cached value flows |
| `self_not_exists` | 3 | 3 | 0 | 0 | NOT EXISTS on the same table (self-referential) |
| `subquery_exists_complex` | 4 | 4 | 0 | 0 | Complex EXISTS subquery patterns |
| `subquery_in` | 13 | 5 | 8 | 0 | `col IN (subquery)` is rejected at predicate evaluation time. |
| `subquery_in_from` | 3 | 3 | 0 | 0 | Subquery in FROM clause (derived table) |
| `subquery_scalar_in_where` | 3 | 3 | 0 | 0 | Scalar subquery used in WHERE predicate |
| `update_correlated_exists` | 4 | 4 | 0 | 0 | UPDATE with correlated EXISTS |

## CTEs

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `cte` | 41 | 26 | 4 | 11 | WITH ... |
| `cte_aggregate` | 4 | 4 | 0 | 0 | CTE materialization + GROUP BY aggregation. |
| `cte_error_codes` | 6 | 2 | 0 | 4 | Java's cte.yamsql error tests: CTE-specific validation errors. |
| `cte_java_patterns` | 8 | 6 | 0 | 2 | CTE patterns from Java's cte.yamsql. |
| `cte_multi_reference` | 2 | 2 | 0 | 0 | CTE referenced multiple times |
| `cte_recursive_tree` | 3 | 3 | 0 | 0 | Recursive CTE tree traversal |
| `cte_with_insert` | 2 | 1 | 0 | 1 | CTE used in INSERT ... |
| `recursive_cte` | 26 | 18 | 5 | 3 | WITH RECURSIVE CTEs — semi-naive (level-order) evaluation. |
| `recursive_cte_advanced` | 2 | 2 | 0 | 0 | Advanced recursive CTE edge cases — regression guards for column alias |
| `recursive_cte_aggregate` | 3 | 3 | 0 | 0 | Recursive CTE combined with aggregation — exercises the interaction |
| `recursive_cte_tree_java` | 4 | 4 | 0 | 0 | Recursive CTE for tree traversal. |
| `update_dml_cte` | 4 | 2 | 0 | 2 | UPDATE with WITH clause and UPDATE/DELETE using CTE in WHERE. |

## Set operations (UNION / INTERSECT / EXCEPT)

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `and_index_intersection` | 4 | 4 | 0 | 0 | AND over two indexed columns executes |
| `and_index_intersection_composite_pk` | 2 | 2 | 0 | 0 | 2-key pk merge |
| `composite_aggregate_intersection` | 7 | 7 | 0 | 0 | Multi-aggregate queries using |
| `in_over_intersection` | 7 | 7 | 0 | 0 | IN predicates layered over a pk-intersection. |
| `union` | 2 | 1 | 1 | 0 | UNION / UNION ALL — set operations over SELECT results. |
| `union_aggregate_java` | 7 | 5 | 1 | 1 | Aggregate over UNION ALL patterns from |
| `union_columns` | 12 | 7 | 2 | 3 | UNION column-binding: SQL standard is positional, not name-based. |
| `union_comprehensive` | 4 | 3 | 1 | 0 | Comprehensive UNION tests |
| `union_empty_tables_java` | 9 | 9 | 0 | 0 | UNION ALL behavior on empty tables |
| `union_star` | 5 | 5 | 0 | 0 | Java's union.yamsql tests UNION ALL with SELECT * on either side. |
| `union_with_aggregate` | 2 | 2 | 0 | 0 | UNION combined with aggregates |

## DML (INSERT / UPDATE / DELETE)

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `case_when_update_java` | 8 | 8 | 0 | 0 | CASE WHEN in UPDATE SET from Java's |
| `delete_all_rows` | 6 | 6 | 0 | 0 | DELETE all rows from table |
| `delete_complex_where` | 7 | 7 | 0 | 0 | DELETE with complex WHERE predicates |
| `dml_conditional` | 6 | 6 | 0 | 0 | Conditional DML operations |
| `dml_error_codes` | 9 | 4 | 0 | 5 | Error codes for DML operations aligned with Java behavior. |
| `dml_returning_probes` | 5 | 4 | 0 | 1 | Probes for DML RETURNING clause (Postgres / Java fdb-relational |
| `dml_with_null_safe` | 7 | 7 | 0 | 0 | DML (UPDATE / DELETE) with IS NOT DISTINCT FROM in WHERE — the |
| `insert_arity` | 7 | 4 | 0 | 3 | INSERT column count mismatches (Java's inserts-updates-deletes.yamsql): |
| `insert_default_values` | 4 | 4 | 0 | 0 | INSERT with NULL for optional columns |
| `insert_multi_row` | 7 | 7 | 0 | 0 | Multi-row INSERT variations |
| `insert_multi_row_java` | 11 | 10 | 0 | 1 | Multi-row INSERT patterns. |
| `insert_returning` | 4 | 4 | 0 | 0 | INSERT with verification queries |
| `insert_select` | 15 | 12 | 0 | 3 | INSERT INTO ... |
| `insert_select_complex` | 2 | 2 | 0 | 0 | INSERT ... |
| `insert_select_java` | 10 | 10 | 0 | 0 | INSERT...SELECT patterns. |
| `insert_select_transform` | 2 | 2 | 0 | 0 | INSERT ... |
| `insert_values_expr` | 27 | 22 | 1 | 4 | INSERT INTO t VALUES with expressions (arithmetic, CASE, CAST, etc). |
| `multi_insert_delete` | 6 | 6 | 0 | 0 | Multiple INSERT/DELETE/UPDATE operations |
| `update_case_when` | 10 | 9 | 0 | 1 | UPDATE SET col = CASE ... |
| `update_comprehensive` | 8 | 8 | 0 | 0 | Comprehensive UPDATE patterns |
| `update_computed_multi` | 5 | 5 | 0 | 0 | Verifies multi-column UPDATE with self-referencing SET expressions. |
| `update_delete` | 14 | 14 | 0 | 0 | UPDATE and DELETE with NULL-aware predicates. |
| `update_delete_where_java` | 12 | 12 | 0 | 0 | UPDATE and DELETE with complex WHERE. |
| `update_set_expr` | 15 | 15 | 0 | 0 | UPDATE ... |
| `update_where_in` | 3 | 3 | 0 | 0 | UPDATE with IN-list WHERE condition |

## Ordering & pagination

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `e2e_order_management` | 5 | 5 | 0 | 0 | End-to-end order management |
| `index_scan_order` | 5 | 5 | 0 | 0 | Index scan ordering (ASC/DESC) |
| `limit_offset_java` | 10 | 10 | 0 | 0 | LIMIT and OFFSET patterns. |
| `limit_one_over_wide_filter` | 1 | 1 | 0 | 0 | RFC-188 finding 2 SQL-surface guard. |
| `limit_zero` | 3 | 3 | 0 | 0 | LIMIT 0 edge case |
| `offset` | 8 | 7 | 0 | 1 | LIMIT / OFFSET (Go extension) |
| `order_by_complex` | 3 | 3 | 0 | 0 | Complex ORDER BY patterns |
| `order_by_dupe_col` | 4 | 2 | 0 | 2 | Java's orderby.yamsql: `ORDER BY b, b` (same column repeated) errors |
| `order_by_elimination` | 43 | 43 | 0 | 0 | ORDER BY elimination: when the chosen scan cursor emits rows in a |
| `order_by_expression` | 4 | 4 | 0 | 0 | ORDER BY <non-aggregate expression> — `ORDER BY a + b`, `ORDER BY |
| `order_by_index` | 4 | 4 | 0 | 0 | ORDER BY on indexed columns. |
| `order_by_limit` | 13 | 12 | 0 | 1 | ORDER BY with LIMIT — common query pattern. |
| `order_by_nulls` | 4 | 4 | 0 | 0 | Java-conformant NULL ordering (swingshift-35, 3b87574d): |
| `order_by_nulls_java` | 8 | 8 | 0 | 0 | ORDER BY with NULL values and multiple |

## Scalar functions & expressions

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `arithmetic` | 22 | 15 | 0 | 7 | swingshift-35 commit ad249d55: applyMathOp and applyArithmeticOp |
| `bitwise` | 7 | 5 | 2 | 0 | Bitwise operators: &, \|, ^, <<, >>. |
| `case_insensitive_keywords` | 9 | 8 | 0 | 1 | SQL standard says keywords are case-insensitive. |
| `case_when` | 11 | 11 | 0 | 0 | CASE WHEN ... |
| `case_when_in_java` | 5 | 4 | 1 | 0 | CASE WHEN with IN predicate from Java's |
| `cast` | 19 | 11 | 0 | 8 | swingshift-35 commits 1acc097b/258073ee/13f43b58: CAST Java-conformance. |
| `cast_scalar_java` | 12 | 10 | 0 | 2 | Scalar CAST patterns from Java's cast-tests.yamsql. |
| `coalesce_nullif` | 3 | 2 | 1 | 0 | COALESCE(v1, v2, ...) returns the first non-NULL argument, or NULL |
| `datetime_functions` | 27 | 19 | 8 | 0 | Two groups: |
| `function_in_predicate` | 5 | 5 | 0 | 0 | Functions used in WHERE predicates |
| `greatest_least` | 11 | 10 | 0 | 1 | swingshift-35 commit 97e0c731: GREATEST / LEAST propagate NULL |
| `in_expression_types` | 6 | 6 | 0 | 0 | IN predicate with various expression types |
| `like` | 16 | 16 | 0 | 0 | LIKE pattern matching with SQL wildcards (% and _). |
| `like_patterns` | 5 | 5 | 0 | 0 | LIKE pattern matching |
| `like_patterns_java` | 10 | 10 | 0 | 0 | LIKE/NOT LIKE pattern matching. |
| `like_prefix_pushdown` | 41 | 41 | 0 | 0 | LIKE prefix pushdown: `WHERE col LIKE 'foo%'` on a STRING column |
| `null_arithmetic` | 5 | 5 | 0 | 0 | NULL propagation in arithmetic expressions |
| `null_arithmetic_java` | 9 | 9 | 0 | 0 | NULL propagation through arithmetic |
| `null_in_expressions` | 6 | 5 | 1 | 0 | NULL behavior in various expression contexts |
| `nullif_coalesce_combined_java` | 6 | 2 | 4 | 0 | NULLIF and COALESCE combined |
| `numeric_functions` | 47 | 47 | 0 | 0 | Scalar numeric functions: ABS / MOD / FLOOR / CEIL / CEILING / ROUND / |
| `numeric_overflow_detection` | 5 | 3 | 0 | 2 | Numeric overflow detection |
| `overflow` | 10 | 4 | 0 | 6 | nightshift-36: integer overflow is now checked. |
| `overflow_mixed` | 3 | 2 | 0 | 1 | Follow-up probe for `feedback_next_shift_arithmetic_overflow` (which |
| `scalar_functions_java` | 17 | 15 | 2 | 0 | Scalar function patterns from Java's |
| `select_constant_expression` | 3 | 3 | 0 | 0 | Constant expressions in SELECT |
| `select_expression_projection` | 4 | 4 | 0 | 0 | Computed columns in SELECT |
| `select_expressions_java` | 9 | 7 | 0 | 2 | SELECT with various expression types. |
| `string_concat_plus` | 7 | 6 | 0 | 1 | Java's ADD string family end-to-end |
| `string_functions` | 13 | 13 | 0 | 0 | STRING-family scalar functions: UPPER / LOWER / LENGTH / CHAR_LENGTH / |
| `string_functions_java` | 11 | 10 | 0 | 1 | String function patterns. |
| `trim_concat` | 10 | 10 | 0 | 0 | TRIM / LTRIM / RTRIM / CONCAT / REPLACE — a Go-only read-side extension |
| `window_function_probes` | 3 | 0 | 3 | 0 | DIAGNOSTIC ONLY: probes window function syntax. |

## Predicates & WHERE

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `between` | 17 | 15 | 0 | 2 | swingshift-35 commit 8ee5e98d: BETWEEN / NOT BETWEEN Kleene short-circuit. |
| `between_edge_cases` | 5 | 5 | 0 | 0 | BETWEEN edge cases |
| `between_java` | 16 | 16 | 0 | 0 | BETWEEN patterns from Java's between.yamsql. |
| `complex_where_java` | 10 | 10 | 0 | 0 | Complex WHERE clause combinations. |
| `distinct_from_java` | 11 | 11 | 0 | 0 | IS [NOT] DISTINCT FROM patterns from |
| `is_distinct_from` | 12 | 12 | 0 | 0 | IS DISTINCT FROM / IS NOT DISTINCT FROM — NULL-safe equality |
| `map_path_predicate_kleene` | 7 | 7 | 0 | 0 | Pins the map-path (JOIN / CTE / HAVING) predicate evaluator's |
| `multi_predicate_push` | 3 | 3 | 0 | 0 | Multiple predicates with push-down |
| `multiple_where_predicates` | 4 | 4 | 0 | 0 | Multiple WHERE predicates |
| `where_complex_predicates` | 5 | 5 | 0 | 0 | Complex WHERE predicate combinations |
| `where_literal_on_left` | 10 | 10 | 0 | 0 | Java has tests with the literal on the LEFT side of comparison |
| `where_or_optimization` | 4 | 4 | 0 | 0 | WHERE with OR predicates |

## Column resolution & aliasing

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `alias_resolution` | 5 | 5 | 0 | 0 | Alias resolution edge cases |
| `ambiguous_column` | 13 | 5 | 0 | 8 | Java's join-tests.yamsql: SELECT unqualified column that appears in |
| `ambiguous_group_key_reread` | 4 | 1 | 0 | 3 | — |
| `qualified_star` | 13 | 10 | 0 | 3 | SELECT <tbl>.* on a multi-source FROM clause restricts the projected |
| `qualified_star_more` | 4 | 2 | 0 | 2 | More qualifier-star edge cases from Java's select-a-star.yamsql: |
| `unknown_qualifier` | 6 | 2 | 0 | 4 | Java's SemanticAnalyzer rejects qualified column references whose |
| `wrong_qualifier` | 14 | 5 | 0 | 9 | `SELECT a.id, c.label FROM a, b` where no source is named `c`. |

## NULL handling

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `not_in_null_behavior` | 2 | 2 | 0 | 0 | NOT IN with NULL values edge cases |
| `not_null_constraint_java` | 7 | 5 | 0 | 2 | NOT NULL constraint enforcement. |
| `not_null_violation` | 3 | 1 | 0 | 2 | swingshift-35 commits 1f389611/e9959ba9/38410fec: INSERT/UPDATE NULL |
| `null_operator_alignment` | 7 | 7 | 0 | 0 | NULL operator tests aligned with Java's null-operator-tests.yamsql. |
| `where_is_null_is_not_null` | 7 | 7 | 0 | 0 | IS NULL / IS NOT NULL predicates |

## NULL handling & boolean logic

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `boolean` | 30 | 30 | 0 | 0 | Subset of fdb-record-layer/yaml-tests/src/test/resources/boolean.yamsql |
| `boolean_3vl_java` | 18 | 18 | 0 | 0 | Boolean three-valued logic from Java's |

## Index usage

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `composite_secondary_index_prefix_pushdown` | 11 | 11 | 0 | 0 | Pure-prefix pushdown on composite secondary indexes: when WHERE |
| `covering_index_java` | 7 | 7 | 0 | 0 | Covering index optimization. |
| `covering_index_pushdown` | 26 | 26 | 0 | 0 | Covering-index pushdown: when every column the SELECT reads from each |
| `index_range_and_or` | 10 | 10 | 0 | 0 | Port of Java standard-tests.yamsql — AND/OR range predicates with index. |
| `index_range_predicates_java` | 10 | 10 | 0 | 0 | Index scan with range predicates |
| `index_scan_direction` | 8 | 8 | 0 | 0 | Index scan direction tests |
| `multi_column_index_java` | 7 | 7 | 0 | 0 | Multi-column (composite) index patterns. |
| `secondary_index_pushdown` | 80 | 80 | 0 | 0 | Secondary-index pushdown: `SELECT ... |
| `unique_index_violation` | 5 | 3 | 0 | 2 | Tests that unique index constraints are enforced. |

## Types

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `array_column_type` | 5 | 5 | 0 | 0 | ARRAY column types. |
| `bytes` | 14 | 11 | 0 | 3 | BYTES column type — hex literals (x'DEADBEEF'), comparisons, IN, |
| `datetime_column_types` | 43 | 39 | 4 | 0 | Go extension: DATE and TIMESTAMP column types. |
| `integer_column_types` | 27 | 22 | 0 | 5 | Comprehensive INTEGER (INT32) column type coverage: |
| `intermingle_type_filter` | 3 | 3 | 0 | 0 | INTERMINGLE_TABLES=true puts every table's primary key in the SAME FDB |
| `mixed_type_equality` | 5 | 3 | 0 | 2 | swingshift-35 commit 6853cee5: valuesEqual and compareValues no longer |
| `numeric_types` | 8 | 6 | 0 | 2 | Arithmetic across numeric column types — pins that: |
| `select_where_comparison_types` | 6 | 6 | 0 | 0 | WHERE with all comparison operators |
| `type_coercion_comparison` | 5 | 5 | 0 | 0 | Type coercion in comparisons |
| `type_coercion_java` | 11 | 11 | 0 | 0 | Implicit type coercion in comparisons |
| `type_mismatch_alignment` | 7 | 3 | 0 | 4 | Java's ExceptionUtil.translateErrorCode maps |
| `type_promotion` | 8 | 8 | 0 | 0 | Verifies implicit type promotion in comparisons and arithmetic. |
| `uuid_column` | 6 | 5 | 0 | 1 | UUID column type. |

## Keys & primary keys

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `composite_pk` | 4 | 3 | 0 | 1 | Composite PRIMARY KEY (col1, col2). |
| `composite_pk_java` | 10 | 9 | 0 | 1 | Composite primary key patterns. |
| `composite_pk_prefix_pushdown` | 15 | 14 | 0 | 1 | Pure-prefix composite PK pushdown: equalities on a leading subset |
| `pk_pushdown` | 97 | 95 | 0 | 2 | Primary-key equality pushdown: queries of the form |
| `pk_range_scan` | 7 | 7 | 0 | 0 | PK range scan with comparison operators |

## Error codes & validation

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `error_code_regression` | 17 | 3 | 0 | 14 | Comprehensive error-code regression test covering all SQLSTATE codes |
| `error_codes_java` | 10 | 0 | 2 | 8 | SQL error code conformance. |
| `unique_constraint_violation` | 4 | 2 | 0 | 2 | Unique constraint error handling |
| `unique_violation` | 6 | 3 | 0 | 3 | UNIQUE constraint violations raise SQLSTATE 23505 per the SQL |

## End-to-end scenarios

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `e2e_ecommerce` | 9 | 9 | 0 | 0 | end-to-end ecommerce scenario |
| `e2e_inventory` | 5 | 5 | 0 | 0 | End-to-end inventory management scenario |
| `e2e_user_sessions` | 6 | 6 | 0 | 0 | End-to-end user session tracking |

## Other

| Scenario | Cases | Supported | Unsupported | Error-path | What it pins |
|---|--:|--:|--:|--:|---|
| `bare_col_with_agg` | 9 | 6 | 0 | 3 | SQL §7.10 GR1: when a SELECT list contains an aggregate function, |
| `bug_hunt_probes` | 13 | 9 | 1 | 3 | Throwaway probes targeting features likely to surface bugs: |
| `cascades_plan_shapes` | 9 | 9 | 0 | 0 | Tests that verify the Cascades planner produces correct results for |
| `collation_and_nan_pins` | 3 | 3 | 0 | 0 | documented Go-right divergences and |
| `comparison_edge_cases` | 9 | 9 | 0 | 0 | Edge cases in comparison operators |
| `comparison_promotion_gate` | 14 | 10 | 0 | 4 | unpromotable comparisons reject at |
| `computed_column_names` | 6 | 6 | 0 | 0 | Verifies that unnamed computed expressions in SELECT projections |
| `datetime_edge_pins` | 9 | 6 | 0 | 3 | DATE/TIMESTAMP extension edge coverage |
| `empty_result_edge_cases_java` | 11 | 11 | 0 | 0 | Empty result handling in various |
| `empty_table_operations` | 9 | 9 | 0 | 0 | Operations on empty tables |
| `float_column` | 10 | 10 | 0 | 0 | FLOAT (32-bit) column type. |
| `in_over_primary_scan_sarg` | 17 | 17 | 0 | 0 | An IN over a PRIMARY-KEY prefix, ordered by that key: `WHERE pk IN (...) |
| `in_plan_winner_stability` | 10 | 10 | 0 | 0 | Exercises the cost model's IN-plan rung (criterion #6) at the SQL level, on |
| `information_schema` | 5 | 4 | 0 | 1 | INFORMATION_SCHEMA.* system-table queries. |
| `int_float_lanes` | 6 | 4 | 0 | 2 | 32-bit arithmetic lanes end-to-end |
| `integer_range` | 12 | 5 | 0 | 7 | INTEGER (32-bit) column range enforcement. |
| `java_alignment_probes` | 14 | 14 | 0 | 0 | Probes derived from Java's yamsql test suite to verify Go matches |
| `min_max_string` | 3 | 0 | 3 | 0 | MIN/MAX on string columns is REJECTED |
| `mixed_agg_nonagg` | 4 | 4 | 0 | 0 | Mixed aggregate and non-aggregate expressions |
| `multi_feature` | 3 | 3 | 0 | 0 | End-to-end scenario chaining several features at once: CTE + WHERE + |
| `multi_feature_integer` | 11 | 11 | 0 | 0 | Integration tests combining multiple SQL features against INTEGER (INT32) |
| `multi_operator_pipeline` | 6 | 6 | 0 | 0 | Tests that exercise multiple Cascades operators working together |
| `negative_values` | 6 | 6 | 0 | 0 | Negative numbers and zero edge cases |
| `output_column_naming` | 8 | 8 | 0 | 0 | a query's OUTPUT COLUMN NAMES are part of its |
| `parse_channel_pins` | 5 | 5 | 0 | 0 | dotted display names survive the parser→IR |
| `quoted_identifier_pins` | 4 | 4 | 0 | 0 | quoted-identifier shapes that must keep |
| `select_no_from` | 6 | 0 | 6 | 0 | FROM-less SELECT — fdb-relational 4.11.1.0's QueryVisitor.visitSimpleTable |
| `select_star_single_table` | 4 | 4 | 0 | 0 | SELECT * from single table |
| `set_op_fetch_pushdown` | 2 | 2 | 0 | 0 | set operations push below the fetch |
| `string_comparison` | 5 | 5 | 0 | 0 | String comparison edge cases |

