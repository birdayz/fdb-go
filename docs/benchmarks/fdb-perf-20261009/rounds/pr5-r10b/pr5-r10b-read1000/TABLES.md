# pr5-r10b-read1000

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead, new=master8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead+PR5patch8199a1c86c4df4ea2a877cd39355376d07ccebb3847dc2f4e4d74eb7e882eb54, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|read1000|cgo|586.248|872.879|392.386|
|read1000|old|439.942|635.676|463.079|
|read1000|new|420.079|633.671|414.947|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|read1000|new/old|0.961367|0.992697|0.908048|
|read1000|new/cgo|0.721373|0.720495|1.068004|
|read1000|old/cgo|0.750361|0.725795|1.176154|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.908048|[0.891231,0.925246]|36|36|
|new/cgo|1.068004|[1.002081,1.138489]|36|18|
|old/cgo|1.176154|[1.096730,1.267415]|36|18|
