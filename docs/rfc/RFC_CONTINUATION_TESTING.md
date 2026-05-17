# RFC: Continuation Correctness Testing

## Problem

Java's test suite exercises cursor continuation/resume across transaction boundaries via `testFlatMapReasons` (6 resumption cycles), `pipelineWithInnerLimits`, `OrElse` under TIME_LIMIT, and `JoinWithLimitTest.joinWithContinuationAndLimit`. Go has zero equivalent tests. The NOT EXISTS emission bug (review round 5) slipped through because no test forced the FlatMap code path under conditions that exercise mid-stream stop/resume.

Current Go tests verify final query results but never verify that stopping and resuming mid-query produces the same results as running to completion.

## Root Cause

Go's `paginatingRows` uses a 4-second per-transaction time limit. At small test sizes (10-100 rows), queries complete within a single transaction — the continuation path is never exercised. The FlatMapContinuation proto was wired but never tested under conditions where it actually fires.

## Proposal

### 1. Configurable Time Limit per Test

Add a `WithTimeLimit(d time.Duration)` option to the Cascades generator (or expose via DSN parameter):

```go
// In cascades_generator.go
const txPageTimeLimit = 4 * time.Second // production default

// Test override:
// fdbsql:///db?cluster_file=...&schema=main&tx_time_limit=100ms
```

With `tx_time_limit=100ms`, even a 50-row JOIN will span multiple transactions, exercising every continuation code path.

### 2. Continuation Correctness Test Suite

New test file: `pkg/relational/sqldriver/continuation_correctness_test.go`

Pattern:
```go
func TestFDB_ContinuationCorrectnessJoin(t *testing.T) {
    // 1. Insert 200 rows into orders + 20 customers
    // 2. Run JOIN with tx_time_limit=100ms (forces ~10 page breaks)
    // 3. Collect all results
    // 4. Run same JOIN with tx_time_limit=30s (single page, no continuation)
    // 5. Compare: results must be identical (same rows, same order)
}
```

Test matrix:
- INNER JOIN via FlatMap (PK correlated scan)
- LEFT OUTER JOIN via FlatMap (NULL row at page boundary)
- EXISTS via FlatMap (inner match at page boundary)
- NOT EXISTS via FlatMap (inner exhaustion at page boundary)
- Three-way join (nested FlatMap continuations)
- JOIN + ORDER BY (InMemorySort continuation interaction)
- JOIN + GROUP BY (streaming aggregation continuation interaction)

Each test runs the query TWICE: once with aggressive time limit (forces continuations), once without (baseline). Results must match exactly.

### 3. Pre-built Dataset for Scale Tests

For 100K+ validation without 3-minute insert penalty:

```
testdata/
  stress_100k_orders.sql.zst    # zstd-compressed INSERT statements
  stress_100k_customers.sql.zst
```

Generation:
```bash
just generate-stress-data 100000  # produces .sql.zst files
```

Import helper:
```go
func importCompressedSQL(t *testing.T, db *sql.DB, path string) {
    r, _ := os.Open(path)
    zr := zstd.NewReader(r)
    scanner := bufio.NewScanner(zr)
    for scanner.Scan() {
        db.Exec(scanner.Text())
    }
}
```

Trade-offs:
- Adds ~2MB to repo (100K rows compressed)
- Import takes ~20s vs 3min for generation
- Decouples data generation from test execution

### 4. Alternative: Raw KV Bulk Load

For maximum speed, bypass SQL and write FDB key-value pairs directly:

```go
func bulkLoadRecords(t *testing.T, store *FDBRecordStore, records []proto.Message) {
    // Write serialized records directly to FDB subspace
    // ~50K rows/s vs 5K rows/s via SQL INSERT
}
```

Trade-off: couples to wire format (record store header, tuple encoding, split records). Breaks if format changes. Recommend SQL approach for portability.

## Recommendation

Phase 1 (immediate): Configurable time limit + continuation correctness test suite. Small dataset (200 rows), aggressive time limit (100ms). Catches continuation bugs without scale penalty.

Phase 2 (next shift): Pre-built compressed dataset for 100K scale tests. Used for performance regression detection, not correctness.

## Implementation Plan

1. Add `tx_time_limit` DSN parameter to `parseDSN` in `pkg/relational/sqldriver/driver.go`
2. Thread through to `cascadesPlan.Execute()` → `paginatingRows.txPageTimeLimit`
3. Write `continuation_correctness_test.go` with the test matrix above
4. Verify: run each query with 100ms limit, compare against 30s baseline
5. If any mismatch: continuation bug found — fix before merge

## Open Questions

1. Should the aggressive-time-limit tests run in CI? (They're fast — 200 rows + FDB container = ~5s per test)
2. Should we also test continuation across schema changes (Java's check_value mechanism)?
3. For the pre-built dataset: commit as testdata/ or generate in CI as a build step?
