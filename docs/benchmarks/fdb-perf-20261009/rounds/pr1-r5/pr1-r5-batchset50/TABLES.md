# pr1-r5-batchset50

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published9377c0863aef7ecdee250cdc323e40ebf69d5e46, new=old+PR1patch33f1ed2062a8f412f0ee291af16379f41e32aa7edee5b4274d1db1f7fc7b96ef, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50|cgo|2193.699|4104.372|851.059|
|batchset50|old|1887.260|3311.742|868.660|
|batchset50|new|2051.194|3443.656|871.319|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50|new/old|1.039208|0.987230|0.997384|
|batchset50|new/cgo|0.929302|0.837800|0.997856|
|batchset50|old/cgo|0.894241|0.848637|1.000473|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
