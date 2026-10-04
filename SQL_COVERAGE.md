# SQL Coverage (measured)

<!-- GENERATED FILE — DO NOT EDIT BY HAND.
     Regenerate with `just sql-coverage` (or `go run ./cmd/gen-sql-coverage`).
     Source: pkg/relational/conformance/yamsql/testdata/*.yaml. A drift guard
     (TestSQLCoverageUpToDate) fails CI if this file is stale. -->

Ledger B of RFC-165 — the **measured** corpus number. Every count is computed by
walking the yamsql conformance corpus and classifying each test case by its declared
outcome, so it cannot go stale. For the ANSI-standard scorecard see
`SQL_ANSI_CONFORMANCE.md`; for the scenario inventory see `FEATURE_MATRIX.md`.

**Buckets** (classified on typed outcome fields, never SQL text):
- **supported** — a positive assertion (rows verified, empty result, or a DML step that must succeed).
- **unsupported** — an explicitly-unsupported feature we cleanly reject (SQLSTATE `0A000`/`0AF00`/`0AF01`/`42883`).
- **error-path** — correct rejection/constraint semantics (unknown column, overflow, unique violation, type mismatch, …): supported behaviour, not a gap.

**383 scenarios · 3148 test cases** — 2756 supported (87.5%), 102 unsupported-feature pins, 290 error-path pins.

| Feature area | Cases | Supported | Unsupported | Error-path | Supported % |
|---|--:|--:|--:|--:|--:|
| Aggregates & GROUP BY | 351 | 316 | 19 | 16 | 90.0% |
| Joins | 313 | 296 | 2 | 15 | 94.6% |
| Subqueries (EXISTS / IN / scalar) | 323 | 267 | 33 | 23 | 82.7% |
| CTEs | 202 | 161 | 4 | 37 | 79.7% |
| Set operations (UNION / INTERSECT / EXCEPT) | 68 | 59 | 5 | 4 | 86.8% |
| DML (INSERT / UPDATE / DELETE) | 241 | 201 | 3 | 37 | 83.4% |
| Ordering & pagination | 138 | 133 | 0 | 5 | 96.4% |
| Scalar functions & expressions | 392 | 334 | 21 | 37 | 85.2% |
| Predicates & WHERE | 104 | 102 | 0 | 2 | 98.1% |
| Column resolution & aliasing | 59 | 30 | 0 | 29 | 50.8% |
| NULL handling | 27 | 24 | 3 | 0 | 88.9% |
| NULL handling & boolean logic | 48 | 48 | 0 | 0 | 100.0% |
| Index usage | 202 | 199 | 0 | 3 | 98.5% |
| Types | 148 | 127 | 4 | 17 | 85.8% |
| Keys & primary keys | 133 | 128 | 0 | 5 | 96.2% |
| Error codes & validation | 39 | 10 | 3 | 26 | 25.6% |
| End-to-end scenarios | 20 | 20 | 0 | 0 | 100.0% |
| Other | 340 | 301 | 5 | 34 | 88.5% |
| **Total** | **3148** | **2756** | **102** | **290** | **87.5%** |

