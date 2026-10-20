# R10 PR5 direct idle writer — read1000

Old=9377c0863aef7ecdee250cdc323e40ebf69d5e46;new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+patch4a413f5d64d829b87e681bd9202a5bfa1fd48c1e74b5c3c68ec4ea88e3ef6063 (PR1/PR2/PR3/PR4 NOT stacked);old9377 code matches590145 in pkg/fdbgo/tools/dependency pins;CGo=immutable native7.3.77. FrozenV6,5000 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|592.045|893.213|396.534|169.879|226.655|1542.187|13.011|18|0.000600|
|old|450.760|647.071|473.155|183.821|292.855|7973.612|63.214|36|0.001200|
|new|434.873|634.885|430.301|160.739|266.714|7773.714|62.102|36|0.001200|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|0.967574|0.970464|0.912081|0.894070|0.923655|6/6|
|new/cgo|0.725834|0.725622|1.073458|0.928205|1.183855|0/6|
|old/cgo|0.750159|0.747706|1.176933|1.038179|1.281708|0/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=0.974906 (below1 in6/6), go_mallocs_per_op=0.982457 (below1 in6/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed790707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.967574|[0.939954, 0.996654]|5/6|
|new/old|p99_ns|0.970464|[0.872766, 1.095080]|3/6|
|new/old|process_cpu_ns_per_op|0.912081|[0.895684, 0.932058]|6/6|
|new/old|user_cpu_ns_per_op|0.894070|[0.853837, 0.949661]|5/6|
|new/old|system_cpu_ns_per_op|0.923655|[0.907430, 0.941467]|6/6|
|new/cgo|mean_ns|0.725834|[0.710670, 0.741028]|6/6|
|new/cgo|p99_ns|0.725622|[0.698089, 0.762904]|6/6|
|new/cgo|process_cpu_ns_per_op|1.073458|[1.041504, 1.105970]|0/6|
|new/cgo|user_cpu_ns_per_op|0.928205|[0.897709, 0.959507]|6/6|
|new/cgo|system_cpu_ns_per_op|1.183855|[1.139867, 1.228945]|0/6|
|old/cgo|mean_ns|0.750159|[0.733202, 0.766464]|6/6|
|old/cgo|p99_ns|0.747706|[0.693007, 0.814419]|6/6|
|old/cgo|process_cpu_ns_per_op|1.176933|[1.145235, 1.211906]|0/6|
|old/cgo|user_cpu_ns_per_op|1.038179|[0.986499, 1.085419]|2/6|
|old/cgo|system_cpu_ns_per_op|1.281708|[1.224946, 1.347325]|0/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|0.972241|1.038609|0.897322|0.896676|0.897733|
|1|new/old|0.911155|0.912395|0.884801|0.842970|0.909875|
|2|new/old|0.979743|0.828274|0.934445|0.894288|0.960289|
|3|new/old|0.967668|0.829018|0.903788|0.881749|0.917208|
|4|new/old|0.949814|1.249607|0.901801|0.833191|0.947307|
|5|new/old|1.028639|1.027375|0.952084|1.028526|0.911103|
|0|new/cgo|0.749281|0.718617|1.123404|0.946961|1.274064|
|1|new/cgo|0.703726|0.690613|1.059131|0.878552|1.195608|
|2|new/cgo|0.747427|0.684336|1.118625|0.984068|1.218478|
|3|new/cgo|0.702790|0.726990|1.013066|0.879816|1.111623|
|4|new/cgo|0.712469|0.817560|1.035941|0.926373|1.112715|
|5|new/cgo|0.740996|0.723122|1.095389|0.958430|1.199092|
|0|old/cgo|0.770674|0.691904|1.251951|1.056080|1.419201|
|1|old/cgo|0.772345|0.756923|1.197027|1.042210|1.314034|
|2|old/cgo|0.762881|0.826219|1.197100|1.100393|1.268866|
|3|old/cgo|0.726272|0.876929|1.120912|0.997809|1.211964|
|4|old/cgo|0.750114|0.654254|1.148746|1.111838|1.174608|
|5|old/cgo|0.720365|0.703854|1.150517|0.931848|1.316088|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-read1000-p0-cgo|1542.202|13.012|3|0.000600|
|b000-read1000-p1-new|7769.962|62.099|6|0.001200|
|b000-read1000-p2-old|7975.547|63.205|6|0.001200|
|b001-read1000-p0-old|7972.578|63.197|6|0.001200|
|b001-read1000-p1-cgo|1542.176|13.011|3|0.000600|
|b001-read1000-p2-new|7766.418|62.096|6|0.001200|
|b002-read1000-p0-cgo|1542.198|13.012|3|0.000600|
|b002-read1000-p1-old|7974.646|63.222|6|0.001200|
|b002-read1000-p2-new|7772.165|62.094|6|0.001200|
|b003-read1000-p0-new|7775.264|62.105|6|0.001200|
|b003-read1000-p1-old|7981.933|63.250|6|0.001200|
|b003-read1000-p2-cgo|1542.176|13.011|3|0.000600|
|b004-read1000-p0-old|7968.699|63.176|6|0.001200|
|b004-read1000-p1-new|7784.150|62.115|6|0.001200|
|b004-read1000-p2-cgo|1542.198|13.012|3|0.000600|
|b005-read1000-p0-new|7776.888|62.115|6|0.001200|
|b005-read1000-p1-cgo|1541.064|13.010|3|0.000600|
|b005-read1000-p2-old|7972.075|63.227|6|0.001200|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.

## Frozen-window CPU limitation (review disclosure)

CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. The Apple Go binding releases native transactions/futures through Go GC finalizers (7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work (bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.

|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|
|---|---:|---|---:|---:|
|new/old|0.912081|[0.895684,0.932058]|36|36|
|new/cgo|1.073458|[1.041504,1.105970]|36|18|
|old/cgo|1.176933|[1.145235,1.211906]|36|18|
