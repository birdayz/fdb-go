# pr4-r9-read1000

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published9377c0863aef7ecdee250cdc323e40ebf69d5e46, new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+PR4patch2414b55ab15d20c8597ebc54917b67c24320c8f1b02e8ab6a412a0c1de3be014, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|read1000|cgo|594.566|903.339|409.774|
|read1000|old|457.034|770.930|485.500|
|read1000|new|440.617|693.051|463.217|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|read1000|new/old|0.964496|0.914975|0.970774|
|read1000|new/cgo|0.752136|0.766511|1.189687|
|read1000|old/cgo|0.779823|0.837739|1.225503|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
