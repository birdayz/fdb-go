# R11 combined PR3+PR4+PR5v2 — read1000

Old=8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead;new=master8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead+combinedPR3+PR4+PR5v2patch999bc230aadbf6f5069b237d29bd4c6a690f30d015971ec175d417e2e0dd6316 (authoritative combined patch applied once; PR1/PR2 NOT stacked);OLD freshly rebuilt from the exact base; frozenV6 unchanged;CGo=immutable native7.3.77. FrozenV6,5000 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|577.986|940.409|374.597|160.552|214.045|1542.178|13.011|18|0.000600|
|old|429.305|650.035|466.386|180.436|282.855|7967.958|63.185|36|0.001200|
|new|379.685|550.141|359.189|138.951|215.123|7798.284|64.088|37|0.001233|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|0.840159|0.832775|0.768017|0.788324|0.754945|6/6|
|new/cgo|0.624057|0.580552|0.951806|0.881347|1.005264|4/6|
|old/cgo|0.742784|0.697130|1.239303|1.118001|1.331572|0/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=0.978798 (below1 in6/6), go_mallocs_per_op=1.014194 (below1 in0/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed791707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.840159|[0.781259, 0.886461]|6/6|
|new/old|p99_ns|0.832775|[0.777984, 0.879939]|6/6|
|new/old|process_cpu_ns_per_op|0.768017|[0.731416, 0.796344]|6/6|
|new/old|user_cpu_ns_per_op|0.788324|[0.745722, 0.828130]|6/6|
|new/old|system_cpu_ns_per_op|0.754945|[0.717703, 0.785300]|6/6|
|new/cgo|mean_ns|0.624057|[0.570882, 0.670067]|6/6|
|new/cgo|p99_ns|0.580552|[0.548244, 0.624421]|6/6|
|new/cgo|process_cpu_ns_per_op|0.951806|[0.890268, 1.015928]|4/6|
|new/cgo|user_cpu_ns_per_op|0.881347|[0.811851, 0.944150]|6/6|
|new/cgo|system_cpu_ns_per_op|1.005264|[0.929019, 1.083223]|3/6|
|old/cgo|mean_ns|0.742784|[0.729907, 0.756350]|6/6|
|old/cgo|p99_ns|0.697130|[0.658052, 0.747409]|6/6|
|old/cgo|process_cpu_ns_per_op|1.239303|[1.189175, 1.286782]|0/6|
|old/cgo|user_cpu_ns_per_op|1.118001|[1.056393, 1.169145]|1/6|
|old/cgo|system_cpu_ns_per_op|1.331572|[1.275127, 1.396836]|0/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|0.904732|0.860516|0.809446|0.841821|0.789867|
|1|new/old|0.710071|0.724908|0.686418|0.704783|0.674384|
|2|new/old|0.815291|0.858414|0.755864|0.771187|0.745457|
|3|new/old|0.877886|0.789920|0.780240|0.816740|0.756779|
|4|new/old|0.888312|0.923836|0.797756|0.848777|0.766891|
|5|new/old|0.861057|0.853591|0.785067|0.756688|0.803353|
|0|new/cgo|0.689000|0.689633|1.079727|0.979961|1.155555|
|1|new/cgo|0.513677|0.536415|0.834827|0.808223|0.854080|
|2|new/cgo|0.594527|0.583072|0.944373|0.930158|0.954622|
|3|new/cgo|0.662017|0.532657|0.959510|0.911549|0.995857|
|4|new/cgo|0.679220|0.576731|1.019904|0.935265|1.085688|
|5|new/cgo|0.624285|0.577812|0.892541|0.746226|1.013113|
|0|old/cgo|0.761552|0.801418|1.333907|1.164096|1.462975|
|1|old/cgo|0.723417|0.739977|1.216207|1.146769|1.266459|
|2|old/cgo|0.729220|0.679243|1.249396|1.206138|1.280586|
|3|old/cgo|0.754104|0.674318|1.229763|1.116083|1.315914|
|4|old/cgo|0.764618|0.624278|1.278466|1.101897|1.415701|
|5|old/cgo|0.725022|0.676920|1.136899|0.986174|1.261104|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-read1000-p0-cgo|1541.083|13.010|3|0.000600|
|b000-read1000-p1-new|7802.216|64.095|7|0.001400|
|b000-read1000-p2-old|7971.686|63.187|6|0.001200|
|b001-read1000-p0-old|7959.850|63.163|6|0.001200|
|b001-read1000-p1-cgo|1542.176|13.011|3|0.000600|
|b001-read1000-p2-new|7795.139|64.087|6|0.001200|
|b002-read1000-p0-cgo|1542.154|13.011|3|0.000600|
|b002-read1000-p1-old|7962.926|63.178|6|0.001200|
|b002-read1000-p2-new|7802.283|64.098|6|0.001200|
|b003-read1000-p0-new|7798.102|64.088|6|0.001200|
|b003-read1000-p1-cgo|1542.205|13.012|3|0.000600|
|b003-read1000-p2-old|7975.478|63.237|6|0.001200|
|b004-read1000-p0-old|7966.362|63.196|6|0.001200|
|b004-read1000-p1-new|7796.080|64.077|6|0.001200|
|b004-read1000-p2-cgo|1542.179|13.012|3|0.000600|
|b005-read1000-p0-new|7798.466|64.079|6|0.001200|
|b005-read1000-p1-old|7969.555|63.183|6|0.001200|
|b005-read1000-p2-cgo|1542.198|13.012|3|0.000600|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.768017|[0.731416,0.796344]|37|36|
|new/cgo|0.951806|[0.890268,1.015928]|37|18|
|old/cgo|1.239303|[1.189175,1.286782]|36|18|
