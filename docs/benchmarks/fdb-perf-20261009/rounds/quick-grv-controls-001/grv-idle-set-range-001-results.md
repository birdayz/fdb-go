# Short Set100 and Range100 controls — frozen GRV candidate

Campaign `quick-grv-idle-set-range-001`, seed391713:6paired blocks perworkload, eacharmorder3times,24fresh processes total. Each process100warmup+300measured operations;7200measured operations. Completed in91seconds including cooldown/setup/analysis. No builds/tests/profiles ran concurrently. Same unmodified v3 harness/options,2second cooldown+1second group/perCPU gate, GOMAXPROCS8, client CPUs8–15/FDB2–7/collector0–1.

## Results

Geometric means of paired RUN-level Go/CGo latency ratios:

| Workload | Mean ratio | Observed mean-ratio range across6pairs | p50 ratio | Run-p99 ratio |
|---|---:|---:|---:|---:|
| Set100 | 0.88621 | 0.64025–1.04707 | 0.88335 | 0.69337 |
| Range100 | 0.64269 | 0.52995–0.68014 | 0.63032 | 0.74532 |

Thus observed paired mean latency was about11.4% lower for Set100 and35.7% lower for Range100. Set100 varied materially: one pair favored CGo in mean latency; this is not an every-run win. All6Range100 mean ratios favored Go. Small-N tails and adaptive iteration remain diagnostic, not publication-grade inference.

Pooled latency summaries are descriptive and distinct from paired run-level ratios:

| Workload/arm | Mean µs | p50 µs | p99 µs |
|---|---:|---:|---:|
| Set100 Go | 1828.669 | 1857.527 | 3821.373 |
| Set100 CGo | 2047.369 | 1972.566 | 5579.302 |
| Range100 Go | 582.874 | 571.156 | 1214.482 |
| Range100 CGo | 902.633 | 874.723 | 1736.199 |

Median RUN-level process CPU and Go heap counters:

| Workload/arm | CPU µs/op | Go heap B/op | Go allocations/op |
|---|---:|---:|---:|
| Set100 Go | 590.227 | 8539.49 | 86.97 |
| Set100 CGo | 570.487 | 392.05 | 10.00 |
| Range100 Go | 652.053 | 42135.29 | 95.58 |
| Range100 CGo | 677.745 | 19308.64 | 224.17 |

Paired run CPU/op ratios Go/CGo: Set1001.02381, Range1000.96036. Go therefore still consumed slightly more process CPU on Set100 despite lower observed mean wall latency; Range100 consumed slightly less CPU and fewer Go allocation objects, but more Go heap bytes. Native C++ heap is EXCLUDED from Go allocation statistics: these are not total-memory comparisons. Process CPU sums every process thread.

## Validation

All24arms passed exact CSV counts/index/phase checks, exact values/range rows, fresh-transaction persistence checks and prearm quiescence gates. Zero measured/warmup errors or callback retries.

For1800measured Set100 Go operations:1800successful GRV batches/readversions and1800started/completed commits. For1800measured Range100 Go operations:1800successful GRV batches/readversions and0commits. Both:0GRVcachehits,0connectionfailures,0transaction retries. Go executables loaded no native FDB library. Every CGo arm loaded the expected libfdb_c SHA256 `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`; its RPC counters are UNOBSERVED, not zero. See `counter-path-validation.json`.

Set100 overwrites a preseeded perworker key with changing100-byte values and validates persisted final bytes after timing. Range100 checks100exact ordered rows with8-byte values, WantAll and limitN+1. These are the frozen v3 workloads, not byte-identical replay of the historical repository benchmarks.

## Source and correctness status

Exact frozen `grv-idle-001` binary reused; no source or harness changes/rebuilds for this campaign:
- Base commit `26abdbaac12be60ea8955027f6047e10474596f1`.
- Client delta SHA256 `32d59b91cdd509ba0cb985f88f52a2d62fdae4ad0020f832183ba79555e024e3`.
- Go binary `5b6e7302f977785317a83e4d9172555c48fe400f9362536e7fe8436d0b9c355b`.
- CGo binary `af2e91489057be13a4b74c55c047b06de15d2483a981697523b076648f929dde`.
- Frozen v3 harness `41f594c990477bc4b29c565160599a57266cecb23adaae582180f7464fb2d0ea`.
- FDB7.3.77/API730, Go1.26.9; same existing co-located VM/native SSD-backed server.

At root authorization: full91completed149/150 (103actual,47cached), only a recordlayer lock-registry20ms polling-pulse fixture failed. Root had fixed that fixture locally and fast102 was running. Root reported production client unchanged since this frozen candidate (comments/CPU metadata only), but latest local tree was NOT asserted full-green. Earlier actual client_test and all new GRV/retry regressions passed before this candidate's first measurement. This result is not a remote full-suite pass claim. See `authorization.json`.

One guest/server, co-location, short fresh-process windows, sixblocks and adaptive selection all limit generalization; no physicalSKU or whole-host-isolation claim. No further comparison/profile queued after this authorized campaign.
