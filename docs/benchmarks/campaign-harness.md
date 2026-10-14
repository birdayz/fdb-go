# Benchmark campaign harness

Benchmark-only sources for comparing the legacy baseline `9a9a44867`, the
post-parity baseline `3824f1465`, RFC-258 PR2 `5052212f5` and a final tree.
Nothing here changes production code. Every benchmark reads
`FDB_BENCH_CLUSTER_FILE` to use an existing cluster; unset, it starts its own
FoundationDB container as before.

## Overlay sets

Copy each set byte-for-byte into every tree it lists, then build there.

| Set | Paths | Trees |
|---|---|---|
| common | `pkg/testutil/benchcluster/` (whole directory), `pkg/recordlayer/benchmark_test.go`, `pkg/relational/sqlbench/BUILD.bazel`, `pkg/relational/sqlbench/common_bench_test.go` | 9a, 3824, PR2, final |
| remote | `pkg/recordlayer/benchfetch/BUILD.bazel`, `fixture_test.go`, `matrix_bench_test.go` | PR2, final |
| final-only | `pkg/recordlayer/benchfetch/planskip/`, `pkg/relational/sqlbench/fetchmethod/` | final |
| client scripts | `tools/clientbench/scripts/` (run on the host from the final tree) | host |

The clientbench executables themselves need no overlay: `tools/clientbench`,
`pkg/fdbgo`, `go.mod`, `MODULE.bazel` and `patches/` are identical between
`9a9a44867` and `3824f1465`, so one baseline client binary represents both.

`pkg/recordlayer/benchmark_test.go` is the final tree's file: identical to
9a/3824's except `BenchmarkScanRecordsSplitVersioned` (which uses
`SetStoreRecordVersions`, present in every listed tree), the
`FDB_BENCH_CLUSTER_FILE` branch of `ensureBenchDB`, `benchSubspace` clearing
the benchmark's subspace on every body invocation (Go reruns a body to size
`b.N`; each run now starts empty, so save benchmarks always insert fresh
records), and `verifyOrders` after each write benchmark. The remote set uses only
APIs present at PR2 (`ScanIndexRecordsWithFetchMethod`,
`GetCommonPrimaryKeyLength`, the StoreTimer events). Compile each overlaid tree
before any timing; a compile failure is a harness defect, not a skipped cell.

## Targets and benchmarks

| Matrix | Target | Benchmarks |
|---|---|---|
| B (Record Layer) | `//pkg/recordlayer:recordlayer_test` (add `--ginkgo.skip='.*'`) | existing `benchmark_test.go` benchmarks |
| C1 | `//pkg/recordlayer/benchfetch:benchfetch_test` | `BenchmarkIndexFetch/payload={small,split}/cell={limit1,rows10,rows1000}/dir={forward,reverse}/method={SCAN_AND_FETCH,USE_REMOTE_FETCH}` |
| C2 | `//pkg/recordlayer/benchfetch/planskip:planskip_test` | `BenchmarkPlanSkip/cell={rows10,rows1000}/dir=…/method=…` (Skip 3) |
| D common | `//pkg/relational/sqlbench:sqlbench_test` | `BenchmarkFDB_{PlanCacheHit,IndexScanRange,TimestampInsert}` |
| D method | `//pkg/relational/sqlbench/fetchmethod:fetchmethod_test` | `BenchmarkSQLIndexFetch/query={rows10,rows1000}/method=…` |
| A (client) | `//tools/clientbench/cmd/clientbench:clientbench_{go,cgo}` | `scripts/matrix_plan.py`, then `scripts/run.py` and `scripts/analyze.py` |

None of the new targets has tests; `bazelisk test` builds them and runs them
empty. Run benchmarks from the built binary with `-test.run='^$'`.

## What the harness checks

### Classic Record Layer benchmarks (`benchmark_test.go`, matrix B)

Their timed loops are unchanged, including the checks some already make while
timed (for example `BenchmarkScanIndexRecords` checks every row's primary key
inside the loop). Those checks are byte-identical in every compared tree, so
they are part of the measured operation, equally in each arm. The overlay adds
only untimed work:

- `benchSubspace` clears the benchmark's subspace before the body's timer
  starts, on every body run.
- After the loop, with the timer stopped, each write benchmark checks that
  exactly the IDs it wrote exist: for `b.N` saves, Orders `0..b.N-1` and not
  `b.N` (`SaveRecordBatch`: `0..10*b.N-1`); for `DeleteRecord`, the deleted IDs
  are gone and the other 100 remain. Each such check logs one receipt (two for
  `DeleteRecord`). Read benchmarks log none.

### New benchmarks (C1, C2, SQL common and method)

Nothing but the operation itself is timed:

