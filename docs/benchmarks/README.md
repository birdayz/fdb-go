# Benchmarks

Raw output behind the tables in the top-level README.

## 2026-10-12, master 9a9a448

AMD Ryzen 9 3900X (24 threads), Linux, Go 1.26.9. Single-node FoundationDB 7.3.77
in Docker on the same host, started by each test binary: the client benchmarks use
the `ssd` engine, the Record Layer and SQL benchmarks `memory`. Each
benchmark ran 5 times for 3 s; the README reports the median. The runs were
sequential, on a host also running other work.

| File | Source | Benchmarks |
|---|---|---|
| [client.txt](2026-10-12/client.txt) | [`pkg/fdbgo/bench`](../../pkg/fdbgo/bench/bench_test.go) | `Get`, `GetRange`, `Set`: Go client vs. Apple's binding over libfdb_c 7.3.77 |
| [recordlayer.txt](2026-10-12/recordlayer.txt) | [`pkg/recordlayer`](../../pkg/recordlayer/benchmark_test.go) | `SaveRecord`, `SaveRecordWithIndex`, `LoadRecord`, `ScanIndexRecords` |
| [sql.txt](2026-10-12/sql.txt) | [`pkg/relational/sqltest/metamorphic`](../../pkg/relational/sqltest/metamorphic/plan_cache_bench_test.go) | `FDB_PlanCacheHit` (point select), `FDB_IndexScanRange`, `FDB_TimestampInsert` |
| [testcontainer.txt](2026-10-12/testcontainer.txt) | below | `foundationdbtc.Run` until it returns, image already pulled |

Reproduce (needs Docker):

```sh
bazelisk build //pkg/fdbgo/bench:bench_test //pkg/recordlayer:recordlayer_test \
  //pkg/relational/sqltest/metamorphic:metamorphic_test
flags='-test.run=^$ -test.benchtime=3s -test.count=5 -test.benchmem'
bazel-bin/pkg/fdbgo/bench/bench_test_/bench_test $flags \
  -test.bench='^Benchmark(Get|Set|GetRange)$'
bazel-bin/pkg/recordlayer/recordlayer_test_/recordlayer_test $flags --ginkgo.skip='.*' \
  -test.bench='^Benchmark(SaveRecord|LoadRecord|SaveRecordWithIndex|ScanIndexRecords)$'
bazel-bin/pkg/relational/sqltest/metamorphic/metamorphic_test_/metamorphic_test $flags \
  -test.bench='^BenchmarkFDB_(PlanCacheHit|IndexScanRange|TimestampInsert)$'
```

Testcontainer startup, timed with:

```go
start := time.Now()
c, err := foundationdbtc.Run(ctx, "")
elapsed := time.Since(start)
```
