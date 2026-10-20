# R9 PR4 inline lone GRV — read1000

Old=9377c0863aef7ecdee250cdc323e40ebf69d5e46;new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+patch2414b55ab15d20c8597ebc54917b67c24320c8f1b02e8ab6a412a0c1de3be014 (PR1/PR2/PR3 NOT stacked);old9377 code matches590145 in pkg/fdbgo/tools/dependency pins;CGo=immutable native7.3.77. FrozenV6,5000 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|594.566|903.339|409.774|171.458|237.301|1541.646|13.011|18|0.000600|
|old|457.034|770.930|485.500|191.362|292.153|7976.036|63.210|36|0.001200|
|new|440.617|693.051|463.217|172.586|291.904|7913.230|62.175|37|0.001233|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|0.964496|0.914975|0.970774|0.928647|0.997702|5/6|
|new/cgo|0.752136|0.766511|1.189687|1.049756|1.291803|0/6|
|old/cgo|0.779823|0.837739|1.225503|1.130414|1.294779|0/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=0.991921 (below1 in6/6), go_mallocs_per_op=0.983617 (below1 in6/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed789707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.964496|[0.938952, 0.992425]|5/6|
|new/old|p99_ns|0.914975|[0.836524, 0.976358]|5/6|
|new/old|process_cpu_ns_per_op|0.970774|[0.941429, 1.004078]|5/6|
|new/old|user_cpu_ns_per_op|0.928647|[0.880229, 0.976897]|5/6|
|new/old|system_cpu_ns_per_op|0.997702|[0.970038, 1.029776]|3/6|
|new/cgo|mean_ns|0.752136|[0.707641, 0.807677]|6/6|
|new/cgo|p99_ns|0.766511|[0.720778, 0.831152]|6/6|
|new/cgo|process_cpu_ns_per_op|1.189687|[1.091815, 1.323720]|0/6|
|new/cgo|user_cpu_ns_per_op|1.049756|[0.957835, 1.166764]|3/6|
|new/cgo|system_cpu_ns_per_op|1.291803|[1.190657, 1.429922]|0/6|
|old/cgo|mean_ns|0.779823|[0.732069, 0.844897]|6/6|
|old/cgo|p99_ns|0.837739|[0.772426, 0.910966]|6/6|
|old/cgo|process_cpu_ns_per_op|1.225503|[1.107018, 1.385603]|0/6|
|old/cgo|user_cpu_ns_per_op|1.130414|[1.006319, 1.275316]|2/6|
|old/cgo|system_cpu_ns_per_op|1.294779|[1.181151, 1.459175]|0/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|0.934713|0.950799|0.967181|0.961331|0.970743|
|1|new/old|1.023219|0.984855|1.048346|1.028636|1.060543|
|2|new/old|0.961596|0.741869|0.931405|0.903839|0.948400|
|3|new/old|0.975024|1.003139|0.981613|0.935684|1.011489|
|4|new/old|0.978558|0.896203|0.980222|0.923824|1.018137|
|5|new/old|0.917404|0.939507|0.921071|0.830158|0.980875|
|0|new/cgo|0.671112|0.729681|1.036668|0.913803|1.128119|
|1|new/cgo|0.728832|0.786937|1.114192|0.967871|1.225380|
|2|new/cgo|0.890965|0.718447|1.496526|1.315146|1.628481|
|3|new/cgo|0.782875|0.761676|1.291862|1.167397|1.380424|
|4|new/cgo|0.723364|0.699231|1.130156|1.003989|1.223979|
|5|new/cgo|0.733575|0.923104|1.123460|0.981612|1.221752|
|0|old/cgo|0.717987|0.767439|1.071844|0.950560|1.162119|
|1|old/cgo|0.712293|0.799038|1.062809|0.940926|1.155428|
|2|old/cgo|0.926548|0.968428|1.606740|1.455066|1.717083|
|3|old/cgo|0.802929|0.759293|1.316061|1.247640|1.364744|
|4|old/cgo|0.739214|0.780215|1.152958|1.086775|1.202175|
|5|old/cgo|0.799621|0.982540|1.219733|1.182440|1.245574|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-read1000-p0-old|7969.422|63.193|6|0.001200|
|b000-read1000-p1-cgo|1542.202|13.012|3|0.000600|
|b000-read1000-p2-new|7911.691|62.170|6|0.001200|
|b001-read1000-p0-new|7904.117|62.146|6|0.001200|
|b001-read1000-p1-cgo|1539.946|13.009|3|0.000600|
|b001-read1000-p2-old|7977.635|63.220|6|0.001200|
|b002-read1000-p0-cgo|1541.115|13.011|3|0.000600|
|b002-read1000-p1-new|7905.528|62.183|6|0.001200|
|b002-read1000-p2-old|7980.139|63.230|6|0.001200|
|b003-read1000-p0-cgo|1541.086|13.010|3|0.000600|
|b003-read1000-p1-old|7982.254|63.199|6|0.001200|
|b003-read1000-p2-new|7917.006|62.199|6|0.001200|
|b004-read1000-p0-new|7917.264|62.181|7|0.001400|
|b004-read1000-p1-old|7973.110|63.206|6|0.001200|
|b004-read1000-p2-cgo|1542.176|13.011|3|0.000600|
|b005-read1000-p0-old|7974.437|63.213|6|0.001200|
|b005-read1000-p1-new|7914.768|62.170|6|0.001200|
|b005-read1000-p2-cgo|1542.176|13.011|3|0.000600|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.
