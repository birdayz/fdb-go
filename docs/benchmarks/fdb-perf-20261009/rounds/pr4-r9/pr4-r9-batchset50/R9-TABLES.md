# R9 PR4 inline lone GRV — batchset50

Old=9377c0863aef7ecdee250cdc323e40ebf69d5e46;new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+patch2414b55ab15d20c8597ebc54917b67c24320c8f1b02e8ab6a412a0c1de3be014 (PR1/PR2/PR3 NOT stacked);old9377 code matches590145 in pkg/fdbgo/tools/dependency pins;CGo=immutable native7.3.77. FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|2205.156|4504.724|856.523|467.853|384.123|1616.141|59.001|0|0.000000|
|old|1914.603|3912.838|888.864|495.294|385.531|80428.502|299.603|48|0.003200|
|new|1947.206|3831.847|880.097|490.108|385.452|80481.715|298.612|50|0.003333|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|1.029174|1.020431|0.992056|0.982662|1.004039|3/6|
|new/cgo|0.893802|0.858061|1.018904|1.037027|0.997761|2/6|
|old/cgo|0.868465|0.840881|1.027062|1.055324|0.993747|2/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=1.000495 (below1 in3/6), go_mallocs_per_op=0.996662 (below1 in6/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed789707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|1.029174|[0.929763, 1.146332]|1/6|
|new/old|p99_ns|1.020431|[0.913466, 1.139282]|2/6|
|new/old|process_cpu_ns_per_op|0.992056|[0.955094, 1.034962]|3/6|
|new/old|user_cpu_ns_per_op|0.982662|[0.944286, 1.021354]|4/6|
|new/old|system_cpu_ns_per_op|1.004039|[0.951663, 1.058374]|3/6|
|new/cgo|mean_ns|0.893802|[0.814757, 1.002835]|5/6|
|new/cgo|p99_ns|0.858061|[0.766381, 0.982919]|5/6|
|new/cgo|process_cpu_ns_per_op|1.018904|[0.983912, 1.050467]|2/6|
|new/cgo|user_cpu_ns_per_op|1.037027|[1.013079, 1.061356]|2/6|
|new/cgo|system_cpu_ns_per_op|0.997761|[0.939642, 1.057845]|2/6|
|old/cgo|mean_ns|0.868465|[0.822966, 0.907196]|6/6|
|old/cgo|p99_ns|0.840881|[0.747622, 0.961420]|5/6|
|old/cgo|process_cpu_ns_per_op|1.027062|[0.993151, 1.063538]|2/6|
|old/cgo|user_cpu_ns_per_op|1.055324|[1.017078, 1.103596]|1/6|
|old/cgo|system_cpu_ns_per_op|0.993747|[0.945724, 1.041636]|3/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|1.297549|1.026087|1.083184|1.058845|1.114614|
|1|new/old|1.059784|1.022643|0.963192|0.975098|0.947977|
|2|new/old|1.012918|0.900990|1.017786|0.986171|1.058039|
|3|new/old|1.012381|1.184978|1.010397|1.020339|0.997508|
|4|new/old|0.840961|0.825855|0.933526|0.955777|0.907824|
|5|new/old|1.002071|1.220278|0.951761|0.906757|1.011957|
|0|new/cgo|1.161565|1.128758|1.074199|1.058615|1.093954|
|1|new/cgo|0.938190|0.799923|1.050522|1.062824|1.034779|
|2|new/cgo|0.888936|0.705289|1.023083|0.994146|1.059688|
|3|new/cgo|0.778187|0.796935|0.985276|1.066671|0.894745|
|4|new/cgo|0.790731|0.799535|0.949777|0.991201|0.903841|
|5|new/cgo|0.855320|0.983632|1.035663|1.051718|1.017055|
|0|old/cgo|0.895200|1.100061|0.991705|0.999783|0.981464|
|1|old/cgo|0.885266|0.782211|1.090668|1.089966|1.091565|
|2|old/cgo|0.877599|0.782793|1.005204|1.008086|1.001559|
|3|old/cgo|0.768670|0.672531|0.975138|1.045408|0.896980|
|4|old/cgo|0.940271|0.968130|1.017408|1.037063|0.995612|
|5|old/cgo|0.853553|0.806072|1.088155|1.159868|1.005037|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50-p0-new|80351.328|298.521|8|0.003200|
|b000-batchset50-p1-old|80465.309|299.611|8|0.003200|
|b000-batchset50-p2-cgo|1616.051|59.000|0|0.000000|
|b001-batchset50-p0-old|80417.776|299.656|8|0.003200|
|b001-batchset50-p1-new|80500.890|298.616|8|0.003200|
|b001-batchset50-p2-cgo|1616.230|59.002|0|0.000000|
|b002-batchset50-p0-cgo|1616.141|59.001|0|0.000000|
|b002-batchset50-p1-new|80609.274|298.669|9|0.003600|
|b002-batchset50-p2-old|80386.070|299.596|8|0.003200|
|b003-batchset50-p0-new|80587.258|298.633|9|0.003600|
|b003-batchset50-p1-cgo|1616.096|59.001|0|0.000000|
|b003-batchset50-p2-old|80439.229|299.580|8|0.003200|
|b004-batchset50-p0-old|80388.563|299.574|8|0.003200|
|b004-batchset50-p1-cgo|1616.275|59.002|0|0.000000|
|b004-batchset50-p2-new|80347.981|298.603|8|0.003200|
|b005-batchset50-p0-cgo|1616.141|59.001|0|0.000000|
|b005-batchset50-p1-old|80523.053|299.633|8|0.003200|
|b005-batchset50-p2-new|80462.541|298.609|8|0.003200|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.
