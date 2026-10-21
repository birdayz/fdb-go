# Multi-node FoundationDB benchmark — 20 October 2026

## Executive findings

**New Go comes with higher latency than 267a4143 in prominent directly paired cases, alongside lower allocation and concurrent lifecycle CPU.** At w64, get100 p50 is **1.348 [1.308, 1.395]** (mean 1.310), get1000 p50 **1.298 [1.254, 1.331]** (mean 1.261), and set100 p50 **1.283 [1.273, 1.294]**—about 28% higher—with p99 **1.110 [1.055, 1.190]**. At w8, get1000 p50 is **1.154 [1.139, 1.172]**; set100 p50 is **1.137 [1.099, 1.168]**, mean **1.178 [1.138, 1.217]**, and p99 **1.109 [1.047, 1.163]**—about 11% higher. Against the same campaign's CGo/libfdb_c arm, new/CGo p50 at w64 is **0.934 [0.911, 0.966]** for get100, **0.770 [0.730, 0.821]** for get1000 and **0.893 [0.887, 0.899]** for set100; previous/CGo p50 is **0.693**, **0.593** and **0.696**, respectively, so the new-Go median-latency advantage over CGo is narrower. At w8, new/CGo p50 is **0.977 [0.961, 0.992]**, **1.003 [0.986, 1.021]** and **1.013 [0.978, 1.047]** in the same workload order: get100 is below CGo, while get1000 and set100 remain unresolved around one, not evidence of equivalence. The CPU/allocation gains do not remove the new/previous regressions: set100 E−A new/previous CPU is 0.586 at w64 and 0.553 at w8, while Go bytes/malloc ratios are 0.612/0.561 and 0.629/0.555, respectively. **This is not an across-the-board speedup, and no causal mechanism is inferred.**

- **Set100 is measured cleanly at w8 and w64, not w1.** At w64, new/previous ratios are p50 **1.283 [1.273, 1.294]**, p99 **1.110 [1.055, 1.190]**, and lifecycle CPU E−A **0.586 [0.572, 0.599]**. Go bytes fall to **0.612** and mallocs to **0.561** of previous Go. At w8, p99 is **1.109 [1.047, 1.163]** and E−A **0.553 [0.542, 0.565]**.
- **The concurrent Go-versus-CGo set CPU gap narrows but remains.** E−A new/CGo is **1.148 [1.119, 1.179]** at w8 and **1.249 [1.213, 1.284]** at w64, versus previous/CGo **2.075** and **2.132** within this campaign. At w64 new/CGo p50 is **0.893 [0.887, 0.899]** and p99 **0.900 [0.835, 0.989]**. W8 new/CGo p50 is unresolved, **1.013 [0.978, 1.047]**; its p99 upper bound is only just below one (unrounded **0.99954**), so do not present that as a strong tail win.
- **Allocation reductions are consistent across all 14 eligible M1 cases; latency and CPU are not.** New/previous Go bytes range **0.612–0.941**, mallocs **0.555–0.811**. Single-worker E−A rises for every eligible read case despite fewer allocations. At concurrency, E−A usually falls, but w64 range100 is uncertain. Its new/previous p50 is **1.435 [1.099, 2.378]**; that wide, unstable interval is why range100 is left out of the opening headline. GRV median latency improves at w8/w64; Get and Set medians generally worsen.
- **Mapped remote fetch is not uniformly faster in the current topology.** One-row small fetch has a modest ~2–3% mean advantage; large small-payload fetch, positive Skip and the eligible split multi-row cases are slower remotely while allocating fewer Go bytes. The prior campaign’s reverse-Skip10 result does not reproduce: remote/ordinary **0.158** then versus **1.169 [1.151, 1.185]** now. This is a descriptive cohort comparison, not a production-change causal estimate.
- **26 cases are eligible.** No partial-arm/block salvage, reruns or capacity substitution. W1 loses all write comparisons as a consequence of the preregistered stopping policy. Both concurrent batch cases remain unavailable. W32 was dropped for budget before measurement and was not restored when execution ended early. A1 re-attempts were explicitly declined.

Preregistered ratios below are numerator/denominator: **below one means less latency/CPU/allocation**, not an aggregate speedup. In the post-hoc throughput section, **above one means more operations/s**. Brackets are nominal 95% paired-block percentile bootstrap intervals. Six blocks, not transaction counts, determine the independent sample size. There is no multiplicity correction, equivalence claim, pooled campaign inference or worker-scaling claim.

## Scope, identities and statistical method

