# Stress & Performance Benchmarks

## Performance Summary

All benchmarks run against a single-node FDB 7.3 testcontainer (SSD/Redwood engine).

### Per-row SaveRecord (Java-aligned, existence checks enabled)

| Workers | Go (rows/s) | Java sync (rows/s) | Go advantage |
|---------|-------------|---------------------|--------------|
| 1       | 15,500      | 9,020               | 1.7x         |
| 4       | 38,600      | 24,172              | 1.6x         |
| 8       | 49,900      | 29,310              | 1.7x         |

### SaveRecord approaches (single transaction, 2000 rows/batch)

| Approach              | rows/s | vs sequential |
|-----------------------|--------|---------------|
| sequential            | 15,500 | 1.0x          |
| concurrent_8          | 32,300 | 2.1x          |
| concurrent_32         | 42,100 | 2.7x          |
| concurrent_128        | 44,500 | 2.9x          |
| concurrent_512        | 45,700 | 2.9x          |
| SaveRecordBatch       | 59,000 | 3.8x          |

`SaveRecordBatch` pipelines all existence-check reads upfront (one TCP flush),
then resolves sequentially. It is semantically identical to calling `SaveRecord`
N times — same existence checks, same index maintenance, same record counts.

### SQL INSERT (1M rows, 4 workers)

| Table     | Indexes | rows/s |
|-----------|---------|--------|
| customers | none    | 16,600 |
| orders    | 3 VALUE | 8,600  |

### 10M ingest (minimal schema, 4 workers)

10,000,000 rows in 4m22s at **38,100 rows/s** — flat throughput, zero degradation.

## Root cause: conflict range fix

The primary scaling fix was in `loadRecordStoreState`: the store header read
used `GetRange(subspace.range(), 1)` which generated a read conflict range
covering the **entire subspace** `[ss\x00, ss\xff)`. Every concurrent
`SaveRecord` write conflicted with this range, causing 178% transaction retries
at 8 workers.

Fix: point-read the exact store info key (`tx.Get(expectedInfoKey)`), generating
a minimal conflict range `[key, key\x00)`. Java uses `getRange` but doesn't
suffer this issue because Java's async client pipelines reads differently.

## Build tags

These files carry `//go:build stress`. A plain `go test ./...` does **not** set
the tag, so it skips this package entirely — keeping the default Go test run fast
and clean instead of spinning up million-row FDB workloads. Bazel sets the tag
globally via `.bazelrc` (`--@rules_go//go/config:tags=...,stress`), but the
`stress_test` target is also `manual`, so `bazel test //...` never picks it up;
only an explicit invocation (the commands below, or the nightly stress workflow)
runs it. To run directly with the Go toolchain, pass `-tags stress`.

`stress_10m_test.go` additionally carries `realcluster`. That second tag is what
keeps the **advertised scale list equal to the executed scale list**: under
plain `stress`, this package is exactly 10K / 100K / 1M and runs all three. The
10M scales are not compiled in at all, rather than being registered and skipped
— a single-node FDB testcontainer cannot sustain 10M rows of writes (sustained
load wedged the container, and the client, correctly having no default
transaction timeout, retried a dead cluster to the 1h deadline).

## Scale coverage

| Scale | Tag              | Where it runs                          |
|-------|------------------|----------------------------------------|
| 10K   | `stress`         | nightly-stress (whole target, no filter)|
| 100K  | `stress`         | nightly-stress (whole target, no filter)|
| 1M    | `stress`         | nightly-stress (whole target, no filter)|
| 10M   | `stress realcluster` | manual, against a real cluster only|

The nightly workflow passes no `--test.run`, so it executes the entire target —
all three container-sized scales, plus the ingest-parallelism matrix and the
SaveRecord comparisons.

## Running

```sh
# All stress tests
bazelisk test //pkg/relational/sqldriver/stress:stress_test --test_output=streamed

# Specific benchmark
bazelisk test //pkg/relational/sqldriver/stress:stress_test \
  --test_arg="--test.run=TestFDB_SaveRecordConcurrentVsBatch" \
  --test_output=streamed --cache_test_results=no

# 1M stress suite (all query types)
bazelisk test //pkg/relational/sqldriver/stress:stress_test \
  --test_arg="--test.run=TestFDB_Stress_1M" \
  --test_arg="--test.timeout=600s" \
  --test_output=streamed --test_timeout=600

# 10M scales — NOT a Bazel target (the `realcluster` tag is not in .bazelrc, so
# gazelle leaves stress_10m_test.go out of the go_test srcs). Point it at a real
# cluster; a testcontainer cannot sustain these.
FDB_STRESS_CLUSTER_FILE=/etc/foundationdb/fdb.cluster \
  go test -tags 'stress realcluster' ./pkg/relational/sqldriver/stress/ \
  -run 'TestFDB_(Stress|Ingest)_10M' -timeout 3h -v
```

`FDB_STRESS_CLUSTER_FILE` also works for the container-sized scales, if you want
to run them against an existing cluster instead of a fresh testcontainer. It is
strict: an unreadable path fails the run rather than quietly falling back to
Docker, so numbers labelled real-cluster always are.
