# SQL Conformance Matrix

Java fdb-relational **4.12.11.0** vs Go implementation vs ANSI SQL standard.

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
| Searched CASE (`CASE WHEN cond`) | Y | Y | Y | |
| Simple CASE (`CASE expr WHEN val`) | N | Y | Y | Java accepts the syntax but mis-evaluates: `visitCaseExpressionFunctionCall` is a no-op that always falls through to ELSE; Go evaluates correctly (`case_simple_int_match` divergence) |
| CAST | Y | Y | Y | Overflow detection aligned |
| COALESCE | Y | Y | Y | |
| NULLIF | N | N | Y | Both reject (42883 — no function-registry entry); shared ANSI gap F261-03 |
| GREATEST / LEAST | Y | Y | Y | |
| CARDINALITY (array length, `ln`) | Y | Y | Y | Added in Java 4.12 (scalar fn + index support); Go ports both (RFC-143) |
| String functions (UPPER etc.) | N | N | Y | Both reject -- Java has no function catalog entry |
| Math functions (ABS etc.) | N | N | Y | Both reject |
| CURRENT_TIMESTAMP / CURRENT_DATE | N | Ext | Y | Go extension: proper TIMESTAMP/DATE types, comparisons, CAST |
| Date-part functions (YEAR etc.) | N | Ext | Y | Go extension: YEAR/MONTH/DAY/HOUR/MINUTE/SECOND/DAYOFWEEK/DAYOFYEAR |

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
| GROUP BY column | Y | Y | Y | 4.12 wires GroupByExpression end-to-end (groupby-tests.yamsql); 4.11 SQL layer didn't |
| GROUP BY expression | Y | Y | Y | Wired in 4.12 (was a Go-only extension under 4.11) |
| COUNT / SUM / AVG / MIN / MAX | Y | Y | Y | 4.12 plans aggregates through the SQL layer via GroupByExpression + StreamingAggregationRule |
| HAVING | Y | Y | Y | 4.12 wires visitHavingClause (groupby-tests.yamsql HAVING cases) |
| COUNT(DISTINCT col) | N | N | Y | Both reject (0A000) |
| Empty-table aggregates | Y | Y | Y | NULL for SUM/AVG, 0 for COUNT; 4.12 plans the empty-table implicit group under HAVING |

## Set Operations

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| UNION ALL | Y | Y | Y | |
| UNION (dedup) | N | N | Y | No Cascades rule in either engine |
| INTERSECT / EXCEPT | N | N | Y | |
| ORDER BY on UNION result | P | Y | Y | Named columns work; positional refs limited |

## Joins

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| INNER JOIN (explicit + comma) | Y | Y | Y | |
| CROSS JOIN | Y | Y | Y | |
| LEFT OUTER JOIN | Y | Y | Y | Added in Java 4.12; Go plans via NLJ with JoinLeftOuter |
| RIGHT OUTER JOIN | Y | Y | Y | Added in Java 4.12. Go has no RIGHT join type: the translator rewrites `a RIGHT JOIN b` to `b LEFT JOIN a` by swapping the operands (`cascades_translator.go`, `kind = logical.JoinLeft`; `SELECT *` keeps declaration order via the pre-swap legs), planned as a materialized NLJ with `JoinLeftOuter` |
| FULL OUTER JOIN | N | Ext | Y | Java rejects (SYNTAX_ERROR — grammar accepts only LEFT/RIGHT); Go-only extension via NLJ with JoinFullOuter |
| Self-join | Y | Y | Y | |
| 3+ way join | Y | Y | Y | |

## Subqueries

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| Scalar subquery | N | Ext | Y | Go extension — Java grammar has no `subqueryExpressionAtom` |
| EXISTS / NOT EXISTS | Y | Y | Y | Correlated + nested EXISTS in WHERE; 4.12 also added EXISTS in the projection list (`SELECT EXISTS(...)`), which Go matches |
| Correlated subquery | P | P | Y | Correlated EXISTS works; correlated scalar rejected by both |
| Derived table (FROM subquery) | Y | Y | Y | Column alias propagation working |
| Correlated array UNNEST in FROM (+ `WITH ORDINALITY`) | Y | Y | Y | Added in Java 4.12; Go ports the lateral array unnest + ordinality index (RFC-142) |

## CTEs

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| WITH (basic) | Y | Y | Y | |
| WITH column rename | Y | Y | Y | |
| Chained CTEs | Y | Y | Y | |
| WITH RECURSIVE (level-order) | Y | Y | Y | RecursiveLevelUnionPlan |
| Recursive DFS order | N | Y | P | Go extension |
| UNION DISTINCT in recursive CTE | Y | Y | Y | Cycle detection for recursive CTEs |

## Ordering / Limiting / Distinct

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| ORDER BY (index-backed) | Y | Y | Y | Sort elimination via ImplementSortRule (PLANNING phase, D-1 aligned) |
| ORDER BY (no index) | N | Ext | Y | Go extension: in-memory sort |
| ORDER BY aggregate | N | Ext | Y | Go extension |
| NULLS FIRST / LAST | Y | Y | Y | |
| LIMIT / OFFSET (outer SELECT) | Y | Y | Y | Wired via `limitClause`→`LogicalLimit`; rejected only inside subqueries (0AF00) and DELETE |
| SELECT DISTINCT | P | Ext | Y | Java rejects most DISTINCT; Go has ImplementDistinctFinalRule (D-3 aligned) |
| DISTINCT + ORDER BY | N | Ext | Y | Go extension |

## Schema / Types

