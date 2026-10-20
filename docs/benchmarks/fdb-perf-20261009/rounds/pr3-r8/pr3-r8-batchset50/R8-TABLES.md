# R8 PR3 buffered readLoop — batchset50

Old=9377c0863aef7ecdee250cdc323e40ebf69d5e46;new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+patchf02c03072178a7a1bfd4703e58a7401afdfcba1ccee3273ed5f721306d0c2128 (PR1/PR2 NOT stacked);old9377 code matches590145 in pkg/fdbgo/tools/dependency pins;CGo=immutable native7.3.77. FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2059.369|3860.198|831.965|466.970|360.834|1616.118|59.001|0|0.000000|
|old|2095.948|3730.364|889.866|495.820|392.887|80404.581|299.595|48|0.003200|
|new|1997.083|3483.171|879.212|488.011|378.088|80486.237|299.622|48|0.003200|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|0.942714|0.930274|0.981686|0.992083|0.968116|5/6|
|new/cgo|0.944841|0.917028|1.033915|1.047367|1.018715|2/6|
|old/cgo|1.002257|0.985761|1.053204|1.055726|1.052266|1/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=1.000667 (below1 in1/6), go_mallocs_per_op=1.000156 (below1 in1/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed788707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.942714|[0.892021, 0.992356]|5/6|
|new/old|p99_ns|0.930274|[0.831315, 1.034562]|5/6|
|new/old|process_cpu_ns_per_op|0.981686|[0.961281, 0.999728]|5/6|
|new/old|user_cpu_ns_per_op|0.992083|[0.965207, 1.018417]|3/6|
|new/old|system_cpu_ns_per_op|0.968116|[0.934516, 1.003237]|4/6|
|new/cgo|mean_ns|0.944841|[0.911005, 0.979458]|5/6|
|new/cgo|p99_ns|0.917028|[0.842446, 1.006744]|4/6|
|new/cgo|process_cpu_ns_per_op|1.033915|[0.997817, 1.069619]|2/6|
|new/cgo|user_cpu_ns_per_op|1.047367|[1.021764, 1.077920]|1/6|
|new/cgo|system_cpu_ns_per_op|1.018715|[0.965727, 1.073754]|3/6|
|old/cgo|mean_ns|1.002257|[0.928456, 1.076770]|2/6|
|old/cgo|p99_ns|0.985761|[0.852050, 1.111865]|3/6|
|old/cgo|process_cpu_ns_per_op|1.053204|[1.013897, 1.089986]|1/6|
|old/cgo|user_cpu_ns_per_op|1.055726|[1.027363, 1.083648]|0/6|
|old/cgo|system_cpu_ns_per_op|1.052266|[0.978094, 1.130711]|2/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|0.842941|0.734917|1.018051|1.034975|0.996233|
|1|new/old|0.989308|0.998027|0.981889|1.028845|0.924041|
|2|new/old|0.889017|0.875532|0.938749|0.956411|0.917050|
|3|new/old|0.960552|0.951625|0.976586|1.005564|0.940443|
|4|new/old|0.956349|0.917142|0.983424|0.944993|1.033453|
|5|new/old|1.030630|1.156410|0.993124|0.985200|1.003450|
|0|new/cgo|0.978144|0.880732|1.075138|1.042694|1.121888|
|1|new/cgo|1.014082|1.119379|1.099090|1.114439|1.078710|
|2|new/cgo|0.915463|0.819706|1.016992|1.040915|0.987901|
|3|new/cgo|0.961674|1.008904|1.054541|1.052444|1.057352|
|4|new/cgo|0.925338|0.895705|0.996542|1.037709|0.951605|
|5|new/cgo|0.880455|0.814334|0.967243|0.999289|0.929122|
|0|old/cgo|1.160394|1.198409|1.056074|1.007458|1.126129|
|1|old/cgo|1.025042|1.121593|1.119363|1.083195|1.167383|
|2|old/cgo|1.029747|0.936239|1.083348|1.088355|1.077259|
|3|old/cgo|1.001168|1.060191|1.079825|1.046620|1.124313|
|4|old/cgo|0.967574|0.976626|1.013340|1.098113|0.920802|
|5|old/cgo|0.854289|0.704191|0.973940|1.014300|0.925928|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50-p0-old|80368.806|299.584|8|0.003200|
|b000-batchset50-p1-new|80461.235|299.622|8|0.003200|
|b000-batchset50-p2-cgo|1616.186|59.002|0|0.000000|
|b001-batchset50-p0-new|80365.450|299.579|8|0.003200|
|b001-batchset50-p1-old|80435.088|299.606|8|0.003200|
|b001-batchset50-p2-cgo|1616.045|59.000|0|0.000000|
|b002-batchset50-p0-cgo|1616.186|59.002|0|0.000000|
|b002-batchset50-p1-old|80408.211|299.625|8|0.003200|
|b002-batchset50-p2-new|80528.010|299.674|8|0.003200|
|b003-batchset50-p0-new|80533.725|299.660|8|0.003200|
|b003-batchset50-p1-cgo|1616.141|59.001|0|0.000000|
|b003-batchset50-p2-old|80512.634|299.656|8|0.003200|
|b004-batchset50-p0-cgo|1616.058|59.000|0|0.000000|
|b004-batchset50-p1-new|80511.238|299.623|8|0.003200|
|b004-batchset50-p2-old|80400.950|299.503|8|0.003200|
|b005-batchset50-p0-old|80373.878|299.485|8|0.003200|
|b005-batchset50-p1-cgo|1616.096|59.001|0|0.000000|
|b005-batchset50-p2-new|80421.958|299.582|8|0.003200|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.
