# R11 combined PR3+PR4+PR5v2 — batchset50

Old=8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead;new=master8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead+combinedPR3+PR4+PR5v2patch999bc230aadbf6f5069b237d29bd4c6a690f30d015971ec175d417e2e0dd6316 (authoritative combined patch applied once; PR1/PR2 NOT stacked);OLD freshly rebuilt from the exact base; frozenV6 unchanged;CGo=immutable native7.3.77. FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2146.240|3968.513|844.773|463.365|392.634|1616.099|59.001|1|0.000067|
|old|1958.748|3472.971|900.369|508.777|390.280|80408.274|299.604|50|0.003333|
|new|2031.345|3833.113|790.716|458.264|328.489|80331.891|303.402|48|0.003200|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|1.018752|1.030152|0.865475|0.892531|0.831368|6/6|
|new/cgo|0.930681|0.898750|0.924004|0.989734|0.847593|6/6|
|old/cgo|0.913550|0.872444|1.067627|1.108908|1.019516|0/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=0.998796 (below1 in4/6), go_mallocs_per_op=1.012608 (below1 in0/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed791707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|1.018752|[0.952265, 1.085820]|3/6|
|new/old|p99_ns|1.030152|[0.899085, 1.168447]|2/6|
|new/old|process_cpu_ns_per_op|0.865475|[0.829873, 0.897002]|6/6|
|new/old|user_cpu_ns_per_op|0.892531|[0.833905, 0.944556]|6/6|
|new/old|system_cpu_ns_per_op|0.831368|[0.812498, 0.851298]|6/6|
|new/cgo|mean_ns|0.930681|[0.866314, 0.986939]|5/6|
|new/cgo|p99_ns|0.898750|[0.749875, 1.064603]|3/6|
|new/cgo|process_cpu_ns_per_op|0.924004|[0.897914, 0.946131]|6/6|
|new/cgo|user_cpu_ns_per_op|0.989734|[0.975036, 1.009084]|4/6|
|new/cgo|system_cpu_ns_per_op|0.847593|[0.792177, 0.900940]|6/6|
|old/cgo|mean_ns|0.913550|[0.856449, 0.981062]|5/6|
|old/cgo|p99_ns|0.872444|[0.730084, 1.076086]|5/6|
|old/cgo|process_cpu_ns_per_op|1.067627|[1.036683, 1.103344]|0/6|
|old/cgo|user_cpu_ns_per_op|1.108908|[1.045589, 1.191678]|1/6|
|old/cgo|system_cpu_ns_per_op|1.019516|[0.969377, 1.065695]|2/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|1.079360|1.001334|0.889351|0.957298|0.807942|
|1|new/old|0.953429|0.864881|0.861166|0.902003|0.808212|
|2|new/old|0.901160|0.813956|0.925226|0.979050|0.860050|
|3|new/old|1.121840|1.286000|0.867865|0.865366|0.871379|
|4|new/old|1.120897|1.131008|0.865056|0.896301|0.826016|
|5|new/old|0.958642|1.165638|0.789990|0.770947|0.816836|
|0|new/cgo|0.910351|0.659017|0.938396|1.030555|0.832686|
|1|new/cgo|0.795775|0.732470|0.869633|0.986928|0.742009|
|2|new/cgo|0.936466|1.140259|0.963671|0.976293|0.946798|
|3|new/cgo|1.042591|1.157878|0.939817|0.968142|0.902920|
|4|new/cgo|0.960508|0.772625|0.931918|0.975418|0.878784|
|5|new/cgo|0.956528|1.070315|0.903590|1.002410|0.798812|
|0|old/cgo|0.843417|0.658139|1.055147|1.076525|1.030626|
|1|old/cgo|0.834646|0.846903|1.009832|1.094151|0.918087|
|2|old/cgo|1.039179|1.400886|1.041552|0.997184|1.100864|
|3|old/cgo|0.929359|0.900371|1.082908|1.118766|1.036196|
|4|old/cgo|0.856910|0.683129|1.077292|1.088270|1.063883|
|5|old/cgo|0.997794|0.918223|1.143799|1.300231|0.977935|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50-p0-cgo|1616.096|59.001|0|0.000000|
|b000-batchset50-p1-new|80351.555|303.414|8|0.003200|
|b000-batchset50-p2-old|80412.509|299.606|8|0.003200|
|b001-batchset50-p0-new|80423.984|303.434|8|0.003200|
|b001-batchset50-p1-cgo|1616.045|59.000|0|0.000000|
|b001-batchset50-p2-old|80404.038|299.601|8|0.003200|
|b002-batchset50-p0-cgo|1616.230|59.002|0|0.000000|
|b002-batchset50-p1-old|80402.269|299.588|8|0.003200|
|b002-batchset50-p2-new|80403.363|303.422|8|0.003200|
|b003-batchset50-p0-old|80396.733|299.590|8|0.003200|
|b003-batchset50-p1-new|80270.784|303.390|8|0.003200|
|b003-batchset50-p2-cgo|1616.102|59.001|0|0.000000|
|b004-batchset50-p0-new|80243.104|303.340|8|0.003200|
|b004-batchset50-p1-old|80448.026|299.668|9|0.003600|
|b004-batchset50-p2-cgo|1616.102|59.001|1|0.000400|
|b005-batchset50-p0-old|80522.397|299.646|9|0.003600|
|b005-batchset50-p1-cgo|1616.090|59.001|0|0.000000|
|b005-batchset50-p2-new|80312.227|303.365|8|0.003200|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.865475|[0.829873,0.897002]|48|50|
|new/cgo|0.924004|[0.897914,0.946131]|48|1|
|old/cgo|1.067627|[1.036683,1.103344]|50|1|

This frozen base8ab5c5a4d Go write-only commit also omits the causal-read-risky GRV flag that C++ supplies (NativeAPI.actor.cpp:6577-6578). Go transaction.go:1980-1985 invokes ensureReadVersion; grvFlags at:3398-3411 only adds this flag on an explicit option. Without the flag the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). This pre-existing implementation difference is measured, not corrected or quietly stacked into this campaign.
