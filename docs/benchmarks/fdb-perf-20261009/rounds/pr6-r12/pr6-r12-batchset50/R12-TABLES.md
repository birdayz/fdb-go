# R12 PR6 missing-read-version commit GRV uses CAUSAL_READ_RISKY, matching C++; no other optimization stacked — batchset50

Exact base 8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead; OLD freshly rebuilt as candidate-pr5-r10b-old. NEW=PR6 missing-read-version commit GRV uses CAUSAL_READ_RISKY, matching C++; no other optimization stacked; patchSHA256=07912ec16db4bb91a776a914aa772108d2d0329f1bdc2fd22bfba4fbbfcd55ce. FrozenV6 unchanged. CPU is window-only, NOT lifecycle-complete. Counts fixed before measurement.
FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2113.588|3841.608|853.915|448.538|400.982|1616.115|59.001|0|0.000000|
|old|1938.334|3890.653|882.362|485.839|396.257|80377.069|299.560|48|0.003200|
|new|2091.701|4100.957|897.760|499.185|398.037|80423.328|299.608|49|0.003267|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|1.064660|1.058541|1.021180|1.033439|1.005600|0/6|
|new/cgo|0.950090|0.987899|1.052337|1.113693|0.985239|1/6|
|old/cgo|0.892388|0.933265|1.030511|1.077658|0.979752|1/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=1.000438 (below1 in3/6), go_mallocs_per_op=1.000092 (below1 in2/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed792707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|1.064660|[1.021527, 1.105247]|1/6|
|new/old|p99_ns|1.058541|[0.966506, 1.172359]|2/6|
|new/old|process_cpu_ns_per_op|1.021180|[1.010393, 1.033829]|0/6|
|new/old|user_cpu_ns_per_op|1.033439|[1.010163, 1.053810]|1/6|
|new/old|system_cpu_ns_per_op|1.005600|[0.985909, 1.025599]|3/6|
|new/cgo|mean_ns|0.950090|[0.859434, 1.015233]|3/6|
|new/cgo|p99_ns|0.987899|[0.876869, 1.118666]|3/6|
|new/cgo|process_cpu_ns_per_op|1.052337|[1.024253, 1.082550]|1/6|
|new/cgo|user_cpu_ns_per_op|1.113693|[1.068862, 1.150880]|0/6|
|new/cgo|system_cpu_ns_per_op|0.985239|[0.916840, 1.038701]|2/6|
|old/cgo|mean_ns|0.892388|[0.837236, 0.930698]|6/6|
|old/cgo|p99_ns|0.933265|[0.846314, 1.028701]|4/6|
|old/cgo|process_cpu_ns_per_op|1.030511|[1.007195, 1.050498]|1/6|
|old/cgo|user_cpu_ns_per_op|1.077658|[1.055553, 1.099440]|0/6|
|old/cgo|system_cpu_ns_per_op|0.979752|[0.920809, 1.025069]|3/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|0.982348|1.126816|1.020467|1.049782|0.983958|
|1|new/old|1.130647|0.985262|1.051236|1.072226|1.026705|
|2|new/old|1.044145|1.025574|1.007842|1.028744|0.981659|
|3|new/old|1.109465|1.024547|1.028093|1.019605|1.038583|
|4|new/old|1.099465|1.330205|1.014304|1.047222|0.974215|
|5|new/old|1.029483|0.906616|1.005825|0.985247|1.030561|
|0|new/cgo|0.751850|0.860338|0.999959|1.175341|0.834502|
|1|new/cgo|1.018825|1.023170|1.117762|1.155347|1.075078|
|2|new/cgo|0.943868|0.908967|1.058709|1.100269|1.008693|
|3|new/cgo|1.012691|1.135469|1.067589|1.125841|1.004526|
|4|new/cgo|1.036925|1.258190|1.052205|1.115456|0.979494|
|5|new/cgo|0.968773|0.813183|1.021685|1.016924|1.027212|
|0|old/cgo|0.765360|0.763512|0.979903|1.119605|0.848107|
|1|old/cgo|0.901099|1.038475|1.063284|1.077522|1.047115|
|2|old/cgo|0.903963|0.886300|1.050471|1.069527|1.027539|
|3|old/cgo|0.912775|1.108265|1.038416|1.104193|0.967209|
|4|old/cgo|0.943118|0.945862|1.037366|1.065157|1.005419|
|5|old/cgo|0.941029|0.896943|1.015769|1.032151|0.996750|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50-p0-cgo|1616.314|59.003|0|0.000000|
|b000-batchset50-p1-old|80320.947|299.575|8|0.003200|
|b000-batchset50-p2-new|80506.867|299.694|9|0.003600|
|b001-batchset50-p0-cgo|1616.141|59.001|0|0.000000|
|b001-batchset50-p1-new|80371.462|299.622|8|0.003200|
|b001-batchset50-p2-old|80387.968|299.536|8|0.003200|
|b002-batchset50-p0-old|80366.170|299.664|8|0.003200|
|b002-batchset50-p1-cgo|1616.051|59.000|0|0.000000|
|b002-batchset50-p2-new|80436.659|299.622|8|0.003200|
|b003-batchset50-p0-new|80225.933|299.424|8|0.003200|
|b003-batchset50-p1-old|80225.181|299.486|8|0.003200|
|b003-batchset50-p2-cgo|1616.006|59.000|0|0.000000|
|b004-batchset50-p0-old|80428.435|299.556|8|0.003200|
|b004-batchset50-p1-new|80409.997|299.595|8|0.003200|
|b004-batchset50-p2-cgo|1616.096|59.001|0|0.000000|
|b005-batchset50-p0-new|80436.826|299.590|8|0.003200|
|b005-batchset50-p1-cgo|1616.134|59.001|0|0.000000|
|b005-batchset50-p2-old|80447.744|299.564|8|0.003200|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|1.021180|[1.010393,1.033829]|49|48|
|new/cgo|1.052337|[1.024253,1.082550]|49|0|
|old/cgo|1.030511|[1.007195,1.050498]|48|0|

OLD base8ab5c5a4d omits CAUSAL_READ_RISKY on write-only commit GRVs; NEW explicitly adds the C++ flag when no read version exists. This is the PR6 intervention, not an unstated algorithmic-equivalence assumption. C++ NativeAPI.actor.cpp:6577-6578; GrvProxyServer.actor.cpp:647-648.
