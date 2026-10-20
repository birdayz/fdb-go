# R13 PR7 executionLease kept on the state-call stack; no PR3/PR4/PR5/PR6 stacked — batchset50

Exact base 8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead; OLD freshly rebuilt as candidate-pr5-r10b-old. NEW=PR7 executionLease kept on the state-call stack; no PR3/PR4/PR5/PR6 stacked; patchSHA256=e3d3b651ce8cb613b9383a6a4645276b6f663be3d90f79643c6f050e52c26b1b. FrozenV6 unchanged. CPU is window-only, NOT lifecycle-complete. Counts fixed before measurement.
FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2078.459|4053.007|827.842|444.197|382.846|1616.118|59.001|0|0.000000|
|old|2093.258|4005.068|891.835|490.558|389.863|80444.874|299.618|49|0.003267|
|new|2086.031|3844.374|890.206|487.730|405.206|79156.221|245.590|49|0.003267|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|1.019012|0.984390|1.002689|0.986874|1.023056|2/6|
|new/cgo|1.009282|0.941266|1.071124|1.091327|1.047884|0/6|
|old/cgo|0.990451|0.956192|1.068251|1.105842|1.024268|0/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=0.983769 (below1 in6/6), go_mallocs_per_op=0.819644 (below1 in6/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed793707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|1.019012|[0.968937, 1.068402]|3/6|
|new/old|p99_ns|0.984390|[0.949404, 1.026353]|4/6|
|new/old|process_cpu_ns_per_op|1.002689|[0.974611, 1.033575]|2/6|
|new/old|user_cpu_ns_per_op|0.986874|[0.933262, 1.041486]|3/6|
|new/old|system_cpu_ns_per_op|1.023056|[0.995263, 1.053803]|2/6|
|new/cgo|mean_ns|1.009282|[0.929058, 1.091282]|3/6|
|new/cgo|p99_ns|0.941266|[0.842941, 1.022818]|4/6|
|new/cgo|process_cpu_ns_per_op|1.071124|[1.042506, 1.096844]|0/6|
|new/cgo|user_cpu_ns_per_op|1.091327|[1.041992, 1.140429]|0/6|
|new/cgo|system_cpu_ns_per_op|1.047884|[0.966767, 1.121392]|1/6|
|old/cgo|mean_ns|0.990451|[0.918746, 1.062519]|3/6|
|old/cgo|p99_ns|0.956192|[0.863337, 1.018066]|4/6|
|old/cgo|process_cpu_ns_per_op|1.068251|[1.037969, 1.098303]|0/6|
|old/cgo|user_cpu_ns_per_op|1.105842|[1.069311, 1.152839]|0/6|
|old/cgo|system_cpu_ns_per_op|1.024268|[0.961938, 1.094543]|2/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|1.080276|1.077889|1.071016|1.099067|1.037612|
|1|new/old|0.938992|0.933925|0.951793|0.913937|1.003467|
|2|new/old|1.096368|1.017957|1.002999|1.016423|0.988667|
|3|new/old|0.999039|0.981789|1.002129|1.021635|0.977225|
|4|new/old|1.061311|0.945571|0.973479|0.893872|1.082252|
|5|new/old|0.949507|0.956482|1.018848|0.990794|1.053139|
|0|new/cgo|1.176260|1.061546|1.105598|1.195955|1.009407|
|1|new/cgo|1.028020|0.921774|1.065717|1.108902|1.016502|
|2|new/cgo|1.063229|1.053191|1.103867|1.059190|1.157459|
|3|new/cgo|0.842285|0.730931|1.010223|1.131676|0.883646|
|4|new/cgo|0.984583|0.975676|1.046129|1.001412|1.101645|
|5|new/cgo|0.991363|0.946288|1.098703|1.061238|1.145193|
|0|old/cgo|1.088851|0.984838|1.032290|1.088155|0.972817|
|1|old/cgo|1.094812|0.986989|1.119695|1.213324|1.012990|
|2|old/cgo|0.969774|1.034613|1.100566|1.042076|1.170727|
|3|old/cgo|0.843095|0.744489|1.008077|1.107710|0.904241|
|4|old/cgo|0.927704|1.031838|1.074629|1.120307|1.017920|
|5|old/cgo|1.044082|0.989342|1.078378|1.071099|1.087410|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50-p0-new|79178.496|245.571|8|0.003200|
|b000-batchset50-p1-cgo|1616.230|59.002|0|0.000000|
|b000-batchset50-p2-old|80559.296|299.644|8|0.003200|
|b001-batchset50-p0-cgo|1616.179|59.002|0|0.000000|
|b001-batchset50-p1-new|79092.310|245.604|8|0.003200|
|b001-batchset50-p2-old|80494.509|299.694|8|0.003200|
|b002-batchset50-p0-old|80343.328|299.568|8|0.003200|
|b002-batchset50-p1-new|79083.386|245.578|8|0.003200|
|b002-batchset50-p2-cgo|1616.275|59.002|0|0.000000|
|b003-batchset50-p0-cgo|1616.051|59.000|0|0.000000|
|b003-batchset50-p1-old|80395.238|299.573|8|0.003200|
|b003-batchset50-p2-new|79190.006|245.602|8|0.003200|
|b004-batchset50-p0-old|80599.261|299.709|9|0.003600|
|b004-batchset50-p1-cgo|1616.058|59.000|0|0.000000|
|b004-batchset50-p2-new|79133.946|245.546|8|0.003200|
|b005-batchset50-p0-new|79191.283|245.639|9|0.003600|
|b005-batchset50-p1-old|80313.155|299.592|8|0.003200|
|b005-batchset50-p2-cgo|1616.006|59.000|0|0.000000|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|1.002689|[0.974611,1.033575]|49|49|
|new/cgo|1.071124|[1.042506,1.096844]|49|0|
|old/cgo|1.068251|[1.037969,1.098303]|49|0|

This frozen base8ab5c5a4d Go write-only commit also omits the causal-read-risky GRV flag that C++ supplies (NativeAPI.actor.cpp:6577-6578). Go transaction.go:1980-1985 invokes ensureReadVersion; grvFlags at:3398-3411 only adds this flag on an explicit option. Without the flag the server waits for updateLastCommit (GrvProxyServer.actor.cpp:647-648). This pre-existing implementation difference is measured, not corrected or quietly stacked into this campaign.
