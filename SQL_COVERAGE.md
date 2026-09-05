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

**368 scenarios · 2986 test cases** — 2609 supported (87.4%), 114 unsupported-feature pins, 263 error-path pins.

| Feature area | Cases | Supported | Unsupported | Error-path | Supported % |
|---|--:|--:|--:|--:|--:|
| Aggregates & GROUP BY | 345 | 312 | 19 | 14 | 90.4% |
| Joins | 313 | 296 | 2 | 15 | 94.6% |
| Subqueries (EXISTS / IN / scalar) | 313 | 258 | 35 | 20 | 82.4% |
| CTEs | 134 | 93 | 7 | 34 | 69.4% |
| Set operations (UNION / INTERSECT / EXCEPT) | 68 | 59 | 5 | 4 | 86.8% |
| DML (INSERT / UPDATE / DELETE) | 238 | 202 | 4 | 32 | 84.9% |
| Ordering & pagination | 122 | 118 | 0 | 4 | 96.7% |
| Scalar functions & expressions | 381 | 328 | 21 | 32 | 86.1% |
| Predicates & WHERE | 104 | 102 | 0 | 2 | 98.1% |
| Column resolution & aliasing | 59 | 30 | 0 | 29 | 50.8% |
| NULL handling | 27 | 24 | 3 | 0 | 88.9% |
| NULL handling & boolean logic | 48 | 48 | 0 | 0 | 100.0% |
| Index usage | 182 | 179 | 0 | 3 | 98.4% |
| Types | 148 | 127 | 4 | 17 | 85.8% |
| Keys & primary keys | 133 | 128 | 0 | 5 | 96.2% |
| Error codes & validation | 39 | 10 | 3 | 26 | 25.6% |
| End-to-end scenarios | 20 | 20 | 0 | 0 | 100.0% |
| Other | 312 | 275 | 11 | 26 | 88.1% |
| **Total** | **2986** | **2609** | **114** | **263** | **87.4%** |

