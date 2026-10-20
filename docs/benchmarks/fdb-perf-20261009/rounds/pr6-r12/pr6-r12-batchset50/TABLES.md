# pr6-r12-batchset50

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. source identities are retained in plan.json and verified against each manifest; frozen native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50|cgo|2113.588|3841.608|853.915|
|batchset50|old|1938.334|3890.653|882.362|
|batchset50|new|2091.701|4100.957|897.760|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50|new/old|1.064660|1.058541|1.021180|
|batchset50|new/cgo|0.950090|0.987899|1.052337|
|batchset50|old/cgo|0.892388|0.933265|1.030511|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|1.021180|[1.010393,1.033829]|49|48|
|new/cgo|1.052337|[1.024253,1.082550]|49|0|
|old/cgo|1.030511|[1.007195,1.050498]|48|0|

OLD base8ab5c5a4d omits CAUSAL_READ_RISKY on write-only commit GRVs; NEW explicitly adds the C++ flag when no read version exists. This is the PR6 intervention, not an unstated algorithmic-equivalence assumption. C++ NativeAPI.actor.cpp:6577-6578; GrvProxyServer.actor.cpp:647-648.
