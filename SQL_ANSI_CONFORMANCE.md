# SQL ANSI Conformance (SQL:2023 Core)

<!-- GENERATED FILE — DO NOT EDIT BY HAND.
     Regenerate with `just sql-coverage` (or `go run ./cmd/gen-sql-coverage`).
     The roster (Identifier/Core?/Name/Java?) is the hand-authored PINNED-FACT source
     in ansi_roster.go; the Go? column + completeness are DERIVED from `ansi:` tags
     on the yamsql corpus. Drift guards: TestAnsiLedgerUpToDate + TestAnsiLedgerEvidenceExists. -->

Ledger A of RFC-165 — the ANSI-standard scorecard, modeled on PostgreSQL Appendix D.
The `Java?`/`Core?`/`Identifier`/`Name` columns are pinned facts (SQL:2023 Core; the
frozen fdb-relational 4.12.11.0 reference). **`Go?` and completeness are derived** by
walking `ansi:`-tagged corpus scenarios — a claim of support traces to a named,
passing case, never a hand-typed status. For the measured corpus number see `SQL_COVERAGE.md`.

**Axes** (RFC-165 §4.2): `Java?` × `Go?`. The headline is keyed on **`Go?` only** —
a feature only Java has is never counted as Go-supported.

Counting follows PostgreSQL Appendix D: every Core row (parent **and** subfeature) counts. A
parent's status is *derived* from its subfeatures (partial = some supported), so a parent-level
"supported" is slightly more generous than PG's binary per-row assessment — but the number is
reproducible and drift-guarded, never hand-typed.

> **Denominator (pinned fact):** SQL:2023 Core as enumerated by PostgreSQL 18 = **176** mandatory
> feature/subfeature rows (it was 177 in PG13–15; `F812` "Basic flagging" lost Core status in PG16).

This ledger tracks **176** Core rows. **39** are N/A for an embedded record-layer SQL surface (cursors, table privileges, host-language binding, modules, SQL-invoked routines). Of the **137 applicable** Core rows: **Go supports 32** (27 shared-parity + 5 Go-only extension); **4 shared gaps** (roadmap); **0 port-fidelity divergences** (Java has it, Go rejects it → RFC-164); **101 not yet tagged** (Phase 1 — these are unknown, not gaps).

