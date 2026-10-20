# stack-r11v2-batchset50

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead, new=master8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead+combinedPR3+PR4+PR5v2patch999bc230aadbf6f5069b237d29bd4c6a690f30d015971ec175d417e2e0dd6316, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50|cgo|2146.240|3968.513|844.773|
|batchset50|old|1958.748|3472.971|900.369|
|batchset50|new|2031.345|3833.113|790.716|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50|new/old|1.018752|1.030152|0.865475|
|batchset50|new/cgo|0.930681|0.898750|0.924004|
|batchset50|old/cgo|0.913550|0.872444|1.067627|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.865475|[0.829873,0.897002]|48|50|
|new/cgo|0.924004|[0.897914,0.946131]|48|1|
|old/cgo|1.067627|[1.036683,1.103344]|50|1|

This frozen base8ab5c5a4d Go write-only commit also omits the causal-read-risky GRV flag that C++ supplies (NativeAPI.actor.cpp:6577-6578). Go transaction.go:1980-1985 invokes ensureReadVersion; grvFlags at:3398-3411 only adds this flag on an explicit option. Without the flag the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). This pre-existing implementation difference is measured, not corrected or quietly stacked into this campaign.
