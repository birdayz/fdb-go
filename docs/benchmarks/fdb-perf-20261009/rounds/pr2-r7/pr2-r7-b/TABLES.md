# pr2-r7-b

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published9377c0863aef7ecdee250cdc323e40ebf69d5e46, new=old+PR2patchb7736a4a40a159b1c1e75904c28292ee7be0e0f763e89a5a8d9c5d756c876a18, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50x1000|cgo|2533.030|4295.025|1124.146|
|batchset50x1000|old|2306.771|4000.602|1160.975|
|batchset50x1000|new|2502.756|4022.817|1119.243|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50x1000|new/old|1.053486|1.000072|0.962118|
|batchset50x1000|new/cgo|0.969890|0.910074|0.990249|
|batchset50x1000|old/cgo|0.920648|0.910009|1.029238|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
