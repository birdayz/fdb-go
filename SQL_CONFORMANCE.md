# SQL Conformance Matrix

Java fdb-relational **4.11.1.0** vs Go implementation vs ANSI SQL standard.

**Y** = works, **N** = not supported, **P** = partial, **Ext** = Go-only extension

## DML

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| SELECT | Y | Y | Y | |
| INSERT VALUES | Y | Y | Y | |
| INSERT...SELECT | Y | Y | Y | Column-list form rejected (Java-aligned) |
| UPDATE | Y | Y | Y | Multi-column self-ref correct |
| DELETE | Y | Y | Y | |

## Expressions

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| Arithmetic (+, -, *, /, %) | Y | Y | Y | |
| CASE WHEN / simple CASE | Y | Y | Y | |
| CAST | Y | Y | Y | Overflow detection aligned |
| COALESCE / NULLIF | Y | Y | Y | |
| GREATEST / LEAST | Y | Y | Y | |
| String functions (UPPER etc.) | N | N | Y | Both reject -- Java has no function catalog entry |
| Math functions (ABS etc.) | N | N | Y | Both reject |
| CURRENT_TIMESTAMP (no parens) | N | Ext | Y | Go extension -- Java's visitSimpleFunctionCall is broken |
| Date-part functions (YEAR etc.) | N | N | Y | |

## Predicates

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| WHERE / AND / OR / NOT | Y | Y | Y | Tri-state (Kleene) |
| BETWEEN | Y | Y | Y | |
| IN (literal list) | Y | Y | Y | |
| IN (subquery) | N | N | Y | Both reject -- Java NPEs, Go clean error |
| LIKE / prefix pushdown | Y | Y | Y | |
| IS NULL / IS NOT NULL | Y | Y | Y | |
| IS DISTINCT FROM | Y | Y | Y | |
| Byte literal predicates (x'cafe') | Y | Y | Y | |

## Aggregation

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| GROUP BY column | P | Ext | Y | Java core has rules but fdb-relational 4.11.1.0 SQL layer doesn't wire GroupByExpression |
| GROUP BY expression | N | Ext | Y | Go extension -- Java's SQL layer can't produce it |
| COUNT / SUM / AVG / MIN / MAX | P | Ext | Y | Java core has StreamingAggregationRule; SQL layer gaps prevent most queries |
| HAVING | P | Ext | Y | Java core supports; SQL layer wiring incomplete |
| COUNT(DISTINCT col) | N | N | Y | Both reject (0A000) |
| Empty-table aggregates | P | Ext | Y | NULL for SUM/AVG, 0 for COUNT |

## Set Operations

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| UNION ALL | Y | Y | Y | |
| UNION (dedup) | N | N | Y | No Cascades rule in either engine |
| INTERSECT / EXCEPT | N | N | Y | |
| ORDER BY on UNION result | P | P | Y | Positional column mapping incomplete |

## Joins

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| INNER JOIN (explicit + comma) | Y | Y | Y | |
| CROSS JOIN | Y | Y | Y | |
| LEFT OUTER JOIN | Y | Y | Y | Via NLJ with JoinLeftOuter |
| RIGHT OUTER JOIN | Y | Y | Y | Rewritten to LEFT OUTER |
| FULL OUTER JOIN | N | N | Y | |
| Self-join | Y | Y | Y | |
| 3+ way join | Y | Y | Y | |

## Subqueries

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| Scalar subquery | Y | Y | Y | Uncorrelated scalar subqueries work |
| EXISTS / NOT EXISTS | Y | Y | Y | Correlated + nested EXISTS working (swingshift-81) |
| Correlated subquery | P | P | Y | Both have partial support |
| Derived table (FROM subquery) | Y | Y | Y | Column alias propagation working |

## CTEs

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| WITH (basic) | Y | Y | Y | |
| WITH column rename | Y | Y | Y | |
| Chained CTEs | Y | Y | Y | |
| WITH RECURSIVE (level-order) | Y | Y | Y | RecursiveLevelUnionPlan (swingshift-81) |
| Recursive DFS order | N | Y | P | Go extension |
| UNION DISTINCT in recursive CTE | Y | Y | Y | Cycle detection for recursive CTEs |

