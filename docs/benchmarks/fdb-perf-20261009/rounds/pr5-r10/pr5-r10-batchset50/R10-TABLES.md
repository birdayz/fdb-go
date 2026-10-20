# R10 PR5 direct idle writer — batchset50

Old=9377c0863aef7ecdee250cdc323e40ebf69d5e46;new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+patch4a413f5d64d829b87e681bd9202a5bfa1fd48c1e74b5c3c68ec4ea88e3ef6063 (PR1/PR2/PR3/PR4 NOT stacked);old9377 code matches590145 in pkg/fdbgo/tools/dependency pins;CGo=immutable native7.3.77. FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2190.163|3872.828|858.186|465.299|390.630|1616.182|59.002|0|0.000000|
|old|1977.197|3652.260|882.368|498.432|382.934|80417.325|299.609|48|0.003200|
|new|2019.112|3708.510|794.270|450.999|340.246|80199.275|298.374|48|0.003200|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|1.008551|0.982294|0.894214|0.905094|0.880160|6/6|
|new/cgo|0.922956|0.925107|0.923019|0.966532|0.872084|6/6|
|old/cgo|0.915131|0.941782|1.032213|1.067881|0.990824|1/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=0.997437 (below1 in6/6), go_mallocs_per_op=0.995908 (below1 in6/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed790707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|1.008551|[0.938564, 1.079270]|2/6|
|new/old|p99_ns|0.982294|[0.919024, 1.047889]|4/6|
|new/old|process_cpu_ns_per_op|0.894214|[0.873120, 0.913014]|6/6|
|new/old|user_cpu_ns_per_op|0.905094|[0.896666, 0.912377]|6/6|
|new/old|system_cpu_ns_per_op|0.880160|[0.830505, 0.919748]|6/6|
|new/cgo|mean_ns|0.922956|[0.865583, 0.980677]|5/6|
|new/cgo|p99_ns|0.925107|[0.849195, 0.995274]|5/6|
|new/cgo|process_cpu_ns_per_op|0.923019|[0.905445, 0.942937]|6/6|
|new/cgo|user_cpu_ns_per_op|0.966532|[0.931643, 0.998121]|5/6|
|new/cgo|system_cpu_ns_per_op|0.872084|[0.828398, 0.922835]|6/6|
|old/cgo|mean_ns|0.915131|[0.859118, 0.969590]|5/6|
|old/cgo|p99_ns|0.941782|[0.907340, 0.977597]|5/6|
|old/cgo|process_cpu_ns_per_op|1.032213|[1.003156, 1.062114]|1/6|
|old/cgo|user_cpu_ns_per_op|1.067881|[1.030036, 1.108487]|1/6|
|old/cgo|system_cpu_ns_per_op|0.990824|[0.936736, 1.050594]|3/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|1.124626|1.077759|0.918362|0.910451|0.928775|
|1|new/old|0.903070|1.083580|0.849215|0.914010|0.771556|
|2|new/old|1.101812|0.990610|0.901968|0.897021|0.908116|
|3|new/old|1.043923|0.981207|0.922966|0.914977|0.933800|
|4|new/old|1.004210|0.848473|0.897910|0.906834|0.886223|
|5|new/old|0.897133|0.932752|0.877017|0.887591|0.863282|
|0|new/cgo|0.894822|0.950585|0.893147|0.949646|0.829479|
|1|new/cgo|0.926438|1.037083|0.924843|0.993160|0.842553|
|2|new/cgo|1.034461|0.978295|0.926768|0.889327|0.977282|
|3|new/cgo|0.988604|0.993338|0.969979|0.989408|0.945310|
|4|new/cgo|0.901562|0.773858|0.902325|0.962392|0.832681|
|5|new/cgo|0.808726|0.845511|0.922949|1.020771|0.818227|
|0|old/cgo|0.795662|0.882001|0.972543|1.043050|0.893089|
|1|old/cgo|1.025877|0.957089|1.089056|1.086597|1.092018|
|2|old/cgo|0.938873|0.987568|1.027496|0.991422|1.076164|
|3|old/cgo|0.947008|1.012363|1.050937|1.081347|1.012326|
|4|old/cgo|0.897783|0.912061|1.004917|1.061267|0.939584|
|5|old/cgo|0.901456|0.906469|1.052373|1.150047|0.947810|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50-p0-cgo|1616.096|59.001|0|0.000000|
|b000-batchset50-p1-new|80247.261|298.419|8|0.003200|
|b000-batchset50-p2-old|80481.584|299.574|8|0.003200|
|b001-batchset50-p0-old|80305.917|299.630|8|0.003200|
|b001-batchset50-p1-new|80176.099|298.349|8|0.003200|
|b001-batchset50-p2-cgo|1616.365|59.003|0|0.000000|
|b002-batchset50-p0-new|80222.451|298.392|8|0.003200|
|b002-batchset50-p1-cgo|1616.365|59.003|0|0.000000|
|b002-batchset50-p2-old|80392.698|299.534|8|0.003200|
|b003-batchset50-p0-cgo|1616.096|59.001|0|0.000000|
|b003-batchset50-p1-old|80491.270|299.606|8|0.003200|
|b003-batchset50-p2-new|80142.746|298.351|8|0.003200|
|b004-batchset50-p0-old|80375.018|299.612|8|0.003200|
|b004-batchset50-p1-cgo|1616.051|59.000|0|0.000000|
|b004-batchset50-p2-new|80144.006|298.356|8|0.003200|
|b005-batchset50-p0-new|80319.283|298.392|8|0.003200|
|b005-batchset50-p1-old|80441.952|299.660|8|0.003200|
|b005-batchset50-p2-cgo|1616.269|59.002|0|0.000000|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.894214|[0.873120,0.913014]|48|48|
|new/cgo|0.923019|[0.905445,0.942937]|48|0|
|old/cgo|1.032213|[1.003156,1.062114]|48|0|

This frozen master590145 Go write-only commit also omits the causal-read-risky GRV flag that C++ supplies (NativeAPI.actor.cpp:6577-6578). Go transaction.go:1980-1985 invokes ensureReadVersion; grvFlags at:3398-3411 only adds this flag on an explicit option. Without the flag the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). This pre-existing implementation difference is measured, not corrected or quietly stacked into this campaign.
