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
| LEFT OUTER JOIN | Y | N | Y | Go missing ImplementOuterJoinRule |
| RIGHT OUTER JOIN | Y | N | Y | Same gap |
| FULL OUTER JOIN | N | N | Y | |
| Self-join | Y | Y | Y | |
| 3+ way join | Y | P | Y | Go has NLJ merger edge cases |

## Subqueries

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| Scalar subquery | Y | N | Y | Needs DecorrelateValuesRule port |
| EXISTS / NOT EXISTS | Y | N | Y | Needs SelectExpression + correlation |
| Correlated subquery | P | N | Y | Java has partial support |
| Derived table (FROM subquery) | Y | P | Y | Column alias propagation incomplete |

## CTEs

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| WITH (basic) | Y | Y | Y | |
| WITH column rename | Y | Y | Y | |
| Chained CTEs | Y | Y | Y | |
| WITH RECURSIVE (level-order) | Y | N | Y | Translator rejects -- infrastructure exists |
| Recursive pre/post/DFS order | N | N | P | |

## Ordering / Limiting / Distinct

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| ORDER BY (index-backed) | Y | Y | Y | Sort elimination via RemoveSortRule |
| ORDER BY (no index) | N | Ext | Y | Go extension: in-memory sort |
| ORDER BY aggregate | N | Ext | Y | Go extension |
| NULLS FIRST / LAST | Y | Y | Y | |
| LIMIT / OFFSET (clause) | N | N | Y | Both reject at parse time |
| SELECT DISTINCT | N | Ext | Y | Go extension -- Java rejects all DISTINCT |
| DISTINCT + ORDER BY | N | Ext | Y | Go extension: ImplementDistinctFinalRule |

## Schema / Types

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| BIGINT / INTEGER / DOUBLE / STRING / BOOLEAN | Y | Y | Y | |
| BYTES | Y | Y | -- | FDB-specific, not ANSI |
| DATE / TIMESTAMP / TIME | N | N | Y | Phase 5 TODO |
| CREATE TABLE / INDEX | Y | Y | Y | |
| INFORMATION_SCHEMA | N | Ext | Y | Go extension |

## Summary

| Category | Java 4.11.1.0 | Go | ANSI coverage |
|---|---|---|---|
| Core DML | Full | Full | Full |
| Expressions | Partial (no scalar fns) | Partial + datetime ext | ~60% |
| Predicates | Full | Full + byte literals | ~90% |
| Aggregation | **Partial** (core rules exist, SQL layer gaps) | **Full** (Go extension) | ~85% |
| Set operations | UNION ALL only | UNION ALL only | ~25% |
| Joins | Full except FULL OUTER | Missing OUTER JOINs | ~70% |
| Subqueries | Partial | **Missing** | ~30% |
| CTEs | Full | Basic (no recursive) | ~60% |
| Ordering | Index-only | Full (in-memory sort ext) | ~80% |
| Types | Core types | Core types + BYTES | ~60% |

Go is more capable than Java 4.11.1.0 in aggregation, ordering, and DISTINCT (Go extensions). Go's main gaps vs Java are subqueries and OUTER JOINs. Both engines lack string/math functions and DATE/TIMESTAMP types.

## Yamsql Conformance

98 scenario test suite. Current: **72/98 pass (73%)**, up from 63/98 (64%) at start of dayshift-79.

Remaining 26 failures dominated by: subqueries/EXISTS (~10), OUTER JOIN (~3), recursive CTE (~1), derived table scope (~5), and miscellaneous edge cases (~7).