## Ordering / Limiting / Distinct

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| ORDER BY (index-backed) | Y | Y | Y | Sort elimination via ImplementSortRule (PLANNING phase, D-1 aligned) |
| ORDER BY (no index) | N | Ext | Y | Go extension: in-memory sort |
| ORDER BY aggregate | N | Ext | Y | Go extension |
| NULLS FIRST / LAST | Y | Y | Y | |
| LIMIT / OFFSET (clause) | N | N | Y | Both reject at parse time |
| SELECT DISTINCT | P | Ext | Y | Java rejects most DISTINCT; Go has ImplementDistinctFinalRule (D-3 aligned) |
| DISTINCT + ORDER BY | N | Ext | Y | Go extension |

## Schema / Types

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| BIGINT / INTEGER / DOUBLE / STRING / BOOLEAN | Y | Y | Y | |
| BYTES | Y | Y | -- | FDB-specific, not ANSI |
| DATE / TIMESTAMP / TIME | N | N | Y | Phase 5 TODO |
| CREATE TABLE / INDEX | Y | Y | Y | |
| INFORMATION_SCHEMA | N | Ext | Y | Go extension |

## Error Code Conformance

| Error | SQLSTATE | Java | Go | Notes |
|---|---|:---:|:---:|---|
| Unknown table | 42F01 | Y | Y | validateTablesAndColumns + SourceNotFoundError |
| Unknown column | 42703 | Y | Y | ColumnNotFoundError in WHERE/SELECT/ORDER BY |
| Ambiguous column | 42702 | Y | Y | JOIN ambiguity detection |
| Unknown qualifier | 42703 | Y | Y | SourceNotFoundError → 42703 (swingshift-83) |
| Type mismatch in comparison | 42804 | Y | Y | COMPARISON_OF_INCOMPATIBLE_TYPES → DATATYPE_MISMATCH |
| Type mismatch in BETWEEN | 42804 | Y | Y | Same as comparison |
| IN-list type mismatch | 42804 | Y | Y | Same as comparison |
| GROUP BY violation | 42803 | Y | Y | Non-grouped column in SELECT |
| Duplicate ORDER BY | 42701 | Y | Y | |
| UNION column mismatch | 42F64 | Y | Y | |
| UNION type mismatch | 42F65 | Y | Y | |
| NOT NULL violation | 23502 | Y | Y | |
| PK constraint | 23505 | Y | Y | |
| Integer overflow | 22003 | Y | Y | |
| Unsupported function | 42883 | Y | Y | |

## Summary

| Category | Java 4.11.1.0 | Go | ANSI coverage |
|---|---|---|---|
| Core DML | Full | Full | Full |
| Expressions | Partial (no scalar fns) | Partial + datetime ext | ~60% |
| Predicates | Full | Full + byte literals | ~90% |
| Aggregation | **Partial** (core rules exist, SQL layer gaps) | **Full** (Go extension) | ~85% |
| Set operations | UNION ALL only | UNION ALL only | ~25% |
| Joins | Full except FULL OUTER | Full except FULL OUTER | ~85% |
| Subqueries | Partial | Partial (EXISTS + scalar work, correlated partial) | ~70% |
| CTEs | Full + recursive | Full + recursive + DFS ext | ~90% |
| Ordering | Index-only | Full (in-memory sort ext) | ~80% |
| Types | Core types | Core types + BYTES | ~60% |
| Error codes | Full | Full | ~90% |

Go is more capable than Java 4.11.1.0 in aggregation, ordering, DISTINCT, and recursive CTEs. Go's main gap vs Java is **correlated subqueries** (needs DecorrelateValuesRule for the correlated case). Uncorrelated scalar subqueries and EXISTS work. Both engines lack string/math functions and DATE/TIMESTAMP types.

## Yamsql Conformance

105 scenario test suite. Current: **105/105 pass (100%)**.

## Cascades Divergence Status

See `CASCADES_DIVERGENCE.md` for the full audit. 5 divergences resolved (D-1, D-3, D-9, D-10, D-12). Remaining: D-2 (PushOrdering structural vs constraint, 2-3 shifts), D-4 (cost model, by design), D-5 (InComparison architecture), D-7 (multi-aggregate), D-8 (CardinalityProperty), D-11 (ConstantObjectValue promotion).
