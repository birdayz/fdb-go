# R12 PR6 missing-read-version commit GRV uses CAUSAL_READ_RISKY, matching C++; no other optimization stacked — set100

Exact base 8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead; OLD freshly rebuilt as candidate-pr5-r10b-old. NEW=PR6 missing-read-version commit GRV uses CAUSAL_READ_RISKY, matching C++; no other optimization stacked; patchSHA256=07912ec16db4bb91a776a914aa772108d2d0329f1bdc2fd22bfba4fbbfcd55ce. FrozenV6 unchanged. CPU is window-only, NOT lifecycle-complete. Counts fixed before measurement.
FrozenV6,3000 measured+200warmup/run,workers1,all6three-armorders.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2017.604|3333.917|548.673|233.717|316.304|440.173|10.002|0|0.000000|
|old|1719.797|2929.550|567.137|247.055|315.198|7422.596|79.351|41|0.002278|
|new|1705.886|3084.419|559.982|237.270|318.925|7436.067|79.379|42|0.002333|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|1.031209|1.071252|0.999300|0.975072|1.018431|4/6|
|new/cgo|0.845044|0.871088|1.009538|1.006879|1.012819|2/6|
|old/cgo|0.819469|0.813150|1.010245|1.032620|0.994489|3/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=1.001856 (below1 in1/6), go_mallocs_per_op=1.000335 (below1 in0/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed792707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|1.031209|[0.936251, 1.149288]|3/6|
|new/old|p99_ns|1.071252|[0.959354, 1.216619]|2/6|
|new/old|process_cpu_ns_per_op|0.999300|[0.979792, 1.020428]|4/6|
|new/old|user_cpu_ns_per_op|0.975072|[0.938009, 1.012076]|4/6|
|new/old|system_cpu_ns_per_op|1.018431|[1.005061, 1.034276]|1/6|
|new/cgo|mean_ns|0.845044|[0.784286, 0.909007]|6/6|
|new/cgo|p99_ns|0.871088|[0.741095, 1.023552]|4/6|
|new/cgo|process_cpu_ns_per_op|1.009538|[0.983755, 1.034961]|2/6|
|new/cgo|user_cpu_ns_per_op|1.006879|[0.952137, 1.065231]|4/6|
|new/cgo|system_cpu_ns_per_op|1.012819|[0.985058, 1.045722]|2/6|
|old/cgo|mean_ns|0.819469|[0.744100, 0.888895]|6/6|
|old/cgo|p99_ns|0.813150|[0.713564, 0.930745]|5/6|
|old/cgo|process_cpu_ns_per_op|1.010245|[0.991329, 1.036490]|3/6|
|old/cgo|user_cpu_ns_per_op|1.032620|[1.004443, 1.063491]|2/6|
|old/cgo|system_cpu_ns_per_op|0.994489|[0.956070, 1.038997]|4/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|1.301398|0.975924|1.029857|1.003881|1.050445|
|1|new/old|1.098092|1.041797|1.037344|1.048637|1.028799|
|2|new/old|1.023206|1.041779|0.983261|0.965210|0.997786|
|3|new/old|0.998656|1.411921|0.979214|0.946802|1.004134|
|4|new/old|0.965170|1.160139|0.998801|0.991096|1.004431|
|5|new/old|0.853197|0.871073|0.969282|0.901399|1.025977|
|0|new/cgo|0.863645|0.678056|1.015112|1.096714|0.960955|
|1|new/cgo|0.937765|1.053825|1.048908|1.111429|1.005288|
|2|new/cgo|0.836348|0.679374|1.000860|0.963500|1.032007|
|3|new/cgo|0.947106|1.150513|1.048673|0.998277|1.088508|
|4|new/cgo|0.765330|0.901296|0.986175|0.978975|0.991432|
|5|new/cgo|0.741675|0.867903|0.960545|0.907851|1.003275|
|0|old/cgo|0.663629|0.694784|0.985682|1.092474|0.914808|
|1|old/cgo|0.853995|1.011545|1.011148|1.059880|0.977148|
|2|old/cgo|0.817380|0.652128|1.017898|0.998228|1.034297|
|3|old/cgo|0.948381|0.814857|1.070933|1.054367|1.084027|
|4|old/cgo|0.792948|0.776886|0.987359|0.987771|0.987058|
|5|old/cgo|0.869289|0.996361|0.990986|1.007158|0.977873|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-set100-p0-cgo|440.155|10.001|0|0.000000|
|b000-set100-p1-old|7439.528|79.380|7|0.002333|
|b000-set100-p2-new|7446.245|79.390|7|0.002333|
|b001-set100-p0-old|7421.632|79.353|7|0.002333|
|b001-set100-p1-cgo|441.981|10.003|0|0.000000|
|b001-set100-p2-new|7435.613|79.392|7|0.002333|
|b002-set100-p0-old|7431.709|79.371|7|0.002333|
|b002-set100-p1-new|7431.405|79.403|7|0.002333|
|b002-set100-p2-cgo|443.760|10.005|0|0.000000|
|b003-set100-p0-new|7435.107|79.356|7|0.002333|
|b003-set100-p1-cgo|440.192|10.002|0|0.000000|
|b003-set100-p2-old|7406.533|79.323|7|0.002333|
|b004-set100-p0-new|7436.520|79.357|7|0.002333|
|b004-set100-p1-old|7419.269|79.331|6|0.002000|
|b004-set100-p2-cgo|440.123|10.001|0|0.000000|
|b005-set100-p0-cgo|440.080|10.001|0|0.000000|
|b005-set100-p1-new|7439.965|79.368|7|0.002333|
|b005-set100-p2-old|7423.560|79.349|7|0.002333|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.999300|[0.979792,1.020428]|42|41|
|new/cgo|1.009538|[0.983755,1.034961]|42|0|
|old/cgo|1.010245|[0.991329,1.036490]|41|0|

OLD base8ab5c5a4d omits CAUSAL_READ_RISKY on write-only commit GRVs; NEW explicitly adds the C++ flag when no read version exists. This is the PR6 intervention, not an unstated algorithmic-equivalence assumption. C++ NativeAPI.actor.cpp:6577-6578; GrvProxyServer.actor.cpp:647-648.
