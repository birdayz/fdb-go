# Benchmarks

Raw output behind the tables in the top-level README.

## Dedicated-host campaigns

Each campaign's reports live in a folder named after the campaign.

- [hcloud-rebench-20261014](hcloud-rebench-20261014/README.md): one dedicated
  16-vCPU host; client (pure-Go vs CGo), Record Layer, remote fetch and SQL
  phases, each counterbalanced across the trees of the RFC-258 work. Harness:
  [campaign-harness.md](campaign-harness.md); campaign tooling:
  [`tools/benchcampaign`](../../tools/benchcampaign/README.md).
- [hcloud-multinode-vpc-20261014](hcloud-multinode-vpc-20261014/README.md):
  four dedicated 16-vCPU hosts on an isolated private VPC (1 client host + 3
  FoundationDB server hosts, `double` redundancy, `ssd-2`, 9 storage servers);
  multi-node `USE_REMOTE_FETCH` vs `SCAN_AND_FETCH` (`M2rl`, `M2skip`) and
  pure-Go vs CGo concurrency scaling at 1, 8, 32, and 64 workers (`M1`).
- [fdbmn-20261020](fdbmn-20261020/README.md): the multi-node phases repeated on
  master `fa8725f1d` under a new preregistered protocol; phase pages
  [M1](fdbmn-20261020/phase-m1-client.md) and
  [M2](fdbmn-20261020/phase-m2-remotefetch.md), detailed analysis in
  [RESULTS.md](fdbmn-20261020/RESULTS.md), local diagnostic profiles in
  [profiles-local.md](fdbmn-20261020/profiles-local.md).
- [fdb-perf-20261009](fdb-perf-20261009/README.md): one dedicated VM, rounds
  R1–R18 of pure-Go vs CGo client comparisons, including the R17 confirmatory
  round and its preregistration.
- [OTel ingest 1x/2x/3x on Kubernetes + Cilium](otel-ingest-25k-k8s-cilium-scaling.md):
  six `ccx43` hosts, k3s + Cilium native routing, fdb-kubernetes-operator
  v2.38.0, FDB 7.3.77 `single` redundancy with up to 18 Redwood storage servers.
  Record Layer span ingest with 11 KV writes per span reached 25,387.9 ACKed and
  21,697.6 drained spans/s at 3x. The verdict is `NEAR_LINEAR`: 108.3% drained
  efficiency and 89.5% ACK efficiency. The report covers the root causes found
  on the way (TLog memory monitor, cold-range hotspot, topology) and the run's
  limits (one run, not sustained, no HA).
- [OTel spans in relational OTEL_V1 stores, local](otel-relational-local-20261107.md):
  one host, single-process FDB. The receipts of the relational OTEL_V1 store
  that replaced the custom-index store. Span ingest through the replacement's
  writer costs 283 against 372 µs/span of client CPU (0.76x) with 42% fewer
  prefix-compressed bytes (G2A). Against the custom store at the merge-base,
  log ingest takes 0.38x the CPU and 0.95x the bytes per log (G2B). In G3,
  also against the merge-base, collector-bench ingest with messages takes
  0.93x the wall time and 0.70x the process CPU, every query
  class's p50 is at most 0.95x after the queues drain, and otelbench's direct
  reads are at most 0.63x and its ingest 0.68x the CPU. Round 1's 1.32x CPU figure is
  superseded. Also covers SQL and direct-scan query latency and the relational
  fixes the comparison found.
- [GenAI/OTel collector ingest 1x/2x/3x on Kubernetes + Cilium](otel-genai-collector-ingest-k8s-scaling.md):
  the same six-host setup running a sustained 10-minute custom in-process
  Collector-model queue/exporter experiment, not an upstream Collector pipeline
  (spans, correlated logs, deduplicated messages with a TEXT index, queries
  under load; 600 s including 60 s warmup). Nine measured runs
  (`k8s-fts6`..`k8s-fts13`, `k8s-fts14c`) for PR #1023 took 1x from
  666 to 2,785 spans/s and 3x to 8,178 spans/s (97.9% efficiency), and cut
  storage mutations from 34 to 6.6 KB per span at 1x. Producers use independent
  stores in one shared FDB cluster per stage. `fts14c` had zero ingest/query
  failures, but its original cleanup gate failed; a later read-only inventory
  recheck passed. These experiments are not correctness evidence. The report
  traces the bottlenecks in order (storage-bound layout, client CPU, single-flight
  writers, TEXT read round trips) and includes a box-3 CPU profile of the
  remaining cost.

The two 2026-10-14 campaigns keep their frozen protocol versions under `protocol/`
([single-node](hcloud-rebench-20261014/protocol/),
[multi-node](hcloud-multinode-vpc-20261014/protocol/), with the provisioning plan).

## Change-specific comparisons

- [2026-10-12: record-load range-read parity](2026-10-12-record-load-parity/README.md)
  — before/after latency and allocations, including the cost of Java-compatible reads.
- [RFC-259 prototype](rfc-259/README.md): server CPU per transaction with GRV batch
  windows and write coalescing, against libfdb_c, at 1–32 workers.
- [RFC-263 revisions 4–6](rfc-263/README.md): timer/incarnation prototype screens,
  production proofs and traces, and fixed-population p999 confirmation. CPU qualification
  passed, along with p999 and queue-wait; merge gates remain pending.

Design notes, draft RFCs and the benchmark backlog: [docs/perf](../perf/README.md).

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
