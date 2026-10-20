# R5 PR1 keyrange memcmp — unprofiled

Old=9377c0863aef7ecdee250cdc323e40ebf69d5e46; new=old+patch33f1ed2062a8f412f0ee291af16379f41e32aa7edee5b4274d1db1f7fc7b96ef; CGo=immutable native7.3.77. FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders.2500was chosen before execution fromR3wallbudget,not after rerunning results.

Absolute values are medians of6run statistics (µs/transaction); ratios are geometric means of6within-block ratios,NOT ratios of medians. Component medians need not sum. Go GC counts cover Go heap only; native heap is unobserved.

|Arm|Mean|p99|CPU|User CPU|System CPU|Go GC total|Go GC/tx aggregate|
|---|---:|---:|---:|---:|---:|---:|---:|
|cgo|2193.699|4104.372|851.059|473.829|370.497|0|0.000000|
|old|1887.260|3311.742|868.660|478.031|386.481|48|0.003200|
|new|2051.194|3443.656|871.319|471.693|396.078|48|0.003200|

|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|
|---|---:|---:|---:|---:|---:|---:|
|new/old|1.039208|0.987230|0.997384|0.973697|1.027339|3/6|
|new/cgo|0.929302|0.837800|0.997856|0.981600|1.022008|2/6|
|old/cgo|0.894241|0.848637|1.000473|1.008117|0.994811|2/6|

## Per-block paired ratios

|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|
|---|---|---:|---:|---:|---:|---:|
|0|new/old|1.002283|1.091015|1.061667|1.030773|1.100601|
|1|new/old|0.953309|0.865817|0.954936|0.962331|0.945653|
|2|new/old|1.066055|0.997471|0.968280|0.928797|1.020426|
|3|new/old|0.991349|0.751334|0.982954|0.939204|1.040301|
|4|new/old|1.145258|1.182553|1.010261|1.001976|1.020366|
|5|new/old|1.089131|1.105855|1.009822|0.982917|1.042856|
|0|new/cgo|0.840163|0.791826|0.933250|0.981724|0.881856|
|1|new/cgo|0.885513|0.730045|0.951011|1.032695|0.863737|
|2|new/cgo|0.950233|0.867781|1.029837|1.000950|1.066848|
|3|new/cgo|1.033408|0.875970|1.038441|1.003721|1.082765|
|4|new/cgo|0.958590|0.867969|1.026982|0.955808|1.127539|
|5|new/cgo|0.919704|0.906689|1.012771|0.918862|1.148626|
|0|old/cgo|0.838249|0.725770|0.879042|0.952415|0.801249|
|1|old/cgo|0.928883|0.843186|0.995890|1.073119|0.913376|
|2|old/cgo|0.891354|0.869981|1.063573|1.077684|1.045492|
|3|old/cgo|1.042426|1.165886|1.056449|1.068693|1.040819|
|4|old/cgo|0.837008|0.733979|1.016551|0.953924|1.105034|
|5|old/cgo|0.844438|0.819899|1.002921|0.934832|1.101423|

## Per-run GC and allocation

|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|
|---|---:|---:|---:|---:|
|b000-batchset50-p0-cgo|1616.230|59.002|0|0.000000|
|b000-batchset50-p1-new|80424.899|299.611|8|0.003200|
|b000-batchset50-p2-old|80467.539|299.607|8|0.003200|
|b001-batchset50-p0-new|80594.454|299.659|8|0.003200|
|b001-batchset50-p1-cgo|1616.230|59.002|0|0.000000|
|b001-batchset50-p2-old|80467.018|299.604|8|0.003200|
|b002-batchset50-p0-cgo|1616.141|59.001|0|0.000000|
|b002-batchset50-p1-old|80400.106|299.553|8|0.003200|
|b002-batchset50-p2-new|80478.490|299.621|8|0.003200|
|b003-batchset50-p0-old|80411.581|299.536|8|0.003200|
|b003-batchset50-p1-cgo|1616.141|59.001|0|0.000000|
|b003-batchset50-p2-new|80302.582|299.559|8|0.003200|
|b004-batchset50-p0-old|80321.990|299.458|8|0.003200|
|b004-batchset50-p1-new|80466.128|299.645|8|0.003200|
|b004-batchset50-p2-cgo|1616.051|59.000|0|0.000000|
|b005-batchset50-p0-new|80446.624|299.582|8|0.003200|
|b005-batchset50-p1-old|80256.323|299.451|8|0.003200|
|b005-batchset50-p2-cgo|1616.134|59.001|0|0.000000|

Sixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Native internal counters unobserved;GoMemStats excludeC++heap,so no Go/CGoallocation ratios. No profilers/builds overlappedtiming. Benchmark byte/persistence/counterchecks and remote standalone harness tests are NOT parent correctness/Bazel/finalreview approval.
