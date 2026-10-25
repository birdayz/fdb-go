# SQL Conformance Matrix

Java fdb-relational **4.14.2.0** vs Go implementation vs ANSI SQL standard. The Java and Go
columns are measured against a live 4.14.2.0 JVM: `DocClaimsJavaProbe`
(`conformance/doc_claims_java_probe_test.go`, in `//conformance:conformance_probes_test`) runs one
query per row on both engines and pins both answers. Each row's notes name its probe cases.
Rows measured elsewhere name their test, and rows read from Java source say so. Every known
difference is in DIVERGENCES.md. The run of Java's own yamsql corpus against Go is under
[Yamsql Conformance](#yamsql-conformance).

**Y** = works, **N** = not supported, **P** = partial, **Ext** = Go-only extension

## DML

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| SELECT | Y | Y | Y | `select` |
| INSERT VALUES | Y | Y | Y | Every probe's setup |
| INSERT...SELECT | Y | Y | Y | `insert_select`. Column-list form rejected by both, 0AF00 (`insert_select_column_list`) |
| UPDATE | Y | Y | Y | `update`. Multi-column self-ref correct |
| DELETE | Y | Y | Y | `delete` |

## Expressions

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| Arithmetic (+, -, *, /, %) | Y | Y | Y | `arithmetic` |
| Searched CASE (`CASE WHEN cond`) | Y | Y | Y | `searched_case` |
| Simple CASE (`CASE expr WHEN val`) | N | Y | Y | Java accepts the syntax but mis-evaluates: `visitCaseExpressionFunctionCall` is a no-op that answers the ELSE for every row (`simple_case`), or the last THEN with no ELSE (`simple_case_no_else`); Go evaluates correctly (`case_simple_int_match` divergence) |
| CAST | Y | Y | Y | `cast`. Overflow is 22F3H in both (`cast_overflow`) |
| COALESCE | Y | Y | Y | `coalesce` |
| NULLIF | N | N | Y | Both reject with 0AF00 `Unsupported operator NULLIF` (`nullif`); shared ANSI gap F261-03 |
| GREATEST / LEAST | Y | Y | Y | `greatest_least` |
| CARDINALITY (array length, `ln`) | Y | Y | Y | `cardinality`. Scalar fn + index support; Go ports both (RFC-143) |
| String functions (UPPER etc.) | N | Ext | Y | Java: 0AF00 `Unsupported operator UPPER` (`upper`). Go extension (RFC-087): UPPER/LOWER/SUBSTRING/TRIM/CONCAT/REPLACE/POSITION/REVERSE/`*_LENGTH` (`string_functions.yaml`, `trim_concat.yaml`); read-side only, zero wire impact. Go's POSITION takes two arguments: the ANSI `POSITION(x IN s)` is 0AF00 in Go and in Java answers `s` unchanged (`position_in`) |
| Math functions (ABS etc.) | N | Ext | Y | Java: 0AF00 `Unsupported operator ABS` (`abs`). Go extension: ABS/MOD/FLOOR/CEIL(ING)/ROUND/SQRT (`numeric_functions.yaml`), with typed error channels (ABS(MinInt64)→22003, integral MOD(x,0)→22012; floating MOD(x,0)→NaN (RFC-253), SQRT(<0)→22023) |
| CURRENT_TIMESTAMP / CURRENT_DATE | N | Ext | Y | Java: XX000 `expected Expression but got null` (`current_timestamp`, `current_date`). Go extension: proper TIMESTAMP/DATE types, comparisons, CAST |
| Date-part functions (YEAR etc.) | N | Ext | Y | Java: 0AF00 `Unsupported operator YEAR`, and no temporal value to apply one to (`date_part_of_integer`, `date_part`). Go extension: YEAR/MONTH/DAY/HOUR/MINUTE/SECOND/DAYOFWEEK/DAYOFYEAR; over a BIGINT, YEAR is NULL. `EXTRACT(YEAR FROM v)` is 0AF00 in Go and in Java answers `v` unchanged (`extract_year`) |
| `&&`, `\|\|`, `XOR` | N | Ext | -- | Java: 0AF00 `Unsupported operator &&` / `\|\|` / `XOR` (`symbolic_and`, `symbolic_or`, `xor`). Go evaluates them as AND/OR/XOR |

## Predicates

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| WHERE / AND / OR / NOT | Y | Y | Y | `and_or_not`. Tri-state (Kleene) |
| Parenthesized WHERE condition (`WHERE (v = 10)`) | P | Y | Y | Java reads `( … )` as a one-field record and refuses a WHERE that is one parenthesized condition, 42804 (`where_paren`); parenthesized operands of AND/OR answer in both (`where_paren_compound`) |
| BETWEEN | Y | Y | Y | `between` |
| IN (literal list) | Y | Y | Y | `in_list` |
| IN (subquery) | N | N | Y | Both reject with 0AF00: Java `IN predicate does not support nested SELECT`, Go `Cascades planner could not plan query` (`in_subquery`) |
| LIKE / prefix pushdown | Y | Y | Y | `like` |
| IS NULL / IS NOT NULL | Y | Y | Y | `is_null` |
| IS DISTINCT FROM | Y | Y | Y | `is_distinct_from` |
| Byte literal predicates (x'cafe') | Y | Y | Y | `byte_literal` |

## Aggregation

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| GROUP BY column | P | Y | Y | Java plans a grouping only over input ordered on the grouping columns, by an index or the primary key (`group_by_ordered`, `group_by_primary_key`); otherwise 0AF00 `Cascades planner could not plan query` (`group_by_unordered`). Go also hash-aggregates unordered input |
| GROUP BY expression | P | Y | Y | Java groups an expression only through an aggregate index on it (Java's `aggregate-index-tests.yamsql:347-350`); otherwise 0AF00 (`group_by_expression`) |
| COUNT / SUM / AVG / MIN / MAX | Y | Y | Y | `aggregates` |
| HAVING | Y | Y | Y | `having` (over an ordered grouping) |
| COUNT(DISTINCT col) | N | N | Y | Both reject with 0AF00 (`count_distinct`) |
| Empty-table aggregates | Y | Y | Y | `empty_aggregates`: 0 for COUNT, NULL for SUM |

## Set Operations

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| UNION ALL | Y | Y | Y | `union_all` |
| UNION (dedup) | N | N | Y | Both 0AF00 `only UNION ALL is supported` (`union_distinct`) |
| INTERSECT / EXCEPT | N | N | Y | Both 42601 (`intersect`, `except`) |
| ORDER BY on UNION result | N | Y | Y | Java sorts only the right leg for a named key (`UnionTrailingOrderByJavaProbe`) and cannot plan a positional one, 0AF00 (`union_order_by_position`). Go sorts the combined result |

## Joins

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| INNER JOIN (explicit + comma) | Y | Y | Y | `inner_join`, `comma_join` |
| CROSS JOIN | N | Ext | Y | Java crashes on any join without ON, XXXXX NullPointerException (`cross_join`, `JoinWithoutOnJavaProbe`); Go answers the cartesian product |
| LEFT OUTER JOIN | Y | Y | Y | `left_join`. Go plans via NLJ with JoinLeftOuter |
| RIGHT OUTER JOIN | Y | Y | Y | `right_join`. Go has no RIGHT join type: the translator rewrites `a RIGHT JOIN b` to `b LEFT JOIN a` by swapping the operands (`cascades_translator.go`, `kind = logical.JoinLeft`; `SELECT *` keeps declaration order via the pre-swap legs), planned as a materialized NLJ with `JoinLeftOuter` |
| FULL OUTER JOIN | N | Ext | Y | Java: 42601, the grammar accepts only LEFT/RIGHT (`full_join`); without OUTER, Java reads `FULL` as the left table's alias and answers an inner join (`full_join_without_outer`). Go-only extension via NLJ with JoinFullOuter |
| Self-join | Y | Y | Y | `self_join` |
| 3+ way join | Y | Y | Y | `three_way_join` |

## Subqueries

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| Scalar subquery | N | Ext | Y | Java: 42601, the grammar has no `subqueryExpressionAtom` (`scalar_subquery`) |
| EXISTS / NOT EXISTS | Y | Y | Y | `exists`, `not_exists`, and in the projection list (`exists_projection`) |
| Correlated subquery | P | P | Y | Correlated EXISTS works in both. A correlated scalar subquery is 42601 in Java (`correlated_scalar_subquery`) and answered by Go; the shapes Go refuses are under DIVERGENCES.md "Optimization-Quality Gaps" |
| Derived table (FROM subquery) | Y | Y | Y | `derived_table` |
| Correlated array UNNEST in FROM (+ ordinal) | Y | Y | Y | `FROM t, t.arr AS e [AT ordinal]` (`unnest`, `unnest_ordinality`); Go ports the lateral array unnest + ordinality index (RFC-142) |

## CTEs

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| WITH (basic) | Y | Y | Y | `with` |
| WITH column rename | Y | Y | Y | `with_column_rename` |
| Chained CTEs | Y | Y | Y | `with_chained` |
| WITH RECURSIVE (level-order) | Y | Y | Y | `recursive_level_order`. RecursiveLevelUnionPlan |
| Recursive DFS order | Y | Y | P | `TRAVERSAL ORDER PRE_ORDER` / `POST_ORDER` (`recursive_pre_order`; Java's `recursive-cte.yamsql`) |
| WITH RECURSIVE without a self-reference | N | N | Y | Both refuse with `condition is not met!`, with or without a UNION body: Java XX000, Go 0A000 (`recursive_without_self_reference`, `recursive_union_without_self_reference`) |
| UNION DISTINCT in recursive CTE | N | Y | Y | Java accepts it and runs UNION ALL: a node reached twice is returned twice, where Go returns it once (`recursive_union_distinct_diamond`). Over a tree, where no node repeats, both answer alike (`recursive_union_distinct`) |

## Ordering / Limiting / Distinct

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| ORDER BY (index-backed) | Y | Y | Y | `order_by_index`. Sort elimination via ImplementSortRule (PLANNING phase, D-1 aligned) |
| ORDER BY (no index) | N | Ext | Y | Java: 0AF00 (`order_by_no_index`). Go extension: in-memory sort |
| ORDER BY aggregate | P | Y | Y | Java orders by an aggregate only through a permuted aggregate index (Java's `aggregate-index-tests.yamsql:381-386`); otherwise 0AF00 (`order_by_aggregate`). Go sorts |
| NULLS FIRST / LAST | P | Y | Y | Java answers only the null order an index provides: NULLS FIRST over an ascending index (`order_by_nulls_first`), 0AF00 for NULLS LAST over it (`order_by_nulls_last`). Go puts the NULL where either clause asks |
| LIMIT / OFFSET | N | Ext | Y | Java: 0AF00 `LIMIT clause is not supported.` / `OFFSET clause is not supported.` (`limit`, `limit_offset`). Go answers them on the outer SELECT and in a derived table (`limit_in_subquery`) |
| SELECT DISTINCT | N | Y | Y | Java plans DISTINCT but returns every row, duplicates included, whether or not an index orders the column (`distinct_ordered`, `distinct_unordered`); DIVERGENCES.md "Java Upstream Bugs". Go deduplicates (ImplementDistinctFinalRule, D-3 aligned) |
| DISTINCT + ORDER BY | N | Y | Y | Same Java defect (`distinct_order_by`) |

## Schema / Types

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| BIGINT / INTEGER / DOUBLE / STRING / BOOLEAN | Y | Y | Y | `core_types` |
| BYTES | Y | Y | -- | `bytes`. FDB-specific, not ANSI |
| DATE / TIMESTAMP | N | Ext | Y | Java: a DATE/TIMESTAMP column is 42F18 (`date_column`, `timestamp_column`), a CAST to either is 42601 (`cast_date`, `cast_timestamp`). Go extension: temporal values, CAST, comparisons, YEAR/MONTH/DAY; a column spelled DATE/TIMESTAMP is stored as STRING (DIVERGENCES.md "Go-Only Extensions") |
| ARRAY column type | Y | Y | Y | `array_column`. All primitive element types: STRING/BIGINT/INTEGER/DOUBLE/FLOAT/BOOLEAN/BYTES/UUID ARRAY |
| NOT NULL on a scalar column | N | N | Y | Both reject the CREATE with 0A000 `NOT NULL is only allowed for ARRAY column type` (`scalar_not_null`) |
| CREATE TABLE / INDEX | Y | Y | Y | Every probe's schema |
| Qualified table names (template.table) | Y | Y | P | The qualifier is the schema template's name, where ANSI's is the schema's (`ws_f_table_qualifier_conformance_test.go`) |
| INFORMATION_SCHEMA | N | Ext | Y | Java: 42703 `Unknown reference INFORMATION_SCHEMA.TABLES` (`information_schema_tables`, `information_schema_columns`). Go extension. `TABLES` is a keyword in both grammars and must be quoted |

## Error Code Conformance

| Error | SQLSTATE | Java | Go | Notes |
|---|---|:---:|:---:|---|
| Unknown table | 42F01 | Y | Y | `unknown_table` |
| Unknown column | 42703 | Y | Y | `unknown_column` |
| Ambiguous column | 42702 | Y | Y | `ambiguous_column` |
| Unknown qualifier | 42703 | Y | Y | `unknown_qualifier` |
| Type mismatch in comparison | 42804 | Y | Y | `comparison_type_mismatch` |
| Type mismatch in BETWEEN | 42804 | Y | Y | `between_type_mismatch` |
| IN-list type mismatch | 22000 | Y | Y | `in_list_type_mismatch`: both report a value that cannot be promoted, not 42804 |
| GROUP BY violation | 42803 | Y | Y | `group_by_violation`. Non-grouped column in SELECT |
| Duplicate ORDER BY | 42701 | Y | Y | `duplicate_order_by` |
| UNION column mismatch | 42F64 | Y | Y | `union_column_mismatch` |
| UNION type mismatch | 42F65 | Y | Y | `union_type_mismatch` |
| NOT NULL violation | 23502 | P | Y | A NOT NULL array column left out of the INSERT: 23502 in both (`not_null_omitted`). An explicit NULL into it: Java XX000 `Cannot set a non-nullable field to the NULL value`, Go 23502 (`not_null_violation`) |
| PK constraint | 23505 | Y | Y | `primary_key_violation` |
| Unique index violation | 23505 | Y | Y | `unique_index_violation` |
| Integer overflow | 22003 | N | Y | Java leaks `ArithmeticException` as XXXXX (`integer_overflow`); 22003 is a Go-only code (DIVERGENCES.md "Go-only SQLSTATEs") |
| INT32 range overflow | 22000 | Y | Y | `int32_overflow`: INSERT of a value > MAX_INT32 into an INTEGER column |
| Unknown scalar function | 0AF00 | Y | Y | `unknown_function`: `Unsupported operator NOSUCHFN` in both |
| Unknown table function | 42883 | Y | Y | `unknown_table_function` |
| FDB transaction timeout | 53F00 | Y | Y | Read from source, not probed: Java `ExceptionUtil.java:64-65`; Go translateFDBError |
| FDB transaction conflict | 40001 | Y | Y | Read from source, not probed: Java `RecordContextTransaction.java:67-68`; Go translateFDBError |
| Deserialization failure | XXF01 | Y | Y | Read from source, not probed: Java `ExceptionUtil.java:68-69`; Go RecordDeserializationError → translateFDBError |

## Summary

> **Measured numbers live in the generated, drift-guarded ledgers (RFC-165), not here:**
> - `SQL_COVERAGE.md` (Ledger B) — measured corpus coverage (% of test cases that pass).
> - `SQL_ANSI_CONFORMANCE.md` (Ledger A) — ANSI SQL:2023 Core scorecard, Go support *derived* from `# ansi:` corpus tags.
>
> The table below is a **qualitative** Java-vs-Go narrative only (the old hand-typed `~%` ANSI-coverage
> column has been deleted — it was fabricated). For any percentage, defer to the ledgers (`just sql-coverage`).

| Category | Java 4.14.2.0 | Go |
|---|---|---|
| Core DML | Full | Full |
| Expressions | No string, math or temporal functions; simple CASE mis-evaluates | + string, math and temporal functions, `&&`/`\|\|`/`XOR` (Go extensions) |
| Predicates | Full except IN (subquery) | Full except IN (subquery) |
| Aggregation | Grouping only over input an index or the primary key orders; no COUNT(DISTINCT) | Any grouping (hash aggregation); no COUNT(DISTINCT) |
| Set operations | UNION ALL only | UNION ALL only |
| Joins | INNER, comma, LEFT/RIGHT OUTER; a join without ON crashes; FULL OUTER rejected | + CROSS JOIN, FULL OUTER (Go extensions) |
| Subqueries | EXISTS (correlated, nested, in the projection list); no scalar subquery | + scalar subqueries, correlated included (Go extension) |
| CTEs | Full + recursive (level, pre- and post-order); a recursive UNION does not deduplicate | Full + recursive, UNION deduplicates |
| Ordering | Index-provided order only; no LIMIT/OFFSET | Full (in-memory sort, LIMIT/OFFSET: Go extensions) |
| DISTINCT | Planned but does not deduplicate | Correct |
| Types | Core types | + DATE/TIMESTAMP (Go extension) |
| Error codes | Mapped by ExceptionUtil | ExceptionUtil 1:1 port, plus 22003 and 23502 where Java reports XXXXX and XX000 |

Go is more capable than Java 4.14.2.0 in ordering (in-memory sort, LIMIT/OFFSET), grouping over
unordered input, scalar subqueries, CROSS and FULL OUTER JOIN, string, math and temporal functions,
and INFORMATION_SCHEMA. Java's DISTINCT and recursive UNION do not deduplicate; Go's do. Neither
engine supports UNION with deduplication, INTERSECT, EXCEPT, IN (subquery), NULLIF or
COUNT(DISTINCT).

## Yamsql Conformance

The yamsql corpus is the conformance harness. **Do not hand-type the counts here** — they rot
(this line previously read "115/115" long after the corpus passed 300 files). The current,
measured numbers are generated into `SQL_COVERAGE.md` (corpus pass rate) and
`SQL_ANSI_CONFORMANCE.md` (ANSI Core scorecard), each guarded by a drift test that fails CI if
stale. Run `just sql-coverage` to regenerate; see `FEATURE_MATRIX.md` for the scenario inventory.

<!-- BEGIN javacorpus ledger: rendered by TestSQLConformanceStatesTheLedger -->
Java's own yamsql corpus (fdb-record-layer 4.14.2.0, 255 files) runs against Go in
`pkg/relational/conformance/javacorpus`, pinned by `pinned_ledger_test.go`:

- **150 files pass, 0 fail, 105 are skipped whole**, by class: polarity:negative-execution 37, polarity:negative-parse 25, conformance:go-accepts-what-java-rejects 15, polarity:fixed-version-meta 11, conformance:java-planner-bug 4, plan-assertion 4, unsupported:continuation 3, fragment 2, unsupported:multi-cluster 2, conformance:java-disabled 1, vacuous:all-assertions-skipped 1.
- A pass means every assertion that ran held; plan assertions never run (RFC-201 ruling 2).
  The 230 files that parse carry 1259 of them (`explain` 1209, `explainContains` 50, `planHash` 0)
  in 82 files; 5002 executions of them were skipped.
- Other assertions skipped, per execution: unsupported:continuation 152, unsupported:random-injection 30, no-checks 8.
- 9344 query executions were asserted. By default a test block runs each query five times, plus a plan-cache check.
<!-- END javacorpus ledger -->

## Cascades Divergence Status

See `DIVERGENCES.md` — the authoritative Go-vs-Java divergence doc (Cascades planner included).
