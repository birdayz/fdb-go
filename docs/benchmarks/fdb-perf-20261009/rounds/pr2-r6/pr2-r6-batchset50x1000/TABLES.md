# pr2-r6-batchset50x1000

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published9377c0863aef7ecdee250cdc323e40ebf69d5e46, new=old+PR2patchb7736a4a40a159b1c1e75904c28292ee7be0e0f763e89a5a8d9c5d756c876a18, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50x1000|cgo|2457.386|4362.234|1079.472|
|batchset50x1000|old|2330.614|4331.954|1061.833|
|batchset50x1000|new|2259.096|4343.585|997.239|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50x1000|new/old|0.973700|1.045277|0.920064|
|batchset50x1000|new/cgo|0.919068|1.004037|0.951449|
|batchset50x1000|old/cgo|0.943893|0.960546|1.034111|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
