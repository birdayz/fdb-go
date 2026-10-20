# Candidate002 vs CGo — existing matched campaign, no new measurements

Population: six counterbalanced paired blocks per workload,100 measured transactions/arm/block and40 warmups;600 measured transactions per arm per workload. One operation is **one committed transaction containing50 distinct Sets**, not one Set. All observations passed byte/persistence checks, no measured or warmup errors/retries. Go1.26.9, native/server7.3.77API730,workers1,GOMAXPROCS8; immutableV6harness.

## Absolute timings

Each entry is the **median of six run-level statistics**: run mean latency, run p99 latency, or measured process CPU per operation. These are not pooled percentiles. Units: microseconds per50-Set transaction. Do not obtain the paired ratios below by dividing these medians.

|Workload|Arm|Mean latency µs/op|p99 latency µs/op|Process CPU µs/op|
|---|---|---:|---:|---:|
|50×100B|CGo native|2490.816|3777.209|836.420|
|50×100B|Go WIP baseline|2577.659|4062.662|1186.415|
|50×100B|Go replay002|2554.772|4311.325|1027.870|
|50×1000B|CGo native|3177.265|5627.457|1163.755|
|50×1000B|Go WIP baseline|3083.791|4959.950|1337.555|
|50×1000B|Go replay002|3045.221|4279.610|1273.075|

## Go vs native paired ratios

Geometric mean of the **six within-block Go/CGo run-level ratios**. Lower is better. Adaptive/exploratory six-block comparison, not confirmatory or a noninferiority/capacity/global-speed claim.

|Workload|Go arm / CGo|Mean ratio|p99 ratio|CPU/op ratio|
|---|---|---:|---:|---:|
|50×100B|WIP baseline / CGo|1.027464|0.989904|1.407489|
|50×100B|replay002 / CGo|1.017579|1.028369|1.208517|
|50×1000B|WIP baseline / CGo|0.991106|0.992049|1.145319|
|50×1000B|replay002 / CGo|0.999159|0.906073|1.099630|

For attribution, replay002/WIPbaseline paired ratios are mean0.990379/1.008125, p991.038857/0.913335, CPU0.858633/0.960108 for100B/1000B respectively. CPU improved against the Go baseline, but native still used less CPU in all six paired blocks for each workload.

Replay002/CGo pair ranges: small mean0.940431–1.225816, p990.756373–1.385245, CPU1.066039–1.385480;large mean0.907853–1.168103,p990.558201–1.523621,CPU1.048262–1.201250. p99 estimates are noisy at100 observations/run.

**Native allocation is unmeasured.** CGo's Go-runtime MemStats excludes the C++ heap; no Go-vs-native total-allocation comparison is valid. Native internal counters are unobserved, not zero. Go-only replay002/WIP baseline allocated-byte ratios0.756208/0.788678 are attribution within the Go implementations, not native-heap comparisons.

## Coverage limitation and reporting rule

Existing001/002 broader-control campaigns had only two Go arms. **CGo was not measured in those controls**; no native control number or ratio can be inferred or substituted from another campaign. Future campaigns and controls must include CGo, split by workload if necessary to stay within1–3min budgets, per root#426/userrequirement.

Source of this table: existing002 `analysis.json` per_run entries (derived from rawCSV/summary), and `counter-resource-validation.json` paired comparisons. Formula for absolute statistic: median(run_stat/1000) over six runs. Formula for comparison: exp(mean(log(Go_run_stat/CGo_run_stat))) pairing identical workload/block. Source/binary identities are in002REPORT.md and preserved campaign manifests. Delta correctness review/root tests and known WIP baseline correctness issues remain separate acceptance gates.
