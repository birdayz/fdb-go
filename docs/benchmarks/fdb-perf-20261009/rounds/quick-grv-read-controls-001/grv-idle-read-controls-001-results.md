# Short Read1000 and Get10000 controls — frozen GRV candidate

Campaign `quick-grv-idle-read-controls-001`, seed391714:6paired blocks perworkload, eacharmorder3times,24fresh processes total. Each process100warmup+300measured operations;7200measured operations. Completed in84seconds including cooldown/setup/analysis. No builds/tests/profiles ran concurrently. Same unmodified v3 harness/options,2second cooldown+1second group/perCPU gate, GOMAXPROCS8, client CPUs8–15/FDB2–7/collector0–1.

## Results

Geometric means of paired RUN-level Go/CGo latency ratios:

| Workload | Mean ratio | Observed mean-ratio range across6pairs | p50 ratio | Run-p99 ratio |
|---|---:|---:|---:|---:|
| Read1000 | 0.75861 | 0.73643–0.80431 | 0.72619 | 0.90925 |
| Get10000 | 0.67654 | 0.61068–0.81451 | 0.67636 | 0.68816 |

Observed paired mean latency was about24.1% lower for Read1000 and32.3% lower for Get10000. All6mean-latency pairs favored Go in each workload. This does not imply every tail comparison or CPU metric wins.

Pooled latency summaries are descriptive and distinct from paired run-level ratios:

| Workload/arm | Mean µs | p50 µs | p99 µs |
|---|---:|---:|---:|
| Read1000 Go | 478.925 | 454.187 | 939.873 |
| Read1000 CGo | 631.281 | 616.986 | 1122.802 |
| Get10000 Go | 512.714 | 493.286 | 935.873 |
| Get10000 CGo | 760.545 | 756.034 | 1353.421 |

Read1000 paired run-p99 ratios ranged0.48488–1.68780: substantial variation, with some pairs favoring CGo despite the lower pooled Go p99. Get10000 paired run-p99 ratios ranged0.60617–0.84747. Small-N tails remain diagnostic, not publication-grade inference.

Median RUN-level process CPU and Go heap counters:

| Workload/arm | CPU µs/op | Go heap B/op | Go allocations/op |
|---|---:|---:|---:|
| Read1000 Go | 530.315 | 39288.20 | 63.48 |
| Read1000 CGo | 434.040 | 1488.00 | 13.00 |
| Get10000 Go | 566.433 | 17007.80 | 85.24 |
| Get10000 CGo | 592.925 | 10809.81 | 16.14 |

Paired run CPU/op ratios Go/CGo: Read10001.21911, Get100000.99678. Read1000 therefore still uses about21.9% MORE paired process CPU despite lower wall latency and allocates substantial Go heap bytes. Get10000 paired CPU is nearly equal; the median CPU table is a different descriptive aggregation. Native C++ heap is EXCLUDED from Go allocation statistics; these are not total-memory comparisons. Process CPU sums every process thread. No new profiler was run in this campaign, so allocation sites are not attributed from these counters alone.

## Validation

All24arms passed exact CSV counts/index/phase checks, exact returned values, fresh-transaction persistence checks and prearm quiescence gates. Zero measured/warmup errors or callback retries.

For each workload's1800measured Go operations:1800successful GRV batches/readversions,0commits,0GRVcachehits,0connectionfailures and0transaction retries. Go executable loaded no native FDB library. Every CGo arm loaded libfdb_c SHA256 `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`; native RPC counters are UNOBSERVED, not zero. See `counter-path-validation.json`.

Read1000 uses ReadTransact and a1000-byte value; Get10000 uses Transact/Get/emptyCommit and a10000-byte value. Sizes are decimalbytes. These are the frozen v3 workload definitions, not byte-identical replay of historical repository benchmarks.

## Source and correctness status

Exact frozen `grv-idle-001` binary reused; no source/harness changes or rebuilds for this campaign:
- Base commit `26abdbaac12be60ea8955027f6047e10474596f1`.
- Client delta SHA256 `32d59b91cdd509ba0cb985f88f52a2d62fdae4ad0020f832183ba79555e024e3`.
- Go binary `5b6e7302f977785317a83e4d9172555c48fe400f9362536e7fe8436d0b9c355b`.
- CGo binary `af2e91489057be13a4b74c55c047b06de15d2483a981697523b076648f929dde`.
- Frozen v3 harness `41f594c990477bc4b29c565160599a57266cecb23adaae582180f7464fb2d0ea`.
- FDB7.3.77/API730, Go1.26.9; same existing co-located VM/native SSD-backed server.

At authorization, root reported full103 running on fixture/resource/comment-only changes; measured production candidate already passed actual client_test and its GRV/retry regressions. Earlier full91 completed149/150 (103actual,47cached), with only a lockregistry polling fixture failure subsequently fixed locally. Latest local tree was NOT claimed full-green. This measurement is not a remote full-suite rerun/pass claim. See `authorization.json`.

One guest/server, co-location, short fresh-process windows, sixblocks and adaptive selection limit generalization; no physicalSKU or whole-host-isolation claim. No further comparison/profile queued after this authorized campaign.
