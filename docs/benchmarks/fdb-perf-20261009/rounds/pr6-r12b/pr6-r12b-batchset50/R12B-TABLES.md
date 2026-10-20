# R12B PR6 missing-read-version commit GRV uses CAUSAL_READ_RISKY, matching C++; no other optimization stacked — batchset50

Exact base 8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead; OLD freshly rebuilt as candidate-pr5-r10b-old. NEW=PR6 missing-read-version commit GRV uses CAUSAL_READ_RISKY, matching C++; no other optimization stacked; patchSHA256=07912ec16db4bb91a776a914aa772108d2d0329f1bdc2fd22bfba4fbbfcd55ce. FrozenV6 unchanged. CPU is window-only, NOT lifecycle-complete. Counts fixed before measurement. Independent preregistered R12b repeat; report separately and pooled with original R12, never overwrite original evidence.
FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2092.958|3696.590|845.095|455.949|390.974|1616.099|59.001|0|0.000000|
|old|1974.590|3745.394|896.867|487.750|404.695|80440.627|299.618|48|0.003200|
|new|2032.932|3709.330|880.404|492.206|392.301|80454.699|299.644|48|0.003200|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|1.035822|0.975961|0.998885|1.016274|0.977144|4/6|
|new/cgo|0.975216|0.968062|1.055157|1.088975|1.016299|0/6|
|old/cgo|0.941490|0.991906|1.056334|1.071537|1.040071|0/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=1.000099 (below1 in2/6), go_mallocs_per_op=1.000044 (below1 in3/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed794707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|1.035822|[0.986363, 1.091378]|1/6|
|new/old|p99_ns|0.975961|[0.898441, 1.055591]|3/6|
|new/old|process_cpu_ns_per_op|0.998885|[0.973924, 1.027450]|4/6|
|new/old|user_cpu_ns_per_op|1.016274|[0.983020, 1.064266]|3/6|
|new/old|system_cpu_ns_per_op|0.977144|[0.922174, 1.050259]|4/6|
|new/cgo|mean_ns|0.975216|[0.916322, 1.038355]|5/6|
|new/cgo|p99_ns|0.968062|[0.888633, 1.053744]|4/6|
|new/cgo|process_cpu_ns_per_op|1.055157|[1.042059, 1.069766]|0/6|
|new/cgo|user_cpu_ns_per_op|1.088975|[1.027271, 1.154620]|1/6|
|new/cgo|system_cpu_ns_per_op|1.016299|[0.952735, 1.080784]|3/6|
|old/cgo|mean_ns|0.941490|[0.920528, 0.961237]|6/6|
|old/cgo|p99_ns|0.991906|[0.944583, 1.044319]|4/6|
|old/cgo|process_cpu_ns_per_op|1.056334|[1.033515, 1.074965]|0/6|
|old/cgo|user_cpu_ns_per_op|1.071537|[1.039066, 1.108410]|0/6|
|old/cgo|system_cpu_ns_per_op|1.040071|[1.003063, 1.082168]|1/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|1.150739|0.996092|1.033964|1.128138|0.928216|
|1|new/old|1.004202|1.014538|0.964313|1.015635|0.901787|
|2|new/old|1.033484|1.156557|0.979151|0.999586|0.954344|
|3|new/old|1.012047|1.002423|0.990290|0.961048|1.026444|
|4|new/old|0.941663|0.840001|0.970447|1.011147|0.923450|
|5|new/old|1.085207|0.878071|1.058733|0.989890|1.149595|
|0|new/cgo|1.109277|0.949686|1.082575|1.206103|0.949812|
|1|new/cgo|0.969338|1.107152|1.041550|1.166339|0.908218|
|2|new/cgo|0.991677|1.118479|1.061115|1.049218|1.076637|
|3|new/cgo|0.959658|0.909148|1.032373|0.975835|1.106589|
|4|new/cgo|0.860200|0.825148|1.043843|1.121517|0.959797|
|5|new/cgo|0.977245|0.932906|1.070362|1.032406|1.117034|
|0|old/cgo|0.963970|0.953412|1.047014|1.069110|1.023266|
|1|old/cgo|0.965282|1.091287|1.080095|1.148385|1.007131|
|2|old/cgo|0.959547|0.967077|1.083709|1.049653|1.128144|
|3|old/cgo|0.948234|0.906950|1.042495|1.015387|1.078081|
|4|old/cgo|0.913490|0.982318|1.075631|1.109153|1.039360|
|5|old/cgo|0.900515|1.062449|1.010984|1.042950|0.971677|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50-p0-new|80488.922|299.694|8|0.003200|
|b000-batchset50-p1-old|80398.208|299.579|8|0.003200|
|b000-batchset50-p2-cgo|1616.141|59.001|0|0.000000|
|b001-batchset50-p0-cgo|1616.102|59.001|0|0.000000|
|b001-batchset50-p1-new|80442.915|299.635|8|0.003200|
|b001-batchset50-p2-old|80420.755|299.552|8|0.003200|
|b002-batchset50-p0-old|80477.600|299.680|8|0.003200|
|b002-batchset50-p1-cgo|1616.096|59.001|0|0.000000|
|b002-batchset50-p2-new|80360.154|299.653|8|0.003200|
|b003-batchset50-p0-new|80466.483|299.551|8|0.003200|
|b003-batchset50-p1-cgo|1616.096|59.001|0|0.000000|
|b003-batchset50-p2-old|80460.499|299.656|8|0.003200|
|b004-batchset50-p0-cgo|1616.230|59.002|0|0.000000|
|b004-batchset50-p1-old|80354.643|299.581|8|0.003200|
|b004-batchset50-p2-new|80426.157|299.600|8|0.003200|
|b005-batchset50-p0-old|80553.424|299.674|8|0.003200|
|b005-batchset50-p1-new|80528.224|299.668|8|0.003200|
|b005-batchset50-p2-cgo|1616.051|59.000|0|0.000000|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.998885|[0.973924,1.027450]|48|48|
|new/cgo|1.055157|[1.042059,1.069766]|48|0|
|old/cgo|1.056334|[1.033515,1.074965]|48|0|

OLD base8ab5c5a4d omits CAUSAL_READ_RISKY on write-only commit GRVs; NEW explicitly adds the C++ flag when no read version exists. This is the PR6 intervention, not an unstated algorithmic-equivalence assumption. C++ NativeAPI.actor.cpp:6577-6578; GrvProxyServer.actor.cpp:647-648.
