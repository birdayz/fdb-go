# pr2-r6-batchset50

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published9377c0863aef7ecdee250cdc323e40ebf69d5e46, new=old+PR2patchb7736a4a40a159b1c1e75904c28292ee7be0e0f763e89a5a8d9c5d756c876a18, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50|cgo|2078.282|3816.013|848.520|
|batchset50|old|1919.798|3413.086|881.109|
|batchset50|new|1922.554|3421.356|875.186|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50|new/old|1.003520|1.000807|1.008998|
|batchset50|new/cgo|0.952870|0.936712|1.031448|
|batchset50|old/cgo|0.949528|0.935957|1.022250|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
