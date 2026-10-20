# R7: PR2 large-value follow-up

All 18 blocks: R6 + R7a + R7b; 54 runs, 2,000 measured + 200 warmup per run. Same exact R6 binaries; 50 distinct 1,000-byte Sets + commit/operation, one worker, no profiles/builds. Each campaign covers all six three-arm orders, with fresh equal-length prefixes.

## Absolute medians of 18 run-level statistics (µs/transaction)

|Arm|p99|p99.9|p90|p50|CPU|Mean|User CPU|System CPU|
|---|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|4420.725|6453.070|2984.209|2330.809|1109.987|2458.211|508.752|584.093|
|old|4000.602|6408.801|2701.776|2228.954|1141.371|2295.126|538.197|598.048|
|new|4156.535|6198.587|2578.047|2175.559|1073.440|2271.568|491.606|580.057|

## Paired geometric-mean ratios and 95% block-bootstrap CIs

|Ratio|Metric|GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|p99_ns|1.007330|[0.945971, 1.071614]|8/18|
|new/old|p999_ns|0.963198|[0.872894, 1.049706]|10/18|
|new/old|p90_ns|0.970607|[0.887742, 1.061718]|12/18|
|new/old|p50_ns|0.995857|[0.955944, 1.043488]|13/18|
|new/old|process_cpu_ns_per_op|0.945445|[0.932449, 0.957267]|18/18|
|new/old|mean_ns|0.997556|[0.952829, 1.045151]|12/18|
|new/old|user_cpu_ns_per_op|0.917987|[0.899274, 0.936732]|18/18|
|new/old|system_cpu_ns_per_op|0.970539|[0.948681, 0.991949]|13/18|
|new/cgo|p99_ns|0.932831|[0.878479, 0.987032]|12/18|
|new/cgo|p999_ns|0.900345|[0.825579, 0.975995]|10/18|
|new/cgo|p90_ns|0.923783|[0.861402, 0.990569]|14/18|
|new/cgo|p50_ns|0.935601|[0.906281, 0.967078]|15/18|
|new/cgo|process_cpu_ns_per_op|0.988070|[0.955155, 1.024013]|11/18|
|new/cgo|mean_ns|0.924516|[0.892391, 0.957587]|15/18|
|new/cgo|user_cpu_ns_per_op|0.972376|[0.937591, 1.011255]|12/18|
|new/cgo|system_cpu_ns_per_op|1.002667|[0.961674, 1.045455]|9/18|
|old/cgo|p99_ns|0.926043|[0.871588, 0.985218]|14/18|
|old/cgo|p999_ns|0.934745|[0.862712, 1.014277]|12/18|
|old/cgo|p90_ns|0.951759|[0.883857, 1.023321]|10/18|
|old/cgo|p50_ns|0.939493|[0.907604, 0.970027]|16/18|
|old/cgo|process_cpu_ns_per_op|1.045086|[1.015103, 1.081178]|5/18|
|old/cgo|mean_ns|0.926781|[0.893599, 0.960326]|15/18|
|old/cgo|user_cpu_ns_per_op|1.059249|[1.025626, 1.098534]|4/18|
|old/cgo|system_cpu_ns_per_op|1.033103|[0.993811, 1.079302]|8/18|

## Campaign point estimates (all blocks retained)

|Campaign|Ratio|Metric|GM|Pairs <1|
|---|---|---|---:|---:|
|R6|new/old|p99_ns|1.045277|3/6|
|R7a|new/old|p99_ns|0.977807|2/6|
|R7b|new/old|p99_ns|1.000072|3/6|
|R6|new/old|process_cpu_ns_per_op|0.920064|6/6|
|R7a|new/old|process_cpu_ns_per_op|0.954689|6/6|
|R7b|new/old|process_cpu_ns_per_op|0.962118|6/6|
|R6|new/cgo|p99_ns|1.004037|3/6|
|R7a|new/cgo|p99_ns|0.888346|5/6|
|R7b|new/cgo|p99_ns|0.910074|4/6|
|R6|new/cgo|process_cpu_ns_per_op|0.951449|4/6|
|R7a|new/cgo|process_cpu_ns_per_op|1.023845|3/6|
|R7b|new/cgo|process_cpu_ns_per_op|0.990249|4/6|
|R6|old/cgo|p99_ns|0.960546|4/6|
|R7a|old/cgo|p99_ns|0.908508|5/6|
|R7b|old/cgo|p99_ns|0.910009|5/6|
|R6|old/cgo|process_cpu_ns_per_op|1.034111|2/6|
|R7a|old/cgo|process_cpu_ns_per_op|1.072438|2/6|
|R7b|old/cgo|process_cpu_ns_per_op|1.029238|1/6|

