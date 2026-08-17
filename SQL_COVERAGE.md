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

**353 scenarios · 2801 test cases** — 2464 supported (88.0%), 111 unsupported-feature pins, 226 error-path pins.

| Feature area | Cases | Supported | Unsupported | Error-path | Supported % |
|---|--:|--:|--:|--:|--:|
| Aggregates & GROUP BY | 326 | 293 | 19 | 14 | 89.9% |
| Joins | 277 | 268 | 3 | 6 | 96.8% |
| Subqueries (EXISTS / IN / scalar) | 313 | 258 | 35 | 20 | 82.4% |
| CTEs | 108 | 77 | 7 | 24 | 71.3% |
| Set operations (UNION / INTERSECT / EXCEPT) | 61 | 52 | 5 | 4 | 85.2% |
| DML (INSERT / UPDATE / DELETE) | 210 | 192 | 1 | 17 | 91.4% |
| Ordering & pagination | 122 | 118 | 0 | 4 | 96.7% |
| Scalar functions & expressions | 381 | 328 | 21 | 32 | 86.1% |
| Predicates & WHERE | 104 | 102 | 0 | 2 | 98.1% |
| Column resolution & aliasing | 59 | 30 | 0 | 29 | 50.8% |
| NULL handling | 27 | 24 | 3 | 0 | 88.9% |
| NULL handling & boolean logic | 48 | 48 | 0 | 0 | 100.0% |
| Index usage | 176 | 173 | 0 | 3 | 98.3% |
| Types | 148 | 127 | 4 | 17 | 85.8% |
| Keys & primary keys | 133 | 128 | 0 | 5 | 96.2% |
| Error codes & validation | 39 | 10 | 3 | 26 | 25.6% |
| End-to-end scenarios | 20 | 20 | 0 | 0 | 100.0% |
| Other | 249 | 216 | 10 | 23 | 86.7% |
| **Total** | **2801** | **2464** | **111** | **226** | **88.0%** |

