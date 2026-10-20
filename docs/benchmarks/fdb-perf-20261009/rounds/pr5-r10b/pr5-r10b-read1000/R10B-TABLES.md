# R10B PR5v2 nonblocking direct idle writer — read1000

Old=8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead;new=master8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead+patch8199a1c86c4df4ea2a877cd39355376d07ccebb3847dc2f4e4d74eb7e882eb54 (PR1/PR2/PR3/PR4 NOT stacked);OLD freshly rebuilt from the exact base; frozenV6 unchanged;CGo=immutable native7.3.77. FrozenV6,5000 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|586.248|872.879|392.386|159.935|232.451|1542.165|13.011|18|0.000600|
|old|439.942|635.676|463.079|177.241|281.569|7971.312|63.214|36|0.001200|
|new|420.079|633.671|414.947|165.798|252.570|7851.137|65.107|36|0.001200|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|0.961367|0.992697|0.908048|0.920945|0.899189|6/6|
|new/cgo|0.721373|0.720495|1.068004|1.006731|1.110859|1/6|
|old/cgo|0.750361|0.725795|1.176154|1.093150|1.235400|0/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=0.984930 (below1 in6/6), go_mallocs_per_op=1.030098 (below1 in0/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed790907, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.961367|[0.951374, 0.973119]|6/6|
|new/old|p99_ns|0.992697|[0.929304, 1.048039]|2/6|
|new/old|process_cpu_ns_per_op|0.908048|[0.891231, 0.925246]|6/6|
|new/old|user_cpu_ns_per_op|0.920945|[0.878856, 0.967515]|5/6|
|new/old|system_cpu_ns_per_op|0.899189|[0.861942, 0.934890]|6/6|
|new/cgo|mean_ns|0.721373|[0.687446, 0.761496]|6/6|
|new/cgo|p99_ns|0.720495|[0.665394, 0.783499]|6/6|
|new/cgo|process_cpu_ns_per_op|1.068004|[1.002081, 1.138489]|1/6|
|new/cgo|user_cpu_ns_per_op|1.006731|[0.921593, 1.094793]|2/6|
|new/cgo|system_cpu_ns_per_op|1.110859|[1.042643, 1.186649]|0/6|
|old/cgo|mean_ns|0.750361|[0.716524, 0.788799]|6/6|
|old/cgo|p99_ns|0.725795|[0.683651, 0.770098]|6/6|
|old/cgo|process_cpu_ns_per_op|1.176154|[1.096730, 1.267415]|0/6|
|old/cgo|user_cpu_ns_per_op|1.093150|[1.025173, 1.173057]|2/6|
|old/cgo|system_cpu_ns_per_op|1.235400|[1.147230, 1.340206]|0/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|0.952510|0.943360|0.896385|0.865915|0.916103|
|1|new/old|0.944163|1.045770|0.917136|1.003120|0.865022|
|2|new/old|0.984706|1.074488|0.884708|0.853688|0.905885|
|3|new/old|0.972385|0.858591|0.939532|0.878943|0.977616|
|4|new/old|0.962479|1.031514|0.882485|0.973830|0.828898|
|5|new/old|0.952538|1.019351|0.929623|0.961241|0.908641|
|0|new/cgo|0.668299|0.617590|0.950943|0.857997|1.018428|
|1|new/cgo|0.760111|0.809677|1.184180|1.153055|1.207082|
|2|new/cgo|0.807613|0.833078|1.185607|1.076152|1.268618|
|3|new/cgo|0.687990|0.679558|1.007376|0.874432|1.102062|
|4|new/cgo|0.715425|0.677620|1.072646|1.084848|1.064395|
|5|new/cgo|0.697847|0.729247|1.028672|1.030807|1.027179|
|0|old/cgo|0.701619|0.654670|1.060865|0.990857|1.111695|
|1|old/cgo|0.805064|0.774240|1.291172|1.149468|1.395435|
|2|old/cgo|0.820157|0.775325|1.340110|1.260591|1.400418|
|3|old/cgo|0.707528|0.791480|1.072210|0.994868|1.127295|
|4|old/cgo|0.743315|0.656918|1.215483|1.114001|1.284108|
|5|old/cgo|0.732618|0.715404|1.106548|1.072371|1.130456|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-read1000-p0-old|7974.728|63.214|6|0.001200|
|b000-read1000-p1-cgo|1541.061|13.010|3|0.000600|
|b000-read1000-p2-new|7842.720|65.087|6|0.001200|
|b001-read1000-p0-cgo|1542.154|13.011|3|0.000600|
|b001-read1000-p1-new|7853.510|65.106|6|0.001200|
|b001-read1000-p2-old|7978.114|63.227|6|0.001200|
|b002-read1000-p0-cgo|1542.179|13.012|3|0.000600|
|b002-read1000-p1-old|7970.982|63.214|6|0.001200|
|b002-read1000-p2-new|7849.070|65.091|6|0.001200|
|b003-read1000-p0-old|7958.883|63.178|6|0.001200|
|b003-read1000-p1-new|7848.187|65.109|6|0.001200|
|b003-read1000-p2-cgo|1542.221|13.012|3|0.000600|
|b004-read1000-p0-new|7855.882|65.115|6|0.001200|
|b004-read1000-p1-old|7971.642|63.216|6|0.001200|
|b004-read1000-p2-cgo|1542.176|13.011|3|0.000600|
|b005-read1000-p0-new|7853.203|65.113|6|0.001200|
|b005-read1000-p1-cgo|1541.083|13.010|3|0.000600|
|b005-read1000-p2-old|7968.934|63.158|6|0.001200|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.908048|[0.891231,0.925246]|36|36|
|new/cgo|1.068004|[1.002081,1.138489]|36|18|
|old/cgo|1.176154|[1.096730,1.267415]|36|18|