- Fixtures are seeded and read back before any timer: C1 checks every
  record's fields, a CRC-32 of its payload and its key-value count (1 small,
  2 split: just over the 100,000-byte split threshold); C2 checks every
  record; SQL seeds a database it created (see below).
- Every measured operation's result is verified under `b.StopTimer`, without
  a receipt per operation: C1 and C2 check exact IDs and order, fields,
  payload checksum and terminal reason (`ReturnLimitReached` for `limit1`,
  otherwise `SourceExhausted`); SQL reads check exact rows; `TimestampInsert`
  checks each insert affected one row.
- C1, C2 and the SQL method benchmark run an instrumented operation (its own
  StoreTimer; measured operations have none) before and after the measured
  loop and assert the path: remote fetch has `remote_fetch` > 0, no
  `scan_index` and no `load_record`; SCAN_AND_FETCH has no remote fetch and an
  index scan. C2 also asserts the Skip placement: one remote-fetch opening
  whose entries include the skipped ones, or exactly one completed load per
  returned row for SCAN_AND_FETCH. The SQL method benchmark checks `EXPLAIN`
  (IDX_A, not covering) on its measured and audited connections, warms the
  audited connection before resetting its timer (catalog reads are record
  loads), and measures on a pinned connection without a timer.
- The SQL common reads warm and measure one pinned connection, not the pool.
  `TimestampInsert` reads back every row after the loop: exactly IDs
  `1..b.N`, each with its label.
- SQL benchmarks name their database per body run, drop a database or
  template of the same name through the system catalog first (an earlier
  process may have left it on a reused cluster), and drop both when the
  benchmark ends. Clearing the cluster between arm processes still keeps the
  directory-layer allocations, and so `prefix-B`, from drifting.
- `prefix-B` reports the store prefix length: fixed for the Record Layer
  subspaces, resolved through the relational keyspace for SQL, where the
  directory layer allocates it.

### Receipts

A receipt is one log line, `FDB_BENCH_AUDIT ` followed by a JSON object whose
`case` is the benchmark name. They are written only at these points, all
untimed:

| Benchmarks | When | Fields besides `case` |
|---|---|---|
| classic write benchmarks | after the loop | `first_id`, `count`, `present`, `n`, `prefix_bytes` |
| C1 | per payload fixture | `phase` = `fixture`, `payload`, `payload_bytes`, `chunks`, `rows`, `split`, `prefix_bytes` |
| C1 | before and after each cell's loop | `phase`, `method`, `payload`, `payload_bytes`, `chunks`, `reverse`, `skip`, `limit`, `rows`, `remote_openings`, `remote_entries`, `index_scans`, `record_loads`, `checksum`, `prefix_bytes` |
| C2 | fixture; before and after each cell's loop | fixture: `phase`, `rows`, `prefix_bytes`; cells: `phase`, `method`, `payload` (`small`), `reverse`, `skip` (3), `limit` (0), `rows`, `remote_openings`, `remote_entries`, `index_scans`, `record_loads`, `checksum`, `prefix_bytes` |
| SQL `PlanCacheHit`, `IndexScanRange` | after warm-up, before the timer starts | `query`, `rows`, `checksum`, `prefix_bytes` |
| SQL `TimestampInsert` | after the loop and read-back | `rows`, `first_id`, `last_id`, `prefix_bytes` |
| SQL method | fixture; before and after each cell's loop | fixture: `phase`, `rows`, `prefix_bytes`; cells: `phase`, `method`, `query`, `rows`, `remote_openings`, `remote_entries`, `index_scans`, `record_loads`, `checksum`, `prefix_bytes` |

`checksum` is the CRC-32 of the returned rows in order. Measured operations
collect no counters.

Counters are logical: `remote_fetch` counts remote-fetch openings, not RPCs;
`load_record` counts completed loads. No RPC observer exists.

## Client campaign plans

`matrix_plan.py` freezes a 2–6 arm plan with explicit CPU groups, per-arm
declared revisions, one shared option set and equal-length prefixes; blocks
must be a multiple of `factorial(arms)` (6 for three arms). `run.py` enforces
the 180-second campaign cap and waits 3 seconds (cooldown and quiescence)
before every run, so `matrix_plan.py` rejects plans whose waits alone reach the
cap: three arms fit 6, 12 or 18 blocks of one workload (12 blocks leave 72
seconds for 36 runs), and four or more arms (24 blocks minimum) cannot fit.
Size iterations per workload accordingly.
`analyze.py` reports geometric means of paired block ratios with **nominal 95%**
percentile block-bootstrap intervals (20,000 resamples by default) plus
moving-block and order sensitivity diagnostics. The 99% intervals in
`tools/clientbench/results/r17` came from a separate retained analysis, not
from `analyze.py`.