| Item | Recorded configuration |
|---|---|
| Current production (`final`, “new”) | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` |
| Previous production (`before`, “previous”) | `267a41434a81723b07e7150869d702636a1b91a9` — previous campaign’s final Go |
| CGo | Apple binding built from current tree; hash-pinned libfdb_c 7.3.77 |
| Frozen workload/timing base | `d94b4c61377d173b7c6884fde426a227dab0f0bb` plus declared compatibility amendment below |
| Compiler | Go 1.26.9; compiler SHA256 `e90c4e12df236ea0c2e7536fd68da8a47fc0b585c19588bf6c1a70cfc296a0f9` |
| Deployment | Four dedicated ccx43, fsn1, AMD EPYC-Milan, each 16 vCPU/64 GB; actual private VPC, no netem |
| FDB | 7.3.77/API 730; double ssd-2; 17 processes, 9 storage (3 per node), 3 coordinators; client runs no fdbserver |
| Client runtime | GOMAXPROCS=8, GOGC=100, GOMEMLIMIT=off; benchmark CPUs 8–15, housekeeping 0–1, CPUs 2–7 idle |
| M1 | Three arms; six blocks/all six permutations once; 18 processes per workload. Per-process 180 s/per-workload 900 s caps retained |
| M1 geometry | w1: 3000 measured + 200 warmup total; w8: 12,000 + 800; w64: 32,000 + 1600. Counts differ: no scaling/capacity inference |
| M2 | New-master ordinary vs strict remote only; six balanced blocks; three testing.B repeats/process; 3 s benchtime; mean `ns/op`, **not p99** |
| Inference | Existing frozen numerical APIs; seed 492817; 20,000 whole-paired-block resamples; all arms retained together |

**CPU scope matters.** E−A is process user+system CPU after the final fixpoint/database close/native StopNetwork, minus the post-identity/pre-backend-open snapshot, divided by measured transactions. It includes startup, fixtures, warmup, validation, CSV output and shutdown/drain. It is not isolated steady-state transaction CPU. Window CPU can shift deferred native finalization outside its window; it is not substituted for lifecycle evidence. Absolute whole-process E includes identity hashing and is not paired cross-arm evidence. Go allocation metrics exclude the native C++ heap.

The approved compatibility amendment is **“d94b4c613 + #948 lifecycle drain fix + matching run.py validator.”** It accepts fully executed, stable queued cleanup chains; workload, timing, pacing, CPU accounting and numeric estimators are unchanged. All three client arms use the identical amended archive. Combined patch SHA256 `31bc349b7a1ef71fc5bbf300ed728fe536e161f5610a1187edc718e63fddb007`; archive `76990b867f337bdad0132cc69aa3f6f9a9bcf0cb0859cc8c7f8514def684f508`. Original inputs/builds are retained. This compatibility difference is another reason historical comparisons remain descriptive.

## Coverage and omissions

| Phase | Planned cases | Clean | Contaminated | Unverifiable/unreached | Consequence |
|---|---:|---:|---:|---:|---|
| M2rl |12|9|3|0|Three whole cases excluded by pre-arm client CPU gates|
| M2skip |4|3|1|0|rows10-forward excluded by pre-arm client CPU gate|
| M1w1 |7|4|1|2|set100 failed before its first arm; both batch cases not reached|
| M1w64 |7|5|1|1|set100 clean; batchset50 post-arm log-queue gate; batchset50x1000 not reached|
| M1w8 |7|5|1|1|set100 clean; batchset50 post-arm log-queue gate; batchset50x1000 not reached|
| M1w32 |7|—|—|—|Unexecuted-for-budget after11:15 setup rule; never substituted with capacity data|

M2rl omissions: `split-limit1-reverse`, `split-rows10-forward`, `split-rows1000-forward`. M2skip omission: `skip3-rows10-forward`. Every other eligible case retains its complete six-block population and clean R4 reconciliation. All five phase controllers prove runner quiescence and report no orchestration errors; rc3 records known adverse/omitted populations, not an overall clean campaign pass.

**W1 write-case loss is a protocol consequence.** The preregistered protocol preregisters the “conservative stop-on-runner-failure policy”; the driver stops on any nonzero case return. Set100’s first arm was not launched after the CPU rejection. Neither batch case was reached. The owner declined A1/new-protocol re-attempts under the existing hard guard. All three comparisons remain N/A; next-campaign backlog is w1 writes plus the pre-arm burst fix.

At w64 the first new-Go batchset50 arm completed 32,000/32,000 commits with 0 errors/retries, persisted validation OK and complete drains; the post-arm log queue was **426,278,385 bytes**, violating the unchanged 100 MB gate. At w8 it was **118,497,454 bytes** after the first new-Go arm. Entire comparisons are excluded; these retained single-arm records are not throughput claims. No historical production or threshold was patched.

## M1: current new Go versus previous Go

Each cell is a directly estimated ratio and nominal 95% interval, never a quotient of separately estimated ratios. `wN/workload` identifies independent within-worker comparisons. All other workloads are N/A as listed above.

|Case|Mean latency new/previous|p50 new/previous|p99 new/previous|E−A CPU new/previous|
|---|---|---|---|---|
|w1/grv|1.046 [1.002, 1.076]|1.049 [1.006, 1.080]|1.073 [0.927, 1.245]|1.292 [1.255, 1.330]|
|w1/get100|1.094 [1.062, 1.133]|1.088 [1.053, 1.128]|1.108 [0.984, 1.242]|1.190 [1.166, 1.211]|
|w1/get1000|1.088 [1.071, 1.103]|1.091 [1.082, 1.099]|1.121 [0.965, 1.302]|1.186 [1.166, 1.205]|
|w1/range100|1.077 [1.062, 1.091]|1.080 [1.064, 1.092]|1.106 [1.021, 1.202]|1.125 [1.108, 1.138]|
|w8/grv|1.034 [1.023, 1.047]|0.941 [0.930, 0.954]|0.945 [0.895, 1.001]|0.817 [0.792, 0.843]|
|w8/get100|1.110 [1.090, 1.133]|1.113 [1.097, 1.128]|1.067 [0.919, 1.244]|0.762 [0.750, 0.774]|
|w8/get1000|1.147 [1.136, 1.157]|1.154 [1.139, 1.172]|1.161 [1.024, 1.286]|0.762 [0.745, 0.780]|
|w8/range100|1.118 [1.101, 1.139]|1.112 [1.096, 1.129]|1.056 [0.950, 1.191]|0.865 [0.801, 0.977]|
|w8/set100|1.178 [1.138, 1.217]|1.137 [1.099, 1.168]|1.109 [1.047, 1.163]|0.553 [0.542, 0.565]|
|w64/grv|0.956 [0.937, 0.974]|0.923 [0.907, 0.938]|0.923 [0.813, 1.036]|0.707 [0.692, 0.720]|
|w64/get100|1.310 [1.273, 1.354]|1.348 [1.308, 1.395]|1.125 [1.057, 1.198]|0.738 [0.719, 0.759]|
|w64/get1000|1.261 [1.231, 1.290]|1.298 [1.254, 1.331]|1.077 [1.002, 1.150]|0.742 [0.716, 0.771]|
|w64/range100|1.393 [1.079, 2.279]|1.435 [1.099, 2.378]|1.134 [0.875, 1.860]|1.180 [0.999, 1.567]|
|w64/set100|1.265 [1.253, 1.277]|1.283 [1.273, 1.294]|1.110 [1.055, 1.190]|0.586 [0.572, 0.599]|

### Go allocation changes

Absolutes are existing analyzer medians of run-level counters. Ratio intervals resample the same six complete blocks. Native heap is not included.

|Case|Go bytes/tx previous → new|Bytes ratio [95%]|Go mallocs/tx previous → new|Malloc ratio [95%]|
|---|---|---|---|---|
|w1/grv|3,834.0 → 3,370.1|0.879 [0.878, 0.881]|38.09 → 30.10|0.790 [0.790, 0.791]|
|w1/get100|6,366.7 → 5,886.5|0.924 [0.924, 0.925]|69.20 → 54.31|0.785 [0.785, 0.785]|
|w1/get1000|8,234.3 → 7,749.7|0.941 [0.941, 0.942]|69.22 → 54.32|0.785 [0.785, 0.785]|
|w1/range100|50,284.6 → 44,835.0|0.891 [0.891, 0.892]|75.71 → 60.33|0.797 [0.796, 0.797]|
|w8/grv|3,549.6 → 2,973.0|0.837 [0.836, 0.839]|35.78 → 27.79|0.777 [0.776, 0.777]|
|w8/get100|6,265.5 → 5,449.1|0.870 [0.869, 0.870]|67.69 → 51.85|0.766 [0.766, 0.766]|
|w8/get1000|8,128.7 → 7,318.9|0.900 [0.899, 0.901]|67.70 → 51.91|0.767 [0.767, 0.767]|
|w8/range100|50,147.0 → 44,848.6|0.894 [0.894, 0.895]|74.50 → 60.03|0.809 [0.805, 0.815]|
|w8/set100|10,295.0 → 6,478.5|0.629 [0.628, 0.630]|100.83 → 55.90|0.555 [0.554, 0.555]|
|w64/grv|3,466.9 → 2,878.6|0.831 [0.830, 0.831]|34.33 → 27.13|0.790 [0.790, 0.790]|
|w64/get100|6,317.4 → 5,370.8|0.850 [0.850, 0.851]|66.52 → 51.30|0.771 [0.771, 0.771]|
|w64/get1000|8,169.6 → 7,249.7|0.887 [0.886, 0.888]|66.54 → 51.38|0.772 [0.772, 0.772]|
|w64/range100|50,067.5 → 44,704.5|0.901 [0.893, 0.918]|72.79 → 58.69|0.811 [0.806, 0.820]|
|w64/set100|10,491.4 → 6,418.9|0.612 [0.612, 0.612]|98.78 → 55.40|0.561 [0.561, 0.561]|

### Concurrent set100 absolutes

Medians of run-level p50/p99 and CPU/counters; these absolute columns are not ratios of pooled transactions. CGo’s small Go heap counts do **not** describe its native memory usage.

|Workers|Arm|p50 ms|p99 ms|E−A CPU µs/tx|Go bytes/tx|Go mallocs/tx|
|---|---|---|---|---|---|---|
|w8|previous Go|3.845|5.662|436.3|10,295.0|100.83|
|w8|new Go|4.431|6.441|236.3|6,478.5|55.90|
|w8|CGo|4.357|6.732|208.1|441.1|10.00|
|w64|previous Go|4.186|7.382|232.2|10,491.4|98.78|
|w64|new Go|5.384|8.123|134.7|6,418.9|55.40|
|w64|CGo|5.994|9.176|107.8|440.4|10.00|

### Current Go-versus-CGo comparisons

The previous/CGo column provides a same-campaign reference for the old Go client. The new/CGo contrasts are computed directly with CGo as denominator, not by dividing CIs or point estimates from other contrasts.

|Case|Previous/CGo E−A CPU|New/CGo E−A CPU|New/CGo p50|New/CGo p99|
|---|---|---|---|---|
|w1/grv|0.606 [0.581, 0.629]|0.783 [0.768, 0.797]|0.636 [0.616, 0.649]|0.713 [0.675, 0.762]|
|w1/get100|0.678 [0.666, 0.689]|0.806 [0.779, 0.833]|0.675 [0.661, 0.690]|0.797 [0.735, 0.902]|
|w1/get1000|0.674 [0.667, 0.683]|0.800 [0.783, 0.821]|0.673 [0.666, 0.680]|0.756 [0.718, 0.801]|
|w1/range100|0.807 [0.788, 0.826]|0.908 [0.893, 0.921]|0.697 [0.636, 0.738]|0.795 [0.715, 0.881]|
|w8/grv|0.891 [0.856, 0.924]|0.728 [0.715, 0.740]|1.025 [1.007, 1.045]|0.996 [0.865, 1.157]|
|w8/get100|1.361 [1.336, 1.384]|1.037 [1.027, 1.046]|0.977 [0.961, 0.992]|0.972 [0.866, 1.108]|
|w8/get1000|1.488 [1.445, 1.537]|1.133 [1.096, 1.182]|1.003 [0.986, 1.021]|0.924 [0.784, 1.083]|
|w8/range100|1.566 [1.541, 1.595]|1.354 [1.268, 1.513]|0.966 [0.949, 0.983]|1.083 [0.983, 1.207]|
|w8/set100|2.075 [2.033, 2.106]|1.148 [1.119, 1.179]|1.013 [0.978, 1.047]|0.936 [0.867, 1.000]|
|w64/grv|0.799 [0.776, 0.825]|0.565 [0.550, 0.581]|1.001 [0.970, 1.040]|0.948 [0.772, 1.131]|
|w64/get100|1.537 [1.471, 1.605]|1.135 [1.082, 1.203]|0.934 [0.911, 0.966]|0.773 [0.693, 0.853]|
|w64/get1000|1.558 [1.526, 1.600]|1.156 [1.111, 1.197]|0.770 [0.730, 0.821]|0.395 [0.336, 0.494]|
|w64/range100|1.081 [1.060, 1.106]|1.275 [1.061, 1.731]|0.867 [0.656, 1.480]|0.869 [0.665, 1.453]|
|w64/set100|2.132 [2.088, 2.183]|1.249 [1.213, 1.284]|0.893 [0.887, 0.899]|0.900 [0.835, 0.989]|

W64 range100 is particularly unstable: new/previous mean, p50, p99 and E−A intervals are wide. Existing per-order, first/second-half, moving-block and leave-one-block sensitivity outputs remain alongside every estimate. Do not flatten those diagnostics into a universal performance verdict. P99.9 is retained in the detailed outputs but is not a headline with only six run blocks.

## M2: current ordinary versus strict remote fetch

`f` is the record-row fraction whose storage-process/address teams are disjoint from index teams. **f=1 means cross-storage-process, not cross-machine.** With two replicas on three machines, teams necessarily share a machine; all P0 machine-disjoint fractions here are0. An integral partial placement is retained under its fraction and never pooled with cross-server cases.

P0 used the preregistered first-all-pass-else-attempt 3 rule and selected integral partial attempt 3, not the best attempt. DD stayed off for M2; before/after placement receipts matched for eligible cases. `split-rows1000-reverse` is a **partial f=.8200** result, not a strict cross-server result. DD was re-enabled after M2 and held healthy zero movement for121.474 s before M1, with stable roles and pins.

Ordinary/remote milliseconds are medians of each process’s three testing.B mean repeats. Remote/ordinary ratios use six paired blocks. Bytes/malloc ratios below are point estimates; full intervals and audit quantities are in the machine-readable report. `rows/op` and `prefix-B` are audit quantities, not improvements.

|Case|f|Ordinary ms|Remote ms|Remote/ordinary ns/op [95%]|Go bytes ratio|Go mallocs ratio|
|---|---|---|---|---|---|---|
|M2rl/small-limit1-forward|1.000|3.165|3.087|0.976 [0.967, 0.986]|0.977|1.080|
|M2rl/small-limit1-reverse|1.000|3.196|3.106|0.974 [0.970, 0.978]|0.976|1.090|
|M2rl/small-rows10-forward|1.000|3.654|3.718|1.018 [1.007, 1.029]|0.788|0.637|
|M2rl/small-rows10-reverse|1.000|3.737|3.702|0.995 [0.984, 1.007]|0.783|0.636|
|M2rl/small-rows1000-forward|1.000|102.749|123.646|1.213 [1.171, 1.260]|0.565|0.481|
|M2rl/small-rows1000-reverse|1.000|101.429|122.551|1.213 [1.184, 1.242]|0.559|0.481|
|M2rl/split-limit1-forward|1.000|4.428|4.448|1.009 [0.992, 1.030]|0.716|0.986|
|M2rl/split-rows10-reverse|1.000|7.983|10.625|1.326 [1.292, 1.365]|0.698|0.547|
|M2rl/split-rows1000-reverse|0.820|359.495|831.216|2.304 [2.247, 2.360]|0.693|0.454|
|M2skip/skip3-rows10-reverse|1.000|4.498|5.259|1.169 [1.151, 1.185]|0.899|0.841|
|M2skip/skip3-rows1000-forward|1.000|103.323|126.712|1.220 [1.198, 1.243]|0.634|0.583|
|M2skip/skip3-rows1000-reverse|1.000|109.655|126.707|1.152 [1.110, 1.195]|0.629|0.583|

In the current cohort, remote small rows1000 is ~21% slower in both directions despite roughly44% fewer Go bytes and52% fewer Go allocations. Reverse Skip10 is ~17% slower remotely; Skip1000 is ~15–22% slower. These are within-cohort conclusions for the displayed cases and placement—not claims that remote fetch can never help elsewhere.

## Comparison with the 14 October campaign — descriptive only

The prior final Go SHA is exactly the current previous Go SHA267a4143. The table below compares the **same Go revision versus its campaign’s CGo arm**, so cohort variation is visible before discussing new code. Blocks are never pooled across campaigns; no cross-campaign CI, ratio-of-ratios or significance test is computed. The current direct new/previous contrasts above are the reported production comparisons, not explanations of a mechanism.

### Same historical Go revision: prior final/CGo versus current previous/CGo

|Case|Prior final/CGo p99|Current previous/CGo p99|Prior final/CGo E−A CPU|Current previous/CGo E−A CPU|
|---|---|---|---|---|
|w1/grv|0.498 [0.408, 0.601]|0.665 [0.572, 0.759]|0.570 [0.551, 0.588]|0.606 [0.581, 0.629]|
|w1/get100|0.865 [0.734, 1.037]|0.720 [0.633, 0.827]|0.660 [0.648, 0.671]|0.678 [0.666, 0.689]|
|w1/get1000|0.705 [0.654, 0.779]|0.674 [0.596, 0.769]|0.629 [0.603, 0.653]|0.674 [0.667, 0.683]|
|w1/range100|0.715 [0.595, 0.861]|0.719 [0.634, 0.817]|0.747 [0.731, 0.761]|0.807 [0.788, 0.826]|
|w8/grv|1.101 [0.869, 1.457]|1.054 [0.896, 1.236]|0.904 [0.878, 0.931]|0.891 [0.856, 0.924]|
|w8/get100|0.815 [0.741, 0.882]|0.911 [0.853, 0.993]|1.291 [1.264, 1.331]|1.361 [1.336, 1.384]|
|w8/get1000|0.931 [0.859, 0.993]|0.796 [0.687, 0.926]|1.362 [1.342, 1.382]|1.488 [1.445, 1.537]|
|w8/range100|0.925 [0.861, 0.987]|1.025 [0.975, 1.076]|1.544 [1.476, 1.651]|1.566 [1.541, 1.595]|
|w8/set100|0.916 [0.860, 0.976]|0.844 [0.803, 0.879]|2.142 [2.123, 2.160]|2.075 [2.033, 2.106]|
|w64/grv|0.960 [0.757, 1.190]|1.028 [0.893, 1.186]|0.736 [0.721, 0.752]|0.799 [0.776, 0.825]|
|w64/get100|0.679 [0.601, 0.776]|0.688 [0.614, 0.770]|1.429 [1.385, 1.478]|1.537 [1.471, 1.605]|
|w64/get1000|0.385 [0.336, 0.438]|0.367 [0.308, 0.446]|1.608 [1.509, 1.705]|1.558 [1.526, 1.600]|
|w64/range100|0.732 [0.506, 0.912]|0.767 [0.745, 0.790]|1.069 [0.923, 1.180]|1.081 [1.060, 1.106]|
|w64/set100|0.872 [0.815, 0.927]|0.810 [0.784, 0.836]|1.981 [1.935, 2.021]|2.132 [2.088, 2.183]|

For set100, prior final/CGo E−A was2.142 at w8 and1.981 at w64; the same Go revision now gives2.075 and2.132. Current new/CGo gives1.148 and1.249, respectively. This supports the direction of a smaller Go-versus-CGo CPU gap as a description; the reported numerical comparison remains the directly paired current new/previous result, not a comparison of those cohort ratios.

### M2 historical remote/ordinary ratios and placement

#### Known cohort differences and matched properties

The reverse-Skip10 contrast is **descriptive, not a paired old/new M2 A/B**. The known differences and matched properties are:

| Dimension | Prior cohort | Current cohort / reporting limit |
|---|---|---|
| Production and arms | M2 ordinary/strict-remote both use 267a4143; M1 has Go/CGo, two arms and 12 processes/case | M2 methods both use fa8725f1; M1 has previous/new/CGo, three arms/all six permutations and 18 processes/case. No concurrent old/new M2 arm was measured |
| Placement | Selected P0 attempt 3; split rows10 f=0, split rows1000 f=.696/seven record shards; small and planskip f=1 | Selected P0 attempt 3; split rows10 f=1, split rows1000 f=.820/six record shards; small and planskip f=1. Replica teams and shard boundaries differ in all three stores, even where the fraction matches |
| Harness | Frozen d94b4c613 client harness | Declared cleanup-chain compatibility patch for M1, applied identically to all three client arms. M2 workload overlay archives are unchanged; that client-only patch does not explain an M2 mechanism |
| Logical/physical data layout | Record Layer 1000-record stores, VALUE price_idx, no record versions; small unsplit one chunk; split 100,000-byte padding/two chunks; fetch quantity=id%7+1; planskip quantity=1 | Same frozen fixture schema, canonical key schema and store prefixes; fetch prefix length 19 bytes/planskip 22 bytes. Freshly seeded data and different physical shard/replica layouts—not a matched historical database |
| M2 schedule and runtime | Six blocks, two methods, three repeats/process, 3 s; GOMAXPROCS=8, CPUs 8–15 | Same actual sealed 12-case M2rl/four-case M2skip geometry and method orders; the production revision changes. testing.B calibration counts are not fixed transaction populations |
| Machines and network | Dedicated cloud hosts/private VPC, fixed declared topology | Fresh host cohort and measured network conditions, not the same physical instances or a controlled cross-campaign load experiment |
| Eligibility / operations | All 16 M2 cases clean; old M1 worker sequence and two-arm schedules | Twelve M2 cases clean; four CPU-gate exclusions. New M1 sequence/three-arm schedules and w8-only operator polling deviation; no excluded partial results contribute estimates |

Offline canonical data-key censuses match across cohorts: 2000 small, 3000 split and 2000 planskip keys. That establishes matching key shape, **not independently verified serialized-value/store-header byte identity**. All three stores have one index shard; small/planskip have one record shard in each cohort. The untimed P0 split rows100 cell also changed from f=.25 to f=1 and contributes no performance estimate. For reverse Skip10, the prior index and record teams both occupied the node1/node2 machine pair; current index and record teams span node1/node3 and node2/node3 respectively. Thus even f=1 in both cohorts is not a matched physical placement.

No causal mechanism is identified here for the latency, CPU or M2 ratio differences. At reporting time the same arms are being profiled locally, and a local old-versus-new M2 A/B is planned. No result from that profiling or planned A/B is claimed in this report.

#### Historical/current ratios

|Case|Prior f → current f|Prior remote/ordinary ns/op [95%]|Current remote/ordinary ns/op [95%]|
|---|---|---|---|
|M2rl/small-limit1-forward|1.000 → 1.000|0.979 [0.964, 0.995]|0.976 [0.967, 0.986]|
|M2rl/small-limit1-reverse|1.000 → 1.000|0.985 [0.969, 1.003]|0.974 [0.970, 0.978]|
|M2rl/small-rows10-forward|1.000 → 1.000|1.031 [1.018, 1.042]|1.018 [1.007, 1.029]|
|M2rl/small-rows10-reverse|1.000 → 1.000|1.026 [1.013, 1.039]|0.995 [0.984, 1.007]|
|M2rl/small-rows1000-forward|1.000 → 1.000|1.226 [1.175, 1.280]|1.213 [1.171, 1.260]|
|M2rl/small-rows1000-reverse|1.000 → 1.000|1.255 [1.217, 1.293]|1.213 [1.184, 1.242]|
|M2rl/split-limit1-forward|1.000 → 1.000|1.110 [1.087, 1.131]|1.009 [0.992, 1.030]|
|M2rl/split-rows10-reverse|0.000 → 1.000|0.854 [0.823, 0.886]|1.326 [1.292, 1.365]|
|M2rl/split-rows1000-reverse|0.696 → 0.820|2.300 [2.206, 2.384]|2.304 [2.247, 2.360]|
|M2skip/skip3-rows10-reverse|1.000 → 1.000|0.158 [0.155, 0.160]|1.169 [1.151, 1.185]|
|M2skip/skip3-rows1000-forward|1.000 → 1.000|1.360 [1.295, 1.414]|1.220 [1.198, 1.243]|
|M2skip/skip3-rows1000-reverse|1.000 → 1.000|1.324 [1.250, 1.407]|1.152 [1.110, 1.195]|

Two historical placement changes are decisive confounds: `split-rows10-reverse` changed from f=0 to f=1; `split-rows1000-reverse` changed from f=.696 to f=.820. Their old/new ratios must not be read as a code-regression test. For reverse Skip10, both labels are cross-server but the cohorts still differ: prior ordinary/remote absolutes were **31.127ms/4.873ms**, versus **4.498ms/5.259ms** now. The dramatic old ratio therefore does not recur; this table does not establish which production/environment difference caused that change.

## Operational findings and deviations

- **Pre-arm bursts:** the five CPU-gate failures all proxy-overlap a stored 10 s controller SSH/systemctl poll (0.69–0.96 s). Busy CPUs were 8/11/14/11/10, user+system time, steal 0; other groups passed. Nearby snapshots prove replacement `systemd --user`/`(sd-pam)` PIDs, sometimes on benchmark CPUs. Failed gates precede process snapshots, and quiescence records lack interval timestamps: SSH/PAM churn is the best-supported hypothesis, **not PID-proven attribution**. Pull/collection happens afterward and cannot explain these windows.
- **Boundary-only operator change:** The owner authorized local ControlMaster/ControlPersist and60 s polling, activated **13:13:19.616081Z only after w64 quiescence and full collection/R4**, before w8. No host configuration, seal, workload command, timing, pacing, gate, R3/R4, reserve or deadline changed. W8 had **zero CPU-gate omissions after the switch**—supporting evidence for the attribution, **not proof**; worker level/sequence changed and connection reuse does not guarantee elimination of per-channel PAM work. W64 was never changed mid-phase.
- **Qualification evidence stays separate:** capacity and R1/R2/R3/R4 diagnostics never enter ratios. M2 diagnostic1 exposed a 1-repeat diagnostic plan inconsistent with the frozen parser’s 3-pair requirement; fresh diagnostic2 correctly refused a reused placement work directory before any process. Root-approved diagnostic3 used 3 repeats/same 1× and an output-specific work namespace, passed R4-clean1/1, and left final 6-block/3-repeat/3 s budgets unchanged. Both failures remain retained. Setup corrections and P0 continuation evidence remain in the retained raw archive, bound by SHA256 below.
- **No A1:** all adverse and unreached cases remain as preregistered consequences. No outcome-driven re-attempt, sample reduction, gate relaxation or capacity-data substitution occurred.

## Collection, teardown and verification

Measurements ended 13:27Z. Final forensic collection passed on all four roles in 19.6 s, with 24,531 client artifacts and node artifact counts 215/201/201; captured trace counts were 2/28/16/16. Archives are hash-verified without extraction; live traces are explicitly prefix snapshots. Database storage files, SSH private keys and cloud metadata are excluded. Retained local binaries/source archives are referenced only after exact hash matching.

Campaign-only teardown removed four servers and the owned network, firewall and SSH public-key record. **Read-only exact inventory verification passed across all 10 kinds: the original 17 protected resources remain, with no missing/unexpected resource.** The four implicit campaign primary IPs auto-deleted with their servers; none was directly deleted. The deadline guard was cancelled after verification, and no live guard process remained. Operational completion was about four hours before 17:30Z; the 17:55:06Z provider deadline was never extended. Resource identifiers, addresses and private receipts are deliberately omitted from this public results package; raw evidence is retained by SHA256.

Offline seeded analysis then ran at **nice 15**, pinned to **0,1,6,7,11,12,13,18,19,23**, avoiding the owner’s profiling and local FDB CPUs. It completed in 4m36s. It has no timing-sensitive benchmark steps. All code/protocol/build/runtime/controller/R4/raw hashes and the analysis execution envelope are retained. Independent review confirmed all 26 eligible populations, direct contrasts and historical joins, without rerunning estimators.

## Post-hoc throughput and client cores busy

**POST-HOC, EXPLORATORY — throughput was not preregistered.** This addendum derives new metrics and bootstrap intervals from the retained raw data; it does not change any preregistered estimate, eligibility decision or omission. It describes achieved rate at the existing fixed-worker operating points, **not saturation, peak sustainable throughput or worker scaling**.

For each eligible M1 process, `throughput_ops_per_second = summary.completed × 1e9 / summary.measured_wall_ns`. `completed` equals `requested` and the number of successful measured `samples.csv` rows; retries are counted separately, not as extra operations. One operation is one logical workload transaction, not one key or RPC. The monotonic wall window starts when all ready workers are released and ends when the last worker finishes its quota. It excludes fixture/setup, warmup, forced drains, validation, CSV output and shutdown; worker scheduling, loop bookkeeping and the finite-run finishing tail remain inside it.

`cpu_util_cores = 1000 × [Δprocess_user_cpu_us + Δprocess_system_cpu_us] / measured_wall_ns`, using `summary.stats_before` and `summary.stats_after`. This is **measured-phase bracketing process CPU / measured wall**, in average logical-CPU equivalents (1.0 = one client core busy), including Go and native threads but not FDB servers. The CPU reads occur just before/after the wall fences; their extra bracket duration is not recorded. This is **not lifecycle E−A CPU**: warmup cleanup can enter the window, and deferred measured-operation cleanup can fall after it. Do not read these cores as complete-lifecycle CPU or normalize them by the worker count.

**Closed-loop cross-check:** all 252 process records validate complete sample indices, worker assignment, nonoverlapping per-worker operation intervals, operations ending within the wall window, and integer latency sums. With `Q = measured ops/s`, `W = workers` and `L = mean measured latency`, `Q / (W × 1e9 / L)` ranges **0.972631–0.999928** (median **0.998339**). Actual rate is thus at most **2.737% below** the full-occupancy cross-check; it never exceeds the worker-time budget. Bookkeeping and early-finishing workers prevent exact equality. This is accounting consistency, not an independent server-counter validation.

Below, absolute rates and cores are **medians of six process values per arm**. Ratios are **geometric means of six within-block throughput ratios**, with the frozen M1 `bootstrap_log_ratio` API, seed **492817**, **20,000** whole-paired-block resamples and nominal 95% percentile intervals. All three arms stay aligned by block; the same seeded draws are used for the three contrasts. Ratios above one mean **more operations/s**. They are not ratios of the displayed absolute medians; the wide w64/range100 interval remains unstable. There is no multiplicity adjustment, equivalence claim or cross-campaign pooling; intervals do not establish block independence on the continuously running server.

<!-- throughput-table:start -->
|Case|Ops/s previous / new / CGo|New/previous [95%]|New/CGo [95%]|Previous/CGo [95%]|Busy cores previous / new / CGo|
|---|---:|---|---|---|---:|
|w1/grv|989 / 927 / 607|0.956 [0.929, 0.998]|1.553 [1.523, 1.606]|1.624 [1.600, 1.649]|0.246 / 0.308 / 0.255|
|w1/get100|712 / 653 / 442|0.914 [0.883, 0.942]|1.468 [1.431, 1.507]|1.607 [1.572, 1.638]|0.351 / 0.387 / 0.328|
|w1/get1000|699 / 650 / 437|0.919 [0.907, 0.934]|1.478 [1.465, 1.492]|1.607 [1.584, 1.637]|0.353 / 0.397 / 0.338|
|w1/range100|594 / 552 / 403|0.929 [0.917, 0.942]|1.412 [1.341, 1.527]|1.521 [1.452, 1.647]|0.415 / 0.442 / 0.353|
|w8/grv|4,589 / 4,511 / 4,598|0.984 [0.970, 0.996]|0.979 [0.959, 0.998]|0.995 [0.973, 1.011]|0.477 / 0.404 / 0.563|
|w8/get100|3,784 / 3,397 / 3,306|0.903 [0.885, 0.920]|1.027 [1.002, 1.049]|1.137 [1.128, 1.146]|0.894 / 0.642 / 0.601|
|w8/get1000|3,760 / 3,283 / 3,194|0.875 [0.867, 0.883]|1.012 [0.985, 1.036]|1.157 [1.124, 1.186]|0.945 / 0.653 / 0.559|
|w8/range100|2,975 / 2,692 / 2,599|0.897 [0.881, 0.911]|1.031 [1.010, 1.053]|1.149 [1.140, 1.161]|1.358 / 1.031 / 0.778|
|w8/set100|2,110 / 1,783 / 1,814|0.854 [0.828, 0.882]|0.990 [0.956, 1.027]|1.159 [1.126, 1.183]|0.793 / 0.393 / 0.332|
|w64/grv|30,937 / 32,673 / 31,850|1.057 [1.036, 1.080]|1.028 [0.979, 1.081]|0.973 [0.928, 1.008]|1.494 / 1.228 / 2.135|
|w64/get100|27,430 / 21,479 / 18,613|0.769 [0.745, 0.792]|1.140 [1.097, 1.179]|1.481 [1.439, 1.532]|3.590 / 2.147 / 1.580|
|w64/get1000|25,634 / 20,749 / 15,074|0.798 [0.779, 0.819]|1.368 [1.294, 1.436]|1.714 [1.620, 1.802]|3.531 / 2.251 / 1.378|
|w64/range100|18,919 / 17,509 / 11,776|0.718 [0.440, 0.925]|1.158 [0.691, 1.514]|1.613 [1.569, 1.650]|3.742 / 3.726 / 2.296|
|w64/set100|14,688 / 11,654 / 10,457|0.796 [0.787, 0.804]|1.113 [1.104, 1.121]|1.399 [1.385, 1.413]|2.911 / 1.409 / 0.987|
<!-- throughput-table:end -->

At w64/set100, new Go achieves **11,654 median ops/s** versus previous Go's **14,688**, while measured-phase busy cores fall from **2.911 to 1.409**. The directly paired throughput ratio is **0.796 [0.787, 0.804]**. Lower CPU/op is not, by itself, higher achieved throughput; lower rate alongside fewer busy client cores also does not identify a waiting mechanism or its cause. Relative to CGo at this operating point, new Go's throughput ratio is **1.113 [1.104, 1.121]**, with median CGo throughput **10,457 ops/s** and **0.987** busy cores.

[`analysis/M1-throughput.csv`](analysis/M1-throughput.csv) contains **252 process**, **42 arm-absolute**, **42 paired-comparison** and **14 whole-case omission** records across all 28 planned M1 cells. Every eligible case retains all six blocks/18 processes; no excluded write arm is salvaged. It includes raw measured counts/wall/CPU, integer latency sums, retry counts and per-process closed-loop factors. Every row is marked post-hoc and not preregistered.

### M2: derived timed rates, with a different CPU scope

[`analysis/M2-throughput.csv`](analysis/M2-throughput.csv) adds the 12 eligible M2 cells: **144 process**, **24 arm-absolute**, **24 paired-comparison** and **4 whole-case omission** records. Each timed repeat's rate is `1e9 / testing.B ns/op` (scan/plan executions per second, **not rows/s**). Three repeat rates are reduced to a median per process, then paired over six blocks with the frozen `bench_analyze.paired_gm` API, seed 492817 and 20,000 resamples. The six-process absolute median is computed from the process rates, **not** by inverting the published even-count latency median. This is a derived timed-benchmark rate, not measured `N / 3s`, whole-process throughput or an old/new production comparison.

M2 has **no retained measured-window CPU counters**, so `cpu_util_cores` is intentionally empty. Its separate `lifecycle_cpu_util_cores = (process_user_cpu_seconds + process_system_cpu_seconds) / (process_wall_ns / 1e9)` uses GNU time CPU and runner wall receipts, including fixtures, calibration, audits and shutdown. Those lifecycle values and their separately labelled paired intervals must not be read as CPU during the timed throughput window. Three repeats remain clustered within each process; 432 repeat samples are not 432 independent replicates.

### Reproduce this post-hoc addendum

The [throughput exporter](../../../tools/benchcampaign/campaigns/fdbmn-20261020/public-export/export_throughput.py) reads only retained inputs and invokes the frozen numerical helpers; it does not launch benchmarks, SSH or cloud operations. [`analysis/throughput-inputs.sha256`](analysis/throughput-inputs.sha256) pins **826 files**, with paths relative to the **campaign directory**. All 810 raw inputs were also checked against the frozen per-phase analysis provenance. The original eight aggregate files and their pins remain unchanged.

For this command, set `FDBMN_CAMPAIGN` to the retained `fdbmn-20261020` directory itself, not its parent archive directory. (`--campaign` also defaults to `FDB_PERF_HOME` when that variable denotes the campaign directory.) Run from the repository root:

```bash
: "${FDBMN_CAMPAIGN:?set to the retained fdbmn-20261020 campaign directory}"
parent=$(mktemp -d)
outputs="$PWD/docs/benchmarks/fdbmn-20261020/analysis/throughput-outputs.sha256"
nice -n 15 taskset -c 0,1,6,7,11,12,13,18,19,23 \
  python3 -B tools/benchcampaign/campaigns/fdbmn-20261020/public-export/export_throughput.py \
  --campaign "$FDBMN_CAMPAIGN" \
  --inputs-manifest docs/benchmarks/fdbmn-20261020/analysis/throughput-inputs.sha256 \
  --output-dir "$parent/export" > "$parent/table.md"
