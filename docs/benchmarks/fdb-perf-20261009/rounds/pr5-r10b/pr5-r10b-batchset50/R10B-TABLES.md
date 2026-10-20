# R10B PR5v2 nonblocking direct idle writer — batchset50

Old=8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead;new=master8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead+patch8199a1c86c4df4ea2a877cd39355376d07ccebb3847dc2f4e4d74eb7e882eb54 (PR1/PR2/PR3/PR4 NOT stacked);OLD freshly rebuilt from the exact base; frozenV6 unchanged;CGo=immutable native7.3.77. FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2091.896|3812.623|827.455|463.612|374.303|1616.186|59.002|0|0.000000|
|old|2049.623|4007.202|900.910|504.827|394.905|80431.400|299.603|48|0.003200|
|new|1957.647|3788.499|798.178|459.340|339.308|80344.762|304.402|49|0.003267|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|0.961412|0.988798|0.889564|0.897306|0.879216|6/6|
|new/cgo|0.895520|1.015197|0.947320|0.980070|0.907743|4/6|
|old/cgo|0.931464|1.026699|1.064927|1.092236|1.032446|0/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=0.999271 (below1 in5/6), go_mallocs_per_op=1.016091 (below1 in0/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed790907, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.961412|[0.931868, 0.988846]|5/6|
|new/old|p99_ns|0.988798|[0.844531, 1.163003]|3/6|
|new/old|process_cpu_ns_per_op|0.889564|[0.851420, 0.936392]|6/6|
|new/old|user_cpu_ns_per_op|0.897306|[0.853346, 0.943233]|6/6|
|new/old|system_cpu_ns_per_op|0.879216|[0.842623, 0.934337]|5/6|
|new/cgo|mean_ns|0.895520|[0.839831, 0.951113]|6/6|
|new/cgo|p99_ns|1.015197|[0.877889, 1.152545]|2/6|
|new/cgo|process_cpu_ns_per_op|0.947320|[0.906298, 0.991362]|4/6|
|new/cgo|user_cpu_ns_per_op|0.980070|[0.915137, 1.030264]|3/6|
|new/cgo|system_cpu_ns_per_op|0.907743|[0.849965, 0.962858]|6/6|
|old/cgo|mean_ns|0.931464|[0.869860, 0.989409]|4/6|
|old/cgo|p99_ns|1.026699|[0.915390, 1.157417]|3/6|
|old/cgo|process_cpu_ns_per_op|1.064927|[1.038745, 1.092669]|0/6|
|old/cgo|user_cpu_ns_per_op|1.092236|[1.057555, 1.136590]|0/6|
|old/cgo|system_cpu_ns_per_op|1.032446|[0.978222, 1.091351]|3/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|0.997660|1.091328|0.900716|0.922051|0.873550|
|1|new/old|0.902036|0.806551|0.856571|0.883269|0.822709|
|2|new/old|0.944507|0.769139|0.856307|0.846860|0.869574|
|3|new/old|0.946820|0.905926|0.915940|0.954549|0.866162|
|4|new/old|1.005551|1.385347|0.826638|0.813860|0.842337|
|5|new/old|0.975828|1.100024|0.990602|0.974171|1.013088|
|0|new/cgo|0.812565|1.178696|0.925098|0.991794|0.848416|
|1|new/cgo|0.793408|0.749486|0.890964|0.973622|0.798638|
|2|new/cgo|0.968530|1.003020|0.956057|1.010131|0.890840|
|3|new/cgo|0.950095|0.900005|1.009500|1.022722|0.991292|
|4|new/cgo|0.909987|1.126974|0.885880|0.835163|0.954705|
|5|new/cgo|0.955394|1.218062|1.025576|1.063711|0.979374|
|0|old/cgo|0.814470|1.080056|1.027069|1.075639|0.971228|
|1|old/cgo|0.879575|0.929248|1.040152|1.102294|0.970742|
|2|old/cgo|1.025435|1.304083|1.116489|1.192797|1.024455|
|3|old/cgo|1.003459|0.993464|1.102147|1.071419|1.144464|
|4|old/cgo|0.904964|0.813496|1.071667|1.026175|1.133400|
|5|old/cgo|0.979060|1.107305|1.035305|1.091914|0.966721|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50-p0-cgo|1616.186|59.002|0|0.000000|
|b000-batchset50-p1-old|80363.930|299.512|8|0.003200|
|b000-batchset50-p2-new|80589.821|304.478|9|0.003600|
|b001-batchset50-p0-cgo|1616.275|59.002|0|0.000000|
|b001-batchset50-p1-new|80427.645|304.420|8|0.003200|
|b001-batchset50-p2-old|80474.486|299.649|8|0.003200|
|b002-batchset50-p0-old|80514.848|299.612|8|0.003200|
|b002-batchset50-p1-new|80286.784|304.358|8|0.003200|
|b002-batchset50-p2-cgo|1616.051|59.000|0|0.000000|
|b003-batchset50-p0-new|80276.397|304.364|8|0.003200|
|b003-batchset50-p1-cgo|1616.186|59.002|0|0.000000|
|b003-batchset50-p2-old|80438.122|299.521|8|0.003200|
|b004-batchset50-p0-old|80405.686|299.594|8|0.003200|
|b004-batchset50-p1-cgo|1616.096|59.001|0|0.000000|
|b004-batchset50-p2-new|80400.554|304.423|8|0.003200|
|b005-batchset50-p0-new|80288.970|304.384|8|0.003200|
|b005-batchset50-p1-old|80424.678|299.613|8|0.003200|
|b005-batchset50-p2-cgo|1616.320|59.003|0|0.000000|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.889564|[0.851420,0.936392]|49|48|
|new/cgo|0.947320|[0.906298,0.991362]|49|0|
|old/cgo|1.064927|[1.038745,1.092669]|48|0|

This frozen base8ab5c5a4d Go write-only commit also omits the causal-read-risky GRV flag that C++ supplies (NativeAPI.actor.cpp:6577-6578). Go transaction.go:1980-1985 invokes ensureReadVersion; grvFlags at:3398-3411 only adds this flag on an explicit option. Without the flag the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). This pre-existing implementation difference is measured, not corrected or quietly stacked into this campaign.
