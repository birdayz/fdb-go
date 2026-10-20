# pr2-r7-a

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published9377c0863aef7ecdee250cdc323e40ebf69d5e46, new=old+PR2patchb7736a4a40a159b1c1e75904c28292ee7be0e0f763e89a5a8d9c5d756c876a18, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50x1000|cgo|2476.638|4423.230|1076.782|
|batchset50x1000|old|2254.443|3899.164|1136.571|
|batchset50x1000|new|2189.881|3930.892|1073.927|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50x1000|new/old|0.967738|0.977807|0.954689|
|batchset50x1000|new/cgo|0.886487|0.888346|1.023845|
|batchset50x1000|old/cgo|0.916041|0.908508|1.072438|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