(cd "$parent/export" && sha256sum -c "$outputs")
```

The exporter refuses an existing output directory and checks the pinned private redaction file before publication. [`analysis/throughput-method.json`](analysis/throughput-method.json) records formulas, scopes, populations and numerical-helper hashes; [`analysis/throughput-outputs.sha256`](analysis/throughput-outputs.sha256) binds the three new aggregate files. The table printed to stdout is the generated table above.

## Public aggregate evidence and retained raw data

- `analysis/coverage.json`: all 44 planned cases; 37 cases in executed phases, 26 eligible, 7 contaminated, 4 unverifiable, 7 budget-unexecuted; A1=0.
- `analysis/M1-comparisons.csv`: all three direct contrasts × 12 metrics for each of 14 clean cases, including stored order/dependence diagnostics. Timing/CPU rows legitimately have no `status`; eligibility comes from the clean enclosing case.
- `analysis/M1-absolutes.csv`: existing analyzer absolute aggregates, 42 arm/case rows.
- `analysis/M1-historical.csv`: side-by-side stored historical/current contrasts; descriptive only.
- `analysis/M2-comparisons.csv`, `M2-absolutes.csv`, `M2-historical.csv`: stored mean-latency/allocation estimates and audit quantities, with placement fractions. Audit metrics are explicitly marked.
- `analysis/method.json`: identities, scopes, six-block seed/resample settings and source hashes.
- `provenance/M2rl-omission-causes.json`: sanitized CPU-gate causes; original source hash retained.
- `provenance/analysis-execution-envelope.json`: sanitized nice/CPU-set/seed authorization envelope; original source hash retained.
- `raw-artifacts.sha256`: private raw protocol/plans, attempts, telemetry, R4 inputs/reports, forensic/source archives, operational-deviation and restoration evidence. The raw transaction CSVs, process snapshots, host traces, resource inventory and private paths are **not** committed. Hashes identify retained evidence without publishing that content.
- Previous public report: [14 October multi-node campaign](../hcloud-multinode-vpc-20261014/README.md). Protocol/execution tools for the current campaign are maintained separately; this change adds results and the offline aggregate exporter.

## Regenerate

There are two separate operations: **numerical regeneration from retained raw data**, and **byte-exact public export from the canonical retained analysis**. The amendment adding this section performed only the latter. It did not rerun the adapter, bootstrap or benchmarks. The narrative in this report was editorially refined; the exporter generates exactly the six analysis CSVs plus `coverage.json` and `method.json`, not the narrative, operational receipts or manifests.

### Retained inputs and layout

Set `FDB_PERF_HOME` to the private archive root, and run the commands below from the repository root. No private archive content or resource-redaction values are published here.

```text
${FDB_PERF_HOME}/
  campaigns/fdbmn-20261020/
    deploy/protocol.json
    execution/analyze_campaign.py
    frozen/cleanup-compat/client-harness.tar.gz
    frozen/tools/clientbench/scripts/{analyze,run}.py
    tooling/tools/benchcampaign/{singlenode,multinode}/...
    provenance/{source-receipt.json,f0-*/runtime.sha256,public-export-redactions.json}
    sealed/provenance/source-receipt.json
    build-check/cleanup-compat-builds.json
    results-raw/{M2rl,M2skip,M1w1,M1w64,M1w8}.tar.gz
    results-raw/{M2rl,M2skip,M1w1,M1w64,M1w8}/...
    phase-controller/<phase>/{complete,inputs,manifest,client-pins-before-r4}.json
    reconcile/<phase>/{input,report}.json
    analysis/measured-492817/...
  hcloud-multinode-vpc-20261014/
    provenance/source-receipt.json
    analysis/<phase>/{summary,report}.json
    reconcile/<phase>/report.json
    plans/testingb/{M2rl,M2skip}.plan.json
