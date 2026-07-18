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

**327 scenarios · 2584 test cases** — 2269 supported (87.8%), 110 unsupported-feature pins, 205 error-path pins.

| Feature area | Cases | Supported | Unsupported | Error-path | Supported % |
|---|--:|--:|--:|--:|--:|
| Aggregates & GROUP BY | 315 | 285 | 18 | 12 | 90.5% |
| Joins | 267 | 258 | 3 | 6 | 96.6% |
| Subqueries (EXISTS / IN / scalar) | 281 | 230 | 36 | 15 | 81.9% |
| CTEs | 105 | 73 | 9 | 23 | 69.5% |
| Set operations (UNION / INTERSECT / EXCEPT) | 53 | 44 | 5 | 4 | 83.0% |
| DML (INSERT / UPDATE / DELETE) | 194 | 179 | 1 | 14 | 92.3% |
| Ordering & pagination | 114 | 110 | 0 | 4 | 96.5% |
| Scalar functions & expressions | 354 | 304 | 22 | 28 | 85.9% |
| Predicates & WHERE | 104 | 102 | 0 | 2 | 98.1% |
| Column resolution & aliasing | 55 | 29 | 0 | 26 | 52.7% |
| NULL handling | 26 | 22 | 0 | 4 | 84.6% |
| NULL handling & boolean logic | 48 | 48 | 0 | 0 | 100.0% |
| Index usage | 162 | 160 | 0 | 2 | 98.8% |
| Types | 145 | 124 | 4 | 17 | 85.5% |
| Keys & primary keys | 132 | 127 | 0 | 5 | 96.2% |
| Error codes & validation | 37 | 8 | 2 | 27 | 21.6% |
| End-to-end scenarios | 20 | 20 | 0 | 0 | 100.0% |
| Other | 172 | 146 | 10 | 16 | 84.9% |
| **Total** | **2584** | **2269** | **110** | **205** | **87.8%** |

