# pr5-r10-batchset50

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published9377c0863aef7ecdee250cdc323e40ebf69d5e46, new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+PR5patch4a413f5d64d829b87e681bd9202a5bfa1fd48c1e74b5c3c68ec4ea88e3ef6063, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50|cgo|2190.163|3872.828|858.186|
|batchset50|old|1977.197|3652.260|882.368|
|batchset50|new|2019.112|3708.510|794.270|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50|new/old|1.008551|0.982294|0.894214|
|batchset50|new/cgo|0.922956|0.925107|0.923019|
|batchset50|old/cgo|0.915131|0.941782|1.032213|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.894214|[0.873120,0.913014]|48|48|
|new/cgo|0.923019|[0.905445,0.942937]|48|0|
|old/cgo|1.032213|[1.003156,1.062114]|48|0|

This frozen master590145 Go write-only commit also omits the causal-read-risky GRV flag that C++ supplies (NativeAPI.actor.cpp:6577-6578). Go transaction.go:1980-1985 invokes ensureReadVersion; grvFlags at:3398-3411 only adds this flag on an explicit option. Without the flag the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). This pre-existing implementation difference is measured, not corrected or quietly stacked into this campaign.