```

Each phase archive contains the contents of its corresponding `results-raw/<phase>/` directory (archive members start at `./`, not at a phase-name directory). Restore into that phase directory if working from archives; retain the controller/R4/source/build/runtime inputs at the paths above. The frozen client scripts must match their members in the compatibility archive. The adapter checks raw-tree stability, source/build/runtime identity, sealed plans and complete R4 populations; it never launches a benchmark or remote/cloud operation.

The following SHA256 manifests identify different layers of the retained evidence:

| Manifest | Population and path base |
|---|---|
| [`raw-artifacts.sha256`](raw-artifacts.sha256) | 61 retained evidence files; paths relative to `${FDB_PERF_HOME}/campaigns/fdbmn-20261020/` |
| [`analysis/adapter-inputs.sha256`](analysis/adapter-inputs.sha256) | 57 current-campaign raw archives, source/helper code, protocol, controller, R4 and identity inputs; paths relative to `${FDB_PERF_HOME}/` |
| [`analysis/historical-inputs.sha256`](analysis/historical-inputs.sha256) | 13 prior-cohort source, analysis, R4 and M2 plan inputs needed for historical joins; paths relative to `${FDB_PERF_HOME}/` |
| [`analysis/export-inputs.sha256`](analysis/export-inputs.sha256) | 23 canonical retained direct export inputs, including all 14 M1 absolute CSVs, five phase summaries/reports, overall summary, source receipt, adapter source and private redactions; paths relative to `${FDB_PERF_HOME}/` |
| [`analysis/export-outputs.sha256`](analysis/export-outputs.sha256) | Eight expected public aggregate files; paths relative to this results directory or a fresh export directory |

Verify the retained input layers without changing them:

```bash
: "${FDB_PERF_HOME:?set the retained private archive root}"
repo="$PWD"
doc="$repo/docs/benchmarks/fdbmn-20261020"
C="$FDB_PERF_HOME/campaigns/fdbmn-20261020"
(
  cd "$FDB_PERF_HOME"
  sha256sum -c "$doc/analysis/adapter-inputs.sha256"
  sha256sum -c "$doc/analysis/historical-inputs.sha256"
  sha256sum -c "$doc/analysis/export-inputs.sha256"
)
(cd "$C" && sha256sum -c "$doc/raw-artifacts.sha256")
```

### Optional numerical regeneration

The frozen private adapter is `campaigns/fdbmn-20261020/execution/analyze_campaign.py`, SHA256 **`700fd3c18a319ea6738c00652846313797a4f4bfbcf50e4b3aee37e720a70289`**. It uses the retained frozen analyzers and numerical APIs, seed 492817 and 20,000 whole-block resamples. Historical comparisons require all 13 pinned prior files; they are not inferred from this report's rounded tables.

The measured run used `--output "$C/analysis/measured-492817"`. That directory is retained and must not be overwritten. To regenerate numerics separately, choose a fresh directory below `"$C/analysis/"`, for example:

```bash
analysis_out="$C/analysis/regenerated-492817"  # must not already exist
nice -n 15 taskset -c 0,1,6,7,11,12,13,18,19,23 \
  python3 -B "$C/execution/analyze_campaign.py" \
  --campaign "$C" \
  --protocol "$C/deploy/protocol.json" \
  --prior "$FDB_PERF_HOME/hcloud-multinode-vpc-20261014" \
  --output "$analysis_out" \
  --phase M2rl --phase M2skip --phase M1w1 --phase M1w64 --phase M1w8 \
  --phase M1w32 --omitted-phase 'M1w32=not executed: budget under the 11:15 rule'
