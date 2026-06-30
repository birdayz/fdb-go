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

**319 scenarios · 2513 query/assertion cases** across 18 feature areas.

| Feature area | Scenarios | Cases |
|---|--:|--:|
| Aggregates & GROUP BY | 49 | 298 |
| Joins | 60 | 264 |
| Subqueries (EXISTS / IN / scalar) | 42 | 281 |
| CTEs | 12 | 85 |
| Set operations (UNION / INTERSECT / EXCEPT) | 8 | 47 |
| DML (INSERT / UPDATE / DELETE) | 25 | 194 |
| Ordering & pagination | 13 | 114 |
| Scalar functions & expressions | 32 | 347 |
| Predicates & WHERE | 12 | 104 |
| Column resolution & aliasing | 6 | 55 |
| NULL handling | 5 | 26 |
| NULL handling & boolean logic | 2 | 48 |
| Index usage | 9 | 162 |
| Types | 12 | 144 |
| Keys & primary keys | 5 | 132 |
| Error codes & validation | 4 | 37 |
| End-to-end scenarios | 3 | 20 |
| Other | 20 | 155 |

## Aggregates & GROUP BY

| Scenario | Cases | What it pins |
|---|--:|---|
| `aggregate_case_expression` | 3 | Aggregates over CASE expressions |
| `aggregate_distinct_count` | 2 | COUNT with DISTINCT values |
| `aggregate_edge_cases` | 10 | Edge cases for aggregate functions that match Java's behavior. |
| `aggregate_empty_table` | 9 | Empty table aggregate edge cases (Java's aggregate-empty-table.yamsql). |
| `aggregate_empty_table_java` | 12 | Aggregate behavior on empty tables |
| `aggregate_expr` | 21 | Aggregate functions accept arbitrary expressions as their argument |
| `aggregate_expression_select` | 16 | SELECT-list expressions that wrap aggregate function calls — |
| `aggregate_expressions_java` | 7 | Aggregates over expressions. |
| `aggregate_index_count_not_null` | 6 | COUNT(col) aggregate index with NULLs |
| `aggregate_index_count_star` | 2 | COUNT(*) aggregate index correctness |
| `aggregate_index_ddl` | 2 | Aggregate indexes via CREATE INDEX ... |
| `aggregate_index_delete` | 5 | Aggregate index correctness after DELETE |
| `aggregate_index_having` | 2 | Aggregate index with HAVING filter |
| `aggregate_index_multi_group` | 1 | Aggregate index with multi-column GROUP BY |
| `aggregate_index_sum` | 2 | SUM aggregate index via DDL |
| `aggregate_index_update` | 5 | Aggregate index correctness after UPDATE |
| `aggregate_null_edge` | 7 | Aggregate NULL edge cases |
| `aggregate_nulls` | 7 | SQL-spec aggregate NULL semantics hardened in swingshift-35 (c370213e): |
| `aggregate_order_by_java` | 6 | Aggregate queries with ORDER BY. |
| `aggregate_sum_large` | 2 | SUM with large values |
| `aggregate_with_null_groups` | 2 | Aggregates with NULL in group keys |
| `avg` | 3 | AVG over BIGINT returns DOUBLE (float) — matches Java's |
| `count_distinct` | 3 | COUNT(DISTINCT) is rejected by both engines. |
| `count_star_vs_col` | 5 | COUNT(*) vs COUNT(col) semantics |
| `distinct_aggregates` | 9 | All DISTINCT-aggregate forms (COUNT/SUM/MIN/MAX/AVG with DISTINCT) |
| `distinct_order_by` | 3 | DISTINCT with ORDER BY |
| `distinct_patterns_java` | 8 | SELECT DISTINCT patterns. |
| `dml_rowcount_java` | 12 | INSERT/UPDATE/DELETE row count semantics. |
| `empty_result_aggregate` | 4 | Aggregates over empty result sets |
| `go_extensions_group_by` | 5 | Go extensions: GROUP BY (Java rejects) |
| `group_by_case` | 1 | GROUP BY with CASE expression |
| `group_by_count_star` | 4 | GROUP BY with COUNT(*) edge cases |
| `group_by_derived_expr` | 4 | GROUP BY expressions through derived tables — regression guards for |
| `group_by_expression_key` | 2 | GROUP BY with expression-based keys |
| `group_by_having_complex` | 4 | Complex GROUP BY + HAVING patterns |
| `group_by_having_java` | 9 | GROUP BY + HAVING patterns from Java's |
| `group_by_multi` | 12 | Multi-column GROUP BY plus GROUP BY on arbitrary expressions. |
| `group_by_null` | 2 | swingshift-35 commit b059485e: groupByKey no longer uses fmt.Sprintf |
| `group_by_proj_expr` | 4 | SELECT projection of an EXPRESSION on group-by columns |
| `group_by_validation` | 24 | Java's groupby-tests.yamsql validates that SELECT columns must |
| `having` | 23 | HAVING filters grouped results (post-aggregate). |
| `having_avg` | 2 | HAVING with AVG aggregate |
| `limit_aggregate` | 3 | LIMIT with GROUP BY aggregates |
| `nested_aggregate_rejection` | 4 | Java's SemanticAnalyzer.validateGroupByAggregates rejects nested |
| `order_by_aggregate` | 3 | ORDER BY aggregate expressions |
| `select_count_where` | 5 | COUNT with various WHERE predicates |
| `select_distinct` | 7 | SELECT DISTINCT pins the dedup semantics. |
| `select_distinct_null` | 1 | SELECT DISTINCT with NULL values |
| `upstream_bug_distinct_compound` | 3 | Compound DISTINCT (Java upstream bug) |

## Joins

| Scenario | Cases | What it pins |
|---|--:|---|
| `coalesce_in_join` | 2 | COALESCE in JOIN and aggregate contexts |
| `composite_pk_join` | 3 | Joins on composite primary key tables |
| `count_distinct_join` | 3 | COUNT(DISTINCT col) against rows materialised from a JOIN or |
| `cross_join` | 5 | CROSS JOIN / comma-join / no-ON INNER JOIN all produce the |
| `cross_join_filter` | 3 | Cross join with various filter patterns |
| `cross_join_no_predicate` | 2 | CROSS JOIN without predicates |
| `cte_join` | 3 | CTE used in JOIN |
| `derived_table_join` | 3 | Derived tables (subqueries in FROM) with joins |
| `distinct_join` | 3 | SELECT DISTINCT on JOIN / comma-join results — dedup happens on |
| `distinct_join_exists` | 3 | DISTINCT with JOIN and EXISTS |
| `flatmap_empty_tables` | 4 | FlatMap with empty tables |
| `flatmap_exists_coverage` | 3 | EXISTS/NOT EXISTS via FlatMap |
| `flatmap_join_pagination` | 6 | FlatMap correlated join pagination |
| `flatmap_left_outer_coverage` | 3 | LEFT OUTER via FlatMap with PK match |
| `flatmap_multicolumn_pk` | 1 | FlatMap with composite primary keys |
| `flatmap_null_fk` | 2 | FlatMap with NULL foreign key values |
| `flatmap_one_to_many` | 3 | FlatMap with 1:N relationship (high fan-out) |
| `flatmap_regression` | 5 | FlatMap regression tests |
| `flatmap_secondary_index` | 3 | FlatMap via secondary index |
| `flatmap_three_way` | 1 | Three-way join (chained FlatMap) |
| `gr1_join` | 4 | SQL §7.10 GR1 with JOIN queries: the bare-col-with-aggregate without |
| `group_by_null_join` | 3 | GROUP BY on nullable JOIN column |
| `in_with_join` | 3 | IN-list combined with JOIN |
| `join_aggregate_having` | 3 | JOIN + aggregate + HAVING combined |
| `join_aggregate_java` | 6 | JOIN combined with GROUP BY/HAVING. |
| `join_aggregate_null` | 6 | stress joins + aggregation + NULL handling |
| `join_chained` | 3 | Multiple INNER JOINs chained in one FROM clause (Java's join-tests.yamsql). |
| `join_complex_patterns` | 6 | advanced join patterns |
| `join_error_codes` | 6 | Error code conformance for JOIN operations, aligned with Java's |
| `join_exists_self` | 4 | Self-join with EXISTS: exercises the NLJ existential path where |
| `join_four_tables` | 2 | Four-table join |
| `join_group_having_exists` | 6 | complex pipeline: JOIN + GROUP BY + HAVING + EXISTS |
| `join_index_correlation` | 6 | Join with correlated index probe |
| `join_left_right_symmetry` | 2 | LEFT/RIGHT join symmetry |
| `join_null_key` | 3 | SQL §7.6: NULL = NULL evaluates to UNKNOWN, so rows with NULL in a |
| `join_on_syntax` | 6 | JOIN ... |
| `join_optimization_probes` | 6 | — |
| `join_order_expression` | 3 | ORDER BY on join results with expressions |
| `join_pagination` | 7 | JOIN with paginated results (LIMIT+OFFSET) |
| `join_patterns_java` | 7 | JOIN patterns from Java test coverage. |
| `join_self_and_cross` | 2 | Self-join and cross-join combined |
| `join_three_way_predicate` | 4 | Three-way join with predicates |
| `join_with_or_predicate` | 2 | JOIN with OR predicates |
| `left_join_aggregate` | 1 | LEFT JOIN + GROUP BY on joined result |
| `left_join_exists_combo` | 2 | LEFT JOIN combined with EXISTS filter |
| `left_join_null_fk_comprehensive` | 4 | LEFT JOIN with NULL FK comprehensive |
| `left_join_star_null` | 2 | SELECT * with LEFT JOIN NULL propagation |
| `limit_join` | 3 | LIMIT with JOIN (Go extension combo test) |
| `multi_column_join` | 4 | Multi-column join predicates — joining on two columns simultaneously. |
| `multi_feature_join_agg_exists` | 3 | Combined JOIN + aggregate + EXISTS |
| `multi_table_where_join_java` | 6 | Multi-table FROM with WHERE join. |
| `nlj_column_ambiguity` | 17 | NLJ merged-row column ambiguity |
| `nlj_null_edge_cases` | 17 | NLJ (Nested Loop Join) NULL edge cases |
| `nlj_predicate_edge_cases` | 18 | NLJ predicate edge cases |
| `outer_join` | 10 | LEFT OUTER JOIN fills the right side with NULLs when there's no |
| `secondary_index_join` | 3 | Secondary index used in JOIN |
| `select_star_join` | 2 | SELECT * in joins |
| `self_join` | 6 | Self-joins — same table referenced twice in the FROM with distinct |
| `self_join_advanced` | 3 | Advanced self-join patterns |
| `update_with_join` | 2 | UPDATE with subquery-based conditions |

## Subqueries (EXISTS / IN / scalar)

| Scenario | Cases | What it pins |
|---|--:|---|
| `case_exists_combo` | 2 | CASE WHEN + EXISTS combinations |
| `correlated_exists_advanced` | 2 | Advanced correlated EXISTS edge cases — regression guards for the fix |
| `correlated_subquery_probes` | 22 | Correlated subqueries reference outer-row columns. |
| `cte_exists` | 1 | CTE combined with EXISTS |
| `delete_with_subquery` | 2 | DELETE with subquery conditions |
| `derived_table` | 5 | Derived table: FROM (SELECT ...) AS alias. |
| `derived_table_group_by` | 9 | Java's groupby-tests.yamsql exercises several derived-table + GROUP BY |
| `derived_table_patterns_java` | 7 | Derived table (subquery in FROM) |
| `derived_table_renamed` | 2 | Derived table with column renaming via AS in the inner SELECT |
| `dml_not_exists` | 5 | DML with correlated NOT EXISTS + WHERE predicates |
| `dml_subquery` | 9 | UPDATE and DELETE with subqueries in WHERE. |
| `exists` | 8 | EXISTS / NOT EXISTS subquery predicates. |
| `exists_multi_table_inner` | 2 | EXISTS with multi-table inner query |
| `exists_subquery_java` | 8 | EXISTS and NOT EXISTS subquery patterns. |
| `exists_with_aggregate` | 2 | EXISTS subquery with aggregate |
| `exists_with_or` | 3 | EXISTS subqueries combined with OR predicates. |
| `having_not_exists` | 1 | HAVING with NOT EXISTS subquery |
| `in_list_advanced` | 10 | Advanced IN-list scenarios from Java's in-predicate.yamsql: |
| `in_list_comprehensive` | 8 | Comprehensive IN-list tests |
| `in_list_index_plan` | 6 | IN-list queries must use InJoin(IndexScan) |
| `in_list_null` | 4 | Java rejects NULL anywhere in the IN list with verbatim "NULL values |
| `in_list_pushdown` | 45 | IN-list pushdown: `WHERE pk_col IN (v1, v2, ...)` on a single-column |
| `in_list_with_order_by` | 3 | IN-list combined with ORDER BY |
| `in_subquery_decomposition` | 11 | `col IN (SELECT ...)` was previously decomposed via a pre-evaluator |
| `insert_select_exists` | 5 | INSERT SELECT with EXISTS filter |
| `limit_exists` | 2 | LIMIT with EXISTS subquery |
| `nested_derived_table` | 16 | Nested derived tables (Java's null-operator-tests.yamsql): |
| `normalized_exists_predicates` | 3 | OR predicates combined with EXISTS subqueries that benefit from CNF |
| `not_exists_or` | 2 | NOT EXISTS combined with OR predicates |
| `not_exists_predicates` | 5 | NOT EXISTS with various predicate shapes |
| `scalar_subquery` | 8 | Scalar subquery: `(SELECT ...)` used as a value-returning expression. |
| `scalar_subquery_advanced` | 10 | Edge-case probes for the scalar-subquery feature added in nightshift-39. |
| `scalar_subquery_dml` | 8 | Scalar subquery on the right-hand side of UPDATE SET, in DELETE WHERE |
| `scalar_subquery_java` | 5 | Scalar subqueries in SELECT and WHERE. |
| `scalar_subquery_projection` | 3 | Scalar subquery in SELECT projection |
| `scalar_subquery_types` | 9 | Type-coverage probes for scalar subqueries: the cached value flows |
| `self_not_exists` | 3 | NOT EXISTS on the same table (self-referential) |
| `subquery_exists_complex` | 4 | Complex EXISTS subquery patterns |
| `subquery_in` | 11 | `col IN (subquery)` is rejected at predicate evaluation time. |
| `subquery_in_from` | 3 | Subquery in FROM clause (derived table) |
| `subquery_scalar_in_where` | 3 | Scalar subquery used in WHERE predicate |
| `update_correlated_exists` | 4 | UPDATE with correlated EXISTS |

## CTEs

| Scenario | Cases | What it pins |
|---|--:|---|
| `cte` | 21 | WITH ... |
| `cte_aggregate` | 4 | CTE materialization + GROUP BY aggregation. |
| `cte_error_codes` | 6 | Java's cte.yamsql error tests: CTE-specific validation errors. |
| `cte_java_patterns` | 8 | CTE patterns from Java's cte.yamsql. |
| `cte_multi_reference` | 2 | CTE referenced multiple times |
| `cte_recursive_tree` | 3 | Recursive CTE tree traversal |
| `cte_with_insert` | 2 | CTE used in INSERT ... |
| `recursive_cte` | 26 | WITH RECURSIVE CTEs — semi-naive (level-order) evaluation. |
| `recursive_cte_advanced` | 2 | Advanced recursive CTE edge cases — regression guards for column alias |
| `recursive_cte_aggregate` | 3 | Recursive CTE combined with aggregation — exercises the interaction |
| `recursive_cte_tree_java` | 4 | Recursive CTE for tree traversal. |
| `update_dml_cte` | 4 | UPDATE with WITH clause and UPDATE/DELETE using CTE in WHERE. |

## Set operations (UNION / INTERSECT / EXCEPT)

| Scenario | Cases | What it pins |
|---|--:|---|
| `composite_aggregate_intersection` | 7 | Multi-aggregate queries using |
| `union` | 2 | UNION / UNION ALL — set operations over SELECT results. |
| `union_aggregate_java` | 7 | Aggregate over UNION ALL patterns from |
| `union_columns` | 11 | UNION column-binding: SQL standard is positional, not name-based. |
| `union_comprehensive` | 4 | Comprehensive UNION tests |
| `union_empty_tables_java` | 9 | UNION ALL behavior on empty tables |
| `union_star` | 5 | Java's union.yamsql tests UNION ALL with SELECT * on either side. |
| `union_with_aggregate` | 2 | UNION combined with aggregates |

## DML (INSERT / UPDATE / DELETE)

| Scenario | Cases | What it pins |
|---|--:|---|
| `case_when_update_java` | 8 | CASE WHEN in UPDATE SET from Java's |
| `delete_all_rows` | 6 | DELETE all rows from table |
| `delete_complex_where` | 7 | DELETE with complex WHERE predicates |
| `dml_conditional` | 6 | Conditional DML operations |
| `dml_error_codes` | 9 | Error codes for DML operations aligned with Java behavior. |
| `dml_returning_probes` | 5 | Probes for DML RETURNING clause (Postgres / Java fdb-relational |
| `dml_with_null_safe` | 7 | DML (UPDATE / DELETE) with IS NOT DISTINCT FROM in WHERE — the |
| `insert_arity` | 7 | INSERT column count mismatches (Java's inserts-updates-deletes.yamsql): |
| `insert_default_values` | 4 | INSERT with NULL for optional columns |
| `insert_multi_row` | 7 | Multi-row INSERT variations |
| `insert_multi_row_java` | 11 | Multi-row INSERT patterns. |
| `insert_returning` | 4 | INSERT with verification queries |
| `insert_select` | 15 | INSERT INTO ... |
| `insert_select_complex` | 2 | INSERT ... |
| `insert_select_java` | 10 | INSERT...SELECT patterns. |
| `insert_select_transform` | 2 | INSERT ... |
| `insert_values_expr` | 11 | INSERT INTO t VALUES with expressions (arithmetic, CASE, CAST, etc). |
| `multi_insert_delete` | 6 | Multiple INSERT/DELETE/UPDATE operations |
| `update_case_when` | 10 | UPDATE SET col = CASE ... |
| `update_comprehensive` | 8 | Comprehensive UPDATE patterns |
| `update_computed_multi` | 5 | Verifies multi-column UPDATE with self-referencing SET expressions. |
| `update_delete` | 14 | UPDATE and DELETE with NULL-aware predicates. |
| `update_delete_where_java` | 12 | UPDATE and DELETE with complex WHERE. |
| `update_set_expr` | 15 | UPDATE ... |
| `update_where_in` | 3 | UPDATE with IN-list WHERE condition |

## Ordering & pagination

| Scenario | Cases | What it pins |
|---|--:|---|
| `e2e_order_management` | 5 | End-to-end order management |
| `index_scan_order` | 5 | Index scan ordering (ASC/DESC) |
| `limit_offset_java` | 10 | LIMIT and OFFSET patterns. |
| `limit_zero` | 3 | LIMIT 0 edge case |
| `offset` | 8 | LIMIT / OFFSET (Go extension) |
| `order_by_complex` | 3 | Complex ORDER BY patterns |
| `order_by_dupe_col` | 4 | Java's orderby.yamsql: `ORDER BY b, b` (same column repeated) errors |
| `order_by_elimination` | 43 | ORDER BY elimination: when the chosen scan cursor emits rows in a |
| `order_by_expression` | 4 | ORDER BY <non-aggregate expression> — `ORDER BY a + b`, `ORDER BY |
| `order_by_index` | 4 | ORDER BY on indexed columns. |
| `order_by_limit` | 13 | ORDER BY with LIMIT — common query pattern. |
| `order_by_nulls` | 4 | Java-conformant NULL ordering (swingshift-35, 3b87574d): |
| `order_by_nulls_java` | 8 | ORDER BY with NULL values and multiple |

## Scalar functions & expressions

| Scenario | Cases | What it pins |
|---|--:|---|
| `arithmetic` | 22 | swingshift-35 commit ad249d55: applyMathOp and applyArithmeticOp |
| `bitwise` | 7 | Bitwise operators: &, \|, ^, <<, >>. |
| `case_insensitive_keywords` | 9 | SQL standard says keywords are case-insensitive. |
| `case_when` | 11 | CASE WHEN ... |
| `case_when_in_java` | 5 | CASE WHEN with IN predicate from Java's |
| `cast` | 16 | swingshift-35 commits 1acc097b/258073ee/13f43b58: CAST Java-conformance. |
| `cast_scalar_java` | 11 | Scalar CAST patterns from Java's cast-tests.yamsql. |
| `coalesce_nullif` | 3 | COALESCE(v1, v2, ...) returns the first non-NULL argument, or NULL |
| `datetime_functions` | 27 | Two groups: |
| `function_in_predicate` | 5 | Functions used in WHERE predicates |
| `greatest_least` | 11 | swingshift-35 commit 97e0c731: GREATEST / LEAST propagate NULL |
| `in_expression_types` | 6 | IN predicate with various expression types |
| `like` | 16 | LIKE pattern matching with SQL wildcards (% and _). |
| `like_patterns` | 5 | LIKE pattern matching |
| `like_patterns_java` | 10 | LIKE/NOT LIKE pattern matching. |
| `like_prefix_pushdown` | 41 | LIKE prefix pushdown: `WHERE col LIKE 'foo%'` on a STRING column |
| `null_arithmetic` | 5 | NULL propagation in arithmetic expressions |
| `null_arithmetic_java` | 9 | NULL propagation through arithmetic |
| `null_in_expressions` | 6 | NULL behavior in various expression contexts |
| `nullif_coalesce_combined_java` | 6 | NULLIF and COALESCE combined |
| `numeric_functions` | 28 | Scalar numeric functions: ABS / MOD / FLOOR / CEIL / CEILING / ROUND / |
| `numeric_overflow_detection` | 5 | Numeric overflow detection |
| `overflow` | 10 | nightshift-36: integer overflow is now checked. |
| `overflow_mixed` | 3 | Follow-up probe for `feedback_next_shift_arithmetic_overflow` (which |
| `scalar_functions_java` | 17 | Scalar function patterns from Java's |
| `select_constant_expression` | 3 | Constant expressions in SELECT |
| `select_expression_projection` | 4 | Computed columns in SELECT |
| `select_expressions_java` | 9 | SELECT with various expression types. |
| `string_functions` | 13 | STRING-family scalar functions: UPPER / LOWER / LENGTH / CHAR_LENGTH / |
| `string_functions_java` | 11 | String function patterns. |
| `trim_concat` | 10 | TRIM / LTRIM / RTRIM / CONCAT / REPLACE — a Go-only read-side extension |
| `window_function_probes` | 3 | DIAGNOSTIC ONLY: probes window function syntax. |

## Predicates & WHERE

| Scenario | Cases | What it pins |
|---|--:|---|
| `between` | 17 | swingshift-35 commit 8ee5e98d: BETWEEN / NOT BETWEEN Kleene short-circuit. |
| `between_edge_cases` | 5 | BETWEEN edge cases |
| `between_java` | 16 | BETWEEN patterns from Java's between.yamsql. |
| `complex_where_java` | 10 | Complex WHERE clause combinations. |
| `distinct_from_java` | 11 | IS [NOT] DISTINCT FROM patterns from |
| `is_distinct_from` | 12 | IS DISTINCT FROM / IS NOT DISTINCT FROM — NULL-safe equality |
| `map_path_predicate_kleene` | 7 | Pins the map-path (JOIN / CTE / HAVING) predicate evaluator's |
| `multi_predicate_push` | 3 | Multiple predicates with push-down |
| `multiple_where_predicates` | 4 | Multiple WHERE predicates |
| `where_complex_predicates` | 5 | Complex WHERE predicate combinations |
| `where_literal_on_left` | 10 | Java has tests with the literal on the LEFT side of comparison |
| `where_or_optimization` | 4 | WHERE with OR predicates |

## Column resolution & aliasing

| Scenario | Cases | What it pins |
|---|--:|---|
| `alias_resolution` | 5 | Alias resolution edge cases |
| `ambiguous_column` | 13 | Java's join-tests.yamsql: SELECT unqualified column that appears in |
| `qualified_star` | 13 | SELECT <tbl>.* on a multi-source FROM clause restricts the projected |
| `qualified_star_more` | 4 | More qualifier-star edge cases from Java's select-a-star.yamsql: |
| `unknown_qualifier` | 6 | Java's SemanticAnalyzer rejects qualified column references whose |
| `wrong_qualifier` | 14 | `SELECT a.id, c.label FROM a, b` where no source is named `c`. |

## NULL handling

| Scenario | Cases | What it pins |
|---|--:|---|
| `not_in_null_behavior` | 2 | NOT IN with NULL values edge cases |
| `not_null_constraint_java` | 7 | NOT NULL constraint enforcement. |
| `not_null_violation` | 3 | swingshift-35 commits 1f389611/e9959ba9/38410fec: INSERT/UPDATE NULL |
| `null_operator_alignment` | 7 | NULL operator tests aligned with Java's null-operator-tests.yamsql. |
| `where_is_null_is_not_null` | 7 | IS NULL / IS NOT NULL predicates |

## NULL handling & boolean logic

| Scenario | Cases | What it pins |
|---|--:|---|
| `boolean` | 30 | Subset of fdb-record-layer/yaml-tests/src/test/resources/boolean.yamsql |
| `boolean_3vl_java` | 18 | Boolean three-valued logic from Java's |

## Index usage

| Scenario | Cases | What it pins |
|---|--:|---|
| `composite_secondary_index_prefix_pushdown` | 11 | Pure-prefix pushdown on composite secondary indexes: when WHERE |
| `covering_index_java` | 7 | Covering index optimization. |
| `covering_index_pushdown` | 24 | Covering-index pushdown: when every column the SELECT reads from each |
| `index_range_and_or` | 10 | Port of Java standard-tests.yamsql — AND/OR range predicates with index. |
| `index_range_predicates_java` | 10 | Index scan with range predicates |
| `index_scan_direction` | 8 | Index scan direction tests |
| `multi_column_index_java` | 7 | Multi-column (composite) index patterns. |
| `secondary_index_pushdown` | 80 | Secondary-index pushdown: `SELECT ... |
| `unique_index_violation` | 5 | Tests that unique index constraints are enforced. |

## Types

| Scenario | Cases | What it pins |
|---|--:|---|
| `array_column_type` | 5 | ARRAY column types. |
| `bytes` | 14 | BYTES column type — hex literals (x'DEADBEEF'), comparisons, IN, |
| `datetime_column_types` | 43 | Go extension: DATE and TIMESTAMP column types. |
| `integer_column_types` | 27 | Comprehensive INTEGER (INT32) column type coverage: |
| `mixed_type_equality` | 5 | swingshift-35 commit 6853cee5: valuesEqual and compareValues no longer |
| `numeric_types` | 8 | Arithmetic across numeric column types — pins that: |
| `select_where_comparison_types` | 6 | WHERE with all comparison operators |
| `type_coercion_comparison` | 5 | Type coercion in comparisons |
| `type_coercion_java` | 11 | Implicit type coercion in comparisons |
| `type_mismatch_alignment` | 7 | Java's ExceptionUtil.translateErrorCode maps |
| `type_promotion` | 8 | Verifies implicit type promotion in comparisons and arithmetic. |
| `uuid_column` | 5 | UUID column type. |

## Keys & primary keys

| Scenario | Cases | What it pins |
|---|--:|---|
| `composite_pk` | 4 | Composite PRIMARY KEY (col1, col2). |
| `composite_pk_java` | 10 | Composite primary key patterns. |
| `composite_pk_prefix_pushdown` | 15 | Pure-prefix composite PK pushdown: equalities on a leading subset |
| `pk_pushdown` | 96 | Primary-key equality pushdown: queries of the form |
| `pk_range_scan` | 7 | PK range scan with comparison operators |

## Error codes & validation

| Scenario | Cases | What it pins |
|---|--:|---|
| `error_code_regression` | 17 | Comprehensive error-code regression test covering all SQLSTATE codes |
| `error_codes_java` | 10 | SQL error code conformance. |
| `unique_constraint_violation` | 4 | Unique constraint error handling |
| `unique_violation` | 6 | UNIQUE constraint violations raise SQLSTATE 23505 per the SQL |

## End-to-end scenarios

| Scenario | Cases | What it pins |
|---|--:|---|
| `e2e_ecommerce` | 9 | end-to-end ecommerce scenario |
| `e2e_inventory` | 5 | End-to-end inventory management scenario |
| `e2e_user_sessions` | 6 | End-to-end user session tracking |

## Other

| Scenario | Cases | What it pins |
|---|--:|---|
| `bare_col_with_agg` | 9 | SQL §7.10 GR1: when a SELECT list contains an aggregate function, |
| `bug_hunt_probes` | 13 | Throwaway probes targeting features likely to surface bugs: |
| `cascades_plan_shapes` | 9 | Tests that verify the Cascades planner produces correct results for |
| `comparison_edge_cases` | 9 | Edge cases in comparison operators |
| `computed_column_names` | 6 | Verifies that unnamed computed expressions in SELECT projections |
| `empty_result_edge_cases_java` | 11 | Empty result handling in various |
| `empty_table_operations` | 9 | Operations on empty tables |
| `float_column` | 10 | FLOAT (32-bit) column type. |
| `information_schema` | 5 | INFORMATION_SCHEMA.* system-table queries. |
| `integer_range` | 12 | INTEGER (32-bit) column range enforcement. |
| `java_alignment_probes` | 14 | Probes derived from Java's yamsql test suite to verify Go matches |
| `min_max_string` | 3 | MIN/MAX on string columns |
| `mixed_agg_nonagg` | 4 | Mixed aggregate and non-aggregate expressions |
| `multi_feature` | 3 | End-to-end scenario chaining several features at once: CTE + WHERE + |
| `multi_feature_integer` | 11 | Integration tests combining multiple SQL features against INTEGER (INT32) |
| `multi_operator_pipeline` | 6 | Tests that exercise multiple Cascades operators working together |
| `negative_values` | 6 | Negative numbers and zero edge cases |
| `select_no_from` | 6 | FROM-less SELECT — fdb-relational 4.11.1.0's QueryVisitor.visitSimpleTable |
| `select_star_single_table` | 4 | SELECT * from single table |
| `string_comparison` | 5 | String comparison edge cases |

