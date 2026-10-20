
## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.865475|[0.829873,0.897002]|48|50|
|new/cgo|0.924004|[0.897914,0.946131]|48|1|
|old/cgo|1.067627|[1.036683,1.103344]|50|1|

This frozen base8ab5c5a4d Go write-only commit also omits the causal-read-risky GRV flag that C++ supplies (NativeAPI.actor.cpp:6577-6578). Go transaction.go:1980-1985 invokes ensureReadVersion; grvFlags at:3398-3411 only adds this flag on an explicit option. Without the flag the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). This pre-existing implementation difference is measured, not corrected or quietly stacked into this campaign.
