# hillclimb-arena-003

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published affef937, new=affef937+arena003, cgo=immutable native7.3.77.

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

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