```

**This command was not run for the exporter amendment.** Do not replace the canonical export inputs or their pins with a new adapter run. Adapter provenance embeds absolute input paths, and `method.json` binds the canonical raw `summary.json` SHA256. Relocation or another output directory may therefore change provenance bytes and `raw_summary_sha256` even if numerical estimates agree. The eight-file byte-identity proof below applies to the canonical retained inputs, not automatically to arbitrary adapter reruns.

### Byte-exact public aggregate export

The in-tree exporter is [`export_public_results.py`](../../../tools/benchcampaign/campaigns/fdbmn-20261020/public-export/export_public_results.py), SHA256 **`71363367505faf8875ce3f4b29ae41c539945acbef21da1058ec6e3f36e9423d`**. It parameterizes the original private exporter's I/O/redaction handling, verifies every manifest input before parsing, rejects unlisted or escaping inputs, validates buffered public outputs, and publishes only to a fresh directory. It contains no estimator or subprocess execution.

The private redaction input is `campaigns/fdbmn-20261020/provenance/public-export-redactions.json`, SHA256 **`1c2b60b2a46941d81ba765a5b3a00a312a286a7536f3222162e9d736ada1eaa8`**. It supplies the protected/campaign identifier and resource-name lists; its contents must remain private.

```bash
export FDB_PERF_HOME
export_parent=$(mktemp -d)
out="$export_parent/aggregates"  # deliberately not created
nice -n 15 taskset -c 0,1,6,7,11,12,13,18,19,23 \
  python3 -B tools/benchcampaign/campaigns/fdbmn-20261020/public-export/export_public_results.py \
  --input-dir "$C" \
  --output-dir "$out" \
  --inputs-manifest "$doc/analysis/export-inputs.sha256" \
  --redactions "$C/provenance/public-export-redactions.json" \
  --aggregate-only
(cd "$out" && sha256sum -c "$doc/analysis/export-outputs.sha256")
while read -r digest name; do
  cmp -- "$out/$name" "$doc/$name"
done < "$doc/analysis/export-outputs.sha256"
```

**Verified:** two fresh exporter-only runs checked all 23 input pins, produced exactly eight files, and matched one another and the existing public aggregates byte-for-byte. No analysis or estimator ran. [`provenance/exporter-reproduction.json`](provenance/exporter-reproduction.json) records the input/exporter/proof hashes, output hashes and normalized aggregate/serialization AST checks. The original exporter SHA256 is **`3a06fa2ae9c3df8e03a1c47ff6231e316eca9f5d5c8f796748a897cae53dc93f`**. The [`non-applying audit diff`](provenance/exporter-io-redaction.diff.txt) records the I/O/redaction parameterization and separation of narrative/static-evidence writing; removed private literals are masked. The exact original and unmasked diff remain privately retained by the receipt's hashes.

Production and benchmark code are unchanged by this results/offline-export change. Benchmark checks are not a full repository-suite claim or correctness/merge authorization.
