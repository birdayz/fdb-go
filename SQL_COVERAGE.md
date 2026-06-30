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

**319 scenarios · 2512 test cases** — 2244 supported (89.3%), 92 unsupported-feature pins, 176 error-path pins.

| Feature area | Cases | Supported | Unsupported | Error-path | Supported % |
|---|--:|--:|--:|--:|--:|
| Aggregates & GROUP BY | 298 | 274 | 14 | 10 | 91.9% |
| Joins | 264 | 255 | 3 | 6 | 96.6% |
| Subqueries (EXISTS / IN / scalar) | 281 | 238 | 27 | 16 | 84.7% |
| CTEs | 85 | 64 | 6 | 15 | 75.3% |
| Set operations (UNION / INTERSECT / EXCEPT) | 47 | 40 | 4 | 3 | 85.1% |
| DML (INSERT / UPDATE / DELETE) | 194 | 180 | 1 | 13 | 92.8% |
| Ordering & pagination | 114 | 95 | 15 | 4 | 83.3% |
| Scalar functions & expressions | 346 | 308 | 14 | 24 | 89.0% |
| Predicates & WHERE | 104 | 102 | 0 | 2 | 98.1% |
| Column resolution & aliasing | 55 | 29 | 0 | 26 | 52.7% |
| NULL handling | 26 | 24 | 0 | 2 | 92.3% |
| NULL handling & boolean logic | 48 | 48 | 0 | 0 | 100.0% |
| Index usage | 162 | 159 | 1 | 2 | 98.1% |
| Types | 144 | 128 | 0 | 16 | 88.9% |
| Keys & primary keys | 132 | 128 | 0 | 4 | 97.0% |
| Error codes & validation | 37 | 17 | 1 | 19 | 45.9% |
| End-to-end scenarios | 20 | 20 | 0 | 0 | 100.0% |
| Other | 155 | 135 | 6 | 14 | 87.1% |
| **Total** | **2512** | **2244** | **92** | **176** | **89.3%** |