| Feature | Java | Go | ANSI | Notes |
|---|:---:|:---:|:---:|---|
| BIGINT / INTEGER / DOUBLE / STRING / BOOLEAN | Y | Y | Y | |
| BYTES | Y | Y | -- | FDB-specific, not ANSI |
| DATE / TIMESTAMP | N | Ext | Y | Go extension: column types, CAST, comparisons, YEAR/MONTH/DAY |
| ARRAY column type | Y | Y | Y | All primitive element types: STRING/BIGINT/INTEGER/DOUBLE/FLOAT/BOOLEAN/BYTES/UUID ARRAY |
| CREATE TABLE / INDEX | Y | Y | Y | |
| Schema-qualified names (schema.table) | Y | Y | Y | Qualifier validated against current schema |
| INFORMATION_SCHEMA | N | Ext | Y | Go extension |

## Error Code Conformance

| Error | SQLSTATE | Java | Go | Notes |
|---|---|:---:|:---:|---|
| Unknown table | 42F01 | Y | Y | validateTablesAndColumns + SourceNotFoundError |
| Unknown column | 42703 | Y | Y | ColumnNotFoundError in WHERE/SELECT/ORDER BY |
| Ambiguous column | 42702 | Y | Y | JOIN ambiguity detection |
| Unknown qualifier | 42703 | Y | Y | SourceNotFoundError → 42703 |
| Type mismatch in comparison | 42804 | Y | Y | COMPARISON_OF_INCOMPATIBLE_TYPES → DATATYPE_MISMATCH |
| Type mismatch in BETWEEN | 42804 | Y | Y | Same as comparison |
| IN-list type mismatch | 42804 | Y | Y | Same as comparison |
| GROUP BY violation | 42803 | Y | Y | Non-grouped column in SELECT |
| Duplicate ORDER BY | 42701 | Y | Y | |
| UNION column mismatch | 42F64 | Y | Y | |
| UNION type mismatch | 42F65 | Y | Y | |
| NOT NULL violation | 23502 | Y | Y | |
| PK constraint | 23505 | Y | Y | |
| Unique index violation | 23505 | Y | Y | RecordAlreadyExistsError → translateFDBError |
| Integer overflow | 22003 | Y | Y | |
| INT32 range overflow | 22003 | Y | Y | INSERT value > MAX_INT32 into INTEGER column |
| Unsupported function | 42883 | Y | Y | |
| FDB transaction timeout | 53F00 | Y | Y | translateFDBError |
| FDB transaction conflict | 40001 | Y | Y | translateFDBError |
| Deserialization failure | XXF01 | Y | Y | RecordDeserializationError → translateFDBError |

## Summary

> **Measured numbers live in the generated, drift-guarded ledgers (RFC-165), not here:**
> - `SQL_COVERAGE.md` (Ledger B) — measured corpus coverage (% of test cases that pass).
> - `SQL_ANSI_CONFORMANCE.md` (Ledger A) — ANSI SQL:2023 Core scorecard, Go support *derived* from `# ansi:` corpus tags.
>
> The table below is a **qualitative** Java-vs-Go narrative only (the old hand-typed `~%` ANSI-coverage
> column has been deleted — it was fabricated). For any percentage, defer to the ledgers (`just sql-coverage`).

| Category | Java 4.12.11.0 | Go |
|---|---|---|
| Core DML | Full | Full |
| Expressions | Partial (no scalar fns) | Partial + datetime ext |
| Predicates | Full | Full + byte literals |
| Aggregation | **Full** (4.12 wires GROUP BY/HAVING through the SQL layer) | **Full** |
| Set operations | UNION ALL only | UNION ALL only |
| Joins | INNER + LEFT/RIGHT OUTER (4.12); FULL OUTER rejected | + FULL OUTER (Go-only ext) |
| Subqueries | Partial | **Matches Java** (EXISTS + scalar work, correlated scalar rejected by both) |
| CTEs | Full + recursive | Full + recursive + DFS ext |
| Ordering | Index-only | Full (in-memory sort ext) |
| Types | Core types | All Java types + DATE/TIMESTAMP ext |
| Error codes | Full | Full (ExceptionUtil 1:1 port) |

4.12 closed several former gaps that Go had already implemented as extensions: GROUP BY/HAVING aggregation, LEFT/RIGHT OUTER JOIN, EXISTS in the projection list, and boolean literals in WHERE are now wired in Java's SQL layer too. Go remains more capable than Java 4.12.11.0 in ordering, DISTINCT, recursive CTEs, FULL OUTER JOIN, and temporal types. **Go matches Java for subquery support** — uncorrelated scalar subqueries and correlated EXISTS both work; correlated scalar subqueries are rejected by both engines. Both engines lack string/math functions. Go extends beyond Java with FULL OUTER JOIN, DATE/TIMESTAMP column types, CAST, CURRENT_TIMESTAMP/CURRENT_DATE, and date-part extraction functions (YEAR/MONTH/DAY/HOUR/MINUTE/SECOND).

## Yamsql Conformance

The yamsql corpus is the conformance harness. **Do not hand-type the counts here** — they rot
(this line previously read "115/115" long after the corpus passed 300 files). The current,
measured numbers are generated into `SQL_COVERAGE.md` (corpus pass rate) and
`SQL_ANSI_CONFORMANCE.md` (ANSI Core scorecard), each guarded by a drift test that fails CI if
stale. Run `just sql-coverage` to regenerate; see `FEATURE_MATRIX.md` for the scenario inventory.

## Cascades Divergence Status

See `CASCADES_DIVERGENCE.md` for the full audit. **ALL divergences resolved** (D-1 through D-12). D-4 (cost model) is intentional by design. Zero open architectural divergences from Java.
