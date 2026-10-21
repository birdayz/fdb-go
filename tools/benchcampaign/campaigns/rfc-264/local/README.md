# Fixed-time local client benchmarks

The [RFC-264 result](../../../../../docs/benchmarks/rfc-264/local-quick-screen.md)
rejected both measured prototypes. These tools preserve the measurement method;
they do not start a cluster or authorize further measurements.

## Configure

Copy `rfc264.json` and replace its three path placeholders with absolute paths to
the build manifest, an existing cluster file and `fdbcli`. The template preserves
the recorded source/workload/order settings: three arms, six cells and four
ABC/BCA/CAB/ABC blocks. It does not contain executable binaries or credentials.

Supply a build manifest with every configured arm:

```json
{
  "schema": 1,
  "base_commit": "<full source revision>",
  "harness_patch_sha256": "<64 lowercase hex>",
  "arms": {
    "master": {
      "binary": "/absolute/path/to/clientbench",
      "binary_sha256": "<64 lowercase hex>",
      "source_revision": "<full source revision>",
      "worktree": "/absolute/path/to/worktree",
      "prototype_patch_sha256": "<64 lowercase hex; hash empty bytes for baseline>",
      "go_version": "go1.26.10"
    }
  }
}
```

All arms must share the same workload harness. Record the actual compiler and
binary identities for a new run; do not relabel new binaries as the historical
result. The two [prototype patches](prototypes/) are archival inputs against
`68b8a64d274d5e5a041912934198ece79e7e009a`, not production changes made by this PR.

## Run

Use an exclusive machine and a nice-0 controller. The driver supplies client
CPU affinity and `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`, empty `GODEBUG` and
closed-loop operation. It removes inherited `GOFLAGS`, `GOEXPERIMENT` and
`FDB_NETWORK_OPTION_*` settings. Workloads, worker counts and per-block arm orders
are configurable; the same driver supports two-arm paired comparisons.

```sh
python3 quick_local.py run --config /absolute/path/to/config.json \
  --output /absolute/path/to/fresh-output --allow-run
```

`--allow-run` is an explicit acknowledgement, not a substitute for permission to
use a shared machine. Output must not exist; there is no resume mode. Each fresh
process uses `--warmup-duration 10s --duration 30s`. The recorded three-arm
configuration has 72 processes and 48 minutes of warmup plus measurement;
fixture, raw-output, validation and compression time is additional.

The driver acquires `$FDB_PERF_HOME/validation/fast.lock` nonblockingly. When
`FDB_PERF_HOME` is unset, its root defaults to `~/.local/share/fdb-perf`. Do not
hold that lock around the command. It is retained through owned-process cleanup,
owned-prefix cleanup and report generation.

Initial checks require a healthy, available cluster and matching server
addresses/CPU affinity. Server cluster-file paths may differ if their normalized
connection bytes are identical. The driver never starts, reconfigures, stops or
repins servers. Each client has an owned process group and a hard timeout.
Only the unique campaign prefix is written or cleared. Cleanup enables writes,
clears that prefix and requires the exact successful empty-range receipt.
Failure to verify cleanup invalidates the result. SIGKILL or power loss cannot
run cleanup; retain the recorded prefix for explicit recovery.

## Evidence and report

The harness retains `manifest.json`, `timed-summary.json` and
`timed-samples.csv`: all measurement and boundary-drain samples, with separate
success/error booleans, latency, completion time and actual callback attempts.
Raw validation precedes gzip level-1 compression outside the measured window.
The plain file is removed only after the gzip footer and path/hash receipt are
written. Reanalysis accepts either plain CSV or a receipt-verified gzip, not
ambiguous duplicate copies.

```sh
python3 quick_local.py summarize --campaign /absolute/path/to/complete-output
```

Reanalysis launches no clients and contacts no cluster. It requires a complete,
cleanup-verified population. Infrastructure, identity, persistence and evidence
failures are fatal; terminal operation errors remain counted observations.

Each cell/arm table contains exactly four signal groups:

1. **Latency:** nearest-rank successful p50/p99. Error p99 uses pooled raw error
   latencies only with at least 20 errors per arm/cell; otherwise count plus n/a.
2. **Traffic:** successful operations divided by measured window seconds.
3. **Errors:** terminal errors and transaction-callback retries per 1,000
   completed logical operations, with pooled raw counts beside the rates.
   Retries are callback entries minus one, clamped at zero; internal RPC retries
   and hedges are unobserved.
4. **Saturation:** aggregate process user+system CPU divided by measured wall
   time (cores busy), and by successful operations (CPU/op). The CPU snapshot
   precedes the drain join but may include concurrent boundary work up to that
   snapshot. Boundary-drain operations do not enter headline populations.

Absolute latency, traffic and saturation values are geometric means of run-level
values. Error/retry rates use pooled counts and completed-operation denominators.
Ratios are geometric means of paired block ratios, with 20,000 whole-block
percentile bootstrap resamples, seed 492817 and nominal 95% intervals. Error-p99
ratios require at least 20 errors in every paired run. Zero error/retry ratios
are undefined; no pseudocounts are added.

The `noninferiority` rule requires every cell's throughput lower95 to be at least
0.98 and CPU/op upper95 to be at most 1.02. Intended p50 improvements are reported
separately; there is no automatic prototype selection or promotion. The
`significant_regression` variant returns BLOCK if any cell has throughput point
at most 0.95 and upper95 below 1, or CPU/op point at least 1.05 and lower95 above 1;
otherwise PASS. Missing comparison populations cannot pass. These are distinct
preregistered rules, not interchangeable equivalence claims.

## Tests

The ordinary Bazel fast lane includes `local_test`. Its fixtures exercise
schedules, window populations, identity, compression, bootstrap, decision
boundaries and owned cleanup without a cluster, Docker, build or benchmark.
Duration-mode window and CPU accounting tests live in
`tools/clientbench/internal/bench/duration_test.go`. Run repository validation
through `just test`.
