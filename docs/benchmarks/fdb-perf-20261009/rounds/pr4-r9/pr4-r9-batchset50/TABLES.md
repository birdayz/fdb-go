# pr4-r9-batchset50

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published9377c0863aef7ecdee250cdc323e40ebf69d5e46, new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+PR4patch2414b55ab15d20c8597ebc54917b67c24320c8f1b02e8ab6a412a0c1de3be014, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50|cgo|2205.156|4504.724|856.523|
|batchset50|old|1914.603|3912.838|888.864|
|batchset50|new|1947.206|3831.847|880.097|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50|new/old|1.029174|1.020431|0.992056|
|batchset50|new/cgo|0.893802|0.858061|1.018904|
|batchset50|old/cgo|0.868465|0.840881|1.027062|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
