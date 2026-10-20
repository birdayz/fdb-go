# pr6-r12b-batchset50

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. source identities are retained in plan.json and verified against each manifest; frozen native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50|cgo|2092.958|3696.590|845.095|
|batchset50|old|1974.590|3745.394|896.867|
|batchset50|new|2032.932|3709.330|880.404|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50|new/old|1.035822|0.975961|0.998885|
|batchset50|new/cgo|0.975216|0.968062|1.055157|
|batchset50|old/cgo|0.941490|0.991906|1.056334|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.998885|[0.973924,1.027450]|48|48|
|new/cgo|1.055157|[1.042059,1.069766]|48|0|
|old/cgo|1.056334|[1.033515,1.074965]|48|0|

OLD base8ab5c5a4d omits CAUSAL_READ_RISKY on write-only commit GRVs; NEW explicitly adds the C++ flag when no read version exists. This is the PR6 intervention, not an unstated algorithmic-equivalence assumption. C++ NativeAPI.actor.cpp:6577-6578; GrvProxyServer.actor.cpp:647-648.
