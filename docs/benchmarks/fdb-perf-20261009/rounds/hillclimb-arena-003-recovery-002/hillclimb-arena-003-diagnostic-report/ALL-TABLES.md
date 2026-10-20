# Arena003 completed campaigns

Old = published affef93729368a3fb183df90c5332b5bd2289b25; new = that source plus arena003 scratch patch; CGo = immutable native 7.3.77/API730. CPU is measured-process user+system time, not wall time. Absolute values are medians of six run-level statistics. Ratios are geometric means of six within-block ratios, NOT ratios of the medians. All workloads use one worker and 40 warmup transactions. Batch workloads use 100 measured transactions/arm; unprofiled controls use 1000; separate profiling campaigns use 3000.

Each campaign contains all six orders of old/new/CGo. Results are exploratory, not confirmatory/noninferiority/global-native-win/capacity evidence. Profiled campaigns are separate diagnostics, NOT replacements for unprofiled results. Native internal counters remain unobserved; Go allocation statistics exclude C++ heap. Pooled tails are descriptive. Source/binary checks and benchmark checks are not main-module correctness approval.


## hillclimb-arena-003 — UNPROFILED
|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50|cgo|2515.694|3166.148|876.520|
|batchset50|old|2499.898|4875.926|1079.365|
|batchset50|new|2504.148|4106.106|1022.870|
|batchset50x1000|cgo|3214.388|5511.671|1180.035|
|batchset50x1000|old|3100.552|5422.141|1256.310|
|batchset50x1000|new|3026.612|4678.376|1103.695|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50|new/old|0.986306|0.885267|0.944523|
|batchset50|new/cgo|0.980349|1.232484|1.174835|
|batchset50|old/cgo|0.993960|1.392217|1.243839|
|batchset50x1000|new/old|0.949831|0.954677|0.915933|
|batchset50x1000|new/cgo|0.934211|0.874438|0.919501|
|batchset50x1000|old/cgo|0.983555|0.915952|1.003896|

## hillclimb-arena-003-read1000 — UNPROFILED
|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|read1000|cgo|599.576|888.213|405.038|
|read1000|old|429.124|720.215|444.842|
|read1000|new|439.068|785.750|474.844|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|read1000|new/old|1.031315|1.089243|1.110292|
|read1000|new/cgo|0.738830|0.856017|1.248844|
|read1000|old/cgo|0.716396|0.785882|1.124789|

## hillclimb-arena-003-get1000 — UNPROFILED
|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|get1000|cgo|669.578|1042.362|502.485|
|get1000|old|466.397|865.399|523.019|
|get1000|new|459.233|732.970|514.730|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|get1000|new/old|1.003656|0.902840|1.009626|
|get1000|new/cgo|0.700186|0.644083|1.071236|
|get1000|old/cgo|0.697635|0.713396|1.061023|

## hillclimb-arena-003-range100 — UNPROFILED
|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|range100|cgo|856.176|1668.892|641.707|
|range100|old|615.947|1288.179|736.490|
|range100|new|617.080|1303.574|739.286|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|range100|new/old|0.996801|1.046184|0.992613|
|range100|new/cgo|0.758956|0.833696|1.209222|
|range100|old/cgo|0.761392|0.796893|1.218221|

## hillclimb-arena-003-set100 — UNPROFILED
|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|set100|cgo|2068.061|3737.886|553.028|
|set100|old|1822.659|3742.851|567.265|
|set100|new|1799.518|3277.785|561.692|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|set100|new/old|0.981586|0.920249|1.009546|
|set100|new/cgo|0.873100|0.879419|1.013140|
|set100|old/cgo|0.889479|0.955631|1.003560|

## hillclimb-arena-003-read-cpu — PROFILED DIAGNOSTIC
|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|read1000|cgo|563.639|858.543|388.163|
|read1000|old|443.356|688.315|449.539|
|read1000|new|442.294|653.051|474.007|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|read1000|new/old|1.005298|0.979884|1.026848|
|read1000|new/cgo|0.778505|0.767609|1.248272|
|read1000|old/cgo|0.774401|0.783367|1.215635|

## hillclimb-arena-003-read-heap — PROFILED DIAGNOSTIC
|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|read1000|cgo|575.760|877.538|374.381|
|read1000|old|434.361|652.761|450.724|
|read1000|new|443.539|689.465|469.588|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|read1000|new/old|1.045325|1.016391|1.062074|
|read1000|new/cgo|0.828766|0.817565|1.340654|
|read1000|old/cgo|0.792831|0.804381|1.262298|
