# R8 PR3 buffered readLoop — read1000

Old=9377c0863aef7ecdee250cdc323e40ebf69d5e46;new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+patchf02c03072178a7a1bfd4703e58a7401afdfcba1ccee3273ed5f721306d0c2128 (PR1/PR2 NOT stacked);old9377 code matches590145 in pkg/fdbgo/tools/dependency pins;CGo=immutable native7.3.77. FrozenV6,5000 measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.

Absolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
|cgo|587.956|869.499|399.705|168.203|221.818|1542.189|13.012|18|0.000600|
|old|445.799|620.815|471.538|182.241|285.714|7974.617|63.212|36|0.001200|
|new|425.796|588.961|445.726|176.915|273.322|7979.965|63.220|37|0.001233|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|0.970600|0.936668|0.961294|0.988293|0.945008|6/6|
|new/cgo|0.725310|0.685535|1.143040|1.049468|1.214841|0/6|
|old/cgo|0.747280|0.731886|1.189064|1.061901|1.285536|0/6|

Go new/old paired allocation ratios: go_alloc_bytes_per_op=1.000254 (below1 in1/6), go_mallocs_per_op=1.000017 (below1 in4/6).

## Nominal 95% block-bootstrap CIs

10,000 whole-block resamples, seed788707, common resampled indices across metrics/comparisons; six blocks, not individual operations.

|Ratio|Metric|Paired GM|95% CI|Pairs <1|
|---|---|---:|---|---:|
|new/old|mean_ns|0.970600|[0.951304, 1.001933]|5/6|
|new/old|p99_ns|0.936668|[0.885261, 0.980531]|5/6|
|new/old|process_cpu_ns_per_op|0.961294|[0.944064, 0.978276]|6/6|
|new/old|user_cpu_ns_per_op|0.988293|[0.939091, 1.049975]|5/6|
|new/old|system_cpu_ns_per_op|0.945008|[0.929805, 0.963960]|6/6|
|new/cgo|mean_ns|0.725310|[0.703411, 0.748085]|6/6|
|new/cgo|p99_ns|0.685535|[0.650548, 0.723824]|6/6|
|new/cgo|process_cpu_ns_per_op|1.143040|[1.082823, 1.203695]|0/6|
|new/cgo|user_cpu_ns_per_op|1.049468|[0.976073, 1.125077]|2/6|
|new/cgo|system_cpu_ns_per_op|1.214841|[1.153180, 1.280873]|0/6|
|old/cgo|mean_ns|0.747280|[0.716901, 0.780705]|6/6|
|old/cgo|p99_ns|0.731886|[0.680772, 0.782169]|6/6|
|old/cgo|process_cpu_ns_per_op|1.189064|[1.129104, 1.252424]|0/6|
|old/cgo|user_cpu_ns_per_op|1.061901|[0.973227, 1.152607]|2/6|
|old/cgo|system_cpu_ns_per_op|1.285536|[1.221445, 1.355669]|0/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|0.972194|0.992656|0.957837|0.989203|0.937764|
|1|new/old|0.953201|0.928462|0.964228|0.991632|0.947610|
|2|new/old|1.046824|0.949389|0.996670|1.127465|0.921890|
|3|new/old|0.948020|0.927049|0.928015|0.894931|0.950799|
|4|new/old|0.951644|0.825888|0.948213|0.987772|0.924986|
|5|new/old|0.955297|1.008053|0.974212|0.953075|0.988521|
|0|new/cgo|0.721924|0.630675|1.159967|1.172357|1.151750|
|1|new/cgo|0.693875|0.668848|1.119747|0.982026|1.229132|
|2|new/cgo|0.720671|0.648837|1.094602|1.006768|1.165714|
|3|new/cgo|0.695395|0.745628|1.025134|0.914526|1.112341|
|4|new/cgo|0.747496|0.675466|1.225392|1.083856|1.334672|
|5|new/cgo|0.775875|0.752978|1.248795|1.162883|1.312067|
|0|old/cgo|0.742572|0.635341|1.211028|1.185153|1.228188|
|1|old/cgo|0.727942|0.720383|1.161288|0.990312|1.297087|
|2|old/cgo|0.688435|0.683426|1.098259|0.892948|1.264484|
|3|old/cgo|0.733523|0.804303|1.104652|1.021895|1.169901|
|4|old/cgo|0.785478|0.817867|1.292317|1.097273|1.442911|
|5|old/cgo|0.812182|0.746962|1.281852|1.220138|1.327303|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-read1000-p0-old|7970.470|63.214|6|0.001200|
|b000-read1000-p1-new|7979.456|63.189|6|0.001200|
|b000-read1000-p2-cgo|1542.202|13.012|3|0.000600|
|b001-read1000-p0-cgo|1542.198|13.012|3|0.000600|
|b001-read1000-p1-old|7975.875|63.210|6|0.001200|
|b001-read1000-p2-new|7983.723|63.267|6|0.001200|
|b002-read1000-p0-cgo|1542.176|13.011|3|0.000600|
|b002-read1000-p1-new|7976.563|63.237|6|0.001200|
|b002-read1000-p2-old|7997.730|63.286|6|0.001200|
|b003-read1000-p0-new|7980.474|63.203|6|0.001200|
|b003-read1000-p1-cgo|1542.157|13.011|3|0.000600|
|b003-read1000-p2-old|7978.062|63.204|6|0.001200|
|b004-read1000-p0-old|7971.880|63.217|6|0.001200|
|b004-read1000-p1-cgo|1542.179|13.012|3|0.000600|
|b004-read1000-p2-new|7973.187|63.215|6|0.001200|
|b005-read1000-p0-new|7986.115|63.226|7|0.001400|
|b005-read1000-p1-old|7973.358|63.199|6|0.001200|
|b005-read1000-p2-cgo|1542.202|13.012|3|0.000600|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.