## Per-block p99 and CPU ratios

|Block|Ratio|p99|CPU|
|---|---|---:|---:|
|R6/b000|new/old|1.141943|0.955246|
|R6/b001|new/old|1.048838|0.932116|
|R6/b002|new/old|1.360382|0.920899|
|R6/b003|new/old|0.939183|0.882864|
|R6/b004|new/old|0.941085|0.919721|
|R6/b005|new/old|0.905723|0.911090|
|R7a/b000|new/old|1.066111|0.943098|
|R7a/b001|new/old|0.780563|0.939337|
|R7a/b002|new/old|0.813309|0.946678|
|R7a/b003|new/old|1.151229|0.967092|
|R7a/b004|new/old|1.081949|0.978800|
|R7a/b005|new/old|1.036776|0.953737|
|R7b/b000|new/old|0.956306|0.939637|
|R7b/b001|new/old|1.155138|0.937382|
|R7b/b002|new/old|1.081718|0.976744|
|R7b/b003|new/old|0.843681|0.954255|
|R7b/b004|new/old|1.017768|0.983176|
|R7b/b005|new/old|0.975022|0.982691|
|R6/b000|new/cgo|1.115094|1.041843|
|R6/b001|new/cgo|1.029546|1.023914|
|R6/b002|new/cgo|0.975910|0.920073|
|R6/b003|new/cgo|1.077320|0.833244|
|R6/b004|new/cgo|0.964062|0.956535|
|R6/b005|new/cgo|0.880402|0.948312|
|R7a/b000|new/cgo|0.919999|1.001918|
|R7a/b001|new/cgo|0.763216|1.218826|
|R7a/b002|new/cgo|0.988982|1.081666|
|R7a/b003|new/cgo|1.031240|0.978889|
|R7a/b004|new/cgo|0.810994|0.952891|
|R7a/b005|new/cgo|0.846237|0.934892|
|R7b/b000|new/cgo|0.783341|0.951173|
|R7b/b001|new/cgo|1.115173|0.992333|
|R7b/b002|new/cgo|0.914917|0.982718|
|R7b/b003|new/cgo|0.719222|1.002170|
|R7b/b004|new/cgo|1.005891|1.034165|
|R7b/b005|new/cgo|0.982589|0.980818|
|R6/b000|old/cgo|0.976488|1.090654|
|R6/b001|old/cgo|0.981607|1.098483|
|R6/b002|old/cgo|0.717380|0.999104|
|R6/b003|old/cgo|1.147081|0.943796|
|R6/b004|old/cgo|1.024416|1.040027|
|R6/b005|old/cgo|0.972044|1.040854|
|R7a/b000|old/cgo|0.862948|1.062369|
|R7a/b001|old/cgo|0.977776|1.297538|
|R7a/b002|old/cgo|1.215998|1.142591|
|R7a/b003|old/cgo|0.895773|1.012199|
|R7a/b004|old/cgo|0.749568|0.973529|
|R7a/b005|old/cgo|0.816220|0.980241|
|R7b/b000|old/cgo|0.819132|1.012278|
|R7b/b001|old/cgo|0.965402|1.058621|
|R7b/b002|old/cgo|0.845800|1.006116|
|R7b/b003|old/cgo|0.852481|1.050212|
|R7b/b004|old/cgo|0.988330|1.051861|
|R7b/b005|old/cgo|1.007760|0.998094|

## Top-1% clustering

Exactly 20 largest measured samples/run (ties broken by ascending operation index). An observation counts if another distinct top-20 index is within ±20, inclusive; no wraparound. Indices and latency values are in r7-analysis.json. All 54 per-run fractions follow.

|Arm|Neighbor samples / top samples|Fraction|Median run fraction|Run range|
|---|---:|---:|---:|---|
|cgo|138/360|0.3833|0.3500|[0.1000, 0.8500]|
|old|141/360|0.3917|0.3750|[0.2000, 0.6000]|
|new|129/360|0.3583|0.4000|[0.1000, 0.6000]|

For context only, a uniformly random 20-position subset of 2,000 has expected fraction 0.318613 under this same rule; this is not a causal model or a significance test.