| Identifier | Core? | Feature | Java? | Go? | Routing | Evidence |
|---|:---:|---|:---:|:---:|---|---|
| E011 | ✓ | Numeric data types | partial | partial | shared parity | arithmetic |
| E011-01 | ✓ | INTEGER and SMALLINT data types | partial | untested | untested (Phase 1) | — |
| E011-02 | ✓ | REAL, DOUBLE PRECISION, and FLOAT data types | partial | untested | untested (Phase 1) | — |
| E011-03 | ✓ | DECIMAL and NUMERIC data types | no | untested | untested (Phase 1) | — |
| E011-04 | ✓ | Arithmetic operators | yes | yes | shared parity | arithmetic |
| E011-05 | ✓ | Numeric comparison | yes | untested | untested (Phase 1) | — |
| E011-06 | ✓ | Implicit casting among the numeric data types | yes | untested | untested (Phase 1) | — |
| E021 | ✓ | Character data types | partial | untested | untested (Phase 1) | — |
| E021-01 | ✓ | CHARACTER data type | no | untested | untested (Phase 1) | — |
| E021-02 | ✓ | CHARACTER VARYING data type | partial | untested | untested (Phase 1) | — |
| E021-03 | ✓ | Character literals | yes | untested | untested (Phase 1) | — |
| E021-04 | ✓ | CHARACTER_LENGTH function | no | untested | untested (Phase 1) | — |
| E021-05 | ✓ | OCTET_LENGTH function | no | untested | untested (Phase 1) | — |
| E021-06 | ✓ | SUBSTRING function | no | untested | untested (Phase 1) | — |
| E021-07 | ✓ | Character concatenation | no | untested | untested (Phase 1) | — |
| E021-08 | ✓ | UPPER and LOWER functions | no | untested | untested (Phase 1) | — |
| E021-09 | ✓ | TRIM function | no | untested | untested (Phase 1) | — |
| E021-10 | ✓ | Implicit casting among the character string types | partial | untested | untested (Phase 1) | — |
| E021-11 | ✓ | POSITION function | no | untested | untested (Phase 1) | — |
| E021-12 | ✓ | Character comparison | yes | untested | untested (Phase 1) | — |
| E031 | ✓ | Identifiers | partial | untested | untested (Phase 1) | — |
| E031-01 | ✓ | Delimited identifiers | partial | untested | untested (Phase 1) | — |
| E031-02 | ✓ | Lower case identifiers | partial | untested | untested (Phase 1) | — |
| E031-03 | ✓ | Trailing underscore | yes | untested | untested (Phase 1) | — |
| E051 | ✓ | Basic query specification | yes | partial | shared parity | distinct_order_by, having |
| E051-01 | ✓ | SELECT DISTINCT | partial | yes | shared parity | distinct_order_by |
| E051-02 | ✓ | GROUP BY clause | yes | untested | untested (Phase 1) | — |
| E051-04 | ✓ | GROUP BY can contain columns not in <select list> | yes | untested | untested (Phase 1) | — |
| E051-05 | ✓ | Select list items can be renamed | yes | untested | untested (Phase 1) | — |
| E051-06 | ✓ | HAVING clause | yes | yes | shared parity | having |
| E051-07 | ✓ | Qualified * in select list | yes | untested | untested (Phase 1) | — |
| E051-08 | ✓ | Correlation names in the FROM clause | yes | untested | untested (Phase 1) | — |
| E051-09 | ✓ | Rename columns in the FROM clause | yes | untested | untested (Phase 1) | — |
| E061 | ✓ | Basic predicates and search conditions | partial | partial | shared parity | between, exists, in_list_comprehensive, like, subquery_in (gap) |
| E061-01 | ✓ | Comparison predicate | yes | untested | untested (Phase 1) | — |
| E061-02 | ✓ | BETWEEN predicate | yes | yes | shared parity | between |
| E061-03 | ✓ | IN predicate with list of values | yes | yes | shared parity | in_list_comprehensive |
| E061-04 | ✓ | LIKE predicate | yes | yes | shared parity | like |
| E061-05 | ✓ | LIKE predicate ESCAPE clause | yes | untested | untested (Phase 1) | — |
| E061-06 | ✓ | NULL predicate | yes | untested | untested (Phase 1) | — |
| E061-07 | ✓ | Quantified comparison predicate | no | untested | untested (Phase 1) | — |
| E061-08 | ✓ | EXISTS predicate | yes | yes | shared parity | exists |
| E061-09 | ✓ | Subqueries in comparison predicate | no | untested | untested (Phase 1) | — |
| E061-11 | ✓ | Subqueries in IN predicate | no | no | shared gap → backlog | subquery_in (gap) |
| E061-12 | ✓ | Subqueries in quantified comparison predicate | no | untested | untested (Phase 1) | — |
| E061-13 | ✓ | Correlated subqueries | partial | untested | untested (Phase 1) | — |
| E061-14 | ✓ | Search condition | yes | untested | untested (Phase 1) | — |
| E071 | ✓ | Basic query expressions | partial | partial | shared parity | union, union (gap) |
| E071-01 | ✓ | UNION DISTINCT table operator | no | no | shared gap → backlog | union (gap) |
| E071-02 | ✓ | UNION ALL table operator | yes | yes | shared parity | union |
| E071-03 | ✓ | EXCEPT DISTINCT table operator | no | untested | untested (Phase 1) | — |
| E071-05 | ✓ | Columns combined via table operators need not have exactly the same data type | partial | untested | untested (Phase 1) | — |
| E071-06 | ✓ | Table operators in subqueries | partial | untested | untested (Phase 1) | — |
| E081 | ✓ | Basic Privileges | no | untested | N/A (out of engine scope) | — |
| E081-01 | ✓ | SELECT privilege | no | untested | N/A (out of engine scope) | — |
| E081-02 | ✓ | DELETE privilege | no | untested | N/A (out of engine scope) | — |
| E081-03 | ✓ | INSERT privilege at the table level | no | untested | N/A (out of engine scope) | — |
| E081-04 | ✓ | UPDATE privilege at the table level | no | untested | N/A (out of engine scope) | — |
| E081-05 | ✓ | UPDATE privilege at the column level | no | untested | N/A (out of engine scope) | — |
| E081-06 | ✓ | REFERENCES privilege at the table level | no | untested | N/A (out of engine scope) | — |
| E081-07 | ✓ | REFERENCES privilege at the column level | no | untested | N/A (out of engine scope) | — |
| E081-08 | ✓ | WITH GRANT OPTION | no | untested | N/A (out of engine scope) | — |
| E081-09 | ✓ | USAGE privilege | no | untested | N/A (out of engine scope) | — |
| E081-10 | ✓ | EXECUTE privilege | no | untested | N/A (out of engine scope) | — |
| E091 | ✓ | Set functions | partial | partial | shared parity | aggregate_sum_large, avg, count_distinct (gap), count_star_vs_col |
| E091-01 | ✓ | AVG | yes | yes | shared parity | avg |
| E091-02 | ✓ | COUNT | yes | yes | shared parity | count_star_vs_col |
| E091-03 | ✓ | MAX | yes | untested | untested (Phase 1) | — |
| E091-04 | ✓ | MIN | yes | untested | untested (Phase 1) | — |
| E091-05 | ✓ | SUM | yes | yes | shared parity | aggregate_sum_large |
| E091-06 | ✓ | ALL quantifier | yes | untested | untested (Phase 1) | — |
| E091-07 | ✓ | DISTINCT quantifier | no | no | shared gap → backlog | count_distinct (gap) |
| E101 | ✓ | Basic data manipulation | yes | yes | shared parity | delete_all_rows, insert_multi_row, update_comprehensive |
| E101-01 | ✓ | INSERT statement | yes | yes | shared parity | insert_multi_row |
| E101-03 | ✓ | Searched UPDATE statement | yes | yes | shared parity | update_comprehensive |
| E101-04 | ✓ | Searched DELETE statement | yes | yes | shared parity | delete_all_rows |
| E111 | ✓ | Single row SELECT statement | yes | untested | untested (Phase 1) | — |
| E121 | ✓ | Basic cursor support | no | untested | N/A (out of engine scope) | — |
| E121-01 | ✓ | DECLARE CURSOR | no | untested | N/A (out of engine scope) | — |
| E121-02 | ✓ | ORDER BY columns need not be in select list | no | untested | N/A (out of engine scope) | — |
| E121-03 | ✓ | Value expressions in ORDER BY clause | no | untested | N/A (out of engine scope) | — |
| E121-04 | ✓ | OPEN statement | no | untested | N/A (out of engine scope) | — |
| E121-06 | ✓ | Positioned UPDATE statement | no | untested | N/A (out of engine scope) | — |
| E121-07 | ✓ | Positioned DELETE statement | no | untested | N/A (out of engine scope) | — |
| E121-08 | ✓ | CLOSE statement | no | untested | N/A (out of engine scope) | — |
| E121-10 | ✓ | FETCH statement implicit NEXT | no | untested | N/A (out of engine scope) | — |
| E121-17 | ✓ | WITH HOLD cursors | no | untested | N/A (out of engine scope) | — |
| E131 | ✓ | Null value support (nulls in lieu of values) | yes | yes | shared parity | boolean |
| E141 | ✓ | Basic integrity constraints | partial | untested | untested (Phase 1) | — |
| E141-01 | ✓ | NOT NULL constraints | yes | untested | untested (Phase 1) | — |
| E141-02 | ✓ | UNIQUE constraints of NOT NULL columns | yes | untested | untested (Phase 1) | — |
| E141-03 | ✓ | PRIMARY KEY constraints | yes | untested | untested (Phase 1) | — |
| E141-04 | ✓ | Basic FOREIGN KEY constraint (NO ACTION default) | no | untested | untested (Phase 1) | — |
| E141-06 | ✓ | CHECK constraints | no | untested | untested (Phase 1) | — |
| E141-07 | ✓ | Column defaults | no | untested | untested (Phase 1) | — |
| E141-08 | ✓ | NOT NULL inferred on PRIMARY KEY | yes | untested | untested (Phase 1) | — |
| E141-10 | ✓ | Names in a foreign key can be specified in any order | no | untested | untested (Phase 1) | — |
| E151 | ✓ | Transaction support | no | untested | N/A (out of engine scope) | — |
| E151-01 | ✓ | COMMIT statement | no | untested | N/A (out of engine scope) | — |
| E151-02 | ✓ | ROLLBACK statement | no | untested | N/A (out of engine scope) | — |
| E152 | ✓ | Basic SET TRANSACTION statement | no | untested | N/A (out of engine scope) | — |
| E152-01 | ✓ | SET TRANSACTION: ISOLATION LEVEL SERIALIZABLE | no | untested | N/A (out of engine scope) | — |
| E152-02 | ✓ | SET TRANSACTION: READ ONLY and READ WRITE | no | untested | N/A (out of engine scope) | — |
| E153 | ✓ | Updatable queries with subqueries | partial | untested | untested (Phase 1) | — |
| E161 | ✓ | SQL comments using leading double minus | yes | untested | untested (Phase 1) | — |
| E171 | ✓ | SQLSTATE support | yes | untested | untested (Phase 1) | — |
| E182 | ✓ | Host language binding | no | untested | N/A (out of engine scope) | — |
| F021 | ✓ | Basic information schema | no | untested | untested (Phase 1) | — |
| F021-01 | ✓ | COLUMNS view | no | untested | untested (Phase 1) | — |
| F021-02 | ✓ | TABLES view | no | untested | untested (Phase 1) | — |
| F021-03 | ✓ | VIEWS view | no | untested | untested (Phase 1) | — |
| F021-04 | ✓ | TABLE_CONSTRAINTS view | no | untested | untested (Phase 1) | — |
| F021-05 | ✓ | REFERENTIAL_CONSTRAINTS view | no | untested | untested (Phase 1) | — |
| F021-06 | ✓ | CHECK_CONSTRAINTS view | no | untested | untested (Phase 1) | — |
| F031 | ✓ | Basic schema manipulation | partial | untested | untested (Phase 1) | — |
| F031-01 | ✓ | CREATE TABLE statement to create persistent base tables | yes | untested | untested (Phase 1) | — |
| F031-02 | ✓ | CREATE VIEW statement | partial | untested | untested (Phase 1) | — |
| F031-03 | ✓ | GRANT statement | no | untested | untested (Phase 1) | — |
| F031-04 | ✓ | ALTER TABLE statement: ADD COLUMN clause | partial | untested | untested (Phase 1) | — |
| F031-13 | ✓ | DROP TABLE statement: RESTRICT clause | yes | untested | untested (Phase 1) | — |
| F031-16 | ✓ | DROP VIEW statement: RESTRICT clause | partial | untested | untested (Phase 1) | — |
| F031-19 | ✓ | REVOKE statement: RESTRICT clause | no | untested | untested (Phase 1) | — |
| F041 | ✓ | Basic joined table | yes | partial | shared parity | join_chained |
| F041-01 | ✓ | Inner join (but not necessarily the INNER keyword) | yes | yes | shared parity | join_chained |
| F041-02 | ✓ | INNER keyword | yes | untested | untested (Phase 1) | — |
| F041-03 | ✓ | LEFT OUTER JOIN | yes | untested | untested (Phase 1) | — |
| F041-04 | ✓ | RIGHT OUTER JOIN | yes | untested | untested (Phase 1) | — |
| F041-05 | ✓ | Outer joins can be nested | yes | untested | untested (Phase 1) | — |
| F041-07 | ✓ | The inner table in a left or right outer join can also be used in an inner join | yes | untested | untested (Phase 1) | — |
| F041-08 | ✓ | All comparison operators are supported (rather than just =) | yes | untested | untested (Phase 1) | — |
| F051 | ✓ | Basic date and time | no | partial | Go-only ext | datetime_column_types |
| F051-01 | ✓ | DATE data type (including DATE literal) | no | yes | Go-only ext | datetime_column_types |
| F051-02 | ✓ | TIME data type | no | untested | untested (Phase 1) | — |
| F051-03 | ✓ | TIMESTAMP data type | no | yes | Go-only ext | datetime_column_types |
| F051-04 | ✓ | Comparison predicate on DATE, TIME, and TIMESTAMP | no | untested | untested (Phase 1) | — |
| F051-05 | ✓ | Explicit CAST between datetime and character string types | no | untested | untested (Phase 1) | — |
| F051-06 | ✓ | CURRENT_DATE | no | untested | untested (Phase 1) | — |
| F051-07 | ✓ | LOCALTIME | no | untested | untested (Phase 1) | — |
| F051-08 | ✓ | LOCALTIMESTAMP | no | untested | untested (Phase 1) | — |
| F081 | ✓ | UNION and EXCEPT in views | no | untested | untested (Phase 1) | — |
| F131 | ✓ | Grouped operations | yes | untested | untested (Phase 1) | — |
| F131-01 | ✓ | WHERE, GROUP BY, and HAVING supported with grouped views | yes | untested | untested (Phase 1) | — |
| F131-02 | ✓ | Multiple tables supported in queries with grouped views | yes | untested | untested (Phase 1) | — |
| F131-03 | ✓ | Set functions supported in queries with grouped views | yes | untested | untested (Phase 1) | — |
| F131-04 | ✓ | Subqueries with GROUP BY and HAVING and grouped views | yes | untested | untested (Phase 1) | — |
| F131-05 | ✓ | Single row SELECT with GROUP BY and HAVING and grouped views | yes | untested | untested (Phase 1) | — |
| F181 | ✓ | Multiple module support | no | untested | N/A (out of engine scope) | — |
| F201 | ✓ | CAST function | yes | yes | shared parity | cast |
| F221 | ✓ | Explicit defaults | no | untested | untested (Phase 1) | — |
| F261 | ✓ | CASE expression | partial | partial | shared parity | case_when, coalesce_nullif, coalesce_nullif (gap) |
| F261-01 | ✓ | Simple CASE | no | yes | Go-only ext | case_when |
| F261-02 | ✓ | Searched CASE | yes | yes | shared parity | case_when |
| F261-03 | ✓ | NULLIF | no | no | shared gap → backlog | coalesce_nullif (gap) |
| F261-04 | ✓ | COALESCE | yes | yes | shared parity | coalesce_nullif |
| F311 | ✓ | Schema definition statement | partial | untested | untested (Phase 1) | — |
| F311-01 | ✓ | CREATE SCHEMA | yes | untested | untested (Phase 1) | — |
| F311-02 | ✓ | CREATE TABLE for persistent base tables | yes | untested | untested (Phase 1) | — |
| F311-03 | ✓ | CREATE VIEW | partial | untested | untested (Phase 1) | — |
| F311-04 | ✓ | CREATE VIEW: WITH CHECK OPTION | no | untested | untested (Phase 1) | — |
| F311-05 | ✓ | GRANT statement | no | untested | untested (Phase 1) | — |
| F471 | ✓ | Scalar subquery values | no | yes | Go-only ext | scalar_subquery |
| F481 | ✓ | Expanded NULL predicate | partial | untested | untested (Phase 1) | — |
| F501 | ✓ | Features and conformance views | no | untested | untested (Phase 1) | — |
| F501-01 | ✓ | SQL_FEATURES view | no | untested | untested (Phase 1) | — |
| F501-02 | ✓ | SQL_SIZING view | no | untested | untested (Phase 1) | — |
| S011 | ✓ | Distinct data types | no | untested | N/A (out of engine scope) | — |
| S011-01 | ✓ | USER_DEFINED_TYPES view | no | untested | N/A (out of engine scope) | — |
| T321 | ✓ | Basic SQL-invoked routines | no | untested | N/A (out of engine scope) | — |
| T321-01 | ✓ | User-defined functions with no overloading | no | untested | N/A (out of engine scope) | — |
| T321-02 | ✓ | User-defined stored procedures with no overloading | no | untested | N/A (out of engine scope) | — |
| T321-03 | ✓ | Function invocation | no | untested | N/A (out of engine scope) | — |
| T321-04 | ✓ | CALL statement | no | untested | N/A (out of engine scope) | — |
| T321-05 | ✓ | RETURN statement | no | untested | N/A (out of engine scope) | — |
| T321-06 | ✓ | ROUTINES view | no | untested | N/A (out of engine scope) | — |
| T321-07 | ✓ | PARAMETERS view | no | untested | N/A (out of engine scope) | — |
| T631 | ✓ | IN predicate with one list element | yes | untested | untested (Phase 1) | — |

