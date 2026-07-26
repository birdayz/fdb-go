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

**341 scenarios · 2734 test cases** — 2393 supported (87.5%), 111 unsupported-feature pins, 230 error-path pins.

| Feature area | Cases | Supported | Unsupported | Error-path | Supported % |
|---|--:|--:|--:|--:|--:|
| Aggregates & GROUP BY | 322 | 289 | 19 | 14 | 89.8% |
| Joins | 273 | 264 | 3 | 6 | 96.7% |
| Subqueries (EXISTS / IN / scalar) | 301 | 245 | 36 | 20 | 81.4% |
| CTEs | 105 | 73 | 9 | 23 | 69.5% |
| Set operations (UNION / INTERSECT / EXCEPT) | 61 | 52 | 5 | 4 | 85.2% |
| DML (INSERT / UPDATE / DELETE) | 210 | 191 | 1 | 18 | 91.0% |
| Ordering & pagination | 115 | 111 | 0 | 4 | 96.5% |
| Scalar functions & expressions | 377 | 323 | 22 | 32 | 85.7% |
| Predicates & WHERE | 104 | 102 | 0 | 2 | 98.1% |
| Column resolution & aliasing | 59 | 30 | 0 | 29 | 50.8% |
| NULL handling | 26 | 22 | 0 | 4 | 84.6% |
| NULL handling & boolean logic | 48 | 48 | 0 | 0 | 100.0% |
| Index usage | 162 | 160 | 0 | 2 | 98.8% |
| Types | 148 | 127 | 4 | 17 | 85.8% |
| Keys & primary keys | 133 | 128 | 0 | 5 | 96.2% |
| Error codes & validation | 37 | 8 | 2 | 27 | 21.6% |
| End-to-end scenarios | 20 | 20 | 0 | 0 | 100.0% |
| Other | 233 | 200 | 10 | 23 | 85.8% |
| **Total** | **2734** | **2393** | **111** | **230** | **87.5%** |