|Campaign/block|Arm|p99 µs|p99.9 µs|Top-20 neighbors|Fraction|
|---|---|---:|---:|---:|---:|
|R6/b000|old|3817.564|6668.713|11/20|0.5500|
|R6/b000|cgo|3909.483|6082.907|12/20|0.6000|
|R6/b000|new|4359.440|6171.576|8/20|0.4000|
|R6/b001|new|4343.079|7001.742|9/20|0.4500|
|R6/b001|cgo|4218.440|6357.705|6/20|0.3000|
|R6/b001|old|4140.850|5877.309|5/20|0.2500|
|R6/b002|cgo|5318.473|7621.507|5/20|0.2500|
|R6/b002|new|5190.353|6960.482|5/20|0.2500|
|R6/b002|old|3815.364|6127.117|5/20|0.2500|
|R6/b003|new|4247.981|6122.717|4/20|0.2000|
|R6/b003|old|4523.059|7188.680|8/20|0.4000|
|R6/b003|cgo|3943.102|5791.290|6/20|0.3000|
|R6/b004|old|4616.047|7297.589|6/20|0.3000|
|R6/b004|new|4344.091|5779.830|6/20|0.3000|
|R6/b004|cgo|4506.028|7310.849|11/20|0.5500|
|R6/b005|cgo|4659.838|6217.846|7/20|0.3500|
|R6/b005|old|4529.568|7240.559|4/20|0.2000|
|R6/b005|new|4102.532|6623.514|2/20|0.1000|
|R7a/b000|old|3611.905|6446.865|10/20|0.5000|
|R7a/b000|new|3850.693|6138.706|9/20|0.4500|
|R7a/b000|cgo|4185.541|6104.678|11/20|0.5500|
|R7a/b001|old|4360.619|9658.012|12/20|0.6000|
|R7a/b001|cgo|4459.730|7993.004|17/20|0.8500|
|R7a/b001|new|3403.736|4969.756|10/20|0.5000|
|R7a/b002|new|3805.123|5004.125|9/20|0.4500|
|R7a/b002|cgo|3847.514|6285.756|2/20|0.1000|
|R7a/b002|old|4678.568|6216.586|5/20|0.2500|
|R7a/b003|new|4523.769|7019.490|8/20|0.4000|
|R7a/b003|old|3929.513|6370.736|9/20|0.4500|
|R7a/b003|cgo|4386.729|7470.227|9/20|0.4500|
|R7a/b004|cgo|5121.344|8634.970|11/20|0.5500|
|R7a/b004|old|3838.794|6088.919|7/20|0.3500|
|R7a/b004|new|4153.380|6140.557|8/20|0.4000|
|R7a/b005|cgo|4739.917|7708.165|10/20|0.5000|
|R7a/b005|new|4011.092|6567.144|4/20|0.2000|
|R7a/b005|old|3868.814|5571.581|12/20|0.6000|
|R7b/b000|new|3489.564|6668.113|9/20|0.4500|
|R7b/b000|cgo|4454.720|6361.216|8/20|0.4000|
|R7b/b000|old|3649.005|5343.573|11/20|0.5500|
|R7b/b001|old|3853.113|5905.679|6/20|0.3000|
|R7b/b001|new|4450.879|6857.841|10/20|0.5000|
|R7b/b001|cgo|3991.202|6331.066|4/20|0.2000|
|R7b/b002|new|4343.869|6203.817|2/20|0.1000|
|R7b/b002|old|4015.712|6454.455|12/20|0.6000|
|R7b/b002|cgo|4747.827|6544.924|3/20|0.1500|
|R7b/b003|cgo|5382.402|8170.733|4/20|0.2000|
|R7b/b003|new|3871.144|5900.827|3/20|0.1500|
|R7b/b003|old|4588.397|6549.995|8/20|0.4000|
|R7b/b004|old|4087.072|6723.773|6/20|0.3000|
|R7b/b004|cgo|4135.331|9753.132|7/20|0.3500|
|R7b/b004|new|4159.691|6502.445|11/20|0.5500|
|R7b/b005|cgo|3954.802|5467.493|5/20|0.2500|
|R7b/b005|old|3985.491|5892.719|4/20|0.2000|
|R7b/b005|new|3885.943|6193.357|12/20|0.6000|

## Scope

Nominal 95% percentile bootstrap over all 18 paired run blocks (10,000 replicates, seed 787707); not an operation bootstrap, not a ratio of pooled quantiles or medians. Same VM and continuously running server: block independence/CI coverage are not established; independent schedule seeds do not mean independent physical hosts. No multiplicity correction, prespecified noninferiority margin, or capacity/global-native claim. R6 prompted these two follow-ups; this is exploratory, not a fresh confirmatory test. Each run has only 2,000 observations, so p99.9 is highly variable (nearest rank 1998, only two observations above it). CPU is measured-phase process getrusage user+system, not perf task-clock or whole-process GNU time. Top-1% index clustering alone does not establish periodicity, stall cause, or that a p99 regression is harmless. Go MemStats excludes native C++ heap; native client internal counters are unobserved.
